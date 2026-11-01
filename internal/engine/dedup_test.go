package engine

// B3 四条验收标准的结果态测试（`design/11 §3.6` B3-AC1..AC4）。

import (
	"context"
	"strings"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// countDedupRows 数 `dedup_index` 行数。
func countDedupRows(t *testing.T, st *store.Store) int {
	t.Helper()
	rows, err := st.Query(context.Background(), `SELECT COUNT(1) FROM dedup_index`)
	if err != nil {
		t.Fatalf("读去重索引失败: %v", err)
	}
	return int(rows[0][0].(int64))
}

// countDispatched 数"真正投递出去"的任务数（node_task_id 非空）。
func countDispatched(t *testing.T, st *store.Store) int {
	t.Helper()
	rows, err := st.Query(context.Background(),
		`SELECT COUNT(1) FROM task WHERE node_task_id IS NOT NULL AND node_task_id <> ''`)
	if err != nil {
		t.Fatalf("读投递数失败: %v", err)
	}
	return int(rows[0][0].(int64))
}

// TestB3AC1AndAC2DedupLedgerAndNoDoubleDelivery 覆盖 B3-AC1（有落账）与 AC2（高置信不重复投递）。
//
// 剧情：同一个 URL 提交两次（两个不同 task_id）。
// 期望：第一个投递；第二个 `deduplicated`、不投递；`dedup_index` 有行；
// `dispatch_log` 有 `deduped` 决策。
//
// 反证：把 `decideDedup` 的调用删掉 ⇒ 两条都会投递（`countDispatched==2`）⇒ 红。
func TestB3AC1AndAC2DedupLedgerAndNoDoubleDelivery(t *testing.T) {
	st := openTestStore(t)
	n := newFakeNode(t)
	addNode(t, st, "n1", n.srv.URL, 5)

	addTask(t, st, "T1", "Movie.2026.1080p.ts")
	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	if got := countDispatched(t, st); got != 1 {
		t.Fatalf("首条应被投递，实际投递数 %d", got)
	}
	if got := countDedupRows(t, st); got == 0 {
		t.Fatal("首条投递后 dedup_index 仍为空 —— B3-AC1 要求命中/未命中都留痕")
	}

	// 同一个任务（同 URL）再提交一次：url_raw 完全相同 ⇒ L2a（exact）命中。
	if _, err := st.Execute(context.Background(),
		`INSERT INTO task (task_id, gid, kind, url_raw, name, internal_state, aria_status, created_at, updated_at)
		 VALUES ('T2', 'gid-T2', 'http', 'http://node.test/T1', 'Movie.2026.1080p.ts', 'queued', 'waiting', 1, 1)`); err != nil {
		t.Fatalf("插重复任务失败: %v", err)
	}
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("次轮失败: %v", err)
	}

	if s := taskState(t, st, "T2"); s != "deduplicated" {
		t.Fatalf("重复任务应进 deduplicated，实际 %q", s)
	}
	if got := taskNodeID(t, st, "T2"); got != nil {
		t.Fatalf("重复任务被投递到节点 %d —— B3-AC2 要求只存在一个真正投递的任务", *got)
	}
	if got := countDispatched(t, st); got != 1 {
		t.Fatalf("真正投递的任务数应为 1，实际 %d", got)
	}
	// 决策必须留 `deduped`（B3-AC1 的落账要求，也是 B1-AC5 的跨阶段子句）
	drows, err := st.Query(context.Background(),
		`SELECT decision, reason FROM dispatch_log WHERE task_id='T2'`)
	if err != nil || len(drows) != 1 {
		t.Fatalf("T2 决策留痕条数=%d，期望 1: %v", len(drows), err)
	}
	if dec, _ := drows[0][0].(string); dec != "deduped" {
		t.Fatalf("decision=%q，期望 deduped", dec)
	}
	if reason, _ := drows[0][1].(string); reason != "dedup_hit_L2a" {
		t.Fatalf("reason=%q，期望 dedup_hit_L2a", reason)
	}
	// 既有任务仍然活着、没被本次判重覆盖
	if s := taskState(t, st, "T1"); s != "running" && s != "dispatching" {
		t.Fatalf("既有任务 T1 状态被改成 %q", s)
	}
}

