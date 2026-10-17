package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// closeGuardTimeout 是收尾守卫的超时上限（见 closeConnDBWithTimeout）。
//
// 正常收尾（conn.Close + db.Close）在内存/临时磁盘库上应为微秒级；2s 是极宽松的余量，
// 只用来把「事务未归还导致 Close 永阻塞」这类**悬挂**与「正常但稍慢」区分开。
const closeGuardTimeout = 2 * time.Second

// closeConnDBWithTimeout 是**带超时的收尾守卫**，供各测试的 `t.Cleanup` 使用。
//
// # 为什么需要它（真因，2026-10-09）
//
// 断言其实**已经打印**了，但随后 `t.Cleanup` 卡在 `conn.Close()`：
// `database/sql` 的 `Conn.Close()` 需拿**写锁**，而一个在途事务持**读锁**
// （`closemu` 的 `closingMutex`）。因此只要有一个事务没 Commit/Rollback，
// `Close()` 会**永久阻塞**，测试进程被拖死 —— `--- FAIL:` 永不打印，
// 直到 `go test -timeout` 到点才以 `panic: test timed out after 2m0s` 收场。
// 该现象由变异 G4（删掉 `Apply` 里某处 `_ = tx.Rollback()`）稳定复现。
//
// # 它是一条**会红的判据**，不是"让测试别挂"的补丁
//
// 超时分支用 `t.Errorf`（不是 `t.Log`、更不吞掉），消息**点名真因**；
// 且超时后**不再等待** Close（不阻塞测试进程，让 `--- FAIL` 立刻打印）。
// 于是「事务泄漏」从"约 2 分钟的整包 panic"变成"秒级的、指名道姓的 FAIL"。
//
// ⚠️ 本守卫**只动收尾**，绝不触碰单写者池锚点（`SetMaxOpenConns(1)` /
// `SetMaxIdleConns(1)` / 两个 lifetime 为 0）—— 锚点是"写串行"的根，不动。
func closeConnDBWithTimeout(t *testing.T, conn *sql.Conn, db *sql.DB) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = conn.Close()
		_ = db.Close()
	}()
	select {
	case <-done:
	case <-time.After(closeGuardTimeout):
		// 超时后**故意不阻塞**：goroutine 继续卡在 Close 上，但不拦测试进程。
		t.Errorf("Store 关闭超时 —— 极可能有一个事务未 Commit/Rollback（database/sql 在 Close 时会等待在途事务）。请检查 InTx/Apply 的错误分支是否漏了 Rollback。")
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// newMemConn 打开一个内存库并取出一条**独立连接**给迁移使用。
//
// 迁移 API 收 `*sql.Conn`（见 migrate.go 顶部说明）：它把「迁移恰好跑在一条
// 独立物理连接上」写进类型签名。这里用共享缓存内存库 + 单连接池，保证
// 连接不被换掉（否则多次 apply/applied_versions 会读到不同的空库）。
func newMemConn(t *testing.T) (*sql.DB, *sql.Conn) {
	t.Helper()
	db, err := sql.Open(DriverName, memoryDSN())
	if err != nil {
		t.Fatalf("打开内存库: %v", err)
	}
	restrictToSingleWriter(db)
	conn, err := db.Conn(context.Background())
	if err != nil {
		_ = db.Close()
		t.Fatalf("获取连接: %v", err)
	}
	t.Cleanup(func() { closeConnDBWithTimeout(t, conn, db) })
	return db, conn
}

// newDiskConn 打开一个临时磁盘库并取出一条连接（用于必须跑在磁盘库的用例）。
func newDiskConn(t *testing.T) (*sql.DB, *sql.Conn) {
	t.Helper()
	db, err := OpenBootstrap(filepath.Join(t.TempDir(), "mig.db"))
	if err != nil {
		t.Fatalf("OpenBootstrap: %v", err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		_ = db.Close()
		t.Fatalf("获取连接: %v", err)
	}
	t.Cleanup(func() { closeConnDBWithTimeout(t, conn, db) })
	return db, conn
}

// writeMigration 往目录写一个迁移文件。
func writeMigration(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("写迁移文件 %s: %v", name, err)
	}
}

// connInt 取「恰好 1 行 1 列」的整数结果。
func connInt(t *testing.T, conn *sql.Conn, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("查询 %q: %v", q, err)
	}
	return n
}

