// 管理口接线的端到端用例：真起 handler、真登录、真读写库。
//
// 重点钉住三件容易静默退化的事：
//  1. **管理台不得产生任务** —— `POST /api/admin/tasks` 必须 405、`/tasks/new` 必须 404；
//  2. 口令门禁真的在跑 —— 错口令 401、对口令发 `sid` Cookie；
//  3. 只读接口能把库里的任务读出来（`internal/tasks` 的读路径真的接上了）。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/config"
	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/state"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
	"github.com/LIUBANGJIAN/download-gateway/internal/tasks"
)

const testAdminPassword = "ci-smoke-pass-9f2c"

// newAdminEnv 建真实库 + 管理口 handler + 已登录客户端。
func newAdminEnv(t *testing.T) (*httptest.Server, *http.Client, *store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "dispatch.db")
	st, err := store.Open(dbPath, filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("打开真实库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	policy := &config.AdminPolicy{
		Password:        testAdminPassword,
		Generated:       false,
		CookieSecure:    config.CookieSecureNever,
		AllowFileDelete: false,
	}
	env := config.EnvSnapshot{
		PublicAddr:              "0.0.0.0:6800",
		AdminAddr:               "127.0.0.1:8080",
		DBPath:                  dbPath,
		MigrationsDir:           "migrations",
		LogLevel:                "info",
		AdminPasswordConfigured: true,
		AdminCookieSecure:       config.CookieSecureNever,
	}
	as := state.NewAdminState(st, ids.NowSecs(), policy, env, nil, nil)
	srv := httptest.NewServer(NewRouter(as, "0.1.3"))
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("建 cookie jar 失败: %v", err)
	}
	return srv, &http.Client{Jar: jar}, st
}

func doReq(t *testing.T, c *http.Client, method, url, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("造请求失败: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, buf
}

// TestAdminLoginGates 验证口令门禁与 `sid` Cookie。
func TestAdminLoginGates(t *testing.T) {
	srv, client, _ := newAdminEnv(t)

	status, body := doReq(t, client, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"definitely-not-the-password"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("错误口令应 401，实际 %d %s", status, body)
	}
	if !strings.Contains(string(body), "40100") {
		t.Fatalf("错误口令应回 40100 信封，实际 %s", body)
	}

	status, body = doReq(t, client, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+testAdminPassword+`"}`)
	if status != http.StatusOK {
		t.Fatalf("正确口令应 200，实际 %d %s", status, body)
	}
	// cookiejar 已按响应把 Cookie 存下；此处直接打受保护接口验证它生效。
	status, body = doReq(t, client, http.MethodGet, srv.URL+"/api/admin/summary", "")
	if status != http.StatusOK || !strings.Contains(string(body), `"task_total"`) {
		t.Fatalf("登录后 summary 应 200，实际 %d %s", status, body)
	}
}

// TestProtectedReadRequiresSession 验证未登录访问只读接口必须 401。
func TestProtectedReadRequiresSession(t *testing.T) {
	srv, _, _ := newAdminEnv(t)
	fresh := &http.Client{}
	status, body := doReq(t, fresh, http.MethodGet, srv.URL+"/api/admin/tasks", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("未登录访问 tasks 应 401，实际 %d %s", status, body)
	}
}

// TestAdminCannotCreateTask 是产品铁律的可执行证据。
func TestAdminCannotCreateTask(t *testing.T) {
	srv, client, _ := newAdminEnv(t)
	if status, body := doReq(t, client, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+testAdminPassword+`"}`); status != http.StatusOK {
		t.Fatalf("登录失败: %d %s", status, body)
	}

	status, body := doReq(t, client, http.MethodPost, srv.URL+"/api/admin/tasks",
		`{"url":"http://203.0.113.9/x.torrent"}`)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/admin/tasks 必须 405（管理台不得产生任务），实际 %d %s", status, body)
	}
	status, body = doReq(t, client, http.MethodGet, srv.URL+"/tasks/new", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET /tasks/new 必须 404（添加任务页已删除），实际 %d %s", status, body)
	}
}

// TestStaticPagesAndAssetsAreEmbedded 证明前端三件套真的编进了二进制。
func TestStaticPagesAndAssetsAreEmbedded(t *testing.T) {
	srv, _, _ := newAdminEnv(t)
	for _, p := range []string{"/", "/login", "/tasks", "/nodes", "/settings", "/app.css", "/app.js"} {
		status, body := doReq(t, &http.Client{}, http.MethodGet, srv.URL+p, "")
		if status != http.StatusOK || len(body) == 0 {
			t.Fatalf("%s 应 200 且非空，实际 %d（%d 字节）", p, status, len(body))
		}
	}
}

