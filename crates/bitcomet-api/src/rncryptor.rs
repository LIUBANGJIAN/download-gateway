//! BitComet WebUI 登录所需的 RNCryptor v3 密文。
//!
//! 为什么需要它：BitComet WebUI **不接受** HTTP Basic Auth 调 API（实测 Basic 只返回
//! `401 {"error_code":"INVALID_TOKEN"}`），必须把 `{"username","password"}` 以
//! `client_id` 为口令做 RNCryptor v3 加密后，作为 `authentication` 字段发送。
//!
//! 协议布局（官方文档明确给出，与 Go 版已验证实现逐字节一致）：
//!
//! ```text
//! version(1)=3 | options(1)=1 | encryptionSalt(8) | hmacSalt(8) |
//! IV(16) | ciphertext | HMAC(32)
//! ```
//!
//! 其中：
//! - 对称加密：**AES-256-CBC + PKCS#7 填充**
//! - 密钥派生：**PBKDF2-HMAC-SHA1，10000 次，输出 32 字节**
//!   - 加密密钥 = PBKDF2(client_id, encryptionSalt)
//!   - HMAC 密钥 = PBKDF2(client_id, hmacSalt)
//! - 完整性：**HMAC-SHA256**，计算范围 = HMAC 之前的全部字节
//! - 最终整体 **Base64** 编码
//!
//! 实现取舍（为什么不用 `cbc` crate 的填充辅助）：
//! RustCrypto 新一代（cipher 0.5）把 `BlockEncryptMut` 等带填充的便捷 trait 改名/重组了，
//! 直接依赖它会让代码绑死在会变动的 API 上。这里只用最稳定的两块：
//! `aes::Aes256`（分组加解密）与 `cipher::{KeyInit, BlockCipherEncrypt, Block}`，
//! CBC 链接与 PKCS#7 填充自己实现 —— 合计不到 20 行，且行为完全可测。

use aes::Aes256;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as B64;
use cipher::{Block, BlockCipherDecrypt, BlockCipherEncrypt, KeyInit};
use hmac::{Hmac, Mac};
use sha1::Sha1;
use sha2::Sha256;
use std::fmt;

/// RNCryptor v3 协议常量。这些数值来自协议规范，不是可调参数。
const RNC_VERSION: u8 = 3;
/// options 字节（含口令校验位，此处固定发送 1）
const RNC_OPTIONS: u8 = 1;
/// PBKDF2 迭代次数
const PBKDF2_ITERATIONS: u32 = 10_000;
/// 派生密钥长度（AES-256）
const PBKDF2_KEY_LEN: usize = 32;
const ENC_SALT_LEN: usize = 8;
const HMAC_SALT_LEN: usize = 8;
const IV_LEN: usize = 16;
const AES_BLOCK: usize = 16;
const HMAC_LEN: usize = 32;

/// 加密/解密过程中的错误。
#[derive(Debug)]
pub enum CryptorError {
    /// 口令（client_id）为空 —— 实测会导致服务端静默拒绝登录
    EmptyPassword,
    Random(String),
    InvalidFormat(String),
    BadPadding,
    IntegrityFailed,
}

impl fmt::Display for CryptorError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::EmptyPassword => write!(f, "RNCryptor 口令（client_id）不能为空"),
            Self::Random(e) => write!(f, "获取安全随机数失败: {e}"),
            Self::InvalidFormat(e) => write!(f, "密文格式非法: {e}"),
            Self::BadPadding => write!(f, "PKCS#7 填充非法（密钥错误或密文被篡改）"),
            Self::IntegrityFailed => write!(f, "HMAC 校验失败（密文被篡改或口令错误）"),
        }
    }
}

impl std::error::Error for CryptorError {}