// connCountRows 数一个查询返回的行数（用于 PRAGMA foreign_key_check 之类的自检）。
func connCountRows(t *testing.T, conn *sql.Conn, q string) int {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("查询 %q: %v", q, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 %q: %v", q, err)
	}
	return n
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Rust migrate.rs 的 7 条用例的 1:1 移植
// ---------------------------------------------------------------------------

// 对照 Rust: store/migrate.rs::applies_once_then_is_idempotent
func TestAppliesOnceThenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeMigration(t, dir, "V1__init.sql",
		"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL);")

	_, conn := newMemConn(t)

	n, err := Apply(conn, dir)
	if err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	if n != 1 {
		t.Fatalf("首次应应用 1 个，得到 %d", n)
	}
	n, err = Apply(conn, dir)
	if err != nil {
		t.Fatalf("二次 Apply: %v", err)
	}
	if n != 0 {
		t.Fatalf("二次应为幂等（0 个），得到 %d", n)
	}
	vers, err := AppliedVersions(conn)
	if err != nil {
		t.Fatalf("AppliedVersions: %v", err)
	}
	if !equalInt64s(vers, []int64{1}) {
		t.Fatalf("applied_versions 应为 [1]，得到 %v", vers)
	}
}

// 对照 Rust: store/migrate.rs::applies_in_version_order_regardless_of_fs_order
//
// ⚠️ 本用例**不足**以守卫 Discover 的排序：它断言的是 AppliedVersions（其 SQL 自带
// `ORDER BY version`），即使 Discover 完全不排序、甚至逆序，登记结果仍是 [1 2]。
// Discover 排序的守卫是下面两条（TestDiscoverSortsByVersionNotLexicographically /
// TestApplyOrderMattersForDependentMigrations），本用例保留仅为 1:1 对齐 Rust。
func TestAppliesInVersionOrderRegardlessOfFsOrder(t *testing.T) {
	dir := t.TempDir()
	// 故意乱序写盘：文件系统顺序无关，最终必须按版本升序应用。
	writeMigration(t, dir, "V2__b.sql", "CREATE TABLE b (x INTEGER);")
	writeMigration(t, dir, "V1__a.sql", "CREATE TABLE a (x INTEGER);")

	_, conn := newMemConn(t)
	n, err := Apply(conn, dir)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n != 2 {
		t.Fatalf("应应用 2 个，得到 %d", n)
	}
	vers, err := AppliedVersions(conn)
	if err != nil {
		t.Fatalf("AppliedVersions: %v", err)
	}
	if !equalInt64s(vers, []int64{1, 2}) {
		t.Fatalf("applied_versions 应为 [1 2]，得到 %v", vers)
	}
}

// 【G2 守卫 · 直接断言 Discover 的排序】
//
// 字典序是 V1 < V10 < V2，**版本序**必须是 1 < 2 < 10。二者在含两位数版本号时
// 分道扬镳 —— 这正是「按文件名先后应用」这一经典 bug 的温床。本用例**直接**检查
// Discover 的返回顺序（不经 AppliedVersions 的 `ORDER BY` 兜底），因此
// 「sort.Slice 被删掉/被改成逆序」一定会让它变红。
func TestDiscoverSortsByVersionNotLexicographically(t *testing.T) {
	dir := t.TempDir()
	// 故意乱序写盘，且刻意让 10 介于 1 与 2 的字典序之间。
	writeMigration(t, dir, "V2__b.sql", "SELECT 1;")
	writeMigration(t, dir, "V10__c.sql", "SELECT 1;")
	writeMigration(t, dir, "V1__a.sql", "SELECT 1;")

	got, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var vers []int64
	var names []string
	for _, m := range got {
		vers = append(vers, m.Version)
		names = append(names, m.Name)
	}
	if !equalInt64s(vers, []int64{1, 2, 10}) {
		t.Fatalf("Discover 应按**版本**升序返回 [1 2 10]（而非字典序 [1 10 2] 或逆序），得到 %v（name=%v）", vers, names)
	}
}

