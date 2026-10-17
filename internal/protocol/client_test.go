package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type captured struct {
	method string
	path   string
	body   string
	header http.Header
}

type recorder struct {
	mu    sync.Mutex
	items []captured
}

func (r *recorder) record(req *http.Request) []byte {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.items = append(r.items, captured{
		method: req.Method,
		path:   req.URL.Path,
		body:   string(body),
		header: req.Header.Clone(),
	})
	r.mu.Unlock()
	return body
}

func (r *recorder) byPath(path string) []captured {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []captured
	for _, it := range r.items {
		if it.path == path {
			out = append(out, it)
		}
	}
	return out
}

func profileFor(t *testing.T, srv *httptest.Server) NodeProfile {
	t.Helper()
	return NodeProfile{
		BaseURL:      srv.URL,
		Username:     "plugin",
		Password:     "test123",
		DeviceName:   DefaultDeviceName,
		ClientIDPath: filepath.Join(t.TempDir(), "client_id"),
	}
}

// fullHandshake is a minimal three-stage BitComet node. It records every request
// and validates the RNCryptor login ciphertext against the supplied client_id
// (exactly like the real node). Any path other than the two login stages is a
// protected call and must carry `Bearer DEV-1`; it then answers with tail.
func fullHandshake(rec *recorder, tail string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := rec.record(r)
		switch r.URL.Path {
		case "/api/webui/login":
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			cid, _ := m["client_id"].(string)
			auth, _ := m["authentication"].(string)
			pt, err := DecryptFromBase64(auth, cid)
			if err != nil {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error_code":"INVALID_TOKEN"}`))
				return
			}
			var creds map[string]string
			_ = json.Unmarshal(pt, &creds)
			if creds["username"] == "" || creds["password"] == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error_code":"INVALID_TOKEN"}`))
				return
			}
			_, _ = w.Write([]byte(`{"error_code":"OK","invite_token":"INV-1"}`))
		case "/api/device_token/get":
			if r.Header.Get("Authorization") != "Bearer INV-1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error_code":"INVALID_TOKEN"}`))
				return
			}
			_, _ = w.Write([]byte(`{"error_code":"OK","device_token":"DEV-1"}`))
		default:
			if r.Header.Get("Authorization") != "Bearer DEV-1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error_code":"INVALID_TOKEN"}`))
				return
			}
			_, _ = w.Write([]byte(tail))
		}
	})
}

