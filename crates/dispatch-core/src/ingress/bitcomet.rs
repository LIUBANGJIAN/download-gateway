//! BitComet 兼容面：8 条复刻路径 + 统一信封 + 可选 token。
//!
//! 所有受保护路由**统一调用** [`envelope::authorize`]（判定表一处实现）。
//! body 一律用 `Bytes` + 手工 `serde_json`，保证**任何错误都回 BitComet 信封**，
//! 而不是 axum 的裸 422。
//!
//! 三个入库入口（http/bt/torrent_links add）**全部**走 `tasks::create_task`，禁止另写 INSERT。

use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use serde_json::{Value, json};

use crate::ingress::envelope::{self, Authz, EC_INVALID_REQUEST, EC_INVALID_TOKEN};
use crate::state::PublicState;
use crate::tasks::query::{self, TaskListQuery};
use crate::tasks::row::TaskRow;
use crate::tasks::{IngressOrigin, TaskCreateInput, TaskKind, create_task};

/// `POST /api/task/http/add` 请求体。
#[derive(serde::Deserialize)]
pub struct HttpAddReq {
    /// HTTP/FTP 直链。
    pub url: String,
    /// 保存目录（收下不转发，仅留痕）。
    #[serde(default)]
    pub save_folder: Option<String>,
    /// 文件名（↦ `task.name`）。
    #[serde(default)]
    pub filename: Option<String>,
    /// 是否稍后开始。
    #[serde(default)]
    pub start_later: Option<bool>,
    /// 最大连接数。
    #[serde(default)]
    pub max_connection_count: Option<i64>,
}

/// `POST /api/task/bt/add` 请求体。
#[derive(serde::Deserialize)]
pub struct BtAddReq {
    /// 种子 URL。
    #[serde(default)]
    pub torrent_url: Option<String>,
    /// 种子文件标识。
    #[serde(default)]
    pub torrent_file: Option<String>,
    /// 保存目录。
    #[serde(default)]
    pub save_folder: Option<String>,
    /// 是否稍后开始。
    #[serde(default)]
    pub start_later: Option<bool>,
}

/// `POST /api/task/torrent_links/add` 请求体。
#[derive(serde::Deserialize)]
pub struct TorrentLinksReq {
    /// 多条链接（换行/逗号/分号分隔）。
    pub torrent_links: String,
    /// 保存目录。
    #[serde(default)]
    pub save_folder: Option<String>,
    /// 是否稍后开始。
    #[serde(default)]
    pub start_later: Option<bool>,
}

/// `POST /api_v2/task_list/get` 请求体。
#[derive(serde::Deserialize)]
pub struct TaskListGetReq {
    /// 状态分组（本轮忽略）。
    #[serde(default)]
    pub state_group: Option<String>,
    /// 任务类型（本轮忽略）。
    #[serde(default)]
    pub task_type: Option<String>,
    /// 关键字。
    #[serde(default)]
    pub keyword: Option<String>,
    /// 起始下标。
    #[serde(default)]
    pub start: Option<i64>,
    /// 条数上限。
    #[serde(default)]
    pub limit: Option<i64>,
}

/// `POST /api_v2/tasks/action` 请求体。
#[derive(serde::Deserialize)]
pub struct TasksActionReq {
    /// 目标任务 id 列表。
    #[serde(default)]
    pub task_ids: Option<Vec<String>>,
    /// 单个任务 id。
    #[serde(default)]
    pub task_id: Option<String>,
    /// 动作名（pause/unpause/…）。
    pub action: String,
}

/// `POST /api_v2/tasks/delete` 请求体。
#[derive(serde::Deserialize)]
pub struct TasksDeleteReq {
    /// 目标任务 id 列表。
    #[serde(default)]
    pub task_ids: Option<Vec<String>>,
    /// 单个任务 id。
    #[serde(default)]
    pub task_id: Option<String>,
    /// 动作名（delete / delete_all）。
    pub action: String,
}

