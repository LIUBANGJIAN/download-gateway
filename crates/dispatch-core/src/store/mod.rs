//! 存储层：SQLite 单写者 actor + 迁移框架。
//!
//! # 为什么是「单写者 actor」而不是连接池
//!
//! 本项目的正确性建立在**若干全局不变量**上（`02 §2.2.3` 去重键唯一、
//! `05 §10` 的 `INV-A1/A3/A5`），这些不变量都要在**权威库**上用 SQL 断言验证。
//! 因此写路径必须满足两个性质：
//!
//! 1. **顺序性**：`task_attempt` 与投递**同事务**写入（`02 §3.2.1` ①）。
//!    若允许多写者并发，事务交错会让「投递时刻一致性」断言失去意义。
//! 2. **无 `SQLITE_BUSY`**：不是靠 `busy_timeout` 重试去掩盖冲突，
//!    而是**从架构上**让 Rust 侧根本没有并发写 —— 所有写经一条 `mpsc`
//!    排队，由**唯一一个 OS 线程**持有 `Connection` 顺序执行。
//!
//! 读路径（`query`）同样走这条通道，代价是读也要排队；
//! 对本项目的量级（写 ~10 行/秒）完全够用，换来的是**零并发事务**的简单心智模型。

pub mod actor;
pub mod migrate;

pub use actor::{Store, StoreError};

use std::path::Path;

use rusqlite::Connection;

/// 连接引导 PRAGMA。
///
/// - `journal_mode = WAL`：读不阻塞写（虽然是单写者，但健康探测/查询仍会读）。
/// - `foreign_keys = ON`：**必须显式开启**，SQLite 默认关闭。
/// - `busy_timeout`：作为"单写者假设被破坏时"的兜底，不作为主要机制。
pub const BOOTSTRAP_PRAGMAS: &str = "\
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
";

/// 打开磁盘库连接（自动建父目录 + 设 PRAGMA）。
///
/// 用于「**先迁移、再把连接交给 actor**」的启动顺序 ——
/// 迁移需要 `&mut Connection` 来开事务，不能在 actor 线程里做。
pub fn open_connection(path: impl AsRef<Path>) -> Result<Connection, StoreError> {
    let path = path.as_ref();
    if let Some(parent) = path.parent()
        && !parent.as_os_str().is_empty()
    {
        std::fs::create_dir_all(parent)?;
    }
    let conn = Connection::open(path)?;
    conn.execute_batch(BOOTSTRAP_PRAGMAS)?;
    Ok(conn)
}
