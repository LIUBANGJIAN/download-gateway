//! `download-gateway` 入口：**双端口 + SQLite 单写者**。
//!
//! 启动顺序（顺序不可调换）：
//!
//! ```text
//! 读配置 → open_connection() → migrate::apply() → Store::from_connection()
//!        → 起两个 axum listener（6800 对外 / 8080 管理）→ 等信号 → 关停 actor
//! ```
//!
//! 之所以**先迁移再起 actor**：迁移要用 `&mut Connection` 开事务，
//! 而连接一旦交给 actor 线程就不再可变。
//!
//! 端口隔离见 `02 §1.3` / `02 §4.4`：对外口可能公网可达，管理口**默认仅内网**。
//!
//! # 本轮增量（T08α/T09α/T10α）
//!
//! 本文件退化为**纯装配**：业务路由由 `dispatch-core` 的 `ingress`/`admin` 提供，
//! `/healthz` 仍由本文件拥有（契约逐字节不变）。两个 listener 都需 `ConnectInfo`
//! （管理口按来源 IP 防爆破登录；对外口 `/api/webui/login` 按来源 IP 限速）。

mod config;

use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{Context, Result};
use axum::extract::State;
use axum::response::Response;
use axum::routing::get;
use dispatch_core::state::{AdminState, EnvSnapshot, HandshakeState, PublicState};
use dispatch_core::store::{self, Store, migrate};
use tokio::net::TcpListener;
use tokio::sync::mpsc;
use tracing_subscriber::EnvFilter;

use crate::config::Config;

/// 对外口 `/healthz` 薄包装（契约与重构前逐字节一致）。
async fn healthz_public(State(s): State<Arc<PublicState>>) -> Response {
    dispatch_core::health::health_body(&s.store, s.started_at, "public").await
}

/// 管理口 `/healthz` 薄包装。
async fn healthz_admin(State(s): State<Arc<AdminState>>) -> Response {
    dispatch_core::health::health_body(&s.store, s.started_at, "admin").await
}

fn init_tracing(level: &str) {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new(level));
    tracing_subscriber::fmt().with_env_filter(filter).init();
}

/// 解析代理自身的稳定 client_id（走 `bitcomet_api::clientid` 文件机制，不落库）。
fn resolve_proxy_client_id() -> String {
    let path = bitcomet_api::clientid::default_client_id_path();
    match bitcomet_api::clientid::load_or_create_client_id(&path) {
        Ok(id) => id,
        Err(e) => {
            let id = bitcomet_api::clientid::new_uuid();
            tracing::warn!(error = %e, path = %path.display(),
                "client_id 文件读写失败，本次使用临时 client_id（重启会变）");
            id
        }
    }
}

/// 监听地址是否指向回环。
fn is_loopback(addr: &str) -> bool {
    let host = addr.rsplit_once(':').map(|(h, _)| h).unwrap_or(addr);
    let host = host.trim_matches(['[', ']']);
    matches!(host, "127.0.0.1" | "localhost" | "::1")
}