// TestTaskListReadsFromDB 证明管理口的读路径真的接到 `internal/tasks`。
func TestTaskListReadsFromDB(t *testing.T) {
	srv, client, st := newAdminEnv(t)
	if status, body := doReq(t, client, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+testAdminPassword+`"}`); status != http.StatusOK {
		t.Fatalf("登录失败: %d %s", status, body)
	}

	// 直接从「唯一入库入口」写一条，再从 HTTP 读回来。
	if _, err := tasks.CreateTask(t.Context(), st, tasks.TaskCreateInput{
		Kind:   domain.KindHTTP,
		URLRaw: "http://example.test/from-db.iso",
		Origin: domain.OriginAria2,
	}); err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}

	status, body := doReq(t, client, http.MethodGet, srv.URL+"/api/admin/tasks", "")
	if status != http.StatusOK {
		t.Fatalf("tasks 列表应 200，实际 %d %s", status, body)
	}
	if !strings.Contains(string(body), `"total":1`) || !strings.Contains(string(body), "from-db.iso") {
		t.Fatalf("列表应含刚写入的 1 条任务，实际 %s", body)
	}
	// 分页参数非法必须显式 422，不许静默 clamp。
	status, body = doReq(t, client, http.MethodGet, srv.URL+"/api/admin/tasks?page=0", "")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("page=0 应 422，实际 %d %s", status, body)
	}
}

// TestPolicyGetAndValidation 覆盖策略读 + 三类必须 422 的校验。
func TestPolicyGetAndValidation(t *testing.T) {
	srv, client, _ := newAdminEnv(t)
	if status, body := doReq(t, client, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+testAdminPassword+`"}`); status != http.StatusOK {
		t.Fatalf("登录失败: %d %s", status, body)
	}

	status, body := doReq(t, client, http.MethodGet, srv.URL+"/api/admin/config", "")
	if status != http.StatusOK {
		t.Fatalf("config 应 200，实际 %d %s", status, body)
	}
	var cfg struct {
		Policies []map[string]any `json:"policies"`
		Env      []map[string]any `json:"env"`
	}
	// 信封是 {code,data:{policies,env}}，先取 data 再解。
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("config 响应不是 JSON: %v", err)
	}
	if err := json.Unmarshal(envelope.Data, &cfg); err != nil {
		t.Fatalf("config.data 解析失败: %v", err)
	}
	if len(cfg.Policies) != len(catalog) || len(cfg.Env) == 0 {
		t.Fatalf("应返回 %d 条策略与若干环境项，实际 %d / %d", len(catalog), len(cfg.Policies), len(cfg.Env))
	}

	// 保存合法值 → 回读到 300。
	status, body = doReq(t, client, http.MethodPut, srv.URL+"/api/admin/config",
		`{"policies":[{"key":"least_tasks","enabled":false,"priority":300}]}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"priority":300`) {
		t.Fatalf("保存策略应 200 且回读 300，实际 %d %s", status, body)
	}

	cases := []struct{ name, payload string }{
		{"优先级越界", `{"policies":[{"key":"least_tasks","priority":99999}]}`},
		{"未知策略键", `{"policies":[{"key":"no_such_policy","enabled":true}]}`},
		{"关闭安全阀", `{"policies":[{"key":"max_concurrent","enabled":false}]}`},
	}
	for _, c := range cases {
		status, body := doReq(t, client, http.MethodPut, srv.URL+"/api/admin/config", c.payload)
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("%s 必须 422，实际 %d %s", c.name, status, body)
		}
	}
}

// TestNodesListNeverLeaksSecrets 钉住「密文不回显」。
func TestNodesListNeverLeaksSecrets(t *testing.T) {
	srv, client, st := newAdminEnv(t)
	if status, body := doReq(t, client, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+testAdminPassword+`"}`); status != http.StatusOK {
		t.Fatalf("登录失败: %d %s", status, body)
	}
	if _, err := st.Execute(t.Context(),
		"INSERT INTO node (alias, base_url, user, pass_enc, max_rate_bytes, created_at, updated_at) "+
			"VALUES ('n1','http://203.0.113.77:9085','u','ciphered-blob',524288,1,1)"); err != nil {
		t.Fatalf("插入节点失败: %v", err)
	}
	status, body := doReq(t, client, http.MethodGet, srv.URL+"/api/admin/nodes", "")
	if status != http.StatusOK {
		t.Fatalf("nodes 应 200，实际 %d %s", status, body)
	}
	if strings.Contains(string(body), "pass_enc") || strings.Contains(string(body), "ciphered-blob") {
		t.Fatalf("节点列表不得泄露密文，实际 %s", body)
	}
	if !strings.Contains(string(body), `"max_rate_kbps":512`) {
		t.Fatalf("限速应折算为 512 KB/s，实际 %s", body)
	}
}
