// migrate.go —— 迁移框架（对 crates/dispatch-core/src/store/migrate.rs 的 1:1 移植）。
//
// 约定：`migrations/V<n>__<name>.sql`，按 `n` 升序应用；已应用版本记录在 `schema_migrations`。
//
// # 为什么 schema_migrations 由 V1__init.sql 自己创建
//
// `02 §3.2` 的 DDL 里第 0 张表就是 schema_migrations，且 `04-ddl-merged-v2.sql`
// 与之逐表逐列一致。若框架另行创建这张表，就会与设计文档的 DDL 产生「两份定义」。
// 因此顺序是：**先探测表是否存在 → 不存在则视为「零个已应用版本」→ 应用 V1
// （其中会建表）→ 再插入版本行**。
//
// # 为什么这里收 *sql.Conn 而不是 *sql.DB
//
// 收 `*sql.Conn` 是对 Rust `apply(conn: &mut Connection)` 的**最贴近映射**：
// 它把「迁移恰好跑在一个独立物理连接上、绝不会去写池抢第二条连接」这一约束
// **写进类型签名**（这也正是包文档里的 Go 特有坑 3）。调用方必须先
// `db.Conn(ctx)` 显式取一条连接，无从把它误接到受 `SetMaxOpenConns(1)` 限制的写池上。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migration 是一个已发现的迁移文件。对齐 Rust `migrate::Migration`。
type Migration struct {
	// Version 是版本号（`V1__init.sql` → 1）。
	Version int64
	// Name 是迁移名（`V1__init.sql` → init）。
	Name string
	// Path 是文件路径（dir 与文件名拼接）。
	Path string
}

// BadNameError 表示迁移文件名不合规（缺少 `__` 分隔）。
// 对齐 Rust `MigrateError::BadName`。
type BadNameError struct {
	File string
}

func (e *BadNameError) Error() string {
	return fmt.Sprintf("迁移文件名不合规（期望 V<number>__<name>.sql）：%s", e.File)
}

// BadVersionError 表示迁移版本号无法解析。对齐 Rust `MigrateError::BadVersion`。
type BadVersionError struct {
	File string
}

func (e *BadVersionError) Error() string {
	return fmt.Sprintf("迁移版本号无法解析：%s", e.File)
}

// ensureMigrationsTable 是兜底建表语句，见 Apply 里的说明。
// 与 V1__init.sql 中 schema_migrations 的定义**逐字一致**。
const ensureMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT    NOT NULL,
  applied_at INTEGER NOT NULL
);`

// Discover 扫描迁移目录，返回按版本升序排列的迁移列表。
//
// 目录不存在（或不是目录）时返回空列表且**不报错**——便于测试与最小部署，
// 对齐 Rust `discover`（`if !dir.is_dir() { return Ok(out) }`）。
func Discover(dir string) ([]Migration, error) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		// 对齐 Rust：不是目录（含不存在、无权限）一律视为「空计划」，不报错。
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: 读取迁移目录 %q: %w", dir, err)
	}

	var out []Migration
	for _, e := range entries {
		fileName := e.Name()
		if !strings.HasSuffix(fileName, ".sql") {
			continue
		}
		stem := strings.TrimSuffix(fileName, ".sql")
		vpart, name, ok := strings.Cut(stem, "__")
		if !ok {
			return nil, &BadNameError{File: fileName}
		}
		version, err := strconv.ParseInt(strings.TrimLeft(vpart, "Vv"), 10, 64)
		if err != nil {
			return nil, &BadVersionError{File: fileName}
		}
		out = append(out, Migration{
			Version: version,
			Name:    name,
			Path:    filepath.Join(dir, fileName),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// tableExists 判断某张表是否存在于 sqlite_master。
func tableExists(conn *sql.Conn, name string) (bool, error) {
	var n int64
	err := conn.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// AppliedVersions 返回已应用的版本号（升序）。
//
// 表不存在时返回空——这正是首次启动的状态。对齐 Rust `applied_versions`。
func AppliedVersions(conn *sql.Conn) ([]int64, error) {
	exists, err := tableExists(conn, "schema_migrations")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	rows, err := conn.QueryContext(context.Background(),
		"SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Apply 应用所有未执行的迁移，返回**本次**应用的数量（幂等：重复调用返回 0）。
//
// 每个迁移包在一个事务里：SQL 与「版本登记」要么都成功、要么都回滚，
// 不会出现「表建好了但没记版本」的半成品状态。对齐 Rust `apply`。
//
// 兜底建表：`V1__init.sql` 自己会创建 schema_migrations（与设计 DDL 逐字一致），
// 但**框架不能假设每个迁移文件都这么做**——否则一个不含该表定义的迁移会在
// 「登记版本」这一步直接炸掉（`no such table: schema_migrations`）。
// `IF NOT EXISTS` 让两种情况都幂等：V1 已建则此处是 no-op。
func Apply(conn *sql.Conn, dir string) (int, error) {
	found, err := Discover(dir)
	if err != nil {
		return 0, err
	}
	done, err := AppliedVersions(conn)
	if err != nil {
		return 0, err
	}
	doneSet := make(map[int64]bool, len(done))
	for _, v := range done {
		doneSet[v] = true
	}

	ctx := context.Background()
	applied := 0
	for _, m := range found {
		if doneSet[m.Version] {
			continue
		}
		body, err := os.ReadFile(m.Path)
		if err != nil {
			return applied, fmt.Errorf("migrate: 读取 %q: %w", m.Path, err)
		}

		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return applied, fmt.Errorf("migrate: 开启事务: %w", err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return applied, fmt.Errorf("migrate: 执行 %q: %w", m.Path, err)
		}
		if _, err := tx.ExecContext(ctx, ensureMigrationsTable); err != nil {
			_ = tx.Rollback()
			return applied, fmt.Errorf("migrate: 兜底建表: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
			m.Version, m.Name, nowSecs()); err != nil {
			_ = tx.Rollback()
			return applied, fmt.Errorf("migrate: 登记版本 %d: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return applied, fmt.Errorf("migrate: 提交 %q: %w", m.Path, err)
		}
		applied++
	}
	return applied, nil
}

// nowSecs 返回当前 Unix 秒。对齐 Rust `migrate::now_secs`。
func nowSecs() int64 { return time.Now().Unix() }
