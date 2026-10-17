package session

import (
	"net/http"
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
func TestSameOriginBranches(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"无任何头（非浏览器）→ 同源", map[string]string{}, true},
		{"Sec-Fetch-Site=same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"Sec-Fetch-Site=none", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"Sec-Fetch-Site=cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"Sec-Fetch-Site=same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		{"Origin 与 Host 匹配", map[string]string{"Origin": "http://127.0.0.1:8080", "Host": "127.0.0.1:8080"}, true},
		{"Origin 与 Host 不匹配", map[string]string{"Origin": "http://evil.example", "Host": "127.0.0.1:8080"}, false},
		{"有 Origin 无 Host", map[string]string{"Origin": "http://x"}, false},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.headers {
			h.Set(k, v)
		}
		if got := SameOrigin(h); got != c.want {
			t.Fatalf("%s：SameOrigin=%v，应为 %v", c.name, got, c.want)
		}
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