// TestB3AC3MediumConfidenceIsMarkedNotSkipped 覆盖 B3-AC3。
//
// 构造：两个 URL 文本不同（⇒ L2a 不命中）但归一化后相同（⇒ L2b 命中，medium）。
// 期望：不投递 + `possible_duplicate=1`，且 `dispatch_log` 有明确 reason。
//
// 反证：
//   - 把 medium 也当可跳过（写 deduplicated）⇒ 状态断言红（用户不知道有这个任务）；
//   - 把 medium 忽略掉（照常投递）⇒ 投递数变 2 ⇒ 静默重复，红。
func TestB3AC3MediumConfidenceIsMarkedNotSkipped(t *testing.T) {
	st := openTestStore(t)
	n := newFakeNode(t)
	addNode(t, st, "n1", n.srv.URL, 5)

	// 第一条：带 utm 参数
	if _, err := st.Execute(context.Background(),
		`INSERT INTO task (task_id, gid, kind, url_raw, name, internal_state, aria_status, created_at, updated_at)
		 VALUES ('M1', 'gid-M1', 'http', 'http://node.test/file.ts?utm_source=a', 'A.S01E01.ts', 'queued', 'waiting', 1, 1)`); err != nil {
		t.Fatalf("插任务失败: %v", err)
	}
	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}

	// 第二条：换了个跟踪参数 ⇒ 归一化后与第一条相同
	if _, err := st.Execute(context.Background(),
		`INSERT INTO task (task_id, gid, kind, url_raw, name, internal_state, aria_status, created_at, updated_at)
		 VALUES ('M2', 'gid-M2', 'http', 'http://node.test/file.ts?spm=b', 'A.S01E01.ts', 'queued', 'waiting', 2, 2)`); err != nil {
		t.Fatalf("插任务失败: %v", err)
	}
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("次轮失败: %v", err)
	}

	rows, err := st.Query(context.Background(),
		`SELECT possible_duplicate, internal_state FROM task WHERE task_id='M2'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读任务失败: %v", err)
	}
	if v, _ := rows[0][0].(int64); v != 1 {
		t.Fatal("中置信命中必须置 possible_duplicate=1（否则用户无从知道要裁决）")
	}
	if s, _ := rows[0][1].(string); s != "queued" {
		t.Fatalf("中置信不该进终态，实际 %q", s)
	}
	if got := taskNodeID(t, st, "M2"); got != nil {
		t.Fatalf("中置信命中却投递了（node=%d）—— 静默重复", *got)
	}
	drows, err := st.Query(context.Background(),
		`SELECT decision, reason FROM dispatch_log WHERE task_id='M2' ORDER BY id DESC LIMIT 1`)
	if err != nil || len(drows) == 0 {
		t.Fatalf("M2 无决策留痕: %v", err)
	}
	if r, _ := drows[0][1].(string); r != "possible_duplicate_pending_review_L2b" {
		t.Fatalf("reason=%q，期望 possible_duplicate_pending_review_L2b", r)
	}

	// 待裁决的任务不得被后续 tick 反复处理（否则 dispatch_log 会被刷屏）
	before := countDispatchLog(t, st, "M2")
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("三轮失败: %v", err)
	}
	if after := countDispatchLog(t, st, "M2"); after != before {
		t.Fatalf("待裁决任务被重复处理：%d → %d 条决策", before, after)
	}
}

// TestB3AC4FilenameStructureIsConsistent 覆盖 B3-AC4。
//
// v2 起 `save_folder` 一律不下发（Q3），所以"同组跨节点结构一致"改由
// **下发的 `filename`** 体现：同组两集在节点上拿到的名字必须只差集号与原名。
//
// 反证：把命名器的 stem 段去掉（只拼集号）⇒ 两条文件名首段不同 ⇒ 红。
func TestB3AC4FilenameStructureIsConsistent(t *testing.T) {
	st := openTestStore(t)
	n := newFakeNode(t)
	addNode(t, st, "n1", n.srv.URL, 5)

	// 上游只发 URL（插件不能造名字，F34）⇒ 名字是不规范的 `01.ts`，
	// 剧名只能从 URL 里认。这正是补名器存在的场景。
	addTaskURL(t, st, "F1", "01.ts", "http://node.test/d/Show.S01E01.1080p.WEB-DL.ts")
	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	first := n.lastFilename()
	if first == "" {
		t.Fatal("下发时没有补名 —— B3-AC4 无从体现结构一致（filename 为空）")
	}

	addTaskURL(t, st, "F2", "02.ts", "http://node.test/d/Show.S01E02.1080p.WEB-DL.ts")
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("次轮失败: %v", err)
	}
	second := n.lastFilename()
	if second == "" {
		t.Fatal("第二集没有补名")
	}
	if first == second {
		t.Fatalf("两集拿到同一个文件名 %q —— 会互相覆盖（F33）", first)
	}
	// 结构一致：`<剧名>.<SxxExx>.<原名>`，两集只差集号与原名。
	pa := strings.Split(first, ".")
	pb := strings.Split(second, ".")
	if len(pa) < 3 || len(pb) < 3 {
		t.Fatalf("文件名结构不足三段：%q / %q", first, second)
	}
	if pa[0] != pb[0] {
		t.Fatalf("同组剧名段不同：%q vs %q（B3-AC4 要求结构一致）", first, second)
	}
	if pa[1] == pb[1] {
		t.Fatalf("两集集号相同：%q vs %q", first, second)
	}
	if len(pa[1]) < 3 || len(pb[1]) < 3 || pa[1][:3] != pb[1][:3] {
		t.Fatalf("同组季段不同：%q vs %q", first, second)
	}

	// 补名审计必须落盘（纪律 5：先落盘决策，再下发）
	if got := countEvents(t, st, "group", "info"); got == 0 {
		t.Fatal("补名没有写 event_log —— 补错了也无法事后追溯")
	}
}

// addTaskURL 插一个"名字与 URL 分离"的任务（模拟上游只发 URL 的真实形态）。
func addTaskURL(t *testing.T, st *store.Store, taskID, name, url string) {
	t.Helper()
	if _, err := st.Execute(context.Background(),
		`INSERT INTO task (task_id, gid, kind, url_raw, name, internal_state, aria_status, created_at, updated_at)
		 VALUES (?1, ?2, 'http', ?3, ?4, 'queued', 'waiting', 3, 3)`,
		taskID, "gid-"+taskID, url, name,
	); err != nil {
		t.Fatalf("插任务失败: %v", err)
	}
}

// countDispatchLog 数某任务的决策条数。
func countDispatchLog(t *testing.T, st *store.Store, taskID string) int {
	t.Helper()
	rows, err := st.Query(context.Background(),
		`SELECT COUNT(1) FROM dispatch_log WHERE task_id = ?1`, taskID)
	if err != nil {
		t.Fatalf("读决策条数失败: %v", err)
	}
	return int(rows[0][0].(int64))
}
