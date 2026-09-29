//! 对外口（6800）路由装配。
//!
//! 返回**未 apply state** 的 `Router<Arc<PublicState>>`；`main.rs` 再挂 `/healthz` 并
//! `.with_state(...)`。这样 `/healthz` 仍由 `main` 拥有（契约零风险），业务路由归本 crate。

pub mod aria2;
pub mod bitcomet;
pub mod envelope;
pub mod handshake;

use std::sync::Arc;

use axum::Router;
use axum::extract::{DefaultBodyLimit, Request, State};
use axum::http::{HeaderValue, Method, StatusCode, header};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use axum::routing::post;

use crate::state::PublicState;

/// 对外口请求体上限（`02 §4.4`）。握手 handler 内另有 64 KiB 更严闸门。
pub const PUBLIC_BODY_LIMIT: usize = 8 * 1024 * 1024;

/// 装配 6800 全部业务路径（未 apply state）。
///
/// CORS 由 [`cors_layer`] 提供，`main.rs` 在 `.with_state(...)` 之后用
/// `axum::middleware::from_fn_with_state` 挂上（`from_fn` 不支持提取 `State`）。
pub fn router() -> Router<Arc<PublicState>> {
    Router::new()
        // Aria2 兼容面
        .route("/jsonrpc", post(aria2::jsonrpc))
        // BitComet 兼容面（8 条复刻路径）
        .route("/api/task/http/add", post(bitcomet::http_add))
        .route("/api/task/bt/add", post(bitcomet::bt_add))
        .route(
            "/api/task/torrent_links/add",
            post(bitcomet::torrent_links_add),
        )
        .route("/api_v2/task_list/get", post(bitcomet::task_list_get))
        .route("/api_v2/tasks/action", post(bitcomet::tasks_action))
        .route("/api_v2/tasks/delete", post(bitcomet::tasks_delete))
        .route("/api/config/about/get", post(bitcomet::about_get))
        .route("/api/task/summary/get", post(bitcomet::summary_get))
        // 自签三段式握手
        .route("/api/webui/ip_verify", post(handshake::ip_verify))
        .route("/api/webui/login", post(handshake::login))
        .route("/api/device_token/get", post(handshake::device_token_get))
        // 体积上限必须**挂在持有 extractor 的那层 Router 上**，否则不生效。
        .layer(DefaultBodyLimit::max(PUBLIC_BODY_LIMIT))
}

/// CORS 中间件：`cors_enabled` 时才回 `Access-Control-Allow-Origin: *`，并处理 OPTIONS 预检。
///
/// ⚠️ 用 `from_fn_with_state` 挂载（`from_fn` 不支持 `State` 提取器）。
pub async fn cors_layer(State(st): State<Arc<PublicState>>, req: Request, next: Next) -> Response {
    if !st.cors_enabled {
        return next.run(req).await;
    }
    let is_preflight = req.method() == Method::OPTIONS;
    let mut resp = if is_preflight {
        StatusCode::NO_CONTENT.into_response()
    } else {
        next.run(req).await
    };
    let headers = resp.headers_mut();
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_ORIGIN,
        HeaderValue::from_static("*"),
    );
    if is_preflight {
        headers.insert(
            header::ACCESS_CONTROL_ALLOW_METHODS,
            HeaderValue::from_static("POST, OPTIONS"),
        );
        headers.insert(
            header::ACCESS_CONTROL_ALLOW_HEADERS,
            HeaderValue::from_static("Content-Type, Authorization"),
        );
        headers.insert(
            header::ACCESS_CONTROL_MAX_AGE,
            HeaderValue::from_static("600"),
        );
    }
    resp
}
