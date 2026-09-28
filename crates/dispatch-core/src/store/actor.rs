//! SQLite 单写者 actor 的实现。
//!
//! 见 [`crate::store`] 的模块文档，了解为什么用 actor 而不是连接池。

use std::path::Path;
use std::thread;

use rusqlite::Connection;
use rusqlite::params_from_iter;
use rusqlite::types::Value;
use tokio::sync::{mpsc, oneshot};

use super::BOOTSTRAP_PRAGMAS;

/// 存储层错误。
#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    /// SQLite 自身的错误（含约束冲突、语法错误等）。
    #[error("sqlite: {0}")]
    Sqlite(#[from] rusqlite::Error),
    /// 文件系统错误（建目录等）。
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    /// actor 线程已退出，channel 关闭。
    #[error("store actor 已退出（channel 已关闭）")]
    ActorGone,
}

type Reply<T> = oneshot::Sender<Result<T, StoreError>>;

/// 发给写者线程的命令。
enum Cmd {
    Execute {
        sql: String,
        params: Vec<Value>,
        reply: Reply<usize>,
    },
    Batch {
        sql: String,
        reply: Reply<()>,
    },
    Query {
        sql: String,
        params: Vec<Value>,
        reply: Reply<Vec<Vec<Value>>>,
    },
    Shutdown {
        reply: oneshot::Sender<()>,
    },
}

/// 数据库句柄。`Clone` 廉价（内部只有一个 `mpsc::Sender`）。
#[derive(Clone)]
pub struct Store {
    tx: mpsc::Sender<Cmd>,
}

impl Store {
    /// 打开（或创建）磁盘库，并启动唯一的写者线程。
    pub fn open(path: impl AsRef<Path>) -> Result<Self, StoreError> {
        let path = path.as_ref();
        if let Some(parent) = path.parent()
            && !parent.as_os_str().is_empty()
        {
            std::fs::create_dir_all(parent)?;
        }
        let conn = Connection::open(path)?;
        conn.execute_batch(BOOTSTRAP_PRAGMAS)?;
        Ok(Self::from_connection(conn))
    }

    /// 用内存库启动（仅测试）。
    pub fn open_in_memory() -> Result<Self, StoreError> {
        let conn = Connection::open_in_memory()?;
        conn.execute_batch("PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;")?;
        Ok(Self::from_connection(conn))
    }

    /// 把一个已配置好的连接交给 actor。
    ///
    /// 启动顺序：`open_connection()` → `migrate::apply()` → `from_connection()`。
    pub fn from_connection(conn: Connection) -> Self {
        let (tx, rx) = mpsc::channel(4096);
        thread::Builder::new()
            .name("store-writer".to_string())
            .spawn(move || run_worker(conn, rx))
            .expect("无法启动 store-writer 线程");
        Store { tx }
    }

    /// 执行一条写语句（返回受影响行数）。
    pub async fn execute(
        &self,
        sql: impl Into<String>,
        params: Vec<Value>,
    ) -> Result<usize, StoreError> {
        let (reply, rx) = oneshot::channel();
        self.tx
            .send(Cmd::Execute {
                sql: sql.into(),
                params,
                reply,
            })
            .await
            .map_err(|_| StoreError::ActorGone)?;
        rx.await.map_err(|_| StoreError::ActorGone)?
    }

    /// 执行一段 SQL 脚本（建表等）。
    pub async fn execute_batch(&self, sql: impl Into<String>) -> Result<(), StoreError> {
        let (reply, rx) = oneshot::channel();
        self.tx
            .send(Cmd::Batch {
                sql: sql.into(),
                reply,
            })
            .await
            .map_err(|_| StoreError::ActorGone)?;
        rx.await.map_err(|_| StoreError::ActorGone)?
    }

    /// 查询，返回按行组织的结果（每行是列值的 `Vec`）。
    pub async fn query(
        &self,
        sql: impl Into<String>,
        params: Vec<Value>,
    ) -> Result<Vec<Vec<Value>>, StoreError> {
        let (reply, rx) = oneshot::channel();
        self.tx
            .send(Cmd::Query {
                sql: sql.into(),
                params,
                reply,
            })
            .await
            .map_err(|_| StoreError::ActorGone)?;
        rx.await.map_err(|_| StoreError::ActorGone)?
    }

    /// 关停写者线程并等待其结束（优雅退出 / 测试收尾）。
    pub async fn shutdown(&self) {
        let (reply, rx) = oneshot::channel();
        if self.tx.send(Cmd::Shutdown { reply }).await.is_ok() {
            let _ = rx.await;
        }
    }
}

