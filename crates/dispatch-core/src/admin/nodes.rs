//! 管理台「节点」页：节点的增 / 删 / 改 / 启用关闭 + 密码回看 + 在线离线探测。
//!
//! # 这段代码是本轮从零建的
//!
//! 在此之前，`GET /api/admin/nodes` 是一个**假接口**（写死 `ok(json!([]))`），
//! 而全仓**没有任何一处** `INSERT/UPDATE/DELETE node` —— `node` 表的 DDL 一直是完整的，
//! 只是没有代码碰过它。本模块把它接通。
//!
//! # 「启用/关闭」是**调度开关**，不是远程开关机
//!
//! 用户的原话是「只做调度开关（enabled）」。因此：
//! [`node_set_enabled`] **只改 `node.enabled` 这一个字段**，
//! 不去触碰节点机器上跑着的任何程序、也不改其它列。
//!
//! # 「在线/离线」是**探测结果**，与 `enabled` 正交
//!
//! `enabled` 是**人工**开关（我要不要往它派活），探测结果是**客观**状态（它现在通不通）。
//! 两者互不影响：一台机器可以是「已关闭但在线」，也可以是「已启用但离线」。
//! 界面上必须分两列显示，否则用户会以为关掉开关就等于把机器关了。
//!
//! 探测规则：只要 `base_url` 有**任何 HTTP 响应**（包括 `401`，BitComet 未认证时正是 401）
//! 就算**在线**；连接被拒 / 超时 / DNS 失败才算**离线**。

use std::sync::Arc;
use std::time::{Duration, Instant};

use axum::Json;
use axum::extract::{Path, Query, State};
use axum::http::StatusCode;
use axum::response::Response;
use rusqlite::types::Value;
use serde::{Deserialize, Serialize};
use serde_json::json;
use tokio::task::JoinSet;

use super::{C_INTERNAL, C_NOT_FOUND, C_PRECONDITION_FAILED, C_VALIDATION_ERROR, err, ok};
use crate::state::AdminState;
use crate::store::{Store, StoreError};

/// 探测超时（毫秒）。刻意很短：这是给**界面刷新**用的，
/// 不能因为某台机器挂了就让整页卡住好几十秒。
pub const PROBE_TIMEOUT_MS: u64 = 1500;

/// 节点别名最大长度。
const ALIAS_MAX: usize = 64;
/// 单机并发上限的允许上界（防手滑填个天文数字）。
const MAX_CONCURRENT_CAP: i64 = 1024;

/// 允许的节点角色（与 `node.role` 的 CHECK 约束逐字一致）。
pub const ROLES: [&str; 3] = ["generic", "series", "netdisk"];

// ---------------------------------------------------------------------------
// 数据模型
// ---------------------------------------------------------------------------

/// 库里的节点行（只取本模块用得到的列）。
#[derive(Debug, Clone)]
struct NodeRow {
    node_id: i64,
    alias: String,
    base_url: String,
    user: String,
    pass_enc: String,
    weight: f64,
    max_concurrent: i64,
    max_rate_bytes: i64,
    role: String,
    tags: String,
    enabled: bool,
    state: String,
    last_probe_at: i64,
    last_seen_at: i64,
    created_at: i64,
    updated_at: i64,
}

/// 探测结果。
#[derive(Debug, Clone)]
pub struct Probe {
    /// 是否在线（拿到任何 HTTP 响应即在线）。
    pub online: bool,
    /// HTTP 状态码（离线时为 `None`）。
    pub http_code: Option<u16>,
    /// 往返耗时（毫秒）。
    pub latency_ms: i64,
    /// 失败原因（在线时为 `None`）。
    pub error: Option<String>,
}

/// 对外返回的节点视图。
///
/// ⚠️ **永远不含** `pass_enc` / `pass` / `token`。要看密码只能走
/// [`node_reveal_secret`] 这个单独的、会写审计的接口。
#[derive(Debug, Clone, Serialize)]
pub struct NodeView {
    /// 主键。
    pub node_id: i64,
    /// 人可读的别名（唯一）。
    pub alias: String,
    /// BitComet WebUI 基址。
    pub base_url: String,
    /// 登录账号。
    pub user: String,
    /// 是否已设置密码（**只报布尔，不报值**）。
    pub password_set: bool,
    /// 权重。
    pub weight: f64,
    /// 单机并发上限。
    pub max_concurrent: i64,
    /// 限速（字节/秒，后端口径）。
    pub max_rate_bytes: i64,
    /// 限速（KB/s，界面口径；由 `max_rate_bytes` 折算，`0` = 不限）。
    pub max_rate_kbps: i64,
    /// 角色。
    pub role: String,
    /// 标签。
    pub tags: String,
    /// 调度开关（人工）。
    pub enabled: bool,
    /// 探测得到的客观状态：`online` / `offline` / `unknown`。
    pub state: String,
    /// 是否在线（`state == "online"` 的便捷副本，便于前端直接用）。
    pub online: bool,
    /// 最近一次探测的 HTTP 状态码。
    pub http_code: Option<u16>,
    /// 最近一次探测耗时（毫秒）。
    pub latency_ms: Option<i64>,
    /// 最近一次探测的失败原因。
    pub probe_error: Option<String>,
    /// 最近一次探测时刻（Unix 秒）。
    pub last_probe_at: i64,
    /// 最近一次在线时刻（Unix 秒）。
    pub last_seen_at: i64,
    /// 创建时间。
    pub created_at: i64,
    /// 更新时间。
    pub updated_at: i64,
}

