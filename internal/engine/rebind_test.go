package engine

// B2 五条验收标准的**结果态**测试。
//
// 判据来源：`design/11 §3.6` 的 B2-AC1..AC5。每条都写成"删掉对应实现就会红"的形态，
// 并尽量断言**库里的最终状态**而不是中间调用（否则改个实现细节测试就失效，
// 而那正是 AC 说"只认结果态"的原因）。

import (
	"context"
	"testing"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// setNodeState 改节点健康状态（模拟离线/降级）。
func setNodeState(t *testing.T, st *store.Store, nodeID int64, state string) {
	t.Helper()
	if _, err := st.Execute(context.Background(),
		`UPDATE node SET state = ?1 WHERE node_id = ?2`, state, nodeID); err != nil {
		t.Fatalf("改节点状态失败: %v", err)
	}
}

// groupRow 读组的绑定视图（B2 断言几乎都要看这几列）。
type groupRow struct {
	GroupID      int64
	BoundNode    *int64
	Epoch        int64
	State        string
	SplitReason  *string
	WaitDeadline int64
	BindingPol   string
}

func readGroup(t *testing.T, st *store.Store) groupRow {
	t.Helper()
	rows, err := st.Query(context.Background(),
		`SELECT group_id, bound_node_id, binding_epoch, group_state,
		        split_reason, COALESCE(wait_deadline,0), binding_policy
		   FROM task_group ORDER BY group_id LIMIT 1`)
	if err != nil || len(rows) == 0 {
		t.Fatalf("读组失败: %v", err)
	}
	r := rows[0]
	g := groupRow{
		GroupID:      r[0].(int64),
		Epoch:        r[2].(int64),
		State:        r[3].(string),
		WaitDeadline: r[5].(int64),
		BindingPol:   r[6].(string),
	}
	if v, ok := r[1].(int64); ok {
		g.BoundNode = &v
	}
	if s, ok := r[4].(string); ok {
		g.SplitReason = &s
	}
	return g
}

// latestBindingLog 返回最新一条绑定日志（按 id 降序）。
type bindingLog struct {
	Action string
	Reason string
	Epoch  int64
	From   *int64
	To     *int64
}

func latestBindingLog(t *testing.T, st *store.Store) bindingLog {
	t.Helper()
	rows, err := st.Query(context.Background(),
		`SELECT action, reason, binding_epoch, from_node_id, to_node_id
		   FROM group_binding_log ORDER BY id DESC LIMIT 1`)
	if err != nil || len(rows) == 0 {
		t.Fatalf("读绑定日志失败: %v", err)
	}
	r := rows[0]
	l := bindingLog{Action: r[0].(string), Reason: r[1].(string), Epoch: r[2].(int64)}
	if v, ok := r[3].(int64); ok {
		l.From = &v
	}
	if v, ok := r[4].(int64); ok {
		l.To = &v
	}
	return l
}

// countBindingLogs 数日志条数（验证"不重复写、也不漏写"）。
func countBindingLogs(t *testing.T, st *store.Store) int {
	t.Helper()
	rows, err := st.Query(context.Background(), `SELECT COUNT(1) FROM group_binding_log`)
	if err != nil {
		t.Fatalf("读日志条数失败: %v", err)
	}
	return int(rows[0][0].(int64))
}

// assertBindingLogConsistent 是 B2-AC1 的**可判定谓词**，一条断言守住"库与日志自洽"。
//
// 谓词（写进文档与代码同一口径）：
//
//	设 L = 最新一条 group_binding_log、g = task_group 当前行，则必须同时成立：
//	  ① L.binding_epoch == g.binding_epoch
//	  ② L.action ∈ {bind, rebind, rejoin} ⇒ g.bound_node_id == L.to_node_id
//	  ③ L.action ∈ {split, release}       ⇒ g.bound_node_id == L.from_node_id
//
// ③ 的依据：split 的定义是"解除绑定，但保留原节点供 rejoin"，所以库里的
// `bound_node_id` 保留的是 L.from_node_id（原节点）。若把它清空，rejoin 就没有目标了。
func assertBindingLogConsistent(t *testing.T, st *store.Store) {
	t.Helper()
	g := readGroup(t, st)
	l := latestBindingLog(t, st)
	if l.Epoch != g.Epoch {
		t.Fatalf("日志 epoch=%d 与库 epoch=%d 不一致", l.Epoch, g.Epoch)
	}
	switch l.Action {
	case "bind", "rebind", "rejoin":
		if g.BoundNode == nil || l.To == nil || *g.BoundNode != *l.To {
			t.Fatalf("action=%s 时库 bound_node=%v 应等于日志 to_node=%v", l.Action, showI64(g.BoundNode), showI64(l.To))
		}
	case "split", "release":
		if g.BoundNode == nil || l.From == nil || *g.BoundNode != *l.From {
			t.Fatalf("action=%s 时库 bound_node=%v 应保留日志 from_node=%v", l.Action, showI64(g.BoundNode), showI64(l.From))
		}
	default:
		t.Fatalf("未知 action %q", l.Action)
	}
}

func showI64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// taskNodeID 读任务的落点（nil = 未落点）。
func taskNodeID(t *testing.T, st *store.Store, taskID string) *int64 {
	t.Helper()
	rows, err := st.Query(context.Background(),
		`SELECT node_id FROM task WHERE task_id = ?1`, taskID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读任务 %s 失败: %v", taskID, err)
	}
	if v, ok := rows[0][0].(int64); ok {
		return &v
	}
	return nil
}

// taskState 读 `internal_state`。
func taskState(t *testing.T, st *store.Store, taskID string) string {
	t.Helper()
	rows, err := st.Query(context.Background(),
		`SELECT internal_state FROM task WHERE task_id = ?1`, taskID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读任务 %s 状态失败: %v", taskID, err)
	}
	s, _ := rows[0][0].(string)
	return s
}

// TestB2AC1AndAC2_StrongWaitsThenSplits 覆盖 B2-AC1（留痕自洽）与 AC2（失效不静默）。
//
// 三步剧情：
//  1. E01 派发并绑定到 n1（`bind`，epoch=1）；
//  2. n1 离线后 E02 入队 ⇒ strong 策略**等待**（不得静默改派），写 `wait_deadline` + 告警；
//  3. 宽限到期后 E02 再入队 ⇒ **拆组**（`split`，epoch=2）并落到 n2，E01 原位不动。
//
// 反证：
//   - 若失效时直接改派（静默改绑）⇒ 第 2 步 E02 会直接落 n2，`group_binding_log` 只有 1 行；
//   - 若没有失效路径 ⇒ 第 3 步 E02 永远排在 queued（B1 的已知限制形态）。
func TestB2AC1AndAC2_StrongWaitsThenSplits(t *testing.T) {
	st := openTestStore(t)
	n1 := newFakeNode(t)
	n2 := newFakeNode(t)
	id1 := addNode(t, st, "n1", n1.srv.URL, 3)
	id2 := addNode(t, st, "n2", n2.srv.URL, 3)

	addTask(t, st, "E01", "Show.Name.S01E01.1080p")
	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	if got := taskNodeID(t, st, "E01"); got == nil || *got != id1 {
		t.Fatalf("E01 应绑定到 n1(%d)，实际 %v", id1, showI64(got))
	}
	if cnt := countBindingLogs(t, st); cnt != 1 {
		t.Fatalf("首轮应有 1 条 bind 日志，实际 %d", cnt)
	}

	// ── 第 2 步：绑定节点离线，新集入队 ──────────────────────────────────
	setNodeState(t, st, id1, "offline")
	addTask(t, st, "E02", "Show.Name.S01E02.1080p")
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}

	if got := taskNodeID(t, st, "E02"); got != nil {
		t.Fatalf("绑定节点离线时 E02 被投递到 %d —— 违反「禁止静默改绑」", *got)
	}
	g := readGroup(t, st)
	if g.State != "active" {
		t.Fatalf("等待期 group_state 应保持 active，实际 %q", g.State)
	}
	if g.WaitDeadline == 0 {
		t.Fatal("strong 策略必须写出 wait_deadline（否则宽限永远不到期）")
	}
	// 原因必须可查：dispatch_log 最后一行 reason 要说明是"绑定节点离线"。
	drows, err := st.Query(context.Background(),
		`SELECT decision, reason FROM dispatch_log WHERE task_id='E02' ORDER BY id DESC LIMIT 1`)
	if err != nil || len(drows) == 0 {
		t.Fatalf("E02 没有决策留痕: %v", err)
	}
	if dec, _ := drows[0][0].(string); dec != "queued" {
		t.Fatalf("E02 decision=%q，期望 queued", dec)
	}
	if reason, _ := drows[0][1].(string); reason != "strong_wait_started_node_offline" {
		t.Fatalf("E02 reason=%q，期望 strong_wait_started_node_offline", reason)
	}
	// 告警必须写且只写一次（刷屏会淹掉真正的告警）。
	if cnt := countEvents(t, st, "group", "warn"); cnt != 1 {
		t.Fatalf("首次失效应写 1 条 group/warn 告警，实际 %d", cnt)
	}
	assertBindingLogConsistent(t, st)

	// ── 第 3 步：宽限到期 ────────────────────────────────────────────────
	past := time.Now().Unix() - 1
	if _, err := st.Execute(context.Background(),
		`UPDATE task_group SET wait_deadline = ?1 WHERE group_id = ?2`, past, g.GroupID); err != nil {
		t.Fatalf("改 wait_deadline 失败: %v", err)
	}
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("第三轮失败: %v", err)
	}

	g = readGroup(t, st)
	if g.State != "split" {
		t.Fatalf("宽限到期后应拆组，实际 group_state=%q", g.State)
	}
	if g.SplitReason == nil || *g.SplitReason == "" {
		t.Fatal("拆组必须写 split_reason（B2-AC3 要求 reason 非空）")
	}
	if g.Epoch != 2 {
		t.Fatalf("拆组后 binding_epoch 应为 2，实际 %d", g.Epoch)
	}
	if l := latestBindingLog(t, st); l.Action != "split" {
		t.Fatalf("最新日志应为 split，实际 %q(%s)", l.Action, l.Reason)
	}
	if got := taskNodeID(t, st, "E02"); got == nil || *got != id2 {
		t.Fatalf("拆组后 E02 应落到健康节点 n2(%d)，实际 %v", id2, showI64(got))
	}
	// AC2 的"已下载内容不被删除"：E01 必须停在原节点、状态不回退。
	if got := taskNodeID(t, st, "E01"); got == nil || *got != id1 {
		t.Fatalf("E01 被迁移到 %v —— 拆组不得动已落点的成员", showI64(got))
	}
	if s := taskState(t, st, "E01"); s != "running" && s != "dispatching" {
		t.Fatalf("E01 状态变成 %q，期望 running/dispatching", s)
	}
	assertBindingLogConsistent(t, st)
}

