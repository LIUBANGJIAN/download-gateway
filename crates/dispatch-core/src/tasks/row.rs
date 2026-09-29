//! 任务行解码：列索引常量 + `ValueExt` + `decode_task`。
//!
//! # 为什么不给 `Store::query` 加列名映射
//!
//! 见 design-1 §3.0(d)：为量级（读 <10 行/次）引入「每命令带列元数据 + 每行分配 String」
//! 是负收益，且会触及 T01 已验收的核心。这里改用**手写列序常量**：
//! `TASK_COLUMNS`（25 列）与所有 `SELECT` 语句**逐字一致、顺序冻结**（禁止 `SELECT *`），
//! 再由 `debug_assert_eq!(row.len(), N_TASK_COLUMNS)` 在 debug 构建下捕获漂移。

use rusqlite::types::Value;

/// 任务表被 SELECT 的 25 列（顺序冻结，必须与 [`crate::tasks::query`] 的 SELECT 一致）。
pub const TASK_COLUMNS: &[&str] = &[
    "task_id",
    "gid",
    "group_id",
    "source_key",
    "kind",
    "url_raw",
    "url_norm",
    "name",
    "size_state",
    "total_size",
    "downloaded_size",
    "permillage",
    "aria_status",
    "internal_state",
    "error_class",
    "error_message",
    "node_id",
    "node_task_id",
    "attempt_no",
    "max_attempts",
    "next_retry_at",
    "possible_duplicate",
    "created_at",
    "updated_at",
    "completed_at",
];

/// 列数（= `TASK_COLUMNS.len()`）。
pub const N_TASK_COLUMNS: usize = 25;

pub const I_TASK_ID: usize = 0;
pub const I_GID: usize = 1;
pub const I_GROUP_ID: usize = 2;
pub const I_SOURCE_KEY: usize = 3;
pub const I_KIND: usize = 4;
pub const I_URL_RAW: usize = 5;
pub const I_URL_NORM: usize = 6;
pub const I_NAME: usize = 7;
pub const I_SIZE_STATE: usize = 8;
pub const I_TOTAL_SIZE: usize = 9;
pub const I_DOWNLOADED_SIZE: usize = 10;
pub const I_PERMILLAGE: usize = 11;
pub const I_ARIA_STATUS: usize = 12;
pub const I_INTERNAL_STATE: usize = 13;
pub const I_ERROR_CLASS: usize = 14;
pub const I_ERROR_MESSAGE: usize = 15;
pub const I_NODE_ID: usize = 16;
pub const I_NODE_TASK_ID: usize = 17;
pub const I_ATTEMPT_NO: usize = 18;
pub const I_MAX_ATTEMPTS: usize = 19;
pub const I_NEXT_RETRY_AT: usize = 20;
pub const I_POSSIBLE_DUPLICATE: usize = 21;
pub const I_CREATED_AT: usize = 22;
pub const I_UPDATED_AT: usize = 23;
pub const I_COMPLETED_AT: usize = 24;

/// 行解码错误。
#[derive(Debug, thiserror::Error)]
pub enum DecodeError {
    /// 列数不符。
    #[error("列数不符: 期望 {0} 实得 {1}")]
    Arity(usize, usize),
    /// 指定列类型不符。
    #[error("列 {0} 类型不符")]
    Type(usize),
}

/// 一条任务行（仅含被 SELECT 的 25 列）。
#[derive(Debug, Clone)]
pub struct TaskRow {
    pub task_id: String,
    pub gid: Option<String>,
    pub group_id: Option<i64>,
    pub source_key: Option<String>,
    pub kind: String,
    pub url_raw: String,
    pub url_norm: Option<String>,
    pub name: Option<String>,
    pub size_state: String,
    pub total_size: Option<i64>,
    pub downloaded_size: i64,
    pub permillage: i64,
    pub aria_status: String,
    pub internal_state: String,
    pub error_class: Option<String>,
    pub error_message: Option<String>,
    pub node_id: Option<i64>,
    pub node_task_id: Option<String>,
    pub attempt_no: i64,
    pub max_attempts: i64,
    pub next_retry_at: i64,
    pub possible_duplicate: bool,
    pub created_at: i64,
    pub updated_at: i64,
    pub completed_at: Option<i64>,
}

/// 按列索引从 `Value` 取值，类型不符即报错。
pub trait ValueExt {
    fn as_i64(&self, idx: usize) -> Result<i64, DecodeError>;
    fn as_opt_i64(&self, idx: usize) -> Result<Option<i64>, DecodeError>;
    fn as_text(&self, idx: usize) -> Result<String, DecodeError>;
    fn as_opt_text(&self, idx: usize) -> Result<Option<String>, DecodeError>;
    fn as_bool(&self, idx: usize) -> Result<bool, DecodeError>;
}

impl ValueExt for [Value] {
    fn as_i64(&self, idx: usize) -> Result<i64, DecodeError> {
        match self.get(idx) {
            Some(Value::Integer(v)) => Ok(*v),
            _ => Err(DecodeError::Type(idx)),
        }
    }

    fn as_opt_i64(&self, idx: usize) -> Result<Option<i64>, DecodeError> {
        match self.get(idx) {
            Some(Value::Null) => Ok(None),
            Some(Value::Integer(v)) => Ok(Some(*v)),
            _ => Err(DecodeError::Type(idx)),
        }
    }