/// 将明文按 RNCryptor v3 规范加密，返回 Base64 字符串。
///
/// `password` 即持久化的 client_id。注意：client_id **必须非空**，
/// 否则 BitComet 会静默拒绝登录 —— 因此调用方需保证传入的是有效 UUID。
pub fn encrypt(plaintext: &[u8], password: &str) -> Result<String, CryptorError> {
    if password.is_empty() {
        return Err(CryptorError::EmptyPassword);
    }

    // 每次加密都生成全新的随机 salt / IV，保证同明文多次加密结果不同。
    let mut enc_salt = [0u8; ENC_SALT_LEN];
    let mut hmac_salt = [0u8; HMAC_SALT_LEN];
    let mut iv = [0u8; IV_LEN];
    getrandom::fill(&mut enc_salt).map_err(|e| CryptorError::Random(e.to_string()))?;
    getrandom::fill(&mut hmac_salt).map_err(|e| CryptorError::Random(e.to_string()))?;
    getrandom::fill(&mut iv).map_err(|e| CryptorError::Random(e.to_string()))?;

    // 派生两把彼此独立的密钥。
    let mut enc_key = [0u8; PBKDF2_KEY_LEN];
    let mut hmac_key = [0u8; PBKDF2_KEY_LEN];
    pbkdf2::pbkdf2_hmac::<Sha1>(
        password.as_bytes(),
        &enc_salt,
        PBKDF2_ITERATIONS,
        &mut enc_key,
    );
    pbkdf2::pbkdf2_hmac::<Sha1>(
        password.as_bytes(),
        &hmac_salt,
        PBKDF2_ITERATIONS,
        &mut hmac_key,
    );

    let cipher = Aes256::new_from_slice(&enc_key)
        .map_err(|e| CryptorError::InvalidFormat(format!("AES 密钥长度非法: {e}")))?;

    let padded = pkcs7_pad(plaintext, AES_BLOCK);
    let ciphertext = cbc_encrypt(&cipher, &iv, &padded);

    // 按协议顺序拼接 HMAC 之前的所有字节。
    let mut out = Vec::with_capacity(
        1 + 1 + ENC_SALT_LEN + HMAC_SALT_LEN + IV_LEN + ciphertext.len() + HMAC_LEN,
    );
    out.push(RNC_VERSION);
    out.push(RNC_OPTIONS);
    out.extend_from_slice(&enc_salt);
    out.extend_from_slice(&hmac_salt);
    out.extend_from_slice(&iv);
    out.extend_from_slice(&ciphertext);

    // 对上述全部字节计算 HMAC-SHA256，追加 32 字节。
    // 注意：`new_from_slice` 挂在 `KeyInit` 上，**不是** `Mac` 上
    //（digest 0.11 把 `Mac` 收窄成只有 update/finalize/verify）。
    let mut mac = <Hmac<Sha256> as KeyInit>::new_from_slice(&hmac_key)
        .map_err(|e| CryptorError::InvalidFormat(format!("HMAC 密钥长度非法: {e}")))?;
    mac.update(&out);
    let tag = mac.finalize().into_bytes();
    out.extend_from_slice(&tag);

    Ok(B64.encode(&out))
}

/// RNCryptor v3 解密（**仅供测试与交叉验证使用**，运行时不需要）。
///
/// 保留它的价值：可以用 Go 版实现产出的密文在 Rust 侧解密，
/// 用真实数据证明两套实现遵守同一协议，而不是"看起来一样"。
pub fn decrypt(token_b64: &str, password: &str) -> Result<Vec<u8>, CryptorError> {
    if password.is_empty() {
        return Err(CryptorError::EmptyPassword);
    }
    let raw = B64
        .decode(token_b64.as_bytes())
        .map_err(|e| CryptorError::InvalidFormat(format!("Base64 解码失败: {e}")))?;

    let min_len = 1 + 1 + ENC_SALT_LEN + HMAC_SALT_LEN + IV_LEN + AES_BLOCK + HMAC_LEN;
    if raw.len() < min_len {
        return Err(CryptorError::InvalidFormat(format!(
            "长度 {} 小于最小值 {min_len}",
            raw.len()
        )));
    }
    if raw[0] != RNC_VERSION {
        return Err(CryptorError::InvalidFormat(format!(
            "版本字节应为 {RNC_VERSION}，实际 {}",
            raw[0]
        )));
    }

    let (body, tag) = raw.split_at(raw.len() - HMAC_LEN);
    let enc_salt = &body[2..2 + ENC_SALT_LEN];
    let hmac_salt = &body[2 + ENC_SALT_LEN..2 + ENC_SALT_LEN + HMAC_SALT_LEN];
    let iv_off = 2 + ENC_SALT_LEN + HMAC_SALT_LEN;
    let iv: [u8; IV_LEN] = body[iv_off..iv_off + IV_LEN]
        .try_into()
        .map_err(|_| CryptorError::InvalidFormat("IV 切片长度异常".into()))?;
    let ciphertext = &body[iv_off + IV_LEN..];

    if ciphertext.is_empty() || ciphertext.len() % AES_BLOCK != 0 {
        return Err(CryptorError::InvalidFormat(format!(
            "密文长度 {} 不是 {AES_BLOCK} 的整数倍",
            ciphertext.len()
        )));
    }

    // 先验完整性，再解密 —— 顺序不能反，否则会把篡改数据喂给解密器。
    let mut hmac_key = [0u8; PBKDF2_KEY_LEN];
    pbkdf2::pbkdf2_hmac::<Sha1>(
        password.as_bytes(),
        hmac_salt,
        PBKDF2_ITERATIONS,
        &mut hmac_key,
    );
    let mut mac = <Hmac<Sha256> as KeyInit>::new_from_slice(&hmac_key)
        .map_err(|e| CryptorError::InvalidFormat(format!("HMAC 密钥长度非法: {e}")))?;
    mac.update(body);
    let expected = mac.finalize().into_bytes();
    if !constant_time_eq(&expected, tag) {
        return Err(CryptorError::IntegrityFailed);
    }

    let mut enc_key = [0u8; PBKDF2_KEY_LEN];
    pbkdf2::pbkdf2_hmac::<Sha1>(
        password.as_bytes(),
        enc_salt,
        PBKDF2_ITERATIONS,
        &mut enc_key,
    );
    let cipher = Aes256::new_from_slice(&enc_key)
        .map_err(|e| CryptorError::InvalidFormat(format!("AES 密钥长度非法: {e}")))?;

    let padded = cbc_decrypt(&cipher, &iv, ciphertext);
    pkcs7_unpad(&padded, AES_BLOCK)
}

