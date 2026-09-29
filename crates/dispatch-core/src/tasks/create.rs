//! **★唯一入库入口 `create_task`**。
//!
//! 三个入口全部走它，禁止任何地方另写 `INSERT INTO task`：
//! - Aria2 兼容面 `aria2.addUri`
//! - BitComet 兼容面 `http/add`、`bt/add`、`torrent_links/add`
//! - 管理台 `POST /api/admin/tasks`
//!
//! 语义 = **受理 + 入库 + 排队**（`internal_state='queued'`, `node_id=NULL`,
//! `dispatch_reason=NULL`）。本轮无节点 ⇒ 任务会长期停在排队态，这是**有意的诚实行为**，
//! 接入面绝不假装已下发。
//!
//! # 事务取舍（见 design-1 §3.0(c)）
//!
//! 本轮唯一的多语句写是 `INSERT task`（权威）+ `INSERT event_log`（审计），二者**无共同不变量**。
//! 强制顺序：先写 `task`（权威），成功后再写 `event_log`；第二步失败只 `warn!`，
//! **不影响**接口返回成功（用户已受理）。反之第一步失败 ⇒ 无部分状态，直接报错。

use rusqlite::types::Value;

use crate::ids;
use crate::store::{Store, StoreError};

/// 任务类型（↦ `task.kind` 的 CHECK 取值）。
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum TaskKind {
    /// `http` / `https` / `ftp` 直链。
    Http,
    /// BitTorrent 种子（种子文件）。
    Bt,
    /// 磁力链接。
    Magnet,
    /// `.torrent` 链接。
    Torrent,
}

impl TaskKind {
    /// 对应 DDL 中的字符串值。
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Http => "http",
            Self::Bt => "bt",
            Self::Magnet => "magnet",
            Self::Torrent => "torrent",
        }
    }
}

/// 调用来源（写 `event_log` 的 `detail.origin` 用）。
#[derive(Clone, Copy, Debug)]
pub enum IngressOrigin {
    /// Aria2 JSON-RPC 面。
    Aria2,
    /// BitComet `http/add`。
    BitCometHttp,
    /// BitComet `bt/add`。
    BitCometBt,
    /// BitComet `torrent_links/add`。
    BitCometTorrent,
    /// 管理台 `POST /api/admin/tasks`。
    Admin,
}

impl IngressOrigin {
    /// 稳定的字符串标识。
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Aria2 => "aria2",
            Self::BitCometHttp => "bitcomet_http",
            Self::BitCometBt => "bitcomet_bt",
            Self::BitCometTorrent => "bitcomet_torrent",
            Self::Admin => "admin",
        }
    }
}

/// 入库输入。
pub struct TaskCreateInput {
    /// 任务类型。
    pub kind: TaskKind,
    /// 原始 URL / 磁力链 / 种子标识。
    pub url_raw: String,
    /// 归一化 URL（本轮由调用方决定，多为 `None`）。
    pub url_norm: Option<String>,
    /// ↦ `task.name`（aria2 `out` / bitcomet `filename`）。
    pub filename: Option<String>,
    /// 任务组 id（本轮恒 `None`）。
    pub group_id: Option<i64>,
    /// 网盘来源键（本轮恒 `None`）。
    pub source_key: Option<String>,
    /// 最大连接数（记元数据；本轮**不转发**给节点）。
    pub max_connection_count: Option<i64>,
    /// `true` ⇒ `aria_status='paused'` + `error_class='not_started'`（用户未点开始）。
    pub start_later: bool,
    /// 保存目录提示；**收下不转发**，仅写 `event_log` 留痕（本轮无节点）。
    pub save_folder_hint: Option<String>,
    /// 调用来源。
    pub origin: IngressOrigin,
}

/// 入库结果。
#[derive(Debug)]
pub struct TaskCreated {
    /// 新任务 id（ULID）。
    pub task_id: String,
    /// 新任务 GID（16 位十六进制）。
    pub gid: String,
    /// 任务组 id（回显）。
    pub group_id: Option<i64>,
}