/// 统一前置：鉴权失败回 401 `INVALID_TOKEN` 并写安全审计。
async fn deny_if_unauthorized(
    st: &PublicState,
    headers: &HeaderMap,
    route: &str,
) -> Option<Response> {
    if let Authz::Deny = envelope::authorize(st, headers) {
        let had_bearer = envelope::bearer_of(headers).is_some();
        tracing::warn!(route = %route, had_bearer, reason = "mismatch", "invalid_bearer");
        let now = crate::ids::now_secs();
        let _ = st
            .store
            .execute(
                "INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at) \
                 VALUES ('warn', 'security', NULL, NULL, 'invalid_bearer', ?1, ?2)",
                vec![
                    rusqlite::types::Value::Text(
                        json!({ "route": route, "had_bearer": had_bearer, "reason": "mismatch" })
                            .to_string(),
                    ),
                    rusqlite::types::Value::Integer(now),
                ],
            )
            .await;
        return Some(envelope::bitcomet_err(
            StatusCode::UNAUTHORIZED,
            EC_INVALID_TOKEN,
            "Invalid token",
        ));
    }
    None
}

// `Err` 类型是 axum `Response`（较大），此处仅为「解析失败即回信封」的便捷返回。
#[allow(clippy::result_large_err)]
fn parse<T: serde::de::DeserializeOwned>(body: &Bytes) -> Result<T, Response> {
    serde_json::from_slice(body).map_err(|e| {
        envelope::bitcomet_err(
            StatusCode::BAD_REQUEST,
            EC_INVALID_REQUEST,
            &format!("请求体非法: {e}"),
        )
    })
}

/// `POST /api/task/http/add`。
pub async fn http_add(
    State(st): State<Arc<PublicState>>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    if let Some(r) = deny_if_unauthorized(&st, &headers, "/api/task/http/add").await {
        return r;
    }
    let req: HttpAddReq = match parse(&body) {
        Ok(v) => v,
        Err(r) => return r,
    };
    let created = match create_task(
        &st.store,
        TaskCreateInput {
            kind: TaskKind::Http,
            url_raw: req.url,
            url_norm: None,
            filename: req.filename,
            group_id: None,
            source_key: None,
            max_connection_count: req.max_connection_count,
            start_later: req.start_later.unwrap_or(false),
            save_folder_hint: req.save_folder,
            origin: IngressOrigin::BitCometHttp,
        },
    )
    .await
    {
        Ok(c) => c,
        Err(e) => return validation_err(&e),
    };
    envelope::bitcomet_ok_resp(json!({
        "proxy_task_id": created.task_id,
        "gid": created.gid,
    }))
}

/// `POST /api/task/bt/add`。
pub async fn bt_add(
    State(st): State<Arc<PublicState>>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    if let Some(r) = deny_if_unauthorized(&st, &headers, "/api/task/bt/add").await {
        return r;
    }
    let req: BtAddReq = match parse(&body) {
        Ok(v) => v,
        Err(r) => return r,
    };
    let Some(target) = req.torrent_url.or(req.torrent_file) else {
        return envelope::bitcomet_err(
            StatusCode::BAD_REQUEST,
            EC_INVALID_REQUEST,
            "缺少 torrent_url / torrent_file",
        );
    };
    let created = match create_task(
        &st.store,
        TaskCreateInput {
            kind: TaskKind::Bt,
            url_raw: target,
            url_norm: None,
            filename: None,
            group_id: None,
            source_key: None,
            max_connection_count: None,
            start_later: req.start_later.unwrap_or(false),
            save_folder_hint: req.save_folder,
            origin: IngressOrigin::BitCometBt,
        },
    )
    .await
    {
        Ok(c) => c,
        Err(e) => return validation_err(&e),
    };
    envelope::bitcomet_ok_resp(json!({
        "proxy_task_id": created.task_id,
        "gid": created.gid,
    }))
}

