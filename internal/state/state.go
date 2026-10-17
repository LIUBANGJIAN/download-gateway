// Package state 提供两端口各自的共享状态。
//
// # 1:1 移植来源
//
// `crates/dispatch-core/src/state.rs` 的 `PublicState` / `HandshakeState` / `AdminState`
// 三个类型及其构造器（`new`）。
//
// # ⚠️ 与 Rust 的三处显式差异（均已在回执记明，非疏漏）
//
//  1. 本包**不含** `AdminPolicy` / `CookieSecureMode` / `EnvSnapshot`：Go 的方法接收者
//     必须与类型同包，`EnvSnapshot.Items()` 归 `internal/config`；为避免
//     `config`（需 `AdminPolicy`）与 `state`（需 `EnvSnapshot`）相互引用形成包循环，
//     这三个类型统一由 `internal/config` 承载，本包**单向**依赖它。
//  2. `HandshakeState.Tokens`、`AdminState.Sessions`、`AdminState.Guard` 三个字段，在 Rust 里
//     分别是 `ingress::handshake::HandshakeStore` 与 `admin::session::{SessionStore,LoginGuard}`。
//     Rust 单 crate 内允许环（`state`↔`ingress`/`admin`）；**Go 编译期禁止包循环**。故按 team-lead
//     裁定把「类型 + 行为」下沉到**叶子包** `internal/handshake` 与 `internal/session`，
//     本包只**持有**其类型（不再用 `any` 占位）。
//     依赖分层：`internal/{ids,config} → internal/{handshake,session} → internal/state →
//     internal/{ingress,admin}`，由 `internal/layerguard` 静态守卫。
//  3. Rust 的 `Arc<...>` 共享字段在 Go 里映射为指针（`*config.AdminPolicy`、
//     `*handshake.HandshakeStore` 等）；`EnvSnapshot` 传值后取址存入（对应 Rust「传入值 → 包进 Arc」）。
//
// # 与 `internal/domain` 的分工（防止后来人读串）
//
//   - **本包**（`internal/state`）：两端口共享**状态容器** —— `PublicState`/`HandshakeState`/
//     `AdminState`，持有 DB 句柄、启动时刻、握手令牌表、会话表、策略、密钥等**运行时对象**。
//   - `internal/domain`：**纯领域模型与纯函数** —— `internal_state ↔ aria_status` 映射
//     （`DeriveAriaStatus`/`AriaViewFrom`/`ClassifyNodeStatus`/`MapErrorClass`）与任务枚举/行投影，
//     不持有任何运行时对象。两者的 `state.go` **同名不同包**（`package state` vs `package domain`）。
package state