impl NodeRow {
    fn to_view(&self, probe: Option<&Probe>) -> NodeView {
        NodeView {
            node_id: self.node_id,
            alias: self.alias.clone(),
            base_url: self.base_url.clone(),
            user: self.user.clone(),
            password_set: !self.pass_enc.is_empty(),
            weight: self.weight,
            max_concurrent: self.max_concurrent,
            max_rate_bytes: self.max_rate_bytes,
            max_rate_kbps: bytes_to_kbps(self.max_rate_bytes),
            role: self.role.clone(),
            tags: self.tags.clone(),
            enabled: self.enabled,
            state: probe.map_or_else(
                || self.state.clone(),
                |p| if p.online { "online" } else { "offline" }.to_string(),
            ),
            online: probe.map_or(self.state == "online", |p| p.online),
            http_code: probe.and_then(|p| p.http_code),
            latency_ms: probe.map(|p| p.latency_ms),
            probe_error: probe.and_then(|p| p.error.clone()),
            last_probe_at: self.last_probe_at,
            last_seen_at: self.last_seen_at,
            created_at: self.created_at,
            updated_at: self.updated_at,
        }
    }
}

/// 字节/秒 → KB/s（界面口径）。
pub fn bytes_to_kbps(bytes: i64) -> i64 {
    if bytes <= 0 { 0 } else { bytes / 1024 }
}

/// KB/s → 字节/秒（存储口径）。`0` 表示不限速。
pub fn kbps_to_bytes(kbps: i64) -> i64 {
    if kbps <= 0 { 0 } else { kbps * 1024 }
}

// ---------------------------------------------------------------------------
// 探测
// ---------------------------------------------------------------------------

/// 构造探测用 HTTP 客户端。
///
/// ⚠️ `.no_proxy()` 是**必须**的：BitComet 节点在内网，而本机/容器环境
/// 可能注入了 `http_proxy`（本项目实测环境就有）。一旦被代理劫持，
/// 探测会全部失败，症状是「所有节点都显示离线」——而机器其实好好的。
/// 这与 `crates/bitcomet-api` 里的约束是同一条，理由相同。
fn probe_client() -> Result<reqwest::Client, reqwest::Error> {
    reqwest::Client::builder()
        .no_proxy()
        .user_agent(bitcomet_api::user_agent())
        .timeout(Duration::from_millis(PROBE_TIMEOUT_MS))
        .build()
}

/// 探测一台节点。**任何** HTTP 响应（含 401/403）都算在线。
async fn probe_node(client: &reqwest::Client, base_url: &str) -> Probe {
    let url = format!("{}/", base_url.trim_end_matches('/'));
    let started = Instant::now();
    match client.get(&url).send().await {
        Ok(resp) => Probe {
            online: true,
            http_code: Some(resp.status().as_u16()),
            latency_ms: started.elapsed().as_millis() as i64,
            error: None,
        },
        Err(e) => {
            let reason = if e.is_timeout() {
                format!("超时（>{PROBE_TIMEOUT_MS}ms）")
            } else if e.is_connect() {
                "连接被拒绝或不可达".to_string()
            } else {
                format!("{e}")
            };
            Probe {
                online: false,
                http_code: None,
                latency_ms: started.elapsed().as_millis() as i64,
                error: Some(reason),
            }
        }
    }
}

/// 并发探测一批节点（键为 `node_id`）。
///
/// 用 `JoinSet` 而不是顺序 `await`：3 台机器全挂时，顺序探测要 4.5 秒，
/// 并发只要 1.5 秒。界面刷新对延迟是敏感的。
async fn probe_all(rows: &[NodeRow]) -> Result<std::collections::HashMap<i64, Probe>, String> {
    if rows.is_empty() {
        return Ok(std::collections::HashMap::new());
    }
    let client = probe_client().map_err(|e| format!("构造探测客户端失败：{e}"))?;
    let mut set: JoinSet<(i64, Probe)> = JoinSet::new();
    for r in rows {
        let c = client.clone();
        let id = r.node_id;
        let url = r.base_url.clone();
        set.spawn(async move { (id, probe_node(&c, &url).await) });
    }
    let mut out = std::collections::HashMap::new();
    while let Some(joined) = set.join_next().await {
        match joined {
            Ok((id, p)) => {
                out.insert(id, p);
            }
            Err(e) => return Err(format!("探测任务 panic：{e}")),
        }
    }
    Ok(out)
}

