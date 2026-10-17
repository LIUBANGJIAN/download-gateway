//! 管理口令的**可修改**实现：落库哈希 + 修改 / 查询接口。
//!
//! # 这个模块解决什么
//!
//! 在此之前，管理口令只能由部署时的环境变量 `DISPATCH_ADMIN_PASSWORD` 注入，
//! 或由进程启动时随机生成；`AdminPolicy.password` 包在 `Arc` 里，**运行时改不了**。
//! 用户要「自己设置管理口令」，缺的正是三层：**存哪里 / 怎么校验 / 怎么立即生效**。
//!
//! # 存储
//!
//! 复用 `app_config` KV（不新建表）：
//!
//! * `admin.password_hash` —— argon2id 的 PHC 串
//!   （`$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>`）
//! * `admin.password_updated_at` —— 修改时刻（Unix 秒），供界面显示「上次修改」
//!
//! # 优先级（改密真正生效的关键）
//!
//! 库里的哈希 > 环境变量 > 启动时随机生成。
//!
//! 一旦用户在管理台改过口令，**环境变量不再参与校验**。否则旧的部署凭据仍能登进来，
//! 等于改密没生效 —— 这是最容易被忽略、也最难察觉的一种「假成功」。
//! 当前实际用的是哪一路，由 [`password_status`] 的 `source` 字段如实暴露。
//!
//! # 为什么是 argon2id 而不是明文比较
//!
//! 环境变量/随机口令只活在内存里，明文比较尚可接受；但一旦**落库**，
//! 明文就意味着「拿到 db 文件 = 拿到管理口令」。argon2id 是目前口令哈希的推荐算法，
//! PHC 串自带参数（m/t/p），换参数或升级算法后旧哈希仍可校验。

use std::sync::Arc;

use argon2::password_hash::SaltString;
use argon2::{Argon2, PasswordHash, PasswordHasher, PasswordVerifier};
use axum::Json;
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::Response;
use rusqlite::types::Value;
use serde::Deserialize;
use serde_json::json;

use super::{C_INTERNAL, C_UNAUTHENTICATED, C_VALIDATION_ERROR, err, ok};
use crate::state::{AdminCredential, AdminState};
use crate::store::{Store, StoreError};

/// `app_config` 里存哈希的键。
pub const KEY_HASH: &str = "admin.password_hash";
/// `app_config` 里存修改时刻的键。
pub const KEY_UPDATED_AT: &str = "admin.password_updated_at";

/// 新口令最小长度（字符数）。
pub const MIN_LEN: usize = 10;
/// 新口令最大长度（字符数）。上限只为防「粘进来一整篇文章」把库撑大。
pub const MAX_LEN: usize = 128;

// ---------------------------------------------------------------------------
// 哈希
// ---------------------------------------------------------------------------

/// 生成 argon2id 的 PHC 串。
///
/// 盐用 `getrandom` 0.4 自己生成 16 字节（见 `Cargo.toml` 里关掉 `rand` feature 的理由），
/// 再交给 `SaltString::encode_b64` 做 base64 编码。
pub fn hash_password(plain: &str) -> Result<String, String> {
    let mut salt_bytes = [0u8; 16];
    getrandom::fill(&mut salt_bytes).map_err(|e| format!("生成随机盐失败：{e}"))?;
    let salt = SaltString::encode_b64(&salt_bytes).map_err(|e| format!("编码随机盐失败：{e}"))?;
    Argon2::default()
        .hash_password(plain.as_bytes(), &salt)
        .map(|h| h.to_string())
        .map_err(|e| format!("计算口令哈希失败：{e}"))
}

