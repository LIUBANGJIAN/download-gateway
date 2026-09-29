//! 集成冒烟：起内存 store，直调两个 Router，验 404 消除与入库路径。

use std::net::SocketAddr;
use std::sync::Arc;

use axum::Router;
use axum::body::Body;
use axum::extract::ConnectInfo;
use axum::http::{Request, StatusCode, header};
use dispatch_core::ingress::handshake::HandshakeStore;
use dispatch_core::state::{
    AdminPolicy, AdminState, CookieSecureMode, EnvSnapshot, HandshakeState, PublicState,
};
use dispatch_core::store::{Store, migrate, open_connection};
use serde_json::{Value, json};
use tower::ServiceExt;

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
        proxy_client_id: "integration-test".into(),
        tokens: HandshakeStore::new(),
    };
    let st = PublicState::new(store.clone(), 0, token.map(str::to_string), false, hs);
    dispatch_core::ingress::router().with_state(st)
}

fn admin_router(store: &Store) -> Router {
    let policy = Arc::new(AdminPolicy {
        password: "test123".into(),
        generated: false,
        cookie_secure: CookieSecureMode::Auto,
        allow_file_delete: false,
    });
    let env = EnvSnapshot {
        public_addr: "127.0.0.1:6800".into(),
        admin_addr: "127.0.0.1:8080".into(),
        db_path: "mem".into(),
        migrations_dir: "migrations".into(),
        log_level: "info".into(),
        state_dir: None,
        web_dir: None,
        admin_password_configured: true,
        public_token_configured: false,
        allow_file_delete: false,
        public_cors: false,
        admin_cookie_secure: "auto",
    };
    let st = AdminState::new(store.clone(), 0, policy, env, None);
    dispatch_core::admin::router().with_state(st)
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

async fn call(router: &Router, req: Request<Body>) -> (StatusCode, Vec<u8>) {
    let resp = router.clone().oneshot(req).await.unwrap();
    let status = resp.status();
    let bytes = axum::body::to_bytes(resp.into_body(), usize::MAX)
        .await
        .unwrap();
    (status, bytes.to_vec())
}

/// 登录管理台，返回可直接用作 `Cookie` 头的 `sid=…` 串。
async fn login_sid(app: &Router) -> String {
    let body = json!({ "password": "test123" });
    let resp = app
        .clone()
        .oneshot(req(
            "POST",
            "/api/admin/login",
            Body::from(body.to_string()),
        ))
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK, "登录应成功");
    let sc = resp
        .headers()
        .get(header::SET_COOKIE)
        .expect("登录响应必须带 Set-Cookie")
        .to_str()
        .unwrap()
        .to_string();
    sc.split(';').next().unwrap().trim().to_string()
}

#[tokio::test]
async fn admin_root_is_no_longer_404_and_carries_banner() {
    let store = migrated_store();
    let app = admin_router(&store);
    let (status, body) = call(&app, req("GET", "/", Body::empty())).await;
    assert_eq!(status, StatusCode::OK, "管理台首页应 200（原 404）");
    let html = String::from_utf8(body).unwrap();
    assert!(
        html.contains("仅受理任务，调度/下发未启用"),
        "首页必须含「未启用」横幅明文"
    );
    store.shutdown().await;
}

#[tokio::test]
async fn aria2_add_uri_then_tell_status_and_unknown_method() {
    let store = migrated_store();
    let app = public_router(&store, None);

    let add = json!({"jsonrpc":"2.0","id":"1","method":"aria2.addUri","params":[["http://example.com/a.bin"]]});
    let (_, body) = call(&app, req("POST", "/jsonrpc", Body::from(add.to_string()))).await;
    let v: Value = serde_json::from_slice(&body).unwrap();
    let gid = v["result"].as_str().unwrap().to_string();
    assert_eq!(gid.len(), 16, "GID 应为 16 位 hex: {gid}");
    assert!(
        gid.chars()
            .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase())
    );

    let tell = json!({"jsonrpc":"2.0","id":"2","method":"aria2.tellStatus","params":[gid]});
    let (_, body) = call(&app, req("POST", "/jsonrpc", Body::from(tell.to_string()))).await;
    let v: Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(v["result"]["status"], "waiting");
    assert_eq!(v["result"]["dir"], "");
    assert_eq!(v["result"]["dirKnown"], false);
    assert_eq!(v["result"]["dispatched"], false);

    let unknown = json!({"jsonrpc":"2.0","id":"3","method":"aria2.noSuchMethod","params":[]});
    let (status, body) = call(
        &app,
        req("POST", "/jsonrpc", Body::from(unknown.to_string())),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "未知方法仍应 200（JSON-RPC 语义）");
    let v: Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(
        v["error"]["code"], -32601,
        "未知方法必须是 -32601，不是 404"
    );
    store.shutdown().await;
}

