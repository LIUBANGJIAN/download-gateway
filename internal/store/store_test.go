package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// mustOpenDisk 打开一个临时磁盘库 Store（无迁移）。
// 磁盘库是「运行时 PRAGMA 断言」与「并发写证伪实验」的正确载体（内存库不支持 WAL）。
func mustOpenDisk(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	// 不存在的迁移目录 ⇒ 零迁移、不报错（由 Apply 保证）。
	s, err := Open(dbPath, filepath.Join(t.TempDir(), "no-such-migrations"))
	if err != nil {
		t.Fatalf("打开磁盘库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustOpenMem 打开一个内存库 Store（仅测试）。
func mustOpenMem(t *testing.T) *Store {
	t.Helper()
	s, err := OpenInMemory()
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustOpenRealSchema 打开临时磁盘库并应用**真实的** migrations/V1__init.sql。
// 供「限定列名唯一冲突」用例使用 —— 要的正是真实 DDL 里的约束与限定列名。
func mustOpenRealSchema(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "real.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("打开真实 schema 库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustExec 执行一条写语句，失败即 Fatal。
func mustExec(t *testing.T, s *Store, sqlText string, args ...any) {
	t.Helper()
	if _, err := s.Execute(context.Background(), sqlText, args...); err != nil {
		t.Fatalf("执行 %q 出错: %v", sqlText, err)
	}
}

// oneCell 取「恰好 1 行 1 列」的查询结果。
func oneCell(t *testing.T, s *Store, sqlText string, args ...any) any {
	t.Helper()
	rows, err := s.Query(context.Background(), sqlText, args...)
	if err != nil {
		t.Fatalf("查询 %q 出错: %v", sqlText, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("查询 %q 期望 1×1 结果，得到 %v", sqlText, rows)
	}
	return rows[0][0]
}

// oneInt 取一个整数单元格（modernc 的 INTEGER 映射为 int64）。
func oneInt(t *testing.T, s *Store, sqlText string, args ...any) int64 {
	t.Helper()
	v := oneCell(t, s, sqlText, args...)
	n, ok := v.(int64)
	if !ok {
		t.Fatalf("查询 %q 期望 int64，得到 %T(%v)", sqlText, v, v)
	}
	return n
}

// oneString 取一个文本单元格。
func oneString(t *testing.T, s *Store, sqlText string, args ...any) string {
	t.Helper()
	v := oneCell(t, s, sqlText, args...)
	sv, ok := v.(string)
	if !ok {
		t.Fatalf("查询 %q 期望 string，得到 %T(%v)", sqlText, v, v)
	}
	return sv
}

// ---------------------------------------------------------------------------
// Rust actor.rs 的 4 条用例的 1:1 移植
// ---------------------------------------------------------------------------

// 对照 Rust: store/actor.rs::concurrent_writes_are_serialized_without_busy
//
// **T01 的核心验收**：128 个 goroutine 同时写，全部成功且计数精确 —— 这不只是
// 「跑得通」，它证明了**架构层面的串行化**（单写者池，无 SQLITE_BUSY）。
//
// 偏差说明（有意）：Rust 用内存库 + tokio::spawn；这里改用**磁盘库** + goroutine，
// 以便同一用例同时充当「把 SetMaxOpenConns(1) 改成 10」证伪实验的载体
// （WAL 只在磁盘库上有意义）。断言的行为完全一致。
func TestConcurrentWritesAreSerializedWithoutBusy(t *testing.T) {
	s := mustOpenDisk(t)
	if _, err := s.Execute(context.Background(),
		"CREATE TABLE t (id INTEGER PRIMARY KEY AUTOINCREMENT, v INTEGER NOT NULL)"); err != nil {
		t.Fatalf("建表: %v", err)
	}

	const N = 128
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Execute(context.Background(), "INSERT INTO t (v) VALUES (?)", int64(i))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发写 #%d 失败（疑似 SQLITE_BUSY）: %v", i, err)
		}
	}

	if got := oneInt(t, s, "SELECT COUNT(*) FROM t"); got != N {
		t.Fatalf("行数应为 %d，得到 %d", N, got)
	}
	var wantSum int64
	for i := 0; i < N; i++ {
		wantSum += int64(i)
	}
	if got := oneInt(t, s, "SELECT SUM(v) FROM t"); got != wantSum {
		t.Fatalf("SUM 应精确为 %d，得到 %d", wantSum, got)
	}

	// 顺带把「串行化机制」钉死：池的连接上限就是 1。
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("写池连接上限应为 1，得到 %d", got)
	}
}

// 对照 Rust: store/actor.rs::writes_are_visible_to_subsequent_reads
func TestWritesAreVisibleToSubsequentReads(t *testing.T) {
	s := mustOpenMem(t)
	if _, err := s.Execute(context.Background(),
		"CREATE TABLE k (a INTEGER PRIMARY KEY, b TEXT NOT NULL)"); err != nil {
		t.Fatalf("建表: %v", err)
	}
	if _, err := s.Execute(context.Background(),
		"INSERT INTO k (a, b) VALUES (?, ?)", int64(1), "x"); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if got := oneString(t, s, "SELECT b FROM k WHERE a = ?", int64(1)); got != "x" {
		t.Fatalf("后续读应看到 x，得到 %q", got)
	}
}

// 对照 Rust: store/actor.rs::foreign_keys_are_enforced
//
// PRAGMA foreign_keys = ON 必须真的生效 —— 这是全部 REFERENCES 有意义的前提。
func TestForeignKeysAreEnforced(t *testing.T) {
	s := mustOpenMem(t)
	if err := s.ExecuteBatch(context.Background(),
		"CREATE TABLE parent (id INTEGER PRIMARY KEY);"+
			"CREATE TABLE child ("+
			"  id INTEGER PRIMARY KEY,"+
			"  p  INTEGER NOT NULL REFERENCES parent(id)"+
			");"); err != nil {
		t.Fatalf("建表: %v", err)
	}
	if _, err := s.Execute(context.Background(),
		"INSERT INTO child (id, p) VALUES (1, 999)"); err == nil {
		t.Fatal("外键未生效：违反外键的插入应报错（PRAGMA foreign_keys 没打开）")
	}
}

// 对照 Rust: store/actor.rs::unique_violation_surfaces_as_error
func TestUniqueViolationSurfacesAsError(t *testing.T) {
	s := mustOpenMem(t)
	mustExec(t, s, "CREATE TABLE u (k TEXT PRIMARY KEY)")
	mustExec(t, s, "INSERT INTO u (k) VALUES ('a')")
	if _, err := s.Execute(context.Background(), "INSERT INTO u (k) VALUES ('a')"); err == nil {
		t.Fatal("重复主键应报错，不能静默成功")
	}
}

// ---------------------------------------------------------------------------
// Go 特有坑的守卫用例（Rust 侧没有）
// ---------------------------------------------------------------------------

// 坑 1 的**根因守卫**：`cache=shared` 到底买到了什么？
//
// 前面的 TestMemorySchemaSurvivesRepeatedAccess 只证明「单池内多连接看到同一个库」。
// 但共享缓存 URI 的**充分价值**在于：**库身份由 DSN 里的名字固定，与「开几个池、
// 每个池签出哪条连接」完全解耦**。因此从**同一个 DSN 名字**开出的**两条独立连接池**，
// 必须命中同一个内存库。
//
// 为什么必须单列本用例：把 `cache=shared` 改成 `cache=private`（变异 S9）时，
// 单池用例**照样全绿**——池被限制为 1 连接、那条连接常驻，private 缓存下「一个连接
// 一个私有库」仍自洽。只有「两个池共享一个名字」这一维度能把该变异照出来：
// private 下两个池各自得到一个**空的**私有库，池 B 看不到池 A 建的表。
func TestNamedSharedCacheKeepsLibraryIdentityAcrossPools(t *testing.T) {
	// 同一个 DSN（同一个内存库名字）——库身份只由这里决定，与连接池无关。
	dsn := memoryDSN()

	openPool := func() *sql.DB {
		db, err := sql.Open(DriverName, dsn)
		if err != nil {
			t.Fatalf("打开池: %v", err)
		}
		restrictToSingleWriter(db)
		if err := db.Ping(); err != nil {
			_ = db.Close()
			t.Fatalf("连接池: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	poolA := openPool()
	poolB := openPool()

	// 池 A：建表 + 写哨兵。
	if _, err := poolA.Exec(
		"CREATE TABLE shared_probe (id INTEGER PRIMARY KEY, v INTEGER NOT NULL)"); err != nil {
		t.Fatalf("池 A 建表: %v", err)
	}
	if _, err := poolA.Exec("INSERT INTO shared_probe (id, v) VALUES (1, 424242)"); err != nil {
		t.Fatalf("池 A 写入: %v", err)
	}

	// 池 B：必须能看到池 A 建的表 —— 同名 ⇒ 同库。cache=private 时这里是 no such table。
	var n int64
	if err := poolB.QueryRow("SELECT COUNT(*) FROM shared_probe").Scan(&n); err != nil {
		t.Fatalf("池 B 看不到池 A 建的表（库身份未按 DSN 名绑定，疑似 cache 非 shared）: %v", err)
	}
	if n != 1 {
		t.Fatalf("池 B 应看到 1 行，得到 %d", n)
	}
	var v int64
	if err := poolB.QueryRow("SELECT v FROM shared_probe WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("池 B 读取哨兵: %v", err)
	}
	if v != 424242 {
		t.Fatalf("哨兵值应为 424242，得到 %d", v)
	}
}

// 坑 1 的守卫：**建表后连续多次访问，表必须都还在**。
//
// 内存库的「库身份」若绑定在物理连接上，连接一被换掉表就没了，症状是随机
// `no such table`。本用例通过 200 次往返 + 64 路并发读反复签出/归还连接，
// 逼出任何「换库」行为。
func TestMemorySchemaSurvivesRepeatedAccess(t *testing.T) {
	s := mustOpenMem(t)
	if _, err := s.Execute(context.Background(),
		"CREATE TABLE surf (id INTEGER PRIMARY KEY, v INTEGER NOT NULL)"); err != nil {
		t.Fatalf("建表: %v", err)
	}
	if _, err := s.Execute(context.Background(),
		"INSERT INTO surf (id, v) VALUES (1, 100)"); err != nil {
		t.Fatalf("写入哨兵行: %v", err)
	}

	// 反复往返：每 25 次夹一次写，其余为读。若库被换掉，这里会报 no such table。
	for i := 0; i < 200; i++ {
		if i%25 == 0 {
			if _, err := s.Execute(context.Background(),
				"INSERT INTO surf (v) VALUES (?)", int64(i)); err != nil {
				t.Fatalf("第 %d 次写失败（疑似内存库被换掉）: %v", i, err)
			}
		}
		n := oneInt(t, s, "SELECT COUNT(*) FROM surf")
		if n < 1 {
			t.Fatalf("第 %d 次读：表空了（内存库被换掉）", i)
		}
	}

	// 并发读也必须命中同一个库。
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rows, err := s.Query(context.Background(), "SELECT COUNT(*) FROM surf")
			if err != nil {
				t.Errorf("并发读 #%d 失败: %v", i, err)
				return
			}
			if len(rows) != 1 {
				t.Errorf("并发读 #%d 结果异常: %v", i, rows)
			}
		}(i)
	}
	wg.Wait()

	// 最初写入的哨兵行必须仍然可读、且值不变。
	if got := oneInt(t, s, "SELECT v FROM surf WHERE id = 1"); got != 100 {
		t.Fatalf("哨兵行丢失或被改：期望 100，得到 %d", got)
	}
}

// 守卫：写池必须被限制为单连接。
func TestSingleWriterPoolIsConfigured(t *testing.T) {
	s := mustOpenMem(t)
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections 应为 1，得到 %d", got)
	}
}

// 额外要求 A：**运行时 PRAGMA 必须真断言实际值**（不接受只检查 DSN 文本）。
//
// 必须在**磁盘库**上：内存库不支持 WAL，`journal_mode` 不会是 wal。
func TestRuntimePragmasOnDisk(t *testing.T) {
	s := mustOpenDisk(t)

	jm := oneString(t, s, "PRAGMA journal_mode")
	fk := oneInt(t, s, "PRAGMA foreign_keys")
	bt := oneInt(t, s, "PRAGMA busy_timeout")
	t.Logf("运行时 PRAGMA 实测: journal_mode=%q foreign_keys=%d busy_timeout=%d", jm, fk, bt)

	if jm != "wal" {
		t.Fatalf("PRAGMA journal_mode 运行时实际值应为 wal，得到 %q", jm)
	}
	if fk != 1 {
		t.Fatalf("PRAGMA foreign_keys 运行时实际值应为 1，得到 %d", fk)
	}
	if bt != 5000 {
		t.Fatalf("PRAGMA busy_timeout 运行时实际值应为 5000，得到 %d", bt)
	}
}

// 额外要求 B：**驱动名的守卫用例** —— 证明驱动名写错会被抓住。
//
// `sqlite3` 是 mattn/go-sqlite3 的名字；本项目只注册 `sqlite`（modernc）。
// 打开 + 执行一次查询都必须失败，防「静默写错驱动名」。
func TestUnknownDriverNameIsRejected(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:whatever?mode=memory&cache=shared")
	if err == nil {
		defer db.Close()
		err = db.Ping()
	}
	if err == nil {
		t.Fatal("驱动名 sqlite3 不应存在，但打开/查询却成功了")
	}
	t.Logf("符合预期，驱动名 sqlite3 被拒绝: %v", err)
}

// 坑 4：DDL 的 RowsAffected 返回 0、DML 返回实际行数 —— 与 Rust 一致，不去「修正」。
func TestRowsAffectedSemantics(t *testing.T) {
	s := mustOpenMem(t)
	n, err := s.Execute(context.Background(), "CREATE TABLE ddl_probe (x INTEGER)")
	if err != nil {
		t.Fatalf("DDL: %v", err)
	}
	if n != 0 {
		t.Fatalf("DDL 的 RowsAffected 应为 0（对齐 Rust），得到 %d", n)
	}
	n, err = s.Execute(context.Background(), "INSERT INTO ddl_probe (x) VALUES (1)")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if n != 1 {
		t.Fatalf("INSERT 的 RowsAffected 应为 1，得到 %d", n)
	}
}

// 对齐 Rust open_connection：打开磁盘库应自动创建父目录。
func TestOpenBootstrapCreatesParentDirs(t *testing.T) {
	nested := filepath.Join(t.TempDir(), "a", "b", "c", "x.db")
	db, err := OpenBootstrap(nested)
	if err != nil {
		t.Fatalf("OpenBootstrap 失败: %v", err)
	}
	defer db.Close()
	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("库文件未被创建: %v", err)
	}
}

// 坑 3 的守卫：Open 必须先应用迁移、再把池限制为单写者。
func TestOpenAppliesMigrationsThenRestrictsPool(t *testing.T) {
	migDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(migDir, "V1__w.sql"),
		[]byte("CREATE TABLE widget (id INTEGER PRIMARY KEY);"),
		0o644,
	); err != nil {
		t.Fatalf("写迁移文件: %v", err)
	}

	s, err := Open(filepath.Join(t.TempDir(), "app.db"), migDir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer s.Close()

	if got := oneInt(t, s,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='widget'"); got != 1 {
		t.Fatalf("迁移后 widget 表应存在，得到计数 %d", got)
	}
	if got := oneInt(t, s, "SELECT COUNT(*) FROM schema_migrations"); got != 1 {
		t.Fatalf("应登记 1 个版本，得到 %d", got)
	}
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("迁移完成后写池应被限制为 1 连接，得到 %d", got)
	}
}

// ---------------------------------------------------------------------------
// 事务原语 Store.InTx（design/12 §9）
// ---------------------------------------------------------------------------

// InTx：fn 返回 nil ⇒ **提交**；返回错误 ⇒ **回滚**，且该错误**原样**返回（errors.Is 可判）。
func TestInTxCommitsOnSuccessAndRollsBackOnError(t *testing.T) {
	s := mustOpenMem(t)
	ctx := context.Background()
	mustExec(t, s, "CREATE TABLE tx_probe (id INTEGER PRIMARY KEY, v TEXT NOT NULL)")

	// 提交路径。
	if err := s.InTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO tx_probe (id, v) VALUES (1, 'c')")
		return err
	}); err != nil {
		t.Fatalf("InTx 提交路径: %v", err)
	}
	if n := oneInt(t, s, "SELECT COUNT(*) FROM tx_probe"); n != 1 {
		t.Fatalf("提交后应有 1 行，得到 %d", n)
	}

	// 回滚路径：fn 返回一个可识别的哨兵错误。
	sentinel := errors.New("boom-xyz")
	err := s.InTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO tx_probe (id, v) VALUES (2, 'r')"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx 应**原样**返回 fn 的错误（errors.Is 可判），得到 %v", err)
	}
	if n := oneInt(t, s, "SELECT COUNT(*) FROM tx_probe"); n != 1 {
		t.Fatalf("回滚后应仍只有 1 行，得到 %d（回滚未生效）", n)
	}
}

