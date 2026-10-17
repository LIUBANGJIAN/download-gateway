package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// taskView 是参与对账的任务投影。
type taskView struct {
	TaskID        string
	NodeTaskID    string
	InternalState string
	Permillage    int64
	ProgressAt    int64
	SizeState     string
	TotalSize     *int64
	Downloaded    int64
	AttemptNo     int64
	StartedAt     int64
}

// Reconcile 把一个节点的快照对账进库。
//
// 返回值：
//   - applied：真正写入了进度的任务数
//   - missing：节点列表里找不到的代理任务数（**只记日志、不改状态**）
//
// ⚠️ 为什么"任务消失"只记日志（B1 规格 §7.5 的裁决）：
// `missing_on_node` / `external` 这两个状态值**还不在** Go 的枚举里，
// 而 B1 只被允许扩 `failed_terminal`。硬写一个未入枚举的裸串会在
// `aria_status` 派生时落进宽松兜底（显示成"排队中"），把"任务被外部删了"
// 伪装成"还在排队" —— 比不写更糟。所以 B1 只留 warn 痕迹，真正落库留后续。
func Reconcile(ctx context.Context, st *store.Store, nodeID int64, snap Snapshot, interval time.Duration) (applied, missing int, err error) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	views, err := loadTaskViews(ctx, st, nodeID)
	if err != nil {
		return 0, 0, err
	}
	if len(views) == 0 {
		return 0, 0, nil
	}

	byNodeID := make(map[string]protocol.RemoteTask, len(snap.Tasks))
	for _, t := range snap.Tasks {
		byNodeID[t.TaskID] = t
	}

	grace := int64((2 * interval).Seconds())
	now := snap.ObservedAt

	for _, v := range views {
		nt, found := byNodeID[v.NodeTaskID]
		if !found {
			// 宽限期（`design/04 §9.7`，默认 2 个轮询周期）：刚投出去的任务
			// 可能还没进列表，此时判"消失"会误报。
			if v.StartedAt > 0 && now-v.StartedAt > grace {
				missing++
				writeEvent(ctx, st, "warn", "task", nodeID, v.TaskID,
					fmt.Sprintf("node_task_missing: 节点 #%d 的任务列表里找不到 node_task_id=%s（代理侧状态未改动）", nodeID, v.NodeTaskID))
			}
			continue
		}

		wrote, err := applyOne(ctx, st, nodeID, v, nt, now)
		if err != nil {
			// 单个任务写失败**不中止整轮**：一条越界读数/坏数据不该让其余
			// 所有任务本轮都拿不到进度 —— 那在界面上表现为"整个集群卡住"，
			// 而根因只是一个节点的一个整数。
			// 但也不许静默：落一条 error 事件 + 打日志，保证可查。
			writeEvent(ctx, st, "error", "task", nodeID, v.TaskID,
				fmt.Sprintf("node_task_progress_write_failed: 回读进度写库失败，本轮已跳过该任务：%v", err))
			slog.Warn("回读进度写库失败，本轮跳过", "node_id", nodeID, "task_id", v.TaskID, "err", err)
			continue
		}
		if wrote {
			applied++
		}
	}
	return applied, missing, nil
}

// ApplySnapshot 是纯决策：给定任务当前视图与节点读数，算出应写什么。
//
// 第三个返回值是"是否应写"：进度**只前进不后退**（§7.3 防 D-04）。
// 把一个乱序/陈旧的快照写回去，用户会看到进度条倒着走。
func ApplySnapshot(v taskView, nt protocol.RemoteTask, observedAt int64) (domain.InternalState, *domain.ErrorClass, bool) {
	if observedAt <= v.ProgressAt {
		return domain.InternalState(v.InternalState), nil, false
	}
	var ec *string
	if nt.ErrorCode != "" {
		code := nt.ErrorCode
		ec = &code
	}
	state, errClass := domain.ClassifyNodeStatus(nt.State, ec, nt.Permillage)
	return state, errClass, true
}

