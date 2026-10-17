// Package store 提供 SQLite 存储层：**单写者连接池** + 迁移框架。
//
// # 1:1 移植来源与「为什么是单写者」
//
// 本包是 Rust 版 crates/dispatch-core/src/store/{mod.rs,actor.rs} 的**行为等价**重写。
// Rust 侧的取舍（见 store/mod.rs 模块文档）逐条照搬：
//
//   - 本项目的正确性建立在若干**全局不变量**上（去重键唯一、INV-A1/A3/A5…），
//     这些不变量都要在权威库上用 SQL 断言验证。因此写路径必须满足两条性质：
//     ① **顺序性**：task_attempt 与投递同事务写入，若允许多写者并发，事务交错会让
//     「投递时刻一致性」断言失去意义；② **无 SQLITE_BUSY**：不是靠 busy_timeout
//     重试掩盖冲突，而是**从架构上**让写根本没有并发。
//   - Rust 用「mpsc 队列 + 唯一 OS 线程持有 Connection」实现；
//     Go 的等价物是**一个 `*sql.DB` 且 `SetMaxOpenConns(1)`**——
//     `database/sql` 会把该池上的所有操作互斥串行，语义与「唯一线程顺序执行」一致。
//   - 读路径**也走同一条池**（与 Rust 一致：读也排队）。量级极小（写 ~10 行/秒），
//     换来的是「零并发事务」的简单心智模型。**不做「独立读池」等未被要求的优化。**
//
// # Go 特有的四个坑（Rust 侧没有，本包逐一处理，均有测试）
//
//  1. **内存库会被连接池「换掉」**：SQLite 的 `:memory:` **每个连接一个独立库**。
//     `database/sql` 一旦回收并重建连接，之前建的表全没了，症状是偶发
//     `no such table`。本包对策见 [memoryDSN] 的说明（选用命名共享缓存 URI +
//     常驻空闲连接）。
//  2. **WAL 对内存库无意义**：内存库不支持 WAL。因此「journal_mode 实际值 == wal」
//     的断言只能跑在**磁盘库**上（见 store_test.go 的 TestRuntimePragmasOnDisk）。
//  3. **迁移不能跑在写池上**：`SetMaxOpenConns(1)` 时单个事务会独占那唯一的连接。
//     迁移必须在写池受限**之前**、用**一个独立连接**完成。启动顺序严格对齐 Rust：
//     `open_connection()` → `migrate::apply()` → `from_connection()`。
//     见 [Open]：顺序被封装进单一入口，调用者无从搞反。
//  4. **RowsAffected 的语义**：INSERT 的 `RowsAffected()` 可靠；DDL 语句返回 0——
//     与 Rust `Connection::execute` 行为一致，**不要「修正」它**。
//
// # 驱动名
//
// `modernc.org/sqlite` 注册的驱动名是 **`sqlite`（没有数字 3）**。
// `sqlite3` 是 `mattn/go-sqlite3` 的名字——写错**编译期不报**，
// 只在运行期由 `sql.Open`/首用时报 `unknown driver`。见 store_test.go 的
// TestUnknownDriverNameIsRejected。
//
// # PRAGMA 必须走 DSN
//
// `journal_mode`/`synchronous` 是**库级持久**设置；`foreign_keys`/`busy_timeout`
// 是**连接级**的——而 `database/sql` 会按需新建连接，只有把 PRAGMA 写进 DSN
// 才能保证**每个新建连接**都生效（对齐 Rust `BOOTSTRAP_PRAGMAS`）。
// `modernc.org/sqlite` 的 DSN 语法是 `_pragma=name(value)`，**与 mattn 的
// `?_journal_mode=WAL&_foreign_keys=on` 不同**，不可照抄网上 mattn 的写法。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	_ "modernc.org/sqlite" // 纯 Go 驱动，注册名为 "sqlite"（零 cgo）
)

// DriverName 是 database/sql 使用的驱动名。
//
// ⚠️ 是 `sqlite`，**不是** `sqlite3`。写成 `sqlite3` 会在运行期报 unknown driver。
const DriverName = "sqlite"