/// 校验口令与 PHC 串是否匹配。
///
/// 哈希串损坏/格式不认识时一律**返回 false**（而不是 panic，也不放行），
/// 并 `warn` 一条 —— 这种状态通常意味着库里被人工改过。
pub fn verify_password(phc: &str, input: &str) -> bool {
    match PasswordHash::new(phc) {
        Ok(parsed) => Argon2::default()
            .verify_password(input.as_bytes(), &parsed)
            .is_ok(),
        Err(e) => {
            tracing::warn!("库里的管理口令哈希无法解析（口令校验将一律失败）：{e}");
            false
        }
    }
}

// ---------------------------------------------------------------------------
// 存取
// ---------------------------------------------------------------------------

/// 读库里的哈希。读失败只记日志并返回 `None`（回退到环境变量/随机口令），
/// 不让「读不出配置」把管理口整个打死。
pub async fn load_hash(store: &Store) -> Option<String> {
    let rows = match store
        .query(
            "SELECT value FROM app_config WHERE key = ?1",
            vec![Value::Text(KEY_HASH.to_string())],
        )
        .await
    {
        Ok(r) => r,
        Err(e) => {
            tracing::warn!("读取管理口令哈希失败（回退到环境变量/随机口令）：{e}");
            return None;
        }
    };
    match rows.first().and_then(|r| r.first()) {
        Some(Value::Text(s)) if !s.trim().is_empty() => Some(s.clone()),
        _ => None,
    }
}

/// 读「上次修改时刻」（Unix 秒）；从未改过返回 `0`。
pub async fn load_updated_at(store: &Store) -> i64 {
    let Ok(rows) = store
        .query(
            "SELECT value FROM app_config WHERE key = ?1",
            vec![Value::Text(KEY_UPDATED_AT.to_string())],
        )
        .await
    else {
        return 0;
    };
    match rows.first().and_then(|r| r.first()) {
        Some(Value::Text(s)) => s.trim().parse::<i64>().unwrap_or(0),
        Some(Value::Integer(v)) => *v,
        _ => 0,
    }
}

/// 落库一条哈希 + 修改时刻（同一个事务内两条 UPSERT）。
pub async fn save_hash(store: &Store, phc: &str, now: i64) -> Result<(), StoreError> {
    store
        .execute(
            "INSERT INTO app_config (key, value, updated_at) VALUES (?1, ?2, ?3)
             ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
            vec![
                Value::Text(KEY_HASH.to_string()),
                Value::Text(phc.to_string()),
                Value::Integer(now),
            ],
        )
        .await?;
    store
        .execute(
            "INSERT INTO app_config (key, value, updated_at) VALUES (?1, ?2, ?3)
             ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
            vec![
                Value::Text(KEY_UPDATED_AT.to_string()),
                Value::Text(now.to_string()),
                Value::Integer(now),
            ],
        )
        .await?;
    Ok(())
}

/// 删除库里的口令哈希与修改时刻，让凭据回退到「环境变量 / 启动时随机口令」。
///
/// 仅供 `reset-password --clear` 这条**人工补救**路径使用（忘记口令时把哈希清掉，
/// 重启后按环境变量或随机口令登录）。正常改口令走 [`save_hash`]，不要用本函数。
///
/// 幂等：本来就没有这两行也不算失败 ——「已经清过了」与「刚清掉」对调用者意义相同。
pub async fn clear_hash(store: &Store) -> Result<(), StoreError> {
    store
        .execute(
            "DELETE FROM app_config WHERE key IN (?1, ?2)",
            vec![
                Value::Text(KEY_HASH.to_string()),
                Value::Text(KEY_UPDATED_AT.to_string()),
            ],
        )
        .await?;
    Ok(())
}

// ---------------------------------------------------------------------------
// 校验新口令
// ---------------------------------------------------------------------------

