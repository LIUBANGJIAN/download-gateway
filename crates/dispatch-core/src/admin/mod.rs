//! 管理口（8080）：会话 + REST + 内嵌零构建 Web 管理台。
//!
//! 返回**未 apply state** 的 `Router<Arc<AdminState>>`；`main.rs` 再挂 `/healthz`。
//! 全部 REST 响应统一信封 `{code, data, message}`（`code=0` 成功）。

pub mod config;
pub mod rest;
pub mod session;
pub mod web;

use std::sync::Arc;

use axum::Json;
use axum::Router;
use axum::extract::DefaultBodyLimit;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use serde_json::Value;

use crate::state::AdminState;

/// 管理口请求体上限（比对外口更严：管理请求都很小）。
pub const ADMIN_BODY_LIMIT: usize = 1024 * 1024;

/// 错误码：未认证。
pub const C_UNAUTHENTICATED: i64 = 40100;
/// 错误码：CSRF 校验失败。
pub const C_CSRF_INVALID: i64 = 40301;
/// 错误码：资源不存在。
pub const C_NOT_FOUND: i64 = 40400;
/// 错误码：前置条件失败。
pub const C_PRECONDITION_FAILED: i64 = 40900;
/// 错误码：校验错误。
pub const C_VALIDATION_ERROR: i64 = 42200;
/// 错误码：请求过于频繁。
pub const C_TOO_MANY_REQUESTS: i64 = 42900;
/// 错误码：服务器内部错误。
pub const C_INTERNAL: i64 = 50000;

/// 成功信封。
pub fn ok(data: Value) -> Response {
    (
        StatusCode::OK,
        Json(serde_json::json!({ "code": 0, "data": data, "message": "ok" })),
    )
        .into_response()
}

/// 失败信封。
pub fn err(status: StatusCode, code: i64, message: impl Into<String>) -> Response {
    (
        status,
        Json(serde_json::json!({ "code": code, "data": Value::Null, "message": message.into() })),
    )
        .into_response()
}

/// 装配 8080 全部路径。
pub fn router() -> Router<Arc<AdminState>> {
    Router::new()
        // Web 壳（SPA 路由都回同一页面）
        .route("/", get(web::index))
        .route("/login", get(web::index))
        .route("/tasks", get(web::index))
        .route("/tasks/new", get(web::index))
        .route("/app.css", get(web::asset_css))
        .route("/app.js", get(web::asset_js))
        // REST
        .route("/api/admin/login", post(rest::login))
        .route("/api/admin/logout", post(rest::logout))
        .route("/api/admin/summary", get(rest::summary))
        .route(
            "/api/admin/tasks",
            get(rest::tasks_list).post(rest::tasks_create),
        )
        .route("/api/admin/tasks/{id}/{action}", post(rest::task_action))
        .route("/api/admin/nodes", get(rest::nodes_list))
        .route("/api/admin/config", get(rest::config_get))
        // 体积上限必须挂在持有 extractor 的这层 Router 上。
        .layer(DefaultBodyLimit::max(ADMIN_BODY_LIMIT))
}