func startNode(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// Ported from crates/bitcomet-api/src/client.rs `mod tests` (one Go test per
// Rust #[test]); each names the Rust case it mirrors.
// ---------------------------------------------------------------------------

func TestIsOKCodeCaseInsensitive(t *testing.T) {
	// Rust: ok_code_is_case_insensitive
	for _, ok := range []string{"OK", "ok", "", "  ok  "} {
		if !IsOKCode(ok) {
			t.Fatalf("IsOKCode(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"INVALID_TOKEN", "Error"} {
		if IsOKCode(bad) {
			t.Fatalf("IsOKCode(%q) = true, want false", bad)
		}
	}
}

func TestCheckErrorCodeAcceptsMissingField(t *testing.T) {
	// Rust: check_error_code_accepts_missing_field
	v := map[string]any{"about_info": map[string]any{}}
	if err := checkErrorCode(v); err != nil {
		t.Fatalf("无 error_code 字段应按成功处理, got %v", err)
	}
}

func TestCheckErrorCodeRejectsFailure(t *testing.T) {
	// Rust: check_error_code_rejects_failure
	v := map[string]any{"error_code": "INVALID_TOKEN", "error_message": "Invalid token"}
	err := checkErrorCode(v)
	var re *RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("want *RemoteError, got %v", err)
	}
	if re.Code != "INVALID_TOKEN" || re.Message != "Invalid token" {
		t.Fatalf("RemoteError = %+v", re)
	}
}

func TestClientIsConstructibleWithoutNetwork(t *testing.T) {
	// Rust: client_is_constructible_without_network
	p := NodeProfile{
		BaseURL:      "http://node.invalid:9085/",
		Username:     "u",
		Password:     "p",
		DeviceName:   DefaultDeviceName,
		ClientIDPath: filepath.Join(t.TempDir(), "client_id"),
	}
	c, err := NewClient(p, 5)
	if err != nil {
		t.Fatalf("构造不应触网: %v", err)
	}
	if c.ClientID() == "" {
		t.Fatal("client_id 必须非空")
	}
	if got := c.BaseURL(); got != "http://node.invalid:9085" {
		t.Fatalf("BaseURL() = %q, want normalized", got)
	}
	if c.HasToken() {
		t.Fatal("构造后尚无 token")
	}
}

func TestURLJoinHasNoDoubleSlash(t *testing.T) {
	// Rust: url_join_has_no_double_slash
	p := NodeProfile{
		BaseURL:      "http://node.invalid:9085///",
		Username:     "u",
		Password:     "p",
		DeviceName:   DefaultDeviceName,
		ClientIDPath: filepath.Join(t.TempDir(), "client_id"),
	}
	c, err := NewClient(p, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.url("/api/webui/login"); got != "http://node.invalid:9085/api/webui/login" {
		t.Fatalf("url() = %q", got)
	}
}

func TestCallWithoutLoginIsRejectedLocally(t *testing.T) {
	// Rust: call_without_login_is_rejected_locally
	var hit bool
	var mu sync.Mutex
	srv := startNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	c, err := NewClient(profileFor(t, srv), 5)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Call(context.Background(), "/api_v2/task_list/get", map[string]any{})
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hit {
		t.Fatal("未登录时不得发出任何 HTTP 请求")
	}
}

// TestLiveLoginAndFetchTaskList mirrors the Rust test of the same name, which is
// marked `#[ignore]` because it needs a real node and credentials. It is ported
// as a SKIP, not "fixed" — that is the 1:1 behaviour-preserving choice.
//
// Rust source (client.rs): `#[ignore = "需要真实 BitComet 节点与凭据（见测试文档注释）"]`
func TestLiveLoginAndFetchTaskList(t *testing.T) {
	t.Skip("对照 Rust #[ignore]：需要真实 BitComet 节点与凭据（见测试文档注释）")

	url := mustEnv(t, "DISPATCH_TEST_NODE_URL")
	user := mustEnv(t, "DISPATCH_TEST_NODE_USER")
	pass := mustEnv(t, "DISPATCH_TEST_NODE_PASS")

	p := NodeProfile{BaseURL: url, Username: user, Password: pass, DeviceName: DefaultDeviceName}
	c, err := NewClient(p, 20)
	if err != nil {
		t.Fatalf("构造客户端: %v", err)
	}
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("三段式登录必须成功: %v", err)
	}
	if !c.HasToken() {
		t.Fatal("登录后应持有 device_token")
	}
	list, err := c.FetchTaskList(context.Background())
	if err != nil {
		t.Fatalf("任务列表拉取必须成功: %v", err)
	}
	if list == nil {
		t.Fatal("任务列表响应应为 JSON 对象")
	}
}

func mustEnv(t *testing.T, key string) string {
	t.Helper()
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		t.Skipf("缺少环境变量 %s", key)
	}
	return v
}

// ---------------------------------------------------------------------------
// httptest end-to-end + every error branch (observable terminal state only).
// ---------------------------------------------------------------------------

func TestFullHandshakeThenProtectedCall(t *testing.T) {
	// Mirrors crates/dispatch-core/tests/handshake_probe.rs::full_handshake_then_protected_call
	// (client side): three-stage login then a protected call, asserting the exact
	// request text (method/path/headers/body) of each stage.
	rec2 := &recorder{}
	srv2 := startNode(t, fullHandshake(rec2, `{"error_code":"OK","tasks":[]}`))

	c, err := NewClient(profileFor(t, srv2), 5)
	if err != nil {
		t.Fatal(err)
	}
	if c.HasToken() {
		t.Fatal("构造后不应持有 token")
	}

	list, err := c.FetchTaskList(context.Background())
	if err != nil {
		t.Fatalf("FetchTaskList: %v", err)
	}
	if list == nil {
		t.Fatal("响应应为 JSON 对象")
	}
	if !c.HasToken() {
		t.Fatal("登录后应持有 token")
	}

	// -- stage 1 request text --
	login := rec2.byPath("/api/webui/login")
	if len(login) != 1 {
		t.Fatalf("login 次数 = %d, want 1", len(login))
	}
	if login[0].method != http.MethodPost {
		t.Fatalf("login method = %s", login[0].method)
	}
	if got := login[0].header.Get("Client-Type"); got != ClientType {
		t.Fatalf("Client-Type = %q, want %q", got, ClientType)
	}
	if got := login[0].header.Get("User-Agent"); got != UserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, UserAgent)
	}
	if got := login[0].header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var loginBody map[string]any
	if err := json.Unmarshal([]byte(login[0].body), &loginBody); err != nil {
		t.Fatalf("login body not JSON: %v", err)
	}
	if len(loginBody) != 2 {
		t.Fatalf("login body keys = %v, want exactly {authentication, client_id}", loginBody)
	}
	if loginBody["client_id"] != c.ClientID() {
		t.Fatalf("login client_id = %v, want %s", loginBody["client_id"], c.ClientID())
	}
	if _, ok := loginBody["authentication"].(string); !ok {
		t.Fatal("login authentication must be a base64 string")
	}

	// -- stage 2 request text (sorted keys, matching serde_json BTreeMap order) --
	dt := rec2.byPath("/api/device_token/get")
	if len(dt) != 1 {
		t.Fatalf("device_token 次数 = %d, want 1", len(dt))
	}
	if got := dt[0].header.Get("Authorization"); got != "Bearer INV-1" {
		t.Fatalf("stage2 Authorization = %q", got)
	}
	wantDT := fmt.Sprintf(`{"device_id":%q,"device_name":"dispatch-proxy","invite_token":"INV-1","platform":"webui"}`, c.ClientID())
	if dt[0].body != wantDT {
		t.Fatalf("stage2 body = %s\n want %s", dt[0].body, wantDT)
	}

	// -- protected request text --
	pr := rec2.byPath("/api_v2/task_list/get")
	if len(pr) != 1 {
		t.Fatalf("task_list 次数 = %d, want 1", len(pr))
	}
	if got := pr[0].header.Get("Authorization"); got != "Bearer DEV-1" {
		t.Fatalf("protected Authorization = %q", got)
	}
	if pr[0].body != "{}" {
		t.Fatalf("protected body = %q, want {}", pr[0].body)
	}
}

