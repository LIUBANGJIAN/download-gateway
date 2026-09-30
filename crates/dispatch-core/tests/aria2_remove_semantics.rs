//! `aria2.remove` **软删**语义 + `aria2.removeDownloadResult` **真正清除** 的集成测试（缺陷 C-2）。
//!
//! 复用 `ingress_smoke.rs` / `handshake_probe.rs` 的脚手架写法：内存库 + 真迁移、装配公共
//! `ingress::router`、用 `oneshot` 直打 `Router`（不监听端口）。
//!
//! 依据 aria2 官方手册：
//! - `aria2.remove`：*"...If the specified download is in progress, it is first stopped. The
//!   status of the removed download becomes `removed`. This method returns GID of removed
//!   download."* ⇒ 软删 + 返回 GID。
//! - `aria2.forceRemove`：*"behaves just like aria2.remove except ... without performing any
//!   actions which take time"* ⇒ 行为与 `remove` 一致。
//! - `aria2.removeDownloadResult`：*"removes a completed/error/removed download denoted by gid
//!   from memory. This method returns `OK` for success."* ⇒ 真正清除 + 返回 `"OK"`。
//!
//! ⚠️ aria2 面的**鉴权失败/业务错误也是 HTTP 200 + 体内 `error.code`**，故此文件一律断言
//! `jsonrpc` 响应体的 `error.code`，**不**判 HTTP 状态码（业务只判 `StatusCode::OK`）。
//! `public_router(&store, None)` 表示**不配公共 token**（放行），测试不需要带 token。

use std::net::SocketAddr;

use axum::Router;
use axum::body::Body;
use axum::extract::ConnectInfo;
use axum::http::{Request, StatusCode};
use dispatch_core::ingress::handshake::HandshakeStore;
use dispatch_core::state::{HandshakeState, PublicState};
use dispatch_core::store::{Store, migrate, open_connection};
use serde_json::{Value, json};
use tower::ServiceExt;

/// aria2 业务错误码（源码 `ingress/aria2.rs` 的 `E_ARIA_GENERIC`）。
///
/// 该常量非 `pub`，无法从外部 crate 导入，故此处以字面量复刻。
/// 注意：官方 aria2 对「GID 不存在」同样返回业务错误码 **1**（不是 -1）；
/// `-1` 是**鉴权失败**（`ingress/envelope.rs` 的 `ARIA2_UNAUTHORIZED`）。二者勿混。
const E_ARIA_GENERIC: i64 = 1;

// ---------------------------------------------------------------------------
// 脚手架（照抄 `ingress_smoke.rs` / `handshake_probe.rs` 的写法）
// ---------------------------------------------------------------------------

fn migrated_store() -> Store {
    let mut conn = open_connection(":memory:").unwrap();
    let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("..")
        .join("..")
        .join("migrations");
    migrate::apply(&mut conn, &dir).unwrap();
    Store::from_connection(conn)
}

fn public_router(store: &Store, token: Option<&str>) -> Router {
    let hs = HandshakeState {
        proxy_client_id: "aria2-remove-test".into(),
        tokens: HandshakeStore::new(),
    };
    let st = PublicState::new(store.clone(), 0, token.map(str::to_string), false, hs);
    dispatch_core::ingress::router().with_state(st)
}

fn addr() -> SocketAddr {
    "127.0.0.1:12345".parse().unwrap()
}

fn req(method: &str, uri: &str, body: Body) -> Request<Body> {
    let mut r = Request::builder()
        .method(method)
        .uri(uri)
        .header("content-type", "application/json")
        .body(body)
        .unwrap();
    r.extensions_mut().insert(ConnectInfo(addr()));
    r
}

async fn call(router: &Router, req: Request<Body>) -> (StatusCode, Value) {
    let resp = router.clone().oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = axum::body::to_bytes(resp.into_body(), usize::MAX)
        .await
        .unwrap();
    let v = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, v)
}

