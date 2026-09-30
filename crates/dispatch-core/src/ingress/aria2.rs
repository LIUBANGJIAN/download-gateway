//! Aria2 兼容面：`POST /jsonrpc`（JSON-RPC 2.0）。
//!
//! 目标：让 AriaNg / aria2 客户端**不改代码**接入；**未实现方法一律返 `-32601`，绝不 404**。
//!
//! 支持：单条请求 / 批量数组 / `system.multicall`（限 32 条）；`token:` 参数鉴权。
//!
//! ⚠️ token 豁免：`system.multicall` 的**外层不校验** token（官方手册明确「不在调用里
//! 指定 token，而是每个嵌套子调用各自把 token 作为首个参数」）。强制点只在每个子调用
//! 内部（经 `handle_method` → `token_param_of`）；外层仅做通用规则的「剥离」。
//!
//! # 为什么手写 `Bytes` 解析
//!
//! aria2 客户端有时用 `text/plain` 发 JSON，用 `Json` 提取器会直接 415。这里统一收
//! `Bytes` 再 `serde_json::from_str`，容忍各种 Content-Type（体积仍受 `DefaultBodyLimit` 约束）。
//!
//! # 诚实字段（本轮无节点）
//!
//! `tellStatus` 追加 `dispatched:false` + `dispatchState:"queued_no_node"`；`dir` 返回空串 +
//! `dirKnown:false`（代理**不知**节点落盘路径，绝不编造）。

use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use serde_json::{Value, json};

use crate::ingress::envelope::{self, ARIA2_UNAUTHORIZED};
use crate::state::PublicState;
use crate::tasks::query;
use crate::tasks::row::TaskRow;
use crate::tasks::state::aria_view;
use crate::tasks::{IngressOrigin, TaskCreateInput, TaskKind, create_task};

/// JSON-RPC 方法未实现。
const E_METHOD_NOT_FOUND: i64 = -32601;
/// JSON-RPC 请求非法。
const E_INVALID_REQUEST: i64 = -32600;
/// JSON-RPC 参数非法。
const E_INVALID_PARAMS: i64 = -32602;
/// JSON-RPC 解析错误。
const E_PARSE: i64 = -32700;
/// aria2 业务错误（如 GID 不存在）。
const E_ARIA_GENERIC: i64 = 1;
/// `system.multicall` 子请求上限。
const MULTICALL_MAX: usize = 32;
/// 终态内部状态集合（对应 aria2 `tellStopped` 的 `complete`/`error`/`removed`）。
const STOPPED_STATES: &[&str] = &["completed", "failed", "removed"];

/// JSON-RPC 错误。
#[derive(Debug)]
pub struct RpcError {
    /// 错误码。
    pub code: i64,
    /// 错误信息。
    pub message: String,
}

impl RpcError {
    fn new(code: i64, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }
}

/// `/jsonrpc` 入口。
pub async fn jsonrpc(
    State(st): State<Arc<PublicState>>,
    _headers: HeaderMap,
    body: Bytes,
) -> Response {
    let parsed: Result<Value, _> = serde_json::from_slice(&body);
    let value = match parsed {
        Ok(v) => v,
        Err(_) => return single(E_PARSE, Value::Null, "Parse error"),
    };
    match value {
        Value::Array(arr) => {
            let mut out = Vec::with_capacity(arr.len());
            for item in arr {
                out.push(dispatch_call(&st, item).await);
            }
            (StatusCode::OK, axum::Json(Value::Array(out))).into_response()
        }
        obj @ Value::Object(_) => {
            (StatusCode::OK, axum::Json(dispatch_call(&st, obj).await)).into_response()
        }
        _ => single(E_INVALID_REQUEST, Value::Null, "Invalid Request"),
    }
}

