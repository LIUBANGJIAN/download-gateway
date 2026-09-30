//! 两端口各自的共享状态。
//!
//! 为什么拆成两套 state：对外口（6800）与管理口（8080）的中间件需求完全不同
//! （对外是可选 token + CORS；管理是强制会话 + CSRF + 防爆破退避）。
//! `/healthz` 仍由 `main.rs` 拥有，两个 state 各提供 `store/started_at`，契约逐字节不变。

use std::path::PathBuf;
use std::sync::Arc;

use crate::ids;
use crate::ingress::handshake::HandshakeStore;
use crate::store::Store;

/// 对外口（6800）共享状态。
pub struct PublicState {
    /// 数据库句柄（克隆廉价）。
    pub store: Store,
    /// 进程启动时刻（Unix 秒），供 `/healthz` 计算 uptime。
    pub started_at: i64,
    /// `DISPATCH_PUBLIC_TOKEN`；`None` = 不校验。
    pub public_token: Option<String>,
    /// `aria2.getSessionInfo` 用的进程级 session id。
    pub session_id: String,
    /// `DISPATCH_PUBLIC_CORS`；`true` 才回 `Access-Control-Allow-Origin`。
    pub cors_enabled: bool,
    /// 自签三段式握手所需的独立状态（代理 client_id + 令牌表）。
    pub handshake: HandshakeState,
}

impl PublicState {
    /// 构造对外口状态。`session_id` 在进程内唯一（重启即变）。
    pub fn new(
        store: Store,
        started_at: i64,
        public_token: Option<String>,
        cors_enabled: bool,
        handshake: HandshakeState,
    ) -> Arc<Self> {
        Arc::new(Self {
            store,
            started_at,
            public_token,
            session_id: ids::new_hex_128(),
            cors_enabled,
            handshake,
        })
    }
}

/// 握手子状态。
pub struct HandshakeState {
    /// 代理**自身**的稳定标识（UUID v4）。
    ///
    /// 设计结论：它**不落 SQLite `app_config`**，而是复用 `bitcomet_api::clientid`
    /// 的**文件机制**（`DISPATCH_STATE_DIR`/`XDG_CONFIG_HOME`/`APPDATA`…），与代理
    /// 作为客户端去连节点时用的是同一套路径解析，避免两处定义。
    pub proxy_client_id: String,
    /// 一次性 invite / 无 TTL device 令牌表（内存，重启失效）。
    pub tokens: HandshakeStore,
}

/// 管理口（8080）共享状态。
pub struct AdminState {
    /// 数据库句柄。
    pub store: Store,
    /// 进程启动时刻（Unix 秒）。
    pub started_at: i64,
    /// 管理面策略（口令、Cookie、删文件开关）。
    pub policy: Arc<AdminPolicy>,
    /// 内存会话表（空闲 30min 滑动续期）。
    pub sessions: crate::admin::session::SessionStore,
    /// 登录防爆破退避。
    pub guard: crate::admin::session::LoginGuard,
    /// 配置快照（`GET /api/admin/config` 的数据源）。
    pub env: Arc<EnvSnapshot>,
    /// 节点密码的加解密器（供管理台「眼睛」回看原文）。
    pub secrets: Arc<crate::secret::SecretBox>,
    /// `DISPATCH_WEB_DIR`；`Some` 则从磁盘读页面（热改）。
    pub web_dir: Option<PathBuf>,
}

impl AdminState {
    /// 构造管理口状态。
    pub fn new(
        store: Store,
        started_at: i64,
        policy: Arc<AdminPolicy>,
        env: EnvSnapshot,
        secrets: Arc<crate::secret::SecretBox>,
        web_dir: Option<PathBuf>,
    ) -> Arc<Self> {
        Arc::new(Self {
            store,
            started_at,
            policy,
            sessions: crate::admin::session::SessionStore::new(1800),
            guard: crate::admin::session::LoginGuard::new(),
            env: Arc::new(env),
            secrets,
            web_dir,
        })
    }
}

/// 管理面策略。
pub struct AdminPolicy {
    /// 管理口令（明文，仅存内存）。
    pub password: String,
    /// `true` = 口令由启动时生成（非环境注入）⇒ `GET /api/admin/config` 要标「已生成」。
    pub generated: bool,
    /// Cookie `Secure` 属性策略。
    pub cookie_secure: CookieSecureMode,
    /// 是否允许删节点文件（`remove` 的 `delete_files=true`）。
    pub allow_file_delete: bool,
}

/// Cookie `Secure` 属性策略（见 design-1 §3.9）。
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum CookieSecureMode {
    /// 仅当 `X-Forwarded-Proto: https`（反代终止 TLS）时加 `Secure`；直连明文 HTTP 不加。
    Auto,
    /// 恒加 `Secure`（已上 TLS 的用户）。
    Always,
    /// 恒不加（排障）。
    Never,
}

impl CookieSecureMode {
    /// 解析环境变量取值；未知/空白回落 `Auto`。
    pub fn parse(s: &str) -> Self {
        match s.trim().to_ascii_lowercase().as_str() {
            "always" => Self::Always,
            "never" => Self::Never,
            _ => Self::Auto,
        }
    }

    /// 供 `EnvSnapshot` 上报的稳定字符串。
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Auto => "auto",
            Self::Always => "always",
            Self::Never => "never",
        }
    }
}

/// `GET /api/admin/config` 的数据源；由 `main.rs` 从 `Config` 填入。
///
/// 只读快照：进程启动时的有效环境，不含密钥明文（密钥类字段只上报「是否已配置」）。
pub struct EnvSnapshot {
    /// 对外监听地址。
    pub public_addr: String,
    /// 管理监听地址。
    pub admin_addr: String,
    /// SQLite 路径。
    pub db_path: String,
    /// 迁移目录。
    pub migrations_dir: String,
    /// 日志级别。
    pub log_level: String,
    /// `DISPATCH_STATE_DIR`（client_id 文件目录来源）。
    pub state_dir: Option<String>,
    /// `DISPATCH_WEB_DIR`。
    pub web_dir: Option<String>,
    /// 管理口令是否由环境注入（`false` 表示是启动生成的）。
    pub admin_password_configured: bool,
    /// `DISPATCH_PUBLIC_TOKEN` 是否已配置。
    pub public_token_configured: bool,
    /// 是否允许删节点文件。
    pub allow_file_delete: bool,
    /// 是否开启对外口 CORS。
    pub public_cors: bool,
    /// `DISPATCH_ADMIN_COOKIE_SECURE` 的有效取值（`auto`/`always`/`never`）。
    pub admin_cookie_secure: &'static str,
}
