//! 握手联调探针 + 三段式握手端到端集成测试。
//!
//! `#[ignore]` 的那条用于**手工生成一条合法 `/api/webui/login` 密文**（写入真客户端/curl 联调）。

use std::net::SocketAddr;

use axum::body::Body;
use axum::extract::ConnectInfo;
use axum::http::{Request, StatusCode};
use dispatch_core::ingress::handshake::HandshakeStore;
use dispatch_core::state::{HandshakeState, PublicState};
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

fn public_router(store: &Store, token: Option<&str>) -> axum::Router {
    let hs = HandshakeState {
        proxy_client_id: "integration-test".into(),
        tokens: HandshakeStore::new(),
    };
    let st = PublicState::new(store.clone(), 0, token.map(str::to_string), false, hs);
    dispatch_core::ingress::router().with_state(st)
}

fn req(method: &str, uri: &str, body: Body, bearer: Option<&str>) -> Request<Body> {
    let mut r = Request::builder()
        .method(method)
        .uri(uri)
        .header("content-type", "application/json");
    if let Some(b) = bearer {
        r = r.header("authorization", format!("Bearer {b}"));
    }
    let mut r = r.body(body).unwrap();
    let addr: SocketAddr = "127.0.0.1:55555".parse().unwrap();
    r.extensions_mut().insert(ConnectInfo(addr));
    r
}

async fn call(router: &axum::Router, request: Request<Body>) -> (StatusCode, Value) {
    let resp = router.clone().oneshot(request).await.unwrap();
    let status = resp.status();
    let bytes = axum::body::to_bytes(resp.into_body(), usize::MAX)
        .await
        .unwrap();
    let v = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    (status, v)
}

/// 手工联调用：打印一条合法 `/api/webui/login` 密文。
///
/// ```bash
/// CLIENT_ID="11111111-1111-4111-8111-111111111111" \
/// PLAINTEXT='{"username":"plugin","password":"test123"}' \
/// cargo test -p dispatch-core -- --ignored --nocapture print_login_ciphertext
/// ```
#[test]
#[ignore = "手工联调用：打印一条合法 /api/webui/login 密文"]
fn print_login_ciphertext() {
    let cid = std::env::var("CLIENT_ID").expect("需要 CLIENT_ID");
    let pt = std::env::var("PLAINTEXT").expect("需要 PLAINTEXT");
    let ct = bitcomet_api::rncryptor::encrypt(pt.as_bytes(), &cid).expect("加密应成功");
    println!("CIPHERTEXT={ct}");
}

/// 完整三段式握手端到端（token 未配置 ⇒ 任意口令放行）。
#[tokio::test]
async fn full_handshake_then_protected_call() {
    let store = migrated_store();
    let app = public_router(&store, None);
    let client_id = "11111111-1111-4111-8111-111111111111";

    // 1) ip_verify
    let (status, v) = call(
        &app,
        req("POST", "/api/webui/ip_verify", Body::from("{}"), None),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(v["bypass_eligible"], false, "我们从不实现免密码");

    // 2) login（明文里 password 与 token 无关，token 未配置）
    let ct = bitcomet_api::rncryptor::encrypt(
        br#"{"username":"plugin","password":"whatever"}"#,
        client_id,
    )
    .unwrap();
    let login = json!({ "client_id": client_id, "authentication": ct });
    let (status, v) = call(
        &app,
        req(
            "POST",
            "/api/webui/login",
            Body::from(login.to_string()),
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "登录应成功: {v}");
    let invite = v["invite_token"]
        .as_str()
        .expect("应返回 invite_token")
        .to_string();

    // 3) device_token/get
    let dt = json!({ "invite_token": invite, "device_id": client_id, "device_name": "test", "platform": "webui" });
    let (status, v) = call(
        &app,
        req(
            "POST",
            "/api/device_token/get",
            Body::from(dt.to_string()),
            Some(&invite),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "换 device_token 应成功: {v}");
    let device = v["device_token"]
        .as_str()
        .expect("应返回 device_token")
        .to_string();

    // 4) 带 Bearer device_token 打受保护接口
    let body = json!({"url":"http://example.com/a.bin","start_later":false});
    let (status, v) = call(
        &app,
        req(
            "POST",
            "/api/task/http/add",
            Body::from(body.to_string()),
            Some(&device),
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(v["error_code"], "OK");
    assert!(v["proxy_task_id"].is_string());
    store.shutdown().await;
}

/// 「绕门」堵死证明：token 已配置 + 口令不符 → 握手必须失败（拿不到 device_token）。
#[tokio::test]
async fn handshake_cannot_bypass_when_token_configured() {
    let store = migrated_store();
    let app = public_router(&store, Some("secret"));
    let client_id = "22222222-2222-4222-8222-222222222222";
    // 明文里 password 写成 wrong（≠ 配置的 secret）
    let ct =
        bitcomet_api::rncryptor::encrypt(br#"{"username":"plugin","password":"wrong"}"#, client_id)
            .unwrap();
    let login = json!({ "client_id": client_id, "authentication": ct });
    let (status, v) = call(
        &app,
        req(
            "POST",
            "/api/webui/login",
            Body::from(login.to_string()),
            None,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNAUTHORIZED, "口令不符必须 401: {v}");
    assert_eq!(v["error_code"], "INVALID_TOKEN");
    assert!(v.get("invite_token").is_none(), "不得签发 invite_token");
    store.shutdown().await;
}