// TestB2AC3CapacityDoesNotOversell 覆盖 B2-AC3：容量满不超卖、且处置有 reason。
//
// 两个子场景：
//   - `split_policy=none`（默认）⇒ 排队，`group_state=active` + reason=`bound_capacity`；
//   - `split_policy=capacity`       ⇒ 拆组，`group_state=split` + `split_reason` 非空。
func TestB2AC3CapacityDoesNotOversell(t *testing.T) {
	st := openTestStore(t)
	n1 := newFakeNode(t)
	n2 := newFakeNode(t)
	id1 := addNode(t, st, "n1", n1.srv.URL, 1) // 只能跑 1 个
	addNode(t, st, "n2", n2.srv.URL, 3)

	addTask(t, st, "E01", "Show.S01E01.1080p")
	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	if got := taskNodeID(t, st, "E01"); got == nil || *got != id1 {
		t.Fatalf("E01 应落 n1，实际 %v", showI64(got))
	}
	// 组内并发上限放大到 10，把变量隔离到"节点容量"这一条上。
	if _, err := st.Execute(context.Background(),
		`UPDATE task_group SET max_concurrent_in_group = 10`); err != nil {
		t.Fatalf("改组内上限失败: %v", err)
	}

	addTask(t, st, "E02", "Show.S01E02.1080p")
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("次轮失败: %v", err)
	}
	if got := taskNodeID(t, st, "E02"); got != nil {
		t.Fatalf("节点满容时 E02 被投递到 %d —— 超卖", *got)
	}
	if g := readGroup(t, st); g.State != "active" {
		t.Fatalf("split_policy=none 时应保持 active，实际 %q", g.State)
	}
	drows, err := st.Query(context.Background(),
		`SELECT reason FROM dispatch_log WHERE task_id='E02' ORDER BY id DESC LIMIT 1`)
	if err != nil || len(drows) == 0 {
		t.Fatalf("E02 无决策留痕: %v", err)
	}
	if r, _ := drows[0][0].(string); r != "bound_capacity" {
		t.Fatalf("reason=%q，期望 bound_capacity", r)
	}

	// ── 切到 capacity：允许拆组 ──────────────────────────────────────────
	if _, err := st.Execute(context.Background(),
		`UPDATE task_group SET split_policy = 'capacity'`); err != nil {
		t.Fatalf("改 split_policy 失败: %v", err)
	}
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("三轮失败: %v", err)
	}
	g := readGroup(t, st)
	if g.State != "split" || g.SplitReason == nil || *g.SplitReason != "bound_capacity_split" {
		t.Fatalf("split_policy=capacity 应拆组且 reason=bound_capacity_split，实际 %q/%v",
			g.State, derefStr(g.SplitReason))
	}
	if got := taskNodeID(t, st, "E02"); got == nil {
		t.Fatal("拆组后 E02 仍未落点")
	}
	assertBindingLogConsistent(t, st)
}