func TestCallReloginsOnceOn401ThenSucceeds(t *testing.T) {
	var mu sync.Mutex
	loginSeq := 0
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		switch r.URL.Path {
		case "/api/webui/login":
			mu.Lock()
			loginSeq++
			n := loginSeq
			mu.Unlock()
			fmt.Fprintf(w, `{"error_code":"OK","invite_token":"INV-%d"}`, n)
		case "/api/device_token/get":
			switch r.Header.Get("Authorization") {
			case "Bearer INV-1":
				_, _ = w.Write([]byte(`{"error_code":"OK","device_token":"DEV-1"}`))
			case "Bearer INV-2":
				_, _ = w.Write([]byte(`{"error_code":"OK","device_token":"DEV-2"}`))
			default:
				w.WriteHeader(http.StatusUnauthorized)
			}
		default:
			if r.Header.Get("Authorization") == "Bearer DEV-2" {
				_, _ = w.Write([]byte(`{"error_code":"OK","tasks":[]}`))
			} else {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error_code":"INVALID_TOKEN"}`))
			}
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(profileFor(t, srv), 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.FetchTaskList(context.Background()); err != nil {
		t.Fatalf("应在 401 后重登一次并成功: %v", err)
	}
	if n := len(rec.byPath("/api/webui/login")); n != 2 {
		t.Fatalf("login 次数 = %d, want 2 (初次 + 一次重登)", n)
	}
	if n := len(rec.byPath("/api_v2/task_list/get")); n != 2 {
		t.Fatalf("task_list 次数 = %d, want 2 (401 + 重试)", n)
	}
}

func TestCallTokenRejectedAfterSecond401(t *testing.T) {
	srv := startNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/webui/login":
			_, _ = w.Write([]byte(`{"error_code":"OK","invite_token":"INV-1"}`))
		case "/api/device_token/get":
			_, _ = w.Write([]byte(`{"error_code":"OK","device_token":"DEV-1"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error_code":"INVALID_TOKEN"}`))
		}
	}))
	c, _ := NewClient(profileFor(t, srv), 5)
	_, err := c.FetchTaskList(context.Background())
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("err = %v, want ErrTokenRejected", err)
	}
}

func TestLoginNon2xxIsLoginError(t *testing.T) {
	// Rust: ClientError::Login{status, detail} on non-2xx login stage.
	srv := startNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server exploded"))
	}))
	c, _ := NewClient(profileFor(t, srv), 5)
	err := c.Login(context.Background())
	var le *LoginError
	if !errors.As(err, &le) {
		t.Fatalf("want *LoginError, got %v", err)
	}
	if le.Status != http.StatusInternalServerError {
		t.Fatalf("Status = %d", le.Status)
	}
	if le.Detail != "server exploded" {
		t.Fatalf("Detail = %q", le.Detail)
	}
}