// applyOne 写一个任务的进度。
func applyOne(ctx context.Context, st *store.Store, nodeID int64, v taskView, nt protocol.RemoteTask, observedAt int64) (bool, error) {
	state, errClass, shouldWrite := ApplySnapshot(v, nt, observedAt)
	if !shouldWrite {
		return false, nil
	}

	// ⚠️ 千分比必须在这里再夹一次，不能只信解析层。
	// `task.permillage` 的 DDL 是 `CHECK (permillage BETWEEN 0 AND 1000)`；
	// 节点只要报一个 1500（实测某些版本对做种任务会这么报），写库就抛
	// `CHECK constraint failed`，而这个错误是**每轮都会重现**的 ——
	// 该任务会被永久冻结在旧进度上，且错误信息里只有 "constraint failed"，
	// 排查者第一反应会是"数据库损坏"。
	p := clampPermillage(nt.Permillage)

	aria := string(domain.DeriveAriaStatus(state, p, errClass))
	var errClassArg any
	if errClass != nil {
		errClassArg = string(*errClass)
	}

	// 大小回填（`design/02 §2.4.3` 的三级优先级）。
	totalArg, sizeState := backfillSize(v, nt)

	var completedArg any
	if state == domain.StateCompleted {
		completedArg = observedAt
	}

	if _, err := st.Execute(ctx,
		`UPDATE task
		    SET internal_state = ?1, aria_status = ?2, error_class = ?3,
		        permillage = ?4, downloaded_size = ?5,
		        total_size = COALESCE(?6, total_size),
		        size_state = ?7, progress_at = ?8, updated_at = ?8,
		        completed_at = COALESCE(?9, completed_at)
		  WHERE task_id = ?10`,
		string(state), aria, errClassArg,
		p, nt.Downloaded,
		totalArg, sizeState, observedAt, completedArg, v.TaskID,
	); err != nil {
		return false, fmt.Errorf("sync: 写任务进度: %w", err)
	}

	// 进度曲线：只追加。`UNIQUE(task_id, observed_at)` 让重复快照自然被忽略。
	if _, err := st.Execute(ctx,
		`INSERT OR IGNORE INTO task_progress
		   (task_id, node_id, permillage, downloaded, total, rate, observed_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)`,
		v.TaskID, nodeID, p, nt.Downloaded, totalArg, nt.Rate, observedAt,
	); err != nil {
		return false, fmt.Errorf("sync: 写进度历史: %w", err)
	}

	// `task_attempt.state` 跟随（`design/02 §3.4` 的枚举；`dispatched` 之后再迁移）。
	attemptState := attemptStateFor(state)
	if attemptState != "" {
		if _, err := st.Execute(ctx,
			`UPDATE task_attempt SET state = ?1
			  WHERE task_id = ?2 AND attempt_no = ?3 AND state <> ?1`,
			attemptState, v.TaskID, v.AttemptNo,
		); err != nil {
			return false, fmt.Errorf("sync: 写尝试状态: %w", err)
		}
	}

	return true, nil
}

// attemptStateFor 把内部状态映射到 `task_attempt.state`。
//
// `completed` 仍记 `running`：attempt 表没有 `completed` 值，而「这次尝试成功了」
// 由 `task.internal_state` 表达；把 attempt 写成 `superseded` 会让人误以为被替换过。
func attemptStateFor(s domain.InternalState) string {
	switch s {
	case domain.StateRunning, domain.StateCompleted:
		return "running"
	case domain.StatePaused, domain.StateQueued, domain.StateDispatching:
		return "created"
	case domain.StateFailedTerminal, domain.StateFailed:
		return "failed"
	case domain.StateRemoved:
		return "superseded"
	default:
		return ""
	}
}