// TestB2AC4SingleFailureDoesNotDragGroup 覆盖 B2-AC4：单集失败不牵连其他集。
//
// 构造：同组两集都落 n1；E02 的 URL 命中失败注入（403 = 不可重试）。断言：
//   - E02 进 `failed_terminal`；
//   - E01 的 `node_id` 与组绑定**都不变**（不因一集失败而整组重绑/迁移）。
func TestB2AC4SingleFailureDoesNotDragGroup(t *testing.T) {
	st := openTestStore(t)
	n1 := newFakeNode(t)
	n2 := newFakeNode(t)
	id1 := addNode(t, st, "n1", n1.srv.URL, 3)
	addNode(t, st, "n2", n2.srv.URL, 3)

	addTask(t, st, "E01", "Show.S01E01.1080p")
	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	// 让 E02 的投递失败（它的 url_raw 是 http://node.test/E02）。
	n1.setFailSubstr("/E02")

	addTask(t, st, "E02", "Show.S01E02.1080p")
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("次轮失败: %v", err)
	}

	if s := taskState(t, st, "E02"); s != "failed_terminal" {
		t.Fatalf("不可重试的失败应进 failed_terminal，实际 %q", s)
	}
	g := readGroup(t, st)
	if g.State != "active" || g.Epoch != 1 {
		t.Fatalf("一集失败把整组拖走了：state=%q epoch=%d", g.State, g.Epoch)
	}
	if got := taskNodeID(t, st, "E01"); got == nil || *got != id1 {
		t.Fatalf("E01 落点被改动：%v", showI64(got))
	}
	if cnt := countBindingLogs(t, st); cnt != 1 {
		t.Fatalf("一集失败不应新增绑定日志，实际 %d 条", cnt)
	}
	_ = n2
}