/// 回归 BUG-1：顶层请求缺 `jsonrpc` 字段 → `-32600 Invalid Request`（设计 §8.1）。
#[tokio::test]
async fn aria2_missing_jsonrpc_field_is_invalid_request() {
    let store = migrated_store();
    let app = public_router(&store, None);
    let body = json!({"id":"1","method":"aria2.getVersion","params":[]});
    let (status, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(body.to_string()))).await;
    assert_eq!(status, StatusCode::OK, "JSON-RPC 语义下仍应 HTTP 200");
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(v["error"]["code"], -32600, "缺 jsonrpc 必须 -32600: {v}");
    assert_eq!(
        v["error"]["message"],
        "Invalid Request: missing or invalid jsonrpc (must be \"2.0\")"
    );

    // 值不符（非 "2.0"）同样拒绝
    let body = json!({"jsonrpc":"1.0","id":"2","method":"aria2.getVersion","params":[]});
    let (_, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(body.to_string()))).await;
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(v["error"]["code"], -32600, "jsonrpc!=2.0 必须 -32600: {v}");
    store.shutdown().await;
}

#[tokio::test]
async fn bitcomet_http_add_accepts_and_returns_proxy_task_id() {
    let store = migrated_store();
    let app = public_router(&store, None);
    let body =
        json!({"url":"http://example.com/a.bin","start_later":false,"max_connection_count":4});
    let (status, bytes) = call(
        &app,
        req("POST", "/api/task/http/add", Body::from(body.to_string())),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(v["error_code"], "OK");
    assert!(v["proxy_task_id"].is_string());
    assert!(v["gid"].is_string());
    store.shutdown().await;
}

#[tokio::test]
async fn protected_route_rejects_unknown_bearer_when_token_configured() {
    let store = migrated_store();
    let app = public_router(&store, Some("secret"));
    let body = json!({"url":"http://example.com/a.bin"});
    let mut r = req("POST", "/api/task/http/add", Body::from(body.to_string()));
    r.headers_mut()
        .insert("authorization", "Bearer wrong-token".parse().unwrap());
    let (status, bytes) = call(&app, r).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(v["error_code"], "INVALID_TOKEN");
    store.shutdown().await;
}

#[tokio::test]
async fn admin_tasks_requires_session() {
    let store = migrated_store();
    let app = admin_router(&store);
    let (status, _) = call(&app, req("GET", "/api/admin/summary", Body::empty())).await;
    assert_eq!(
        status,
        StatusCode::UNAUTHORIZED,
        "无会话访问 summary 应 401"
    );
    store.shutdown().await;
}

/// 回归 §R：`system.multicall` 的**外层不校验 token**（官方手册：token 放在每个子调用里）。
/// 情形 1：外层不带 token、子调用各带 → 两个单元素数组，无顶层 error。
#[tokio::test]
async fn multicall_outer_without_token_succeeds() {
    let store = migrated_store();
    let app = public_router(&store, Some("secret"));
    let body = json!({
        "jsonrpc":"2.0","id":"1","method":"system.multicall",
        "params":[[
            {"methodName":"aria2.getVersion","params":["token:secret"]},
            {"methodName":"aria2.getGlobalStat","params":["token:secret"]}
        ]]
    });
    let (status, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(body.to_string()))).await;
    assert_eq!(status, StatusCode::OK);
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert!(
        v.get("error").is_none(),
        "外层不应带 token 但被拒（回归缺陷）: {v}"
    );
    let r = v["result"].as_array().expect("result 应为数组");
    assert_eq!(r.len(), 2);
    assert!(r[0].as_array().unwrap()[0]["version"].is_string());
    assert!(r[1].as_array().unwrap()[0]["numWaiting"].is_string());
    store.shutdown().await;
}