/// 单个 RPC 调用 → 响应对象。
async fn dispatch_call(st: &PublicState, call: Value) -> Value {
    let id = call.get("id").cloned().unwrap_or(Value::Null);
    // JSON-RPC 2.0 §4/§5：请求对象必须带字符串成员 `"jsonrpc":"2.0"`。缺失或值不符
    // 一律回 `-32600 Invalid Request`（设计 §8.1：`缺 jsonrpc -32600`）。
    // 注意：这里只作用于**顶层/批量元素**；`system.multicall` 的**嵌套子调用**由
    // `handle_method` 直接分派，形状是 `{methodName,params}`，不带 `jsonrpc`。
    if call.get("jsonrpc").and_then(Value::as_str) != Some("2.0") {
        return error_obj(
            id,
            E_INVALID_REQUEST,
            "Invalid Request: missing or invalid jsonrpc (must be \"2.0\")",
        );
    }
    let Some(method) = call.get("method").and_then(Value::as_str) else {
        return error_obj(id, E_INVALID_REQUEST, "Invalid Request: missing method");
    };
    let mut params = call
        .get("params")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();

    if method == "system.multicall" {
        // 官方文档（aria2c.rst §"RPC authorization secret token"）原文：
        //   "The system.multicall RPC method is treated specially. ... we don't
        //    specify the token in the call. Instead, each nested method call has
        //    to provide the token as the first parameter as described above."
        // ⇒ 外层**不校验** token。强制点在下面对每个子调用调用的 `handle_method`
        //   （其内部会调 `token_param_of`），这里只做通用规则的「剥离」：
        //   首个参数若是 "token:..." 字符串，处理前移除，不做相等判定。
        if params
            .first()
            .and_then(Value::as_str)
            .is_some_and(|s| s.starts_with("token:"))
        {
            params.remove(0);
        }
        let Some(calls) = params.first().and_then(Value::as_array) else {
            return error_obj(id, E_INVALID_PARAMS, "system.multicall 需要数组参数");
        };
        if calls.len() > MULTICALL_MAX {
            return error_obj(
                id,
                E_INVALID_REQUEST,
                format!("system.multicall 最多 {MULTICALL_MAX} 条"),
            );
        }
        let mut results = Vec::with_capacity(calls.len());
        for c in calls {
            let mname = c.get("methodName").and_then(Value::as_str);
            let cparams = c
                .get("params")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default();
            match mname {
                Some(m) => match handle_method(st, m, cparams).await {
                    Ok(v) => results.push(json!([v])),
                    Err(e) => results.push(json!({ "code": e.code, "message": e.message })),
                },
                None => {
                    results.push(json!({ "code": E_INVALID_REQUEST, "message": "Invalid Request" }))
                }
            }
        }
        return ok_obj(id, Value::Array(results));
    }

    if method == "system.listMethods" {
        return ok_obj(id, json!(IMPLEMENTED_METHODS));
    }

    match handle_method(st, method, params).await {
        Ok(v) => ok_obj(id, v),
        Err(e) => error_obj(id, e.code, e.message),
    }
}

/// 已实现的方法清单（`system.listMethods` 返回）。
pub const IMPLEMENTED_METHODS: &[&str] = &[
    "aria2.addUri",
    "aria2.remove",
    "aria2.forceRemove",
    "aria2.removeDownloadResult",
    "aria2.pause",
    "aria2.forcePause",
    "aria2.pauseAll",
    "aria2.forcePauseAll",
    "aria2.unpause",
    "aria2.unpauseAll",
    "aria2.tellStatus",
    "aria2.getUris",
    "aria2.getFiles",
    "aria2.tellActive",
    "aria2.tellWaiting",
    "aria2.tellStopped",
    "aria2.getOption",
    "aria2.getGlobalStat",
    "aria2.getVersion",
    "aria2.getSessionInfo",
    "system.multicall",
    "system.listMethods",
];

