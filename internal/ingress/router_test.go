// 对外口接线的端到端用例：**真起 HTTP handler，真写 SQLite**。
//
// # 为什么不是「mock 一下 store」
//
// 本轮要证明的事情是「`internal/tasks` 真的接到 HTTP 上了」，mock 掉 store 只能证明
// handler 会调函数、证明不了数据真的落库并被读回来。所以这里用真实迁移建库、
// 真实请求打 `httptest.Server`，断言的是**可观测终态**：
//
//	addUri 返回的 GID → tellStatus 能查到同一条 → remove 后 tellStopped 仍能查到。
package ingress

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/handshake"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/state"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

const testToken = "ci-smoke-public-token-2f7a"

// newPublicServer 建真实库 + 真实 handler。
func newPublicServer(t *testing.T, token *string) *httptest.Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "dispatch.db")
	st, err := store.Open(dbPath, filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("打开真实库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ps := state.NewPublicState(st, ids.NowSecs(), token, false, state.HandshakeState{
		Tokens: handshake.NewStore(),
	})
	srv := httptest.NewServer(NewRouter(ps, "0.1.3"))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, contentType string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("造请求失败: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
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

func token() *string {
	v := testToken
	return &v
}

// get 打一次 GET 并读全响应体。
func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
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

// jsonRPC 打一次 `/jsonrpc`，返回解析后的对象。
func jsonRPC(t *testing.T, srv *httptest.Server, payload string) map[string]any {
	t.Helper()
	status, body := post(t, srv.URL+"/jsonrpc", "application/json", []byte(payload), nil)
	if status != http.StatusOK {
		t.Fatalf("jsonrpc 应恒返 200，实际 %d：%s", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v；原文 %s", err, body)
	}
	return out
}

// TestAria2AddStatusRemoveRoundTrip 是本轮接线的**核心证据链**。
func TestAria2AddStatusRemoveRoundTrip(t *testing.T) {
	srv := newPublicServer(t, token())

	// ① addUri：未带 token ⇒ 必须被拒（code=-1），且不得返回 result。
	got := jsonRPC(t, srv, `{"jsonrpc":"2.0","id":"1","method":"aria2.addUri","params":[["http://example.test/a.iso"]]}`)
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -1 {
		t.Fatalf("未带 token 应回 code=-1，实际 %v", got)
	}
	if _, has := got["result"]; has {
		t.Fatalf("被拒时不得返回 result：%v", got)
	}

	// ② 带正确 token ⇒ 返回 GID。
	got = jsonRPC(t, srv, `{"jsonrpc":"2.0","id":"2","method":"aria2.addUri","params":["token:`+testToken+`",["http://example.test/a.iso"],{"out":"a.iso","dir":"/downloads"}]}`)
	gid, _ := got["result"].(string)
	if len(gid) != 16 {
		t.Fatalf("addUri 应返回 16 位 GID，实际 %v", got)
	}

	// ③ tellStatus 必须能查到同一条，且 `out` 落到 name。
	got = jsonRPC(t, srv, `{"jsonrpc":"2.0","id":"3","method":"aria2.tellStatus","params":["token:`+testToken+`","`+gid+`"]}`)
	result, _ := got["result"].(map[string]any)
	if result == nil {
		t.Fatalf("tellStatus 应返回 result，实际 %v", got)
	}
	if result["gid"] != gid {
		t.Fatalf("tellStatus 的 gid 应等于 addUri 返回值：want %s got %v", gid, result["gid"])
	}
	if result["status"] != "waiting" {
		t.Fatalf("新任务状态应为 waiting，实际 %v", result["status"])
	}
	// 诚实字段：代理不知道节点落盘路径。
	if result["dir"] != "" || result["dirKnown"] != false {
		t.Fatalf("dir 必须为空串且 dirKnown=false，实际 %v / %v", result["dir"], result["dirKnown"])
	}
	if result["dispatched"] != false {
		t.Fatalf("dispatched 必须为 false，实际 %v", result["dispatched"])
	}

	// ④ remove 是**软删**：返回 GID，且 tellStopped 仍能查到。
	got = jsonRPC(t, srv, `{"jsonrpc":"2.0","id":"4","method":"aria2.remove","params":["token:`+testToken+`","`+gid+`"]}`)
	if got["result"] != gid {
		t.Fatalf("remove 应回被删 GID，实际 %v", got)
	}
	got = jsonRPC(t, srv, `{"jsonrpc":"2.0","id":"5","method":"aria2.tellStopped","params":["token:`+testToken+`",0,10]}`)
	stopped, _ := got["result"].([]any)
	if len(stopped) != 1 {
		t.Fatalf("tellStopped 应有 1 条（软删后仍在），实际 %v", got["result"])
	}

	// ⑤ removeDownloadResult 才是硬删：之后 tellStopped 为空。
	got = jsonRPC(t, srv, `{"jsonrpc":"2.0","id":"6","method":"aria2.removeDownloadResult","params":["token:`+testToken+`","`+gid+`"]}`)
	if got["result"] != "OK" {
		t.Fatalf("removeDownloadResult 应回 OK，实际 %v", got)
	}
	got = jsonRPC(t, srv, `{"jsonrpc":"2.0","id":"7","method":"aria2.tellStopped","params":["token:`+testToken+`",0,10]}`)
	if arr, _ := got["result"].([]any); len(arr) != 0 {
		t.Fatalf("硬删后 tellStopped 应为空，实际 %v", got["result"])
	}
}

// TestAria2MethodNotFoundIsRPCError 钉住「未实现方法返 -32601，绝不 404」。
func TestAria2MethodNotFoundIsRPCError(t *testing.T) {
	srv := newPublicServer(t, nil)
	got := jsonRPC(t, srv, `{"jsonrpc":"2.0","id":"x","method":"aria2.noSuchMethod","params":[]}`)
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32601 {
		t.Fatalf("未实现方法应回 -32601，实际 %v", got)
	}
}

// TestAria2MulticallCapAndTokenIsolation 钉住 multicall 的两条承重语义：
// 子调用上限 32、外层不校验 token（token 由每个子调用各自携带）。
func TestAria2MulticallCapAndTokenIsolation(t *testing.T) {
	srv := newPublicServer(t, token())

	// 33 条 ⇒ 超限拒绝。
	calls := make([]string, 33)
	for i := range calls {
		calls[i] = `{"methodName":"aria2.getVersion","params":["token:` + testToken + `"]}`
	}
	payload := `{"jsonrpc":"2.0","id":"m","method":"system.multicall","params":[[` + strings.Join(calls, ",") + `]]}`
	got := jsonRPC(t, srv, payload)
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32600 {
		t.Fatalf("33 条子调用应回 -32600，实际 %v", got)
	}

	// 2 条正常子调用：外层不带 token，子调用各带 ⇒ 应成功。
	payload = `{"jsonrpc":"2.0","id":"m2","method":"system.multicall","params":[[` +
		`{"methodName":"aria2.getVersion","params":["token:` + testToken + `"]},` +
		`{"methodName":"aria2.getGlobalStat","params":["token:` + testToken + `"]}]]}`
	got = jsonRPC(t, srv, payload)
	arr, _ := got["result"].([]any)
	if len(arr) != 2 {
		t.Fatalf("multicall 应返回 2 条结果，实际 %v", got)
	}

	// 子调用 token 错误 ⇒ 该子项是错误对象（不是整包失败）。
	payload = `{"jsonrpc":"2.0","id":"m3","method":"system.multicall","params":[[` +
		`{"methodName":"aria2.getVersion","params":["token:wrong"]}]]}`
	got = jsonRPC(t, srv, payload)
	arr, _ = got["result"].([]any)
	if len(arr) != 1 {
		t.Fatalf("multicall 应返回 1 条结果，实际 %v", got)
	}
	sub, _ := arr[0].(map[string]any)
	if sub["code"] == nil {
		t.Fatalf("子调用 token 错误应回错误对象，实际 %v", sub)
	}
}

// TestBitCometAuthGates 验证 BitComet 面三态：无 Bearer 401 / 错 Bearer 401 / 对 Bearer 放行。
func TestBitCometAuthGates(t *testing.T) {
	srv := newPublicServer(t, token())

	status, body := post(t, srv.URL+"/api/config/about/get", "application/json", []byte(`{}`), nil)
	if status != http.StatusUnauthorized || !strings.Contains(string(body), ECInvalidToken) {
		t.Fatalf("无 Bearer 应 401 INVALID_TOKEN，实际 %d %s", status, body)
	}

	status, _ = post(t, srv.URL+"/api/config/about/get", "application/json", []byte(`{}`),
		map[string]string{"Authorization": "Bearer definitely-wrong"})
	if status != http.StatusUnauthorized {
		t.Fatalf("错误 Bearer 应 401，实际 %d", status)
	}

	status, body = post(t, srv.URL+"/api/config/about/get", "application/json", []byte(`{}`),
		map[string]string{"Authorization": "Bearer " + testToken})
	if status != http.StatusOK || !strings.Contains(string(body), `"error_code":"OK"`) {
		t.Fatalf("正确 Bearer 应 200 OK 信封，实际 %d %s", status, body)
	}
	if !strings.Contains(string(body), `"platform":"proxy"`) {
		t.Fatalf("响应应自称 proxy 平台，实际 %s", body)
	}
}

// TestBitCometAddThenTaskList 证明「入库 → 列表可读」这条业务链真通。
func TestBitCometAddThenTaskList(t *testing.T) {
	srv := newPublicServer(t, token())
	auth := map[string]string{"Authorization": "Bearer " + testToken}

	status, body := post(t, srv.URL+"/api/task/http/add", "application/json",
		[]byte(`{"url":"http://example.test/b.iso","filename":"b.iso"}`), auth)
	if status != http.StatusOK || !strings.Contains(string(body), `"proxy_task_id"`) {
		t.Fatalf("http/add 应 200 且返回 proxy_task_id，实际 %d %s", status, body)
	}

	status, body = post(t, srv.URL+"/api_v2/task_list/get", "application/json", []byte(`{}`), auth)
	if status != http.StatusOK {
		t.Fatalf("task_list/get 应 200，实际 %d %s", status, body)
	}
	if !strings.Contains(string(body), `"filtered_task_count":1`) {
		t.Fatalf("列表中应有 1 条任务，实际 %s", body)
	}
	if !strings.Contains(string(body), "http://example.test/b.iso") {
		t.Fatalf("列表中应含刚提交的 URL，实际 %s", body)
	}
}

// TestIpVerifyNeedsNoCredential 钉住握手第一步免凭据（否则真客户端进不来）。
func TestIpVerifyNeedsNoCredential(t *testing.T) {
	srv := newPublicServer(t, token())
	status, body := post(t, srv.URL+"/api/webui/ip_verify", "", nil, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"error_code":"OK"`) {
		t.Fatalf("握手第一步应免凭据且 200 OK，实际 %d %s", status, body)
	}
}

// TestHealthzReportsVersion 钉住 `/healthz` 自报版本（版本号注入链的观测点）。
func TestHealthzReportsVersion(t *testing.T) {
	srv := newPublicServer(t, nil)
	status, body := get(t, srv.URL+"/healthz")
	_ = status
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("healthz 应返回 JSON，实际 %s", body)
	}
	if out["version"] != "0.1.3" {
		t.Fatalf("healthz 应自报 0.1.3，实际 %v", out["version"])
	}
	if out["status"] != "ok" {
		t.Fatalf("healthz 应 status=ok，实际 %v", out["status"])
	}
}