// bootstrapPragmas 是**磁盘库**的连接引导 PRAGMA（对齐 Rust `BOOTSTRAP_PRAGMAS`）。
//
//   - journal_mode(WAL)：读不阻塞写（虽是单写者，但健康探测/查询仍会读）。
//   - synchronous(NORMAL)：WAL 下的常规取舍（比 FULL 快、仍抗进程崩溃）。
//   - foreign_keys(1)：**必须显式开启**，SQLite 默认关闭；这是全部 REFERENCES 有意义的前提。
//   - busy_timeout(5000)：作为「单写者假设被破坏时」的兜底，**不作为主要机制**。
var bootstrapPragmas = []string{
	"journal_mode(WAL)",
	"synchronous(NORMAL)",
	"foreign_keys(1)",
	"busy_timeout(5000)",
}

// memoryPragmas 是**内存库**的引导 PRAGMA。
//
// 刻意**不含** journal_mode/synchronous：内存库不支持 WAL（见包文档坑 2）。
// 只保留连接级的两个，对齐 Rust `Store::open_in_memory()` 里的
// `PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;`。
var memoryPragmas = []string{
	"foreign_keys(1)",
	"busy_timeout(5000)",
}

// pragmaQuery 把 PRAGMA 列表编成 modernc 的 DSN 查询串（`_pragma=name(value)`）。
func pragmaQuery(pragmas []string) string {
	parts := make([]string, len(pragmas))
	for i, p := range pragmas {
		parts[i] = "_pragma=" + p
	}
	return strings.Join(parts, "&")
}

// diskDSN 给磁盘库路径拼上引导 PRAGMA 的查询串。
//
// modernc 把第一个 `?`（且不在首位）之前的整段当作文件名，之后的当作查询参数，
// 因此「普通路径 + `?` + 参数」这种写法是官方支持的（驱动自测即如此用）。
func diskDSN(path string, pragmas []string) string {
	return path + "?" + pragmaQuery(pragmas)
}

// memSeq 为每个内存库生成进程内唯一的名字，避免不同测试共用同一个共享缓存库。
var memSeq atomic.Uint64

// memoryDSN 构造**命名共享缓存**的内存库 DSN。
//
// 【为何选共享缓存，而非普通 `:memory:`】
//
//	普通 `:memory:` 的「库身份」绑定在**具体物理连接**上：`database/sql` 一旦因
//	任何原因换掉那条连接（空闲回收、SetConnMaxLifetime 到期、连接被判坏而丢弃），
//	新连接就是一个**全新的空库**，此前建的表全部消失，症状是随机的 `no such table`。
//	改用命名共享缓存 URI（`file:<name>?mode=memory&cache=shared`）后，库的身份由
//	**DSN 里的名字**固定，与「当前签出的是哪个连接」解耦。
//
//	代价与配套：共享缓存内存库在**最后一个连接关闭时销毁**，因此必须保证始终有
//	≥1 条连接存活——这就是 [restrictToSingleWriter] 里 `SetMaxIdleConns(1)` +
//	`SetConnMaxLifetime(0)` + `SetConnMaxIdleTime(0)`（让那条连接常驻、永不因空闲被回收）
//	存在的原因。两者是**同一套策略的两半**：缓存共享定「库身份」，空闲锚点保「库不销」。
func memoryDSN() string {
	return fmt.Sprintf("file:memdb-%d-%d?mode=memory&cache=shared&%s",
		os.Getpid(), memSeq.Add(1), pragmaQuery(memoryPragmas))
}

// Store 是存储层句柄：内部持**唯一**的 `*sql.DB`（读与写都走它）。
//
// 与 Rust `Store`（内部只有一个 `mpsc::Sender`）对应：`*sql.DB` 本身即并发安全，
// 因此 Go 侧无需 `Clone`，多 goroutine 直接共用一个 `*Store` 即可。
type Store struct {
	db *sql.DB
}

// restrictToSingleWriter 把连接池限制为**单连接**，这是「写串行、无 SQLITE_BUSY」的根因。
//
//   - SetMaxOpenConns(1)：等价于 Rust「唯一 OS 线程持有 Connection 顺序执行」。
//   - SetMaxIdleConns(1)：保留唯一连接常驻（内存库「库不销」的锚点）。
//   - SetConnMaxLifetime(0) / SetConnMaxIdleTime(0)：连接**永不**因寿命/空闲被回收，
//     从而杜绝「连接被换掉 ⇒ 内存库被换掉」。
func restrictToSingleWriter(db *sql.DB) {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
}

