//! 管理台本轮新增端点的集成测试：节点增删改启停 + 密码回看、派发策略读写。
//!
//! 复用 `ingress_smoke.rs` 的脚手架写法：内存库 + 真迁移、装配 `admin_router`、
//! 用 `oneshot` 直打 `Router`（不监听端口）。
//!
//! 节点基址一律使用 **RFC 5737 文档保留地址**（`192.0.2.0/24`、`198.51.100.0/24`、
//! `203.0.113.0/24`）——它们在公网不可路由，探测必然离线；仓库 CI 闸门也禁止在
//! 测试里出现 `192.168.*` / `10.*` / `172.16-31.*` 私网地址。

use std::net::SocketAddr;
use std::sync::Arc;

use axum::Router;
use axum::body::Body;
use axum::extract::ConnectInfo;
use axum::http::{Request, StatusCode, header};
use dispatch_core::admin::policy::CATALOG;
use dispatch_core::state::{AdminPolicy, AdminState, CookieSecureMode, EnvSnapshot};
use dispatch_core::store::{Store, migrate, open_connection};
use serde_json::{Value, json};
use tower::ServiceExt;

/// 测试用管理台口令：`admin_router` 把它写进策略，`login_sid` 用它登录。
const ADMIN_PW: &str = "test-pw-123";

// ---------------------------------------------------------------------------
// 脚手架（照抄 `ingress_smoke.rs` 的写法）
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

fn addr() -> SocketAddr {
    "127.0.0.1:12345".parse().unwrap()
}

fn admin_router(store: &Store) -> Router {
    let policy = Arc::new(AdminPolicy {
        password: ADMIN_PW.to_string(),
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
    let secrets = std::sync::Arc::new(dispatch_core::secret::SecretBox::with_key(
        "admin-nodes-policy-test-key-0123456789",
    ));
    let st = AdminState::new(store.clone(), 0, policy, env, secrets, None);
    dispatch_core::admin::router().with_state(st)
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

/// 带会话 Cookie 的请求。
fn req_cookie(method: &str, uri: &str, body: Body, cookie: &str) -> Request<Body> {
    let mut r = req(method, uri, body);
    r.headers_mut()
        .insert(header::COOKIE, cookie.parse().unwrap());
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
    let body = json!({ "password": ADMIN_PW });
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

fn parse(bytes: &[u8]) -> Value {
    serde_json::from_slice(bytes).expect("响应体应为合法 JSON")
}

/// 新增一个节点，返回 `(node_id, 整个响应体)`。
async fn create_node(app: &Router, cookie: &str, body: Value) -> (i64, Value) {
    let (status, bytes) = call(
        app,
        req_cookie(
            "POST",
            "/api/admin/nodes",
            Body::from(body.to_string()),
            cookie,
        ),
    )
    .await;
    let text = String::from_utf8_lossy(&bytes).to_string();
    assert_eq!(status, StatusCode::OK, "新增节点应成功，响应：{text}");
    let v = parse(&bytes);
    let id = v["data"]["node"]["node_id"]
        .as_i64()
        .expect("响应应含 node_id");
    (id, v)
}

/// 拉节点列表；`probe == false` 时用 `?probe=0` 跳过真实探测（更快、更稳）。
async fn list_nodes(app: &Router, cookie: &str, probe: bool) -> Value {
    let uri = if probe {
        "/api/admin/nodes"
    } else {
        "/api/admin/nodes?probe=0"
    };
    let (status, bytes) = call(app, req_cookie("GET", uri, Body::empty(), cookie)).await;
    assert_eq!(status, StatusCode::OK, "节点列表应成功");
    parse(&bytes)
}

fn find_node(list: &Value, id: i64) -> &Value {
    list["data"]["items"]
        .as_array()
        .expect("items 应为数组")
        .iter()
        .find(|n| n["node_id"].as_i64() == Some(id))
        .expect("列表应包含该节点")
}

fn find_policy<'a>(config: &'a Value, key: &str) -> &'a Value {
    config["data"]["policies"]
        .as_array()
        .expect("policies 应为数组")
        .iter()
        .find(|p| p["key"].as_str() == Some(key))
        .expect("应包含该策略")
}

// ---------------------------------------------------------------------------
// 1-2. 产品原则：管理台永不提供「添加任务」
// ---------------------------------------------------------------------------

/// `POST /api/admin/tasks` 必须已不存在（404 或 405，绝不能 200）。
#[tokio::test]
async fn post_admin_tasks_route_is_gone() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (status, _b) = call(
        &app,
        req_cookie(
            "POST",
            "/api/admin/tasks",
            Body::from(json!({ "uri": "http://example.test/file.iso" }).to_string()),
            &cookie,
        ),
    )
    .await;

    assert!(
        matches!(
            status,
            StatusCode::NOT_FOUND | StatusCode::METHOD_NOT_ALLOWED
        ),
        "管理台不得再提供「添加任务」，实际状态 {status}"
    );
    assert_ne!(status, StatusCode::OK);
}

/// `GET /tasks/new` 必须 404 —— 前端「添加任务」页也必须消失。
#[tokio::test]
async fn get_tasks_new_page_is_gone() {
    let store = migrated_store();
    let app = admin_router(&store);

    let (status, _b) = call(&app, req("GET", "/tasks/new", Body::empty())).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "「添加任务」页必须已删除");
}

