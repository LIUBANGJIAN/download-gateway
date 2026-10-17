//! 自助找回：`POST /api/admin/password/reset-request` —— 生成一次性临时口令并**只写日志**。
//!
//! 这是 Go 侧 `internal/adminpass/temp.go` + `internal/admin/selfreset.go` 的 Rust 对应实现，
//! 路径、环境变量、响应字段、错误码、日志键名、频率限制全部逐条对齐
//! （验收脚本 `scripts/selfreset-e2e.sh` 对两套实现都跑一遍）。
//!
//! # 为什么这个接口不需要登录
//!
//! 它存在的理由就是「用户进不去」。所以它天然是匿名口，安全性不能建立在会话上，
//! 只能建立在**口令交付通道**上：口令只出现在服务端日志里，而能读到日志的人
//! （`docker logs` / `journalctl` / 容器平台控制台）本来就能拿到这个容器的完全控制权。
//! 攻击者能点按钮，但看不到结果。
//!
//! # 这个接口能被人滥用成什么样（如实写清）
//!
//! * **刷日志**：每次生成写一行带口令的日志。限制为同一 IP 60 秒 1 次、
//!   全局 10 分钟 5 次（见 [`TempCode::issue`]），把刷屏成本抬到「会留下明显痕迹」。
//! * **把人锁在外面？** 不会。临时口令与真实口令**并存**，原口令不失效
//!   （这是刻意不采用「覆盖原口令」实现的原因 —— 那种做法下谁点一下按钮都能造成拒绝服务）。
//! * **拿到临时口令？** 需要日志读权限 = 宿主机控制权，与 `reset-password` 子命令同级。
//!
//! # 为什么是「额外凭据」而不是「覆盖掉原口令」
//!
//! 覆盖原口令的做法实现更省事（登录路径一行都不用改），但任何能访问管理口的人
//! 只要点一次按钮，就能把真正的管理员挡在门外（而临时口令 10 分钟后就过期，
//! 用户若没去看日志，会被**永久**锁死）。所以让两者并存：原口令照常可用，
//! 临时口令单独有效、单独受 TTL 与频率限制约束。
//!
//! # 生命周期（任何一个成立即失效）
//!
//! * 过期（默认 600 秒）；
//! * 用户在管理台改了口令（[`crate::state::AdminState::set_credential`] 内清空）；
//! * 又生成了一串新的（同一时刻只允许存在一串，避免日志里躺着多串有效口令）；
//! * 进程重启（只活在内存里，不落库）。

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};

use axum::extract::{ConnectInfo, State};
use axum::http::header::{CACHE_CONTROL, RETRY_AFTER};
use axum::http::{HeaderValue, StatusCode};
use axum::response::Response;
use serde_json::json;

use super::{C_INTERNAL, C_NOT_FOUND, C_TOO_MANY_REQUESTS, err, ok};
use crate::state::AdminState;

/// 控制登录页的「忘记口令」入口是否可用（`0/false/off/no` 关闭）。
pub const ENV_ALLOW_SELF_RESET: &str = "DISPATCH_ALLOW_SELF_RESET";
/// 临时口令有效期（秒），允许 30–86400。
pub const ENV_TEMP_PASSWORD_TTL: &str = "DISPATCH_TEMP_PASSWORD_TTL";

/// 临时口令长度（字符数）。
///
/// 12 位、32 个字符的字母表 ≈ 60 bit 熵：从日志里抄写不会出错（去掉了 I/O/0/1），
/// 在线爆破在「10 分钟 + 每分钟 1 次 + 全局 10 分钟 5 次」的频率限制下不可行。
pub const TEMP_CODE_LEN: usize = 12;

/// 去掉了容易看错的 `I`、`O`、`0`、`1`。长度 32 ⇒ `256 % 32 == 0`，
/// 用随机字节直接取模没有偏置，不需要拒绝采样。
const TEMP_ALPHABET: &[u8] = b"ABCDEFGHJKLMNPQRSTUVWXYZ23456789";

/// 临时口令默认有效期（秒）。
pub const DEFAULT_TEMP_TTL: i64 = 600;
/// 同一来源 IP 两次生成的间隔（秒）。
pub const TEMP_PER_IP_COOLDOWN: i64 = 60;
/// 全局计数的滑动窗口（秒）。
pub const TEMP_GLOBAL_WINDOW: i64 = 600;
/// 窗口内允许生成的次数上限。
///
/// 这个上限防的不是爆破（口令熵够），而是**日志被刷屏**：每次生成都会往日志里
/// 写一行带口令的记录，如果谁都能无限点，真正的记录就会被冲走，
/// 用户翻日志时看到的是一屏陌生口令。
pub const TEMP_GLOBAL_MAX: usize = 5;

