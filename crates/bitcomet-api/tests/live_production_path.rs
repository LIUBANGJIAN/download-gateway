//! T02 验收补强：用**生产构造路径**（`Client::new` + 持久化 `client_id`）对真实节点做三段式认证。
//!
//! # 为什么要单独立一个文件
//!
//! `client.rs` 内已有 `live_login_and_fetch_task_list`，但它走的是**一次性内存 client_id**：
//! 实测（三轮独立实验）
//!   ① 预先在 `DISPATCH_STATE_DIR` 放一个 `client_id` 并**加锁**让它删不掉 → 测试仍打印新 UUID；
//!   ② 全盘搜索该 UUID → 不存在；
//!   ③ 5ms 轮询 `.probe/live-state`、`%TEMP%`（3 层）、仓库（2 层）→ 全运行期 **0 次命中**。
//! 结论：那个测试只证明了「认证链路本身可用」，**没有**覆盖「生产路径下的持久化 client_id」。
//!
//! 而这一条恰恰是运维关键：`client_id` 稳定 ⇒ 节点侧把它认作**同一台客户端**，
//! 401 时清 token 重登不会在节点设备列表里堆出一串孤儿设备。
//!
//! 本测试因此断言两件事：
//!   1. `Client::new` 在测试开始前就把 `client_id` **落盘**；
//!   2. 落盘的 id 与客户端实际用于登录的 id **一致**，且三次认证全部成功。
//!
//! 需要凭据，故 `#[ignore]`。运行方式见 `.probe/live_test.py`。

/// 读取三件套环境变量；缺失则打印跳过并返回 `None`（**不失败**，便于本地/CI 默认跳过）。
fn test_node() -> Option<(String, String, String)> {
    match (
        std::env::var("DISPATCH_TEST_NODE_URL"),
        std::env::var("DISPATCH_TEST_NODE_USER"),
        std::env::var("DISPATCH_TEST_NODE_PASS"),
    ) {
        (Ok(u), Ok(n), Ok(p)) if !u.trim().is_empty() => Some((u, n, p)),
        _ => {
            eprintln!(
                "[跳过] 缺少 DISPATCH_TEST_NODE_URL / DISPATCH_TEST_NODE_USER / DISPATCH_TEST_NODE_PASS"
            );
            None
        }
    }
}

/// 生产路径联调：`Client::new` → `login()` → `fetch_task_list()`，并校验 client_id 落盘与复用。
#[tokio::test]
#[ignore = "需要真实 BitComet 节点与凭据（DISPATCH_TEST_NODE_*）"]
async fn live_production_path_persists_client_id_and_logs_in() {
    let Some((url, user, pass)) = test_node() else {
        return;
    };

    let state_dir = std::env::var("DISPATCH_STATE_DIR").unwrap_or_else(|_| {
        let fallback = std::env::temp_dir().join("dg-live-production");
        fallback.to_string_lossy().to_string()
    });
    let cid_path = std::path::Path::new(&state_dir).join("client_id");

    // 先清干净，确保这次落盘确实是「本次构造」造成的。
    let _ = std::fs::remove_file(&cid_path);
    assert!(
        !cid_path.exists(),
        "起始状态应为无 client_id：{}",
        cid_path.display()
    );

    let profile = bitcomet_api::NodeProfile::new(url, user, pass);
    let mut client = bitcomet_api::Client::new(profile, 20).expect("构造 Client 失败");
    let id = client.client_id().to_string();

    // 断言 1：构造即落盘（不触网）。
    assert!(
        cid_path.is_file(),
        "Client::new 必须把 client_id 落盘到 {}，实际不存在",
        cid_path.display()
    );
    let on_disk = std::fs::read_to_string(&cid_path).expect("读取落盘 client_id");
    assert_eq!(
        on_disk.trim(),
        id,
        "落盘内容与实际使用的 client_id 不一致（说明登录用的不是持久化 id）"
    );
    eprintln!("[1/4] 落盘 client_id 已确认（长度 {}，值已隐藏）", id.len());

    // 断言 2：三段式认证。
    client.login().await.expect("三段式登录失败");
    assert!(client.has_token(), "登录后必须持有 device_token");
    eprintln!("[2/4] 三段式登录成功，已取得 device_token");

    // 断言 3：任务列表。
    let list = client.fetch_task_list().await.expect("拉取任务列表失败");
    let top: Vec<&String> = list
        .as_object()
        .expect("任务列表顶层应为 JSON 对象")
        .keys()
        .collect();
    eprintln!("[3/4] /api_v2/task_list/get 顶层键 = {top:?}");

    // 断言 4：复用——再构造一次必须拿到同一个 id（跨进程语义的进程内版本）。
    let again = bitcomet_api::Client::new(
        bitcomet_api::NodeProfile::new(
            std::env::var("DISPATCH_TEST_NODE_URL").unwrap_or_default(),
            std::env::var("DISPATCH_TEST_NODE_USER").unwrap_or_default(),
            std::env::var("DISPATCH_TEST_NODE_PASS").unwrap_or_default(),
        ),
        20,
    )
    .expect("二次构造 Client 失败");
    assert_eq!(
        again.client_id(),
        id,
        "二次构造必须复用同一个持久化 client_id"
    );
    eprintln!("[4/4] 二次构造复用了同一个 client_id");

    // 顺带探一次 about（T03 健康探针要用）。
    eprintln!("（附）about = {}", client.about().await.is_ok());
}