/// 校验新口令的**格式**（不查旧口令 —— 那一步由 handler 做）。
pub fn validate_new(new: &str, current: &str) -> Result<(), String> {
    let n = new.chars().count();
    if n < MIN_LEN {
        return Err(format!("新口令至少 {MIN_LEN} 个字符（当前 {n} 个）"));
    }
    if n > MAX_LEN {
        return Err(format!("新口令最多 {MAX_LEN} 个字符（当前 {n} 个）"));
    }
    if new.chars().any(char::is_control) {
        return Err("新口令不能包含控制字符（换行、制表符等）".into());
    }
    if new == current {
        return Err("新口令不能与当前口令相同".into());
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

/// `POST /api/admin/password` 请求体。
#[derive(Debug, Deserialize)]
pub struct ChangeReq {
    /// 当前口令（防「会话被劫持后直接改密」）。
    pub current_password: String,
    /// 新口令。
    pub new_password: String,
}

/// `POST /api/admin/password` —— 修改管理口令。
///
/// 成功即**立即生效**：内存凭据被替换为新的哈希，本次进程内后续登录都用新口令。
///
/// ⚠️ 按用户选定的策略（「最简：只做修改，不踢会话」），**不失效其它会话**。
/// 若日后要收紧，只需在这里调 `st.sessions.revoke_all()`（当前 `SessionStore` 还没这个方法）。
pub async fn change_password(
    State(st): State<Arc<AdminState>>,
    headers: HeaderMap,
    Json(req): Json<ChangeReq>,
) -> Response {
    let expiry = match super::rest::require_write(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let now = crate::ids::now_secs();

    // ① 旧口令必须对。否则「拿到一个已登录的浏览器」就等于能改密。
    if !st.verify_admin_password(&req.current_password) {
        audit(&st, now, "修改管理口令失败：当前口令不正确", "warn").await;
        return err(
            StatusCode::UNAUTHORIZED,
            C_UNAUTHENTICATED,
            "当前口令不正确",
        );
    }

    // ② 新口令格式。
    if let Err(msg) = validate_new(&req.new_password, &req.current_password) {
        return err(StatusCode::UNPROCESSABLE_ENTITY, C_VALIDATION_ERROR, msg);
    }

    // ③ 哈希 + 落库。
    let phc = match hash_password(&req.new_password) {
        Ok(v) => v,
        Err(e) => return err(StatusCode::INTERNAL_SERVER_ERROR, C_INTERNAL, e),
    };
    if let Err(e) = save_hash(&st.store, &phc, now).await {
        return err(
            StatusCode::INTERNAL_SERVER_ERROR,
            C_INTERNAL,
            format!("保存新口令失败：{e}"),
        );
    }

    // ④ 立即生效。顺序很重要：**先落库再换内存**，
    // 否则「换内存成功、落库失败」会让重启后口令悄悄回退到旧的。
    st.set_credential(AdminCredential::Hash(phc));

    audit(&st, now, "管理口令已修改", "info").await;

    let mut resp = ok(json!({
        "changed": true,
        "source": st.password_source(),
        "updated_at": now,
        "sessions_revoked": false,
    }));
    super::rest::renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

/// `GET /api/admin/password` —— 返回口令**状态**（绝不返回口令本身）。
///
/// 界面用它显示「当前来源」和「上次修改时间」，并据此决定提示文案：
/// 来源是 `environment` 时说明「改过一次后，环境变量里的旧口令就失效了」。
pub async fn password_status(State(st): State<Arc<AdminState>>, headers: HeaderMap) -> Response {
    let expiry = match super::rest::require_read(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let updated_at = load_updated_at(&st.store).await;
    let mut resp = ok(json!({
        "source": st.password_source(),
        "updated_at": updated_at,
        "customized": st.password_source() == "database",
        "min_length": MIN_LEN,
        "max_length": MAX_LEN,
    }));
    super::rest::renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

/// 写一条 `security` 审计。失败只记日志（审计绝不能阻断主流程）。
///
/// 公开版：`main.rs` 的 `reset-password` 子命令也要留痕，但它**不该为此依赖 `rusqlite`**
/// （根 crate 的依赖面越小越好，SQL 只留在本 crate 里）。
pub async fn log_security_audit(store: &Store, now: i64, message: &str, level: &str) {
    if let Err(e) = store
        .execute(
            "INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at)
             VALUES (?1, 'security', NULL, NULL, ?2, NULL, ?3)",
            vec![
                Value::Text(level.to_string()),
                Value::Text(message.to_string()),
                Value::Integer(now),
            ],
        )
        .await
    {
        tracing::warn!("写口令审计失败（不影响主流程）：{e}");
    }
}

/// 写一条口令相关的审计。失败只记日志（审计绝不能阻断主流程）。
///
/// `pub(super)`：`admin::selfreset` 生成临时口令时也要留一条（**不记口令本身**）。
pub(super) async fn audit(st: &AdminState, now: i64, message: &str, level: &str) {
    log_security_audit(&st.store, now, message, level).await;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hash_roundtrip_and_reject_wrong() {
        let phc = hash_password("correct horse battery").unwrap();
        assert!(phc.starts_with("$argon2id$v=19$"), "实际：{phc}");
        assert!(verify_password(&phc, "correct horse battery"));
        assert!(!verify_password(&phc, "correct horse batterY"));
        assert!(!verify_password(&phc, ""));
    }

    /// 同一个口令两次哈希必须得到**不同**的串（盐随机）。
    #[test]
    fn salt_is_random() {
        let a = hash_password("same-password-123").unwrap();
        let b = hash_password("same-password-123").unwrap();
        assert_ne!(a, b, "盐未随机化");
        assert!(verify_password(&a, "same-password-123"));
        assert!(verify_password(&b, "same-password-123"));
    }

    /// 坏哈希串必须**判失败**，不能 panic、也不能放行。
    #[test]
    fn broken_hash_never_passes() {
        assert!(!verify_password("not-a-phc-string", "anything"));
        assert!(!verify_password("", ""));
        assert!(!verify_password(
            "$argon2id$v=19$m=19456,t=2,p=1$broken",
            "x"
        ));
    }

    #[test]
    fn new_password_validation() {
        assert!(validate_new("1234567890", "old-password").is_ok());
        assert!(
            validate_new("123456789", "old-password").is_err(),
            "9 字符应被拒"
        );
        assert!(validate_new("", "old").is_err());
        assert!(
            validate_new(&"x".repeat(MAX_LEN + 1), "old").is_err(),
            "超长应被拒"
        );
        assert!(
            validate_new("1234567890", "1234567890").is_err(),
            "与当前相同应被拒"
        );
        assert!(
            validate_new("abc\ndefghij", "old").is_err(),
            "控制字符应被拒"
        );
        // 中文按**字符**计数，不是字节。
        assert!(validate_new("中文口令十个字符测试", "old").is_ok());
    }

    /// `reset-password --clear` 的落库效果：两条键都要消失，读回一律为「没有」。
    ///
    /// 这条用例守的是「清了但没真清」—— 只删哈希不删修改时刻的话，界面会继续显示
    /// 「上次修改时间」，用户会以为口令还在库里，进而怀疑命令没生效。
    #[tokio::test]
    async fn clear_hash_removes_both_keys() {
        let store = Store::open_in_memory().unwrap();
        store
            .execute(
                "CREATE TABLE app_config (key TEXT PRIMARY KEY, value TEXT, updated_at INTEGER)",
                vec![],
            )
            .await
            .unwrap();

        save_hash(&store, "phc-placeholder", 42).await.unwrap();
        assert_eq!(load_hash(&store).await.as_deref(), Some("phc-placeholder"));
        assert_eq!(load_updated_at(&store).await, 42);

        clear_hash(&store).await.unwrap();
        assert!(load_hash(&store).await.is_none(), "哈希应已删除");
        assert_eq!(load_updated_at(&store).await, 0, "修改时刻应已删除");

        // 幂等：再清一次不应报错。
        clear_hash(&store).await.unwrap();
    }
}