/// 情形 2：外层带 token、子调用不带 → HTTP 200，逐个子调用失败（`-1`），而非顶层 error。
#[tokio::test]
async fn multicall_outer_token_without_subtokens_yields_per_call_faults() {
    let store = migrated_store();
    let app = public_router(&store, Some("secret"));
    let body = json!({
        "jsonrpc":"2.0","id":"1","method":"system.multicall",
        "params":["token:secret",[
            {"methodName":"aria2.getVersion","params":[]},
            {"methodName":"aria2.getGlobalStat","params":[]}
        ]]
    });
    let (status, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(body.to_string()))).await;
    assert_eq!(status, StatusCode::OK);
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert!(v.get("error").is_none(), "外层应只剥离不校验: {v}");
    let r = v["result"].as_array().expect("result 应为数组");
    assert_eq!(r.len(), 2);
    for item in r {
        assert_eq!(item["code"], -1, "子调用缺 token 应逐个失败: {item}");
        assert_eq!(item["message"], "Unauthorized");
    }
    store.shutdown().await;
}

/// 情形 3：两层都带 token → 与情形 1 同结果（容忍两种形态）。
#[tokio::test]
async fn multicall_both_layers_with_token_succeeds() {
    let store = migrated_store();
    let app = public_router(&store, Some("secret"));
    let body = json!({
        "jsonrpc":"2.0","id":"1","method":"system.multicall",
        "params":["token:secret",[
            {"methodName":"aria2.getVersion","params":["token:secret"]},
            {"methodName":"aria2.getGlobalStat","params":["token:secret"]}
        ]]
    });
    let (status, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(body.to_string()))).await;
    assert_eq!(status, StatusCode::OK);
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert!(v.get("error").is_none(), "两层都带 token 必须成功: {v}");
    let r = v["result"].as_array().expect("result 应为数组");
    assert_eq!(r.len(), 2);
    assert!(r[0].as_array().unwrap()[0]["version"].is_string());
    store.shutdown().await;
}

/// 情形 4（防线保留）：子调用既不带外层又带 token，但 token 错 → 逐个失败。
#[tokio::test]
async fn multicall_wrong_subtoken_still_rejected() {
    let store = migrated_store();
    let app = public_router(&store, Some("secret"));
    let body = json!({
        "jsonrpc":"2.0","id":"1","method":"system.multicall",
        "params":[[
            {"methodName":"aria2.getVersion","params":["token:wrong"]}
        ]]
    });
    let (status, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(body.to_string()))).await;
    assert_eq!(status, StatusCode::OK);
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    let r = v["result"].as_array().expect("result 应为数组");
    assert_eq!(r[0]["code"], -1, "子调用错 token 应失败: {v}");
    store.shutdown().await;
}

/// 防线保留：multicall 子调用数 > 32 → 顶层 `-32600`。
#[tokio::test]
async fn multicall_over_32_is_rejected() {
    let store = migrated_store();
    let app = public_router(&store, Some("secret"));
    let calls: Vec<Value> = (0..33)
        .map(|_| json!({"methodName":"aria2.getVersion","params":["token:secret"]}))
        .collect();
    let body = json!({
        "jsonrpc":"2.0","id":"1","method":"system.multicall","params":[calls]
    });
    let (status, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(body.to_string()))).await;
    assert_eq!(status, StatusCode::OK);
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(v["error"]["code"], -32600, "超 32 条应顶层拒绝: {v}");
    store.shutdown().await;
}

/// 回归 BUG-2：`GET /api/admin/tasks?page=0` → `422` + `code=42200`，不得静默 clamp。
#[tokio::test]
async fn admin_tasks_page_below_one_is_422() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;
    let mut r = req("GET", "/api/admin/tasks?page=0", Body::empty());
    r.headers_mut()
        .insert(header::COOKIE, cookie.parse().unwrap());
    let (status, bytes) = call(&app, r).await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY, "page<1 应 422");
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(v["code"], 42200, "错误码应为 42200: {v}");
    store.shutdown().await;
}