// ---------------------------------------------------------------------------
// 3-5. 节点新增：回 node_id、永不吐密文、别名冲突 409
// ---------------------------------------------------------------------------

/// 新增节点：响应 200，能从响应里取到 `node_id`；响应体**不得**出现 `pass_enc`；
/// 列表里 `password_set` 为 true，且绝不泄露密码原值。
#[tokio::test]
async fn node_create_hides_ciphertext_and_reports_password_set() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (status, bytes) = call(
        &app,
        req_cookie(
            "POST",
            "/api/admin/nodes",
            Body::from(
                json!({
                    "alias": "cipher-a",
                    "base_url": "http://192.0.2.10:8080",
                    "password": "node-pw-1"
                })
                .to_string(),
            ),
            &cookie,
        ),
    )
    .await;
    let created_text = String::from_utf8_lossy(&bytes);
    assert_eq!(status, StatusCode::OK, "新增应成功");
    assert!(
        !created_text.contains("pass_enc"),
        "新增响应不得出现密文字段名 pass_enc"
    );
    assert!(
        !created_text.contains("node-pw-1"),
        "新增响应不得回显密码原值"
    );

    let created = parse(&bytes);
    assert_eq!(created["data"]["node"]["password_set"], json!(true));
    let id = created["data"]["node"]["node_id"]
        .as_i64()
        .expect("响应应含 node_id");

    let list = list_nodes(&app, &cookie, false).await;
    let list_text = list.to_string();
    assert!(
        !list_text.contains("pass_enc"),
        "列表响应不得出现密文字段名 pass_enc"
    );
    assert!(!list_text.contains("node-pw-1"), "列表响应不得回显密码原值");
    assert_eq!(find_node(&list, id)["password_set"], json!(true));
}

/// 同一 alias 再 POST 一次 → 409（错误码 40900）。
#[tokio::test]
async fn node_create_duplicate_alias_is_conflict() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let body = || {
        json!({
            "alias": "dup-a",
            "base_url": "http://198.51.100.7:8080",
            "password": "dup-pw-1"
        })
        .to_string()
    };

    let (status, _b) = call(
        &app,
        req_cookie("POST", "/api/admin/nodes", Body::from(body()), &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "首次新增应成功");

    let (status, bytes) = call(
        &app,
        req_cookie("POST", "/api/admin/nodes", Body::from(body()), &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT, "别名重复必须 409");
    assert_eq!(parse(&bytes)["code"], json!(40900));
}

// ---------------------------------------------------------------------------
// 6. 限速换算：写入 kbps，库里存 bytes（×1024，不是 ×1000）
// ---------------------------------------------------------------------------

/// 写 `max_rate_kbps: 512` → 列表回读 `max_rate_kbps == 512` 且 `max_rate_bytes == 524288`。
#[tokio::test]
async fn node_rate_kbps_is_persisted_as_bytes() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (id, _v) = create_node(
        &app,
        &cookie,
        json!({
            "alias": "rate-a",
            "base_url": "http://192.0.2.11:8080",
            "password": "rate-pw-1",
            "max_rate_kbps": 512
        }),
    )
    .await;

    let list = list_nodes(&app, &cookie, false).await;
    let item = find_node(&list, id);
    assert_eq!(item["max_rate_kbps"], json!(512));
    assert_eq!(
        item["max_rate_bytes"],
        json!(524288),
        "512 KB/s 必须存成 512×1024=524288 字节/秒"
    );
}