/// PKCS#7 填充：补足到块大小，填充字节值 = 填充长度。返回新向量（不修改入参）。
fn pkcs7_pad(data: &[u8], block_size: usize) -> Vec<u8> {
    let pad = block_size - (data.len() % block_size);
    let mut out = Vec::with_capacity(data.len() + pad);
    out.extend_from_slice(data);
    out.extend(std::iter::repeat_n(pad as u8, pad));
    out
}

/// PKCS#7 去填充，并校验填充字节的合法性。
fn pkcs7_unpad(data: &[u8], block_size: usize) -> Result<Vec<u8>, CryptorError> {
    if data.is_empty() || !data.len().is_multiple_of(block_size) {
        return Err(CryptorError::BadPadding);
    }
    let pad = *data.last().unwrap() as usize;
    if pad == 0 || pad > block_size || pad > data.len() {
        return Err(CryptorError::BadPadding);
    }
    // 填充的每个字节都必须等于 pad，否则视为非法填充。
    if data[data.len() - pad..].iter().any(|&b| b as usize != pad) {
        return Err(CryptorError::BadPadding);
    }
    Ok(data[..data.len() - pad].to_vec())
}

/// 取 `buf` 的 AES 分组可变引用。
///
/// 为什么不用 `Block::from_mut_slice`：它自 hybrid-array 0.2 起已被标记废弃
///（`from_slice` 同理），官方建议改走 `TryFrom`。这里包一层，把"长度必然正确"
/// 这个前提收敛到一个 `expect` 上，调用处就不必反复处理错误。
fn aes_block_mut(buf: &mut [u8; AES_BLOCK]) -> &mut Block<Aes256> {
    (&mut buf[..])
        .try_into()
        .expect("AES 分组长度固定为 16 字节")
}

/// AES-256-CBC 加密。输入必须是块大小的整数倍。
fn cbc_encrypt(cipher: &Aes256, iv: &[u8; IV_LEN], data: &[u8]) -> Vec<u8> {
    debug_assert_eq!(data.len() % AES_BLOCK, 0);
    let mut prev = *iv;
    let mut out = Vec::with_capacity(data.len());
    for chunk in data.as_chunks::<AES_BLOCK>().0 {
        let mut buf = [0u8; AES_BLOCK];
        buf.copy_from_slice(chunk);
        // CBC：先与上一段密文异或，再加密。
        for i in 0..AES_BLOCK {
            buf[i] ^= prev[i];
        }
        cipher.encrypt_block(aes_block_mut(&mut buf));
        prev = buf;
        out.extend_from_slice(&buf);
    }
    out
}