/// 入库错误。
#[derive(Debug, thiserror::Error)]
pub enum CreateError {
    /// URL 非法（空白等）。
    #[error("url 非法: {0}")]
    InvalidUrl(String),
    /// `kind` 非法。
    #[error("kind 非法: {0}")]
    InvalidKind(String),
    /// GID 连续冲突过多。
    #[error("GID 连续冲突过多")]
    GidExhausted,
    /// 存储层错误。
    #[error(transparent)]
    Store(#[from] StoreError),
}

/// 唯一入库入口。见模块文档。
pub async fn create_task(
    store: &Store,
    input: TaskCreateInput,
) -> Result<TaskCreated, CreateError> {
    create_task_using(store, input, ids::new_gid).await
}

/// 内部实现，允许注入 GID 生成器（便于单测「GID 冲突重试」）。
pub(crate) async fn create_task_using<F>(
    store: &Store,
    input: TaskCreateInput,
    mut gid_gen: F,
) -> Result<TaskCreated, CreateError>
where
    F: FnMut() -> String,
{
    let url_raw = input.url_raw.trim().to_string();
    if url_raw.is_empty() {
        return Err(CreateError::InvalidUrl(input.url_raw));
    }

    let now = ids::now_ms();
    let task_id = ids::new_task_id(now);
    let (aria_status, error_class): (&str, Option<&str>) = if input.start_later {
        ("paused", Some("not_started"))
    } else {
        ("waiting", None)
    };
    let group_id = input.group_id;

    // GID 冲突：单语句语义 —— INSERT 撞 `gid UNIQUE` 就换一个 GID 重试，≤8 次。
    for _ in 0..8 {
        let gid = gid_gen();
        let params: Vec<Value> = vec![
            Value::Text(task_id.clone()),                 // 1 task_id
            Value::Text(gid.clone()),                     // 2 gid
            opt_i64(group_id),                            // 3 group_id
            opt_text(input.source_key.clone()),           // 4 source_key
            Value::Text(input.kind.as_str().to_string()), // 5 kind
            Value::Text(url_raw.clone()),                 // 6 url_raw
            opt_text(input.url_norm.clone()),             // 7 url_norm
            opt_text(input.filename.clone()),             // 8 name
            Value::Text("unknown".into()),                // 9 size_state
            Value::Null,                                  // 10 total_size
            Value::Integer(0),                            // 11 downloaded_size
            Value::Integer(0),                            // 12 permillage
            Value::Text(aria_status.to_string()),         // 13 aria_status
            Value::Text("queued".into()),                 // 14 internal_state
            opt_text(error_class.map(str::to_string)),    // 15 error_class
            Value::Null,                                  // 16 error_message
            Value::Null,                                  // 17 node_id
            Value::Null,                                  // 18 node_task_id
            Value::Integer(0),                            // 19 attempt_no
            Value::Integer(3),                            // 20 max_attempts
            Value::Integer(0),                            // 21 next_retry_at
            Value::Integer(0),                            // 22 possible_duplicate
            Value::Integer(now),                          // 23 created_at
            Value::Integer(now),                          // 24 updated_at
        ];

        match store.execute(INSERT_TASK_SQL, params).await {
            Ok(_) => {
                // 权威行已写入，接口即视为成功。审计 best-effort。
                let detail = serde_json::json!({
                    "origin": input.origin.as_str(),
                    "kind": input.kind.as_str(),
                    "save_folder_hint": input.save_folder_hint,
                    "max_connection_count": input.max_connection_count,
                })
                .to_string();
                if let Err(e) = store
                    .execute(
                        "INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at) \
                         VALUES ('info', 'task', NULL, ?1, 'task_accepted', ?2, ?3)",
                        vec![
                            Value::Text(task_id.clone()),
                            Value::Text(detail),
                            Value::Integer(now),
                        ],
                    )
                    .await
                {
                    tracing::warn!(error = %e, task_id = %task_id, "写 event_log 审计失败（不影响受理结果）");
                }
                return Ok(TaskCreated {
                    task_id,
                    gid,
                    group_id,
                });
            }
            Err(StoreError::Sqlite(e)) if is_gid_conflict(&e) => {
                tracing::warn!(gid = %gid, "GID 冲突，换新 GID 重试");
                continue;
            }
            Err(e) => return Err(CreateError::Store(e)),
        }
    }
    Err(CreateError::GidExhausted)
}

/// `INSERT INTO task` 的**唯一** SQL 文案（与 `V1__init.sql` 的列一一对应，无新列）。
pub(crate) const INSERT_TASK_SQL: &str = "\
INSERT INTO task (
  task_id, gid, group_id, source_key, kind, url_raw, url_norm, name,
  size_state, total_size, downloaded_size, permillage, aria_status, internal_state,
  error_class, error_message, node_id, node_task_id, attempt_no, max_attempts,
  next_retry_at, possible_duplicate, created_at, updated_at
) VALUES (
  ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8,
  ?9, ?10, ?11, ?12, ?13, ?14,
  ?15, ?16, ?17, ?18, ?19, ?20,
  ?21, ?22, ?23, ?24
)";

