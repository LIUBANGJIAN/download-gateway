package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
	"github.com/LIUBANGJIAN/download-gateway/internal/secret"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// testKey 与 engine 测试同一个密钥，方便两边共用节点密文。
const testKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t06.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mustExec(t *testing.T, st *store.Store, sql string, args ...any) {
	t.Helper()
	if _, err := st.Execute(context.Background(), sql, args...); err != nil {
		t.Fatalf("执行 SQL 失败: %v\nSQL: %s", err, sql)
	}
}

// addNode 插一个 online 节点，返回 node_id。
func addNode(t *testing.T, st *store.Store, alias, baseURL string) int64 {
	t.Helper()
	box := secret.WithKey(testKey)
	enc, err := box.Wrap("node-pass")
	if err != nil {
		t.Fatalf("加密节点密码失败: %v", err)
	}
	now := time.Now().Unix()
	mustExec(t, st,
		`INSERT INTO node (alias, base_url, user, pass_enc, weight, max_concurrent, role,
		                   enabled, state, created_at, updated_at)
		 VALUES (?1, ?2, 'admin', ?3, 1.0, 3, 'generic', 1, 'online', ?4, ?4)`,
		alias, baseURL, enc, now)
	rows, err := st.Query(context.Background(), `SELECT node_id FROM node WHERE alias = ?1`, alias)
	if err != nil || len(rows) == 0 {
		t.Fatalf("读 node_id 失败: %v", err)
	}
	return rows[0][0].(int64)
}

// dispatchedTask 插一个"已落点、在下载中"的任务 + 对应 attempt。
type taskSeed struct {
	TaskID        string
	NodeTaskID    string
	InternalState string
	Permillage    int64
	ProgressAt    int64
	StartedAt     int64
	TotalSize     *int64
	SizeState     string
}

func seedDispatched(t *testing.T, st *store.Store, nodeID int64, s taskSeed) {
	t.Helper()
	if s.InternalState == "" {
		s.InternalState = "running"
	}
	if s.SizeState == "" {
		s.SizeState = "unknown"
	}
	var total any
	if s.TotalSize != nil {
		total = *s.TotalSize
	}
	mustExec(t, st,
		`INSERT INTO task (task_id, gid, kind, url_raw, name, internal_state, aria_status,
		                   permillage, progress_at, size_state, total_size, downloaded_size,
		                   node_id, node_task_id, attempt_no, created_at, updated_at)
		 VALUES (?1, ?2, 'http', ?3, ?1, ?4, 'active',
		         ?5, ?6, ?7, ?8, 0, ?9, ?10, 1, ?11, ?11)`,
		s.TaskID, "gid-"+s.TaskID, "http://node.test/"+s.TaskID, s.InternalState,
		s.Permillage, s.ProgressAt, s.SizeState, total, nodeID, s.NodeTaskID, s.StartedAt)
	mustExec(t, st,
		`INSERT INTO task_attempt (task_id, attempt_no, node_id, node_task_id, state, started_at)
		 VALUES (?1, 1, ?2, ?3, 'running', ?4)`,
		s.TaskID, nodeID, s.NodeTaskID, s.StartedAt)
}

func i64p(v int64) *int64 { return &v }

// ---------------------------------------------------------------------------
// 纯函数层：这三条是「回读会不会把界面弄坏」的全部依据。
// ---------------------------------------------------------------------------

// TestApplySnapshotRefusesStaleObservation 守住 D-04：进度只前进、不后退。
//
// 为什么这条必须存在：三级轮询（或将来并发多拉）天然会送进乱序快照；
// 一次 `observed_at` 相等的重复快照就足以让进度条往后退一格。
func TestApplySnapshotRefusesStaleObservation(t *testing.T) {
	v := taskView{TaskID: "T1", InternalState: "running", Permillage: 400, ProgressAt: 1000}
	nt := protocol.RemoteTask{TaskID: "1001", State: "running", Permillage: 108, Downloaded: 100}

	cases := []struct {
		name     string
		observed int64
		want     bool
	}{
		{"早于上次写入", 999, false},
		{"与上次写入同刻（重复快照）", 1000, false},
		{"晚于上次写入", 1001, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, wrote := ApplySnapshot(v, nt, c.observed)
			if wrote != c.want {
				t.Fatalf("observedAt=%d 的写入判定 = %v，期望 %v", c.observed, wrote, c.want)
			}
		})
	}
}

