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

mod config;

use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::routing::get;
use axum::{Json, Router};
use dispatch_core::store::{self, Store, migrate};
use tokio::net::TcpListener;
use tokio::sync::mpsc;
use tracing_subscriber::EnvFilter;

use crate::config::Config;

#[derive(Clone)]
struct AppState {
    store: Store,
    started_at: i64,
    /// `"public"` 或 `"admin"`，用于 `/healthz` 自报身份。
    port_role: &'static str,
}

fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// 健康检查：**两个端口都提供**（Docker `HEALTHCHECK` 打这个）。
///
/// 会真查一次库（`SELECT 1`），而不是无条件返回 ok —— 否则
/// "进程活着但库打不开"会被误判为健康。
async fn healthz(State(st): State<Arc<AppState>>) -> impl IntoResponse {
    let db_ok = st.store.query("SELECT 1", vec![]).await.is_ok();
    let body = serde_json::json!({
        "status": if db_ok { "ok" } else { "degraded" },
        "port": st.port_role,
        "db": db_ok,
        "uptime_seconds": now_secs().saturating_sub(st.started_at),
        "version": dispatch_core::VERSION,
    });
    let code = if db_ok {
        StatusCode::OK
    } else {
        StatusCode::SERVICE_UNAVAILABLE
    };
    (code, Json(body))
}

fn init_tracing(level: &str) {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new(level));
    tracing_subscriber::fmt().with_env_filter(filter).init();
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

    let mut conn = store::open_connection(&cfg.db_path).context("打开 SQLite 失败")?;
    let applied = migrate::apply(&mut conn, &cfg.migrations_dir).context("执行迁移失败")?;
    let versions = migrate::applied_versions(&conn).context("读取迁移版本失败")?;
    tracing::info!(applied, versions = ?versions, "迁移完成");

    let store = Store::from_connection(conn);

    let public_state = Arc::new(AppState {
        store: store.clone(),
        started_at: now_secs(),
        port_role: "public",
    });
    let admin_state = Arc::new(AppState {
        store: store.clone(),
        started_at: now_secs(),
        port_role: "admin",
    });

    let public_app = Router::new()
        .route("/healthz", get(healthz))
        .with_state(public_state);
    let admin_app = Router::new()
        .route("/healthz", get(healthz))
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
        if let Err(e) = axum::serve(public_listener, public_app).await {
            let _ = tx_public.send(format!("对外端口异常退出: {e}")).await;
        }
    });
    let tx_admin = tx.clone();
    let h_admin = tokio::spawn(async move {
        if let Err(e) = axum::serve(admin_listener, admin_app).await {
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