// backfillSize 按 §2.4.3 的三级优先级决定 total_size 与 size_state。
//
//	priority 1: total_size > 0          → resolved（权威）
//	priority 2: dl > 0 且 0<p≤1000      → estimated（total_hat = dl/(p/1000)）
//	priority 3: 其余                    → 不动
//
// ⚠️ `total_size=0 且 permillage=0` 是**未知**、不是 0 字节；把它写成 resolved/0
// 会让"估算剩余字节"的负载项算出一个假的 0，节点看起来最闲。
func backfillSize(v taskView, nt protocol.RemoteTask) (any, string) {
	if nt.TotalSize > 0 {
		return nt.TotalSize, string(domain.SizeResolved)
	}
	if nt.Downloaded > 0 && nt.Permillage > 0 && nt.Permillage <= 1000 {
		hat := int64(float64(nt.Downloaded) / (float64(nt.Permillage) / 1000.0))
		return hat, string(domain.SizeEstimated)
	}
	if v.TotalSize != nil {
		return nil, v.SizeState
	}
	return nil, string(domain.SizeUnknown)
}

// loadTaskViews 读参与对账的任务。
//
// 只取"已落点且未终态"的：`completed`/`failed_terminal`/`removed` 再回读
// 只会把终态改回 `running`（节点做种时仍报 running），那会让已完成的任务
// 在界面上反复跳回"下载中"。
func loadTaskViews(ctx context.Context, st *store.Store, nodeID int64) ([]taskView, error) {
	rows, err := st.Query(ctx, `
		SELECT t.task_id, t.node_task_id, t.internal_state, t.permillage, t.progress_at,
		       t.size_state, t.total_size, t.downloaded_size, t.attempt_no,
		       COALESCE((SELECT a.started_at FROM task_attempt a
		                  WHERE a.task_id = t.task_id AND a.attempt_no = t.attempt_no), 0)
		  FROM task t
		 WHERE t.node_id = ?1
		   AND t.node_task_id IS NOT NULL
		   AND t.internal_state IN ('dispatching','running','paused','queued')`,
		nodeID)
	if err != nil {
		return nil, fmt.Errorf("sync: 读任务视图: %w", err)
	}
	out := make([]taskView, 0, len(rows))
	for _, r := range rows {
		v := taskView{
			TaskID:        toString(r[0]),
			NodeTaskID:    toString(r[1]),
			InternalState: toString(r[2]),
			Permillage:    toInt64(r[3]),
			ProgressAt:    toInt64(r[4]),
			SizeState:     toString(r[5]),
			Downloaded:    toInt64(r[7]),
			AttemptNo:     toInt64(r[8]),
			StartedAt:     toInt64(r[9]),
		}
		if n, ok := r[6].(int64); ok {
			v.TotalSize = &n
		}
		out = append(out, v)
	}
	return out, nil
}

// clampPermillage 把千分比夹到 DDL 允许的 [0,1000]。
//
// 这是**写库边界自己的守卫**，不是重复劳动：`protocol.ParseTaskList` 里也夹了一次，
// 但 `Snapshot` 是公开结构体，任何构造路径（包括将来新增的、或测试里手搓的）
// 都可能塞进越界值。DB 边界的约束必须由 DB 边界自己守。
func clampPermillage(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > 1000 {
		return 1000
	}
	return v
}

// writeEvent 写一条 event_log。
//
// ⚠️ 这里曾经是 `_, _ =` —— 而 `event_log.category` 在 DDL 里有 CHECK 枚举
// (`node|task|dedup|group|system|security`)，调用方传自定义 category 时
// INSERT 会**稳定失败且被完全吞掉**：代码看起来"写了告警"，实际一条都进不了库，
// 日志里也查不到任何痕迹。审计失败本身必须可见，哪怕它不影响主流程。
func writeEvent(ctx context.Context, st *store.Store, level, category string, nodeID int64, taskID, message string) {
	if _, err := st.Execute(ctx,
		`INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, NULL, ?6)`,
		level, category, nodeID, taskID, message, time.Now().Unix(),
	); err != nil {
		slog.Warn("写 event_log 失败（审计痕迹缺失）",
			"level", level, "category", category, "task_id", taskID, "err", err)
	}
}