/// 打一次 `/jsonrpc`，返回解析后的响应体（并断言 HTTP 恒为 200）。
async fn rpc(app: &Router, method: &str, params: Value) -> Value {
    let body = json!({ "jsonrpc": "2.0", "id": "1", "method": method, "params": params });
    let (status, v) = call(app, req("POST", "/jsonrpc", Body::from(body.to_string()))).await;
    assert_eq!(
        status,
        StatusCode::OK,
        "aria2 面一律 HTTP 200，实际 {status}: {v}"
    );
    v
}

/// 用 `aria2.addUri` 建一条任务，返回其 GID。
async fn add_task(app: &Router, url: &str) -> String {
    let v = rpc(app, "aria2.addUri", json!([[url]])).await;
    v["result"]
        .as_str()
        .unwrap_or_else(|| panic!("aria2.addUri 应返回 gid，实际: {v}"))
        .to_string()
}

/// 从 `tellActive`/`tellWaiting`/`tellStopped` 的结果数组里抽出全部 gid。
fn gids_of(v: &Value) -> Vec<String> {
    v["result"]
        .as_array()
        .map(|a| {
            a.iter()
                .filter_map(|t| t["gid"].as_str().map(str::to_string))
                .collect()
        })
        .unwrap_or_default()
}

/// 在 `tellStopped` 结果里按 gid 找到对应条目（找不到即失败）。
fn stopped_entry<'a>(v: &'a Value, gid: &str) -> &'a Value {
    v["result"]
        .as_array()
        .unwrap_or_else(|| panic!("tellStopped result 应为数组: {v}"))
        .iter()
        .find(|t| t["gid"].as_str() == Some(gid))
        .unwrap_or_else(|| panic!("tellStopped 应包含 gid={gid}，实际: {v}"))
}

// ---------------------------------------------------------------------------
// 1-6. `remove` 软删 → `removeDownloadResult` 真正清除
// ---------------------------------------------------------------------------

/// 核心用例：`remove` 只进 `removed` 终态（仍可查），`removeDownloadResult` 才真正清除。
#[tokio::test]
async fn remove_is_soft_delete_then_remove_download_result_purges() {
    let store = migrated_store();
    let app = public_router(&store, None);
    let gid = add_task(&app, "http://example.com/a.bin").await;

    // 1) aria2.remove(gid) → result 就是该 gid（字符串）。
    let v = rpc(&app, "aria2.remove", json!([gid])).await;
    assert_eq!(
        v["result"],
        json!(gid),
        "remove 应返回被删的 gid，实际: {v}"
    );

    // 2) remove 之后 tellStatus 仍查得到，且 status == "removed"（旧实现会删行 ⇒ 此断言必失败）。
    let v = rpc(&app, "aria2.tellStatus", json!([gid])).await;
    assert_eq!(
        v["result"]["gid"],
        json!(gid),
        "软删后 tellStatus 仍应查得到，实际: {v}"
    );
    assert_eq!(
        v["result"]["status"],
        json!("removed"),
        "软删后 status 应为 removed，实际: {v}"
    );

    // 3) tellStopped 里包含该 gid，且其 status == "removed"。
    let v = rpc(&app, "aria2.tellStopped", json!([0, 100])).await;
    assert_eq!(
        stopped_entry(&v, &gid)["status"],
        json!("removed"),
        "tellStopped 里的已删任务 status 应为 removed，实际: {v}"
    );

    // 4) tellWaiting 与 tellActive 不再包含该 gid。
    let v = rpc(&app, "aria2.tellWaiting", json!([0, 100])).await;
    assert!(
        !gids_of(&v).contains(&gid),
        "tellWaiting 不应包含已 removed 的任务，实际: {v}"
    );
    let v = rpc(&app, "aria2.tellActive", json!([0, 100])).await;
    assert!(
        !gids_of(&v).contains(&gid),
        "tellActive 不应包含已 removed 的任务，实际: {v}"
    );

    // 5) removeDownloadResult(gid) → 返回 "OK"。
    let v = rpc(&app, "aria2.removeDownloadResult", json!([gid])).await;
    assert_eq!(
        v["result"],
        json!("OK"),
        "removeDownloadResult 应返回 OK，实际: {v}"
    );

    // 6) 之后再 tellStatus(gid) → 已不在（error，code == E_ARIA_GENERIC），tellStopped 也不再包含。
    let v = rpc(&app, "aria2.tellStatus", json!([gid])).await;
    assert!(
        v.get("error").is_some(),
        "清除后 tellStatus 应报错（GID 不存在），实际: {v}"
    );
    assert_eq!(
        v["error"]["code"],
        json!(E_ARIA_GENERIC),
        "清除后 tellStatus 的错误码应为 E_ARIA_GENERIC，实际: {v}"
    );
    let v = rpc(&app, "aria2.tellStopped", json!([0, 100])).await;
    assert!(
        !gids_of(&v).contains(&gid),
        "清除后 tellStopped 不应再包含该 gid，实际: {v}"
    );

    store.shutdown().await;
}

