// 三段式握手的端到端用例：真起 handler、真写 SQLite、真做 RNCryptor 加解密。
//
// 与 Rust 侧 `ingress/handshake.rs` 的用例一一对应，两套实现打的是同一套契约。
package ingress

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/handshake"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
	"github.com/LIUBANGJIAN/download-gateway/internal/state"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

const testClientID = "ci-smoke-client-id-8f2c"

// loginBody 造 `/api/webui/login` 的请求体：用 client_id 作口令加密凭据。
func loginBody(t *testing.T, clientID, username, password string) []byte {
	t.Helper()
	plain, err := json.Marshal(map[string]any{"username": username, "password": password})
	if err != nil {
		t.Fatalf("序列化凭据失败: %v", err)
	}
	cipher, err := protocol.EncryptToBase64(plain, clientID)
	if err != nil {
		t.Fatalf("RNCryptor 加密失败: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"client_id":      clientID,
		"authentication": cipher,
	})
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	return body
}

// envField 取出 BitComet 信封里的业务字段。
//
// ⚠️ 信封是**扁平**的：业务字段直接铺在顶层（`BitcometOK` 把 data 的键并进信封），
// 不是 `{"data": {...}}` —— 这条契约与 Rust 的 `bitcomet_ok_resp` 一致。
func envField(t *testing.T, payload []byte, key string) any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %s", payload)
	}
	return env[key]
}

// 三段式全链路：ip_verify → login → device_token/get → 用 device_token 过鉴权。
func TestHandshakeFullFlow(t *testing.T) {
	token := testToken
	srv := newPublicServer(t, &token)

	// 第 1 段：免凭据，恒回 bypass_eligible=false。
	code, body := post(t, srv.URL+"/api/webui/ip_verify", "application/json", []byte(`{}`), nil)
	if code != http.StatusOK {
		t.Fatalf("ip_verify 应 200，实际 %d %s", code, body)
	}
	if envField(t, body, "bypass_eligible") != false {
		t.Fatalf("必须如实说 bypass 不可用，实际 %v", envField(t, body, "bypass_eligible"))
	}

	// 第 2 段：口令登录换 invite_token。
	code, body = post(t, srv.URL+"/api/webui/login", "application/json",
		loginBody(t, testClientID, "admin", testToken), nil)
	if code != http.StatusOK {
		t.Fatalf("login 应 200，实际 %d %s", code, body)
	}
	invite, _ := envField(t, body, "invite_token").(string)
	if len(invite) != 32 {
		t.Fatalf("invite_token 应为 32 位十六进制，实际 %q", invite)
	}

	// 第 3 段：invite 换 device_token。
	payload, _ := json.Marshal(map[string]any{"invite_token": invite, "device_id": testClientID})
	code, body = post(t, srv.URL+"/api/device_token/get", "application/json", payload, nil)
	if code != http.StatusOK {
		t.Fatalf("device_token/get 应 200，实际 %d %s", code, body)
	}
	device, _ := envField(t, body, "device_token").(string)
	if len(device) != 32 {
		t.Fatalf("device_token 应为 32 位十六进制，实际 %q", device)
	}

	// 拿到的 device_token 必须真能过鉴权（否则整条链路只是「有个响应」）。
	code, body = post(t, srv.URL+"/api_v2/task_list/get", "application/json", []byte(`{}`),
		map[string]string{"Authorization": "Bearer " + device})
	if code != http.StatusOK {
		t.Fatalf("device_token 应能过鉴权，实际 %d %s", code, body)
	}
}

// 凭据不符（密文里 password != DISPATCH_PUBLIC_TOKEN）必须 401，且不得回 invite。
func TestHandshakeWrongPassword(t *testing.T) {
	token := testToken
	srv := newPublicServer(t, &token)

	code, body := post(t, srv.URL+"/api/webui/login", "application/json",
		loginBody(t, testClientID, "admin", "not-the-token"), nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("错误口令应 401，实际 %d %s", code, body)
	}
	if strings.Contains(string(body), "invite_token") {
		t.Fatalf("失败响应不得带 invite_token：%s", body)
	}
}

// `bypass:true` 必须被拒：我们从不实现免密码登录。
func TestHandshakeRejectsBypass(t *testing.T) {
	token := testToken
	srv := newPublicServer(t, &token)

	body := []byte(`{"client_id":"` + testClientID + `","bypass":true}`)
	code, payload := post(t, srv.URL+"/api/webui/login", "application/json", body, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("bypass 应 400，实际 %d %s", code, payload)
	}
}

// 缺 authentication 必须 401（不能因为「没密文」就放行）。
func TestHandshakeMissingAuthentication(t *testing.T) {
	token := testToken
	srv := newPublicServer(t, &token)

	body := []byte(`{"client_id":"` + testClientID + `"}`)
	code, payload := post(t, srv.URL+"/api/webui/login", "application/json", body, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("缺 authentication 应 401，实际 %d %s", code, payload)
	}
}

