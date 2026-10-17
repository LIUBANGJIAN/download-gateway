// 「忘记口令 → 点按钮 → 从日志取临时口令 → 登录改口令」这条自助链路的**路由级**用例。
//
// 为什么光有 `internal/adminpass/temp_test.go` 不够：那组用例只证明「临时口令的数据结构对」，
// 证明不了三件真正会出事的事：
//
//  1. 接口真的**匿名可用**（不需要 Cookie/CSRF）—— 否则用户在最需要它的时候正好用不了；
//  2. 口令真的**只从日志出去**、响应里拿不到 —— 一旦漏到响应体，这个匿名接口就等于没有认证；
//  3. 用临时口令改完口令后它**立刻失效** —— 否则日志里那串在 TTL 内仍是有效后门。
//
// 第 3 条尤其容易假通过：只看「新口令能登」是不够的，必须再拿临时口令去登一次并断言 401，
// 否则 `SetHash` 里那句 `temp.clearLocked()` 被删掉也不会有人发现。
package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/adminpass"
)

// syncBuffer 是并发安全的 bytes.Buffer（handler 可能在别的 goroutine 里写日志）。
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs 把默认 slog 输出接到内存缓冲，返回「取当前日志文本」的函数。
//
// 刻意用真的 TextHandler 而不是自己造一个假 handler：用户的实际动作是
// `docker logs ... | grep temp_password`，只有走真格式才能验证那个 grep 真的抓得到。
func captureLogs(t *testing.T) func() string {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// tempCodeRe 与 `scripts/selfreset-e2e.sh` 里的 grep 模式一致（去掉易混字符的 12 位）。
var tempCodeRe = regexp.MustCompile(`temp_password=([A-Z2-9]{12})`)

// extractTempCode 从日志文本里取最新一串临时口令；取不到就 Fatal。
func extractTempCode(t *testing.T, logs string) string {
	t.Helper()
	m := tempCodeRe.FindAllStringSubmatch(logs, -1)
	if len(m) == 0 {
		t.Fatalf("日志里没有 temp_password=<12 位>，用户就无从取码。日志尾部：\n%s", tail(logs, 1200))
	}
	return m[len(m)-1][1]
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// requestReset 发一次匿名找回请求，返回状态码与原始 body。
func requestReset(t *testing.T, c *http.Client, base string) (int, string) {
	t.Helper()
	status, body := doReq(t, c, http.MethodPost, base+"/api/admin/password/reset-request", "")
	return status, string(body)
}

// TestSelfResetOnlyDeliversCodeViaLog 是这条链路的主用例，一次跑完四步：
// 匿名可取码 → 码不在响应里 → 码能登录 → 原口令仍然有效。
func TestSelfResetOnlyDeliversCodeViaLog(t *testing.T) {
	srv, _, _ := newAdminEnv(t)
	logs := captureLogs(t)

	status, body := requestReset(t, &http.Client{}, srv.URL)
	if status != http.StatusOK {
		t.Fatalf("匿名请求应 200，实际 %d %s", status, body)
	}

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			ExpiresIn int64  `json:"expires_in"`
			LogHint   string `json:"log_hint"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("响应不是预期信封：%v / %s", err, body)
	}
	if envelope.Code != 0 {
		t.Fatalf("应成功信封 code=0，实际 %d", envelope.Code)
	}
	if envelope.Data.ExpiresIn != adminpass.DefaultTempTTL {
		t.Fatalf("默认有效期应 %d，实际 %d", adminpass.DefaultTempTTL, envelope.Data.ExpiresIn)
	}
	if !strings.Contains(envelope.Data.LogHint, "temp_password") {
		t.Fatalf("log_hint 应告诉用户 grep 哪个键，实际 %q", envelope.Data.LogHint)
	}

	code := extractTempCode(t, logs())

	// ① 口令绝不能出现在响应里 —— 这是这个匿名接口的**唯一安全边界**。
	if strings.Contains(body, code) {
		t.Fatalf("临时口令泄漏到了 HTTP 响应里：%s", body)
	}

	// ② 用临时口令能登录（匿名接口 + 新客户端，不带任何 Cookie）。
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("建 jar 失败：%v", err)
	}
	tempClient := &http.Client{Jar: jar}
	loginStatus, loginBody := doReq(t, tempClient, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+code+`"}`)
	if loginStatus != http.StatusOK {
		t.Fatalf("临时口令应能登录，实际 %d %s", loginStatus, loginBody)
	}

	// ③ 原口令**仍然有效**（并存而非覆盖）：别人乱点按钮锁不了管理员。
	oldStatus, oldBody := doReq(t, &http.Client{}, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+testAdminPassword+`"}`)
	if oldStatus != http.StatusOK {
		t.Fatalf("原口令应保持有效，实际 %d %s", oldStatus, oldBody)
	}
}

// TestSelfResetRateLimited 验证频率限制与 `Retry-After`。
func TestSelfResetRateLimited(t *testing.T) {
	srv, _, _ := newAdminEnv(t)
	_ = captureLogs(t)

	if status, body := requestReset(t, &http.Client{}, srv.URL); status != http.StatusOK {
		t.Fatalf("第一次应 200，实际 %d %s", status, body)
	}

	resp, err := (&http.Client{}).Post(srv.URL+"/api/admin/password/reset-request", "", nil)
	if err != nil {
		t.Fatalf("第二次请求失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("同 IP 立即再生成应 429，实际 %d %s", resp.StatusCode, raw)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatalf("429 必须带 Retry-After，否则前端只能盲目重试")
	}
	if !strings.Contains(string(raw), "42900") {
		t.Fatalf("应回 42900 信封，实际 %s", raw)
	}
}

// TestSelfResetDisabledReturns404 验证开关关闭后这条路径**不存在**（不是 403）。
func TestSelfResetDisabledReturns404(t *testing.T) {
	srv, _, _ := newAdminEnv(t)
	_ = captureLogs(t)
	t.Setenv(EnvAllowSelfReset, "0")

	status, body := requestReset(t, &http.Client{}, srv.URL)
	if status != http.StatusNotFound {
		t.Fatalf("关闭后应 404，实际 %d %s", status, body)
	}
}

// TestSelfResetTTLFromEnv 验证 TTL 环境变量生效，以及越界值**静默回落默认值**。
func TestSelfResetTTLFromEnv(t *testing.T) {
	srv, _, _ := newAdminEnv(t)
	_ = captureLogs(t)
	t.Setenv(EnvTempPasswordTTL, "45")

	status, body := requestReset(t, &http.Client{}, srv.URL)
	if status != http.StatusOK {
		t.Fatalf("应 200，实际 %d %s", status, body)
	}
	if !strings.Contains(body, `"expires_in":45`) {
		t.Fatalf("应回 expires_in=45，实际 %s", body)
	}

	// 越界（低于 30）回落默认值：部署参数写错不该把唯一的自救路径关掉。
	srv2, _, _ := newAdminEnv(t)
	t.Setenv(EnvTempPasswordTTL, "5")
	status, body = requestReset(t, &http.Client{}, srv2.URL)
	if status != http.StatusOK {
		t.Fatalf("越界 TTL 应仍 200，实际 %d %s", status, body)
	}
	if !strings.Contains(body, `"expires_in":600`) {
		t.Fatalf("越界 TTL 应回落 600，实际 %s", body)
	}
}

// TestTempCodeDiesAfterPasswordChange 是这条链路最容易被改坏的一步。
//
// 断言两件事：改密成功、**且**旧临时口令立刻登不进来。
// 只断言前者的话，`SetHash` 里清临时口令那一行删掉也照样绿。
func TestTempCodeDiesAfterPasswordChange(t *testing.T) {
	srv, _, st := newAdminEnv(t)
	logs := captureLogs(t)

	if status, body := requestReset(t, &http.Client{}, srv.URL); status != http.StatusOK {
		t.Fatalf("生成临时口令应 200，实际 %d %s", status, body)
	}
	code := extractTempCode(t, logs())

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("建 jar 失败：%v", err)
	}
	tempClient := &http.Client{Jar: jar}
	if status, body := doReq(t, tempClient, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+code+`"}`); status != http.StatusOK {
		t.Fatalf("临时口令应能登录，实际 %d %s", status, body)
	}

	// 用临时口令当「当前口令」把自己的口令改掉 —— 这正是用户拿到码之后的动作。
	const newPass = "brand-new-pass-77"
	status, body := doReq(t, tempClient, http.MethodPost, srv.URL+"/api/admin/password",
		`{"current_password":"`+code+`","new_password":"`+newPass+`"}`)
	if status != http.StatusOK {
		t.Fatalf("用临时口令改密应成功，实际 %d %s", status, body)
	}
	if !strings.Contains(string(body), `"source":"database"`) {
		t.Fatalf("改密后来源应变成 database，实际 %s", body)
	}

	// ① 临时口令必须立刻作废。
	status, body = doReq(t, &http.Client{}, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+code+`"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("改密后临时口令必须失效，实际 %d %s", status, body)
	}

	// ② 新口令可用（证明上面那条 401 不是因为别的坏了）。
	if status, body := doReq(t, &http.Client{}, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+newPass+`"}`); status != http.StatusOK {
		t.Fatalf("新口令应能登录，实际 %d %s", status, body)
	}

	// ③ 审计留痕且**不含口令**（event_log 在管理台界面上是可见的）。
	rows, err := st.Query(context.Background(),
		"SELECT message FROM event_log WHERE category = 'security' ORDER BY id", nil)
	if err != nil {
		t.Fatalf("查审计失败：%v", err)
	}
	var joined strings.Builder
	for _, r := range rows {
		for _, c := range r {
			if s, ok := c.(string); ok {
				joined.WriteString(s)
				joined.WriteString("\n")
			}
		}
	}
	audit := joined.String()
	if !strings.Contains(audit, "已生成一次性临时管理口令") {
		t.Fatalf("生成临时口令应留审计，实际：%s", audit)
	}
	if strings.Contains(audit, code) {
		t.Fatalf("审计里不能出现临时口令本身（界面上可见）：%s", audit)
	}
}
