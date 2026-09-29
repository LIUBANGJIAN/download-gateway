//! 代理自签三段式握手（`ip_verify` / `login` / `device_token/get`）。
//!
//! 为什么要实现：真实 BitComet 插件在**探测阶段**就会打这三条，不实现 ⇒ 真客户端
//! 走不到 `/api/task/http/add`，兼容面用真客户端**测不了**。它是入口，不是锦上添花。
//!
//! # 攻击面（如实写明）
//!
//! `client_id` 由客户端自报、**且同时是解密口令** ⇒ 任何人用自造的 `client_id` 加密任意明文
//! 都能通过 RNCryptor 解密。**「解密成功」零身份证明**，该握手在语义上等价于「公开可握手」。
//! 真正承载凭据的是**密文里的 `password` 字段**。因此 `verify_login_credentials` 的第 5 步
//! （`DISPATCH_PUBLIC_TOKEN` 已配置时必须 `password == token`）是**承重墙**：
//! 缺了它，「配置了 token」的安全承诺会被 device_token 签发链路整体绕过。
//!
//! token **未配置**时放行是**有意的内网可用性取舍**（等价于任务路由默认不校验），
//! 不构成越权；但启动日志必须显式 `warn!` 告知「6800 无鉴权」。

use std::collections::{HashMap, VecDeque};
use std::net::IpAddr;
use std::sync::{Arc, Mutex};

