//! 管理 API handler（6 个业务端点 + login/logout）。
//!
//! 统一信封 `{code, data, message}`；写操作要求会话 + CSRF；读操作仅会话。

use std::net::SocketAddr;
use std::sync::Arc;

use axum::Json;
use axum::extract::{ConnectInfo, Path, Query, State};
use axum::http::{HeaderValue, StatusCode, header};
use axum::response::Response;
use serde_json::json;

use super::{
    C_CSRF_INVALID, C_INTERNAL, C_NOT_FOUND, C_PRECONDITION_FAILED, C_TOO_MANY_REQUESTS,
    C_UNAUTHENTICATED, C_VALIDATION_ERROR, err, ok,
};
use crate::ingress::envelope;
use crate::state::AdminState;
use crate::tasks::query::{self, TaskListQuery};
use crate::tasks::{IngressOrigin, TaskCreateInput, TaskKind, create_task};

/// 会话有效期（秒），也用于 `expires_in`。
const SESSION_MAX_AGE: i64 = 1800;

/// `POST /api/admin/login` 请求体。
#[derive(serde::Deserialize)]
pub struct LoginReq {
    /// 管理口令。
    pub password: String,
}

/// `POST /api/admin/tasks` 请求体。
#[derive(serde::Deserialize)]
pub struct CreateReq {
    /// 链接。
    pub url: String,
    /// 任务类型（缺省按 `http`）。
    #[serde(default)]
    pub kind: Option<String>,
    /// 组 id。
    #[serde(default)]
    pub group_id: Option<i64>,
    /// 来源键。
    #[serde(default)]
    pub source_key: Option<String>,
    /// 文件名。
    #[serde(default)]
    pub filename: Option<String>,
}

/// `POST /api/admin/tasks/{id}/{action}` 请求体。
#[derive(serde::Deserialize, Default)]
pub struct ActionReq {
    /// 是否同时删除节点文件。
    #[serde(default)]
    pub delete_files: Option<bool>,
}

/// `POST /api/admin/login`。
pub async fn login(
    ConnectInfo(addr): ConnectInfo<SocketAddr>,
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Json(req): Json<LoginReq>,
) -> Response {
    let now = crate::ids::now_secs();
    let ip = addr.ip();

    if let Some(until) = st.guard.locked_until(ip, now) {
        return err(
            StatusCode::TOO_MANY_REQUESTS,
            C_TOO_MANY_REQUESTS,
            format!("登录尝试过多，请 {} 秒后重试", (until - now).max(0)),
        );
    }

    if !envelope::ct_eq(&req.password, &st.policy.password) {
        let count = st.guard.record_failure(ip, now);
        tracing::warn!(ip = %ip, failures = count, "管理登录失败");
        log_security(&st, now, "admin_login_failed", json!({ "failures": count })).await;
        return err(StatusCode::UNAUTHORIZED, C_UNAUTHENTICATED, "口令错误");
    }

    st.guard.record_success(ip);
    let sid = st.sessions.create(now);
    let secure = super::session::cookie_secure_for(st.policy.cookie_secure, &headers);
    let mut resp = ok(json!({ "expires_in": SESSION_MAX_AGE }));
    let cookie = super::session::set_cookie_header(&sid, SESSION_MAX_AGE, secure);
    if let Ok(v) = HeaderValue::from_str(&cookie) {
        resp.headers_mut().insert(header::SET_COOKIE, v);
    }
    resp
}

/// `POST /api/admin/logout`。
pub async fn logout(State(st): State<Arc<AdminState>>, headers: axum::http::HeaderMap) -> Response {
    let (guard, _) = super::session::guard_write(&st, &headers);
    match guard {
        super::session::Guard::Unauthenticated => {
            return err(StatusCode::UNAUTHORIZED, C_UNAUTHENTICATED, "未认证");
        }
        super::session::Guard::CsrfInvalid => {
            return err(StatusCode::FORBIDDEN, C_CSRF_INVALID, "CSRF 校验失败");
        }
        super::session::Guard::Ok => {}
    }
    if let Some(sid) = cookie_sid(&headers) {
        st.sessions.revoke(&sid);
    }
    let secure = super::session::cookie_secure_for(st.policy.cookie_secure, &headers);
    let mut resp = ok(serde_json::Value::Null);
    if let Ok(v) = HeaderValue::from_str(&super::session::clear_cookie_header(secure)) {
        resp.headers_mut().insert(header::SET_COOKIE, v);
    }
    resp
}