// ---------------------------------------------------------------------------
// 7. 对不存在的 gid 调 remove → 业务错误
// ---------------------------------------------------------------------------

/// 不存在的 GID ⇒ `error.code == E_ARIA_GENERIC`（不要判 HTTP 状态码，它仍是 200）。
#[tokio::test]
async fn remove_unknown_gid_is_business_error() {
    let store = migrated_store();
    let app = public_router(&store, None);

    let v = rpc(&app, "aria2.remove", json!(["ffffffffffffffff"])).await;
    assert!(
        v.get("error").is_some(),
        "不存在的 gid 应返回 error 对象，实际: {v}"
    );
    assert_eq!(
        v["error"]["code"],
        json!(E_ARIA_GENERIC),
        "不存在的 gid 错误码应为 E_ARIA_GENERIC，实际: {v}"
    );

    store.shutdown().await;
}

// ---------------------------------------------------------------------------
// 8. system.listMethods 广告 removeDownloadResult
// ---------------------------------------------------------------------------

/// `system.listMethods` 的返回里必须包含 `aria2.removeDownloadResult`。
#[tokio::test]
async fn list_methods_advertises_remove_download_result() {
    let store = migrated_store();
    let app = public_router(&store, None);

    let v = rpc(&app, "system.listMethods", json!([])).await;
    let methods: Vec<&str> = v["result"]
        .as_array()
        .unwrap_or_else(|| panic!("listMethods result 应为数组: {v}"))
        .iter()
        .filter_map(Value::as_str)
        .collect();
    assert!(
        methods.contains(&"aria2.removeDownloadResult"),
        "listMethods 应广告 aria2.removeDownloadResult，实际: {v}"
    );

    store.shutdown().await;
}

// ---------------------------------------------------------------------------
// 9. forceRemove 与 remove 行为一致
// ---------------------------------------------------------------------------

/// `aria2.forceRemove` 与 `aria2.remove` 等价：返回 gid、进 `removed` 终态、进 `tellStopped`。
#[tokio::test]
async fn force_remove_behaves_like_remove() {
    let store = migrated_store();
    let app = public_router(&store, None);
    let gid = add_task(&app, "http://example.com/b.bin").await;

    let v = rpc(&app, "aria2.forceRemove", json!([gid])).await;
    assert_eq!(
        v["result"],
        json!(gid),
        "forceRemove 应返回被删的 gid，实际: {v}"
    );

    // 复用断言 2：仍可查到且 status == "removed"。
    let v = rpc(&app, "aria2.tellStatus", json!([gid])).await;
    assert_eq!(
        v["result"]["status"],
        json!("removed"),
        "forceRemove 后 status 应为 removed，实际: {v}"
    );

    // 复用断言 3：tellStopped 包含且 status == "removed"。
    let v = rpc(&app, "aria2.tellStopped", json!([0, 100])).await;
    assert_eq!(
        stopped_entry(&v, &gid)["status"],
        json!("removed"),
        "forceRemove 后 tellStopped 应包含且为 removed，实际: {v}"
    );

    store.shutdown().await;
}
