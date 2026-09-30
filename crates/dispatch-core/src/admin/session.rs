//! 管理会话、登录防爆破退避、CSRF 校验、Cookie 生成。
//!
//! - 会话：内存 `Map<sid, Session>`，空闲 30min **滑动续期**，重启全失效。
//! - 防爆破：按来源 IP 计连续失败，5 次 → 锁定 5 分钟；成功清零。
//! - CSRF：写操作要求同源（`Origin` 或 `Sec-Fetch-Site`），读操作不查。

use std::collections::HashMap;
use std::net::IpAddr;
use std::sync::Mutex;

use axum::http::{HeaderMap, header};

use crate::ids;
use crate::state::{AdminState, CookieSecureMode};

/// 连续失败多少次触发锁定。
pub const MAX_FAILURES: u32 = 5;
/// 锁定时长（秒）。
pub const LOCK_SECS: i64 = 300;
/// 会话 Cookie 名。
pub const COOKIE_NAME: &str = "sid";

#[derive(Clone, Copy)]
struct Session {
    last_seen_at: i64,
}

/// 内存会话表。
pub struct SessionStore {
    idle_ttl_secs: i64,
    inner: Mutex<HashMap<String, Session>>,
}

impl SessionStore {
    /// 新建，`idle_ttl_secs` 为空闲过期时间。
    pub fn new(idle_ttl_secs: i64) -> Self {
        Self {
            idle_ttl_secs,
            inner: Mutex::new(HashMap::new()),
        }
    }

    /// 创建会话，返回 `sid`。
    pub fn create(&self, now: i64) -> String {
        let sid = ids::new_hex_128();
        self.inner
            .lock()
            .expect("会话表锁中毒")
            .insert(sid.clone(), Session { last_seen_at: now });
        sid
    }

    /// 校验并**滑动续期**；返回新的过期时刻；过期/不存在返回 `None`。
    pub fn touch(&self, sid: &str, now: i64) -> Option<i64> {
        let mut inner = self.inner.lock().expect("会话表锁中毒");
        let s = inner.get_mut(sid)?;
        if now - s.last_seen_at > self.idle_ttl_secs {
            inner.remove(sid);
            return None;
        }
        s.last_seen_at = now;
        Some(now + self.idle_ttl_secs)
    }

    /// 主动失效。
    pub fn revoke(&self, sid: &str) {
        self.inner.lock().expect("会话表锁中毒").remove(sid);
    }
}

#[derive(Clone, Copy, Default)]
struct Fail {
    count: u32,
    locked_until: i64,
}

/// 登录防爆破退避（按来源 IP）。
pub struct LoginGuard {
    inner: Mutex<HashMap<IpAddr, Fail>>,
}

impl Default for LoginGuard {
    fn default() -> Self {
        Self::new()
    }
}

impl LoginGuard {
    /// 空表。
    pub fn new() -> Self {
        Self {
            inner: Mutex::new(HashMap::new()),
        }
    }

    /// 若被锁定则返回解锁时刻。
    pub fn locked_until(&self, ip: IpAddr, now: i64) -> Option<i64> {
        let mut inner = self.inner.lock().expect("登录守卫锁中毒");
        let f = inner.get(&ip)?;
        if f.locked_until > now {
            Some(f.locked_until)
        } else {
            if f.locked_until != 0 {
                inner.remove(&ip);
            }
            None
        }
    }

    /// 记录一次失败，返回连续失败数。
    pub fn record_failure(&self, ip: IpAddr, now: i64) -> u32 {
        let mut inner = self.inner.lock().expect("登录守卫锁中毒");
        let f = inner.entry(ip).or_default();
        f.count += 1;
        if f.count >= MAX_FAILURES {
            f.locked_until = now + LOCK_SECS;
        }
        f.count
    }

    /// 记录成功（清零）。
    pub fn record_success(&self, ip: IpAddr) {
        self.inner.lock().expect("登录守卫锁中毒").remove(&ip);
    }
}

/// 守卫结论。
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Guard {
    /// 通过。
    Ok,
    /// 未认证。
    Unauthenticated,
    /// CSRF 校验失败（跨源写）。
    CsrfInvalid,
}