// TestBackfillSizeThreeTiers 守住 `design/02 §2.4.3` 的三级优先级。
//
// 最关键的一条是第三档：`total_size=0 且 permillage=0` 必须**保持原值/未知**。
// 若把它写成 resolved/0，节点负载估算会得到假的 0 剩余字节，从而永远最"闲"，
// 所有任务都会被调度过去 —— 这是一个不会报错、只会让集群严重偏斜的静默故障。
func TestBackfillSizeThreeTiers(t *testing.T) {
	cases := []struct {
		name      string
		view      taskView
		remote    protocol.RemoteTask
		wantTotal any
		wantState string
	}{
		{
			name:      "档一：节点报出真实总大小 → resolved 原样采用",
			view:      taskView{},
			remote:    protocol.RemoteTask{TotalSize: 12345, Downloaded: 600, Permillage: 48},
			wantTotal: int64(12345),
			wantState: string(domain.SizeResolved),
		},
		{
			name:      "档二：只有下载量与千分比 → estimated 反推",
			view:      taskView{},
			remote:    protocol.RemoteTask{TotalSize: 0, Downloaded: 2500, Permillage: 500},
			wantTotal: int64(5000),
			wantState: string(domain.SizeEstimated),
		},
		{
			name:      "档三：total=0 且 permillage=0 是未知、不是 0 字节",
			view:      taskView{SizeState: "unknown", TotalSize: nil},
			remote:    protocol.RemoteTask{TotalSize: 0, Downloaded: 0, Permillage: 0},
			wantTotal: nil,
			wantState: string(domain.SizeUnknown),
		},
		{
			name:      "档三之保留旧值：已有 resolved 总大小不被未知读数抹掉",
			view:      taskView{SizeState: "resolved", TotalSize: i64p(8000)},
			remote:    protocol.RemoteTask{TotalSize: 0, Downloaded: 0, Permillage: 0},
			wantTotal: nil,
			wantState: string(domain.SizeResolved),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotTotal, gotState := backfillSize(c.view, c.remote)
			if gotState != c.wantState {
				t.Fatalf("size_state = %q，期望 %q", gotState, c.wantState)
			}
			if c.wantTotal == nil {
				if gotTotal != nil {
					t.Fatalf("total_size 应为 nil（不写），实际 %v", gotTotal)
				}
				return
			}
			if gotTotal != c.wantTotal {
				t.Fatalf("total_size = %v，期望 %v", gotTotal, c.wantTotal)
			}
		})
	}
}