import (
	"github.com/LIUBANGJIAN/download-gateway/internal/adminpass"
	"github.com/LIUBANGJIAN/download-gateway/internal/config"
	"github.com/LIUBANGJIAN/download-gateway/internal/handshake"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/secret"
	"github.com/LIUBANGJIAN/download-gateway/internal/session"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// PublicState 是对外口（6800）共享状态（对照 Rust `struct PublicState`）。
type PublicState struct {
	// Store 数据库句柄（`*store.Store` 即并发安全，等价 Rust 里「克隆廉价」的 Store）。
	Store *store.Store
	// StartedAt 进程启动时刻（Unix 秒），供 `/healthz` 计算 uptime。
	StartedAt int64
	// PublicToken `DISPATCH_PUBLIC_TOKEN`；nil = 不校验。
	PublicToken *string
	// SessionID `aria2.getSessionInfo` 用的进程级 session id（128bit 十六进制，重启即变）。
	SessionID string
	// CORSEnabled `DISPATCH_PUBLIC_CORS`；true 才回 `Access-Control-Allow-Origin`。
	CORSEnabled bool
	// Handshake 自签三段式握手所需的独立状态（代理 client_id + 令牌表）。
	Handshake HandshakeState
}

// NewPublicState 构造对外口状态。`session_id` 在进程内唯一（重启即变）。
//
// 对照 Rust `PublicState::new`：内部生成 `ids::new_hex_128()` 作为 session id。
func NewPublicState(
	store *store.Store,
	startedAt int64,
	publicToken *string,
	corsEnabled bool,
	handshake HandshakeState,
) *PublicState {
	return &PublicState{
		Store:       store,
		StartedAt:   startedAt,
		PublicToken: publicToken,
		SessionID:   ids.NewHex128(),
		CORSEnabled: corsEnabled,
		Handshake:   handshake,
	}
}

// HandshakeState 是握手子状态（对照 Rust `struct HandshakeState`）。
type HandshakeState struct {
	// ProxyClientID 代理**自身**的稳定标识（UUID v4）。
	//
	// 设计结论：它**不落 SQLite `app_config`**，而是复用 `bitcomet-api::clientid`
	// 的**文件机制**（`DISPATCH_STATE_DIR`/`XDG_CONFIG_HOME`/`APPDATA`…），与代理
	// 作为客户端去连节点时用的是同一套路径解析，避免两处定义。
	ProxyClientID string
	// Tokens 一次性 invite / 无 TTL device 令牌表（内存，重启失效）。
	//
	// 类型来自**叶子包** `internal/handshake`（Rust 侧原为
	// `ingress::handshake::HandshakeStore`；为破 Go 包循环而下沉，见包文档差异 2）。
	Tokens *handshake.HandshakeStore
}

// AdminState 是管理口（8080）共享状态（对照 Rust `struct AdminState`）。
type AdminState struct {
	// Store 数据库句柄。
	Store *store.Store
	// StartedAt 进程启动时刻（Unix 秒）。
	StartedAt int64
	// Policy 管理面策略（口令、Cookie、删文件开关）。
	Policy *config.AdminPolicy
	// Sessions 内存会话表（空闲 30min 滑动续期）。
	//
	// 类型来自**叶子包** `internal/session`（Rust 侧原为 `admin::session::SessionStore`）。
	Sessions *session.SessionStore
	// Guard 登录防爆破退避。
	//
	// 类型来自**叶子包** `internal/session`（Rust 侧原为 `admin::session::LoginGuard`）。
	Guard *session.LoginGuard
	// Env 配置快照（`GET /api/admin/config` 的数据源）。
	Env *config.EnvSnapshot
	// Secrets 节点密码的加解密器（供管理台「眼睛」回看原文）。
	Secrets *secret.SecretBox
	// WebDir `DISPATCH_WEB_DIR`；非 nil 则从磁盘读页面（热改）。
	WebDir *string
	// Credential 管理口令的**当前凭据**（可热替换）。
	//
	// 为什么与 `Policy.Password` 并存：`Policy` 是启动时的策略快照（口令来源、
	// Cookie 策略、删文件开关），而口令本身会**在运行时被用户改掉**。
	// 把会变的东西塞进只读快照，结果就是「改了没生效」或「重建整个 Policy」。
	// 登录校验一律走这里，`Policy.Password` 只在装配时提供初始明文。
	Credential *adminpass.Credential
}

// sessionIdleTTLSecs 是管理会话空闲过期秒数（Rust `AdminState::new` 里写死的 `1800` = 30 分钟）。
const sessionIdleTTLSecs int64 = 1800

// NewAdminState 构造管理口状态。
//
// 对照 Rust `AdminState::new`：内部初始化 `SessionStore::new(1800)` 与 `LoginGuard::new()`。
// `env` 传值后取址存入（对应 Rust「传入 `EnvSnapshot` 值 → 包进 `Arc`」）。
func NewAdminState(
	store *store.Store,
	startedAt int64,
	policy *config.AdminPolicy,
	env config.EnvSnapshot,
	secrets *secret.SecretBox,
	webDir *string,
) *AdminState {
	var initialPlain string
	if policy != nil {
		initialPlain = policy.Password
	}
	return &AdminState{
		Store:     store,
		StartedAt: startedAt,
		Policy:    policy,
		Sessions:  session.NewSessionStore(sessionIdleTTLSecs),
		Guard:     session.NewGuard(),
		Env:       &env,
		Secrets:   secrets,
		WebDir:    webDir,
		// 初始凭据 = 环境变量注入或启动时随机生成的口令（明文，只在内存里）。
		// 启动时若库里已有用户自设的哈希，由调用方随后 `Credential.SetHash()` 覆盖。
		Credential: adminpass.NewCredential(initialPlain),
	}
}