/// 从 `Cookie` 头取指定键。
fn cookie_value(headers: &HeaderMap, key: &str) -> Option<String> {
    let raw = headers.get(header::COOKIE)?.to_str().ok()?;
    for part in raw.split(';') {
        let kv = part.trim();
        if let Some((k, v)) = kv.split_once('=')
            && k.trim() == key
        {
            return Some(v.trim().to_string());
        }
    }
    None
}

/// 读守卫：仅查会话（**滑动续期**），不动 CSRF；返回 `(结论, 新过期时刻)`。
pub fn guard_read(st: &AdminState, headers: &HeaderMap) -> (Guard, Option<i64>) {
    let now = ids::now_secs();
    match cookie_value(headers, COOKIE_NAME)
        .as_deref()
        .and_then(|sid| st.sessions.touch(sid, now))
    {
        Some(exp) => (Guard::Ok, Some(exp)),
        None => (Guard::Unauthenticated, None),
    }
}

/// 写守卫：查会话 + CSRF + 续期，返回 `(结论, 新过期时刻)`。
pub fn guard_write(st: &AdminState, headers: &HeaderMap) -> (Guard, Option<i64>) {
    let now = ids::now_secs();
    let expiry = cookie_value(headers, COOKIE_NAME)
        .as_deref()
        .and_then(|sid| st.sessions.touch(sid, now));
    match expiry {
        None => (Guard::Unauthenticated, None),
        Some(exp) => {
            if same_origin(headers) {
                (Guard::Ok, Some(exp))
            } else {
                (Guard::CsrfInvalid, Some(exp))
            }
        }
    }
}

/// 是否同源。
///
/// 判定顺序：① `Sec-Fetch-Site`（浏览器自动带）；② `Origin` 与 `Host` 比对；
/// ③ 两者都无（多为 curl 等非浏览器客户端）⇒ 视为同源（浏览器必然会带上前两者之一）。
pub fn same_origin(headers: &HeaderMap) -> bool {
    if let Some(v) = headers.get("sec-fetch-site").and_then(|v| v.to_str().ok()) {
        match v.trim().to_ascii_lowercase().as_str() {
            "same-origin" | "none" => return true,
            "cross-site" | "same-site" => return false,
            _ => {}
        }
    }
    if let Some(origin) = headers.get(header::ORIGIN).and_then(|v| v.to_str().ok()) {
        let origin = origin.trim();
        let ohost = origin.split_once("://").map(|(_, h)| h).unwrap_or(origin);
        return match headers.get(header::HOST).and_then(|v| v.to_str().ok()) {
            Some(h) => ohost.eq_ignore_ascii_case(h.trim()),
            None => false,
        };
    }
    true
}

/// 依据策略与请求头，决定是否给 Cookie 加 `Secure`。
pub fn cookie_secure_for(mode: CookieSecureMode, headers: &HeaderMap) -> bool {
    match mode {
        CookieSecureMode::Always => true,
        CookieSecureMode::Never => false,
        CookieSecureMode::Auto => headers
            .get("x-forwarded-proto")
            .and_then(|v| v.to_str().ok())
            .map(|v| v.trim().eq_ignore_ascii_case("https"))
            .unwrap_or(false),
    }
}

/// 生成 `Set-Cookie` 头值（会话建立）。
pub fn set_cookie_header(sid: &str, max_age: i64, secure: bool) -> String {
    let mut s =
        format!("{COOKIE_NAME}={sid}; HttpOnly; SameSite=Strict; Path=/; Max-Age={max_age}");
    if secure {
        s.push_str("; Secure");
    }
    s
}