/// `POST /api/task/torrent_links/add`（逐条受理）。
pub async fn torrent_links_add(
    State(st): State<Arc<PublicState>>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    if let Some(r) = deny_if_unauthorized(&st, &headers, "/api/task/torrent_links/add").await {
        return r;
    }
    let req: TorrentLinksReq = match parse(&body) {
        Ok(v) => v,
        Err(r) => return r,
    };
    let links = bitcomet_api::addtask::split_add_links(&req.torrent_links);
    if links.is_empty() {
        return envelope::bitcomet_err(
            StatusCode::BAD_REQUEST,
            EC_INVALID_REQUEST,
            "torrent_links 为空",
        );
    }
    let start_later = req.start_later.unwrap_or(false);
    let mut accepted = Vec::with_capacity(links.len());
    for link in links {
        let kind = match bitcomet_api::addtask::classify_add_target(&link) {
            bitcomet_api::addtask::AddKind::Torrent => {
                if link
                    .trim_start()
                    .to_ascii_lowercase()
                    .starts_with("magnet:")
                {
                    TaskKind::Magnet
                } else {
                    TaskKind::Torrent
                }
            }
            bitcomet_api::addtask::AddKind::Http => TaskKind::Http,
        };
        match create_task(
            &st.store,
            TaskCreateInput {
                kind,
                url_raw: link.clone(),
                url_norm: None,
                filename: None,
                group_id: None,
                source_key: None,
                max_connection_count: None,
                start_later,
                save_folder_hint: req.save_folder.clone(),
                origin: IngressOrigin::BitCometTorrent,
            },
        )
        .await
        {
            Ok(c) => accepted.push(json!({
                "url": link,
                "proxy_task_id": c.task_id,
                "gid": c.gid,
            })),
            Err(e) => accepted.push(json!({ "url": link, "error": e.to_string() })),
        }
    }
    envelope::bitcomet_ok_resp(json!({ "accepted": accepted }))
}

/// `POST /api_v2/task_list/get`。
pub async fn task_list_get(
    State(st): State<Arc<PublicState>>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    if let Some(r) = deny_if_unauthorized(&st, &headers, "/api_v2/task_list/get").await {
        return r;
    }
    let req: TaskListGetReq = match parse(&body) {
        Ok(v) => v,
        Err(r) => return r,
    };
    let q = TaskListQuery {
        q: req.keyword.clone().filter(|s| !s.is_empty()),
        size: req.limit.unwrap_or(query::PAGE_DEFAULT).clamp(1, 500),
        page: 1,
        ..Default::default()
    };
    let page = match query::list_tasks(&st.store, &q).await {
        Ok(p) => p,
        Err(e) => return internal_err(&e),
    };
    let tasks: Vec<Value> = page.items.iter().map(task_obj).collect();
    let count = page.total;
    // 分页偏移：本轮实现简化（仅首页），按 start 裁剪。
    let tasks = if let Some(start) = req.start
        && start > 0
    {
        tasks.into_iter().skip(start as usize).collect()
    } else {
        tasks
    };
    (
        StatusCode::OK,
        axum::Json(envelope::bitcomet_task_list(count, tasks)),
    )
        .into_response()
}

/// `POST /api_v2/tasks/action`。
pub async fn tasks_action(
    State(st): State<Arc<PublicState>>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    if let Some(r) = deny_if_unauthorized(&st, &headers, "/api_v2/tasks/action").await {
        return r;
    }
    let req: TasksActionReq = match parse(&body) {
        Ok(v) => v,
        Err(r) => return r,
    };
    let ids = collect_ids(req.task_ids, req.task_id);
    let (internal, aria, err): (&str, &str, Option<&str>) = match req.action.as_str() {
        "pause" => ("paused", "paused", Some("not_started")),
        "unpause" | "resume" => ("queued", "waiting", None),
        "retry" => ("queued", "waiting", None),
        other => {
            return envelope::bitcomet_err(
                StatusCode::BAD_REQUEST,
                EC_INVALID_REQUEST,
                &format!("不支持的动作: {other}"),
            );
        }
    };
    let mut done = 0usize;
    for id in ids {
        if let Ok(Some(_)) = query::get_by_id(&st.store, &id).await
            && query::set_state(&st.store, &id, internal, aria, err)
                .await
                .is_ok()
        {
            done += 1;
        }
    }
    envelope::bitcomet_ok_resp(json!({ "affected": done }))
}