// OpenBootstrap 打开一个**尚未限制连接数**的引导连接池（磁盘库），并设好 PRAGMA。
//
// 用途：仅供**迁移阶段**使用——严格对应 Rust 的 `open_connection()`。
// 调用者拿到它以后**必须先 `migrate.Apply(...)`，再**通过 [Open] / [restrictToSingleWriter]
// 把它变为受限的写池；**不得**把它当写池直接使用（否则会失去写串行化保证）。
//
// 会自动创建父目录（对齐 Rust `open_connection` 的 `create_dir_all(parent)`）。
func OpenBootstrap(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: 创建父目录 %q: %w", dir, err)
		}
	}
	db, err := sql.Open(DriverName, diskDSN(path, bootstrapPragmas))
	if err != nil {
		return nil, fmt.Errorf("store: 打开 %q: %w", path, err)
	}
	// sql.Open 是惰性的；Ping 会真正建连并按 DSN 执行 PRAGMA，
	// 这样连接期错误（路径不可写、PRAGMA 非法等）在此刻暴露，而不是拖到首次业务查询。
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 连接 %q: %w", path, err)
	}
	return db, nil
}

// Open 是磁盘库的**规范入口**，按唯一正确的顺序完成启动（顺序被封装在此，调用者无法搞反）：
//
//	open_connection()      → OpenBootstrap(path)   // 独立连接 + PRAGMA
//	migrate::apply()       → Apply(conn, dir)      // 迁移（写池受限**之前**）
//	from_connection()      → restrictToSingleWriter(db) → &Store{db}
//
// 【为何顺序不可换】迁移里会 `BeginTx` 开事务。若先 `SetMaxOpenConns(1)` 再迁移，
// 当迁移持有那唯一连接时，任何试图在**同一池**上再取第二连接的调用都会死锁。
// 因此迁移必须发生在写池受限之前、且用**一个独立连接**：这里用 `db.Conn(ctx)`
// 显式独占一条物理连接跑迁移（最贴近 Rust「把同一个 Connection 交给 actor」的语义）。
//
// migrationsDir 不存在时视为「零个迁移」，不报错（[Apply] 已保证）。
func Open(path, migrationsDir string) (*Store, error) {
	db, err := OpenBootstrap(path)
	if err != nil {
		return nil, err
	}

	// 独占一条独立物理连接做迁移（此时池尚未受限，不会与写池争抢）。
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 获取迁移连接: %w", err)
	}
	_, applyErr := Apply(conn, migrationsDir)
	closeErr := conn.Close()
	if applyErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 应用迁移: %w", applyErr)
	}
	if closeErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 归还迁移连接: %w", closeErr)
	}

	// 迁移完成，才把池限制为单写者——此后再进入「写串行」的稳态。
	restrictToSingleWriter(db)
	return &Store{db: db}, nil
}

// OpenInMemory 打开一个**仅测试用**的内存库 Store。
//
// 对齐 Rust `Store::open_in_memory()`：建一个空库 + 连接级 PRAGMA（无 WAL），
// 供上层自行建表。库身份与连接常驻性由 [memoryDSN] + [restrictToSingleWriter] 共同保证。
func OpenInMemory() (*Store, error) {
	db, err := sql.Open(DriverName, memoryDSN())
	if err != nil {
		return nil, fmt.Errorf("store: 打开内存库: %w", err)
	}
	restrictToSingleWriter(db)
	// 立即建连并执行 PRAGMA：把「唯一连接」锚定住（共享缓存内存库需 ≥1 连接存活）。
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 连接内存库: %w", err)
	}
	return &Store{db: db}, nil
}

// ---------------------------------------------------------------------------
// 事务原语与误用防护（design/12 §9）
// ---------------------------------------------------------------------------

// ErrTxActive 表示「在事务窗口内误用了 Store 的便捷方法（Execute/ExecuteBatch/Query），
// 或试图嵌套 [Store.InTx]」。
//
// ⚠️ **这是调用方误用，不是运行时状况 ⇒ 不得重试**（调用方不得据它做退避/重试）。
//
// 背景（design/12 §9）：写池只有一条连接，`InTx` 期间那条连接被事务独占。若 fn 内还走池
// （Execute 等），必然死锁（同包文档「坑 3」）。本哨兵把这种误用从「静默死锁」变成
// 「立即报错」。它**只**表示误用——并发读者（管理台/健康轮询，用**不带标记**的 ctx）
// 照常走池、短暂阻塞，不会被误报。
var ErrTxActive = errors.New("store: 事务窗口内不得调用 Execute/ExecuteBatch/Query，也不得嵌套 InTx（误用，不得重试）")

