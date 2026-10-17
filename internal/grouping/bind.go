package grouping

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
)

// BindInput 是一次"选点 + 建绑 + 投递意图"的写入输入。
//
// ⚠️ 与 B1 规格 §8.5 的差异（有意）：规格假设 `INSERT INTO task(...)` 发生在事务里；
// 但本项目 A 类已经把**受理即入库**做完了（`tasks.CreateTask` 是唯一入库入口，
// 任务入库时就是 `queued`）。因此这里对 `task` 是 **UPDATE**，不是 INSERT。
// 好处是保住"唯一入库入口"这条纪律；语义与规格一致（同一事务里写任务+尝试+决策）。
type BindInput struct {
	// TaskID 已存在的任务 id。
	TaskID string
	// AffinityKey 组键；空串表示不建组（普通负载调度）。
	AffinityKey string
	// Name 建议的 `task_group.name`（= stem）。
	Name string
	// Season "S01" 或空。
	Season string
	// EpisodeNo 集号（可空；写 `task.episode_no`）。
	EpisodeNo *int64
	// NodeID 选出的节点 n*。
	NodeID int64
	// DispatchReason 'scored' | 'affinity_hit'。
	DispatchReason domain.DispatchReason
	// Candidates 候选计数（写 `dispatch_log.candidates`，DDL 要求整数）。
	Candidates int
	// ScoreJSON 决策留痕（含全部候选与淘汰原因）。
	ScoreJSON string
	// Detail 写进 `group_binding_log.detail`（建议 JSON）。
	Detail *string
	// Now 事务内**只取一次**的时间戳（喂给 task_attempt.started_at 与 group_binding_log.at，
	// 二者必须同刻 —— 否则 INV-A3 用 `b.at <= af.dispatched_at` 重建绑定时会漏掉本次绑定）。
	Now int64
}

// BindResult 是事务写入结果。
type BindResult struct {
	// GroupID 组 id；0 表示不建组。
	GroupID int64
	// BindingEpoch 建绑后的代数（未建绑时为当前值）。
	BindingEpoch int64
	// WroteLog 本事务是否为"建绑赢家"（`UPDATE ... WHERE bound_node_id IS NULL` rowcount==1）。
	WroteLog bool
	// AttemptNo 本次写入的尝试号（从 1 开始）。
	AttemptNo int64
}

// ErrTaskAlreadyDispatched 表示该任务已经在别的 tick 里被派发过。
//
// 这不是错误路径的"意外"，而是并发保护：调度循环可能重复扫到同一任务
// （比如上一轮事务已提交、但内存游标还没前移）。用 `internal_state` 做条件更新，
// 只有第一个赢家能写。
var ErrTaskAlreadyDispatched = errors.New("grouping: 任务已被派发")