/// 分派单个方法（含 `token:` 抽取/校验）。
pub async fn handle_method(
    st: &PublicState,
    method: &str,
    mut params: Vec<Value>,
) -> Result<Value, RpcError> {
    if envelope::token_param_of(&mut params, st.public_token.as_deref()).is_err() {
        return Err(RpcError::new(ARIA2_UNAUTHORIZED, "Unauthorized"));
    }

    match method {
        "aria2.addUri" => add_uri(st, &params).await,
        "aria2.remove" | "aria2.forceRemove" => {
            // aria2 官方语义（manual §"aria2.remove"）：
            //   "This method removes the download denoted by gid (string). If the
            //    specified download is in progress, it is first stopped. The status of
            //    the removed download becomes `removed`. This method returns GID of
            //    removed download."
            // ⇒ **软删**：只把行推进 `removed` 终态，**不**删行，因此删除后仍可被
            //   `tellStatus` / `tellStopped` 查到；真正的清除由
            //   `aria2.removeDownloadResult` 完成。
            // `forceRemove` 官方定义 = 「与 remove 相同，仅省去耗时动作（如通知 tracker）」
            // （"behaves just like aria2.remove except ... without performing any actions
            // which take time"）。本代理无 tracker 语义、也不做耗时收尾，故二者等价。
            // 返回值：官方为「被删下载的 GID」，故回 `Value::String(gid)`。
            // GID 不存在时的业务错误由 `set_by_gid` 天然给出（`E_ARIA_GENERIC`）。
            //
            // ⚠️ 未来地雷（**只记录，不要动 `migrations/`**）：软删保留了整行，行上的
            //   `(group_id, episode_no)` 仍占用部分唯一索引 `ux_task_group_episode`
            //   （`migrations/V1__init.sql:250`）。当前分组（T06/T07）尚未落地、建任务时
            //   `group_id`/`episode_no` 均为 NULL（部分索引对 NULL 不去重），**暂无影响**；
            //   但等分组落地后，一条已 `removed` 的旧行会把「同一组同一集重新添加」堵死在
            //   唯一索引上。届时应让 `remove` 顺带把这些列置空，或把唯一索引改成
            //   `... WHERE episode_no IS NOT NULL AND <逻辑删除标记> IS NULL`。
            let gid = str_param(&params, 0)?;
            set_by_gid(st, &gid, "removed", "removed", None).await?;
            Ok(Value::String(gid))
        }
        "aria2.removeDownloadResult" => {
            // aria2 官方语义（manual §"aria2.removeDownloadResult"）：
            //   "This method removes a completed/error/removed download denoted by gid
            //    from memory. This method returns OK for success."
            // 即：这才是**真正清除**下载结果的动作——把库行硬删（`file`/`progress`
            // 由 `ON DELETE CASCADE` 清理）；成功后返回字符串 `"OK"`。
            //
            // 与官方语义的**差异（如实记录）**：官方只对「已完成/出错/已移除」（即 stopped）
            // 的下载定义此操作，**但手册并未逐字写明**对进行中（active/waiting/paused）
            // 任务调用时的后果（真实 aria2 会报「not in stopped state」，我们**未确证**该
            // 报错文本/错误码）。本实现选择「更简单」而非「更严格」：**不校验前置状态**，
            // 只要 GID 存在就直接硬删。理由：常规客户端总是先 `remove`（进 removed 终态）
            // 再 `removeDownloadResult`，此时前置条件天然满足；而多一条状态校验会引入
            // 需与真实 aria2 逐字对齐的报错口径。GID 不存在时返回业务错误（与 `remove` 一致）。
            let gid = str_param(&params, 0)?;
            let row = query::get_by_gid(&st.store, &gid)
                .await
                .map_err(store_err)?
                .ok_or_else(|| RpcError::new(E_ARIA_GENERIC, format!("GID {gid} 不存在")))?;
            query::delete_task(&st.store, &row.task_id)
                .await
                .map_err(store_err)?;
            Ok(Value::String("OK".into()))
        }
        "aria2.pause" | "aria2.forcePause" => {
            let gid = str_param(&params, 0)?;
            set_by_gid(st, &gid, "paused", "paused", Some("not_started")).await?;
            Ok(Value::String(gid))
        }
        "aria2.unpause" => {
            let gid = str_param(&params, 0)?;
            set_by_gid(st, &gid, "queued", "waiting", None).await?;
            Ok(Value::String(gid))
        }
        "aria2.pauseAll" | "aria2.forcePauseAll" => {
            let tasks =
                query::list_by_states(&st.store, &["queued", "running", "dispatching"], 10_000)
                    .await
                    .map_err(store_err)?;
            for t in tasks {
                let _ = query::set_state(
                    &st.store,
                    &t.task_id,
                    "paused",
                    "paused",
                    Some("not_started"),
                )
                .await;
            }
            Ok(Value::String("OK".into()))
        }
        "aria2.unpauseAll" => {
            let tasks = query::list_by_states(&st.store, &["paused"], 10_000)
                .await
                .map_err(store_err)?;
            for t in tasks {
                let _ = query::set_state(&st.store, &t.task_id, "queued", "waiting", None).await;
            }
            Ok(Value::String("OK".into()))
        }
        "aria2.tellStatus" => {
            let gid = str_param(&params, 0)?;
            let row = query::get_by_gid(&st.store, &gid)
                .await
                .map_err(store_err)?
                .ok_or_else(|| RpcError::new(E_ARIA_GENERIC, format!("GID {gid} 不存在")))?;
            Ok(status_obj(&row))
        }
        "aria2.getUris" => {
            let gid = str_param(&params, 0)?;
            let row = query::get_by_gid(&st.store, &gid)
                .await
                .map_err(store_err)?
                .ok_or_else(|| RpcError::new(E_ARIA_GENERIC, format!("GID {gid} 不存在")))?;
            Ok(json!([{ "uri": row.url_raw, "status": "waiting" }]))
        }
        "aria2.getFiles" => {
            let gid = str_param(&params, 0)?;
            let row = query::get_by_gid(&st.store, &gid)
                .await
                .map_err(store_err)?
                .ok_or_else(|| RpcError::new(E_ARIA_GENERIC, format!("GID {gid} 不存在")))?;
            Ok(json!([file_obj(&row)]))
        }
        "aria2.getOption" => {
            let gid = str_param(&params, 0)?;
            let row = query::get_by_gid(&st.store, &gid)
                .await
                .map_err(store_err)?
                .ok_or_else(|| RpcError::new(E_ARIA_GENERIC, format!("GID {gid} 不存在")))?;
            Ok(json!({
                "gid": gid,
                "dir": "",
                "out": row.name.clone().unwrap_or_default(),
            }))
        }
        "aria2.tellActive" => {
            let tasks = query::list_by_states(&st.store, &["running", "dispatching"], 100)
                .await
                .map_err(store_err)?;
            Ok(Value::Array(tasks.iter().map(status_obj).collect()))
        }
        "aria2.tellWaiting" => {
            let tasks = query::list_by_states(&st.store, &["queued", "paused"], 100_000)
                .await
                .map_err(store_err)?;
            Ok(Value::Array(
                slice_by_offset_num(tasks, &params)
                    .iter()
                    .map(status_obj)
                    .collect(),
            ))
        }
        "aria2.tellStopped" => {
            // 终态集合（对应 aria2 `complete`/`error`/`removed`）。本轮无节点 ⇒ 通常为空数组，
            // 但一旦有终态行即应如实返回，并按 `offset`/`num` 截取。
            let tasks = query::list_by_states(&st.store, STOPPED_STATES, 100_000)
                .await
                .map_err(store_err)?;
            Ok(Value::Array(
                slice_by_offset_num(tasks, &params)
                    .iter()
                    .map(status_obj)
                    .collect(),
            ))
        }
        "aria2.getGlobalStat" => {
            let waiting =
                query::list_by_states(&st.store, &["queued", "paused", "dispatching"], 1_000_000)
                    .await
                    .map_err(store_err)?;
            let stopped = query::list_by_states(&st.store, STOPPED_STATES, 1_000_000)
                .await
                .map_err(store_err)?;
            Ok(json!({
                "downloadSpeed": "0",
                "uploadSpeed": "0",
                "numActive": "0",
                "numWaiting": waiting.len().to_string(),
                "numStopped": stopped.len().to_string(),
                "numStoppedTotal": stopped.len().to_string(),
            }))
        }
        "aria2.getVersion" => Ok(json!({
            "version": crate::VERSION,
            "enabledFeatures": ["BitTorrent", "MessageDigest", "RPC"],
        })),
        "aria2.getSessionInfo" => Ok(json!({ "sessionId": st.session_id })),
        _ => Err(RpcError::new(
            E_METHOD_NOT_FOUND,
            format!("Method not found: {method}"),
        )),
    }
}