/// [`TempCode::issue`] 的失败原因。
#[derive(Debug, PartialEq, Eq)]
pub enum IssueError {
    /// 频率限制（附带建议的重试秒数）。
    TooFrequent(i64),
    /// 随机源异常（→ 500）。
    Random,
}

/// 生成临时口令的进程内状态（不带锁，由 [`TempCode::inner`] 保护）。
#[derive(Default)]
struct TempInner {
    code: String,
    expires_at: i64,
    /// 每个来源 IP 上次生成的时刻。
    by_ip: HashMap<String, i64>,
    /// 全局生成时刻的滑动窗口（按时间升序）。
    recent: Vec<i64>,
}

impl TempInner {
    /// 清空临时口令本身（不动频率限制的计数）。
    fn clear_locked(&mut self) {
        self.code.clear();
        self.expires_at = 0;
    }
}

/// 一次性临时口令的**进程内**持有者。
///
/// 时间一律由调用方传入（`now`），不做内部读钟 —— 这样过期边界、频率窗口
/// 都能用固定时间戳做单元测试，不必 `sleep`。
pub struct TempCode {
    inner: Mutex<TempInner>,
}

impl Default for TempCode {
    fn default() -> Self {
        Self::new()
    }
}

impl TempCode {
    /// 构造一个空的持有者（还没有任何临时口令）。
    pub fn new() -> Self {
        Self {
            inner: Mutex::new(TempInner::default()),
        }
    }

    /// 生成一串新的临时口令并在进程内登记，返回 `(口令, 有效期秒数)`。
    ///
    /// 频率限制同时看两个维度：
    ///
    /// * 同一 IP 至少间隔 [`TEMP_PER_IP_COOLDOWN`]；
    /// * 全局窗口 [`TEMP_GLOBAL_WINDOW`] 内最多 [`TEMP_GLOBAL_MAX`] 次。
    ///
    /// 触发限制时返回 [`IssueError::TooFrequent`]（带建议的重试秒数，供 `Retry-After` 用）。
    /// 生成失败（随机源异常）时**不改动任何已有状态** —— 老口令继续有效，
    /// 不至于把人锁死。
    pub fn issue(&self, now: i64, ip: &str, ttl: i64) -> Result<(String, i64), IssueError> {
        let ttl = if ttl <= 0 { DEFAULT_TEMP_TTL } else { ttl };
        let mut inner = self.inner.lock().unwrap_or_else(|e| e.into_inner());

        // 滑动窗口：把窗口外的时间戳扔掉。
        inner.recent.retain(|t| now - t < TEMP_GLOBAL_WINDOW);

        if let Some(last) = inner.by_ip.get(ip)
            && now - last < TEMP_PER_IP_COOLDOWN
        {
            return Err(IssueError::TooFrequent(TEMP_PER_IP_COOLDOWN - (now - last)));
        }
        if inner.recent.len() >= TEMP_GLOBAL_MAX {
            let oldest = inner.recent[0];
            return Err(IssueError::TooFrequent(TEMP_GLOBAL_WINDOW - (now - oldest)));
        }

        // 顺手清掉太久没来的 IP，避免 map 随着扫描/爬虫无限增长。
        if inner.by_ip.len() > 128 {
            let cutoff = now - TEMP_GLOBAL_WINDOW;
            inner.by_ip.retain(|_, t| *t > cutoff);
        }

        let code = generate_temp_code()?;
        // 只保留最新一串：日志里躺着多串有效口令，等于把有效期拉长到「最早那串的 TTL」。
        inner.code = code.clone();
        inner.expires_at = now + ttl;
        inner.by_ip.insert(ip.to_string(), now);
        inner.recent.push(now);
        Ok((code, ttl))
    }

    /// 判断输入是否是**当前有效**的临时口令。
    ///
    /// 过期时顺手清掉（免得每次登录都白算一次常量时间比较）。
    pub fn verify(&self, input: &str, now: i64) -> bool {
        if input.is_empty() {
            return false;
        }
        let mut inner = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        if inner.code.is_empty() {
            return false;
        }
        if now > inner.expires_at {
            inner.clear_locked();
            return false;
        }
        // 已持有写锁，直接比较即可；仍走常量时间比较以保持与 Go 侧同一套语义。
        crate::ingress::envelope::ct_eq(input, &inner.code)
    }

    /// 报告当前是否有未过期的临时口令（供状态接口/日志判断，不回传口令本身）。
    pub fn active(&self, now: i64) -> bool {
        let inner = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        !inner.code.is_empty() && now <= inner.expires_at
    }

