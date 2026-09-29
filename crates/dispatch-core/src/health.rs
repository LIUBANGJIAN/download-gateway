//! `/healthz` 响应体：**契约与旧 `src/main.rs::healthz` 逐字节一致**。
//!
//! 搬到本模块只是为了让 `main.rs` 退化为纯装配，并让两个端口共用同一实现；
//! 字段名、字段顺序（`serde_json` 默认按 BTreeMap 升序）、`port` 取值、`SELECT 1` 真查库、
//! 库不通 → 503 —— 全部与重构前保持不变。

use axum::Json;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};

use crate::store::Store;

/// 由 store 存活性 + 启动时刻 + 端口身份（`"public"` / `"admin"`）生成健康响应。
///
/// 会**真查一次库**（`SELECT 1`），而不是无条件返回 ok —— 否则
/// "进程活着但库打不开"会被误判为健康。
pub async fn health_body(store: &Store, started_at: i64, role: &'static str) -> Response {
    let db_ok = store.query("SELECT 1", vec![]).await.is_ok();
    let body = serde_json::json!({
        "status": if db_ok { "ok" } else { "degraded" },
        "port": role,
        "db": db_ok,
        "uptime_seconds": crate::ids::now_secs().saturating_sub(started_at),
        "version": crate::VERSION,
    });
    let code = if db_ok {
        StatusCode::OK
    } else {
        StatusCode::SERVICE_UNAVAILABLE
    };
    (code, Json(body)).into_response()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::Store;

    #[tokio::test]
    async fn healthy_store_returns_200() {
        let store = Store::open_in_memory().unwrap();
        let resp = health_body(&store, crate::ids::now_secs(), "public").await;
        assert_eq!(resp.status(), StatusCode::OK);
        store.shutdown().await;
    }

    /// 库不通 → 503（契约未破）：把 store actor 关停后 `SELECT 1` 失败。
    #[tokio::test]
    async fn dead_store_returns_503() {
        let store = Store::open_in_memory().unwrap();
        store.shutdown().await;
        let resp = health_body(&store, crate::ids::now_secs(), "admin").await;
        assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
    }
}
