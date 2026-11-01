package session

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/config"
)

// ---------------------------------------------------------------------------
// 与 Rust `crates/dispatch-core/src/admin/session.rs::tests` 逐条对照
// （`guard_read_*`/`guard_write_*` 需 `AdminState`，属 handler 层，留 T04）
// ---------------------------------------------------------------------------

// 对照 `session_ttl_and_sliding_renewal`。
func TestSessionTTLAndSlidingRenewal(t *testing.T) {
	s := NewSessionStore(1800)
	sid := s.Create(1000)
	if _, ok := s.Touch(sid, 1500); !ok {
		t.Fatalf("未过期应有效")
	}
	// 滑动续期：从 1500 起再等 1700 秒仍有效
	if _, ok := s.Touch(sid, 3200); !ok {
		t.Fatalf("滑动续期后应有效")
	}
	// 超过空闲 TTL 失效
	if _, ok := s.Touch(sid, 3200+1801); ok {
		t.Fatalf("空闲超时应失效")
	}
}

// 对照 `revoke_invalidates`。
func TestRevokeInvalidates(t *testing.T) {
	s := NewSessionStore(1800)
	sid := s.Create(0)
	s.Revoke(sid)
	if _, ok := s.Touch(sid, 10); ok {
		t.Fatalf("已吊销会话应失效")
	}
}

// 对照 `five_failures_lock_then_success_clears`。
func TestFiveFailuresLockThenSuccessClears(t *testing.T) {
	g := NewGuard()
	ip := netip.MustParseAddr("203.0.113.7")
	for i := 0; i < 4; i++ {
		g.RecordFailure(ip, 1000)
		if _, locked := g.LockedUntil(ip, 1000); locked {
			t.Fatalf("第 %d 次失败（不足 %d）不应锁定", i+1, MaxFailures)
		}
	}
	if got := g.RecordFailure(ip, 1000); got != 5 {
		t.Fatalf("第 5 次失败计数应为 5，实际 %d", got)
	}
	if until, locked := g.LockedUntil(ip, 1000); !locked || until != 1000+LockSecs {
		t.Fatalf("应锁定至 %d，实际 (%d,%v)", 1000+LockSecs, until, locked)
	}
	// 锁定窗口后自动解锁
	if _, locked := g.LockedUntil(ip, 1000+LockSecs+1); locked {
		t.Fatalf("锁定窗口过后应自动解锁")
	}

	g.RecordFailure(ip, 2000)
	g.RecordSuccess(ip)
	if _, locked := g.LockedUntil(ip, 2000); locked {
		t.Fatalf("成功后应清零")
	}
}

// 对照 `cookie_secure_follows_mode_and_xfp`。
func TestCookieSecureFollowsModeAndXFP(t *testing.T) {
	h := http.Header{}
	if CookieSecureFor(config.CookieSecureAuto, h) {
		t.Fatalf("Auto + 无 XFP 应为 false")
	}
	h.Set("X-Forwarded-Proto", "https")
	if !CookieSecureFor(config.CookieSecureAuto, h) {
		t.Fatalf("Auto + XFP=https 应为 true")
	}
	if !CookieSecureFor(config.CookieSecureAlways, http.Header{}) {
		t.Fatalf("Always 恒为 true")
	}
	if CookieSecureFor(config.CookieSecureNever, h) {
		t.Fatalf("Never 恒为 false")
	}
}

// 对照 `set_cookie_has_expected_attributes`。
func TestSetCookieHasExpectedAttributes(t *testing.T) {
	c := SetCookieHeader("abc", 1800, false)
	for _, want := range []string{"sid=abc", "HttpOnly", "SameSite=Strict", "Path=/", "Max-Age=1800"} {
		if !strings.Contains(c, want) {
			t.Fatalf("Set-Cookie 应含 %q，实际 %q", want, c)
		}
	}
	if strings.Contains(c, "Secure") {
		t.Fatalf("明文 HTTP 下不得带 Secure，实际 %q", c)
	}
	if !strings.Contains(SetCookieHeader("x", 1, true), "; Secure") {
		t.Fatalf("secure=true 时应带 Secure")
	}
}

// ---------------------------------------------------------------------------
// 本包新增：ClearCookieHeader / SameOrigin / CookieValue 的分支
// ---------------------------------------------------------------------------

func TestClearCookieHeader(t *testing.T) {
	c := ClearCookieHeader(false)
	for _, want := range []string{"sid=", "HttpOnly", "SameSite=Strict", "Path=/", "Max-Age=0"} {
		if !strings.Contains(c, want) {
			t.Fatalf("清 Cookie 值应含 %q，实际 %q", want, c)
		}
	}
	if strings.Contains(c, "Secure") {
		t.Fatalf("secure=false 时不应带 Secure")
	}
	if !strings.Contains(ClearCookieHeader(true), "; Secure") {
		t.Fatalf("secure=true 时应带 Secure")
	}
}