// ErrCommit 表示 [Store.InTx] 的 tx.Commit() 失败，**区别于 fn 返回的错误**：
// 调用方可用 errors.Is(err, ErrCommit) 把「业务错」与「提交期错」分开处理。
var ErrCommit = errors.New("store: 事务提交失败")

// txScopeKey 是 ctx 里事务作用域标记的**未导出** key —— 外部包无法构造，故标记无法伪造。
type txScopeKey struct{}

// txScope 是「当前是否仍在某个 InTx 事务窗口内」的标记，带**生命周期状态**：
//
//   - fn 内（窗口开着，active=true）拿同一个 ctx 再进 ⇒ 判为**嵌套**（拒绝）；
//   - 事务结束后再用**旧 ctx**（窗口已关，active=false）进 ⇒ 判为**新根事务**（放行）。
//
// 用 atomic：窗口关闭（invalidate）可能与「fn 泄漏给后台 goroutine 的 ctx」的读取并发。
type txScope struct {
	active atomic.Bool
}

func (sc *txScope) invalidate()    { sc.active.Store(false) }
func (sc *txScope) inWindow() bool { return sc.active.Load() }

// txScopeOf 取出 ctx 里的事务作用域标记（无则返回 (nil, false)）。
func txScopeOf(ctx context.Context) (*txScope, bool) {
	if ctx == nil {
		return nil, false
	}
	sc, ok := ctx.Value(txScopeKey{}).(*txScope)
	return sc, ok
}

// rejectInTx 是 Execute/ExecuteBatch/Query 入口的守卫：带**活跃**标记的 ctx ⇒ [ErrTxActive]。
func rejectInTx(ctx context.Context) error {
	if sc, ok := txScopeOf(ctx); ok && sc.inWindow() {
		return ErrTxActive
	}
	return nil
}

