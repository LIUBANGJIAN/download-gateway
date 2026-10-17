// Package handshake 承载代理**自签三段式握手**的内存令牌表（`HandshakeStore`）及其行为。
//
// # 1:1 移植来源
//
// `crates/dispatch-core/src/ingress/handshake.rs` 的 `HandshakeStore`（及 `LoginFail`）、
// 其全部方法（`issue_invite`/`redeem_invite`/`issue_device`/`is_device`/
// `check_login_rate`/`device_count`）与常量（`HANDSHAKE_BODY_LIMIT`/`INVITE_TTL_SECS`/
// `DEVICE_CAP`/`LOGIN_RATE_PER_MIN`）。
//
// # 为什么单独成「叶子包」（Rust→Go 的结构性偏离，已获 team-lead 裁定）
//
// Rust 单 crate 内，`state.rs` 可 `use crate::ingress::handshake::HandshakeStore` 而
// `ingress` 又用 `crate::state::PublicState` —— 单 crate 允许环。**Go 编译期禁止包循环**。
// 故把「类型 + 行为」下沉到本叶子包，`internal/state` 只**持有**类型，HTTP 处理函数
// （`ip_verify`/`login`/`device_token_get`，它们要用 `PublicState`）留在 `internal/ingress`。
//
// # 依赖分层（不得违反，由 `internal/layerguard` 静态守卫）
//
//	internal/ids → internal/handshake → internal/state → internal/{ingress,admin}
//
// 本包**不得** import 任何比自身更高层的 `internal/**`。
//
// # 攻击面（如实写明，搬自 Rust 模块文档）
//
// `client_id` 由客户端自报、**且同时是解密口令** ⇒ 任何人用自造的 `client_id` 加密任意明文
// 都能通过 RNCryptor 解密。**「解密成功」零身份证明**，该握手在语义上等价于「公开可握手」。
// 真正承载凭据的是**密文里的 `password` 字段**；因此「`DISPATCH_PUBLIC_TOKEN` 已配置时必须
// `password == token`」是**承重墙**（该校验在 `internal/ingress` 的 handler 层实现）。
package handshake

import (
	"net/netip"
	"sync"

	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
)

// 常量（逐字对照 Rust）。
const (
	// BodyLimit 是握手请求体上限（**64 KiB 显式闸门**，独立于 Router 的 8 MiB 体积上限）。
	BodyLimit = 64 * 1024
	// InviteTTLSecs 是 invite_token 有效期（秒）。
	InviteTTLSecs int64 = 120
	// DeviceCap 是 device_token 表软上限（满了淘汰最旧，防内存被刷）。
	DeviceCap = 128
	// LoginRatePerMin 是 `/api/webui/login` 按来源 IP 的限速阈值（次/分钟，超限 429）。
	LoginRatePerMin = 30
)

// LoginFail 是握手失败原因（纯函数输出，便于单测；对应 Rust `enum LoginFail`）。
//
// 实际校验函数 `verify_login_credentials` 需要 `PublicState`，故留在 `internal/ingress`；
// 但其**输出类型**定义于此，保持 Rust「类型与行为同处」的内聚。
type LoginFail int

const (
	// LoginFailDecrypt 密文解密失败（口令错 / 密文损坏）。
	LoginFailDecrypt LoginFail = iota
	// LoginFailShape 明文不是合法 JSON 或缺 `password` 字段。
	LoginFailShape
	// LoginFailCredentialMismatch `password` 与已配置的 `DISPATCH_PUBLIC_TOKEN` 不符。
	LoginFailCredentialMismatch
)

// String 返回稳定的原因串（供日志/审计）。
func (f LoginFail) String() string {
	switch f {
	case LoginFailDecrypt:
		return "decrypt_failed"
	case LoginFailShape:
		return "shape_invalid"
	case LoginFailCredentialMismatch:
		return "credential_mismatch"
	default:
		return "unknown"
	}
}

type invite struct {
	deviceID  string
	expiresAt int64
}

// inner 是受互斥锁保护的可变状态（对应 Rust `struct Inner`）。
type inner struct {
	// invites invite_token → 绑定信息（一次性）。
	invites map[string]invite
	// devices device_token → device_id。
	devices map[string]string
	// deviceOrder device_token 的插入顺序（淘汰最旧用）。
	deviceOrder []string
	// loginHits 每个来源 IP 在最近一分钟内的登录时刻。
	loginHits map[netip.Addr][]int64
}

// HandshakeStore 是握手令牌表（内存，重启失效；对应 Rust `struct HandshakeStore`）。
//
// 不引 master key：令牌是 128 位随机不透明串、内存查表校验，不需要 HMAC 签发。
// Rust 用 `Mutex<Inner>`；Go 用 `sync.Mutex`（字段内嵌）。
type HandshakeStore struct {
	mu    sync.Mutex
	inner inner
}

// NewStore 构造一张空表（对应 Rust `HandshakeStore::new`）。
func NewStore() *HandshakeStore {
	return &HandshakeStore{
		inner: inner{
			invites:   make(map[string]invite),
			devices:   make(map[string]string),
			loginHits: make(map[netip.Addr][]int64),
		},
	}
}

// IssueInvite 签发一次性 invite_token（TTL 120s）。顺带清理过期 invite。
func (s *HandshakeStore) IssueInvite(deviceID string, now int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.inner.invites {
		if v.expiresAt <= now {
			delete(s.inner.invites, k)
		}
	}
	token := ids.NewHex128()
	s.inner.invites[token] = invite{deviceID: deviceID, expiresAt: now + InviteTTLSecs}
	return token
}

// RedeemInvite 兑换 invite_token（**一次性**：命中即删）；过期或不存在返回 `("", false)`。
func (s *HandshakeStore) RedeemInvite(inviteToken string, now int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.inner.invites[inviteToken]
	// Rust `inner.invites.remove(invite)?` —— **先移除**，再判过期。
	if !ok {
		return "", false
	}
	delete(s.inner.invites, inviteToken)
	if inv.expiresAt > now {
		return inv.deviceID, true
	}
	return "", false
}

// IssueDevice 签发 device_token（无 TTL）。超过软上限时淘汰最旧，保证表大小 ≤ [DeviceCap]。
func (s *HandshakeStore) IssueDevice(deviceID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.inner.devices) >= DeviceCap {
		if len(s.inner.deviceOrder) == 0 {
			break
		}
		old := s.inner.deviceOrder[0]
		s.inner.deviceOrder = s.inner.deviceOrder[1:]
		delete(s.inner.devices, old)
	}
	token := ids.NewHex128()
	s.inner.devices[token] = deviceID
	s.inner.deviceOrder = append(s.inner.deviceOrder, token)
	return token
}

// IsDevice 报告 token 是否为本进程签发的 device_token。
func (s *HandshakeStore) IsDevice(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.inner.devices[token]
	return ok
}

// CheckLoginRate 登录限速：记录本次并返回是否**允许**（最近一分钟内 > [LoginRatePerMin] 次则拒绝）。
func (s *HandshakeStore) CheckLoginRate(ip netip.Addr, now int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	windowStart := now - 60
	hits := s.inner.loginHits[ip]
	// 丢弃窗口外的（对应 Rust `while front < window_start { pop_front }`）。
	cut := 0
	for cut < len(hits) && hits[cut] < windowStart {
		cut++
	}
	hits = hits[cut:]
	hits = append(hits, now)
	s.inner.loginHits[ip] = hits
	return len(hits) <= LoginRatePerMin
}

// DeviceCount 返回当前 device 表大小（供测试与自检）。
func (s *HandshakeStore) DeviceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inner.devices)
}