// 【G2 守卫 · 让「顺序」成为**成败判据**】
//
// 前一条用例直接读 Discover，但真正致命的是**应用顺序**：V10 依赖 V1 建的表。
// 若迁移按文件名顺序（V1 < V10 < V2）或逆序执行，V10 会在表存在之前插入而**报错**，
// 于是「Apply 成功」本身就是「已按版本序应用」的证明。这条用例把顺序从
// 「一个可读出的属性」升级为「决定成败的约束」，与真实部署的故障模式一致。
func TestApplyOrderMattersForDependentMigrations(t *testing.T) {
	dir := t.TempDir()
	// 字典序：V10__v10.sql < V1__base.sql < V2__v2.sql。
	// 只有严格按版本 1 → 2 → 10 应用，三条才都能成功。
	writeMigration(t, dir, "V1__base.sql",
		"CREATE TABLE seq (id INTEGER PRIMARY KEY, tag TEXT NOT NULL);")
	writeMigration(t, dir, "V2__v2.sql",
		"INSERT INTO seq (id, tag) VALUES (2, 'v2');")
	writeMigration(t, dir, "V10__v10.sql",
		"INSERT INTO seq (id, tag) VALUES (10, 'v10');")

	_, conn := newMemConn(t)
	n, err := Apply(conn, dir)
	if err != nil {
		t.Fatalf("Apply 失败（迁移未按版本序应用，V10 先跑会撞上未建的 seq）: %v", err)
	}
	if n != 3 {
		t.Fatalf("应应用 3 个，得到 %d", n)
	}
	if c := connInt(t, conn, "SELECT COUNT(*) FROM seq"); c != 2 {
		t.Fatalf("seq 应有 2 行（V1 先于 V2/V10 建表并插入），得到 %d", c)
	}
	vers, err := AppliedVersions(conn)
	if err != nil {
		t.Fatalf("AppliedVersions: %v", err)
	}
	if !equalInt64s(vers, []int64{1, 2, 10}) {
		t.Fatalf("applied_versions 应为 [1 2 10]，得到 %v", vers)
	}
}

// 对照 Rust: store/migrate.rs::rejects_malformed_filename
func TestRejectsMalformedFilename(t *testing.T) {
	dir := t.TempDir()
	writeMigration(t, dir, "init.sql", "SELECT 1;")

	_, conn := newMemConn(t)
	_, err := Apply(conn, dir)
	var badName *BadNameError
	if !errors.As(err, &badName) {
		t.Fatalf("期望 BadNameError，得到 %v", err)
	}
}

// 对照 Rust: store/migrate.rs::missing_dir_yields_empty_plan
func TestMissingDirYieldsEmptyPlan(t *testing.T) {
	_, conn := newMemConn(t)
	n, err := Apply(conn, filepath.Join("这", "个", "目", "录", "不", "存", "在"))
	if err != nil {
		t.Fatalf("目录不存在不应报错，得到 %v", err)
	}
	if n != 0 {
		t.Fatalf("目录不存在应返回 0，得到 %d", n)
	}
}

// 对照 Rust: store/migrate.rs::failed_migration_rolls_back_completely
//
// 迁移失败必须**整体回滚**：不能留下「表建了但版本没记」的半成品。
func TestFailedMigrationRollsBackCompletely(t *testing.T) {
	dir := t.TempDir()
	// 第一条成功、第二条因重名失败 ⇒ 整个事务必须回滚。
	writeMigration(t, dir, "V1__broken.sql",
		"CREATE TABLE ok_table (x INTEGER);\nCREATE TABLE ok_table (x INTEGER);")

	_, conn := newMemConn(t)
	if _, err := Apply(conn, dir); err == nil {
		t.Fatal("坏迁移应报错")
	}
	if n := connInt(t, conn,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ok_table'"); n != 0 {
		t.Fatalf("失败迁移留下了半成品表（事务未回滚）：计数 %d", n)
	}
}

