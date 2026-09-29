//! 对外口协议共享件：入站鉴权（`AuthPolicy`/`authorize`/`bearer_of`/`token_param_of`）、
//! BitComet 统一信封、错误码常量。
//!
//! 收敛到一个文件的原因（主理人合并裁决）：鉴权判定表只有一份实现，任何受保护路由
//! 都调 `authorize`，**不得各自内联一遍**；信封与错误码同处，避免多处分叉。

use axum::Json;
use axum::http::{HeaderMap, StatusCode, header};
use axum::response::{IntoResponse, Response};
use serde_json::{Map, Value};

use crate::state::PublicState;

/// BitComet 成功 `error_code`。
pub const EC_OK: &str = "OK";
/// BitComet 鉴权失败 `error_code`。
pub const EC_INVALID_TOKEN: &str = "INVALID_TOKEN";
/// BitComet 请求非法 `error_code`。
pub const EC_INVALID_REQUEST: &str = "INVALID_REQUEST";
/// Aria2 鉴权失败错误码。
pub const ARIA2_UNAUTHORIZED: i64 = -1;

/// 代理对外口上报的 `platform` 值（区别于真实 BitComet 节点）。
pub const PLATFORM: &str = "proxy";

/// 入站鉴权结论。
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Authz {
    /// 放行。
    Allow,
    /// 拒绝（应回 401 `INVALID_TOKEN` 或 aria2 `-1`）。
    Deny,
}

/// 入站鉴权策略：封装「代理签发 device_token 优先 + 可选公共 token」两个来源。
#[derive(Clone, Copy)]
pub struct AuthPolicy<'a> {
    /// `DISPATCH_PUBLIC_TOKEN`；`None` = 不校验。
    pub public_token: Option<&'a str>,
}

impl<'a> AuthPolicy<'a> {
    /// 由公共 token 构造。
    pub fn new(public_token: Option<&'a str>) -> Self {
        Self { public_token }
    }

    /// 判定表（design-2 §(4)）逐行实现：
    ///
    /// | Bearer | 是代理 device_token | 已配置公共 token | 相等 | 结果 |
    /// |:-:|:-:|:-:|:-:|:--|
    /// | 有 | 是 | 任意 | — | Allow |
    /// | 有 | 否 | 是 | 是 | Allow |
    /// | 有 | 否 | 是 | 否/空 | Deny |
    /// | 无 | — | 是 | — | Deny |
    /// | 有 | 否 | 否 | — | Allow |
    /// | 无 | — | 否 | — | Allow |
    pub fn decide(&self, bearer: Option<&str>, device_token_valid: bool) -> Authz {
        // 行 1：本进程签发的 token 最高优先。
        if device_token_valid {
            return Authz::Allow;
        }
        match self.public_token {
            Some(expected) => match bearer {
                Some(t) if ct_eq(t, expected) => Authz::Allow, // 行 2
                _ => Authz::Deny,                              // 行 3 / 行 4
            },
            None => Authz::Allow, // 行 5 / 行 6
        }
    }
}

/// 从 `Authorization: Bearer <t>` 头提取 token（大小写不敏感前缀）。
pub fn bearer_of(headers: &HeaderMap) -> Option<&str> {
    let raw = headers.get(header::AUTHORIZATION)?.to_str().ok()?.trim();
    let prefix = "bearer ";
    if raw.len() > prefix.len() && raw[..prefix.len()].eq_ignore_ascii_case(prefix) {
        Some(raw[prefix.len()..].trim())
    } else {
        None
    }
}

/// 抽取并校验 Aria2 `params` 里的 `"token:<t>"` 元素。
///
/// 行为：**移除**所有以 `token:` 开头的字符串参数；`expected` 为 `Some` 时要求存在且相等，
/// 否则报错；`None` 时（未配置）无论有没有都放行。
pub fn token_param_of(params: &mut Vec<Value>, expected: Option<&str>) -> Result<(), TokenErr> {
    let mut found: Option<String> = None;
    let mut i = 0;
    while i < params.len() {
        if let Value::String(s) = &params[i]
            && let Some(tok) = s.strip_prefix("token:")
        {
            found = Some(tok.to_string());
            params.remove(i);
            continue;
        }
        i += 1;
    }
    match expected {
        None => Ok(()),
        Some(e) => match found {
            Some(t) if ct_eq(&t, e) => Ok(()),
            Some(_) => Err(TokenErr::Mismatch),
            None => Err(TokenErr::Missing),
        },
    }
}

/// token 校验错误。
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum TokenErr {
    /// 需要 token 但缺失。
    #[error("缺少 token")]
    Missing,
    /// token 不匹配。
    #[error("token 不匹配")]
    Mismatch,
}

/// **统一入站鉴权**：所有受保护的 BitComet 路由都调用它。
pub fn authorize(st: &PublicState, headers: &HeaderMap) -> Authz {
    let bearer = bearer_of(headers);
    let device_ok = bearer
        .map(|t| st.handshake.tokens.is_device(t))
        .unwrap_or(false);
    AuthPolicy::new(st.public_token.as_deref()).decide(bearer, device_ok)
}