async fn add_uri(st: &PublicState, params: &[Value]) -> Result<Value, RpcError> {
    let uris = params
        .first()
        .and_then(Value::as_array)
        .ok_or_else(|| RpcError::new(E_INVALID_PARAMS, "aria2.addUri 需要 [urls] 参数"))?;
    let first = uris
        .first()
        .and_then(Value::as_str)
        .ok_or_else(|| RpcError::new(E_INVALID_PARAMS, "aria2.addUri 的 urls 不能为空"))?
        .to_string();

    let opts = params.get(1).and_then(Value::as_object);
    let out = opts
        .and_then(|o| o.get("out"))
        .and_then(Value::as_str)
        .map(str::to_string);
    let dir = opts
        .and_then(|o| o.get("dir"))
        .and_then(Value::as_str)
        .map(str::to_string);
    let pause = opts
        .and_then(|o| o.get("pause"))
        .and_then(Value::as_str)
        .map(|s| s == "true")
        .unwrap_or(false);

    let created = create_task(
        &st.store,
        TaskCreateInput {
            kind: TaskKind::Http,
            url_raw: first,
            url_norm: None,
            filename: out,
            group_id: None,
            source_key: None,
            max_connection_count: None,
            start_later: pause,
            save_folder_hint: dir,
            origin: IngressOrigin::Aria2,
        },
    )
    .await
    .map_err(|e| RpcError::new(E_ARIA_GENERIC, format!("入库失败: {e}")))?;

    Ok(Value::String(created.gid))
}