// TestAttemptStateForMapping 守住"completed 仍记 running"这条反直觉约定。
//
// attempt 表里没有 `completed`：写成 `superseded` 会让人以为这次尝试被替换掉了。
func TestAttemptStateForMapping(t *testing.T) {
	cases := []struct {
		in   domain.InternalState
		want string
	}{
		{domain.StateRunning, "running"},
		{domain.StateCompleted, "running"},
		{domain.StateQueued, "created"},
		{domain.StateDispatching, "created"},
		{domain.StatePaused, "created"},
		{domain.StateFailed, "failed"},
		{domain.StateFailedTerminal, "failed"},
		{domain.StateRemoved, "superseded"},
	}
	for _, c := range cases {
		if got := attemptStateFor(c.in); got != c.want {
			t.Fatalf("%s 映射为 attempt.state = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 落库层：真 SQLite + 真迁移。
// ---------------------------------------------------------------------------

// TestReconcileWritesProgressAndAttempt 是本包的主干用例：
// 一次快照必须同时改到 task / task_progress / task_attempt 三张表。
func TestReconcileWritesProgressAndAttempt(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	nodeID := addNode(t, st, "n1", "http://node.test")

	seedDispatched(t, st, nodeID, taskSeed{
		TaskID: "T1", NodeTaskID: "1001", InternalState: "running",
		Permillage: 200, ProgressAt: 1000, StartedAt: 900,
	})

	snap := Snapshot{
		NodeID: nodeID, Group: SGAll, ObservedAt: 2000,
		Tasks: []protocol.RemoteTask{{
			TaskID: "1001", State: "running", Permillage: 450,
			Downloaded: 4500, TotalSize: 10000, Rate: 1024,
		}},
	}
	applied, missing, err := Reconcile(ctx, st, nodeID, snap, 15*time.Second)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if applied != 1 || missing != 0 {
		t.Fatalf("applied=%d missing=%d，期望 1/0", applied, missing)
	}

	rows, err := st.Query(ctx, `SELECT internal_state, permillage, downloaded_size, total_size,
	                                   size_state, progress_at, aria_status
	                              FROM task WHERE task_id = 'T1'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读任务失败: %v", err)
	}
	if got := toString(rows[0][0]); got != "running" {
		t.Fatalf("internal_state = %q，期望 running", got)
	}
	if got := toInt64(rows[0][1]); got != 450 {
		t.Fatalf("permillage = %d，期望 450", got)
	}
	if got := toInt64(rows[0][2]); got != 4500 {
		t.Fatalf("downloaded_size = %d，期望 4500", got)
	}
	if got := toInt64(rows[0][3]); got != 10000 {
		t.Fatalf("total_size = %d，期望 10000", got)
	}
	if got := toString(rows[0][4]); got != "resolved" {
		t.Fatalf("size_state = %q，期望 resolved", got)
	}
	if got := toInt64(rows[0][5]); got != 2000 {
		t.Fatalf("progress_at = %d，期望 2000", got)
	}
	if got := toString(rows[0][6]); got != "active" {
		t.Fatalf("aria_status = %q，期望 active", got)
	}

	// 进度曲线必须真的多一行。
	prows, err := st.Query(ctx, `SELECT permillage, downloaded, total, rate, observed_at
	                               FROM task_progress WHERE task_id = 'T1'`)
	if err != nil || len(prows) != 1 {
		t.Fatalf("task_progress 行数 = %d，期望 1（err=%v）", len(prows), err)
	}
	if toInt64(prows[0][4]) != 2000 || toInt64(prows[0][0]) != 450 {
		t.Fatalf("进度行内容不符: %v", prows[0])
	}

	// 第二次快照：千分比到 1000、状态 completed。
	snap2 := Snapshot{
		NodeID: nodeID, Group: SGAll, ObservedAt: 3000,
		Tasks: []protocol.RemoteTask{{
			TaskID: "1001", State: "completed", Permillage: 1000,
			Downloaded: 10000, TotalSize: 10000,
		}},
	}
	if applied, _, err = Reconcile(ctx, st, nodeID, snap2, 15*time.Second); err != nil {
		t.Fatalf("第二轮对账失败: %v", err)
	}
	if applied != 1 {
		t.Fatalf("第二轮 applied = %d，期望 1", applied)
	}

	rows, err = st.Query(ctx, `SELECT internal_state, aria_status, completed_at
	                              FROM task WHERE task_id = 'T1'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读任务失败: %v", err)
	}
	if got := toString(rows[0][0]); got != "completed" {
		t.Fatalf("internal_state = %q，期望 completed", got)
	}
	if got := toString(rows[0][1]); got != "complete" {
		t.Fatalf("aria_status = %q，期望 complete", got)
	}
	if rows[0][2] == nil || toInt64(rows[0][2]) != 3000 {
		t.Fatalf("completed_at = %v，期望 3000", rows[0][2])
	}

	// attempt 跟随：completed 仍记 running（见 attemptStateFor 注释）。
	arows, err := st.Query(ctx, `SELECT state FROM task_attempt WHERE task_id = 'T1'`)
	if err != nil || len(arows) != 1 {
		t.Fatalf("读 attempt 失败: %v", err)
	}
	if got := toString(arows[0][0]); got != "running" {
		t.Fatalf("attempt.state = %q，期望 running", got)
	}
}

// TestReconcileNeverRevivesTerminalTask 守住终态不回退。
//
// 场景是真的会发生的：节点上任务完成后进入做种（`seeding`），
// BitComet 对做种的返回值经常仍然是 running。若把 completed 也纳入对账范围，
// 用户会看到"已完成"的任务每 15 秒跳回"下载中"一次。
func TestReconcileNeverRevivesTerminalTask(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	nodeID := addNode(t, st, "n1", "http://node.test")

	seedDispatched(t, st, nodeID, taskSeed{
		TaskID: "T1", NodeTaskID: "1001", InternalState: "completed",
		Permillage: 1000, ProgressAt: 5000, StartedAt: 900,
	})
	mustExec(t, st, `UPDATE task SET completed_at = 5000 WHERE task_id = 'T1'`)

	snap := Snapshot{
		NodeID: nodeID, Group: SGAll, ObservedAt: 6000,
		Tasks: []protocol.RemoteTask{{
			TaskID: "1001", State: "running", Permillage: 1000, Downloaded: 10000, TotalSize: 10000,
		}},
	}
	applied, missing, err := Reconcile(ctx, st, nodeID, snap, 15*time.Second)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if applied != 0 || missing != 0 {
		t.Fatalf("applied=%d missing=%d，终态任务不该被对账（期望 0/0）", applied, missing)
	}

	rows, err := st.Query(ctx, `SELECT internal_state, completed_at FROM task WHERE task_id = 'T1'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读任务失败: %v", err)
	}
	if got := toString(rows[0][0]); got != "completed" {
		t.Fatalf("终态被改回 %q（必须是 completed）", got)
	}
	if toInt64(rows[0][1]) != 5000 {
		t.Fatalf("completed_at 被改写为 %v", rows[0][1])
	}
	// 也不能凭空多一条进度点。
	prows, _ := st.Query(ctx, `SELECT id FROM task_progress WHERE task_id = 'T1'`)
	if len(prows) != 0 {
		t.Fatalf("终态任务被写入了 %d 条进度历史", len(prows))
	}
}

// TestReconcileMissingRespectsGrace 守住"任务消失"的宽限期。
//
// 刚投出去的任务还没进节点列表是常态（异步 add + 列表有延迟）。
// 若没有宽限期，每一次投递都立刻产生一条"任务在节点上消失"的告警，
// 告警一多就没人看了 —— 而真正的丢失就藏在这堆噪声里。
func TestReconcileMissingRespectsGrace(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	nodeID := addNode(t, st, "n1", "http://node.test")

	now := int64(10000)
	// 刚投出去（1 秒前）→ 宽限期内，不该报。
	seedDispatched(t, st, nodeID, taskSeed{
		TaskID: "FRESH", NodeTaskID: "2001", InternalState: "dispatching",
		ProgressAt: 0, StartedAt: now - 1,
	})
	// 很久以前投的，列表里始终没有 → 该报。
	seedDispatched(t, st, nodeID, taskSeed{
		TaskID: "LOST", NodeTaskID: "2002", InternalState: "running",
		ProgressAt: 0, StartedAt: now - 3600,
	})

	snap := Snapshot{NodeID: nodeID, Group: SGAll, ObservedAt: now, Tasks: nil}
	applied, missing, err := Reconcile(ctx, st, nodeID, snap, 15*time.Second)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if applied != 0 {
		t.Fatalf("applied = %d，期望 0", applied)
	}
	if missing != 1 {
		t.Fatalf("missing = %d，期望 1（只有超期的那条）", missing)
	}

	// 两条任务的状态都不许动（B1 只记日志，不改状态）。
	rows, err := st.Query(ctx, `SELECT task_id, internal_state FROM task ORDER BY task_id`)
	if err != nil || len(rows) != 2 {
		t.Fatalf("读任务失败: %v", err)
	}
	if toString(rows[0][1]) != "dispatching" || toString(rows[1][1]) != "running" {
		t.Fatalf("任务状态被改动了: %v / %v", rows[0][1], rows[1][1])
	}

	// 但必须留下一条可查的痕迹，否则就是"任务凭空没了"。
	//
	// 注意 category 必须是 DDL 枚举内的值（`task`）；曾经的写法把
	// `node_task_missing` 当 category 传，被 CHECK 拦下且错误被吞掉 ——
	// 告警一条都写不进去。所以这里断言的是"用枚举内 category + 消息里带码"。
	erows, err := st.Query(ctx,
		`SELECT level, category, task_id, message FROM event_log WHERE message LIKE 'node_task_missing:%'`)
	if err != nil {
		t.Fatalf("读 event_log 失败: %v", err)
	}
	if len(erows) != 1 {
		t.Fatalf("node_task_missing 事件数 = %d，期望 1", len(erows))
	}
	if toString(erows[0][0]) != "warn" {
		t.Fatalf("事件 level = %q，期望 warn", toString(erows[0][0]))
	}
	if toString(erows[0][1]) != "task" {
		t.Fatalf("事件 category = %q，期望 task（必须在 DDL 枚举内）", toString(erows[0][1]))
	}
	if toString(erows[0][2]) != "LOST" {
		t.Fatalf("事件 task_id = %q，期望 LOST", toString(erows[0][2]))
	}
}

// TestReconcileClampsPermillageIntoDDL 守住 DDL 的 CHECK。
//
// `task.permillage` 有 `CHECK (permillage BETWEEN 0 AND 1000)`。
// 节点只要报出一个 1500（实测某些版本对做种任务会这么报），
// 写库就会直接报约束冲突，而错误信息里只有 "constraint failed"，
// 排查者会以为是数据损坏。这条断言要求：必须先夹取再写。
func TestReconcileClampsPermillageIntoDDL(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	nodeID := addNode(t, st, "n1", "http://node.test")
	seedDispatched(t, st, nodeID, taskSeed{
		TaskID: "T1", NodeTaskID: "1001", InternalState: "running",
		Permillage: 100, ProgressAt: 100, StartedAt: 50,
	})

	snap := Snapshot{
		NodeID: nodeID, Group: SGAll, ObservedAt: 200,
		Tasks: []protocol.RemoteTask{{
			TaskID: "1001", State: "completed", Permillage: 1500, Downloaded: 999, TotalSize: 1000,
		}},
	}
	if _, _, err := Reconcile(ctx, st, nodeID, snap, 15*time.Second); err != nil {
		t.Fatalf("越界千分比不该写库失败: %v", err)
	}
	rows, err := st.Query(ctx, `SELECT permillage, internal_state FROM task WHERE task_id = 'T1'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读任务失败: %v", err)
	}
	if got := toInt64(rows[0][0]); got != 1000 {
		t.Fatalf("库里的 permillage = %d，期望夹取到 1000", got)
	}
	// 夹取之后状态仍必须正确推进，不能因为"读数奇怪"就整个任务被跳过。
	if got := toString(rows[0][1]); got != "completed" {
		t.Fatalf("internal_state = %q，期望 completed（夹取不该影响状态判定）", got)
	}
}

// ---------------------------------------------------------------------------
// 轮询层
// ---------------------------------------------------------------------------

// countNode 是一个只统计 task_list 调用次数的假节点。
type countNode struct {
	srv     *httptest.Server
	listHit atomic.Int64
}

func newCountNode(t *testing.T) *countNode {
	t.Helper()
	c := &countNode{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/webui/login", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"error_code": "OK", "invite_token": "0123456789abcdef0123456789abcdef"})
	})
	mux.HandleFunc("/api/device_token/get", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"error_code": "OK", "device_token": "dev-token"})
	})
	mux.HandleFunc("/api_v2/task_list/get", func(w http.ResponseWriter, r *http.Request) {
		c.listHit.Add(1)
		writeJSON(w, map[string]any{"error_code": "OK", "tasks": []any{}, "filtered_task_count": 0})
	})
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func writeJSON(w http.ResponseWriter, v map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TestRunOnceSkipsNonOnlineNodes 守住"只拉 online 节点"。
//
// degraded/offline 的节点连不上，每轮都去撞一遍只会把日志刷满并拖慢整轮；
// 它们的恢复由健康探测负责，不该由轮询承担。
func TestRunOnceSkipsNonOnlineNodes(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	online := newCountNode(t)
	offline := newCountNode(t)

	onlineID := addNode(t, st, "on", online.srv.URL)
	// 造一个 degraded 节点（同样的连接信息，但 state 不同）。
	box := secret.WithKey(testKey)
	enc, err := box.Wrap("node-pass")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	now := time.Now().Unix()
	mustExec(t, st,
		`INSERT INTO node (alias, base_url, user, pass_enc, weight, max_concurrent, role,
		                   enabled, state, created_at, updated_at)
		 VALUES ('off', ?1, 'admin', ?2, 1.0, 3, 'generic', 1, 'degraded', ?3, ?3)`,
		offline.srv.URL, enc, now)
	// 再插一个"在线但被禁用"的，也不该被拉。
	disabled := newCountNode(t)
	enc2, _ := box.Wrap("node-pass")
	mustExec(t, st,
		`INSERT INTO node (alias, base_url, user, pass_enc, weight, max_concurrent, role,
		                   enabled, state, created_at, updated_at)
		 VALUES ('disabled', ?1, 'admin', ?2, 1.0, 3, 'generic', 0, 'online', ?3, ?3)`,
		disabled.srv.URL, enc2, now)

	p := NewPoller(st, box, DefaultPollConfig())
	p.SetLogf(func(string, ...any) {})
	stats, err := p.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if stats.Nodes != 1 {
		t.Fatalf("拉取成功节点数 = %d，期望 1（只有 online 且 enabled 的那个）", stats.Nodes)
	}
	if got := online.listHit.Load(); got != 1 {
		t.Fatalf("在线节点被拉 %d 次，期望 1", got)
	}
	if got := offline.listHit.Load(); got != 0 {
		t.Fatalf("degraded 节点被拉 %d 次，期望 0", got)
	}
	if got := disabled.listHit.Load(); got != 0 {
		t.Fatalf("禁用节点被拉 %d 次，期望 0", got)
	}
	_ = onlineID
}
