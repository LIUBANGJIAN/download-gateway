//! 迁移框架。
//!
//! 约定：`migrations/V<n>__<name>.sql`，按 `n` 升序应用；
//! 已应用版本记录在 `schema_migrations`。
//!
//! # 为什么 `schema_migrations` 由 `V1__init.sql` 自己创建
//!
//! `02 §3.2` 的 DDL 里第 0 张表就是 `schema_migrations`，且 `04-ddl-merged-v2.sql`
//! 与之**逐表逐列一致**（由 `02 §8` 的 A-4 交叉比对守着）。
//! 若框架另行创建这张表，就会与设计文档的 DDL 产生"两份定义"。
//! 因此这里的顺序是：**先探测表是否存在 → 不存在则视为"零个已应用版本" →
//! 应用 V1（其中会建表）→ 再插入版本行**。

use std::fs;
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use rusqlite::Connection;

/// 一个已发现的迁移文件。
#[derive(Debug, Clone)]
pub struct Migration {
    /// 版本号（`V1__init.sql` → `1`）。
    pub version: i64,
    /// 迁移名（`V1__init.sql` → `init`）。
    pub name: String,
    /// 文件绝对/相对路径。
    pub path: PathBuf,
}

/// 迁移相关错误。
#[derive(Debug, thiserror::Error)]
pub enum MigrateError {
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    #[error("sqlite: {0}")]
    Sqlite(#[from] rusqlite::Error),
    #[error("迁移文件名不合规（期望 V<number>__<name>.sql）：{0}")]
    BadName(String),
    #[error("迁移版本号无法解析：{0}")]
    BadVersion(String),
}

/// 扫描迁移目录，返回按版本升序排列的迁移列表。
///
/// 目录不存在时返回空列表（不报错）—— 便于测试与最小部署。
pub fn discover(dir: &Path) -> Result<Vec<Migration>, MigrateError> {
    let mut out = Vec::new();
    if !dir.is_dir() {
        return Ok(out);
    }
    for entry in fs::read_dir(dir)? {
        let entry = entry?;
        let file_name = entry.file_name().to_string_lossy().to_string();
        if !file_name.ends_with(".sql") {
            continue;
        }
        let stem = &file_name[..file_name.len() - 4];
        let (vpart, name) = stem
            .split_once("__")
            .ok_or_else(|| MigrateError::BadName(file_name.clone()))?;
        let version: i64 = vpart
            .trim_start_matches(['V', 'v'])
            .parse()
            .map_err(|_| MigrateError::BadVersion(file_name.clone()))?;
        out.push(Migration {
            version,
            name: name.to_string(),
            path: entry.path(),
        });
    }
    out.sort_by_key(|m| m.version);
    Ok(out)
}

fn table_exists(conn: &Connection, name: &str) -> Result<bool, rusqlite::Error> {
    let n: i64 = conn.query_row(
        "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?1",
        [name],
        |row| row.get(0),
    )?;
    Ok(n > 0)
}

/// 已应用的版本号（升序）。表不存在时返回空 —— 这正是首次启动的状态。
pub fn applied_versions(conn: &Connection) -> Result<Vec<i64>, rusqlite::Error> {
    if !table_exists(conn, "schema_migrations")? {
        return Ok(Vec::new());
    }
    let mut stmt = conn.prepare("SELECT version FROM schema_migrations ORDER BY version")?;
    let rows = stmt.query_map([], |row| row.get::<_, i64>(0))?;
    rows.collect()
}

/// 兜底建表语句：见 [`apply`] 里的说明。
const ENSURE_MIGRATIONS_TABLE: &str = "\
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT    NOT NULL,
  applied_at INTEGER NOT NULL
);";

fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// 应用所有未执行的迁移，返回**本次**应用的数量（幂等：重复调用返回 0）。
///
/// 每个迁移包在一个事务里：SQL 与"版本登记"要么都成功、要么都回滚，
/// 不会出现"表建好了但没记版本"的半成品状态。
pub fn apply(conn: &mut Connection, dir: &Path) -> Result<usize, MigrateError> {
    let found = discover(dir)?;
    let done = applied_versions(conn)?;
    let mut applied = 0usize;
    for m in found {
        if done.contains(&m.version) {
            continue;
        }
        let sql = fs::read_to_string(&m.path)?;
        let tx = conn.transaction()?;
        tx.execute_batch(&sql)?;
        // 兜底建表。`V1__init.sql` 自己会创建 `schema_migrations`
        // （与 `02 §3.2` 的 DDL 逐字一致），但**框架不能假设每个迁移文件都这么做** ——
        // 否则一个不含该表定义的迁移会在「登记版本」这一步直接炸掉：
        //   实测 Err: no such table: schema_migrations
        // `IF NOT EXISTS` 让两种情况都幂等：V1 已建则此处是 no-op。
        tx.execute_batch(ENSURE_MIGRATIONS_TABLE)?;
        tx.execute(
            "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?1, ?2, ?3)",
            rusqlite::params![m.version, m.name, now_secs()],
        )?;
        tx.commit()?;
        applied += 1;
    }
    Ok(applied)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn applies_once_then_is_idempotent() {
        let dir = tempfile::tempdir().unwrap();
        fs::write(
            dir.path().join("V1__init.sql"),
            "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL);",
        )
        .unwrap();

        let mut conn = Connection::open_in_memory().unwrap();
        conn.execute_batch("PRAGMA foreign_keys = ON;").unwrap();

        assert_eq!(apply(&mut conn, dir.path()).unwrap(), 1, "首次应应用 1 个");
        assert_eq!(apply(&mut conn, dir.path()).unwrap(), 0, "二次应为幂等");
        assert_eq!(applied_versions(&conn).unwrap(), vec![1]);
    }

    #[test]
    fn applies_in_version_order_regardless_of_fs_order() {
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join("V2__b.sql"), "CREATE TABLE b (x INTEGER);").unwrap();
        fs::write(dir.path().join("V1__a.sql"), "CREATE TABLE a (x INTEGER);").unwrap();
        let mut conn = Connection::open_in_memory().unwrap();
        assert_eq!(apply(&mut conn, dir.path()).unwrap(), 2);
        assert_eq!(applied_versions(&conn).unwrap(), vec![1, 2]);
    }

    #[test]
    fn rejects_malformed_filename() {
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join("init.sql"), "SELECT 1;").unwrap();
        let mut conn = Connection::open_in_memory().unwrap();
        assert!(matches!(
            apply(&mut conn, dir.path()),
            Err(MigrateError::BadName(_))
        ));
    }

    #[test]
    fn missing_dir_yields_empty_plan() {
        let mut conn = Connection::open_in_memory().unwrap();
        let n = apply(&mut conn, Path::new("这/个/目/录/不/存/在")).unwrap();
        assert_eq!(n, 0);
    }

    /// 迁移失败必须**整体回滚**：不能留下"表建了但版本没记"的状态。
    #[test]
    fn failed_migration_rolls_back_completely() {
        let dir = tempfile::tempdir().unwrap();
        fs::write(
            dir.path().join("V1__broken.sql"),
            "CREATE TABLE ok_table (x INTEGER);\nCREATE TABLE ok_table (x INTEGER);",
        )
        .unwrap();
        let mut conn = Connection::open_in_memory().unwrap();
        assert!(apply(&mut conn, dir.path()).is_err(), "坏迁移应报错");

        let exists: i64 = conn
            .query_row(
                "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ok_table'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(exists, 0, "失败迁移留下了半成品表（事务未回滚）");
    }

    /// 真实场景：`V1__init.sql` **自己带** `schema_migrations` 建表语句
    /// （与 `02 §3.2` 的 DDL 逐字一致）。此时兜底的 `IF NOT EXISTS` 必须是 no-op，
    /// 不能因为"表已存在"而报错。
    #[test]
    fn works_when_migration_creates_its_own_schema_migrations() {
        let dir = tempfile::tempdir().unwrap();
        fs::write(
            dir.path().join("V1__init.sql"),
            "CREATE TABLE schema_migrations (
                 version    INTEGER PRIMARY KEY,
                 name       TEXT    NOT NULL,
                 applied_at INTEGER NOT NULL
             );
             CREATE TABLE t (id INTEGER PRIMARY KEY);",
        )
        .unwrap();
        let mut conn = Connection::open_in_memory().unwrap();
        assert_eq!(
            apply(&mut conn, dir.path()).unwrap(),
            1,
            "不应因表已存在而冲突"
        );
        assert_eq!(applied_versions(&conn).unwrap(), vec![1]);
        let n: i64 = conn
            .query_row(
                "SELECT COUNT(*) FROM sqlite_master WHERE name = 't'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(n, 1, "业务表应已建立");
        assert_eq!(apply(&mut conn, dir.path()).unwrap(), 0, "二次仍幂等");
    }

    /// **T01 验收的硬证据**：拿**真实的** `migrations/V1__init.sql` 跑一遍，
    /// 断言的不只是"迁移返回 1"，而是 DDL 的**实际效果**
    /// （14 表 / 17 显式索引 / 外键自检为空）。
    #[test]
    fn real_v1_migration_produces_14_tables_and_17_indexes() {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("..")
            .join("..")
            .join("migrations");
        assert!(
            dir.join("V1__init.sql").is_file(),
            "找不到真实迁移文件：{}",
            dir.display()
        );

        let tmp = tempfile::tempdir().unwrap();
        let mut conn = Connection::open(tmp.path().join("real.db")).unwrap();
        conn.execute_batch("PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;")
            .unwrap();

        assert_eq!(apply(&mut conn, &dir).unwrap(), 1, "应恰好应用 V1");
        assert_eq!(applied_versions(&conn).unwrap(), vec![1]);

        let tables: i64 = conn
            .query_row(
                "SELECT COUNT(*) FROM sqlite_master
                  WHERE type = 'table' AND name NOT LIKE 'sqlite_%'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(tables, 14, "表数应为 14（含 schema_migrations）");

        let indexes: i64 = conn
            .query_row(
                "SELECT COUNT(*) FROM sqlite_master
                  WHERE type = 'index' AND name NOT LIKE 'sqlite_%'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(indexes, 17, "显式索引应为 17");

        // 外键自检必须干净 —— 注意 CTAS 重建表会让这里**抛异常**而非返回空，
        // 所以"能正常跑出 0 行"本身就是一种约束完整性的证明。
        let mut stmt = conn.prepare("PRAGMA foreign_key_check").unwrap();
        let violations = stmt.query_map([], |_| Ok(())).unwrap().count();
        assert_eq!(violations, 0, "外键自检应无违规");

        // 关键表都在
        for t in [
            "node",
            "source_profile",
            "task_group",
            "task",
            "task_attempt",
            "task_file",
            "dedup_index",
            "task_progress",
            "dispatch_log",
            "node_health",
            "event_log",
            "app_config",
            "group_binding_log",
        ] {
            let n: i64 = conn
                .query_row(
                    "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?1",
                    [t],
                    |r| r.get(0),
                )
                .unwrap();
            assert_eq!(n, 1, "缺表：{t}");
        }

        // 部分唯一索引（INV-A5）必须在
        let ux: i64 = conn
            .query_row(
                "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='ux_task_group_episode'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(ux, 1, "缺 INV-A5 的部分唯一索引 ux_task_group_episode");
    }
}