    /// 主动清掉临时口令（改口令成功后由 `set_credential` 调用）。
    pub fn clear(&self) {
        let mut inner = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        inner.clear_locked();
    }
}

/// 生成一串临时口令。随机源异常时返回 [`IssueError::Random`]（调用方转 500）。
fn generate_temp_code() -> Result<String, IssueError> {
    let mut buf = [0u8; TEMP_CODE_LEN];
    getrandom::fill(&mut buf).map_err(|e| {
        tracing::error!("生成临时口令失败（随机源异常）：{e}");
        IssueError::Random
    })?;
    let out: Vec<u8> = buf
        .iter()
        .map(|b| TEMP_ALPHABET[(*b as usize) % TEMP_ALPHABET.len()])
        .collect();
    // 字母表全是 ASCII，转换不会失败；真出错就是 bug，用 expect 显式暴露。
    Ok(String::from_utf8(out).expect("临时口令字母表必须是 ASCII"))
}

// ---------------------------------------------------------------------------
// 环境变量
// ---------------------------------------------------------------------------

/// 读两个只在管理口用到的环境变量：`(是否启用, TTL 秒)`。
///
/// 为什么就地读而不塞进 `Config`：那两个字段会进 `EnvSnapshot`（管理台的「环境快照」
/// 面板），而快照的键集合是与 Go 侧逐字对齐的契约，加键要两边同时改并补断言。
/// 这两个值只影响一个 handler 的行为、不需要展示，就地读取的改动面最小。
pub fn reset_policy() -> (bool, i64) {
    let raw = std::env::var(ENV_ALLOW_SELF_RESET).unwrap_or_default();
    if matches!(
        raw.trim().to_ascii_lowercase().as_str(),
        "0" | "false" | "off" | "no"
    ) {
        return (false, 0);
    }
    let mut ttl = DEFAULT_TEMP_TTL;
    // 解析失败/越界**静默回落默认值**：这类部署参数写错不该让「找回口令」失效，
    // 那是用户唯一能自救的路径，宁可放宽也不能因为一个手误把它关掉。
    if let Ok(s) = std::env::var(ENV_TEMP_PASSWORD_TTL)
        && let Ok(n) = s.trim().parse::<i64>()
        && (30..=86400).contains(&n)
    {
        ttl = n;
    }
    (true, ttl)
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

/// `POST /api/admin/password/reset-request` —— 生成一次性临时口令（**匿名**）。
///
/// 响应只有有效期与日志查看提示，**绝不回传口令本身**。
pub async fn request_temp_password(
    ConnectInfo(addr): ConnectInfo<SocketAddr>,
    State(st): State<Arc<AdminState>>,
) -> Response {
    let (enabled, ttl) = reset_policy();
    if !enabled {
        // 关闭时用 404 而不是 403：这条路径对外**不存在**，不给探测者
        // 「这里有功能但被禁用了」的信息。
        return err(StatusCode::NOT_FOUND, C_NOT_FOUND, "自助重置未启用");
    }

    let now = crate::ids::now_secs();
    // 刻意**不读** `X-Forwarded-For`：那是个可以随便伪造的头，用它做限流等于没限流。
    // 代价是放在反代后面时所有人共享反代的 IP（频率限制退化为全局限制）—— 可接受，
    // 因为全局限制才是防刷日志的主力，按 IP 只是让正常用户不至于互相影响。
    let ip = addr.ip().to_string();

    let (code, expires_in) = match st.temp_code.issue(now, &ip, ttl) {
        Ok(v) => v,
        Err(IssueError::TooFrequent(retry)) => {
            let mut resp = err(
                StatusCode::TOO_MANY_REQUESTS,
                C_TOO_MANY_REQUESTS,
                "生成过于频繁，请稍后再试",
            );
            if retry > 0
                && let Ok(v) = HeaderValue::from_str(&retry.to_string())
            {
                resp.headers_mut().insert(RETRY_AFTER, v);
            }
            return resp;
        }
        Err(IssueError::Random) => {
            return err(
                StatusCode::INTERNAL_SERVER_ERROR,
                C_INTERNAL,
                "生成临时口令失败",
            );
        }
    };

    // ── 唯一的交付通道：日志 ───────────────────────────────────────────────
    // 键名固定为 `temp_password`：用户的排查动作是 `docker logs <c> | grep temp_password`，
    // 日志平台做敏感字段脱敏时也认这个键。
    // 级别用 WARN：让它在一片 INFO 里显眼，且默认日志级别下不会被过滤掉。
    tracing::warn!(
        temp_password = %code,
        expires_in_seconds = expires_in,
        client_ip = %ip,
        "已生成一次性临时管理口令（只打印在本日志里，登录后请立即修改口令）"
    );

    // 审计**不记口令**：event_log 在管理台上是可见的，记了就等于把口令贴到界面上。
    super::password::audit(
        &st,
        now,
        &format!("已生成一次性临时管理口令（来源 IP {ip}）"),
        "warn",
    )
    .await;

    let mut resp = ok(json!({
        "expires_in": expires_in,
        "log_hint": "docker logs <容器名> | grep temp_password",
    }));
    resp.headers_mut()
        .insert(CACHE_CONTROL, HeaderValue::from_static("no-store"));
    resp
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 生成的串长度、字符集必须符合约定（要从日志里手抄）。
    #[test]
    fn code_shape() {
        for _ in 0..64 {
            let code = generate_temp_code().expect("随机源正常");
            assert_eq!(code.chars().count(), TEMP_CODE_LEN);
            assert!(code.bytes().all(|b| TEMP_ALPHABET.contains(&b)), "{code}");
            // 易混字符必须排除。
            for bad in ['I', 'O', '0', '1'] {
                assert!(!code.contains(bad), "{code} 含易混字符 {bad}");
            }
        }
    }

    #[test]
    fn issue_then_verify() {
        let t = TempCode::new();
        let (code, ttl) = t.issue(1000, "10.0.0.1", DEFAULT_TEMP_TTL).unwrap();
        assert_eq!(ttl, DEFAULT_TEMP_TTL);
        assert!(t.verify(&code, 1000));
        assert!(!t.verify("", 1000));
        assert!(!t.verify("WRONGCODE234", 1000));
        assert!(t.active(1000));
    }

    /// 过期边界：`now == expires_at` 仍有效，多 1 秒即失效并顺手清空。
    #[test]
    fn expiry_boundary() {
        let t = TempCode::new();
        let (code, ttl) = t.issue(1000, "10.0.0.1", 600).unwrap();
        assert!(t.verify(&code, 1000 + ttl), "到期那一秒应仍可用");
        assert!(!t.verify(&code, 1000 + ttl + 1));
        assert!(!t.active(1000 + ttl + 1));
        // 过期后同一串再拿来也不能用。
        assert!(!t.verify(&code, 1000 + ttl + 2));
    }

    /// 只保留最新一串：生成第二串后第一串立即失效。
    #[test]
    fn only_latest_survives() {
        let t = TempCode::new();
        let (a, _) = t.issue(1000, "10.0.0.1", 600).unwrap();
        let (b, _) = t.issue(2000, "10.0.0.2", 600).unwrap();
        assert_ne!(a, b);
        assert!(!t.verify(&a, 2000), "旧串必须失效");
        assert!(t.verify(&b, 2000));
    }

    /// 同 IP 60 秒内只能生成一次；换 IP 不受影响（但受全局限制）。
    #[test]
    fn per_ip_cooldown() {
        let t = TempCode::new();
        t.issue(1000, "10.0.0.1", 600).unwrap();
        let err = t.issue(1030, "10.0.0.1", 600).unwrap_err();
        assert_eq!(err, IssueError::TooFrequent(30), "应为剩余冷却秒数");
        assert!(
            t.issue(1000 + TEMP_PER_IP_COOLDOWN, "10.0.0.1", 600)
                .is_ok()
        );
    }

    /// 全局 10 分钟内最多 5 次；窗口滑过后恢复。
    #[test]
    fn global_rate_limit() {
        let t = TempCode::new();
        for i in 0..TEMP_GLOBAL_MAX {
            t.issue(1000 + i as i64, &format!("10.0.0.{i}"), 600)
                .unwrap_or_else(|e| panic!("第 {i} 次不该被限流（{e:?}）"));
        }
        match t.issue(1005, "10.0.0.99", 600) {
            Err(IssueError::TooFrequent(retry)) => {
                assert!(retry > 0, "应给出建议重试秒数，实际 {retry}");
            }
            other => panic!("第 6 次应被全局限流，实际 {other:?}"),
        }
        // 最早那次滑出窗口后就能再生成。
        assert!(t.issue(1000 + TEMP_GLOBAL_WINDOW, "10.0.0.99", 600).is_ok());
    }

    /// `clear` 后立刻失效（改口令时走这条）。
    #[test]
    fn clear_kills_code() {
        let t = TempCode::new();
        let (code, _) = t.issue(1000, "10.0.0.1", 600).unwrap();
        t.clear();
        assert!(!t.verify(&code, 1001));
        assert!(!t.active(1001));
    }

    /// 默认构造函数可用（`Default` 与 `new` 行为一致）。
    #[test]
    fn default_is_usable() {
        let t = TempCode::default();
        let (code, _) = t.issue(1000, "10.0.0.1", 600).unwrap();
        assert!(t.verify(&code, 1000));
    }
}