/// 判定是否是 `task.gid` 的唯一约束冲突（而非其它约束错误）。
fn is_gid_conflict(e: &rusqlite::Error) -> bool {
    matches!(
        e.sqlite_error_code(),
        Some(rusqlite::ErrorCode::ConstraintViolation)
    ) && e.to_string().contains("task.gid")
}

fn opt_text(v: Option<String>) -> Value {
    match v {
        Some(s) => Value::Text(s),
        None => Value::Null,
    }
}

fn opt_i64(v: Option<i64>) -> Value {
    match v {
        Some(i) => Value::Integer(i),
        None => Value::Null,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tasks::query::get_by_gid;

    async fn migrated_store() -> Store {
        let mut conn = crate::store::open_connection(":memory:").unwrap();
        let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("..")
            .join("migrations");
        crate::store::migrate::apply(&mut conn, &dir).unwrap();
        Store::from_connection(conn)
    }

    fn input(start_later: bool) -> TaskCreateInput {
        TaskCreateInput {
            kind: TaskKind::Http,
            url_raw: "http://example.com/a.bin".into(),
            url_norm: None,
            filename: Some("a.bin".into()),
            group_id: None,
            source_key: None,
            max_connection_count: Some(4),
            start_later,
            save_folder_hint: Some("/tmp/should-not-persist".into()),
            origin: IngressOrigin::Aria2,
        }
    }

    #[tokio::test]
    async fn start_later_false_is_waiting_without_error() {
        let store = migrated_store().await;
        let created = create_task(&store, input(false)).await.unwrap();
        let row = get_by_gid(&store, &created.gid).await.unwrap().unwrap();
        assert_eq!(row.aria_status, "waiting");
        assert_eq!(row.internal_state, "queued");
        assert_eq!(row.error_class, None);
        assert_eq!(row.node_id, None);
        store.shutdown().await;
    }

    #[tokio::test]
    async fn start_later_true_is_paused_not_started() {
        let store = migrated_store().await;
        let created = create_task(&store, input(true)).await.unwrap();
        let row = get_by_gid(&store, &created.gid).await.unwrap().unwrap();
        assert_eq!(row.aria_status, "paused");
        assert_eq!(row.error_class.as_deref(), Some("not_started"));
        store.shutdown().await;
    }

    #[tokio::test]
    async fn save_folder_hint_does_not_land_in_any_column() {
        let store = migrated_store().await;
        let created = create_task(&store, input(false)).await.unwrap();
        let row = get_by_gid(&store, &created.gid).await.unwrap().unwrap();
        // 任何文本列都不应包含那个提示串
        let haystack = format!(
            "{}{:?}{:?}{:?}{}",
            row.url_raw, row.url_norm, row.name, row.error_message, row.kind
        );
        assert!(
            !haystack.contains("should-not-persist"),
            "save_folder 不应落库"
        );
        store.shutdown().await;
    }

    #[tokio::test]
    async fn gid_conflict_retries_then_succeeds() {
        let store = migrated_store().await;
        // 预置一行，占住 GID "aaaaaaaaaaaaaaaa"
        let mut seed = input(false);
        seed.url_raw = "http://example.com/seed.bin".into();
        create_task_using(&store, seed, || "aaaaaaaaaaaaaaaa".to_string())
            .await
            .unwrap();

        // 生成器前两次返回冲突 GID，第三次返回新 GID → 应成功。
        let mut calls = 0;
        let created = create_task_using(&store, input(false), || {
            calls += 1;
            if calls <= 2 {
                "aaaaaaaaaaaaaaaa".to_string()
            } else {
                "bbbbbbbbbbbbbbbb".to_string()
            }
        })
        .await
        .unwrap();
        assert_eq!(created.gid, "bbbbbbbbbbbbbbbb");
        store.shutdown().await;
    }

    #[tokio::test]
    async fn gid_conflict_exhausts_after_eight_attempts() {
        let store = migrated_store().await;
        let mut seed = input(false);
        seed.url_raw = "http://example.com/seed2.bin".into();
        create_task_using(&store, seed, || "cccccccccccccccc".to_string())
            .await
            .unwrap();

        let r = create_task_using(&store, input(false), || "cccccccccccccccc".to_string()).await;
        assert!(matches!(r, Err(CreateError::GidExhausted)), "实际 {r:?}");
        store.shutdown().await;
    }

    #[tokio::test]
    async fn empty_url_is_rejected() {
        let store = migrated_store().await;
        let mut i = input(false);
        i.url_raw = "   ".into();
        assert!(matches!(
            create_task(&store, i).await,
            Err(CreateError::InvalidUrl(_))
        ));
        store.shutdown().await;
    }
}