/// `GET /api/admin/summary`。
pub async fn summary(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
) -> Response {
    let expiry = match require_read(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let counts = match query::count_by_internal_state(&st.store).await {
        Ok(c) => c,
        Err(e) => {
            return err(
                StatusCode::INTERNAL_SERVER_ERROR,
                C_INTERNAL,
                format!("{e}"),
            );
        }
    };
    let total = counts.iter().map(|(_, n)| *n).sum::<i64>();
    let migrations = match st
        .store
        .query(
            "SELECT version FROM schema_migrations ORDER BY version",
            vec![],
        )
        .await
    {
        Ok(rows) => rows
            .iter()
            .filter_map(|r| match r.first() {
                Some(rusqlite::types::Value::Integer(v)) => Some(*v),
                _ => None,
            })
            .collect::<Vec<_>>(),
        Err(_) => Vec::new(),
    };
    let task_counts: Vec<_> = counts
        .iter()
        .map(|(s, n)| json!({ "state": s, "count": n }))
        .collect();
    let mut resp = ok(json!({
        "version": crate::VERSION,
        "uptime_seconds": crate::ids::now_secs().saturating_sub(st.started_at),
        "ports": { "public": st.env.public_addr, "admin": st.env.admin_addr },
        "db_path": st.env.db_path,
        "migration_versions": migrations,
        "task_counts": task_counts,
        "task_total": total,
        "nodes": [],
        "env_flags": {
            "public_token_configured": st.env.public_token_configured,
            "admin_password_configured": st.env.admin_password_configured,
            "allow_file_delete": st.env.allow_file_delete,
            "public_cors": st.env.public_cors,
            "admin_cookie_secure": st.env.admin_cookie_secure,
        },
        "scheduler_enabled": false,
    }));
    renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

/// `GET /api/admin/tasks`。
pub async fn tasks_list(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Query(q): Query<TaskListQuery>,
) -> Response {
    let expiry = match require_read(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    // BUG-2：分页参数非法（`page < 1`）必须显式 422，**不得**静默 clamp 到 1。
    if q.page < 1 {
        return err(
            StatusCode::UNPROCESSABLE_ENTITY,
            C_VALIDATION_ERROR,
            format!("page 必须 ≥ 1（收到 {}）", q.page),
        );
    }
    match query::list_tasks(&st.store, &q).await {
        Ok(page) => {
            let mut resp = ok(json!({
                "items": page.items.iter().map(task_view).collect::<Vec<_>>(),
                "page": page.page,
                "size": page.size,
                "total": page.total,
            }));
            renew_cookie(&mut resp, &st, &headers, expiry);
            resp
        }
        Err(e) => err(
            StatusCode::INTERNAL_SERVER_ERROR,
            C_INTERNAL,
            format!("{e}"),
        ),
    }
}

/// `POST /api/admin/tasks`。
pub async fn tasks_create(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Json(req): Json<CreateReq>,
) -> Response {
    let expiry = match require_write(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let kind = match req.kind.as_deref().map(parse_kind) {
        Some(k) => k,
        None => TaskKind::Http,
    };
    match create_task(
        &st.store,
        TaskCreateInput {
            kind,
            url_raw: req.url,
            url_norm: None,
            filename: req.filename,
            group_id: req.group_id,
            source_key: req.source_key,
            max_connection_count: None,
            start_later: false,
            save_folder_hint: None,
            origin: IngressOrigin::Admin,
        },
    )
    .await
    {
        Ok(c) => {
            let mut resp = ok(json!({
                "task_id": c.task_id,
                "gid": c.gid,
                "group_id": c.group_id,
            }));
            renew_cookie(&mut resp, &st, &headers, expiry);
            resp
        }
        Err(e) => err(
            StatusCode::UNPROCESSABLE_ENTITY,
            C_VALIDATION_ERROR,
            format!("{e}"),
        ),
    }
}

/// `POST /api/admin/tasks/{id}/{action}`。
pub async fn task_action(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Path((id, action)): Path<(String, String)>,
    Json(req): Json<ActionReq>,
) -> Response {
    let expiry = match require_write(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let mut resp = run_action(&st, &id, &action, &req).await;
    renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

/// 动作执行体（鉴权已在 [`task_action`] 内完成）；成功/失败都返回最终响应。
async fn run_action(st: &AdminState, id: &str, action: &str, req: &ActionReq) -> Response {
    let Some(row) = (match query::get_by_id(&st.store, id).await {
        Ok(v) => v,
        Err(e) => {
            return err(
                StatusCode::INTERNAL_SERVER_ERROR,
                C_INTERNAL,
                format!("{e}"),
            );
        }
    }) else {
        return err(StatusCode::NOT_FOUND, C_NOT_FOUND, "任务不存在");
    };

    match action {
        "pause" => match query::set_state(
            &st.store,
            &row.task_id,
            "paused",
            "paused",
            Some("not_started"),
        )
        .await
        {
            Ok(_) => ok(json!({ "task_id": row.task_id, "result": "paused" })),
            Err(e) => err(
                StatusCode::INTERNAL_SERVER_ERROR,
                C_INTERNAL,
                format!("{e}"),
            ),
        },
        "unpause" => {
            match query::set_state(&st.store, &row.task_id, "queued", "waiting", None).await {
                Ok(_) => ok(json!({ "task_id": row.task_id, "result": "queued" })),
                Err(e) => err(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    C_INTERNAL,
                    format!("{e}"),
                ),
            }
        }
        "retry" => {
            if row.internal_state != "failed" && row.internal_state != "queued" {
                return err(
                    StatusCode::CONFLICT,
                    C_PRECONDITION_FAILED,
                    "仅失败/排队任务可重试",
                );
            }
            match query::set_state(&st.store, &row.task_id, "queued", "waiting", None).await {
                Ok(_) => ok(json!({ "task_id": row.task_id, "result": "retrying" })),
                Err(e) => err(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    C_INTERNAL,
                    format!("{e}"),
                ),
            }
        }
        "remove" => {
            if req.delete_files.unwrap_or(false) {
                // 本轮无节点 ⇒ 无节点文件 ⇒ **拒绝**删文件请求（不静默降级为「只删库」）。
                return err(
                    StatusCode::UNPROCESSABLE_ENTITY,
                    C_VALIDATION_ERROR,
                    "delete_files=true 被拒绝：本轮无节点文件，禁止删除文件（如需仅移除任务记录，请用 delete_files=false）",
                );
            }
            match query::delete_task(&st.store, &row.task_id).await {
                Ok(_) => ok(json!({ "task_id": row.task_id, "result": "removed" })),
                Err(e) => err(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    C_INTERNAL,
                    format!("{e}"),
                ),
            }
        }
        other => err(
            StatusCode::UNPROCESSABLE_ENTITY,
            C_VALIDATION_ERROR,
            format!("未知动作: {other}"),
        ),
    }
}

/// `GET /api/admin/nodes`（本轮无节点 ⇒ 诚实空数组，不含 pass/token）。
pub async fn nodes_list(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
) -> Response {
    let expiry = match require_read(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let mut resp = ok(json!([]));
    renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

/// `GET /api/admin/config`。
pub async fn config_get(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
) -> Response {
    let expiry = match require_read(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let mut resp = ok(json!(st.env.items()));
    renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

/// 读守卫：`Ok(续期后的过期时刻)` 通过（`Some` 表示有活跃会话、需重发 Cookie）；`Err` 直接返回。
#[allow(clippy::result_large_err)] // `Response` 体量较大；直接回传错误响应最清晰
fn require_read(st: &AdminState, headers: &axum::http::HeaderMap) -> Result<Option<i64>, Response> {
    match super::session::guard_read(st, headers) {
        (super::session::Guard::Ok, exp) => Ok(exp),
        _ => Err(err(StatusCode::UNAUTHORIZED, C_UNAUTHENTICATED, "未认证")),
    }
}

/// 写守卫：`Ok(续期后的过期时刻)` 通过；未认证 401 / 跨源 403 直接返回 `Err`。
#[allow(clippy::result_large_err)] // 同 `require_read`
fn require_write(
    st: &AdminState,
    headers: &axum::http::HeaderMap,
) -> Result<Option<i64>, Response> {
    match super::session::guard_write(st, headers) {
        (super::session::Guard::Ok, exp) => Ok(exp),
        (super::session::Guard::Unauthenticated, _) => {
            Err(err(StatusCode::UNAUTHORIZED, C_UNAUTHENTICATED, "未认证"))
        }
        (super::session::Guard::CsrfInvalid, _) => {
            Err(err(StatusCode::FORBIDDEN, C_CSRF_INVALID, "CSRF 校验失败"))
        }
    }
}

/// 会话滑动续期：把刷新后的 `Set-Cookie`（`Max-Age` = 剩余空闲时长）重发到**活跃**响应上。
///
/// 设计 §5.1：活跃请求刷新 `last_seen_at` 的同时重发 Cookie，令浏览器端 `Max-Age` 随活动滑动。
/// `expiry` 为守卫续期后的新过期时刻；`None`（无会话）不重发。`logout` 走清除分支、不调用此函数。
fn renew_cookie(
    resp: &mut Response,
    st: &AdminState,
    headers: &axum::http::HeaderMap,
    expiry: Option<i64>,
) {
    let Some(exp) = expiry else { return };
    let Some(sid) = cookie_sid(headers) else {
        return;
    };
    let remaining = (exp - crate::ids::now_secs()).max(0);
    let secure = super::session::cookie_secure_for(st.policy.cookie_secure, headers);
    if let Ok(v) =
        HeaderValue::from_str(&super::session::set_cookie_header(&sid, remaining, secure))
    {
        resp.headers_mut().insert(header::SET_COOKIE, v);
    }
}

fn parse_kind(s: &str) -> TaskKind {
    match s.trim().to_ascii_lowercase().as_str() {
        "bt" => TaskKind::Bt,
        "magnet" => TaskKind::Magnet,
        "torrent" => TaskKind::Torrent,
        _ => TaskKind::Http,
    }
}

fn cookie_sid(headers: &axum::http::HeaderMap) -> Option<String> {
    let raw = headers.get(header::COOKIE)?.to_str().ok()?;
    for part in raw.split(';') {
        if let Some((k, v)) = part.trim().split_once('=')
            && k.trim() == super::session::COOKIE_NAME
        {
            return Some(v.trim().to_string());
        }
    }
    None
}

fn task_view(t: &crate::tasks::row::TaskRow) -> serde_json::Value {
    json!({
        "task_id": t.task_id,
        "gid": t.gid.clone().unwrap_or_default(),
        "name": t.name.clone().unwrap_or_else(|| t.url_raw.clone()),
        "url": t.url_raw,
        "kind": t.kind,
        "internal_state": t.internal_state,
        "aria_status": t.aria_status,
        "node_id": t.node_id,
        "total_size": t.total_size,
        "downloaded_size": t.downloaded_size,
        "permillage": t.permillage,
        "created_at": t.created_at,
        "updated_at": t.updated_at,
        "dispatched": false,
        "dispatch_state": "queued_no_node",
    })
}

async fn log_security(st: &AdminState, now: i64, message: &str, detail: serde_json::Value) {
    if let Err(e) = st
        .store
        .execute(
            "INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at) \
             VALUES ('warn', 'security', NULL, NULL, ?1, ?2, ?3)",
            vec![
                rusqlite::types::Value::Text(message.to_string()),
                rusqlite::types::Value::Text(detail.to_string()),
                rusqlite::types::Value::Integer(now),
            ],
        )
        .await
    {
        tracing::warn!(error = %e, "写管理安全审计失败");
    }
}
