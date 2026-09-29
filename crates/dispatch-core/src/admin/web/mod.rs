//! 内嵌零构建 Web 管理台（3 屏 + 原生 ES 模块）。
//!
//! `include_str!` 把 `index.html` / `app.css` / `app.js` 编进二进制（零依赖、可复现）；
//! 若设置了 `DISPATCH_WEB_DIR`，则改从磁盘读同名文件（便于热改/排障）。

use std::sync::Arc;

use axum::extract::State;
use axum::http::{HeaderValue, StatusCode, header};
use axum::response::{IntoResponse, Response};

use crate::state::AdminState;

/// 内嵌入口 HTML。
const INDEX_HTML: &str = include_str!("index.html");
/// 内嵌样式。
const APP_CSS: &str = include_str!("app.css");
/// 内嵌脚本。
const APP_JS: &str = include_str!("app.js");

/// `GET /`、`/login`、`/tasks`、`/tasks/new`：都返回同一壳页面。
pub async fn index(State(st): State<Arc<AdminState>>, _headers: axum::http::HeaderMap) -> Response {
    html_response(read_asset(&st, "index.html", INDEX_HTML))
}

/// `GET /app.css`。
pub async fn asset_css(
    State(st): State<Arc<AdminState>>,
    _headers: axum::http::HeaderMap,
) -> Response {
    asset_response(
        read_asset(&st, "app.css", APP_CSS),
        "text/css; charset=utf-8",
    )
}

/// `GET /app.js`。
pub async fn asset_js(
    State(st): State<Arc<AdminState>>,
    _headers: axum::http::HeaderMap,
) -> Response {
    asset_response(
        read_asset(&st, "app.js", APP_JS),
        "text/javascript; charset=utf-8",
    )
}

/// 读取资源：优先磁盘目录，回落内嵌。
fn read_asset(st: &AdminState, name: &str, embedded: &'static str) -> String {
    if let Some(dir) = &st.web_dir {
        let path = dir.join(name);
        match std::fs::read_to_string(&path) {
            Ok(s) if !s.is_empty() => return s,
            Ok(_) => {
                tracing::warn!(path = %path.display(), "DISPATCH_WEB_DIR 下文件为空，回落内嵌")
            }
            Err(e) => {
                tracing::warn!(path = %path.display(), error = %e, "读 DISPATCH_WEB_DIR 失败，回落内嵌")
            }
        }
    }
    embedded.to_string()
}

fn html_response(body: String) -> Response {
    asset_response(body, "text/html; charset=utf-8")
}

fn asset_response(body: String, content_type: &'static str) -> Response {
    let mut resp = (StatusCode::OK, body).into_response();
    resp.headers_mut()
        .insert(header::CONTENT_TYPE, HeaderValue::from_static(content_type));
    resp
}
