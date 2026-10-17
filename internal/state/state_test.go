package state

import (
	"strings"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/config"
	"github.com/LIUBANGJIAN/download-gateway/internal/handshake"
)

// 对照 Rust `PublicState::new`：session_id 由内部生成（128bit 十六进制），其余字段原样透传。
func TestNewPublicState(t *testing.T) {
	tok := "secret-token"
	tokens := handshake.NewStore()
	hs := HandshakeState{ProxyClientID: "cid-1", Tokens: tokens}
	ps := NewPublicState(nil, 1_700_000_000, &tok, true, hs)

	if ps.Store != nil {
		t.Fatalf("Store 应原样透传（此处为 nil）")
	}
	if ps.StartedAt != 1_700_000_000 {
		t.Fatalf("StartedAt 应为 1700000000，实际 %d", ps.StartedAt)
	}
	if ps.PublicToken == nil || *ps.PublicToken != "secret-token" {
		t.Fatalf("PublicToken 应透传，实际 %v", ps.PublicToken)
	}
	if !ps.CORSEnabled {
		t.Fatalf("CORSEnabled 应为 true")
	}
	if len(ps.SessionID) != 32 {
		t.Fatalf("SessionID 应为 32 字符十六进制，实际 %q（len=%d）", ps.SessionID, len(ps.SessionID))
	}
	for _, c := range ps.SessionID {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("SessionID 必须是小写十六进制，实际 %q", ps.SessionID)
		}
	}
	if ps.Handshake.ProxyClientID != "cid-1" {
		t.Fatalf("Handshake 应透传，实际 %+v", ps.Handshake)
	}
	if ps.Handshake.Tokens != tokens {
		t.Fatalf("Handshake.Tokens 应透传同一指针，实际 %v", ps.Handshake.Tokens)
	}
}

// 「session_id 进程内唯一（重启即变）」的可观测判据：两次构造必须不同。
func TestNewPublicStateSessionIDIsUnique(t *testing.T) {
	a := NewPublicState(nil, 0, nil, false, HandshakeState{})
	b := NewPublicState(nil, 0, nil, false, HandshakeState{})
	if a.SessionID == b.SessionID {
		t.Fatalf("两次构造的 SessionID 不应相同：%q", a.SessionID)
	}
}

// 未配置 public token（nil）时字段应保持 nil（而非空串）。
func TestNewPublicStateNilTokenStaysNil(t *testing.T) {
	ps := NewPublicState(nil, 0, nil, false, HandshakeState{})
	if ps.PublicToken != nil {
		t.Fatalf("未配置 token 时应为 nil，实际 %v", ps.PublicToken)
	}
}

// 对照 Rust `AdminState::new`：策略/环境/密钥/页面目录原样透传；
// 会话表与守卫（T04 落地）在 T01 占位为 nil。
func TestNewAdminState(t *testing.T) {
	policy := config.AdminPolicy{Password: "pw", Generated: false,
		CookieSecure: config.CookieSecureAlways, AllowFileDelete: true}
	env := config.EnvSnapshot{PublicAddr: "0.0.0.0:6800", AdminAddr: "127.0.0.1:8080", LogLevel: "info"}
	wd := "/web"
	as := NewAdminState(nil, 42, &policy, env, nil, &wd)

	if as.StartedAt != 42 {
		t.Fatalf("StartedAt 应为 42，实际 %d", as.StartedAt)
	}
	if as.Policy == nil || as.Policy.Password != "pw" || !as.Policy.AllowFileDelete {
		t.Fatalf("Policy 应透传，实际 %+v", as.Policy)
	}
	if as.Env == nil || as.Env.PublicAddr != "0.0.0.0:6800" {
		t.Fatalf("Env 应透传，实际 %+v", as.Env)
	}
	if as.WebDir == nil || *as.WebDir != "/web" {
		t.Fatalf("WebDir 应透传，实际 %v", as.WebDir)
	}
	if as.Sessions == nil {
		t.Fatalf("Sessions 应由构造器初始化（对应 Rust `SessionStore::new(1800)`）")
	}
	if as.Guard == nil {
		t.Fatalf("Guard 应由构造器初始化（对应 Rust `LoginGuard::new()`）")
	}
	// 行为判据：新建会话表的空闲 TTL 必须是 1800s —— 1801 秒后 touch 必失效。
	// （能杀掉把 `sessionIdleTTLSecs` 改错、或漏传该常量的变异。）
	sid := as.Sessions.Create(0)
	if _, ok := as.Sessions.Touch(sid, 1800); !ok {
		t.Fatalf("空闲 TTL=1800：1800 秒时仍应有效")
	}
	sid2 := as.Sessions.Create(0)
	if _, ok := as.Sessions.Touch(sid2, 1801); ok {
		t.Fatalf("空闲 TTL=1800：1801 秒后应失效")
	}
}

// Env 应存**快照副本**（对应 Rust「传值 → 包进 Arc」）：调用方之后再改自己的 env 变量，
// 不得改变已存入的 Env。
func TestNewAdminStateEnvIsSnapshotCopy(t *testing.T) {
	env := config.EnvSnapshot{LogLevel: "info"}
	as := NewAdminState(nil, 0, &config.AdminPolicy{}, env, nil, nil)
	env.LogLevel = "debug"
	if as.Env.LogLevel != "info" {
		t.Fatalf("Env 应为快照副本，不应随调用方变量变化，实际 %q", as.Env.LogLevel)
	}
}

// 兜底：本包的包名是 state，与 internal/domain/state.go（package domain）不是一回事。
// 这条锁住「包名没有被人改动」这一事实，避免有人把两处合并。
func TestPackageIdentityIsState(t *testing.T) {
	// 纯编译期可见性断言：NewPublicState 返回 *PublicState（而非 domain 类型）。
	var ps *PublicState = NewPublicState(nil, 0, nil, false, HandshakeState{})
	if ps == nil {
		t.Fatalf("NewPublicState 不应返回 nil")
	}
	if strings.TrimSpace(ps.SessionID) == "" {
		t.Fatalf("SessionID 不应为空白")
	}
}