async fn set_by_gid(
    st: &PublicState,
    gid: &str,
    internal: &str,
    aria: &str,
    err: Option<&str>,
) -> Result<(), RpcError> {
    let row = query::get_by_gid(&st.store, gid)
        .await
        .map_err(store_err)?
        .ok_or_else(|| RpcError::new(E_ARIA_GENERIC, format!("GID {gid} 不存在")))?;
    query::set_state(&st.store, &row.task_id, internal, aria, err)
        .await
        .map_err(store_err)?;
    Ok(())
}

fn str_param(params: &[Value], idx: usize) -> Result<String, RpcError> {
    params
        .get(idx)
        .and_then(Value::as_str)
        .map(str::to_string)
        .ok_or_else(|| RpcError::new(E_INVALID_PARAMS, format!("缺少第 {} 个字符串参数", idx + 1)))
}

/// 应用 aria2 `tellWaiting`/`tellStopped` 的 `offset`/`num` 参数。
///
/// `params[0]`=offset（缺省或负值 ⇒ 0）；`params[1]`=num（缺省或负值 ⇒ 不限，`0` ⇒ 空）。
/// 注意：`token:` 已在 [`handle_method`] 内被剥离，故此处的 `params[0]` 即 offset。
fn slice_by_offset_num(rows: Vec<TaskRow>, params: &[Value]) -> Vec<TaskRow> {
    let offset = params.first().and_then(Value::as_i64).unwrap_or(0).max(0) as usize;
    let num = params.get(1).and_then(Value::as_i64).unwrap_or(-1);
    let iter = rows.into_iter().skip(offset);
    if num < 0 {
        iter.collect()
    } else {
        iter.take(num as usize).collect()
    }
}

fn store_err(e: crate::store::StoreError) -> RpcError {
    RpcError::new(E_ARIA_GENERIC, format!("存储错误: {e}"))
}

/// 组装 aria2 `tellStatus` 风格的任务对象。
fn status_obj(t: &TaskRow) -> Value {
    let view = aria_view(t);
    json!({
        "gid": t.gid.clone().unwrap_or_default(),
        "status": view.status,
        "totalLength": t.total_size.map(|v| v.to_string()).unwrap_or_else(|| "0".into()),
        "totalLengthKnown": t.total_size.is_some(),
        "completedLength": t.downloaded_size.to_string(),
        "uploadLength": "0",
        "downloadSpeed": "0",
        "uploadSpeed": "0",
        "connections": "0",
        "numSeeders": "0",
        "seeder": "false",
        "pieceLength": "0",
        "numPieces": "0",
        "dir": "",
        "dirKnown": false,
        "dispatched": false,
        "dispatchState": "queued_no_node",
        "errorCode": view.error_code.to_string(),
        "errorMessage": view.error_message,
        "verifiedLength": "0",
        "verifyIntegrityPending": "false",
        "belongsTo": "",
        "files": [file_obj(t)],
    })
}

/// 文件对象：代理**不知节点落盘路径**（`dir` 为空串 + `dirKnown:false`）。
///
/// `path` 仅在**已知文件名**时给文件名（设计 §4.1.5：`files[].path` 只给文件名）；
/// 文件名未知时**留空串**，绝不拿 `url_raw` 冒充路径（BUG-5）。
fn file_obj(t: &TaskRow) -> Value {
    let path = t.name.clone().filter(|s| !s.is_empty()).unwrap_or_default();
    json!({
        "index": "1",
        "path": path,
        "length": t.total_size.map(|v| v.to_string()).unwrap_or_else(|| "0".into()),
        "completedLength": t.downloaded_size.to_string(),
        "selected": "true",
        "uris": [{ "uri": t.url_raw, "status": "waiting" }],
    })
}

fn ok_obj(id: Value, result: Value) -> Value {
    json!({ "jsonrpc": "2.0", "id": id, "result": result })
}

fn error_obj(id: Value, code: i64, message: impl Into<String>) -> Value {
    json!({ "jsonrpc": "2.0", "id": id, "error": { "code": code, "message": message.into() } })
}

fn single(code: i64, id: Value, message: &str) -> Response {
    (StatusCode::OK, axum::Json(error_obj(id, code, message))).into_response()
}