// ---------------------------------------------------------------------------
// 7-8. 密码回看；编辑留空 = 不覆盖
// ---------------------------------------------------------------------------

/// `POST /api/admin/nodes/{id}/secret` → 200，返回密码与写入一致。
#[tokio::test]
async fn node_secret_can_be_revealed() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (id, _v) = create_node(
        &app,
        &cookie,
        json!({
            "alias": "secret-a",
            "base_url": "http://192.0.2.12:8080",
            "password": "reveal-pw-9"
        }),
    )
    .await;

    let uri = format!("/api/admin/nodes/{id}/secret");
    let (status, bytes) = call(&app, req_cookie("POST", &uri, Body::empty(), &cookie)).await;
    assert_eq!(status, StatusCode::OK, "密码回看应成功");
    let v = parse(&bytes);
    assert_eq!(v["data"]["node_id"], json!(id));
    assert_eq!(v["data"]["password"], json!("reveal-pw-9"));
}

/// 编辑时密码留空 = 不覆盖原密码。两种写法都要测：
/// ① 请求里根本不带 `password` 字段；② 显式传 `"password": ""`。
#[tokio::test]
async fn node_update_blank_password_keeps_old_one() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (id, _v) = create_node(
        &app,
        &cookie,
        json!({
            "alias": "edit-a",
            "base_url": "http://192.0.2.13:8080",
            "password": "keep-pw-1"
        }),
    )
    .await;

    let secret_uri = format!("/api/admin/nodes/{id}/secret");
    let put_uri = format!("/api/admin/nodes/{id}");

    // ① 只改别名，不传 password。
    let (status, bytes) = call(
        &app,
        req_cookie(
            "PUT",
            &put_uri,
            Body::from(json!({ "alias": "edit-renamed" }).to_string()),
            &cookie,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "只改别名应成功");
    assert_eq!(
        parse(&bytes)["data"]["node"]["alias"],
        json!("edit-renamed")
    );

    let (status, bytes) = call(
        &app,
        req_cookie("POST", &secret_uri, Body::empty(), &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(
        parse(&bytes)["data"]["password"],
        json!("keep-pw-1"),
        "不传 password 时不得清掉原密码"
    );

    // ② 显式传空串 password。
    let (status, _b) = call(
        &app,
        req_cookie(
            "PUT",
            &put_uri,
            Body::from(json!({ "alias": "edit-renamed-2", "password": "" }).to_string()),
            &cookie,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "空串密码应被当作「不改」");

    let (status, bytes) = call(
        &app,
        req_cookie("POST", &secret_uri, Body::empty(), &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(
        parse(&bytes)["data"]["password"],
        json!("keep-pw-1"),
        "显式空串不得覆盖原密码"
    );
}

// ---------------------------------------------------------------------------
// 9. 启停独立端点：只切开关，不动其它字段
// ---------------------------------------------------------------------------

/// `POST /api/admin/nodes/{id}/enabled` body `{"enabled":false}` → 200，
/// 回读 `enabled == false`，且 alias / 限速等字段保持不变。
#[tokio::test]
async fn node_enabled_toggle_is_isolated() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (id, _v) = create_node(
        &app,
        &cookie,
        json!({
            "alias": "toggle-a",
            "base_url": "http://192.0.2.14:8080",
            "password": "toggle-pw-1",
            "max_rate_kbps": 256
        }),
    )
    .await;

    let uri = format!("/api/admin/nodes/{id}/enabled");
    let (status, bytes) = call(
        &app,
        req_cookie(
            "POST",
            &uri,
            Body::from(json!({ "enabled": false }).to_string()),
            &cookie,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "启停端点应成功");
    let v = parse(&bytes);
    assert_eq!(v["data"]["node"]["enabled"], json!(false));
    assert_eq!(v["data"]["node"]["alias"], json!("toggle-a"), "不得改别名");
    assert_eq!(
        v["data"]["node"]["max_rate_kbps"],
        json!(256),
        "不得顺带改限速"
    );

    let list = list_nodes(&app, &cookie, false).await;
    let item = find_node(&list, id);
    assert_eq!(item["enabled"], json!(false));
    assert_eq!(item["alias"], json!("toggle-a"));
}

// ---------------------------------------------------------------------------
// 10. 删除
// ---------------------------------------------------------------------------

/// `DELETE /api/admin/nodes/{id}` → 200；随后列表不再包含该节点。
#[tokio::test]
async fn node_delete_removes_from_list() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (id, _v) = create_node(
        &app,
        &cookie,
        json!({
            "alias": "del-a",
            "base_url": "http://192.0.2.15:8080",
            "password": "del-pw-1"
        }),
    )
    .await;

    let uri = format!("/api/admin/nodes/{id}");
    let (status, _b) = call(&app, req_cookie("DELETE", &uri, Body::empty(), &cookie)).await;
    assert_eq!(status, StatusCode::OK, "删除应成功");

    let list = list_nodes(&app, &cookie, false).await;
    let present = list["data"]["items"]
        .as_array()
        .expect("items 应为数组")
        .iter()
        .any(|n| n["node_id"].as_i64() == Some(id));
    assert!(!present, "删除后列表不应再包含该节点");
}

// ---------------------------------------------------------------------------
// 11. 鉴权守卫：不带会话 Cookie 调管理口必须 401
// ---------------------------------------------------------------------------

/// 未认证 `GET /api/admin/nodes` → 401，错误码 40100。
#[tokio::test]
async fn admin_nodes_requires_session() {
    let store = migrated_store();
    let app = admin_router(&store);

    let (status, bytes) = call(&app, req("GET", "/api/admin/nodes", Body::empty())).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED, "未认证必须 401");
    assert_eq!(parse(&bytes)["code"], json!(40100));
}

// ---------------------------------------------------------------------------
// 12-17. 策略读写
// ---------------------------------------------------------------------------

/// `GET /api/admin/config` → 200，`data.policies` 长度 == `CATALOG.len()`，
/// 每条含 `key`/`enabled`/`priority`，且顺序与 `CATALOG` 一致。
#[tokio::test]
async fn config_get_returns_full_catalog() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (status, bytes) = call(
        &app,
        req_cookie("GET", "/api/admin/config", Body::empty(), &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let v = parse(&bytes);

    let policies = v["data"]["policies"].as_array().expect("policies 应为数组");
    assert_eq!(
        policies.len(),
        CATALOG.len(),
        "内置策略条数必须与 CATALOG 一致"
    );
    for p in policies {
        assert!(p.get("key").is_some(), "每条策略应有 key");
        assert!(p.get("enabled").is_some(), "每条策略应有 enabled");
        assert!(p.get("priority").is_some(), "每条策略应有 priority");
    }

    let got: Vec<&str> = policies.iter().filter_map(|p| p["key"].as_str()).collect();
    let want: Vec<&str> = CATALOG.iter().map(|d| d.key).collect();
    assert_eq!(got, want, "策略顺序应恒为 CATALOG 顺序");
}

/// `PUT /api/admin/config` 保存后回读：`least_tasks` 的 `priority == 300`、`enabled == false`。
#[tokio::test]
async fn config_put_then_read_back() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (status, _bytes) = call(
        &app,
        req_cookie(
            "PUT",
            "/api/admin/config",
            Body::from(
                json!({
                    "policies": [
                        { "key": "least_tasks", "enabled": false, "priority": 300 }
                    ]
                })
                .to_string(),
            ),
            &cookie,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "保存应成功");

    let (status, bytes) = call(
        &app,
        req_cookie("GET", "/api/admin/config", Body::empty(), &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let v = parse(&bytes);
    let lt = find_policy(&v, "least_tasks");
    assert_eq!(lt["priority"], json!(300));
    assert_eq!(lt["enabled"], json!(false));
}

/// 越界优先级（`PRIORITY_MAX == 999`）→ 422。
#[tokio::test]
async fn config_put_priority_out_of_range_is_422() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (status, bytes) = call(
        &app,
        req_cookie(
            "PUT",
            "/api/admin/config",
            Body::from(
                json!({
                    "policies": [
                        { "key": "least_tasks", "priority": 99999 }
                    ]
                })
                .to_string(),
            ),
            &cookie,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY, "越界优先级应 422");
    assert_eq!(parse(&bytes)["code"], json!(42200));
}

/// 未知策略键 → 422（不许静默忽略）。
#[tokio::test]
async fn config_put_unknown_key_is_422() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (status, bytes) = call(
        &app,
        req_cookie(
            "PUT",
            "/api/admin/config",
            Body::from(
                json!({
                    "policies": [
                        { "key": "no_such_policy", "enabled": true }
                    ]
                })
                .to_string(),
            ),
            &cookie,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY, "未知键应 422");
    assert_eq!(parse(&bytes)["code"], json!(42200));
}

/// 安全阀 `max_concurrent`（`can_disable == false`）不可关闭 → 422。
#[tokio::test]
async fn config_put_safety_valve_cannot_be_disabled() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (status, bytes) = call(
        &app,
        req_cookie(
            "PUT",
            "/api/admin/config",
            Body::from(
                json!({
                    "policies": [
                        { "key": "max_concurrent", "enabled": false }
                    ]
                })
                .to_string(),
            ),
            &cookie,
        ),
    )
    .await;
    assert_eq!(
        status,
        StatusCode::UNPROCESSABLE_ENTITY,
        "安全阀不可关闭，应 422"
    );
    assert_eq!(parse(&bytes)["code"], json!(42200));
}

/// 保存失败不得留下半写状态：`save()` 先全量校验再落库，
/// 合法项 + 非法项混在一起时，整批拒绝，前一次成功保存的值必须原样保留。
#[tokio::test]
async fn config_save_failure_leaves_previous_state_intact() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    // ① 先成功保存 least_tasks 为 priority 300。
    let (status, _b) = call(
        &app,
        req_cookie(
            "PUT",
            "/api/admin/config",
            Body::from(
                json!({
                    "policies": [
                        { "key": "least_tasks", "priority": 300 }
                    ]
                })
                .to_string(),
            ),
            &cookie,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "首次保存应成功");

    // ② 混入非法项（未知键），且合法项想把 priority 改成 500。
    let (status, bytes) = call(
        &app,
        req_cookie(
            "PUT",
            "/api/admin/config",
            Body::from(
                json!({
                    "policies": [
                        { "key": "least_tasks", "priority": 500 },
                        { "key": "no_such_policy", "enabled": true }
                    ]
                })
                .to_string(),
            ),
            &cookie,
        ),
    )
    .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY, "整批应被拒绝");
    assert_eq!(parse(&bytes)["code"], json!(42200));

    // ③ 回读：必须仍是上一次成功保存的 300，绝不能是 500。
    let (status, bytes) = call(
        &app,
        req_cookie("GET", "/api/admin/config", Body::empty(), &cookie),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    let v = parse(&bytes);
    let lt = find_policy(&v, "least_tasks");
    assert_eq!(
        lt["priority"],
        json!(300),
        "保存失败不得留下半写状态（期望仍为 300）"
    );
}

// ---------------------------------------------------------------------------
// 探测：RFC 5737 地址不可达 → online == false
// ---------------------------------------------------------------------------

/// 对不可达的文档保留地址做真实探测：`online == false`（不断言具体错误串）。
#[tokio::test]
async fn node_probe_marks_unreachable_address_offline() {
    let store = migrated_store();
    let app = admin_router(&store);
    let cookie = login_sid(&app).await;

    let (id, _v) = create_node(
        &app,
        &cookie,
        json!({
            "alias": "probe-a",
            "base_url": "http://203.0.113.9:8080",
            "password": "probe-pw-1"
        }),
    )
    .await;

    // probe 默认开启（不加 ?probe=0），会对节点发起一次 1.5 秒超时的探测。
    let list = list_nodes(&app, &cookie, true).await;
    let item = find_node(&list, id);
    assert_eq!(
        item["online"],
        json!(false),
        "文档保留地址不可达，探测应判定离线"
    );
}