func TestLoginMissingInviteToken(t *testing.T) {
	srv := startNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error_code":"OK"}`)) // no invite_token
	}))
	c, _ := NewClient(profileFor(t, srv), 5)
	err := c.Login(context.Background())
	var mf *MissingFieldError
	if !errors.As(err, &mf) || mf.Field != "invite_token" {
		t.Fatalf("err = %v, want MissingField(invite_token)", err)
	}
}

func TestLoginMissingDeviceToken(t *testing.T) {
	srv := startNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/webui/login":
			_, _ = w.Write([]byte(`{"error_code":"OK","invite_token":"INV-1"}`))
		default:
			_, _ = w.Write([]byte(`{"error_code":"OK"}`)) // no device_token
		}
	}))
	c, _ := NewClient(profileFor(t, srv), 5)
	err := c.Login(context.Background())
	var mf *MissingFieldError
	if !errors.As(err, &mf) || mf.Field != "device_token" {
		t.Fatalf("err = %v, want MissingField(device_token)", err)
	}
}

func TestProtectedCallRemoteErrorCode(t *testing.T) {
	srv := startNode(t, fullHandshake(&recorder{}, `{"error_code":"INVALID_TOKEN","error_message":"Invalid token"}`))
	c, _ := NewClient(profileFor(t, srv), 5)
	_, err := c.FetchTaskList(context.Background())
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != "INVALID_TOKEN" || re.Message != "Invalid token" {
		t.Fatalf("err = %v, want Remote{INVALID_TOKEN, Invalid token}", err)
	}
}

func TestProtectedCallNon2xxTruncatesDetail(t *testing.T) {
	// Rust: `short()` truncates to 300 chars + '…'; non-2xx yields Remote{HTTP_<status>, short(body)}.
	long := strings.Repeat("x", 400)
	// A non-2xx protected response, to exercise Remote{HTTP_<status>, short(body)}.
	srv2 := startNode(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/webui/login":
			_, _ = w.Write([]byte(`{"error_code":"OK","invite_token":"INV-1"}`))
		case "/api/device_token/get":
			_, _ = w.Write([]byte(`{"error_code":"OK","device_token":"DEV-1"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(long))
		}
	}))
	c2, _ := NewClient(profileFor(t, srv2), 5)
	_, err := c2.FetchTaskList(context.Background())
	var re *RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("want *RemoteError, got %v", err)
	}
	if re.Code != "HTTP_500" {
		t.Fatalf("Code = %q, want HTTP_500", re.Code)
	}
	if got := []rune(re.Message); len(got) != 301 || got[len(got)-1] != '…' {
		t.Fatalf("Message not truncated to 300 runes + …: len=%d", len(got))
	}
}

func TestAddHTTPTaskBodyShape(t *testing.T) {
	rec2 := &recorder{}
	srv2 := startNode(t, fullHandshake(rec2, `{"error_code":"OK"}`))

	c, err := NewClient(profileFor(t, srv2), 5)
	if err != nil {
		t.Fatal(err)
	}

	// no save_folder: it must be omitted (keep the node's own folder).
	if _, err := c.AddHTTPTask(context.Background(), "http://e.com/a.bin", nil, false); err != nil {
		t.Fatal(err)
	}
	got := rec2.byPath("/api/task/http/add")
	if len(got) != 1 {
		t.Fatalf("add 次数 = %d", len(got))
	}
	want := `{"start_later":false,"url":"http://e.com/a.bin"}`
	if got[0].body != want {
		t.Fatalf("body = %s\n want %s", got[0].body, want)
	}

	// with save_folder: present, and keys still sorted.
	folder := "D:/dl"
	if _, err := c.AddHTTPTask(context.Background(), "http://e.com/b.bin", &folder, true); err != nil {
		t.Fatal(err)
	}
	got = rec2.byPath("/api/task/http/add")
	want2 := `{"save_folder":"D:/dl","start_later":true,"url":"http://e.com/b.bin"}`
	if got[1].body != want2 {
		t.Fatalf("body = %s\n want %s", got[1].body, want2)
	}
}

func TestAddTorrentLinksBodyShape(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(fullHandshake(rec, `{"error_code":"OK"}`))
	t.Cleanup(srv.Close)
	c, err := NewClient(profileFor(t, srv), 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddTorrentLinks(context.Background(), "magnet:?xt=1", nil, true); err != nil {
		t.Fatal(err)
	}
	got := rec.byPath("/api/task/torrent_links/add")
	if len(got) != 1 {
		t.Fatalf("add 次数 = %d", len(got))
	}
	want := `{"start_later":true,"torrent_links":"magnet:?xt=1"}`
	if got[0].body != want {
		t.Fatalf("body = %s\n want %s", got[0].body, want)
	}
}
