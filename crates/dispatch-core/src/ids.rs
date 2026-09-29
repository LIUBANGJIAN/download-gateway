//! 标识符生成：GID / task_id（ULID）/ 128 位随机十六进制。
//!
//! 三者都是**纯函数**（除随机源外无副作用），可独立单测。
//!
//! 随机源统一走 `getrandom::fill`（getrandom 0.4 API，与 `crates/bitcomet-api` 对齐）。
//! 不引入 `rand` —— 本项目只需要密码学安全随机字节，`getrandom` 已足够且已在依赖树中。

use std::time::{SystemTime, UNIX_EPOCH};

/// 当前 Unix 毫秒。
pub fn now_ms() -> i64 {
    secs_ms().1
}

/// 当前 Unix 秒。
pub fn now_secs() -> i64 {
    secs_ms().0
}

fn secs_ms() -> (i64, i64) {
    match SystemTime::now().duration_since(UNIX_EPOCH) {
        Ok(d) => (d.as_secs() as i64, d.as_millis() as i64),
        Err(_) => (0, 0),
    }
}

/// 读取 `N` 个密码学安全随机字节。失败时回落到全零（几乎不可能，仅避免 panic）。
fn random_bytes<const N: usize>() -> [u8; N] {
    let mut buf = [0u8; N];
    if getrandom::fill(&mut buf).is_err() {
        // 极端情形：OS 随机源不可用。回落全零会让 GID 冲突，由 `create_task` 的重试兜底。
        tracing::error!("获取安全随机数失败：已回落全零，标识唯一性将依赖重试机制");
    }
    buf
}

/// 16 位小写十六进制 GID（正则 `^[0-9a-f]{16}$`）。
///
/// 随机 64 位；**不保证**进程内唯一 —— 唯一性由 `task.gid UNIQUE` 约束 + `create_task`
/// 的换 GID 重试共同保证（`02 §4.1.4`）。
pub fn new_gid() -> String {
    let bytes = random_bytes::<8>();
    let v = u64::from_be_bytes(bytes);
    format!("{v:016x}")
}

/// 26 字符 Crockford Base32 ULID：48bit 毫秒时间戳 + 80bit 随机。
///
/// 时间前缀（前 10 字符）随时间**单调不减**，便于按创建顺序排序。
pub fn new_task_id(now_ms: i64) -> String {
    let mut buf = [0u8; 16];
    let ts = now_ms.max(0) as u64;
    // 高 48 位放毫秒时间戳（大端）。
    buf[0] = (ts >> 40) as u8;
    buf[1] = (ts >> 32) as u8;
    buf[2] = (ts >> 24) as u8;
    buf[3] = (ts >> 16) as u8;
    buf[4] = (ts >> 8) as u8;
    buf[5] = ts as u8;
    // 低 80 位放随机。
    buf[6..16].copy_from_slice(&random_bytes::<10>());
    ulid_encode(&buf)
}

/// 128bit 十六进制（32 字符）：会话 sid / invite_token / device_token。
pub fn new_hex_128() -> String {
    let bytes = random_bytes::<16>();
    let mut out = String::with_capacity(32);
    for b in bytes {
        out.push_str(&format!("{b:02x}"));
    }
    out
}

/// Crockford Base32 字母表（去掉了易混淆的 I/L/O/U）。
const CROCKFORD: &[u8; 32] = b"0123456789ABCDEFGHJKMNPQRSTVWXYZ";

/// 把 16 字节（128bit）编码成 26 字符 ULID 字符串。
///
/// 26 字符 × 5bit = 130bit，最高的 2bit 恒为零（即首字符取值 `0..=7`）。
fn ulid_encode(bytes: &[u8; 16]) -> String {
    let mut value = u128::from_be_bytes(*bytes);
    let mut out = [0u8; 26];
    for slot in out.iter_mut().rev() {
        *slot = CROCKFORD[(value & 0x1f) as usize];
        value >>= 5;
    }
    String::from_utf8(out.to_vec()).expect("Crockford 字母表恒为 ASCII")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn gid_matches_16_lower_hex() {
        for _ in 0..1000 {
            let g = new_gid();
            assert_eq!(g.len(), 16, "GID 长度应为 16: {g}");
            assert!(
                g.chars()
                    .all(|c| c.is_ascii_digit() || ('a'..='f').contains(&c)),
                "GID 必须是纯小写十六进制: {g}"
            );
        }
    }

    #[test]
    fn gid_is_unique_over_1000_draws() {
        use std::collections::HashSet;
        let set: HashSet<String> = (0..1000).map(|_| new_gid()).collect();
        assert_eq!(set.len(), 1000, "1000 次抽取应无重复");
    }

    #[test]
    fn task_id_is_26_chars() {
        let id = new_task_id(1_700_000_000_000);
        assert_eq!(id.len(), 26, "ULID 应为 26 字符: {id}");
        assert!(
            id.bytes().all(|b| CROCKFORD.contains(&b)),
            "字符必须来自字母表: {id}"
        );
    }

    #[test]
    fn task_id_time_prefix_is_monotonic() {
        let a = new_task_id(1_000_000_000_000);
        let b = new_task_id(1_000_000_001_000);
        let c = new_task_id(2_000_000_000_000);
        // 前 10 字符编码 48bit 时间戳，随时间不减。
        assert!(a[..10] <= b[..10], "{a} vs {b}");
        assert!(b[..10] <= c[..10], "{b} vs {c}");
    }

    #[test]
    fn hex_128_is_32_lower_hex() {
        let h = new_hex_128();
        assert_eq!(h.len(), 32);
        assert!(
            h.chars()
                .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase())
        );
    }
}
