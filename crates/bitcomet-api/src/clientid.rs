//! client_id 的生成与持久化。
//!
//! 为什么必须持久化：client_id 既是设备标识，又是登录密文（RNCryptor）的口令。
//! 实测表明：client_id 为空会导致登录**静默失败**；每次运行重新生成也会让服务端
//! 视为新设备、无法稳定绑定。因此首次运行生成 UUID v4 写入本地文件，之后复用。
//!
//! # 路径选择（T02 相对参考实现的改造）
//!
//! 参考实现只认 Windows 的 `%APPDATA%`，兜底是**可执行文件所在目录**。
//! 这在容器里会出问题：运行镜像以非 root（uid 10001）运行，`/usr/local/bin` **不可写**，
//! 于是 client_id 每次重启都会重新生成，节点侧会不断冒出新的设备记录。
//! 本版按以下顺序解析：
//!
//! 1. `DISPATCH_STATE_DIR` —— 容器里显式指向 `/data`（与 SQLite 同卷，一起持久化）
//! 2. `XDG_CONFIG_HOME` —— Linux / macOS 惯例
//! 3. `APPDATA` —— Windows 惯例（与参考实现行为一致）
//! 4. `HOME` / `USERPROFILE` —— 兜底
//! 5. 可执行文件目录 —— 最后手段

use std::fs;
use std::path::{Path, PathBuf};

/// 应用状态目录名。
pub const APP_DIR_NAME: &str = "download-gateway";

/// 显式指定状态目录的环境变量（容器里设为 `/data`）。
pub const STATE_DIR_ENV: &str = "DISPATCH_STATE_DIR";

/// 返回 client_id 的存放目录。
pub fn config_dir() -> PathBuf {
    if let Ok(v) = std::env::var(STATE_DIR_ENV) {
        let t = v.trim();
        if !t.is_empty() {
            return PathBuf::from(t);
        }
    }
    if let Ok(v) = std::env::var("XDG_CONFIG_HOME") {
        let t = v.trim();
        if !t.is_empty() {
            return PathBuf::from(t).join(APP_DIR_NAME);
        }
    }
    if let Ok(v) = std::env::var("APPDATA") {
        let t = v.trim();
        if !t.is_empty() {
            return PathBuf::from(t).join(APP_DIR_NAME);
        }
    }
    for key in ["HOME", "USERPROFILE"] {
        if let Ok(v) = std::env::var(key) {
            let t = v.trim();
            if !t.is_empty() {
                return PathBuf::from(t).join(".config").join(APP_DIR_NAME);
            }
        }
    }
    if let Ok(exe) = std::env::current_exe()
        && let Some(dir) = exe.parent()
    {
        return dir.to_path_buf();
    }
    PathBuf::from(".")
}

/// 默认的 client_id 持久化路径。
pub fn default_client_id_path() -> PathBuf {
    config_dir().join("client_id")
}

/// 生成一个 UUID v4 字符串。
pub fn new_uuid() -> String {
    uuid::Uuid::new_v4().to_string()
}

/// 读取指定路径的 client_id；不存在或为空时生成并落盘。返回值保证非空。
pub fn load_or_create_client_id(path: &Path) -> std::io::Result<String> {
    if let Ok(data) = fs::read_to_string(path) {
        let trimmed = data.trim();
        if !trimmed.is_empty() {
            return Ok(trimmed.to_string());
        }
    }

    let id = new_uuid();
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    fs::write(path, &id)?;
    Ok(id)
}

/// 删除并重建 client_id，用于"文件内容损坏被服务端拒绝"时的自愈。
pub fn regenerate_client_id(path: &Path) -> std::io::Result<String> {
    let _ = fs::remove_file(path);
    load_or_create_client_id(path)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn uuid_has_v4_shape() {
        let id = new_uuid();
        let parts: Vec<&str> = id.split('-').collect();
        assert_eq!(parts.len(), 5, "UUID 应有 5 段: {id}");
        assert_eq!(parts[0].len(), 8);
        assert_eq!(parts[1].len(), 4);
        assert_eq!(parts[2].len(), 4);
        assert_eq!(parts[3].len(), 4);
        assert_eq!(parts[4].len(), 12);
        assert!(parts[2].starts_with('4'), "版本位应为 4: {id}");
        assert!(
            matches!(parts[3].chars().next(), Some('8' | '9' | 'a' | 'b')),
            "variant 位应符合 RFC4122: {id}"
        );
    }

    #[test]
    fn two_uuids_differ() {
        assert_ne!(new_uuid(), new_uuid());
    }

    #[test]
    fn creates_then_reuses_client_id() {
        let dir = std::env::temp_dir().join(format!("dg-test-{}", new_uuid()));
        let path = dir.join("client_id");

        let first = load_or_create_client_id(&path).expect("首次应生成");
        assert!(!first.trim().is_empty());
        assert!(path.exists(), "应已落盘");

        let second = load_or_create_client_id(&path).expect("再次应复用");
        assert_eq!(first, second, "两次读取必须得到同一个 client_id");

        let regen = regenerate_client_id(&path).expect("应能重建");
        assert_ne!(regen, first, "重建后应是全新的 id");

        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn empty_file_is_treated_as_missing() {
        let dir = std::env::temp_dir().join(format!("dg-test-empty-{}", new_uuid()));
        fs::create_dir_all(&dir).unwrap();
        let path = dir.join("client_id");
        fs::write(&path, "   \n").unwrap();

        let id = load_or_create_client_id(&path).expect("空文件应触发生成");
        assert!(!id.trim().is_empty());

        let _ = fs::remove_dir_all(&dir);
    }

    /// 容器场景回归：`DISPATCH_STATE_DIR` 必须优先于其它来源，
    /// 否则会落到不可写的可执行文件目录（uid 10001 + `/usr/local/bin`）。
    #[test]
    fn state_dir_env_takes_priority() {
        let marker = "/tmp/dg-state-dir-probe";
        unsafe { std::env::set_var(STATE_DIR_ENV, marker) };
        let got = config_dir();
        unsafe { std::env::remove_var(STATE_DIR_ENV) };
        assert_eq!(got, PathBuf::from(marker), "应优先采用 DISPATCH_STATE_DIR");
    }
}
