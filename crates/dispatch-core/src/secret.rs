//! 节点密码的**可逆**封装（供管理台「眼睛」按钮回看原文）。
//!
//! # 为什么复用 `bitcomet_api::rncryptor`，而不是引入新的加密 crate
//!
//! `node.pass_enc` 的需求是「**口令派生的对称可逆封装**」。而
//! `bitcomet-api::rncryptor` 恰好提供了 `encrypt` / `decrypt` 这一对：
//! RNCryptor v3 = PBKDF2-HMAC-SHA1(10000 轮) 派生密钥 → AES-256-CBC → HMAC-SHA256。
//! 更关键的是，它的**逐字节正确性**已由跨语言互操作测试
//! （`decrypts_go_generated_ciphertext`，用 Go 实现产出的密文在 Rust 侧解回来）钉住。
//!
//! 为一个已经在依赖树里、且已被验证过的实现，去另找 `aes-gcm` / `chacha20poly1305`，
//! 属于「用更高风险换更低收益」——违反了本项目「能不新增依赖就不新增」的既定纪律。
//!
//! # 主密钥从哪来
//!
//! 按优先级取：
//!
//! 1. 环境变量 `DISPATCH_SECRET_KEY`（非空白即用；容器部署推荐显式注入）；
//! 2. `<库文件所在目录>/node-secret.key` 文件 —— 不存在则**生成 32 字节随机并落盘**。
//!
//! 第 2 条是「开箱即用」的保证：不配任何环境变量，`docker compose up` 之后
//! 「眼睛」也能用，且**重启后仍能解密**。这一点很重要 —— 如果每次启动都换密钥，
//! 历史密文会全部变成废纸，用户会看到「密码读不出来了」这种安静型故障。
//!
//! ⚠️ **诚实边界**：默认方案里，主密钥与密文同处一台机器（密钥文件就在数据目录旁边）。
//! 它防的是**窥屏 / 截图 / 日志泄露**，**不防**已经拿到整机文件系统的人。
//! 要防后者，请用 `DISPATCH_SECRET_KEY` 把密钥挪到容器外（如 Docker secret / 宿主机环境）。

use std::path::{Path, PathBuf};

/// 主密钥的环境变量名。
pub const KEY_ENV: &str = "DISPATCH_SECRET_KEY";
/// 落盘密钥的文件名（与库文件同目录）。
pub const KEY_FILE_NAME: &str = "node-secret.key";
/// 生成密钥的字节数（十六进制后 64 字符）。
const KEY_BYTES: usize = 32;
/// 主密钥的最小长度（环境变量注入时校验）。
const KEY_MIN_LEN: usize = 16;

/// 加解密过程中的错误。
#[derive(Debug, thiserror::Error)]
pub enum SecretError {
    /// 读写密钥文件失败。
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    /// 加密失败。
    #[error("加密失败：{0}")]
    Encrypt(String),
    /// 解密失败（密文损坏、被截断、或主密钥已变）。
    #[error("解密失败：{0}")]
    Decrypt(String),
    /// 解密成功但结果不是合法 UTF-8。
    #[error("解密结果不是合法 UTF-8")]
    NotUtf8,
}

/// 节点密码的加解密器。
///
/// 内部只持有一个字符串主密钥；`Clone` 廉价。
#[derive(Clone)]
pub struct SecretBox {
    key: String,
}

impl SecretBox {
    /// 用**显式密钥**构造（测试与 `DISPATCH_SECRET_KEY` 路径共用）。
    pub fn with_key(key: impl Into<String>) -> Self {
        Self { key: key.into() }
    }

    /// 解析主密钥：环境变量优先，其次密钥文件，最后生成。
    ///
    /// 返回 `(加解密器, generated)`；`generated == true` 表示**本次新生成**了密钥文件，
    /// 调用方应打印一次提示（用户至少该知道机器上多了一个文件）。
    pub fn resolve(db_path: &Path) -> Result<(Self, bool), SecretError> {
        // ① 环境变量优先：非空白即用。
        if let Ok(v) = std::env::var(KEY_ENV) {
            let t = v.trim();
            if !t.is_empty() {
                if t.len() < KEY_MIN_LEN {
                    return Err(SecretError::Encrypt(format!(
                        "{KEY_ENV} 过短（{} 字符）：至少 {KEY_MIN_LEN} 字符，否则派生强度不足",
                        t.len()
                    )));
                }
                return Ok((Self::with_key(t), false));
            }
        }

        // ② 密钥文件。
        let file = key_file_path(db_path);
        if let Ok(s) = std::fs::read_to_string(&file) {
            let t = s.trim();
            if !t.is_empty() {
                return Ok((Self::with_key(t), false));
            }
        }

        // ③ 生成并落盘。
        let key = hex_encode(&random_key_bytes());
        if let Some(parent) = file.parent()
            && !parent.as_os_str().is_empty()
        {
            std::fs::create_dir_all(parent)?;
        }
        std::fs::write(&file, format!("{key}\n"))?;
        restrict_permissions(&file);
        Ok((Self::with_key(key), true))
    }

    /// 加密明文，返回 Base64 密文（可直接存进 `node.pass_enc`）。
    pub fn wrap(&self, plaintext: &str) -> Result<String, SecretError> {
        bitcomet_api::rncryptor::encrypt(plaintext.as_bytes(), &self.key)
            .map_err(|e| SecretError::Encrypt(e.to_string()))
    }