// 对照 Rust: store/migrate.rs::works_when_migration_creates_its_own_schema_migrations
//
// V1__init.sql **自己带** schema_migrations 建表语句。此时兜底的 IF NOT EXISTS
// 必须是 no-op，不能因「表已存在」而报错。
func TestWorksWhenMigrationCreatesItsOwnSchemaMigrations(t *testing.T) {
	dir := t.TempDir()
	writeMigration(t, dir, "V1__init.sql", `CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    applied_at INTEGER NOT NULL
);
CREATE TABLE t (id INTEGER PRIMARY KEY);`)

	_, conn := newMemConn(t)
	n, err := Apply(conn, dir)
	if err != nil {
		t.Fatalf("不应因表已存在而冲突: %v", err)
	}
	if n != 1 {
		t.Fatalf("应应用 1 个，得到 %d", n)
	}
	vers, err := AppliedVersions(conn)
	if err != nil {
		t.Fatalf("AppliedVersions: %v", err)
	}
	if !equalInt64s(vers, []int64{1}) {
		t.Fatalf("applied_versions 应为 [1]，得到 %v", vers)
	}
	if c := connInt(t, conn, "SELECT COUNT(*) FROM sqlite_master WHERE name = 't'"); c != 1 {
		t.Fatalf("业务表 t 应已建立，计数 %d", c)
	}
	n, err = Apply(conn, dir)
	if err != nil {
		t.Fatalf("二次 Apply: %v", err)
	}
	if n != 0 {
		t.Fatalf("二次仍应幂等，得到 %d", n)
	}
}

// 对照 Rust: store/migrate.rs::real_v1_migration_produces_14_tables_and_17_indexes
//
// **最硬的验收**：拿**真实的** migrations/V1__init.sql 在**磁盘库**上跑一遍，
// 断言 DDL 的**实际效果**（14 表 / 17 显式索引 / 外键自检为空 / 关键表与部分唯一索引都在）。
func TestRealV1MigrationProduces14TablesAnd17Indexes(t *testing.T) {
	migDir := filepath.Join("..", "..", "migrations")
	if _, err := os.Stat(filepath.Join(migDir, "V1__init.sql")); err != nil {
		t.Fatalf("找不到真实迁移文件 %s: %v", filepath.Join(migDir, "V1__init.sql"), err)
	}

	_, conn := newDiskConn(t)

	n, err := Apply(conn, migDir)
	if err != nil {
		t.Fatalf("Apply 真实 V1: %v", err)
	}
	if n != 1 {
		t.Fatalf("应恰好应用 V1（1 个），得到 %d", n)
	}
	vers, err := AppliedVersions(conn)
	if err != nil {
		t.Fatalf("AppliedVersions: %v", err)
	}
	if !equalInt64s(vers, []int64{1}) {
		t.Fatalf("applied_versions 应为 [1]，得到 %v", vers)
	}

	tables := connInt(t, conn,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")
	indexes := connInt(t, conn,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name NOT LIKE 'sqlite_%'")
	fkViolations := connCountRows(t, conn, "PRAGMA foreign_key_check")
	t.Logf("真实 V1 迁移实测: 表数=%d 显式索引数=%d 外键违例行数=%d", tables, indexes, fkViolations)

	if tables != 14 {
		t.Fatalf("表数应为 14（含 schema_migrations），得到 %d", tables)
	}
	if indexes != 17 {
		t.Fatalf("显式索引应为 17，得到 %d", indexes)
	}

	// 外键自检必须干净 —— 注意 CTAS 重建表会让这里抛异常而非返回空，
	// 所以「能正常跑出 0 行」本身是一种约束完整性的证明。
	if fkViolations != 0 {
		t.Fatalf("外键自检应无违规（0 行），得到 %d 行", fkViolations)
	}

	// 13 张业务表逐个存在（+ schema_migrations = 14）。
	for _, name := range []string{
		"node", "source_profile", "task_group", "task", "task_attempt", "task_file",
		"dedup_index", "task_progress", "dispatch_log", "node_health", "event_log",
		"app_config", "group_binding_log",
	} {
		if c := connInt(t, conn,
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name); c != 1 {
			t.Fatalf("缺表：%s", name)
		}
	}

	// 部分唯一索引（INV-A5）必须在。
	if c := connInt(t, conn,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='ux_task_group_episode'"); c != 1 {
		t.Fatalf("缺 INV-A5 的部分唯一索引 ux_task_group_episode")
	}
}