/// 定长定时比较（自写 20 行，不引 `subtle`），避免比较耗时侧信道。
pub fn ct_eq(a: &str, b: &str) -> bool {
    let (a, b) = (a.as_bytes(), b.as_bytes());
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

/// 代理侧「入站成功值大小写不敏感」判定（`OK` / `ok` / 空串都算成功）。
///
/// 直接复用 `bitcomet-api` 的实现，避免两处定义分叉。
pub use bitcomet_api::is_ok_code;

/// BitComet 统一成功信封 + 业务字段（`platform:"proxy"`）。
pub fn bitcomet_ok(data: Value) -> Value {
    let mut m = Map::new();
    m.insert("error_code".into(), Value::String(EC_OK.into()));
    m.insert("error_message".into(), Value::String(String::new()));
    m.insert("version".into(), Value::String(crate::VERSION.into()));
    m.insert("platform".into(), Value::String(PLATFORM.into()));
    m.insert("file_size_prefix".into(), Value::String("binary".into()));
    if let Value::Object(o) = data {
        for (k, v) in o {
            m.insert(k, v);
        }
    }
    Value::Object(m)
}

/// 成功信封 → HTTP 200 响应。
pub fn bitcomet_ok_resp(data: Value) -> Response {
    (StatusCode::OK, Json(bitcomet_ok(data))).into_response()
}

/// 统一失败信封（错误码 + 可读信息），HTTP 状态由调用方给。
pub fn bitcomet_err(http: StatusCode, code: &str, msg: &str) -> Response {
    let body = serde_json::json!({
        "error_code": code,
        "error_message": msg,
        "version": crate::VERSION,
        "platform": PLATFORM,
        "file_size_prefix": "binary",
    });
    (http, Json(body)).into_response()
}

/// `/api_v2/task_list/get` 专用信封：顶层键严格对齐 T02 实测的键集合。
pub fn bitcomet_task_list(count: i64, tasks: Vec<Value>) -> Value {
    let mut m = Map::new();
    m.insert("error_code".into(), Value::String(EC_OK.into()));
    m.insert("filtered_task_count".into(), Value::from(count));
    m.insert("platform".into(), Value::String(PLATFORM.into()));
    m.insert("tasks".into(), Value::Array(tasks));
    m.insert("version".into(), Value::String(crate::VERSION.into()));
    Value::Object(m)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ok_code_is_case_insensitive() {
        assert!(is_ok_code("OK"));
        assert!(is_ok_code("ok"));
        assert!(is_ok_code(""));
        assert!(!is_ok_code("INVALID_TOKEN"));
    }

    #[test]
    fn bearer_parsing_is_case_insensitive() {
        let mut h = HeaderMap::new();
        h.insert(header::AUTHORIZATION, "Bearer abc123".parse().unwrap());
        assert_eq!(bearer_of(&h), Some("abc123"));
        h.insert(header::AUTHORIZATION, "bearer xyz".parse().unwrap());
        assert_eq!(bearer_of(&h), Some("xyz"));
        h.insert(header::AUTHORIZATION, "Basic zzz".parse().unwrap());
        assert_eq!(bearer_of(&h), None);
    }

    #[test]
    fn token_param_extraction_and_validation() {
        // 未配置 → 放行且移除 token 元素
        let mut p = vec![
            Value::String("token:aaa".into()),
            Value::String("gid".into()),
        ];
        assert!(token_param_of(&mut p, None).is_ok());
        assert_eq!(p.len(), 1, "token 元素应被移除");

        // 已配置 + 匹配 → Ok
        let mut p = vec![Value::String("token:secret".into())];
        assert!(token_param_of(&mut p, Some("secret")).is_ok());

        // 已配置 + 不匹配 → Mismatch
        let mut p = vec![Value::String("token:wrong".into())];
        assert_eq!(
            token_param_of(&mut p, Some("secret")),
            Err(TokenErr::Mismatch)
        );

        // 已配置 + 缺失 → Missing
        let mut p = vec![Value::String("noslash".into())];
        assert_eq!(
            token_param_of(&mut p, Some("secret")),
            Err(TokenErr::Missing)
        );
    }

    #[test]
    fn decision_table_rows() {
        let configured = AuthPolicy::new(Some("secret"));
        let unconfigured = AuthPolicy::new(None);

        assert_eq!(configured.decide(Some("x"), true), Authz::Allow); // 行 1
        assert_eq!(configured.decide(Some("secret"), false), Authz::Allow); // 行 2
        assert_eq!(configured.decide(Some("wrong"), false), Authz::Deny); // 行 3
        assert_eq!(configured.decide(None, false), Authz::Deny); // 行 4
        assert_eq!(unconfigured.decide(Some("whatever"), false), Authz::Allow); // 行 5
        assert_eq!(unconfigured.decide(None, false), Authz::Allow); // 行 6
    }

    #[test]
    fn ct_eq_basic() {
        assert!(ct_eq("abc", "abc"));
        assert!(!ct_eq("abc", "abd"));
        assert!(!ct_eq("abc", "abcd"));
    }
}