    /// 解密密文（`node.pass_enc`）回明文。
    pub fn open(&self, token_b64: &str) -> Result<String, SecretError> {
        let bytes = bitcomet_api::rncryptor::decrypt(token_b64, &self.key)
            .map_err(|e| SecretError::Decrypt(e.to_string()))?;
        String::from_utf8(bytes).map_err(|_| SecretError::NotUtf8)
    }
}

/// 密钥文件路径：与库文件**同目录**，便于 `VOLUME /data` 一起持久化。
pub fn key_file_path(db_path: &Path) -> PathBuf {
    match db_path.parent() {
        Some(p) if !p.as_os_str().is_empty() => p.join(KEY_FILE_NAME),
        _ => PathBuf::from(KEY_FILE_NAME),
    }
}

fn random_key_bytes() -> [u8; KEY_BYTES] {
    let mut buf = [0u8; KEY_BYTES];
    if getrandom::fill(&mut buf).is_err() {
        // 与 `ids.rs` 的策略一致：不 panic，但要吵。
        tracing::error!(
            "生成节点密钥时获取安全随机数失败：已回落全零，请立即用 {KEY_ENV} 显式注入密钥"
        );
    }
    buf
}

/// 小写十六进制编码。
fn hex_encode(bytes: &[u8]) -> String {
    let mut out = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        out.push_str(&format!("{b:02x}"));
    }
    out
}

/// 尽量收紧密钥文件权限（Unix 0600）。Windows 上无对应语义，直接跳过。
fn restrict_permissions(path: &Path) {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if let Err(e) = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)) {
            tracing::warn!("无法把密钥文件权限收紧到 0600：{e}");
        }
    }
    #[cfg(not(unix))]
    {
        let _ = path;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// **核心契约**：wrap → open 必须逐字节回原文（含中文、符号、空串）。
    #[test]
    fn wrap_then_open_roundtrips() {
        let sb = SecretBox::with_key("a-test-master-key-0123456789");
        for plain in ["", "p@ssw0rd", "中文口令·带符号!#$%", "a"] {
            let enc = sb.wrap(plain).expect("加密应成功");
            assert_ne!(enc, plain, "密文不应等于明文");
            let back = sb.open(&enc).expect("解密应成功");
            assert_eq!(back, plain, "roundtrip 必须逐字节一致");
        }
    }

    /// 密文必须**每次不同**（RNCryptor v3 每次随机 salt/IV）。
    /// 若两条相同，说明随机源坏了 —— 那会让「密文相同 ⇒ 口令相同」可被离线比对。
    #[test]
    fn ciphertext_is_randomized() {
        let sb = SecretBox::with_key("a-test-master-key-0123456789");
        let a = sb.wrap("same").unwrap();
        let b = sb.wrap("same").unwrap();
        assert_ne!(a, b, "同一明文两次加密结果必须不同（salt/IV 随机）");
        assert_eq!(sb.open(&a).unwrap(), sb.open(&b).unwrap());
    }

    /// 换错密钥必须**解密失败**，而不是返回垃圾。
    #[test]
    fn wrong_key_fails_to_open() {
        let a = SecretBox::with_key("key-aaaaaaaaaaaaaaaa");
        let b = SecretBox::with_key("key-bbbbbbbbbbbbbbbb");
        let enc = a.wrap("secret").unwrap();
        assert!(b.open(&enc).is_err(), "错误主密钥必须解密失败");
    }

    /// 密文被篡改必须被发现（HMAC 的作用）。
    #[test]
    fn tampered_ciphertext_is_rejected() {
        let sb = SecretBox::with_key("a-test-master-key-0123456789");
        let mut enc = sb.wrap("secret").unwrap();
        // 改动 Base64 中间一个字符，破坏 HMAC 覆盖的载荷。
        let mid = enc.len() / 2;
        let ch = enc.as_bytes()[mid];
        let repl = if ch == b'A' { 'B' } else { 'A' };
        enc.replace_range(mid..mid + 1, &repl.to_string());
        assert!(sb.open(&enc).is_err(), "篡改后的密文必须被拒绝");
    }

    /// 密钥文件与库文件同目录。
    #[test]
    fn key_file_sits_next_to_db() {
        assert_eq!(
            key_file_path(Path::new("/data/dispatch.db")),
            PathBuf::from("/data/node-secret.key")
        );
        // 无父目录时退化为当前目录，不应 panic。
        assert_eq!(
            key_file_path(Path::new("dispatch.db")),
            PathBuf::from("node-secret.key")
        );
    }

    /// `resolve` 落盘的密钥必须**再次 `resolve` 时复用** ——
    /// 这是「重启后眼睛还能用」的根本保证。
    #[test]
    fn resolve_is_stable_across_calls() {
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("dispatch.db");
        // 确保环境变量不干扰（CI 不注入，但本地可能设过）。
        if std::env::var(KEY_ENV).map(|v| !v.trim().is_empty()) == Ok(true) {
            return;
        }
        let (a, gen1) = SecretBox::resolve(&db).unwrap();
        let (b, gen2) = SecretBox::resolve(&db).unwrap();
        assert!(gen1, "首次应生成密钥文件");
        assert!(!gen2, "第二次应复用既有密钥文件");
        let enc = a.wrap("p").unwrap();
        assert_eq!(
            b.open(&enc).unwrap(),
            "p",
            "两次 resolve 必须得到同一主密钥"
        );
    }
}