// TestB2AC5TwentyFourEpisodesNoOversell 覆盖 B2-AC5：24 集同毫秒并发不超卖。
//
// 构造：单节点 `max_concurrent=3`、组内上限 100（隔离变量），一次入队 24 集，
// 同一轮 TickOnce 全部处理。断言：
//   - 该节点在跑任务数 ≤ 3；
//   - 其余任务保持 queued 且 node_id 为 NULL；
//   - 无 `UNIQUE` 冲突异常（TickOnce 不返回错误即为无异常）。
//
// 反证：把 [TickOnce] 的 tick 级预留去掉（回到"派一个就回滚一个"）⇒ 24 集会在
// 同一轮里全部通过 C4 判定 ⇒ 落点数变成 24 ⇒ 红。
func TestB2AC5TwentyFourEpisodesNoOversell(t *testing.T) {
	st := openTestStore(t)
	n1 := newFakeNode(t)
	id1 := addNode(t, st, "n1", n1.srv.URL, 3)
	// 先派一集把组建出来、绑到 n1，再把它标成 completed 释放节点容量，
	// 然后一次性入队 24 集 —— 这样"24 集同毫秒"才是唯一被检验的变量。
	addTask(t, st, "seed", "Show.S01E00.1080p")
	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	if _, err := st.Execute(context.Background(),
		`UPDATE task_group SET max_concurrent_in_group = 100`); err != nil {
		t.Fatalf("改组内上限失败: %v", err)
	}
	if _, err := st.Execute(context.Background(),
		`UPDATE task SET internal_state='completed', permillage=1000 WHERE task_id='seed'`); err != nil {
		t.Fatalf("释放 seed 容量失败: %v", err)
	}

	for i := 1; i <= 24; i++ {
		addTask(t, st, "EP"+twoDigits(i), "Show.S01E"+twoDigits(i)+".1080p")
	}

	e.cfg.Batch = 40
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce 失败（唯一约束冲突也会走这里）: %v", err)
	}

	rows, err := st.Query(context.Background(),
		`SELECT COUNT(1) FROM task
		  WHERE node_id = ?1 AND internal_state IN ('dispatching','running')`, id1)
	if err != nil {
		t.Fatalf("读在跑任务数失败: %v", err)
	}
	if got := rows[0][0].(int64); got > 3 {
		t.Fatalf("节点在跑 %d 个任务，超过 max_concurrent=3 —— 超卖", got)
	}
	qrows, err := st.Query(context.Background(),
		`SELECT COUNT(1) FROM task WHERE task_id LIKE 'EP%' AND internal_state='queued' AND node_id IS NULL`)
	if err != nil {
		t.Fatalf("读排队数失败: %v", err)
	}
	if got := qrows[0][0].(int64); got < 21 {
		t.Fatalf("应有 ≥21 集排队，实际 %d（说明超额派发了）", got)
	}
}

// twoDigits 把 1..99 补成两位（任务名要能被分组识别器认出来）。
func twoDigits(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// countEvents 数 `event_log` 里某类告警的条数。
func countEvents(t *testing.T, st *store.Store, category, level string) int {
	t.Helper()
	rows, err := st.Query(context.Background(),
		`SELECT COUNT(1) FROM event_log WHERE category = ?1 AND level = ?2`, category, level)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	return int(rows[0][0].(int64))
}

func derefStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
