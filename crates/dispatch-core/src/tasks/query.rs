//! 任务读路径：按 GID/task_id 查询、分页列表、状态变更、删除。
//!
//! 所有 `SELECT` **显式列出 25 个列名、顺序冻结**（禁止 `SELECT *`），与
//! [`super::row::TASK_COLUMNS`] 逐字一致。列序漂移会在 debug 构建下被 `decode_task` 的
//! 元数断言捕获。

use rusqlite::types::Value;
use serde::Deserialize;

use crate::ids;
use crate::store::{Store, StoreError};

use super::row::{N_TASK_COLUMNS, TaskRow, decode_task};

/// 默认分页大小。
pub const PAGE_DEFAULT: i64 = 50;

/// 被 SELECT 的列出串（与 [`TASK_COLUMNS`] 顺序一致）。
pub(crate) const SELECT_COLS: &str = "task_id, gid, group_id, source_key, kind, url_raw, url_norm, name, \
     size_state, total_size, downloaded_size, permillage, aria_status, internal_state, \
     error_class, error_message, node_id, node_task_id, attempt_no, max_attempts, \
     next_retry_at, possible_duplicate, created_at, updated_at, completed_at";

/// 列表查询条件（管理台 `GET /api/admin/tasks` 直接反序列化）。
#[derive(Debug, Clone, Deserialize)]
pub struct TaskListQuery {
    /// 按组过滤。
    #[serde(default)]
    pub group: Option<i64>,
    /// 按来源过滤。
    #[serde(default)]
    pub source: Option<String>,
    /// 按节点过滤。
    #[serde(default)]
    pub node: Option<i64>,
    /// 按内部状态过滤。
    #[serde(default)]
    pub state: Option<String>,
    /// 关键字（匹配 url_raw / name）。
    #[serde(default)]
    pub q: Option<String>,
    /// 页码（从 1 开始）。
    #[serde(default = "default_page")]
    pub page: i64,
    /// 每页条数。
    #[serde(default = "default_size")]
    pub size: i64,
}

fn default_page() -> i64 {
    1
}

fn default_size() -> i64 {
    PAGE_DEFAULT
}

impl Default for TaskListQuery {
    fn default() -> Self {
        Self {
            group: None,
            source: None,
            node: None,
            state: None,
            q: None,
            page: 1,
            size: PAGE_DEFAULT,
        }
    }
}

/// 一页列表结果。
#[derive(Debug, Clone)]
pub struct TaskListPage {
    /// 当前页条目。
    pub items: Vec<TaskRow>,
    /// 页码。
    pub page: i64,
    /// 每页条数。
    pub size: i64,
    /// 过滤后的总数。
    pub total: i64,
}

fn rows_to_tasks(rows: Vec<Vec<Value>>) -> Result<Vec<TaskRow>, StoreError> {
    debug_assert!(rows.iter().all(|r| r.len() == N_TASK_COLUMNS));
    let mut out = Vec::with_capacity(rows.len());
    for r in rows {
        match decode_task(&r) {
            Ok(t) => out.push(t),
            Err(e) => {
                // 列数/类型不符是编程错误，用存储错误包装上抛，避免静默错位。
                return Err(StoreError::Io(std::io::Error::new(
                    std::io::ErrorKind::InvalidData,
                    format!("任务行解码失败: {e}"),
                )));
            }
        }
    }
    Ok(out)
}

/// 按 GID 查询单条。
pub async fn get_by_gid(store: &Store, gid: &str) -> Result<Option<TaskRow>, StoreError> {
    let sql = format!("SELECT {SELECT_COLS} FROM task WHERE gid = ?1 LIMIT 1");
    let rows = store.query(sql, vec![Value::Text(gid.to_string())]).await?;
    Ok(rows_to_tasks(rows)?.into_iter().next())
}

/// 按 task_id 查询单条。
pub async fn get_by_id(store: &Store, task_id: &str) -> Result<Option<TaskRow>, StoreError> {
    let sql = format!("SELECT {SELECT_COLS} FROM task WHERE task_id = ?1 LIMIT 1");
    let rows = store
        .query(sql, vec![Value::Text(task_id.to_string())])
        .await?;
    Ok(rows_to_tasks(rows)?.into_iter().next())
}