use axum::body::Bytes;
use axum::extract::{ConnectInfo, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::Response;
use serde_json::Value;

use crate::ids;
use crate::ingress::envelope::{self, EC_INVALID_REQUEST, EC_INVALID_TOKEN};
use crate::state::PublicState;

/// 握手请求体上限（**64 KiB 显式闸门**，独立于 Router 的 8 MiB 体积上限）。
pub const HANDSHAKE_BODY_LIMIT: usize = 64 * 1024;
/// invite_token 有效期（秒）。
pub const INVITE_TTL_SECS: i64 = 120;
/// device_token 表软上限（满了淘汰最旧，防内存被刷）。
pub const DEVICE_CAP: usize = 128;
/// `/api/webui/login` 按来源 IP 的限速阈值（次/分钟，超限 429）。
pub const LOGIN_RATE_PER_MIN: usize = 30;

/// `/api/webui/login` 请求体。
#[derive(serde::Deserialize)]
pub struct LoginReq {
    /// 客户端自报标识（同时是解密口令）。
    pub client_id: String,
    /// Base64 的 RNCryptor v3 密文（含 `username`/`password`）。
    #[serde(default)]
    pub authentication: Option<String>,
    /// 免密码标记；我们**不支持** `bypass`，传 `true` 直接拒。
    #[serde(default)]
    pub bypass: Option<bool>,
}

/// `/api/device_token/get` 请求体。
#[derive(serde::Deserialize)]
pub struct DeviceTokenReq {
    /// 一次性 invite 令牌。
    #[serde(default)]
    pub invite_token: Option<String>,
    /// 设备标识。
    #[serde(default)]
    pub device_id: Option<String>,
    /// 设备名。
    #[serde(default)]
    pub device_name: Option<String>,
    /// 平台。
    #[serde(default)]
    pub platform: Option<String>,
}

/// 握手失败原因（纯函数输出，便于单测）。
#[derive(Debug, PartialEq, Eq)]
pub enum LoginFail {
    /// 密文解密失败（口令错 / 密文损坏）。
    Decrypt,
    /// 明文不是合法 JSON 或缺 `password` 字段。
    Shape,
    /// `password` 与已配置的 `DISPATCH_PUBLIC_TOKEN` 不符。
    CredentialMismatch,
}

struct Invite {
    device_id: String,
    expires_at: i64,
}

#[derive(Default)]
struct Inner {
    /// invite_token → 绑定信息（一次性）。
    invites: HashMap<String, Invite>,
    /// device_token → device_id。
    devices: HashMap<String, String>,
    /// device_token 的插入顺序（淘汰最旧用）。
    device_order: VecDeque<String>,
    /// 每个来源 IP 在最近一分钟内的登录时刻。
    login_hits: HashMap<IpAddr, VecDeque<i64>>,
}

/// 握手令牌表（内存，重启失效）。
///
/// 不引 master key：令牌是 128 位随机不透明串、内存查表校验，不需要 HMAC 签发。
pub struct HandshakeStore {
    inner: Mutex<Inner>,
}

impl Default for HandshakeStore {
    fn default() -> Self {
        Self::new()
    }
}

impl HandshakeStore {
    /// 空表。
    pub fn new() -> Self {
        Self {
            inner: Mutex::new(Inner::default()),
        }
    }

    /// 签发一次性 invite_token（TTL 120s）。顺带清理过期 invite。
    pub fn issue_invite(&self, device_id: &str, now: i64) -> String {
        let mut inner = self.inner.lock().expect("握手表锁中毒");
        inner.invites.retain(|_, v| v.expires_at > now);
        let token = ids::new_hex_128();
        inner.invites.insert(
            token.clone(),
            Invite {
                device_id: device_id.to_string(),
                expires_at: now + INVITE_TTL_SECS,
            },
        );
        token
    }

    /// 兑换 invite_token（**一次性**：命中即删）；过期或不存在返回 `None`。
    pub fn redeem_invite(&self, invite: &str, now: i64) -> Option<String> {
        let mut inner = self.inner.lock().expect("握手表锁中毒");
        let inv = inner.invites.remove(invite)?;
        if inv.expires_at > now {
            Some(inv.device_id)
        } else {
            None
        }
    }

    /// 签发 device_token（无 TTL）。超过软上限时淘汰最旧，保证表大小 ≤ [`DEVICE_CAP`]。
    pub fn issue_device(&self, device_id: &str) -> String {
        let mut inner = self.inner.lock().expect("握手表锁中毒");
        while inner.devices.len() >= DEVICE_CAP {
            match inner.device_order.pop_front() {
                Some(old) => {
                    inner.devices.remove(&old);
                }
                None => break,
            }
        }
        let token = ids::new_hex_128();
        inner.devices.insert(token.clone(), device_id.to_string());
        inner.device_order.push_back(token.clone());
        token
    }

    /// 是否是本进程签发的 device_token。
    pub fn is_device(&self, token: &str) -> bool {
        let inner = self.inner.lock().expect("握手表锁中毒");
        inner.devices.contains_key(token)
    }

    /// 登录限速：记录本次并返回是否**允许**（最近一分钟内 > [`LOGIN_RATE_PER_MIN`] 次则拒绝）。
    pub fn check_login_rate(&self, ip: IpAddr, now: i64) -> bool {
        let mut inner = self.inner.lock().expect("握手表锁中毒");
        let window_start = now - 60;
        let hits = inner.login_hits.entry(ip).or_default();
        while let Some(front) = hits.front() {
            if *front < window_start {
                hits.pop_front();
            } else {
                break;
            }
        }
        hits.push_back(now);
        hits.len() <= LOGIN_RATE_PER_MIN
    }

    /// 当前 device 表大小（供测试与自检）。
    pub fn device_count(&self) -> usize {
        self.inner.lock().expect("握手表锁中毒").devices.len()
    }
}

/// 校验登录凭据（判据见模块文档第 5 步）。
///
/// 成功返回解出的 `username`（仅用于日志），**绝不**返回密文或口令。
pub fn verify_login_credentials(
    st: &PublicState,
    client_id: &str,
    authentication: &str,
) -> Result<String, LoginFail> {
    let plain = bitcomet_api::rncryptor::decrypt(authentication, client_id)
        .map_err(|_| LoginFail::Decrypt)?;
    let v: Value = serde_json::from_slice(&plain).map_err(|_| LoginFail::Shape)?;
    let pw = v
        .get("password")
        .and_then(Value::as_str)
        .ok_or(LoginFail::Shape)?;
    let username = v
        .get("username")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string();

    // ★承重墙：token 已配置时，密文里的 password 必须等于它，否则授权链被整体绕过。
    match &st.public_token {
        Some(expected) if !envelope::ct_eq(pw, expected) => Err(LoginFail::CredentialMismatch),
        _ => Ok(username),
    }
}

/// 第 1 段（可选）。恒返回 `bypass_eligible:false` —— 我们从不实现免密码，令客户端回落口令登录。
pub async fn ip_verify(
    State(_st): State<Arc<PublicState>>,
    _ci: ConnectInfo<std::net::SocketAddr>,
    _headers: HeaderMap,
    body: Bytes,
) -> Response {
    if body.len() > HANDSHAKE_BODY_LIMIT {
        return envelope::bitcomet_err(
            StatusCode::PAYLOAD_TOO_LARGE,
            EC_INVALID_REQUEST,
            "请求体过大",
        );
    }
    envelope::bitcomet_ok_resp(serde_json::json!({ "bypass_eligible": false }))
}

/// 第 2 段：口令登录，换取一次性 `invite_token`。
pub async fn login(
    State(st): State<Arc<PublicState>>,
    ConnectInfo(addr): ConnectInfo<std::net::SocketAddr>,
    _headers: HeaderMap,
    body: Bytes,
) -> Response {
    let now = ids::now_secs();
    if body.len() > HANDSHAKE_BODY_LIMIT {
        return envelope::bitcomet_err(
            StatusCode::PAYLOAD_TOO_LARGE,
            EC_INVALID_REQUEST,
            "请求体过大",
        );
    }
    // 限速（复用握手表内的按 IP 计数；6800 是公开口，登录做 PBKDF2×2，是 CPU 放大面）。
    if !st.handshake.tokens.check_login_rate(addr.ip(), now) {
        tracing::warn!(ip = %addr.ip(), "握手登录触发限速（>30 次/分）");
        return envelope::bitcomet_err(
            StatusCode::TOO_MANY_REQUESTS,
            EC_INVALID_REQUEST,
            "登录尝试过于频繁",
        );
    }

    let req: LoginReq = match serde_json::from_slice(&body) {
        Ok(v) => v,
        Err(e) => {
            return envelope::bitcomet_err(
                StatusCode::BAD_REQUEST,
                EC_INVALID_REQUEST,
                &format!("请求体非法: {e}"),
            );
        }
    };
    if req.bypass == Some(true) {
        return envelope::bitcomet_err(
            StatusCode::BAD_REQUEST,
            EC_INVALID_REQUEST,
            "不支持 bypass 免密码登录",
        );
    }
    let Some(authentication) = req.authentication.as_deref() else {
        return envelope::bitcomet_err(StatusCode::UNAUTHORIZED, EC_INVALID_TOKEN, "Invalid token");
    };

    match verify_login_credentials(&st, &req.client_id, authentication) {
        Ok(username) => {
            let invite = st.handshake.tokens.issue_invite(&req.client_id, now);
            let prefix = client_id_prefix(&req.client_id);
            tracing::info!(
                client_id_prefix = %prefix,
                auth_required = st.public_token.is_some(),
                username_len = username.len(),
                "handshake_login_ok"
            );
            log_security(
                &st,
                "info",
                "handshake_login_ok",
                &serde_json::json!({
                    "auth_required": st.public_token.is_some(),
                    "client_id_prefix": prefix,
                    "username_len": username.len(),
                })
                .to_string(),
                now,
            )
            .await;
            envelope::bitcomet_ok_resp(serde_json::json!({ "invite_token": invite }))
        }
        Err(fail) => {
            let reason = match fail {
                LoginFail::Decrypt => "decrypt_failed",
                LoginFail::Shape => "shape_invalid",
                LoginFail::CredentialMismatch => "credential_mismatch",
            };
            let prefix = client_id_prefix(&req.client_id);
            tracing::warn!(client_id_prefix = %prefix, reason = %reason, "handshake_login_failed");
            log_security(
                &st,
                "warn",
                "handshake_login_failed",
                &serde_json::json!({
                    "stage": "login",
                    "reason": reason,
                    "client_id_prefix": prefix,
                })
                .to_string(),
                now,
            )
            .await;
            // 失败一律 HTTP 401 + INVALID_TOKEN（保留 architecture 回退方案：
            // 若将来需区分「配置缺失」与「凭据不符」，可在此按 fail 变体返回不同 error_code）。
            envelope::bitcomet_err(StatusCode::UNAUTHORIZED, EC_INVALID_TOKEN, "Invalid token")
        }
    }
}

/// 第 3 段：`Bearer <invite_token>` + `body.invite_token` 双校验，换取 `device_token`。
pub async fn device_token_get(
    State(st): State<Arc<PublicState>>,
    _ci: ConnectInfo<std::net::SocketAddr>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    let now = ids::now_secs();
    if body.len() > HANDSHAKE_BODY_LIMIT {
        return envelope::bitcomet_err(
            StatusCode::PAYLOAD_TOO_LARGE,
            EC_INVALID_REQUEST,
            "请求体过大",
        );
    }
    let req: DeviceTokenReq = match serde_json::from_slice(&body) {
        Ok(v) => v,
        Err(e) => {
            return envelope::bitcomet_err(
                StatusCode::BAD_REQUEST,
                EC_INVALID_REQUEST,
                &format!("请求体非法: {e}"),
            );
        }
    };

    let had_bearer = envelope::bearer_of(&headers).is_some();
    let body_invite = req.invite_token.as_deref();

    // 若同时给了 Bearer 与 body.invite_token，二者必须一致。
    if let (Some(b), Some(bt)) = (envelope::bearer_of(&headers), body_invite)
        && b != bt
    {
        log_security(
            &st,
            "warn",
            "invite_rejected",
            &serde_json::json!({ "had_bearer": had_bearer, "body_has_invite": true }).to_string(),
            now,
        )
        .await;
        return envelope::bitcomet_err(StatusCode::UNAUTHORIZED, EC_INVALID_TOKEN, "Invalid token");
    }

    let invite = envelope::bearer_of(&headers).or(body_invite);
    match invite {
        Some(tok) => match st.handshake.tokens.redeem_invite(tok, now) {
            Some(device_id) => {
                let token = st.handshake.tokens.issue_device(&device_id);
                tracing::info!(
                    device_id_prefix = %client_id_prefix(&device_id),
                    "device_token_issued"
                );
                log_security(
                    &st,
                    "info",
                    "device_token_issued",
                    &serde_json::json!({
                        "client_id_prefix": client_id_prefix(&device_id),
                        "device_id_prefix": client_id_prefix(&device_id),
                    })
                    .to_string(),
                    now,
                )
                .await;
                envelope::bitcomet_ok_resp(serde_json::json!({ "device_token": token }))
            }
            None => {
                log_security(
                    &st,
                    "warn",
                    "invite_rejected",
                    &serde_json::json!({
                        "had_bearer": had_bearer,
                        "body_has_invite": body_invite.is_some(),
                    })
                    .to_string(),
                    now,
                )
                .await;
                envelope::bitcomet_err(StatusCode::UNAUTHORIZED, EC_INVALID_TOKEN, "Invalid token")
            }
        },
        None => envelope::bitcomet_err(StatusCode::UNAUTHORIZED, EC_INVALID_TOKEN, "Invalid token"),
    }
}

/// 日志中只暴露 `client_id` 的**前 8 字符**（它是标识非凭据，但仍做最小暴露）。
fn client_id_prefix(client_id: &str) -> String {
    client_id.chars().take(8).collect()
}

/// 把安全事件写入 `event_log`（best-effort；失败只 `warn!`，不影响握手结果）。
///
/// 硬规则：`detail` 与 `tracing` **一律不含**密码 / invite_token / device_token /
/// `authentication` 密文；`client_id` 只记前 8 字符。
async fn log_security(st: &PublicState, level: &str, message: &str, detail: &str, now: i64) {
    if let Err(e) = st
        .store
        .execute(
            "INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at) \
             VALUES (?1, 'security', NULL, NULL, ?2, ?3, ?4)",
            vec![
                rusqlite::types::Value::Text(level.to_string()),
                rusqlite::types::Value::Text(message.to_string()),
                rusqlite::types::Value::Text(detail.to_string()),
                rusqlite::types::Value::Integer(now),
            ],
        )
        .await
    {
        tracing::warn!(error = %e, "写安全审计 event_log 失败（不影响握手结果）");
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::Store;

    fn dummy_state(token: Option<&str>) -> Arc<PublicState> {
        let store = Store::open_in_memory().unwrap();
        let hs = crate::state::HandshakeState {
            proxy_client_id: "test-client".into(),
            tokens: HandshakeStore::new(),
        };
        PublicState::new(store, ids::now_secs(), token.map(str::to_string), false, hs)
    }

    #[test]
    fn invite_is_one_time_and_expires() {
        let t = HandshakeStore::new();
        let invite = t.issue_invite("dev-1", 1000);
        assert_eq!(t.redeem_invite(&invite, 1000).as_deref(), Some("dev-1"));
        // 一次性：第二次兑换失败
        assert_eq!(t.redeem_invite(&invite, 1000), None);

        // 过期：TTL 之外不可兑换
        let invite2 = t.issue_invite("dev-2", 2000);
        assert_eq!(t.redeem_invite(&invite2, 2000 + INVITE_TTL_SECS + 1), None);
    }

    #[test]
    fn device_table_is_capped_and_evicts_oldest() {
        let t = HandshakeStore::new();
        let mut tokens = Vec::new();
        for i in 0..(DEVICE_CAP + 10) {
            tokens.push(t.issue_device(&format!("dev-{i}")));
        }
        assert_eq!(t.device_count(), DEVICE_CAP, "device 表必须被软上限约束");
        // 最旧的 10 个应已被淘汰
        for old in tokens.iter().take(10) {
            assert!(!t.is_device(old), "最旧的 device_token 应被淘汰");
        }
        // 最新的仍在
        assert!(t.is_device(tokens.last().unwrap()));
    }

    #[test]
    fn login_rate_limit_allows_30_then_denies() {
        let t = HandshakeStore::new();
        let ip: IpAddr = "10.0.0.1".parse().unwrap();
        for _ in 0..LOGIN_RATE_PER_MIN {
            assert!(t.check_login_rate(ip, 1000), "第 30 次以内应放行");
        }
        assert!(!t.check_login_rate(ip, 1000), "第 31 次应被拒");
        // 换一个 IP 不受影响
        let ip2: IpAddr = "10.0.0.2".parse().unwrap();
        assert!(t.check_login_rate(ip2, 1000));
        // 窗口滑动后恢复
        assert!(t.check_login_rate(ip, 1000 + 61));
    }

    #[test]
    fn verify_credentials_paths() {
        // 未配置 token → 解密成功即放行
        let st = dummy_state(None);
        let ct = bitcomet_api::rncryptor::encrypt(
            br#"{"username":"plugin","password":"whatever"}"#,
            "cid-1",
        )
        .unwrap();
        assert_eq!(
            verify_login_credentials(&st, "cid-1", &ct).as_deref(),
            Ok("plugin")
        );

        // 配置了 token 且口令匹配 → 放行
        let st2 = dummy_state(Some("secret"));
        let ct2 = bitcomet_api::rncryptor::encrypt(
            br#"{"username":"plugin","password":"secret"}"#,
            "cid-2",
        )
        .unwrap();
        assert!(verify_login_credentials(&st2, "cid-2", &ct2).is_ok());

        // 配置了 token 但口令不符 → CredentialMismatch（承重墙）
        let ct3 = bitcomet_api::rncryptor::encrypt(
            br#"{"username":"plugin","password":"wrong"}"#,
            "cid-3",
        )
        .unwrap();
        assert_eq!(
            verify_login_credentials(&st2, "cid-3", &ct3),
            Err(LoginFail::CredentialMismatch)
        );

        // 解密失败
        assert_eq!(
            verify_login_credentials(&st2, "cid-4", "not-base64!!!"),
            Err(LoginFail::Decrypt)
        );

        // 明文缺 password
        let ct5 = bitcomet_api::rncryptor::encrypt(br#"{"username":"x"}"#, "cid-5").unwrap();
        assert_eq!(
            verify_login_credentials(&st2, "cid-5", &ct5),
            Err(LoginFail::Shape)
        );
    }
}
