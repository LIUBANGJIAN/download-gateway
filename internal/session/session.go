// Package session 承载管理会话表、登录防爆破退避，以及与请求头相关的 Cookie / 同源判定。
//
// # 1:1 移植来源
//
// `crates/dispatch-core/src/admin/session.rs` 的 `SessionStore` / `LoginGuard` / `Guard`
// 枚举，及状态无关的辅助函数 `same_origin` / `cookie_secure_for` / `set_cookie_header` /
// `clear_cookie_header` / `cookie_value`；常量 `MAX_FAILURES` / `LOCK_SECS` / `COOKIE_NAME`。
//
// # 为什么单独成「叶子包」（结构性偏离，已获 team-lead 裁定）
//
// Rust `session.rs` 里 `guard_read`/`guard_write` 直接收 `&AdminState`。Go 中 `state` 被
// `admin` 依赖，若 `session` 反向依赖 `state` 即成环。故：**类型 + 状态无关行为**下沉本叶子包，
// 依赖 `AdminState` 的 `guard_read`/`guard_write` 留在 `internal/admin`（T04）。
//
// # 依赖分层（不得违反，由 `internal/layerguard` 静态守卫）
//
//	internal/config → internal/session → internal/state → internal/{ingress,admin}
//
// 本包**不得** import 任何比自身更高层的 `internal/**`。
//
// # 三条语义（搬自 Rust 模块文档）
//
//   - 会话：内存 `map<sid, Session>`，空闲 30min **滑动续期**，重启全失效。
//   - 防爆破：按来源 IP 计连续失败，5 次 → 锁定 5 分钟；成功清零。
//   - CSRF：写操作要求同源（`Sec-Fetch-Site` 或 `Origin`），读操作不查。
package session

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"

	"github.com/LIUBANGJIAN/download-gateway/internal/config"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
)

// 常量（逐字对照 Rust）。
const (
	// MaxFailures 是连续失败多少次触发锁定。
	MaxFailures uint32 = 5
	// LockSecs 是锁定时长（秒）。
	LockSecs int64 = 300
	// CookieName 是会话 Cookie 名。
	CookieName = "sid"
)

// session 是一条会话记录（对应 Rust `struct Session`）。
type session struct {
	lastSeenAt int64
}

// SessionStore 是内存会话表（对应 Rust `struct SessionStore`）。
type SessionStore struct {
	idleTTLSecs int64
	mu          sync.Mutex
	inner       map[string]session
}

// NewSessionStore 新建，`idleTTLSecs` 为空闲过期时间（对应 Rust `SessionStore::new`）。
func NewSessionStore(idleTTLSecs int64) *SessionStore {
	return &SessionStore{idleTTLSecs: idleTTLSecs, inner: make(map[string]session)}
}

// Create 创建会话，返回 `sid`。
func (s *SessionStore) Create(now int64) string {
	sid := ids.NewHex128()
	s.mu.Lock()
	s.inner[sid] = session{lastSeenAt: now}
	s.mu.Unlock()
	return sid
}

// Touch 校验并**滑动续期**；返回 `(新的过期时刻, true)`；过期/不存在返回 `(0, false)`。
func (s *SessionStore) Touch(sid string, now int64) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.inner[sid]
	if !ok {
		return 0, false
	}
	if now-st.lastSeenAt > s.idleTTLSecs {
		delete(s.inner, sid)
		return 0, false
	}
	st.lastSeenAt = now
	s.inner[sid] = st
	return now + s.idleTTLSecs, true
}

// Revoke 主动失效。
func (s *SessionStore) Revoke(sid string) {
	s.mu.Lock()
	delete(s.inner, sid)
	s.mu.Unlock()
}

// fail 是某 IP 的连续失败记录（对应 Rust `struct Fail`）。
type fail struct {
	count       uint32
	lockedUntil int64
}

// LoginGuard 是登录防爆破退避（按来源 IP；对应 Rust `struct LoginGuard`）。
type LoginGuard struct {
	mu    sync.Mutex
	inner map[netip.Addr]fail
}

// NewGuard 构造空表（对应 Rust `LoginGuard::new`）。
func NewGuard() *LoginGuard {
	return &LoginGuard{inner: make(map[netip.Addr]fail)}
}

// LockedUntil 若被锁定则返回 `(解锁时刻, true)`；否则 `(0, false)`。
func (g *LoginGuard) LockedUntil(ip netip.Addr, now int64) (int64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.inner[ip]
	if !ok {
		return 0, false
	}
	if f.lockedUntil > now {
		return f.lockedUntil, true
	}
	if f.lockedUntil != 0 {
		delete(g.inner, ip)
	}
	return 0, false
}

// RecordFailure 记录一次失败，返回连续失败数。
func (g *LoginGuard) RecordFailure(ip netip.Addr, now int64) uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.inner[ip]
	f.count++
	if f.count >= MaxFailures {
		f.lockedUntil = now + LockSecs
	}
	g.inner[ip] = f
	return f.count
}

// RecordSuccess 记录成功（清零）。
func (g *LoginGuard) RecordSuccess(ip netip.Addr) {
	g.mu.Lock()
	delete(g.inner, ip)
	g.mu.Unlock()
}

// Guard 是守卫结论（对应 Rust `enum Guard`）。
type Guard int

const (
	// GuardOK 通过。
	GuardOK Guard = iota
	// GuardUnauthenticated 未认证。
	GuardUnauthenticated
	// GuardCSRFInvalid CSRF 校验失败（跨源写）。
	GuardCSRFInvalid
)