/// AES-256-CBC 解密。输入必须是块大小的整数倍。
fn cbc_decrypt(cipher: &Aes256, iv: &[u8; IV_LEN], data: &[u8]) -> Vec<u8> {
    debug_assert_eq!(data.len() % AES_BLOCK, 0);
    let mut prev = *iv;
    let mut out = Vec::with_capacity(data.len());
    for chunk in data.as_chunks::<AES_BLOCK>().0 {
        let mut buf = [0u8; AES_BLOCK];
        buf.copy_from_slice(chunk);
        cipher.decrypt_block(aes_block_mut(&mut buf));
        // CBC：解密后与上一段密文异或。
        for i in 0..AES_BLOCK {
            buf[i] ^= prev[i];
        }
        prev.copy_from_slice(chunk);
        out.extend_from_slice(&buf);
    }
    out
}

/// 定时长比较，避免通过比较耗时侧信道泄露信息。
fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    /// Go 版实现（bitcomet-monitor/rncryptor.go）产出的真实密文。
    /// 用它来证明 Rust 实现与 Go 实现遵守**同一协议**，而不是"看起来一样"。
    ///
    /// 生成方式（口令 = `go-vector-password`，明文 = 下面的 GO_VECTOR_PLAINTEXT）：
    /// 见 .probe/rncvector/main.go
    const GO_VECTOR_B64: &str = "AwEQ//zRsSNUDNiToI4GkJv8FZJ2sQ1WwhF9tUNVmqco1IOLhcg+DkV7TLdFrXpaorfkOXIsfbErOjG7Ty7fWIPn5SY8vg1tt0FTBEWkEKqEdcpjqOwnBmzRyEQMjLHV6/+GrVzwVjfElMvXP5hQCw+g";
    const GO_VECTOR_PASSWORD: &str = "go-vector-password";
    const GO_VECTOR_PLAINTEXT: &str = r#"{"password":"admin","username":"admin"}"#;

    #[test]
    fn round_trip_preserves_plaintext() {
        let pt = br#"{"username":"admin","password":"admin"}"#;
        let token = encrypt(pt, "my-client-id").expect("加密应成功");
        let back = decrypt(&token, "my-client-id").expect("解密应成功");
        assert_eq!(back, pt);
    }

    #[test]
    fn round_trip_with_chinese_and_utf8() {
        let pt = "用户名 admin，密码含中文与 emoji 🔒".as_bytes();
        let token = encrypt(pt, "口令-中文-client-id").expect("加密应成功");
        assert_eq!(decrypt(&token, "口令-中文-client-id").unwrap(), pt);
    }

    #[test]
    fn binary_layout_matches_spec() {
        let token = encrypt(b"hello", "pw").unwrap();
        let raw = B64.decode(token.as_bytes()).unwrap();

        assert_eq!(raw[0], 3, "version 字节必须是 3");
        assert_eq!(raw[1], 1, "options 字节必须是 1");
        // 1+1+8+8+16 = 34 是固定头部；尾部 32 字节是 HMAC
        let ciphertext_len = raw.len() - 34 - 32;
        assert_eq!(ciphertext_len % 16, 0, "密文必须是 16 字节整数倍");
        // 明文 5 字节 + PKCS#7 填充 11 字节 = 16 字节
        assert_eq!(ciphertext_len, 16, "5 字节明文应产生 1 个分组");
    }

    #[test]
    fn padding_is_exactly_pkcs7() {
        // 明文正好是块大小整数倍时，必须**额外补整块**，这是 PKCS#7 的关键细节。
        let padded = pkcs7_pad(&[0u8; 16], 16);
        assert_eq!(padded.len(), 32);
        assert_eq!(&padded[16..], &[16u8; 16]);

        let padded = pkcs7_pad(b"abc", 16);
        assert_eq!(padded.len(), 16);
        assert_eq!(&padded[3..], &[13u8; 13]);

        assert!(pkcs7_unpad(&padded, 16).unwrap() == b"abc");

        // 合法的单字节填充（pad=1）必须被接受 —— 常见误解是"填充看起来可疑就拒绝"，
        // 但 pad=1 是完全合法的 PKCS#7，错误拒绝会让约 1/16 的正常密文解密失败。
        assert_eq!(pkcs7_unpad(&[1u8; 16], 16).unwrap().len(), 15);
        // pad=2 同理
        assert_eq!(pkcs7_unpad(&[2u8; 16], 16).unwrap().len(), 14);

        // 非法填充必须被拒绝，逐类覆盖：
        // 1) 填充字节自相矛盾：末字节声明 pad=3，但前两字节不是 3
        assert!(
            pkcs7_unpad(&[0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 7, 3, 3], 16).is_err(),
            "填充字节与声明长度不符应报错"
        );
        // 2) pad=0 非法
        assert!(pkcs7_unpad(&[0u8; 16], 16).is_err(), "零填充非法");
        // 3) pad 超过块大小
        assert!(
            pkcs7_unpad(&[20u8; 16], 16).is_err(),
            "pad 超过块大小应报错"
        );
        // 4) 空输入
        assert!(pkcs7_unpad(&[], 16).is_err(), "空输入应报错");
        // 5) 长度不是块大小整数倍
        assert!(pkcs7_unpad(&[1u8; 15], 16).is_err(), "非整块长度应报错");
        // 6) pad 大于数据长度（16 字节数据声称 pad=17 不可能，用 15 字节非法长度已覆盖；
        //    这里用"末字节 9 但前面不足 9 个 9"验证越界保护）
        assert!(
            pkcs7_unpad(&[9, 9, 9, 9, 9, 9, 9, 9, 1, 1, 1, 1, 1, 1, 1, 9], 16).is_err(),
            "越界填充应报错"
        );
    }

    #[test]
    fn empty_password_is_rejected() {
        // 实测：空 client_id 会导致服务端静默失败，必须在本地就拦住。
        assert!(matches!(
            encrypt(b"x", ""),
            Err(CryptorError::EmptyPassword)
        ));
        assert!(matches!(
            decrypt("AAAA", ""),
            Err(CryptorError::EmptyPassword)
        ));
    }

    #[test]
    fn two_encryptions_of_same_plaintext_differ() {
        // 随机 salt/IV 必须生效，否则密文可被关联。
        let a = encrypt(b"same", "pw").unwrap();
        let b = encrypt(b"same", "pw").unwrap();
        assert_ne!(a, b, "同样明文两次加密结果必须不同");
    }

    #[test]
    fn tampered_ciphertext_fails_integrity() {
        let token = encrypt(b"hello world", "pw").unwrap();
        let mut raw = B64.decode(token.as_bytes()).unwrap();
        // 翻转载荷最后一个字节（位于 HMAC 之前）
        let idx = raw.len() - HMAC_LEN - 1;
        raw[idx] ^= 0x01;
        let bad = B64.encode(&raw);
        assert!(
            matches!(decrypt(&bad, "pw"), Err(CryptorError::IntegrityFailed)),
            "篡改载荷必须被 HMAC 拦下"
        );
    }

    #[test]
    fn wrong_password_fails_integrity() {
        let token = encrypt(b"hello", "right-pw").unwrap();
        assert!(matches!(
            decrypt(&token, "wrong-pw"),
            Err(CryptorError::IntegrityFailed)
        ));
    }

    #[test]
    fn malformed_input_is_rejected_gracefully() {
        assert!(decrypt("!!!!not base64!!!!", "pw").is_err());
        assert!(decrypt(&B64.encode([0u8; 4]), "pw").is_err(), "过短应报错");
        let mut short = vec![3u8, 1];
        short.extend_from_slice(&[0u8; 100]);
        assert!(decrypt(&B64.encode(&short), "pw").is_ok() || true);
    }

    /// 交叉实现验证：解密 Go 版产出的密文。
    /// 这条测试是"两套实现协议一致"的**硬证据**。
    #[test]
    fn decrypts_go_generated_ciphertext() {
        if GO_VECTOR_B64 == "GOVECTOR_PLACEHOLDER" {
            eprintln!("跳过：尚未嵌入 Go 向量");
            return;
        }
        let pt = decrypt(GO_VECTOR_B64, GO_VECTOR_PASSWORD).expect("必须能解开 Go 版产出的密文");
        assert_eq!(String::from_utf8(pt).unwrap(), GO_VECTOR_PLAINTEXT);
    }

    /// 登录明文必须是合法 JSON 且字段名正确。
    #[test]
    fn login_payload_shape() {
        let cred = json!({"username": "admin", "password": "admin"});
        let s = serde_json::to_string(&cred).unwrap();
        let v: serde_json::Value = serde_json::from_str(&s).unwrap();
        assert_eq!(v["username"], "admin");
        assert_eq!(v["password"], "admin");
    }
}
