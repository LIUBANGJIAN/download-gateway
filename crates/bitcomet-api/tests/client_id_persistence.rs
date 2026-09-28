//! T02 回归：`client_id` 必须**持久化到磁盘**并在多次构造之间保持稳定。
//!
//! 为什么单独立一个集成测试：`client_id` 是节点侧识别「同一台 WebUI 客户端」的唯一凭据。
//! 若每次进程启动都随机重生，节点侧的已绑定设备列表会被 UUID 淹没，且
//! 「登录失败后自愈重生」的语义（[`bitcomet_api::clientid::regenerate_client_id`]）也失去意义。
//!
//! 本测试**不触网**：只验证 `Client::new` 的落盘副作用。
//!
//! 环境变量串行化：`state_dir_env_is_honored` 会临时改写 `DISPATCH_STATE_DIR`，
//! 而同进程内的其它 `#[test]` 默认并行执行、且 `default_profile_persists_*` 也要读同一个变量。
//! 两者共用 [`ENV_LOCK`] 串行化，避免随机串扰（否则会出现「偶发失败」这种最难查的形态）。

use std::path::{Path, PathBuf};
use std::sync::Mutex;

/// 串行化对进程级环境变量 `DISPATCH_STATE_DIR` 的读写。
static ENV_LOCK: Mutex<()> = Mutex::new(());

/// 造一个进程唯一、且已清空的临时目录。
fn fresh_dir(tag: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!(
        "dg-clientid-{}-{}-{}",
        tag,
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0)
    ));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).expect("创建临时目录");
    dir
}

fn profile_with_path(base: &str, path: PathBuf) -> bitcomet_api::NodeProfile {
    let mut p = bitcomet_api::NodeProfile::new(base, "probe-user", "probe-pass");
    p.client_id_path = Some(path);
    p
}

/// 显式指定 `client_id_path` 时：两次构造必须复用同一个 id，且文件确实落盘。
#[test]
fn explicit_path_is_persisted_and_stable() {
    let dir = fresh_dir("explicit");
    let path = dir.join("client_id");

    let c1 = bitcomet_api::Client::new(profile_with_path("http://127.0.0.1:9085", path.clone()), 5)
        .expect("构造 client 1");
    let id1 = c1.client_id().to_string();

    let c2 = bitcomet_api::Client::new(profile_with_path("http://127.0.0.1:9085", path.clone()), 5)
        .expect("构造 client 2");
    let id2 = c2.client_id().to_string();

    assert!(
        path.is_file(),
        "client_id 必须落盘到 {}，实际不存在",
        path.display()
    );
    let on_disk = std::fs::read_to_string(&path).expect("读取落盘文件");
    assert_eq!(on_disk.trim(), id1, "落盘内容应与内存中的 client_id 一致");
    assert_eq!(
        id1, id2,
        "两次构造必须复用同一个 client_id，实际 {id1} != {id2}"
    );

    let _ = std::fs::remove_dir_all(&dir);
}

/// 未显式指定路径、但设了 `DISPATCH_STATE_DIR` 时：必须落到该目录。
///
/// 这是**容器可用性的前提**：镜像以 uid 10001 运行，`/usr/local/bin` 不可写、
/// `%APPDATA%` 在 Linux 下也不存在，只有显式指向的 `/data` 卷才是可持久化位置。
#[test]
fn default_profile_persists_under_state_dir_env() {
    let _guard = ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());

    let dir = fresh_dir("env");
    let dir_str = dir.to_string_lossy().to_string();

    // SAFETY: 持有 ENV_LOCK，且本测试是唯一改写该变量的测试；
    // 读取该变量的 `default_profile_persists_*` 同样持锁。
    unsafe { std::env::set_var(bitcomet_api::clientid::STATE_DIR_ENV, &dir_str) };

    let resolved = bitcomet_api::clientid::config_dir();
    let p = bitcomet_api::NodeProfile::new("http://127.0.0.1:9085", "probe-user", "probe-pass");
    let id = bitcomet_api::Client::new(p, 5)
        .expect("构造 client")
        .client_id()
        .to_string();

    // 立刻还原，缩小污染窗口。
    unsafe { std::env::remove_var(bitcomet_api::clientid::STATE_DIR_ENV) };

    assert_eq!(
        resolved, dir,
        "DISPATCH_STATE_DIR 应被 config_dir() 优先采用"
    );

    let expected = dir.join("client_id");
    assert!(
        expected.is_file(),
        "client_id 应落到 DISPATCH_STATE_DIR 下（{}），实际不存在",
        expected.display()
    );
    assert_eq!(
        std::fs::read_to_string(&expected).unwrap().trim(),
        id,
        "落盘内容应等于内存 client_id"
    );

    let _ = std::fs::remove_dir_all(&dir);
}

/// 复用语义的**跨进程**版本：外部若已给出 `DISPATCH_STATE_DIR`，
/// 则必须复用其中已存在的 id，而**不得**重新生成。
///
/// 该测试由 `.probe/live_test.py` 之外的诊断流程使用：预置一个已知 id 再运行，
/// 打印出来的 `client_id` 必须是那个已知值。
#[test]
fn existing_id_is_reused_not_regenerated() {
    let _guard = ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());

    let dir = match std::env::var(bitcomet_api::clientid::STATE_DIR_ENV) {
        Ok(v) if !v.trim().is_empty() => PathBuf::from(v),
        _ => {
            eprintln!(
                "[跳过] 需要外部设置 {}",
                bitcomet_api::clientid::STATE_DIR_ENV
            );
            return;
        }
    };

    let path: PathBuf = dir.join("client_id");
    let known = "11111111-1111-4111-8111-111111111111";
    std::fs::create_dir_all(&dir).expect("创建状态目录");
    std::fs::write(&path, known).expect("预置 client_id");

    let p = bitcomet_api::NodeProfile::new("http://127.0.0.1:9085", "probe-user", "probe-pass");
    let got = bitcomet_api::Client::new(p, 5)
        .expect("构造 client")
        .client_id()
        .to_string();

    assert_eq!(got, known, "磁盘上已有 client_id 时必须复用，不得重新生成");
    assert_eq!(
        std::fs::read_to_string(&path).unwrap().trim(),
        known,
        "落盘内容不应被改写"
    );
    assert!(Path::new(&path).is_file());
}