// 嵌套 InTx ⇒ ErrTxActive（**绝不静默加入外层**：静默加入会让内层「回滚」把外层一起回滚）。
//
// 内层调用用**带超时的 ctx**（仍带外层标记）作兜底：万一嵌套检查失效，内层会在已占用
// 的池上阻塞到超时后返回错误，而非挂死整个测试进程。
func TestInTxRejectsNested(t *testing.T) {
	s := mustOpenMem(t)
	inner := error(nil)
	err := s.InTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		bctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		inner = s.InTx(bctx, func(context.Context, *sql.Tx) error { return nil })
		return nil
	})
	if err != nil {
		t.Fatalf("外层 InTx 不应报错: %v", err)
	}
	if !errors.Is(inner, ErrTxActive) {
		t.Fatalf("嵌套 InTx 应返回 ErrTxActive，得到 %v", inner)
	}
}

// fn 内用 Store 便捷方法（ctx 带标记）⇒ ErrTxActive —— 把「静默死锁」变成立即报错。
//
// 三条方法各测一次；用**带超时的 ctx**（从 fn 的 ctx 派生，故仍带标记）作兜底：
// 万一守卫失效，会退化成死锁/超时而非挂死整个测试进程。
func TestInTxRejectsStoreMethodsInsideFn(t *testing.T) {
	s := mustOpenMem(t)
	mustExec(t, s, "CREATE TABLE tx_probe2 (id INTEGER PRIMARY KEY)")
	err := s.InTx(context.Background(), func(scopeCtx context.Context, tx *sql.Tx) error {
		tctx, cancel := context.WithTimeout(scopeCtx, 3*time.Second)
		defer cancel()
		if _, e := s.Execute(tctx, "INSERT INTO tx_probe2 (id) VALUES (1)"); !errors.Is(e, ErrTxActive) {
			t.Fatalf("fn 内 Execute 应返回 ErrTxActive，得到 %v", e)
		}
		if e := s.ExecuteBatch(tctx, "INSERT INTO tx_probe2 (id) VALUES (2)"); !errors.Is(e, ErrTxActive) {
			t.Fatalf("fn 内 ExecuteBatch 应返回 ErrTxActive，得到 %v", e)
		}
		if _, e := s.Query(tctx, "SELECT 1"); !errors.Is(e, ErrTxActive) {
			t.Fatalf("fn 内 Query 应返回 ErrTxActive，得到 %v", e)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
}

// 「事务窗口已关」的旧 ctx 再进 InTx ⇒ **新根事务**（不得被误判为嵌套）。
//
// 直接钉住标记的**生命周期**语义：只检查「有没有标记」不够，还必须检查「标记是否仍在
// 窗口内」。故这里故意把 fn 的 ctx 存下来、窗口关闭后再用 —— godoc 禁忌明令不得这样泄漏
// ctx，但正因禁忌存在，其后果必须被钉死。
func TestInTxOldScopeCtxIsNewRoot(t *testing.T) {
	s := mustOpenMem(t)
	var leaked context.Context
	if err := s.InTx(context.Background(), func(scopeCtx context.Context, tx *sql.Tx) error {
		leaked = scopeCtx
		return nil
	}); err != nil {
		t.Fatalf("第一次 InTx: %v", err)
	}
	// 窗口已关：用旧 scopeCtx 再进，应被当成**新根事务**而非嵌套。
	if err := s.InTx(leaked, func(context.Context, *sql.Tx) error { return nil }); err != nil {
		t.Fatalf("窗口关闭后旧 ctx 再进应为新根事务（不得 ErrTxActive），得到 %v", err)
	}
}

// Commit 失败可与 fn 的错误**区分**：fn 自行回滚后仍返回 nil ⇒ InTx 的 Commit 会失败，
// 返回的错误满足 errors.Is(err, ErrCommit)。
func TestInTxCommitFailureIsDistinguishable(t *testing.T) {
	s := mustOpenMem(t)
	err := s.InTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_ = tx.Rollback() // 故意让随后的 Commit 失败（sql.ErrTxDone）
		return nil
	})
	if err == nil {
		t.Fatal("fn 回滚后 Commit 应失败，得到 nil")
	}
	if !errors.Is(err, ErrCommit) {
		t.Fatalf("提交失败应可被 errors.Is(err, ErrCommit) 识别，得到 %v", err)
	}
}

// ---------------------------------------------------------------------------
// IsUniqueViolation：判据落在**限定列名**的文本上
// ---------------------------------------------------------------------------

// 覆盖四类：① 多列 UNIQUE 冲突 ⇒ true；② PRIMARY KEY 重复 ⇒ true（强制反例）；
// ③ **别的列**的 UNIQUE 冲突 ⇒ false（防误判，且正向对照正确列名 ⇒ true）；
// ④ 非唯一性错误（NOT NULL / 外键）⇒ false；另加 nil ⇒ false。
//
// 列名相关断言必须用**限定列名**（task.task_id、task_attempt.task_id、task.gid），
// 因为裸 task_id 在 task 与 task_attempt 同时存在，只有限定名能区分是哪张表。
func TestIsUniqueViolation(t *testing.T) {
	s := mustOpenRealSchema(t)
	ctx := context.Background()
	mustExec(t, s,
		"INSERT INTO node(node_id, alias, base_url, user, pass_enc, created_at, updated_at) "+
			"VALUES (1,'n1','http://x','u','p',0,0)")
	mustExec(t, s,
		"INSERT INTO task(task_id, kind, url_raw, created_at, updated_at) VALUES ('t1','http','http://a',0,0)")

	// ① task_attempt 的 UNIQUE(task_id, attempt_no)。
	mustExec(t, s,
		"INSERT INTO task_attempt(attempt_id, task_id, attempt_no, node_id, state, started_at) "+
			"VALUES (1,'t1',1,1,'dispatched',0)")
	_, err := s.Execute(ctx,
		"INSERT INTO task_attempt(attempt_id, task_id, attempt_no, node_id, state, started_at) "+
			"VALUES (2,'t1',1,1,'dispatched',0)")
	if err == nil {
		t.Fatal("① 期望 task_attempt UNIQUE 冲突")
	}
	if !IsUniqueViolation(err, "task_attempt.task_id") {
		t.Fatalf("① 多列 UNIQUE 冲突应判 true；err=%v", err)
	}

	// ② task.task_id 作 PRIMARY KEY 的重复插入（强制反例）。
	_, err = s.Execute(ctx,
		"INSERT INTO task(task_id, kind, url_raw, created_at, updated_at) VALUES ('t1','http','http://b',0,0)")
	if err == nil {
		t.Fatal("② 期望 task.task_id 主键重复")
	}
	if !IsUniqueViolation(err, "task.task_id") {
		t.Fatalf("② 主键重复应判 true；err=%v", err)
	}

	// ③ 别的列（task.gid）的 UNIQUE 冲突：对 task.task_id 必须 false，对 task.gid 必须 true。
	mustExec(t, s,
		"INSERT INTO task(task_id, kind, url_raw, created_at, updated_at, gid) VALUES ('t2','http','http://c',0,0,'g1')")
	_, err = s.Execute(ctx,
		"INSERT INTO task(task_id, kind, url_raw, created_at, updated_at, gid) VALUES ('t3','http','http://d',0,0,'g1')")
	if err == nil {
		t.Fatal("③ 期望 task.gid UNIQUE 冲突")
	}
	if IsUniqueViolation(err, "task.task_id") {
		t.Fatalf("③ 别列冲突不得判给 task.task_id；err=%v", err)
	}
	if !IsUniqueViolation(err, "task.gid") {
		t.Fatalf("③ 正向对照：task.gid 应判 true；err=%v", err)
	}

	// ④ 非唯一性错误：NOT NULL。
	_, err = s.Execute(ctx,
		"INSERT INTO task(task_id, kind, url_raw, created_at, updated_at) VALUES ('t4', NULL, 'http://e', 0, 0)")
	if err == nil {
		t.Fatal("④ 期望 NOT NULL 冲突")
	}
	if IsUniqueViolation(err, "task.task_id") {
		t.Fatalf("④ NOT NULL 冲突不得判 true；err=%v", err)
	}

	// ④ 非唯一性错误：外键。
	_, err = s.Execute(ctx,
		"INSERT INTO task_attempt(attempt_id, task_id, attempt_no, node_id, state, started_at) "+
			"VALUES (9,'nope',1,1,'dispatched',0)")
	if err == nil {
		t.Fatal("④ 期望外键冲突")
	}
	if IsUniqueViolation(err, "task_attempt.task_id") {
		t.Fatalf("④ 外键冲突不得判 true；err=%v", err)
	}

	// 边界：nil ⇒ false。
	if IsUniqueViolation(nil, "task.task_id") {
		t.Fatal("nil 错误应判 false")
	}
}
