package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/secret"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// fakeNode 是最小可用的假 BitComet 节点：只实现投递与回读需要的 5 个端点。
//
// 为什么不用 mock 客户端：`dispatch.Deps.NewClient` 返回的是具体类型 `*protocol.Client`，
// 想替换它就得引入接口——那会让"生产用的客户端"和"测试用的客户端"分成两条代码路径，
// 而握手/信封解析恰恰是最容易在两条路径间漂移的地方。这里用真客户端打假服务端，
// 保住的正是那段最容易错的代码。
type fakeNode struct {
	srv *httptest.Server

	mu    sync.Mutex
	tasks []map[string]any
	next  int
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	f := &fakeNode{next: 100}
	mux := http.NewServeMux()

	// 三段式握手第 1 段：返回 32 位 invite_token（与真节点同形）。
	mux.HandleFunc("/api/webui/login", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"error_code":   "OK",
			"invite_token": "0123456789abcdef0123456789abcdef",
		})
	})
	// 第 2 段：换 device_token。
	mux.HandleFunc("/api/device_token/get", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"error_code": "OK", "device_token": "dev-token"})
	})
	// 投递：返回递增的 task_id，并记进列表（这样 read-after-write 能命中）。
	add := func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.next++
		id := f.next
		f.tasks = append(f.tasks, map[string]any{
			"task_id":    itoa(id),
			"name":       nameOf(body),
			"state":      "running",
			"permillage": 0,
			"total_size": 0,
		})
		f.mu.Unlock()
		writeJSON(w, map[string]any{"error_code": "OK", "task_id": itoa(id)})
	}
	mux.HandleFunc("/api/task/http/add", add)
	mux.HandleFunc("/api/task/torrent_links/add", add)

	// 回读：扁平信封，tasks 在顶层。
	mux.HandleFunc("/api_v2/task_list/get", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		tasks := make([]any, 0, len(f.tasks))
		for _, task := range f.tasks {
			cp := map[string]any{}
			for k, v := range task {
				cp[k] = v
			}
			tasks = append(tasks, cp)
		}
		f.mu.Unlock()
		writeJSON(w, map[string]any{
			"error_code": "OK", "tasks": tasks, "filtered_task_count": len(tasks),
		})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// setProgress 让假节点上的第 i 个任务直接变成"已完成"，供回读测试用。
func (f *fakeNode) setProgress(i int, state string, permillage int64, downloaded int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < len(f.tasks) {
		f.tasks[i]["state"] = state
		f.tasks[i]["permillage"] = permillage
		f.tasks[i]["download_size"] = downloaded
	}
}

func writeJSON(w http.ResponseWriter, v map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func nameOf(body map[string]any) string {
	if s, ok := body["url"].(string); ok {
		return s
	}
	if s, ok := body["torrent_links"].(string); ok {
		return s
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// openTestStore 建一个带真实迁移的临时库。
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "b1.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// addNode 插一个节点（默认 online / enabled / weight=1 / max=3）。
func addNode(t *testing.T, st *store.Store, alias, baseURL string, maxConcurrent int64) int64 {
	t.Helper()
	box := secret.WithKey("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	enc, err := box.Wrap("node-pass")
	if err != nil {
		t.Fatalf("加密节点密码失败: %v", err)
	}
	now := time.Now().Unix()
	if _, err := st.Execute(context.Background(),
		`INSERT INTO node (alias, base_url, user, pass_enc, weight, max_concurrent, role,
		                   enabled, state, created_at, updated_at)
		 VALUES (?1, ?2, 'admin', ?3, 1.0, ?4, 'generic', 1, 'online', ?5, ?5)`,
		alias, baseURL, enc, maxConcurrent, now,
	); err != nil {
		t.Fatalf("插节点失败: %v", err)
	}
	var id int64
	rows, err := st.Query(context.Background(), `SELECT node_id FROM node WHERE alias = ?1`, alias)
	if err != nil || len(rows) == 0 {
		t.Fatalf("读节点 id 失败: %v", err)
	}
	id = rows[0][0].(int64)
	return id
}

// addTask 插一个排队任务。
func addTask(t *testing.T, st *store.Store, taskID, name string) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := st.Execute(context.Background(),
		`INSERT INTO task (task_id, gid, kind, url_raw, name, internal_state, aria_status,
		                   created_at, updated_at)
		 VALUES (?1, ?2, 'http', ?3, ?4, 'queued', 'waiting', ?5, ?5)`,
		taskID, "gid-"+taskID, "http://node.test/"+taskID, name, now,
	); err != nil {
		t.Fatalf("插任务失败: %v", err)
	}
}

// newEngine 构造一个只跑一轮的引擎（Tick 设得很大，测试里手动调 TickOnce）。
func newEngine(t *testing.T, st *store.Store) *Engine {
	t.Helper()
	box := secret.WithKey("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	cfg := DefaultConfig()
	cfg.Tick = time.Hour
	cfg.PollInterval = time.Hour
	cfg.VerifyTimeout = 500 * time.Millisecond
	return New(st, box, cfg)
}

// TestTickOnceDeliversTask 是 B1-AC1 的最小形态：
// 排队任务经过一轮调度后必须落点、拿到 node_task_id、且 task_attempt 前移。
func TestTickOnceDeliversTask(t *testing.T) {
	st := openTestStore(t)
	n := newFakeNode(t)
	addNode(t, st, "n1", n.srv.URL, 3)
	addTask(t, st, "T1", "Some.Movie.2026.1080p")

	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce 失败: %v", err)
	}

	rows, err := st.Query(context.Background(),
		`SELECT node_id, node_task_id, internal_state FROM task WHERE task_id = 'T1'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读任务失败: %v", err)
	}
	if rows[0][0] == nil {
		t.Fatal("task.node_id 仍为 NULL —— 任务没有被真正投递")
	}
	if s, _ := rows[0][1].(string); s == "" {
		t.Fatal("task.node_task_id 为空 —— 投递没有拿到节点 id")
	}

	// B1-AC1 要求的 attempt 状态
	arows, err := st.Query(context.Background(),
		`SELECT state FROM task_attempt WHERE task_id = 'T1' AND attempt_no = 1`)
	if err != nil || len(arows) != 1 {
		t.Fatalf("读尝试失败: %v", err)
	}
	if got := arows[0][0].(string); got != "created" && got != "running" {
		t.Fatalf("task_attempt.state = %q，期望 created/running", got)
	}

	// B1-AC5：决策必须留痕
	drows, err := st.Query(context.Background(),
		`SELECT decision, chosen_node, candidates FROM dispatch_log WHERE task_id = 'T1'`)
	if err != nil || len(drows) != 1 {
		t.Fatalf("读决策日志失败: %v", err)
	}
	if drows[0][0].(string) != "assigned" {
		t.Fatalf("dispatch_log.decision = %v，期望 assigned", drows[0][0])
	}
	if drows[0][1] == nil {
		t.Fatal("dispatch_log.chosen_node 为空 —— 违反 AC5 谓词 1")
	}
}

// TestSameAffinityKeyLandsOnOneNode 是 B1-AC4：同组同节点。
//
// 构造：两个节点都健康，两部**同一季不同集**的任务先后入队。
// 若分组或建绑失效，两条任务会按评分各落一个节点 ⇒ COUNT(DISTINCT node_id)=2 ⇒ 红。
func TestSameAffinityKeyLandsOnOneNode(t *testing.T) {
	st := openTestStore(t)
	n1 := newFakeNode(t)
	n2 := newFakeNode(t)
	addNode(t, st, "n1", n1.srv.URL, 3)
	addNode(t, st, "n2", n2.srv.URL, 3)

	addTask(t, st, "E01", "Show.Name.S01E01.1080p.WEB-DL")
	addTask(t, st, "E02", "Show.Name.S01E02.1080p.WEB-DL")

	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce 失败: %v", err)
	}

	rows, err := st.Query(context.Background(),
		`SELECT COUNT(DISTINCT node_id) FROM task
		  WHERE task_id IN ('E01','E02') AND node_id IS NOT NULL`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读任务落点失败: %v", err)
	}
	if got := rows[0][0].(int64); got != 1 {
		t.Fatalf("同组任务落在 %d 个节点上，期望 1（同组同节点失效）", got)
	}

	// 建绑必须留痕（否则"为什么都在同一台"无法回答）
	brows, err := st.Query(context.Background(),
		`SELECT action, reason FROM group_binding_log`)
	if err != nil || len(brows) != 1 {
		t.Fatalf("绑定日志条数=%d，期望 1: %v", len(brows), err)
	}
	if brows[0][0].(string) != "bind" || brows[0][1].(string) != "initial_assign" {
		t.Fatalf("绑定日志 = %v/%v，期望 bind/initial_assign", brows[0][0], brows[0][1])
	}
}

// TestSchedulerOffKeepsQueued 是 B1-AC7：开关关掉后不得投递。
//
// 若把开关做成恒 true，这条会红（任务仍会落点）。
func TestSchedulerOffKeepsQueued(t *testing.T) {
	st := openTestStore(t)
	n := newFakeNode(t)
	addNode(t, st, "n1", n.srv.URL, 3)
	addTask(t, st, "T1", "Some.Movie.2026.1080p")

	t.Setenv("DISPATCH_SCHEDULER", "off")

	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce 失败: %v", err)
	}

	rows, _ := st.Query(context.Background(),
		`SELECT node_id FROM task WHERE task_id = 'T1'`)
	if rows[0][0] != nil {
		t.Fatal("开关关闭时任务仍被投递（node_id 非空）")
	}
	drows, _ := st.Query(context.Background(),
		`SELECT COUNT(1) FROM dispatch_log WHERE decision = 'assigned'`)
	if drows[0][0].(int64) != 0 {
		t.Fatal("开关关闭时仍写了 assigned 决策")
	}
}

// TestNoOnlineNodeRecordsQueuedDecision 守「候选为空 ⇒ 排队且留痕」。
func TestNoOnlineNodeRecordsQueuedDecision(t *testing.T) {
	st := openTestStore(t)
	n := newFakeNode(t)
	addNode(t, st, "n1", n.srv.URL, 3)
	// 把节点改成 degraded（C2 收紧后不可下发）
	if _, err := st.Execute(context.Background(),
		`UPDATE node SET state = 'degraded'`); err != nil {
		t.Fatalf("改节点状态失败: %v", err)
	}
	addTask(t, st, "T1", "Some.Movie.2026.1080p")

	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce 失败: %v", err)
	}

	rows, _ := st.Query(context.Background(),
		`SELECT internal_state, node_id, dispatch_reason FROM task WHERE task_id = 'T1'`)
	if rows[0][0].(string) != "queued" {
		t.Fatalf("internal_state = %v，期望 queued（不能标失败）", rows[0][0])
	}
	if rows[0][1] != nil {
		t.Fatal("无候选时不应有 node_id")
	}
	if rows[0][2] != "affinity_wait" {
		t.Fatalf("dispatch_reason = %v，期望 affinity_wait", rows[0][2])
	}
	drows, _ := st.Query(context.Background(),
		`SELECT decision, reason, chosen_node FROM dispatch_log WHERE task_id = 'T1'`)
	if len(drows) != 1 || drows[0][0].(string) != "queued" {
		t.Fatalf("决策日志 = %v，期望一条 queued", drows)
	}
	if drows[0][2] != nil {
		t.Fatal("queued 决策不得带 chosen_node（违反 AC5 谓词 2）")
	}
	// 排队时**不写** task_attempt（design/02 §3.2.1(3)）
	arows, _ := st.Query(context.Background(),
		`SELECT COUNT(1) FROM task_attempt WHERE task_id = 'T1'`)
	if arows[0][0].(int64) != 0 {
		t.Fatal("排队时写了 task_attempt —— 没有投递就不该有尝试记录")
	}
}

// TestReconcileBackfillsProgress 是 B1-AC2 的最小形态：
// 节点上的进度必须被搬回代理库（否则"投出去了但没动静"）。
func TestReconcileBackfillsProgress(t *testing.T) {
	st := openTestStore(t)
	n := newFakeNode(t)
	addNode(t, st, "n1", n.srv.URL, 3)
	addTask(t, st, "T1", "Some.Movie.2026.1080p")

	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce 失败: %v", err)
	}

	// 假节点上的任务"下载到 500‰"
	n.setProgress(0, "running", 500, 512)

	stats, err := e.Poller().RunOnce(context.Background())
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if stats.Applied != 1 {
		t.Fatalf("applied = %d，期望 1（进度没有被写回）", stats.Applied)
	}

	rows, _ := st.Query(context.Background(),
		`SELECT permillage, downloaded_size, internal_state FROM task WHERE task_id = 'T1'`)
	if got := rows[0][0].(int64); got != 500 {
		t.Fatalf("permillage = %d，期望 500", got)
	}
	if got := rows[0][1].(int64); got != 512 {
		t.Fatalf("downloaded_size = %d，期望 512", got)
	}
	if got := rows[0][2].(string); got != "running" {
		t.Fatalf("internal_state = %q，期望 running", got)
	}

	// task_progress 必须追加一行（进度曲线）
	prows, _ := st.Query(context.Background(),
		`SELECT COUNT(1) FROM task_progress WHERE task_id = 'T1'`)
	if prows[0][0].(int64) != 1 {
		t.Fatalf("task_progress 行数 = %d，期望 1", prows[0][0])
	}
}

// TestReconcileStoppedWithErrorBecomesFailedTerminal 守 R-2 的落库形态：
// 节点报 `stopped` + 错误码 ⇒ `failed_terminal`，**不得**停在 `running`/`queued`。
func TestReconcileStoppedWithErrorBecomesFailedTerminal(t *testing.T) {
	st := openTestStore(t)
	n := newFakeNode(t)
	addNode(t, st, "n1", n.srv.URL, 3)
	addTask(t, st, "T1", "Some.Movie.2026.1080p")

	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce 失败: %v", err)
	}

	n.mu.Lock()
	n.tasks[0]["state"] = "stopped"
	n.tasks[0]["error_code"] = "server_file_not_found"
	n.mu.Unlock()

	if _, err := e.Poller().RunOnce(context.Background()); err != nil {
		t.Fatalf("回读失败: %v", err)
	}

	rows, _ := st.Query(context.Background(),
		`SELECT internal_state, error_class, aria_status FROM task WHERE task_id = 'T1'`)
	if rows[0][0].(string) != "failed_terminal" {
		t.Fatalf("internal_state = %q，期望 failed_terminal（旧实现会停在 queued）", rows[0][0])
	}
	if rows[0][1] == nil || rows[0][1].(string) == "none" {
		t.Fatalf("error_class = %v，期望非 none", rows[0][1])
	}
	if rows[0][2].(string) != "error" {
		t.Fatalf("aria_status = %q，期望 error（失败被藏起来了）", rows[0][2])
	}
}

// TestDeliverFailureIsNotSilent 是 B1-AC6 的最小形态：
// 节点回 404 类错误 ⇒ 终态失败且 error_class 非 none，绝不能停在 queued+attempt_no=0。
func TestDeliverFailureIsNotSilent(t *testing.T) {
	st := openTestStore(t)
	// 一个只会拒绝投递的节点
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/webui/login":
			writeJSON(w, map[string]any{"error_code": "OK", "invite_token": "0123456789abcdef0123456789abcdef"})
		case "/api/device_token/get":
			writeJSON(w, map[string]any{"error_code": "OK", "device_token": "t"})
		default:
			writeJSON(w, map[string]any{
				"error_code": "server_file_not_found", "error_message": "404",
			})
		}
	}))
	defer srv.Close()

	addNode(t, st, "n1", srv.URL, 3)
	addTask(t, st, "T1", "Some.Movie.2026.1080p")

	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce 失败: %v", err)
	}

	rows, _ := st.Query(context.Background(),
		`SELECT internal_state, error_class, attempt_no FROM task WHERE task_id = 'T1'`)
	if rows[0][0].(string) != "failed_terminal" {
		t.Fatalf("internal_state = %q，期望 failed_terminal", rows[0][0])
	}
	if rows[0][1] == nil || rows[0][1].(string) == "none" {
		t.Fatalf("error_class = %v，期望非 none", rows[0][1])
	}
	if rows[0][2].(int64) < 1 {
		t.Fatalf("attempt_no = %d，期望 ≥1", rows[0][2])
	}
}

// TestGroupingWorksWhenNameIsNull 守一条**端到端才暴露**的缺陷：
// aria2 `addUri` 不传 `out` 时，入库的 `task.name` 是 NULL（A 类刻意不拿 URL 猜文件名）。
// 若分组识别只读 `task.name`，整条 URL 推送**都不建组** ——
// 「同剧同节点」在手工测试里看起来"没生效"，实际是根本没分组。
//
// 单元测试抓不到它：那些用例都显式给了 name。只有真推一条不带 out 的任务才会暴露。
func TestGroupingWorksWhenNameIsNull(t *testing.T) {
	st := openTestStore(t)
	n := newFakeNode(t)
	addNode(t, st, "n1", n.srv.URL, 3)

	// name 显式写 NULL，url 里带季集标记
	now := time.Now().Unix()
	if _, err := st.Execute(context.Background(),
		`INSERT INTO task (task_id, gid, kind, url_raw, name, internal_state, aria_status,
		                   created_at, updated_at)
		 VALUES ('T1', 'g1', 'http', 'https://example.invalid/Show.Name.S01E01.1080p.mkv',
		         NULL, 'queued', 'waiting', ?1, ?1)`, now,
	); err != nil {
		t.Fatalf("插任务失败: %v", err)
	}

	e := newEngine(t, st)
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce 失败: %v", err)
	}

	rows, err := st.Query(context.Background(),
		`SELECT affinity_key, bound_node_id FROM task_group`)
	if err != nil {
		t.Fatalf("读组失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("组数 = %d，期望 1（name 为 NULL 时也应从 url 派生组键）", len(rows))
	}
	if got := rows[0][0].(string); got != "name:Show Name|S01" {
		t.Fatalf("affinity_key = %q，期望 name:Show Name|S01", got)
	}
	if rows[0][1] == nil {
		t.Fatal("组未绑定节点")
	}
}
