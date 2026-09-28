//! BitComet WebUI API 客户端（**异步**）。
//!
//! 从参考实现 `bitcomet_core::client` 抽取并异步化：
//! `reqwest::blocking` → `reqwest`（async）；`.no_proxy()`（F11）**保留**。
//!
//! # 三段式认证（依据官方 WebUI API 文档）
//!
//! ```text
//! 生成并持久化 client_id（UUID）
//!   → POST /api/webui/login          { client_id, authentication }
//!         authentication = Base64( RNCryptor_v3( {"username","password"}, 口令 = client_id ) )
//!   → 得到 invite_token
//!   → POST /api/device_token/get     Header: Authorization: Bearer <invite_token>
//!                                    Body: { invite_token, device_id, device_name, platform }
//!   → 得到 device_token
//!   → 后续所有 API 使用 Header: Authorization: Bearer <device_token>
//! ```
//!
//! # 两条实现纪律
//!
//! 1. **401 只自动重登一次**。官方文档明确「收到 401 后应清除本地 Token 并重新登录，
//!    **不应无限重试**」——否则令牌失效时会变成对节点的 DDoS。
//! 2. **成功值不可硬编码**。历史接口的成功值存在 `OK` / `ok` 等大小写差异，
//!    故用 [`is_ok_code`] 统一判定，而不是 `== "OK"`。

use std::time::Duration;

use serde_json::{Map, Value};

use crate::clientid;
use crate::profile::NodeProfile;
use crate::rncryptor;

/// 官方约定的客户端标识头。
pub const CLIENT_TYPE: &str = "BitComet WebUI";