#[tokio::main]
async fn main() -> Result<()> {
    let cfg = Config::from_env()?;
    init_tracing(&cfg.log_level);

    tracing::info!(
        version = dispatch_core::VERSION,
        public = %cfg.public_addr,
        admin = %cfg.admin_addr,
        db = %cfg.db_path.display(),
        migrations = %cfg.migrations_dir.display(),
        "download-gateway 启动中"
    );

    // ── 安全告警（启动即显式告知风险，不静默）──────────────────────────────
    tracing::warn!("调度器与节点下发未实现（T03–T07 未落地）：任务只会入库排队，不会真正下载");
    if cfg.public_token.is_none() {
        tracing::warn!(
            addr = %cfg.public_addr,
            "对外口未配置 DISPATCH_PUBLIC_TOKEN：6800 无鉴权，任何可达者都能投递任务"
        );
    }
    if cfg.public_cors && cfg.public_token.is_none() {
        tracing::warn!(
            "已开启 CORS 但 DISPATCH_PUBLIC_TOKEN 未配置：任意网页可向内网投任务（drive-by）"
        );
    }

    // ── 管理口令 ─────────────────────────────────────────────────────────
    let admin_policy = Arc::new(config::resolve_admin_policy(&cfg));
    if admin_policy.generated {
        tracing::warn!(
            password = %admin_policy.password,
            "DISPATCH_ADMIN_PASSWORD 未设置：已生成随机管理口令（仅本次启动有效，重启会变）"
        );
    }
    if !is_loopback(&cfg.admin_addr)
        && admin_policy.cookie_secure != dispatch_core::state::CookieSecureMode::Always
    {
        tracing::warn!(
            addr = %cfg.admin_addr,
            "会话 Cookie 未强制 Secure，明文传输；请置于 TLS/反代之后，或勿将管理口暴露到不可信网络"
        );
    }

    // ── 数据库 ───────────────────────────────────────────────────────────
    let mut conn = store::open_connection(&cfg.db_path).context("打开 SQLite 失败")?;
    let applied = migrate::apply(&mut conn, &cfg.migrations_dir).context("执行迁移失败")?;
    let versions = migrate::applied_versions(&conn).context("读取迁移版本失败")?;
    tracing::info!(applied, versions = ?versions, "迁移完成");

    let store = Store::from_connection(conn);
    let now = dispatch_core::ids::now_secs();

    // ── 节点密码主密钥 ───────────────────────────────────────────────────
    // 取值顺序：DISPATCH_SECRET_KEY → <库目录>/node-secret.key → 生成并落盘。
    // 生成的密钥**必须**落在持久化卷里，否则重启后已存节点的密码再也回看不了。
    let (secrets, key_generated) = dispatch_core::secret::SecretBox::resolve(&cfg.db_path)
        .context("初始化节点密码主密钥失败")?;
    if key_generated {
        tracing::warn!(
            file = %dispatch_core::secret::key_file_path(&cfg.db_path).display(),
            "DISPATCH_SECRET_KEY 未设置：已生成节点密码主密钥并落盘。\
             该文件与数据库同等重要 —— 丢了就再也回看不了已保存的节点密码（需重新填写）。\
             容器部署请确认它位于持久化卷内。"
        );
    }
    let secrets = Arc::new(secrets);

    // ── 两端口状态 ───────────────────────────────────────────────────────
    let handshake = HandshakeState {
        proxy_client_id: resolve_proxy_client_id(),
        tokens: dispatch_core::ingress::handshake::HandshakeStore::new(),
    };
    let public_state = PublicState::new(
        store.clone(),
        now,
        cfg.public_token.clone(),
        cfg.public_cors,
        handshake,
    );

    let env = EnvSnapshot {
        public_addr: cfg.public_addr.clone(),
        admin_addr: cfg.admin_addr.clone(),
        db_path: cfg.db_path.display().to_string(),
        migrations_dir: cfg.migrations_dir.display().to_string(),
        log_level: cfg.log_level.clone(),
        state_dir: std::env::var("DISPATCH_STATE_DIR").ok(),
        web_dir: cfg.web_dir.as_ref().map(|p| p.display().to_string()),
        admin_password_configured: !admin_policy.generated,
        public_token_configured: cfg.public_token.is_some(),
        allow_file_delete: cfg.allow_file_delete,
        public_cors: cfg.public_cors,
        admin_cookie_secure: admin_policy.cookie_secure.as_str(),
    };
    let admin_state = AdminState::new(
        store.clone(),
        now,
        admin_policy,
        env,
        secrets,
        cfg.web_dir.clone(),
    );

    // ── 组装（业务路由来自 dispatch-core；/healthz 仍由 main 拥有）─────────
    let public_app = dispatch_core::ingress::router()
        .route("/healthz", get(healthz_public))
        .with_state(public_state.clone())
        // CORS 需 `from_fn_with_state`（`from_fn` 不支持 State 提取器）。
        .layer(axum::middleware::from_fn_with_state(
            public_state.clone(),
            dispatch_core::ingress::cors_layer,
        ));
    let admin_app = dispatch_core::admin::router()
        .route("/healthz", get(healthz_admin))
        .with_state(admin_state);

    let public_listener = TcpListener::bind(&cfg.public_addr)
        .await
        .with_context(|| format!("绑定对外端口 {} 失败", cfg.public_addr))?;
    let admin_listener = TcpListener::bind(&cfg.admin_addr)
        .await
        .with_context(|| format!("绑定管理端口 {} 失败", cfg.admin_addr))?;

    tracing::info!(
        "对外端口监听 {}；管理端口监听 {}",
        cfg.public_addr,
        cfg.admin_addr
    );

    let (tx, mut rx) = mpsc::channel::<String>(4);

    let tx_public = tx.clone();
    let h_public = tokio::spawn(async move {
        // 对外口也需 ConnectInfo：`/api/webui/login` 按来源 IP 限速依赖它。
        if let Err(e) = axum::serve(
            public_listener,
            public_app.into_make_service_with_connect_info::<SocketAddr>(),
        )
        .await
        {
            let _ = tx_public.send(format!("对外端口异常退出: {e}")).await;
        }
    });
    let tx_admin = tx.clone();
    let h_admin = tokio::spawn(async move {
        // 管理口需 ConnectInfo：登录防爆破按来源 IP。
        if let Err(e) = axum::serve(
            admin_listener,
            admin_app.into_make_service_with_connect_info::<SocketAddr>(),
        )
        .await
        {
            let _ = tx_admin.send(format!("管理端口异常退出: {e}")).await;
        }
    });

    tokio::select! {
        r = tokio::signal::ctrl_c() => {
            match r {
                Ok(()) => tracing::info!("收到中断信号，开始优雅退出"),
                Err(e) => tracing::warn!(error = %e, "信号监听异常，转入退出流程"),
            }
        }
        Some(msg) = rx.recv() => {
            tracing::error!(%msg, "listener 异常退出，进程准备退出");
        }
    }

    store.shutdown().await;
    h_public.abort();
    h_admin.abort();
    tracing::info!("已退出");
    Ok(())
}