// BindTx 在调用方已开启的事务里完成 §5.3 的写入序列。
//
// 顺序与幂等性（设计文档 §5.3 + `design/04 §8.1`）：
//
//	(0) get-or-create 组（UNIQUE 兜底）
//	(1) UPDATE task  —— 条件：internal_state='queued' AND node_id IS NULL
//	(2) INSERT task_attempt(state='dispatched')
//	(3) UPDATE task_group SET bound_node_id=... WHERE bound_node_id IS NULL（只有第一个生效）
//	(4) 若 (3) 赢 ⇒ INSERT group_binding_log(action='bind')
//	(5) INSERT dispatch_log(decision='assigned')
//
// **事务内禁止网络 I/O**（`design/12 §9` 硬约束④）：这里只记录"打算投递"，
// 真正的 `add` 在提交之后由 `internal/dispatch` 执行。
func BindTx(ctx context.Context, tx *sql.Tx, in BindInput) (BindResult, error) {
	var res BindResult
	now := in.Now
	if now == 0 {
		now = time.Now().Unix()
	}

	// ── (0) get-or-create 组 ────────────────────────────────────────────
	var boundNode *int64
	var epoch int64
	if in.AffinityKey != "" {
		seasonArg := any(nil)
		if in.Season != "" {
			seasonArg = in.Season
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_group (name, kind, season, affinity_key, group_state, created_at, updated_at)
			 VALUES (?1, 'series', ?2, ?3, 'unbound', ?4, ?4)
			 ON CONFLICT(affinity_key) DO NOTHING`,
			in.Name, seasonArg, in.AffinityKey, now,
		); err != nil {
			return res, fmt.Errorf("grouping: 建组: %w", err)
		}
		var bound sql.NullInt64
		var epochNull sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT group_id, bound_node_id, binding_epoch
			   FROM task_group WHERE affinity_key = ?1`, in.AffinityKey,
		).Scan(&res.GroupID, &bound, &epochNull); err != nil {
			return res, fmt.Errorf("grouping: 读组: %w", err)
		}
		if bound.Valid {
			v := bound.Int64
			boundNode = &v
		}
		epoch = epochNull.Int64
	}

	// ── (1) 写任务（条件更新，防重复派发）────────────────────────────────
	reason := string(in.DispatchReason)
	res2, err := tx.ExecContext(ctx,
		`UPDATE task
		    SET group_id = ?1,
		        node_id = ?2,
		        dispatch_reason = ?3,
		        internal_state = 'dispatching',
		        episode_no = COALESCE(?4, episode_no),
		        attempt_no = attempt_no + 1,
		        updated_at = ?5
		  WHERE task_id = ?6
		    AND internal_state = 'queued'
		    AND node_id IS NULL`,
		nullIfZero(res.GroupID), in.NodeID, reason, optI64(in.EpisodeNo), now, in.TaskID,
	)
	if err != nil {
		return res, fmt.Errorf("grouping: 更新任务: %w", err)
	}
	if n, _ := res2.RowsAffected(); n == 0 {
		return res, ErrTaskAlreadyDispatched
	}

	// 读回 attempt_no（刚 +1 的值）
	if err := tx.QueryRowContext(ctx,
		`SELECT attempt_no FROM task WHERE task_id = ?1`, in.TaskID,
	).Scan(&res.AttemptNo); err != nil {
		return res, fmt.Errorf("grouping: 读 attempt_no: %w", err)
	}

	// ── (2) 投递尝试 ────────────────────────────────────────────────────
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_attempt (task_id, attempt_no, node_id, node_task_id, state, reason, started_at)
		 VALUES (?1, ?2, ?3, NULL, 'dispatched', ?4, ?5)`,
		in.TaskID, res.AttemptNo, in.NodeID, reason, now,
	); err != nil {
		return res, fmt.Errorf("grouping: 写尝试: %w", err)
	}

	// ── (3)(4) 首次建绑（幂等：只有赢家留痕）────────────────────────────
	res.BindingEpoch = epoch
	if in.AffinityKey != "" && boundNode == nil {
		up, err := tx.ExecContext(ctx,
			`UPDATE task_group
			    SET bound_node_id = ?1,
			        binding_mode = 'auto',
			        bound_at = ?2,
			        binding_epoch = binding_epoch + 1,
			        group_state = 'active',
			        updated_at = ?2
			  WHERE group_id = ?3 AND bound_node_id IS NULL`,
			in.NodeID, now, res.GroupID,
		)
		if err != nil {
			return res, fmt.Errorf("grouping: 建绑: %w", err)
		}
		if n, _ := up.RowsAffected(); n == 1 {
			res.WroteLog = true
			res.BindingEpoch = epoch + 1
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO group_binding_log
				   (group_id, binding_epoch, from_node_id, to_node_id, action, reason, detail, at)
				 VALUES (?1, ?2, NULL, ?3, 'bind', 'initial_assign', ?4, ?5)`,
				res.GroupID, res.BindingEpoch, in.NodeID, optText(in.Detail), now,
			); err != nil {
				return res, fmt.Errorf("grouping: 写绑定日志: %w", err)
			}
		}
	}

	// ── (5) 决策留痕 ────────────────────────────────────────────────────
	scoreJSON := in.ScoreJSON
	if scoreJSON == "" {
		scoreJSON = "{}"
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dispatch_log
		   (task_id, group_id, chosen_node, candidates, score_json, decision, reason, created_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, 'assigned', ?6, ?7)`,
		in.TaskID, nullIfZero(res.GroupID), in.NodeID, in.Candidates, scoreJSON, reason, now,
	); err != nil {
		return res, fmt.Errorf("grouping: 写决策日志: %w", err)
	}

	return res, nil
}

// QueueInput 是"决定排队"的写入输入。
type QueueInput struct {
	TaskID     string
	GroupID    int64
	Reason     string // 写 dispatch_log.reason（如 no_candidate / bound_capacity）
	Candidates int
	ScoreJSON  string
	Now        int64
}

// QueueTx 记录一次"排队"决策。
//
// 按 `design/02 §3.2.1(3)`：`affinity_wait` **只排队、没有投递** ⇒
// **不写 `task_attempt`**。任务保持 `queued`、`node_id` 保持 NULL、
// `dispatch_reason='affinity_wait'`。
//
// ⚠️ `task.dispatch_reason` 的 CHECK 只允许 4 值，`no_candidate` 这类词**不合法**；
// 它只能进 `dispatch_log.reason`（另一列、自由文本）。
func QueueTx(ctx context.Context, tx *sql.Tx, in QueueInput) error {
	now := in.Now
	if now == 0 {
		now = time.Now().Unix()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE task
		    SET dispatch_reason = 'affinity_wait',
		        internal_state = 'queued',
		        updated_at = ?1
		  WHERE task_id = ?2 AND internal_state = 'queued'`,
		now, in.TaskID,
	); err != nil {
		return fmt.Errorf("grouping: 排队更新: %w", err)
	}
	scoreJSON := in.ScoreJSON
	if scoreJSON == "" {
		scoreJSON = "{}"
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dispatch_log
		   (task_id, group_id, chosen_node, candidates, score_json, decision, reason, created_at)
		 VALUES (?1, ?2, NULL, ?3, ?4, 'queued', ?5, ?6)`,
		in.TaskID, nullIfZero(in.GroupID), in.Candidates, scoreJSON, in.Reason, now,
	); err != nil {
		return fmt.Errorf("grouping: 写排队日志: %w", err)
	}
	return nil
}

// MarshalEvaluated 把评估记录序列化成 `dispatch_log.score_json`。
//
// 结构固定为 `{"decision":..., "chosen":..., "evaluated":[...], "weights":{...}}`，
// 目的是让"为什么选它"能**离线复算**（B1-AC5 的可追溯性）。
func MarshalEvaluated(decision, reason string, chosen int64, evals any, extra map[string]any) string {
	m := map[string]any{
		"decision":  decision,
		"reason":    reason,
		"chosen":    chosen,
		"evaluated": evals,
	}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"error":"score_json_marshal_failed"}`
	}
	return string(b)
}

// nullIfZero 把 0 映射为 SQL NULL（`group_id` 允许 NULL，而 0 不是合法外键）。
func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// optI64 把 `*int64` 映射为 SQL 参数（nil ⇒ NULL）。
func optI64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// optText 把 `*string` 映射为 SQL 参数（nil ⇒ NULL）。
func optText(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