    fn as_text(&self, idx: usize) -> Result<String, DecodeError> {
        match self.get(idx) {
            Some(Value::Text(v)) => Ok(v.clone()),
            _ => Err(DecodeError::Type(idx)),
        }
    }

    fn as_opt_text(&self, idx: usize) -> Result<Option<String>, DecodeError> {
        match self.get(idx) {
            Some(Value::Null) => Ok(None),
            Some(Value::Text(v)) => Ok(Some(v.clone())),
            _ => Err(DecodeError::Type(idx)),
        }
    }

    fn as_bool(&self, idx: usize) -> Result<bool, DecodeError> {
        match self.get(idx) {
            Some(Value::Integer(v)) => Ok(*v != 0),
            _ => Err(DecodeError::Type(idx)),
        }
    }
}

/// 把一行 `Vec<Value>` 解码为 [`TaskRow`]。
pub fn decode_task(row: &[Value]) -> Result<TaskRow, DecodeError> {
    if row.len() != N_TASK_COLUMNS {
        return Err(DecodeError::Arity(N_TASK_COLUMNS, row.len()));
    }
    Ok(TaskRow {
        task_id: row.as_text(I_TASK_ID)?,
        gid: row.as_opt_text(I_GID)?,
        group_id: row.as_opt_i64(I_GROUP_ID)?,
        source_key: row.as_opt_text(I_SOURCE_KEY)?,
        kind: row.as_text(I_KIND)?,
        url_raw: row.as_text(I_URL_RAW)?,
        url_norm: row.as_opt_text(I_URL_NORM)?,
        name: row.as_opt_text(I_NAME)?,
        size_state: row.as_text(I_SIZE_STATE)?,
        total_size: row.as_opt_i64(I_TOTAL_SIZE)?,
        downloaded_size: row.as_i64(I_DOWNLOADED_SIZE)?,
        permillage: row.as_i64(I_PERMILLAGE)?,
        aria_status: row.as_text(I_ARIA_STATUS)?,
        internal_state: row.as_text(I_INTERNAL_STATE)?,
        error_class: row.as_opt_text(I_ERROR_CLASS)?,
        error_message: row.as_opt_text(I_ERROR_MESSAGE)?,
        node_id: row.as_opt_i64(I_NODE_ID)?,
        node_task_id: row.as_opt_text(I_NODE_TASK_ID)?,
        attempt_no: row.as_i64(I_ATTEMPT_NO)?,
        max_attempts: row.as_i64(I_MAX_ATTEMPTS)?,
        next_retry_at: row.as_i64(I_NEXT_RETRY_AT)?,
        possible_duplicate: row.as_bool(I_POSSIBLE_DUPLICATE)?,
        created_at: row.as_i64(I_CREATED_AT)?,
        updated_at: row.as_i64(I_UPDATED_AT)?,
        completed_at: row.as_opt_i64(I_COMPLETED_AT)?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> Vec<Value> {
        vec![
            Value::Text("01JABC".into()),           // task_id
            Value::Text("deadbeefdeadbeef".into()), // gid
            Value::Null,                            // group_id
            Value::Null,                            // source_key
            Value::Text("http".into()),             // kind
            Value::Text("http://e/a.bin".into()),   // url_raw
            Value::Null,                            // url_norm
            Value::Text("a.bin".into()),            // name
            Value::Text("unknown".into()),          // size_state
            Value::Null,                            // total_size
            Value::Integer(0),                      // downloaded_size
            Value::Integer(0),                      // permillage
            Value::Text("waiting".into()),          // aria_status
            Value::Text("queued".into()),           // internal_state
            Value::Null,                            // error_class
            Value::Null,                            // error_message
            Value::Null,                            // node_id
            Value::Null,                            // node_task_id
            Value::Integer(0),                      // attempt_no
            Value::Integer(3),                      // max_attempts
            Value::Integer(0),                      // next_retry_at
            Value::Integer(0),                      // possible_duplicate
            Value::Integer(1_700_000_000_000),      // created_at
            Value::Integer(1_700_000_000_000),      // updated_at
            Value::Null,                            // completed_at
        ]
    }

    #[test]
    fn decodes_full_fixture() {
        let r = decode_task(&fixture()).unwrap();
        assert_eq!(r.task_id, "01JABC");
        assert_eq!(r.gid.as_deref(), Some("deadbeefdeadbeef"));
        assert_eq!(r.group_id, None);
        assert_eq!(r.downloaded_size, 0);
        assert!(!r.possible_duplicate);
        assert_eq!(r.completed_at, None);
    }

    #[test]
    fn wrong_arity_is_rejected() {
        let mut f = fixture();
        f.pop();
        assert!(matches!(decode_task(&f), Err(DecodeError::Arity(25, 24))));
    }

    #[test]
    fn type_mismatch_is_rejected() {
        let mut f = fixture();
        f[I_DOWNLOADED_SIZE] = Value::Text("not-a-number".into());
        assert!(matches!(
            decode_task(&f),
            Err(DecodeError::Type(I_DOWNLOADED_SIZE))
        ));
    }

    #[test]
    fn null_columns_become_none() {
        let r = decode_task(&fixture()).unwrap();
        assert!(r.url_norm.is_none());
        assert!(r.error_class.is_none());
        assert!(r.node_id.is_none());
    }
}