// invite_token 是**一次性**的：第二次兑换必须 401。
func TestHandshakeInviteIsOneShot(t *testing.T) {
	token := testToken
	srv := newPublicServer(t, &token)

	code, body := post(t, srv.URL+"/api/webui/login", "application/json",
		loginBody(t, testClientID, "admin", testToken), nil)
	if code != http.StatusOK {
		t.Fatalf("login 应 200，实际 %d %s", code, body)
	}
	invite, _ := envField(t, body, "invite_token").(string)

	payload, _ := json.Marshal(map[string]any{"invite_token": invite})
	if code, body = post(t, srv.URL+"/api/device_token/get", "application/json", payload, nil); code != http.StatusOK {
		t.Fatalf("首次兑换应 200，实际 %d %s", code, body)
	}
	code, body = post(t, srv.URL+"/api/device_token/get", "application/json", payload, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("重复兑换应 401（一次性），实际 %d %s", code, body)
	}
}

// Bearer 与 body.invite_token 同时给且不一致 → 401（不给「以哪个为准」的模糊地带）。
func TestHandshakeInviteMismatch(t *testing.T) {
	token := testToken
	srv := newPublicServer(t, &token)

	payload, _ := json.Marshal(map[string]any{"invite_token": "aaaa"})
	code, body := post(t, srv.URL+"/api/device_token/get", "application/json", payload,
		map[string]string{"Authorization": "Bearer bbbb"})
	if code != http.StatusUnauthorized {
		t.Fatalf("两个 invite 不一致应 401，实际 %d %s", code, body)
	}
}

// 同一 IP 一分钟内超过 30 次登录必须 429（6800 是公开口，PBKDF2 是 CPU 放大面）。
func TestHandshakeLoginRateLimit(t *testing.T) {
	token := testToken
	srv := newPublicServer(t, &token)

	body := loginBody(t, testClientID, "admin", testToken)
	sawRateLimit := false
	for i := 0; i < loginRatePerMin+2; i++ {
		code, payload := post(t, srv.URL+"/api/webui/login", "application/json", body, nil)
		if code == http.StatusTooManyRequests {
			sawRateLimit = true
			if i < loginRatePerMin {
				t.Fatalf("第 %d 次不该被限流（阈值 %d）", i+1, loginRatePerMin)
			}
			break
		}
		if code != http.StatusOK {
			t.Fatalf("第 %d 次应 200 或 429，实际 %d %s", i+1, code, payload)
		}
	}
	if !sawRateLimit {
		t.Fatalf("连打 %d 次仍未触发限流", loginRatePerMin+2)
	}
}

// 未配置 public token 时：login 放行（内网可用性取舍，与 Rust 一致），
// 但 device_token 仍能过鉴权 —— 证明「不配置 token」不等于「握手坏了」。
func TestHandshakeWithoutPublicToken(t *testing.T) {
	srv := newPublicServer(t, nil)

	code, body := post(t, srv.URL+"/api/webui/login", "application/json",
		loginBody(t, testClientID, "admin", "anything"), nil)
	if code != http.StatusOK {
		t.Fatalf("未配置 token 时应放行，实际 %d %s", code, body)
	}
	invite, _ := envField(t, body, "invite_token").(string)
	payload, _ := json.Marshal(map[string]any{"invite_token": invite})
	code, body = post(t, srv.URL+"/api/device_token/get", "application/json", payload, nil)
	if code != http.StatusOK {
		t.Fatalf("device_token/get 应 200，实际 %d %s", code, body)
	}
	if _, ok := envField(t, body, "device_token").(string); !ok {
		t.Fatalf("响应缺 device_token：%s", body)
	}
}

// 审计里绝不能出现口令 / 密文 / 令牌全串（只留 client_id 前 8 字符）。
func TestHandshakeAuditHasNoSecrets(t *testing.T) {
	token := testToken
	// 这个用例要直接读库，所以自己建一套（`newPublicServer` 不回传 store）。
	dbPath := filepath.Join(t.TempDir(), "dispatch.db")
	st, err := store.Open(dbPath, filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("打开真实库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ps := state.NewPublicState(st, ids.NowSecs(), &token, false, state.HandshakeState{
		Tokens: handshake.NewStore(),
	})
	srv := httptest.NewServer(NewRouter(ps, "0.1.3"))
	t.Cleanup(srv.Close)

	body := loginBody(t, testClientID, "admin", testToken)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	cipherText, _ := parsed["authentication"].(string)
	code, payload := post(t, srv.URL+"/api/webui/login", "application/json", body, nil)
	if code != http.StatusOK {
		t.Fatalf("login 应 200，实际 %d %s", code, payload)
	}

	rows, err := st.Query(t.Context(),
		"SELECT message, detail FROM event_log WHERE category='security'")
	if err != nil {
		t.Fatalf("查审计失败: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("握手成功必须留审计")
	}
	for _, row := range rows {
		line := fmt.Sprint(row...)
		if strings.Contains(line, cipherText) {
			t.Fatalf("审计泄露了 authentication 密文：%s", line)
		}
		if strings.Contains(line, testToken) {
			t.Fatalf("审计泄露了口令：%s", line)
		}
	}
}