/// 生成清除会话的 `Set-Cookie` 头值。
pub fn clear_cookie_header(secure: bool) -> String {
    let mut s = format!("{COOKIE_NAME}=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0");
    if secure {
        s.push_str("; Secure");
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::{AdminPolicy, AdminState, CookieSecureMode, EnvSnapshot};
    use crate::store::Store;
    use std::sync::Arc;

    fn state() -> Arc<AdminState> {
        let store = Store::open_in_memory().unwrap();
        let policy = Arc::new(AdminPolicy {
            password: "pw".into(),
            generated: false,
            cookie_secure: CookieSecureMode::Auto,
            allow_file_delete: false,
        });
        let env = EnvSnapshot {
            public_addr: "0.0.0.0:6800".into(),
            admin_addr: "127.0.0.1:8080".into(),
            db_path: "x".into(),
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
        // 测试用固定主密钥：与生产无关，只为把 AdminState 构造完整。
        let secrets = std::sync::Arc::new(crate::secret::SecretBox::with_key(
            "unit-test-secret-key-0123456789",
        ));
        AdminState::new(store, 0, policy, env, secrets, None)
    }

    #[test]
    fn session_ttl_and_sliding_renewal() {
        let s = SessionStore::new(1800);
        let sid = s.create(1000);
        assert!(s.touch(&sid, 1500).is_some(), "未过期应有效");
        // 滑动续期：从 1500 起再等 1700 秒仍有效
        assert!(s.touch(&sid, 3200).is_some(), "滑动续期后应有效");
        // 超过空闲 TTL 失效
        assert!(s.touch(&sid, 3200 + 1801).is_none(), "空闲超时应失效");
    }

    #[test]
    fn revoke_invalidates() {
        let s = SessionStore::new(1800);
        let sid = s.create(0);
        s.revoke(&sid);
        assert!(s.touch(&sid, 10).is_none());
    }

    #[test]
    fn guard_read_ignores_csrf() {
        let st = state();
        let sid = st.sessions.create(ids::now_secs());
        let mut h = HeaderMap::new();
        h.insert(header::COOKIE, format!("sid={sid}").parse().unwrap());
        h.insert(header::ORIGIN, "http://evil.example".parse().unwrap());
        // 读操作不查 CSRF；且应回传续期后的过期时刻（供重发 Cookie）
        assert_eq!(guard_read(&st, &h).0, Guard::Ok);
        assert!(guard_read(&st, &h).1.is_some(), "活跃读应返回新过期时刻");
    }

    #[test]
    fn guard_write_rejects_cross_origin() {
        let st = state();
        let sid = st.sessions.create(ids::now_secs());
        let mut h = HeaderMap::new();
        h.insert(header::COOKIE, format!("sid={sid}").parse().unwrap());
        h.insert(header::HOST, "127.0.0.1:8080".parse().unwrap());
        h.insert(header::ORIGIN, "http://evil.example".parse().unwrap());
        assert_eq!(guard_write(&st, &h).0, Guard::CsrfInvalid);
    }

    #[test]
    fn guard_write_accepts_same_origin() {
        let st = state();
        let sid = st.sessions.create(ids::now_secs());
        let mut h = HeaderMap::new();
        h.insert(header::COOKIE, format!("sid={sid}").parse().unwrap());
        h.insert(header::HOST, "127.0.0.1:8080".parse().unwrap());
        h.insert(header::ORIGIN, "http://127.0.0.1:8080".parse().unwrap());
        assert_eq!(guard_write(&st, &h).0, Guard::Ok);
    }

    #[test]
    fn five_failures_lock_then_success_clears() {
        let g = LoginGuard::new();
        let ip: IpAddr = "203.0.113.7".parse().unwrap();
        for _ in 0..4 {
            g.record_failure(ip, 1000);
            assert!(g.locked_until(ip, 1000).is_none(), "不足 5 次不应锁定");
        }
        assert_eq!(g.record_failure(ip, 1000), 5);
        assert_eq!(g.locked_until(ip, 1000), Some(1000 + LOCK_SECS));
        // 锁定窗口后自动解锁
        assert!(g.locked_until(ip, 1000 + LOCK_SECS + 1).is_none());

        g.record_failure(ip, 2000);
        g.record_success(ip);
        assert!(g.locked_until(ip, 2000).is_none(), "成功后应清零");
    }

    #[test]
    fn cookie_secure_follows_mode_and_xfp() {
        let mut h = HeaderMap::new();
        assert!(!cookie_secure_for(CookieSecureMode::Auto, &h));
        h.insert("x-forwarded-proto", "https".parse().unwrap());
        assert!(cookie_secure_for(CookieSecureMode::Auto, &h));
        assert!(cookie_secure_for(
            CookieSecureMode::Always,
            &HeaderMap::new()
        ));
        assert!(!cookie_secure_for(CookieSecureMode::Never, &h));
    }

    #[test]
    fn set_cookie_has_expected_attributes() {
        let c = set_cookie_header("abc", 1800, false);
        assert!(c.contains("sid=abc"));
        assert!(c.contains("HttpOnly"));
        assert!(c.contains("SameSite=Strict"));
        assert!(c.contains("Path=/"));
        assert!(c.contains("Max-Age=1800"));
        assert!(!c.contains("Secure"), "明文 HTTP 下不得带 Secure");
    }
}