/// 把探测结果写回 `node`（状态机字段）。
///
/// 单条 SQL 完成读改写，避免「先读后写」在两个请求交错时把 streak 算错：
/// * `state_since` 只在**状态真的变了**时才更新；
/// * `ok_streak` / `fail_streak` 互斥地一增一清零；
/// * `last_seen_at` 只在在线时推进。
async fn persist_probes(
    store: &Store,
    rows: &[NodeRow],
    probes: &std::collections::HashMap<i64, Probe>,
) -> Result<(), StoreError> {
    let now = crate::ids::now_secs();
    for r in rows {
        let Some(p) = probes.get(&r.node_id) else {
            continue;
        };
        let new_state = if p.online { "online" } else { "offline" };
        store
            .execute(
                "UPDATE node SET
                   state         = ?1,
                   state_since   = CASE WHEN state = ?1 THEN state_since ELSE ?2 END,
                   last_probe_at = ?2,
                   last_seen_at  = CASE WHEN ?3 = 1 THEN ?2 ELSE last_seen_at END,
                   ok_streak     = CASE WHEN ?3 = 1 THEN ok_streak + 1 ELSE 0 END,
                   fail_streak   = CASE WHEN ?3 = 1 THEN 0 ELSE fail_streak + 1 END,
                   updated_at    = ?2
                 WHERE node_id = ?4",
                vec![
                    Value::Text(new_state.to_string()),
                    Value::Integer(now),
                    Value::Integer(i64::from(p.online)),
                    Value::Integer(r.node_id),
                ],
            )
            .await?;
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

/// `GET /api/admin/nodes` 的查询参数。
#[derive(Debug, Deserialize)]
pub struct NodesQuery {
    /// `0` = 跳过探测，只回库里的历史状态（默认 `1`，即实时探测）。
    pub probe: Option<i64>,
}

async fn load_rows(store: &Store) -> Result<Vec<NodeRow>, StoreError> {
    let rows = store
        .query(
            "SELECT node_id, alias, base_url, user, pass_enc, weight, max_concurrent,
                    max_rate_bytes, role, tags, enabled, state, last_probe_at,
                    last_seen_at, created_at, updated_at
               FROM node ORDER BY node_id ASC",
            vec![],
        )
        .await?;
    Ok(rows.iter().map(|r| decode_row(r)).collect())
}

fn decode_row(r: &[Value]) -> NodeRow {
    let t = |i: usize| match &r[i] {
        Value::Text(s) => s.clone(),
        _ => String::new(),
    };
    let i = |idx: usize| match &r[idx] {
        Value::Integer(v) => *v,
        _ => 0,
    };
    let f = |idx: usize| match &r[idx] {
        Value::Real(v) => *v,
        Value::Integer(v) => *v as f64,
        _ => 0.0,
    };
    NodeRow {
        node_id: i(0),
        alias: t(1),
        base_url: t(2),
        user: t(3),
        pass_enc: t(4),
        weight: f(5),
        max_concurrent: i(6),
        max_rate_bytes: i(7),
        role: t(8),
        tags: t(9),
        enabled: i(10) != 0,
        state: t(11),
        last_probe_at: i(12),
        last_seen_at: i(13),
        created_at: i(14),
        updated_at: i(15),
    }
}

async fn load_one_row(store: &Store, node_id: i64) -> Result<Option<NodeRow>, StoreError> {
    let rows = store
        .query(
            "SELECT node_id, alias, base_url, user, pass_enc, weight, max_concurrent,
                    max_rate_bytes, role, tags, enabled, state, last_probe_at,
                    last_seen_at, created_at, updated_at
               FROM node WHERE node_id = ?1",
            vec![Value::Integer(node_id)],
        )
        .await?;
    Ok(rows.first().map(|r| decode_row(r)))
}

/// `GET /api/admin/nodes`。
pub async fn nodes_list(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Query(q): Query<NodesQuery>,
) -> Response {
    let expiry = match super::rest::require_read(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let rows = match load_rows(&st.store).await {
        Ok(v) => v,
        Err(e) => {
            return err(
                StatusCode::INTERNAL_SERVER_ERROR,
                C_INTERNAL,
                format!("{e}"),
            );
        }
    };

    let do_probe = q.probe.unwrap_or(1) != 0;
    let probes = if do_probe {
        match probe_all(&rows).await {
            Ok(p) => {
                // 探测结果写回失败**不**该让列表打不开：状态是锦上添花，
                // 节点清单才是主体。失败只记日志。
                if let Err(e) = persist_probes(&st.store, &rows, &p).await {
                    tracing::warn!("把探测结果写回 node 表失败：{e}");
                }
                p
            }
            Err(e) => {
                tracing::warn!("并发探测失败，列表将以库内历史状态返回：{e}");
                std::collections::HashMap::new()
            }
        }
    } else {
        std::collections::HashMap::new()
    };

    let items: Vec<NodeView> = rows
        .iter()
        .map(|r| {
            let p = probes.get(&r.node_id);
            r.to_view(if do_probe { p } else { None })
        })
        .collect();

    let mut resp = ok(json!({ "items": items, "total": items.len(), "probed": do_probe }));
    super::rest::renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

// ---------------------------------------------------------------------------
// 写：新增 / 编辑 / 删除 / 启停
// ---------------------------------------------------------------------------

/// 新增请求体。
#[derive(Debug, Deserialize)]
pub struct NodeCreateReq {
    /// 别名（唯一，必填）。
    pub alias: String,
    /// BitComet WebUI 基址（必填）。
    pub base_url: String,
    /// 登录账号（默认 `admin`）。
    pub user: Option<String>,
    /// 登录密码（必填）。存储时加密。
    pub password: String,
    /// 权重（默认 `1.0`）。
    pub weight: Option<f64>,
    /// 单机并发上限（默认 `3`）。
    pub max_concurrent: Option<i64>,
    /// 限速，单位 **KB/s**（默认 `0` = 不限）。
    pub max_rate_kbps: Option<i64>,
    /// 角色（默认 `generic`）。
    pub role: Option<String>,
    /// 标签（默认空）。
    pub tags: Option<String>,
    /// 是否启用（默认 `true`）。
    pub enabled: Option<bool>,
}

/// 编辑请求体：字段全可选；`password` 留空 = **不改原密码**。
#[derive(Debug, Deserialize)]
pub struct NodeUpdateReq {
    /// 别名。
    pub alias: Option<String>,
    /// 基址。
    pub base_url: Option<String>,
    /// 账号。
    pub user: Option<String>,
    /// 新密码；`None` 或空串 = 不修改。
    pub password: Option<String>,
    /// 权重。
    pub weight: Option<f64>,
    /// 并发上限。
    pub max_concurrent: Option<i64>,
    /// 限速（KB/s）。
    pub max_rate_kbps: Option<i64>,
    /// 角色。
    pub role: Option<String>,
    /// 标签。
    pub tags: Option<String>,
}

/// 校验失败原因。
#[derive(Debug, thiserror::Error)]
pub enum NodeError {
    /// 字段非法。
    #[error("{0}")]
    Invalid(String),
    /// 别名重复。
    #[error("别名「{0}」已被占用")]
    AliasTaken(String),
    /// 节点不存在。
    #[error("节点不存在：id={0}")]
    NotFound(i64),
    /// 存储层错误。
    #[error("{0}")]
    Store(#[from] StoreError),
}

impl NodeError {
    fn status(&self) -> StatusCode {
        match self {
            Self::Invalid(_) => StatusCode::UNPROCESSABLE_ENTITY,
            Self::AliasTaken(_) => StatusCode::CONFLICT,
            Self::NotFound(_) => StatusCode::NOT_FOUND,
            Self::Store(_) => StatusCode::INTERNAL_SERVER_ERROR,
        }
    }

    fn code(&self) -> i64 {
        match self {
            Self::Invalid(_) => C_VALIDATION_ERROR,
            Self::AliasTaken(_) => C_PRECONDITION_FAILED,
            Self::NotFound(_) => C_NOT_FOUND,
            Self::Store(_) => C_INTERNAL,
        }
    }
}

/// 校验基址：必须带 http/https 方案，且不含空白。
fn validate_base_url(u: &str) -> Result<String, NodeError> {
    let t = u.trim();
    if t.is_empty() {
        return Err(NodeError::Invalid("地址不能为空".into()));
    }
    if !(t.starts_with("http://") || t.starts_with("https://")) {
        return Err(NodeError::Invalid(
            "地址必须以 http:// 或 https:// 开头（例如 http://203.0.113.10:9085）".into(),
        ));
    }
    if t.chars().any(char::is_whitespace) {
        return Err(NodeError::Invalid("地址不能含空格".into()));
    }
    Ok(t.trim_end_matches('/').to_string())
}

/// 校验别名。
fn validate_alias(a: &str) -> Result<String, NodeError> {
    let t = a.trim();
    if t.is_empty() {
        return Err(NodeError::Invalid("别名不能为空".into()));
    }
    if t.chars().count() > ALIAS_MAX {
        return Err(NodeError::Invalid(format!(
            "别名过长（{} 字符，上限 {ALIAS_MAX}）",
            t.chars().count()
        )));
    }
    Ok(t.to_string())
}

/// 校验角色。
fn validate_role(r: &str) -> Result<String, NodeError> {
    let t = r.trim().to_ascii_lowercase();
    if ROLES.contains(&t.as_str()) {
        Ok(t)
    } else {
        Err(NodeError::Invalid(format!(
            "角色必须是 {} 之一（收到「{r}」）",
            ROLES.join(" / ")
        )))
    }
}

/// 校验数值类字段。
fn validate_numbers(weight: f64, max_concurrent: i64, max_rate_kbps: i64) -> Result<(), NodeError> {
    if !weight.is_finite() || weight < 0.0 {
        return Err(NodeError::Invalid(format!(
            "权重必须是不小于 0 的数（收到 {weight}）"
        )));
    }
    if !(1..=MAX_CONCURRENT_CAP).contains(&max_concurrent) {
        return Err(NodeError::Invalid(format!(
            "并发上限必须在 1–{MAX_CONCURRENT_CAP} 之间（收到 {max_concurrent}）"
        )));
    }
    if max_rate_kbps < 0 {
        return Err(NodeError::Invalid(format!(
            "限速不能为负数（收到 {max_rate_kbps} KB/s）"
        )));
    }
    Ok(())
}

/// 把 SQLite 的唯一约束冲突翻译成「别名重复」。
fn map_unique_violation(e: StoreError, alias: &str) -> NodeError {
    let msg = format!("{e}");
    if msg.contains("UNIQUE") || msg.contains("unique") {
        NodeError::AliasTaken(alias.to_string())
    } else {
        NodeError::Store(e)
    }
}

/// `POST /api/admin/nodes`。
pub async fn node_create(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Json(req): Json<NodeCreateReq>,
) -> Response {
    let expiry = match super::rest::require_write(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let mut resp = match create_impl(&st, req).await {
        Ok(v) => ok(json!({ "node": v })),
        Err(e) => err(e.status(), e.code(), format!("{e}")),
    };
    super::rest::renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

async fn create_impl(st: &AdminState, req: NodeCreateReq) -> Result<NodeView, NodeError> {
    let alias = validate_alias(&req.alias)?;
    let base_url = validate_base_url(&req.base_url)?;
    let role = validate_role(req.role.as_deref().unwrap_or("generic"))?;
    if req.password.is_empty() {
        return Err(NodeError::Invalid("新增节点时密码必填".into()));
    }
    let weight = req.weight.unwrap_or(1.0);
    let max_concurrent = req.max_concurrent.unwrap_or(3);
    let max_rate_kbps = req.max_rate_kbps.unwrap_or(0);
    validate_numbers(weight, max_concurrent, max_rate_kbps)?;
    let user = req
        .user
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .unwrap_or("admin")
        .to_string();

    let pass_enc = st
        .secrets
        .wrap(&req.password)
        .map_err(|e| NodeError::Invalid(format!("密码加密失败：{e}")))?;

    let now = crate::ids::now_secs();
    st.store
        .execute(
            "INSERT INTO node (alias, base_url, user, pass_enc, device_name, weight,
                               max_concurrent, max_rate_bytes, role, tags, enabled,
                               state, state_since, fail_streak, ok_streak, throttle_until,
                               token_expire_at, last_probe_at, last_seen_at,
                               created_at, updated_at)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11,
                     'unknown', 0, 0, 0, 0, 0, 0, 0, ?12, ?12)",
            vec![
                Value::Text(alias.clone()),
                Value::Text(base_url),
                Value::Text(user),
                Value::Text(pass_enc),
                Value::Text(bitcomet_api::DEFAULT_DEVICE_NAME.to_string()),
                Value::Real(weight),
                Value::Integer(max_concurrent),
                Value::Integer(kbps_to_bytes(max_rate_kbps)),
                Value::Text(role),
                Value::Text(req.tags.unwrap_or_default()),
                Value::Integer(i64::from(req.enabled.unwrap_or(true))),
                Value::Integer(now),
            ],
        )
        .await
        .map_err(|e| map_unique_violation(e, &alias))?;

    let rows = st
        .store
        .query("SELECT last_insert_rowid()", vec![])
        .await
        .map_err(NodeError::Store)?;
    let id = match rows.first().and_then(|r| r.first()) {
        Some(Value::Integer(v)) => *v,
        _ => return Err(NodeError::Invalid("无法取得新建节点的 id".into())),
    };
    let row = load_one_row(&st.store, id)
        .await
        .map_err(NodeError::Store)?
        .ok_or(NodeError::NotFound(id))?;
    audit(st, Some(id), &format!("新增节点「{}」", row.alias)).await;
    Ok(row.to_view(None))
}

/// `PUT /api/admin/nodes/{id}`。
pub async fn node_update(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Path(id): Path<i64>,
    Json(req): Json<NodeUpdateReq>,
) -> Response {
    let expiry = match super::rest::require_write(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let mut resp = match update_impl(&st, id, req).await {
        Ok(v) => ok(json!({ "node": v })),
        Err(e) => err(e.status(), e.code(), format!("{e}")),
    };
    super::rest::renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

async fn update_impl(st: &AdminState, id: i64, req: NodeUpdateReq) -> Result<NodeView, NodeError> {
    let cur = load_one_row(&st.store, id)
        .await
        .map_err(NodeError::Store)?
        .ok_or(NodeError::NotFound(id))?;

    let alias = match &req.alias {
        Some(a) => validate_alias(a)?,
        None => cur.alias.clone(),
    };
    let base_url = match &req.base_url {
        Some(u) => validate_base_url(u)?,
        None => cur.base_url.clone(),
    };
    let role = match &req.role {
        Some(r) => validate_role(r)?,
        None => cur.role.clone(),
    };
    let weight = req.weight.unwrap_or(cur.weight);
    let max_concurrent = req.max_concurrent.unwrap_or(cur.max_concurrent);
    let max_rate_kbps = req
        .max_rate_kbps
        .unwrap_or_else(|| bytes_to_kbps(cur.max_rate_bytes));
    validate_numbers(weight, max_concurrent, max_rate_kbps)?;

    // ⚠️ 密码留空 = **不改**。这是本接口最容易写错的一条：
    // 若把空串当成新密码写进去，用户「只改别名」就会把密码清掉，
    // 而症状要到下次连节点时才暴露。
    let pass_enc = match req.password.as_deref() {
        None => cur.pass_enc.clone(),
        Some("") => cur.pass_enc.clone(),
        Some(p) => st
            .secrets
            .wrap(p)
            .map_err(|e| NodeError::Invalid(format!("密码加密失败：{e}")))?,
    };

    let now = crate::ids::now_secs();
    st.store
        .execute(
            "UPDATE node SET alias = ?1, base_url = ?2, user = ?3, pass_enc = ?4,
                             weight = ?5, max_concurrent = ?6, max_rate_bytes = ?7,
                             role = ?8, tags = ?9, updated_at = ?10
             WHERE node_id = ?11",
            vec![
                Value::Text(alias.clone()),
                Value::Text(base_url),
                Value::Text(
                    req.user
                        .as_deref()
                        .map(str::trim)
                        .filter(|s| !s.is_empty())
                        .map(str::to_string)
                        .unwrap_or(cur.user.clone()),
                ),
                Value::Text(pass_enc),
                Value::Real(weight),
                Value::Integer(max_concurrent),
                Value::Integer(kbps_to_bytes(max_rate_kbps)),
                Value::Text(role),
                Value::Text(req.tags.unwrap_or(cur.tags.clone())),
                Value::Integer(now),
                Value::Integer(id),
            ],
        )
        .await
        .map_err(|e| map_unique_violation(e, &alias))?;

    let row = load_one_row(&st.store, id)
        .await
        .map_err(NodeError::Store)?
        .ok_or(NodeError::NotFound(id))?;
    audit(st, Some(id), &format!("编辑节点「{}」", row.alias)).await;
    Ok(row.to_view(None))
}

/// `DELETE /api/admin/nodes/{id}`。
pub async fn node_delete(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Path(id): Path<i64>,
) -> Response {
    let expiry = match super::rest::require_write(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let mut resp = match delete_impl(&st, id).await {
        Ok(alias) => ok(json!({ "node_id": id, "alias": alias, "result": "deleted" })),
        Err(e) => err(e.status(), e.code(), format!("{e}")),
    };
    super::rest::renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

async fn delete_impl(st: &AdminState, id: i64) -> Result<String, NodeError> {
    let cur = load_one_row(&st.store, id)
        .await
        .map_err(NodeError::Store)?
        .ok_or(NodeError::NotFound(id))?;
    let n = st
        .store
        .execute(
            "DELETE FROM node WHERE node_id = ?1",
            vec![Value::Integer(id)],
        )
        .await
        .map_err(NodeError::Store)?;
    if n == 0 {
        return Err(NodeError::NotFound(id));
    }
    audit(st, Some(id), &format!("删除节点「{}」", cur.alias)).await;
    Ok(cur.alias)
}

/// `POST /api/admin/nodes/{id}/enabled` 的请求体。
#[derive(Debug, Deserialize)]
pub struct EnabledReq {
    /// 目标开关状态。
    pub enabled: bool,
}

/// `POST /api/admin/nodes/{id}/enabled` —— **只切调度开关，不碰机器上任何程序**。
///
/// 单独做一个端点（而不是复用 `PUT`）的理由：语义单一。
/// 若复用 `PUT`，前端为了切一个开关就得把整个节点对象回填一遍，
/// 任何一次回填遗漏都会**静默改掉别的字段**。
pub async fn node_set_enabled(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Path(id): Path<i64>,
    Json(req): Json<EnabledReq>,
) -> Response {
    let expiry = match super::rest::require_write(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let mut resp = match set_enabled_impl(&st, id, req.enabled).await {
        Ok(v) => ok(json!({ "node": v })),
        Err(e) => err(e.status(), e.code(), format!("{e}")),
    };
    super::rest::renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

async fn set_enabled_impl(st: &AdminState, id: i64, enabled: bool) -> Result<NodeView, NodeError> {
    let cur = load_one_row(&st.store, id)
        .await
        .map_err(NodeError::Store)?
        .ok_or(NodeError::NotFound(id))?;
    let now = crate::ids::now_secs();
    st.store
        .execute(
            "UPDATE node SET enabled = ?1, updated_at = ?2 WHERE node_id = ?3",
            vec![
                Value::Integer(i64::from(enabled)),
                Value::Integer(now),
                Value::Integer(id),
            ],
        )
        .await
        .map_err(NodeError::Store)?;
    let row = load_one_row(&st.store, id)
        .await
        .map_err(NodeError::Store)?
        .ok_or(NodeError::NotFound(id))?;
    audit(
        st,
        Some(id),
        &format!(
            "节点「{}」调度开关 → {}（仅停止/恢复派发，不触碰机器上的下载程序）",
            cur.alias,
            if enabled { "启用" } else { "关闭" }
        ),
    )
    .await;
    Ok(row.to_view(None))
}

/// `POST /api/admin/nodes/{id}/secret` —— 回看密码（供界面「眼睛」按钮）。
///
/// 用 `POST` 而不是 `GET`：密码绝不能出现在 URL 里（会进浏览器历史、
/// 反代访问日志、Referer）。响应同时带 `Cache-Control: no-store`。
/// 每次调用都写一条 `security` 审计。
pub async fn node_reveal_secret(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Path(id): Path<i64>,
) -> Response {
    let expiry = match super::rest::require_write(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let mut resp = match reveal_impl(&st, id).await {
        Ok((alias, pw)) => {
            let mut r = ok(json!({ "node_id": id, "alias": alias, "password": pw }));
            r.headers_mut().insert(
                axum::http::header::CACHE_CONTROL,
                axum::http::HeaderValue::from_static("no-store"),
            );
            r
        }
        Err(e) => err(e.status(), e.code(), format!("{e}")),
    };
    super::rest::renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

async fn reveal_impl(st: &AdminState, id: i64) -> Result<(String, String), NodeError> {
    let cur = load_one_row(&st.store, id)
        .await
        .map_err(NodeError::Store)?
        .ok_or(NodeError::NotFound(id))?;
    let pw = st.secrets.open(&cur.pass_enc).map_err(|e| {
        NodeError::Invalid(format!(
            "解密节点「{}」的密码失败：{e}。常见原因：主密钥变过（检查 DISPATCH_SECRET_KEY 或密钥文件是否仍在）",
            cur.alias
        ))
    })?;
    audit(st, Some(id), &format!("查看节点「{}」的密码", cur.alias)).await;
    Ok((cur.alias, pw))
}

/// 写一条安全审计（失败只记日志，不影响主流程）。
async fn audit(st: &AdminState, node_id: Option<i64>, message: &str) {
    let now = crate::ids::now_secs();
    let r = st
        .store
        .execute(
            "INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at)
             VALUES ('info', 'security', ?1, NULL, ?2, NULL, ?3)",
            vec![
                node_id.map_or(Value::Null, Value::Integer),
                Value::Text(message.to_string()),
                Value::Integer(now),
            ],
        )
        .await;
    if let Err(e) = r {
        tracing::warn!("写审计事件失败（不影响主流程）：{e}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kbps_and_bytes_roundtrip() {
        assert_eq!(kbps_to_bytes(0), 0, "0 表示不限速");
        assert_eq!(kbps_to_bytes(1024), 1024 * 1024);
        assert_eq!(bytes_to_kbps(0), 0);
        assert_eq!(bytes_to_kbps(1024 * 1024), 1024);
        // 非 1024 整数倍时向下取整，不应 panic 也不应进位。
        assert_eq!(bytes_to_kbps(1500), 1);
        // 负值一律当作 0（防脏数据把界面搞成负数）。
        assert_eq!(bytes_to_kbps(-5), 0);
        assert_eq!(kbps_to_bytes(-5), 0);
    }

    #[test]
    fn base_url_validation() {
        assert!(validate_base_url("http://203.0.113.10:9085").is_ok());
        assert!(validate_base_url("https://198.51.100.7:9085/").is_ok());
        // 尾斜杠必须被规范掉，否则拼探测 URL 会出现 `//`。
        assert_eq!(
            validate_base_url("http://203.0.113.10:9085/").unwrap(),
            "http://203.0.113.10:9085"
        );
        assert!(validate_base_url("").is_err());
        assert!(
            validate_base_url("203.0.113.10:9085").is_err(),
            "缺方案应被拒"
        );
        assert!(validate_base_url("ftp://x").is_err());
        assert!(validate_base_url("http://a b").is_err(), "含空格应被拒");
    }

    #[test]
    fn alias_validation() {
        assert_eq!(validate_alias("  客厅NAS  ").unwrap(), "客厅NAS");
        assert!(validate_alias("").is_err());
        assert!(validate_alias("   ").is_err());
        assert!(validate_alias(&"x".repeat(ALIAS_MAX + 1)).is_err());
        assert!(validate_alias(&"x".repeat(ALIAS_MAX)).is_ok());
    }

    #[test]
    fn role_validation() {
        assert_eq!(validate_role("SERIES").unwrap(), "series");
        assert_eq!(validate_role(" generic ").unwrap(), "generic");
        assert!(validate_role("nas").is_err());
        assert!(validate_role("").is_err());
    }

    #[test]
    fn number_validation() {
        assert!(validate_numbers(1.0, 3, 0).is_ok());
        assert!(validate_numbers(0.0, 1, 100).is_ok());
        assert!(validate_numbers(-1.0, 3, 0).is_err(), "负权重应被拒");
        assert!(validate_numbers(f64::NAN, 3, 0).is_err(), "NaN 应被拒");
        assert!(validate_numbers(1.0, 0, 0).is_err(), "并发 0 应被拒");
        assert!(validate_numbers(1.0, MAX_CONCURRENT_CAP + 1, 0).is_err());
        assert!(validate_numbers(1.0, 3, -1).is_err(), "负限速应被拒");
    }

    /// 唯一约束冲突必须被翻译成 409，而不是笼统的 500。
    #[test]
    fn unique_violation_maps_to_alias_taken() {
        // 用**真实的** SQLite 唯一约束错误构造，而不是硬编码 ffi 错误码：
        // `rusqlite::ffi::Error::new` 的签名跨版本变过，硬编码会随升级静默失效。
        let conn = rusqlite::Connection::open_in_memory().unwrap();
        conn.execute_batch(
            "CREATE TABLE n (alias TEXT UNIQUE);
             INSERT INTO n VALUES ('客厅NAS');",
        )
        .unwrap();
        let raw = conn
            .execute("INSERT INTO n VALUES ('客厅NAS')", [])
            .expect_err("重复别名应触发唯一约束错误");
        let e = map_unique_violation(StoreError::Sqlite(raw), "客厅NAS");
        assert!(matches!(e, NodeError::AliasTaken(_)), "实际：{e:?}");
        assert_eq!(e.status(), StatusCode::CONFLICT);
        assert_eq!(e.code(), C_PRECONDITION_FAILED);

        // 非唯一类的 SQLite 错误**不得**被误判成 409。
        let other = map_unique_violation(
            StoreError::Sqlite(rusqlite::Error::QueryReturnedNoRows),
            "x",
        );
        assert!(matches!(other, NodeError::Store(_)), "实际：{other:?}");
    }

    /// `to_view` 必须**永不**带出密文，且 `state` 能被探测结果覆盖。
    #[test]
    fn view_never_leaks_ciphertext_and_reflects_probe() {
        let row = NodeRow {
            node_id: 1,
            alias: "客厅NAS".into(),
            base_url: "http://203.0.113.10:9085".into(),
            user: "admin".into(),
            pass_enc: "SUPER-SECRET-CIPHERTEXT".into(),
            weight: 1.0,
            max_concurrent: 3,
            max_rate_bytes: 2048,
            role: "generic".into(),
            tags: String::new(),
            enabled: true,
            state: "unknown".into(),
            last_probe_at: 0,
            last_seen_at: 0,
            created_at: 1,
            updated_at: 2,
        };
        let v = row.to_view(None);
        let js = serde_json::to_string(&v).unwrap();
        assert!(
            !js.contains("SUPER-SECRET-CIPHERTEXT"),
            "列表响应绝不能带密文：{js}"
        );
        assert!(v.password_set, "只报布尔");
        assert_eq!(v.max_rate_kbps, 2, "2048 B/s 应折算成 2 KB/s");
        assert_eq!(v.state, "unknown", "无探测结果时回库内历史状态");
        assert!(!v.online);

        // 有探测结果时，state/online/http_code 全部以探测为准。
        let p = Probe {
            online: true,
            http_code: Some(401),
            latency_ms: 12,
            error: None,
        };
        let v2 = row.to_view(Some(&p));
        assert_eq!(v2.state, "online");
        assert!(v2.online);
        assert_eq!(v2.http_code, Some(401), "BitComet 未认证返 401 也算在线");
        assert_eq!(v2.latency_ms, Some(12));

        let p3 = Probe {
            online: false,
            http_code: None,
            latency_ms: 1500,
            error: Some("超时".into()),
        };
        let v3 = row.to_view(Some(&p3));
        assert_eq!(v3.state, "offline");
        assert!(!v3.online);
        assert!(v3.probe_error.is_some());
    }
}