/// 客户端错误。
#[derive(Debug, thiserror::Error)]
pub enum ClientError {
    /// 传输层错误（连接失败、超时、TLS 等）。
    #[error("HTTP 请求失败: {0}")]
    Http(#[from] reqwest::Error),
    /// JSON 序列化/反序列化错误。
    #[error("JSON 处理失败: {0}")]
    Json(#[from] serde_json::Error),
    /// RNCryptor 加解密错误。
    #[error("RNCryptor 失败: {0}")]
    Crypt(#[from] rncryptor::CryptorError),
    /// client_id 文件读写错误。
    #[error("client_id 读写失败: {0}")]
    Io(#[from] std::io::Error),
    /// 登录接口返回非 2xx。
    #[error("登录失败（HTTP {status}）: {detail}")]
    Login {
        /// HTTP 状态码。
        status: u16,
        /// 截断后的响应正文，便于排障（不含凭据——正文是服务端返回的）。
        detail: String,
    },
    /// 响应里缺少必需字段。
    #[error("响应缺少字段 `{0}`")]
    MissingField(&'static str),
    /// 节点业务错误（`error_code` 非成功值）。
    #[error("节点返回错误 error_code={code}: {message}")]
    Remote {
        /// 节点返回的 `error_code`。
        code: String,
        /// 节点返回的 `error_message`。
        message: String,
    },
    /// 401 且重登一次后仍然 401。
    #[error("设备 Token 被拒（已重登一次仍失败）")]
    TokenRejected,
    /// 内部：尚未登录就调用受保护接口。
    #[error("尚未登录：请先调用 login()")]
    NotLoggedIn,
    /// 供内部重试分支使用的 401 标记（不对外暴露语义）。
    #[error("unauthorized")]
    Unauthorized,
}

/// 统一结果类型。
pub type Result<T> = std::result::Result<T, ClientError>;

/// 判定 BitComet 的「成功」`error_code`。
///
/// 官方文档提醒历史接口存在 `OK` / `ok` 的大小写差异，
/// 因此**不要**写 `code == "OK"`。
pub fn is_ok_code(code: &str) -> bool {
    let t = code.trim();
    t.is_empty() || t.eq_ignore_ascii_case("ok")
}

/// 从响应里提取 `error_code` / `error_message`，非成功即返回 [`ClientError::Remote`]。
fn check_error_code(v: &Value) -> Result<()> {
    let Some(code) = v.get("error_code").and_then(Value::as_str) else {
        // 没有 error_code 字段的响应（如部分历史接口）按成功处理，
        // 由调用方按需校验业务字段。
        return Ok(());
    };
    if is_ok_code(code) {
        return Ok(());
    }
    let message = v
        .get("error_message")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();
    Err(ClientError::Remote {
        code: code.to_string(),
        message,
    })
}

fn short(text: &str) -> String {
    const MAX: usize = 300;
    if text.chars().count() <= MAX {
        return text.to_string();
    }
    let mut s: String = text.chars().take(MAX).collect();
    s.push('…');
    s
}

/// BitComet WebUI 客户端。
pub struct Client {
    http: reqwest::Client,
    base_url: String,
    profile: NodeProfile,
    client_id: String,
    device_token: Option<String>,
}

impl Client {
    /// 构造客户端并从磁盘加载/生成 `client_id`（**不**发网络请求）。
    ///
    /// `timeout_secs` 同时用于连接与整体请求超时。
    pub fn new(profile: NodeProfile, timeout_secs: u64) -> Result<Self> {
        let http = reqwest::Client::builder()
            // F11：节点在内网，绝不能被系统代理劫持
            .no_proxy()
            .connect_timeout(Duration::from_secs(timeout_secs.clamp(1, 30)))
            .timeout(Duration::from_secs(timeout_secs.max(1)))
            .user_agent(crate::user_agent())
            .build()?;

        let path = profile
            .client_id_path
            .clone()
            .unwrap_or_else(clientid::default_client_id_path);
        let client_id = clientid::load_or_create_client_id(&path)?;

        Ok(Self {
            http,
            base_url: profile.normalized_base(),
            profile,
            client_id,
            device_token: None,
        })
    }

    /// 当前使用的 `client_id`。
    pub fn client_id(&self) -> &str {
        &self.client_id
    }

    /// 当前节点 base URL（已规范化）。
    pub fn base_url(&self) -> &str {
        &self.base_url
    }

    /// 是否已持有 device_token。
    pub fn has_token(&self) -> bool {
        self.device_token.is_some()
    }

    /// 节点连接参数。
    pub fn profile(&self) -> &NodeProfile {
        &self.profile
    }

    fn url(&self, path: &str) -> String {
        format!("{}{}", self.base_url, path)
    }

    /// 执行**完整**三段式登录，成功后就绪可用。
    pub async fn login(&mut self) -> Result<()> {
        // ── 第 1 段：用 client_id 作口令加密凭据，换取 invite_token ──
        let plaintext = serde_json::json!({
            "username": self.profile.username,
            "password": self.profile.password,
        })
        .to_string();
        let authentication = rncryptor::encrypt(plaintext.as_bytes(), &self.client_id)?;

        let body = serde_json::json!({
            "client_id": self.client_id,
            "authentication": authentication,
        });
        let resp = self
            .http
            .post(self.url("/api/webui/login"))
            .header("Client-Type", CLIENT_TYPE)
            .json(&body)
            .send()
            .await?;
        let status = resp.status();
        let text = resp.text().await?;
        if !status.is_success() {
            return Err(ClientError::Login {
                status: status.as_u16(),
                detail: short(&text),
            });
        }
        let v: Value = serde_json::from_str(&text).unwrap_or(Value::Null);
        check_error_code(&v)?;
        let invite_token = v
            .get("invite_token")
            .and_then(Value::as_str)
            .ok_or(ClientError::MissingField("invite_token"))?
            .to_string();

        // ── 第 2 段：用 invite_token 换 device_token ──
        let body = serde_json::json!({
            "invite_token": invite_token,
            "device_id": self.client_id,
            "device_name": self.profile.device_name,
            "platform": "webui",
        });
        let resp = self
            .http
            .post(self.url("/api/device_token/get"))
            .header("Client-Type", CLIENT_TYPE)
            .header("Authorization", format!("Bearer {invite_token}"))
            .json(&body)
            .send()
            .await?;
        let status = resp.status();
        let text = resp.text().await?;
        if !status.is_success() {
            return Err(ClientError::Login {
                status: status.as_u16(),
                detail: short(&text),
            });
        }
        let v: Value = serde_json::from_str(&text).unwrap_or(Value::Null);
        check_error_code(&v)?;
        let token = v
            .get("device_token")
            .and_then(Value::as_str)
            .ok_or(ClientError::MissingField("device_token"))?
            .to_string();

        self.device_token = Some(token);
        Ok(())
    }

    /// 若尚未登录则登录。
    pub async fn ensure_login(&mut self) -> Result<()> {
        if self.device_token.is_none() {
            self.login().await?;
        }
        Ok(())
    }

    /// 单次调用（不处理 401 重试）。
    async fn call_once(&mut self, path: &str, body: Value) -> Result<Value> {
        let token = self.device_token.clone().ok_or(ClientError::NotLoggedIn)?;
        let resp = self
            .http
            .post(self.url(path))
            .header("Client-Type", CLIENT_TYPE)
            .header("Authorization", format!("Bearer {token}"))
            .json(&body)
            .send()
            .await?;

        let status = resp.status();
        if status.as_u16() == 401 {
            return Err(ClientError::Unauthorized);
        }
        let text = resp.text().await?;
        let v: Value = serde_json::from_str(&text).unwrap_or(Value::Null);
        if !status.is_success() {
            return Err(ClientError::Remote {
                code: format!("HTTP_{}", status.as_u16()),
                message: short(&text),
            });
        }
        check_error_code(&v)?;
        Ok(v)
    }

    /// 调用受保护接口，**401 时自动重登一次**（不无限重试）。
    pub async fn call(&mut self, path: &str, body: Value) -> Result<Value> {
        match self.call_once(path, body.clone()).await {
            Err(ClientError::Unauthorized) => {
                self.device_token = None;
                self.login().await?;
                match self.call_once(path, body).await {
                    Err(ClientError::Unauthorized) => Err(ClientError::TokenRejected),
                    other => other,
                }
            }
            other => other,
        }
    }

    /// 拉取任务列表（返回原始 JSON，字段建模属后续任务）。
    ///
    /// 端点为 `POST /api_v2/task_list/get`，无参数时发 `{}`。
    pub async fn fetch_task_list(&mut self) -> Result<Value> {
        self.ensure_login().await?;
        self.call("/api_v2/task_list/get", serde_json::json!({}))
            .await
    }

    /// 添加 HTTP/FTP 任务，返回节点响应（含 `task_id`）。
    ///
    /// ⚠️ `save_folder` 默认**不下发**（`02 §2.2.2`：保持节点原下载目录）。
    /// 仅当调用方显式传入时才带上。
    pub async fn add_http_task(
        &mut self,
        url: &str,
        save_folder: Option<&str>,
        start_later: bool,
    ) -> Result<Value> {
        self.ensure_login().await?;
        let mut m = Map::new();
        m.insert("url".to_string(), Value::String(url.to_string()));
        m.insert("start_later".to_string(), Value::Bool(start_later));
        if let Some(f) = save_folder {
            m.insert("save_folder".to_string(), Value::String(f.to_string()));
        }
        self.call("/api/task/http/add", Value::Object(m)).await
    }

    /// 添加磁力/种子链接（可多行），返回节点响应。
    pub async fn add_torrent_links(
        &mut self,
        links: &str,
        save_folder: Option<&str>,
        start_later: bool,
    ) -> Result<Value> {
        self.ensure_login().await?;
        let mut m = Map::new();
        m.insert(
            "torrent_links".to_string(),
            Value::String(links.to_string()),
        );
        m.insert("start_later".to_string(), Value::Bool(start_later));
        if let Some(f) = save_folder {
            m.insert("save_folder".to_string(), Value::String(f.to_string()));
        }
        self.call("/api/task/torrent_links/add", Value::Object(m))
            .await
    }

    /// 读取节点 about 信息（T03 的**带认证**健康探针用它：只看 `error_code` 与 `about_info`）。
    pub async fn about(&mut self) -> Result<Value> {
        self.ensure_login().await?;
        self.call("/api/config/about/get", serde_json::json!({}))
            .await
    }

    /// 读取新任务默认配置（`task_type` 取 `BT` 或 `HTTP`）。
    pub async fn new_task_config(&mut self, task_type: &str) -> Result<Value> {
        self.ensure_login().await?;
        self.call(
            "/api/config/new_task/get",
            serde_json::json!({ "task_type": task_type }),
        )
        .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ok_code_is_case_insensitive() {
        assert!(is_ok_code("OK"));
        assert!(is_ok_code("ok"));
        assert!(is_ok_code(""));
        assert!(is_ok_code("  ok  "));
        assert!(!is_ok_code("INVALID_TOKEN"));
        assert!(!is_ok_code("Error"));
    }

    #[test]
    fn check_error_code_accepts_missing_field() {
        let v = serde_json::json!({ "about_info": {} });
        assert!(
            check_error_code(&v).is_ok(),
            "无 error_code 字段应按成功处理"
        );
    }

    #[test]
    fn check_error_code_rejects_failure() {
        let v = serde_json::json!({
            "error_code": "INVALID_TOKEN",
            "error_message": "Invalid token",
        });
        match check_error_code(&v) {
            Err(ClientError::Remote { code, message }) => {
                assert_eq!(code, "INVALID_TOKEN");
                assert_eq!(message, "Invalid token");
            }
            other => panic!("应报 Remote 错误，实际 {other:?}"),
        }
    }

    #[test]
    fn client_is_constructible_without_network() {
        let dir = std::env::temp_dir().join(format!("dg-cli-{}", clientid::new_uuid()));
        let mut p = NodeProfile::new("http://node.invalid:9085/", "u", "p");
        p.client_id_path = Some(dir.join("client_id"));
        let c = Client::new(p, 5).expect("构造不应触网");
        assert!(!c.client_id().is_empty(), "client_id 必须非空");
        assert_eq!(c.base_url(), "http://node.invalid:9085");
        assert!(!c.has_token(), "构造后尚无 token");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn url_join_has_no_double_slash() {
        let dir = std::env::temp_dir().join(format!("dg-url-{}", clientid::new_uuid()));
        let mut p = NodeProfile::new("http://node.invalid:9085///", "u", "p");
        p.client_id_path = Some(dir.join("client_id"));
        let c = Client::new(p, 5).unwrap();
        assert_eq!(
            c.url("/api/webui/login"),
            "http://node.invalid:9085/api/webui/login"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// 未登录直接调用受保护接口，应报 `NotLoggedIn` 而不是静默发请求。
    #[tokio::test]
    async fn call_without_login_is_rejected_locally() {
        let dir = std::env::temp_dir().join(format!("dg-nologin-{}", clientid::new_uuid()));
        let mut p = NodeProfile::new("http://node.invalid:9085", "u", "p");
        p.client_id_path = Some(dir.join("client_id"));
        let mut c = Client::new(p, 5).unwrap();
        let r = c
            .call_once("/api_v2/task_list/get", serde_json::json!({}))
            .await;
        assert!(matches!(r, Err(ClientError::NotLoggedIn)), "实际 {r:?}");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// **真实节点联调**（默认 `#[ignore]`，CI 不跑 —— 它需要内网环境与凭据）。
    ///
    /// 环境变量（缺任一即打印说明并跳过，不会误报失败）：
    ///
    /// - `DISPATCH_TEST_NODE_URL`，如 `http://node.example:9085`
    /// - `DISPATCH_TEST_NODE_USER`
    /// - `DISPATCH_TEST_NODE_PASS`
    ///
    /// ```bash
    /// cargo test -p bitcomet-api -- --ignored --nocapture live_login
    /// ```
    ///
    /// 断言链路（即 T02 的验收标准）：
    /// `client_id` 持久化 → **三段式登录**（`/api/webui/login` → `/api/device_token/get`）
    /// → `POST /api_v2/task_list/get` 返回合法 JSON 对象。
    #[tokio::test]
    #[ignore = "需要真实 BitComet 节点与凭据（见测试文档注释）"]
    async fn live_login_and_fetch_task_list() {
        let (Ok(url), Ok(user), Ok(pass)) = (
            std::env::var("DISPATCH_TEST_NODE_URL"),
            std::env::var("DISPATCH_TEST_NODE_USER"),
            std::env::var("DISPATCH_TEST_NODE_PASS"),
        ) else {
            eprintln!("[跳过] 缺少 DISPATCH_TEST_NODE_URL / _USER / _PASS 环境变量");
            return;
        };

        let dir = std::env::temp_dir().join(format!("dg-live-{}", clientid::new_uuid()));
        let mut p = NodeProfile::new(url, user, pass);
        p.client_id_path = Some(dir.join("client_id"));

        let mut c = Client::new(p, 20).expect("构造客户端");
        println!("[1/3] client_id = {}", c.client_id());

        c.login().await.expect("三段式登录必须成功");
        assert!(c.has_token(), "登录后应持有 device_token");
        println!("[2/3] 三段式登录成功，已取得 device_token");

        let list = c.fetch_task_list().await.expect("任务列表拉取必须成功");
        let keys: Vec<&String> = list
            .as_object()
            .map(|o| o.keys().collect())
            .unwrap_or_default();
        println!("[3/3] /api_v2/task_list/get 顶层键 = {keys:?}");
        assert!(list.is_object(), "任务列表响应应为 JSON 对象");

        // 顺带验证带认证的健康探针端点（T03 会用它）
        match c.about().await {
            Ok(v) => println!(
                "       /api/config/about/get 顶层键 = {:?}",
                v.as_object().map(|o| o.keys().collect::<Vec<_>>())
            ),
            Err(e) => println!("       about 探测未通过（不影响本测试结论）: {e}"),
        }

        let _ = std::fs::remove_dir_all(&dir);
    }
}