/// `POST /api_v2/tasks/delete`。
pub async fn tasks_delete(
    State(st): State<Arc<PublicState>>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    if let Some(r) = deny_if_unauthorized(&st, &headers, "/api_v2/tasks/delete").await {
        return r;
    }
    let req: TasksDeleteReq = match parse(&body) {
        Ok(v) => v,
        Err(r) => return r,
    };
    if req.action == "delete_all" {
        // 本轮无节点文件 ⇒ 拒绝整体删除（避免误删）。
        return envelope::bitcomet_err(
            StatusCode::BAD_REQUEST,
            EC_INVALID_REQUEST,
            "delete_all 本轮不支持（无节点文件）",
        );
    }
    let ids = collect_ids(req.task_ids, req.task_id);
    let mut done = 0usize;
    for id in ids {
        if let Ok(n) = query::delete_task(&st.store, &id).await
            && n > 0
        {
            done += n;
        }
    }
    envelope::bitcomet_ok_resp(json!({ "deleted": done }))
}

/// `POST /api/config/about/get`。
pub async fn about_get(State(st): State<Arc<PublicState>>, headers: HeaderMap) -> Response {
    if let Some(r) = deny_if_unauthorized(&st, &headers, "/api/config/about/get").await {
        return r;
    }
    envelope::bitcomet_ok_resp(json!({
        "about_info": {
            "version": crate::VERSION,
            "platform": envelope::PLATFORM,
            "proxy": true,
            "note": "仅受理任务，调度/下发未启用",
        }
    }))
}

/// `POST /api/task/summary/get`。
pub async fn summary_get(
    State(st): State<Arc<PublicState>>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    if let Some(r) = deny_if_unauthorized(&st, &headers, "/api/task/summary/get").await {
        return r;
    }
    let req: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
    let task_id = req
        .get("task_id")
        .and_then(Value::as_str)
        .map(str::to_string);
    let row = match task_id {
        Some(id) => query::get_by_id(&st.store, &id).await.ok().flatten(),
        None => None,
    };
    let (added_time, name) = match &row {
        Some(r) => (r.created_at, r.name.clone().unwrap_or_default()),
        None => (0, String::new()),
    };
    envelope::bitcomet_ok_resp(json!({
        "referrer": "",
        "save_folder": "",
        "added_time": added_time,
        "name": name,
    }))
}

fn collect_ids(list: Option<Vec<String>>, single: Option<String>) -> Vec<String> {
    let mut out = list.unwrap_or_default();
    if let Some(s) = single
        && !out.contains(&s)
    {
        out.push(s);
    }
    out
}

fn validation_err(e: &crate::tasks::CreateError) -> Response {
    envelope::bitcomet_err(
        StatusCode::UNPROCESSABLE_ENTITY,
        EC_INVALID_REQUEST,
        &format!("{e}"),
    )
}

fn internal_err(e: &crate::store::StoreError) -> Response {
    envelope::bitcomet_err(
        StatusCode::INTERNAL_SERVER_ERROR,
        "INTERNAL",
        &format!("{e}"),
    )
}

/// BitComet 侧任务对象（代理字段 + 扩展 `proxy_task_id`）。
fn task_obj(t: &TaskRow) -> Value {
    json!({
        "task_id": t.task_id,
        "proxy_task_id": t.task_id,
        "gid": t.gid.clone().unwrap_or_default(),
        "name": t.name.clone().unwrap_or_else(|| t.url_raw.clone()),
        "url": t.url_raw,
        "type": t.kind,
        "state": t.internal_state,
        "aria_status": t.aria_status,
        "total_size": t.total_size,
        "downloaded_size": t.downloaded_size,
        "permillage": t.permillage,
        "created_at": t.created_at,
    })
}