/// 按一批 task_id 查询（顺序不保证）。
pub async fn list_by_ids(store: &Store, ids: &[String]) -> Result<Vec<TaskRow>, StoreError> {
    if ids.is_empty() {
        return Ok(Vec::new());
    }
    let placeholders = (1..=ids.len())
        .map(|i| format!("?{i}"))
        .collect::<Vec<_>>()
        .join(", ");
    let sql = format!("SELECT {SELECT_COLS} FROM task WHERE task_id IN ({placeholders})");
    let params: Vec<Value> = ids.iter().map(|s| Value::Text(s.clone())).collect();
    let rows = store.query(sql, params).await?;
    rows_to_tasks(rows)
}

/// 分页列表（带可选过滤）。
pub async fn list_tasks(store: &Store, q: &TaskListQuery) -> Result<TaskListPage, StoreError> {
    let mut wheres: Vec<String> = Vec::new();
    let mut params: Vec<Value> = Vec::new();
    let push = |frag: String, p: Value, params: &mut Vec<Value>, wheres: &mut Vec<String>| {
        params.push(p);
        wheres.push(frag.replace("?", &format!("?{}", params.len())));
    };

    if let Some(g) = q.group {
        push(
            "group_id = ?".into(),
            Value::Integer(g),
            &mut params,
            &mut wheres,
        );
    }
    if let Some(s) = &q.source {
        push(
            "source_key = ?".into(),
            Value::Text(s.clone()),
            &mut params,
            &mut wheres,
        );
    }
    if let Some(n) = q.node {
        push(
            "node_id = ?".into(),
            Value::Integer(n),
            &mut params,
            &mut wheres,
        );
    }
    if let Some(s) = &q.state {
        push(
            "internal_state = ?".into(),
            Value::Text(s.clone()),
            &mut params,
            &mut wheres,
        );
    }
    if let Some(kw) = &q.q {
        let like = format!("%{kw}%");
        params.push(Value::Text(like.clone()));
        let i = params.len();
        wheres.push(format!("(url_raw LIKE ?{i} OR name LIKE ?{i})"));
    }

    let where_clause = if wheres.is_empty() {
        String::new()
    } else {
        format!(" WHERE {}", wheres.join(" AND "))
    };

    // 总数
    let count_sql = format!("SELECT COUNT(*) FROM task{where_clause}");
    let count_rows = store.query(count_sql, params.clone()).await?;
    let total = match count_rows.first().and_then(|r| r.first()) {
        Some(Value::Integer(n)) => *n,
        _ => 0,
    };

    let size = q.size.clamp(1, 500);
    // `page` 合法性（≥ 1）由上游 admin 层校验（`rest::tasks_list` 非法即 422），
    // 此处**不再静默纠正**；仅对 OFFSET 做非负防御，避免负数偏移。
    let offset = (q.page - 1).max(0) * size;
    let list_sql = format!(
        "SELECT {SELECT_COLS} FROM task{where_clause} ORDER BY created_at DESC, task_id DESC LIMIT {size} OFFSET {offset}"
    );
    let rows = store.query(list_sql, params).await?;
    Ok(TaskListPage {
        items: rows_to_tasks(rows)?,
        page: q.page,
        size,
        total,
    })
}

/// 按内部状态集合列出（取最新 `limit` 条）。
pub async fn list_by_states(
    store: &Store,
    states: &[&str],
    limit: i64,
) -> Result<Vec<TaskRow>, StoreError> {
    if states.is_empty() {
        return Ok(Vec::new());
    }
    let placeholders = (1..=states.len())
        .map(|i| format!("?{i}"))
        .collect::<Vec<_>>()
        .join(", ");
    let mut params: Vec<Value> = states.iter().map(|s| Value::Text(s.to_string())).collect();
    params.push(Value::Integer(limit.max(0)));
    let lim_idx = params.len();
    let sql = format!(
        "SELECT {SELECT_COLS} FROM task WHERE internal_state IN ({placeholders}) \
         ORDER BY created_at DESC, task_id DESC LIMIT ?{lim_idx}"
    );
    let rows = store.query(sql, params).await?;
    rows_to_tasks(rows)
}

/// 各内部状态计数（管理台总览用）。
pub async fn count_by_internal_state(store: &Store) -> Result<Vec<(String, i64)>, StoreError> {
    let rows = store
        .query(
            "SELECT internal_state, COUNT(*) FROM task GROUP BY internal_state",
            vec![],
        )
        .await?;
    let mut out = Vec::with_capacity(rows.len());
    for r in rows {
        if let (Some(Value::Text(s)), Some(Value::Integer(n))) = (r.first(), r.get(1)) {
            out.push((s.clone(), *n));
        }
    }
    Ok(out)
}