// InTx 在**单写者池**上开一个事务，把 tx 交给 fn：fn 返回 nil ⇒ 提交；返回错误 ⇒ 回滚。
//
// 严格按 design/12 §9：
//   - 仍走单写者池 `s.db.BeginTx`（**不另开连接、不另建池**）；
//   - 死锁/误用防护**随 ctx 走**（非全局标志）：传给 fn 的 ctx 携带一个未导出 key 的
//     事务作用域标记。带**活跃**标记的 ctx（只能来自 fn 的参数）调 Execute/ExecuteBatch/
//     Query ⇒ 立即 [ErrTxActive]；不带标记的 ctx（并发读者）⇒ 照常走池、短暂阻塞；
//   - **嵌套 InTx ⇒ [ErrTxActive]**（入口显式检查；否则内层会在已占用的池上开事务，
//     变成阻塞而非报错）。**绝不静默加入外层事务**——静默加入会让内层「回滚」把外层
//     一起回滚（最难查的一类 bug）。
//
// godoc 禁忌（违反会退化成死锁或误导性错误）：
//   - fn **不得**把它收到的 ctx 泄漏给「fn 返回后才跑」的 goroutine（那些 goroutine 之后
//     调 Execute 会拿到误导性的 ErrTxActive，尽管那时窗口其实已关）；
//   - fn **不得**把外层 ctx 传给 Execute/ExecuteBatch/Query（会绕过防护、退回死锁）。
//
// 事务必须短（design/12 §9「事务必须短」）：期间那唯一连接被独占，读写**全阻塞**；
// 禁止在 fn 内做网络 I/O 或等待外部事件（对应 §1.9.5：告警在提交**之后**发）。
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	// 嵌套拒绝：入口显式检查传入 ctx 是否已带**活跃**标记（窗口已关的旧标记不算）。
	if sc, ok := txScopeOf(ctx); ok && sc.inWindow() {
		return ErrTxActive
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启事务: %w", err)
	}
	sc := &txScope{}
	sc.active.Store(true)
	// 窗口在 InTx 返回前**必须失效**：否则「事务已结束还拿旧 ctx 再进 InTx」会被
	// 误判成嵌套（那其实是合法的新根事务）。
	defer sc.invalidate()

	scopeCtx := context.WithValue(ctx, txScopeKey{}, sc)
	if fnErr := fn(scopeCtx, tx); fnErr != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			// 回滚也失败：保留原始错误（errors.Is/As 仍可判），并附上回滚失败信息。
			return fmt.Errorf("store: 回滚失败(%v)：%w", rbErr, fnErr)
		}
		return fnErr // 原样返回（errors.Is/As 仍可判）
	}
	if commitErr := tx.Commit(); commitErr != nil {
		// 提交失败**可区分**：包装 [ErrCommit]。
		return fmt.Errorf("%w: %v", ErrCommit, commitErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 唯一性冲突判定
// ---------------------------------------------------------------------------

// uniqueViolationPrefix 是 SQLite 唯一约束冲突消息的固定前缀（**判据承重部分**）。
const uniqueViolationPrefix = "UNIQUE constraint failed: "

// IsUniqueViolation 判断 err 是否为**指定限定列**引发的唯一约束冲突。
//
// 判据**只落在错误文本**上：`"UNIQUE constraint failed: <table>.<column>"`。
// 理由（team-lead 2026-10-08 实测）：modernc.org/sqlite 的**扩展错误码**在不同版本、
// 不同约束类型下不稳定——同属唯一性冲突，UNIQUE 索引与 PRIMARY KEY 的码并不一致
// （实测 gid 冲突 = 2067、task_id 主键冲突 = 1555），故**错误码不作判据**，以文本为准。
//
// qualifiedColumn 必须是**限定列名**（如 `task.task_id`、`task_attempt.task_id`），
// 而非裸列名——同一列名在多表出现时（本库 task_id 在 task / task_attempt / task_file /
// dedup_index / task_progress 等都有），只有限定名才能区分「是哪张表的哪一列冲突」。
//
// 多列唯一约束（如 task_attempt 的 `UNIQUE(task_id, attempt_no)`）消息形如
// `"UNIQUE constraint failed: task_attempt.task_id, task_attempt.attempt_no"`，
// 文本包含给定限定列名即判 true。
func IsUniqueViolation(err error, qualifiedColumn string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), uniqueViolationPrefix+qualifiedColumn)
}

// ---------------------------------------------------------------------------
// 便捷读写（均带 ctx；事务窗口内一律 [ErrTxActive]）
// ---------------------------------------------------------------------------

// Execute 执行一条**写**语句（或任何不返回行的语句），返回受影响行数。
//
// 对齐 Rust `Store::execute`。⚠️ DDL 语句返回 0（与 Rust 一致，见包文档坑 4）。
//
// ctx 带**活跃**事务标记（即来自 [Store.InTx] 的 fn 参数）时立即返回 [ErrTxActive]：
// fn 内只能用 tx（单写者池的那条连接已被事务独占，再走池必死锁，见 [Store.InTx]）。
func (s *Store) Execute(ctx context.Context, sqlText string, args ...any) (int64, error) {
	if err := rejectInTx(ctx); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ExecuteBatch 执行一段 SQL 脚本（建表、多语句等）。
//
// 对齐 Rust `Store::execute_batch`。modernc 驱动支持单次 Exec 执行多语句脚本。
// 事务窗口内一律 [ErrTxActive]（见 [Store.Execute]）。
func (s *Store) ExecuteBatch(ctx context.Context, sqlText string) error {
	if err := rejectInTx(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, sqlText)
	return err
}

// Query 执行查询，返回按行组织的结果（每行是列值的切片）。
//
// 对齐 Rust `Store::query`（`Vec<Vec<Value>>`）。列值经 `any` 返回：
// INTEGER→int64、REAL→float64、TEXT→string、BLOB→[]byte、NULL→nil。
// 事务窗口内一律 [ErrTxActive]（见 [Store.Execute]）。
func (s *Store) Query(ctx context.Context, sqlText string, args ...any) ([][]any, error) {
	if err := rejectInTx(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Close 关闭连接池（优雅退出 / 测试收尾）。对齐 Rust `Store::shutdown`。
func (s *Store) Close() error {
	return s.db.Close()
}