/// 回归 BUG-3：活跃（已认证）请求须重发 `Set-Cookie`，`Max-Age` 滑动刷新为 1800。
#[tokio::test]
async fn admin_active_request_resends_renewal_cookie() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;
    let mut r = req("GET", "/api/admin/summary", Body::empty());
    r.headers_mut()
        .insert(header::COOKIE, cookie.parse().unwrap());
    let resp = app.clone().oneshot(r).await.unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let sc = resp
        .headers()
        .get(header::SET_COOKIE)
        .expect("活跃响应必须重发 Set-Cookie")
        .to_str()
        .unwrap()
        .to_string();
    assert!(sc.contains("Max-Age=1800"), "Max-Age 应为 1800: {sc}");
    store.shutdown().await;
}

/// 回归 BUG-5：文件名未知时 `files[].path` 留空串，不得用 URL 冒充路径。
#[tokio::test]
async fn aria2_get_files_path_empty_when_name_unknown() {
    let store = migrated_store();
    let app = public_router(&store, None);
    let add =
        json!({"jsonrpc":"2.0","id":"1","method":"aria2.addUri","params":[["http://e/x.bin"]]});
    let (_, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(add.to_string()))).await;
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    let gid = v["result"].as_str().unwrap().to_string();

    let gf = json!({"jsonrpc":"2.0","id":"2","method":"aria2.getFiles","params":[gid]});
    let (_, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(gf.to_string()))).await;
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(
        v["result"][0]["path"], "",
        "未知文件名时 path 应为空串: {v}"
    );
    store.shutdown().await;
}

/// 裁决-1：`tellWaiting` 应用 `offset`/`num` 截取。
#[tokio::test]
async fn aria2_tell_waiting_applies_offset_and_num() {
    let store = migrated_store();
    let app = public_router(&store, None);
    for i in 0..3 {
        let add = json!({"jsonrpc":"2.0","id":"1","method":"aria2.addUri","params":[[format!("http://e/{i}.bin")]]});
        let _ = call(&app, req("POST", "/jsonrpc", Body::from(add.to_string()))).await;
    }
    let all = json!({"jsonrpc":"2.0","id":"2","method":"aria2.tellWaiting","params":[0,100]});
    let (_, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(all.to_string()))).await;
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(v["result"].as_array().unwrap().len(), 3);

    let one = json!({"jsonrpc":"2.0","id":"3","method":"aria2.tellWaiting","params":[1,1]});
    let (_, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(one.to_string()))).await;
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(
        v["result"].as_array().unwrap().len(),
        1,
        "offset/num 应生效: {v}"
    );

    let none = json!({"jsonrpc":"2.0","id":"4","method":"aria2.tellWaiting","params":[0,0]});
    let (_, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(none.to_string()))).await;
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(v["result"].as_array().unwrap().len(), 0, "num=0 应空");
    store.shutdown().await;
}

/// 裁决-1：`tellStopped` 返回终态集合、`getGlobalStat` 的 `numStopped` 用真实计数。
#[tokio::test]
async fn aria2_tellstopped_and_globalstat_use_real_counts() {
    let store = migrated_store();
    let app = public_router(&store, None);
    for u in ["http://e/a.bin", "http://e/b.bin"] {
        let add = json!({"jsonrpc":"2.0","id":"1","method":"aria2.addUri","params":[[u]]});
        let _ = call(&app, req("POST", "/jsonrpc", Body::from(add.to_string()))).await;
    }
    // 直接改库：把 b.bin 置为终态（模拟已有 complete 行）
    store
        .execute(
            "UPDATE task SET internal_state='completed', aria_status='complete', \
             permillage=1000, completed_at=1 WHERE url_raw='http://e/b.bin'",
            vec![],
        )
        .await
        .unwrap();

    let gs = json!({"jsonrpc":"2.0","id":"2","method":"aria2.getGlobalStat","params":[]});
    let (_, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(gs.to_string()))).await;
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(v["result"]["numWaiting"], "1", "numWaiting 应为 1: {v}");
    assert_eq!(
        v["result"]["numStopped"], "1",
        "numStopped 应为真实计数: {v}"
    );

    let ts = json!({"jsonrpc":"2.0","id":"3","method":"aria2.tellStopped","params":[0,10]});
    let (_, bytes) = call(&app, req("POST", "/jsonrpc", Body::from(ts.to_string()))).await;
    let v: Value = serde_json::from_slice(&bytes).unwrap();
    let r = v["result"].as_array().unwrap();
    assert_eq!(r.len(), 1, "终态应被 tellStopped 返回: {v}");
    assert_eq!(r[0]["status"], "complete");
    store.shutdown().await;
}