/// 全表任务数。
pub async fn count_all(store: &Store) -> Result<i64, StoreError> {
    let rows = store.query("SELECT COUNT(*) FROM task", vec![]).await?;
    Ok(match rows.first().and_then(|r| r.first()) {
        Some(Value::Integer(n)) => *n,
        _ => 0,
    })
}

/// 状态变更（pause/unpause/retry）：更新内部状态 + Aria2 状态 + 错误类。
pub async fn set_state(
    store: &Store,
    task_id: &str,
    internal: &str,
    aria: &str,
    err: Option<&str>,
) -> Result<usize, StoreError> {
    store
        .execute(
            "UPDATE task SET internal_state = ?1, aria_status = ?2, error_class = ?3, updated_at = ?4 \
             WHERE task_id = ?5",
            vec![
                Value::Text(internal.to_string()),
                Value::Text(aria.to_string()),
                match err {
                    Some(e) => Value::Text(e.to_string()),
                    None => Value::Null,
                },
                Value::Integer(ids::now_ms()),
                Value::Text(task_id.to_string()),
            ],
        )
        .await
}

/// 删除任务行（`file`/`progress` 由 `ON DELETE CASCADE` 清理）。
///
/// 注意：**只删库行，不删节点文件**（`02 §4.1.2` + Q5）。
pub async fn delete_task(store: &Store, task_id: &str) -> Result<usize, StoreError> {
    store
        .execute(
            "DELETE FROM task WHERE task_id = ?1",
            vec![Value::Text(task_id.to_string())],
        )
        .await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tasks::create::{IngressOrigin, TaskCreateInput, TaskKind, create_task};

    async fn migrated_store() -> Store {
        let mut conn = crate::store::open_connection(":memory:").unwrap();
        let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("..")
            .join("migrations");
        crate::store::migrate::apply(&mut conn, &dir).unwrap();
        Store::from_connection(conn)
    }

    fn mk(url: &str) -> TaskCreateInput {
        TaskCreateInput {
            kind: TaskKind::Http,
            url_raw: url.into(),
            url_norm: None,
            filename: None,
            group_id: None,
            source_key: None,
            max_connection_count: None,
            start_later: false,
            save_folder_hint: None,
            origin: IngressOrigin::Admin,
        }
    }

    #[tokio::test]
    async fn get_and_list_roundtrip() {
        let store = migrated_store().await;
        let a = create_task(&store, mk("http://e/a")).await.unwrap();
        let b = create_task(&store, mk("http://e/b")).await.unwrap();

        assert_eq!(
            get_by_id(&store, &a.task_id)
                .await
                .unwrap()
                .unwrap()
                .task_id,
            a.task_id
        );
        assert!(
            get_by_gid(&store, "ffffffffffffffff")
                .await
                .unwrap()
                .is_none()
        );

        let page = list_tasks(
            &store,
            &TaskListQuery {
                size: 10,
                ..Default::default()
            },
        )
        .await
        .unwrap();
        assert_eq!(page.total, 2);
        assert_eq!(page.items.len(), 2);

        let ids = vec![a.task_id.clone(), b.task_id.clone()];
        assert_eq!(list_by_ids(&store, &ids).await.unwrap().len(), 2);

        assert_eq!(count_all(&store).await.unwrap(), 2);
        let counts = count_by_internal_state(&store).await.unwrap();
        assert_eq!(counts, vec![("queued".to_string(), 2)]);
        store.shutdown().await;
    }

    #[tokio::test]
    async fn set_state_and_delete() {
        let store = migrated_store().await;
        let t = create_task(&store, mk("http://e/c")).await.unwrap();
        let n = set_state(&store, &t.task_id, "paused", "paused", Some("not_started"))
            .await
            .unwrap();
        assert_eq!(n, 1);
        let row = get_by_id(&store, &t.task_id).await.unwrap().unwrap();
        assert_eq!(row.internal_state, "paused");

        assert_eq!(delete_task(&store, &t.task_id).await.unwrap(), 1);
        assert!(get_by_id(&store, &t.task_id).await.unwrap().is_none());
        store.shutdown().await;
    }

    #[tokio::test]
    async fn keyword_filter_matches_url() {
        let store = migrated_store().await;
        create_task(&store, mk("http://e/movie.mkv")).await.unwrap();
        create_task(&store, mk("http://e/music.mp3")).await.unwrap();
        let page = list_tasks(
            &store,
            &TaskListQuery {
                q: Some("movie".into()),
                ..Default::default()
            },
        )
        .await
        .unwrap();
        assert_eq!(page.total, 1);
        assert!(page.items[0].url_raw.contains("movie"));
        store.shutdown().await;
    }
}