/// 写者线程主循环：**顺序**处理命令，这是"无 `SQLITE_BUSY`"的根因。
fn run_worker(conn: Connection, mut rx: mpsc::Receiver<Cmd>) {
    while let Some(cmd) = rx.blocking_recv() {
        match cmd {
            Cmd::Execute { sql, params, reply } => {
                let r = conn.execute(&sql, params_from_iter(params.iter()));
                let _ = reply.send(r.map_err(StoreError::from));
            }
            Cmd::Batch { sql, reply } => {
                let r = conn.execute_batch(&sql);
                let _ = reply.send(r.map_err(StoreError::from));
            }
            Cmd::Query { sql, params, reply } => {
                let _ = reply.send(query_impl(&conn, &sql, &params));
            }
            Cmd::Shutdown { reply } => {
                let _ = reply.send(());
                break;
            }
        }
    }
}

fn query_impl(
    conn: &Connection,
    sql: &str,
    params: &[Value],
) -> Result<Vec<Vec<Value>>, StoreError> {
    let mut stmt = conn.prepare(sql)?;
    let ncols = stmt.column_count();
    let mut rows = stmt.query(params_from_iter(params.iter()))?;
    let mut out = Vec::new();
    while let Some(row) = rows.next()? {
        let mut vals = Vec::with_capacity(ncols);
        for i in 0..ncols {
            let v: Value = row.get(i)?;
            vals.push(v);
        }
        out.push(vals);
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// **T01 的核心验收**：并发写不出 `SQLITE_BUSY`。
    ///
    /// 注意这不只是"跑得通"——它证明了**架构层面的串行化**：
    /// 128 个 tokio 任务同时发起写，全部成功且计数精确。
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn concurrent_writes_are_serialized_without_busy() {
        let store = Store::open_in_memory().unwrap();
        store
            .execute(
                "CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v INTEGER NOT NULL)",
                vec![],
            )
            .await
            .unwrap();

        const N: i64 = 128;
        let mut handles = Vec::new();
        for i in 0..N {
            let s = store.clone();
            handles.push(tokio::spawn(async move {
                s.execute("INSERT INTO t (v) VALUES (?)", vec![Value::Integer(i)])
                    .await
            }));
        }
        for h in handles {
            let r = h.await.expect("并发写任务 panic");
            assert!(r.is_ok(), "并发写失败（疑似 SQLITE_BUSY）: {:?}", r.err());
        }

        let rows = store.query("SELECT COUNT(*) FROM t", vec![]).await.unwrap();
        assert_eq!(rows[0][0], Value::Integer(N), "行数应为 {}", N);
        let sums = store.query("SELECT SUM(v) FROM t", vec![]).await.unwrap();
        assert_eq!(sums[0][0], Value::Integer((0..N).sum()), "和应精确");

        store.shutdown().await;
    }

    #[tokio::test]
    async fn writes_are_visible_to_subsequent_reads() {
        let store = Store::open_in_memory().unwrap();
        store
            .execute(
                "CREATE TABLE k (a INTEGER PRIMARY KEY, b TEXT NOT NULL)",
                vec![],
            )
            .await
            .unwrap();
        store
            .execute(
                "INSERT INTO k (a, b) VALUES (?, ?)",
                vec![Value::Integer(1), Value::Text("x".into())],
            )
            .await
            .unwrap();
        let r = store
            .query("SELECT b FROM k WHERE a = ?", vec![Value::Integer(1)])
            .await
            .unwrap();
        assert_eq!(r[0][0], Value::Text("x".into()));
    }

    /// `PRAGMA foreign_keys = ON` 必须真的生效 —— 这是 `02 §3.2` 全部
    /// `REFERENCES` 有意义的前提。
    #[tokio::test]
    async fn foreign_keys_are_enforced() {
        let store = Store::open_in_memory().unwrap();
        store
            .execute_batch(
                "CREATE TABLE parent (id INTEGER PRIMARY KEY);
                 CREATE TABLE child (
                     id INTEGER PRIMARY KEY,
                     p  INTEGER NOT NULL REFERENCES parent(id)
                 );",
            )
            .await
            .unwrap();
        let bad = store
            .execute("INSERT INTO child (id, p) VALUES (1, 999)", vec![])
            .await;
        assert!(bad.is_err(), "外键未生效：PRAGMA foreign_keys 没打开");
    }

    /// constraint 冲突要以错误形式返回，而不是静默成功。
    #[tokio::test]
    async fn unique_violation_surfaces_as_error() {
        let store = Store::open_in_memory().unwrap();
        store
            .execute("CREATE TABLE u (k TEXT PRIMARY KEY)", vec![])
            .await
            .unwrap();
        store
            .execute("INSERT INTO u (k) VALUES ('a')", vec![])
            .await
            .unwrap();
        let dup = store
            .execute("INSERT INTO u (k) VALUES ('a')", vec![])
            .await;
        assert!(dup.is_err(), "重复主键应报错");
    }
}