// 对照 Rust `same_origin` 的三条分支。
//
// ⚠️ 这里**必须**用真 `*http.Request`，不能再用 `http.Header` 手塞一个 "Host"：
// Go 的 net/http 从不让 Host 出现在 Header map 里，手塞出来的"Header 有 Host"
// 是真实请求中不会出现的状态。第一版测试就是这么写的，于是
// `h.Get("Host")` 恒为空这个 bug 被完美掩盖 —— 单测全绿、线上全员 403。
func TestSameOriginBranches(t *testing.T) {
	cases := []struct {
		name    string
		host    string            // 写进 req.Host（Go 的权威来源）
		headers map[string]string // 其余头部
		want    bool
	}{
		{"无任何头（非浏览器）→ 同源", "127.0.0.1:8080", nil, true},
		{"Sec-Fetch-Site=same-origin", "127.0.0.1:8080", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"Sec-Fetch-Site=none", "127.0.0.1:8080", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"Sec-Fetch-Site=cross-site", "127.0.0.1:8080", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		// 回归：线上事故正是这一条。只带 Origin、不带 Sec-Fetch-Site 的浏览器
		// （老版本 Safari / 部分企业浏览器 / 被中间层剥头的场景）必须放行。
		{"只有 Origin，Host 匹配 → 同源", "127.0.0.1:8080",
			map[string]string{"Origin": "http://127.0.0.1:8080"}, true},
		{"Origin 与 Host 不匹配 → 跨源", "127.0.0.1:8080",
			map[string]string{"Origin": "http://evil.example"}, false},
		{"有 Origin 无 Host → 无法判定即拒绝", "", map[string]string{"Origin": "http://x"}, false},
		// same-site 不再一刀切拒绝，改为 Origin/Host 精确比对。
		{"same-site 且 Origin 与 Host 一致 → 同源", "192.168.10.250:8988",
			map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://192.168.10.250:8988"}, true},
		{"same-site 但 Origin 与 Host 不同 → 拒绝", "192.168.10.250:8988",
			map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://192.168.10.250:8080"}, false},
		{"Origin 带尾斜杠仍视为同源", "127.0.0.1:8080",
			map[string]string{"Origin": "http://127.0.0.1:8080/"}, true},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes", nil)
		req.Host = c.host
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		if got := SameOrigin(req); got != c.want {
			t.Fatalf("%s：SameOrigin=%v，应为 %v", c.name, got, c.want)
		}
	}
}

// TestSameOriginIgnoresHostInHeader 锁住"Host 不能只从 Header 取"这条。
//
// 这是 Rust→Go 移植最容易踩的坑：Rust（actix）的 Host 在 HeaderMap 里，
// Go 的 Host 在 Request.Host 里。若有人把实现改回 `r.Header.Get("Host")`，
// 本用例会红 —— 它构造的正是"Header 里有 Host、Request.Host 是另一个值"的场景，
// 此时必须按 Request.Host 判定。
func TestSameOriginIgnoresHostInHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/nodes", nil)
	req.Host = "real.example:8080"
	req.Header.Set("Host", "real.example:8080") // Rust 式写法：塞进 Header
	req.Header.Set("Origin", "http://evil.example")

	if SameOrigin(req) {
		t.Fatalf("Header 里的 Host 不得覆盖 Request.Host，否则 CSRF 可被绕过")
	}

	// 反过来：Request.Host 与 Origin 一致时，Header 里的脏值也不该干扰。
	req2 := httptest.NewRequest(http.MethodPost, "/api/admin/nodes", nil)
	req2.Host = "192.168.10.250:8988"
	req2.Header.Set("Host", "stale.example")
	req2.Header.Set("Origin", "http://192.168.10.250:8988")
	if !SameOrigin(req2) {
		t.Fatalf("Request.Host 与 Origin 一致时应放行，Header 的陈旧值不应参与判定")
	}
}

// 对照 Rust `cookie_value`：取值、去空白、未命中。
func TestCookieValue(t *testing.T) {
	h := http.Header{}
	if _, ok := CookieValue(h, CookieName); ok {
		t.Fatalf("无 Cookie 头应未命中")
	}
	h.Set("Cookie", "other=1; sid=abc ; more=2")
	if got, ok := CookieValue(h, CookieName); !ok || got != "abc" {
		t.Fatalf("应取到 sid=abc（去空白），实际 (%q,%v)", got, ok)
	}
	if _, ok := CookieValue(h, "missing"); ok {
		t.Fatalf("缺失键应未命中")
	}
	// 无 '=' 的片段应被跳过而不 panic
	h.Set("Cookie", "garbage; sid=xyz")
	if got, ok := CookieValue(h, CookieName); !ok || got != "xyz" {
		t.Fatalf("应跳过无 '=' 片段并取到 sid=xyz，实际 (%q,%v)", got, ok)
	}
}