// String 返回稳定名（供日志）。
func (g Guard) String() string {
	switch g {
	case GuardOK:
		return "ok"
	case GuardUnauthenticated:
		return "unauthenticated"
	case GuardCSRFInvalid:
		return "csrf_invalid"
	default:
		return "unknown"
	}
}

// CookieValue 从 `Cookie` 头取指定键（对应 Rust `cookie_value`）。
//
// 返回 `(值, true)`；未命中返回 `("", false)`。
func CookieValue(h http.Header, key string) (string, bool) {
	raw := h.Get("Cookie")
	if raw == "" {
		return "", false
	}
	for _, part := range strings.Split(raw, ";") {
		k, v, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		if strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// SameOrigin 判定是否同源（对应 Rust `same_origin`）。
//
// 判定顺序：① `Sec-Fetch-Site`（浏览器自动带）；② `Origin` 与 `Host` 比对；
// ③ 两者都无（多为 curl 等非浏览器客户端）⇒ 视为同源（浏览器必会带上前两者之一）。
//
// ⚠️ 这里有一处 **Go 与 Rust 的语义鸿沟**，照搬 Rust 写法会造成全员写操作失败：
// Rust 侧（actix）用 `headers.get(HOST)` 取 Host，Host 确实在 HeaderMap 里；
// 而 Go 的 `net/http` 会把 `Host` **从 Header map 里摘出来**放进 `Request.Host`，
// 于是 `r.Header.Get("Host")` **恒为空字符串**。Rust 版本原样翻译过来，
// 第二条分支就退化成「带了 Origin 且不带 Sec-Fetch-Site ⇒ 必定 false」，
// 症状是节点增删改 / 策略保存 / 任务干预 / 改口令 **全部 403 "CSRF 校验失败"**，
// 而登录与列表（只读）都正常 —— 极易被误判成"前端没带 token"。
// 因此本函数签名收 `*http.Request`，Host 优先取 `r.Host`，Header 仅作兜底。
//
// `Sec-Fetch-Site: same-site` 不直接拒绝：它表示"同站不同源"，但浏览器对
// 同 IP 不同端口等场景的归类并不总与 origin 完全一致。这里继续走 Origin/Host
// **精确比对**，比一刀切更准确且不降低安全性（真正跨源的 Origin 仍然对不上）。
func SameOrigin(r *http.Request) bool {
	if v := r.Header.Get("Sec-Fetch-Site"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "same-origin", "none":
			return true
		case "cross-site":
			// 真跨站，无需再做比对。
			return false
		}
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		ohost := originHost(origin)
		host := requestHost(r)
		if ohost == "" || host == "" {
			return false
		}
		return strings.EqualFold(ohost, host)
	}
	return true
}

// originHost 从 `Origin` 头里取出 host[:port]。
//
// Origin 的规范形态是 `scheme://host[:port]`，但部分客户端/代理会带尾部 `/`，
// 这里一并归一化，避免"就差一个斜杠"被判成跨源。
func originHost(origin string) string {
	origin = strings.TrimSpace(origin)
	if _, after, ok := strings.Cut(origin, "://"); ok {
		origin = after
	}
	// 去掉可能存在的路径/尾部斜杠（`Origin` 本不该有，但脏数据不该让它变成 403）。
	if i := strings.IndexByte(origin, '/'); i >= 0 {
		origin = origin[:i]
	}
	return strings.TrimSpace(origin)
}

// requestHost 取请求的 Host。
//
// `r.Host` 是 Go 的权威来源；`Header["Host"]` 只作兜底（某些中间件 /
// HTTP/2 场景会把它留在 Header 里）。两者都空则视为无法判定。
func requestHost(r *http.Request) string {
	if h := strings.TrimSpace(r.Host); h != "" {
		return h
	}
	if h := strings.TrimSpace(r.Header.Get("Host")); h != "" {
		// 多值（极少见）取第一个，避免 `a, b` 与 Origin 比对永远失败。
		if i := strings.IndexByte(h, ','); i >= 0 {
			h = strings.TrimSpace(h[:i])
		}
		return h
	}
	return ""
}

// CookieSecureFor 依据策略与请求头，决定是否给 Cookie 加 `Secure`（对应 Rust `cookie_secure_for`）。
func CookieSecureFor(mode config.CookieSecureMode, h http.Header) bool {
	switch mode {
	case config.CookieSecureAlways:
		return true
	case config.CookieSecureNever:
		return false
	default: // Auto：仅当 X-Forwarded-Proto: https
		return strings.EqualFold(strings.TrimSpace(h.Get("X-Forwarded-Proto")), "https")
	}
}

// SetCookieHeader 生成 `Set-Cookie` 头值（会话建立；对应 Rust `set_cookie_header`）。
func SetCookieHeader(sid string, maxAge int64, secure bool) string {
	s := fmt.Sprintf("%s=%s; HttpOnly; SameSite=Strict; Path=/; Max-Age=%d", CookieName, sid, maxAge)
	if secure {
		s += "; Secure"
	}
	return s
}

// ClearCookieHeader 生成清除会话的 `Set-Cookie` 头值（对应 Rust `clear_cookie_header`）。
func ClearCookieHeader(secure bool) string {
	s := fmt.Sprintf("%s=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0", CookieName)
	if secure {
		s += "; Secure"
	}
	return s
}
