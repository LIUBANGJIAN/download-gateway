package tasks

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// logf 是包级日志 seam（与 internal/secret 同形），便于测试静音/断言。
// 对齐 Rust 侧的 `tracing::warn!`（审计失败只告警，不影响受理结果）。
var logf = log.Printf

// TaskCreateInput 是入库输入。对应 Rust 侧 `tasks/create.rs::TaskCreateInput`。
type TaskCreateInput struct {
	// Kind 任务类型（↦ `task.kind` 的 CHECK 取值）。
	Kind domain.TaskKind
	// URLRaw 原始 URL / 磁力链 / 种子标识。
	URLRaw string
	// URLNorm 归一化 URL（本轮由调用方决定，多为 nil）。
	URLNorm *string
	// Filename ↦ `task.name`（aria2 `out` / bitcomet `filename`）。
	Filename *string
	// GroupID 任务组 id（本轮恒 nil）。
	GroupID *int64
	// SourceKey 网盘来源键（本轮恒 nil）。
	SourceKey *string
	// MaxConnectionCount 最大连接数（记元数据；本轮**不转发**给节点）。
	MaxConnectionCount *int64
	// StartLater true ⇒ `aria_status='paused'` + `error_class='not_started'`（用户未点开始）。
	StartLater bool
	// SaveFolderHint 保存目录提示；**收下不转发**，仅写 `event_log` 留痕（本轮无节点）。
	SaveFolderHint *string
	// Origin 调用来源。
	Origin domain.IngressOrigin
}

// TaskCreated 是入库结果。对应 Rust 侧 `tasks/create.rs::TaskCreated`。
type TaskCreated struct {
	// TaskID 新任务 id（ULID）。
	TaskID string
	// GID 新任务 GID（16 位十六进制）。
	GID string
	// GroupID 任务组 id（回显）。
	GroupID *int64
}

// CreateErrorKind 是 [`CreateError`] 的分类。对应 Rust 侧 `CreateError` 的四个变体。
type CreateErrorKind int

const (
	// CreateInvalidURL URL 非法（空白等）。
	CreateInvalidURL CreateErrorKind = iota
	// CreateInvalidKind `kind` 非法。
	CreateInvalidKind
	// CreateGIDExhausted GID 连续冲突过多。
	CreateGIDExhausted
	// CreateStore 存储层错误。
	CreateStore
)

// CreateError 是入库错误。对应 Rust 侧 `tasks/create.rs::CreateError`。
//
// 用 `errors.As(err, &*CreateError)` 判读 `Kind`；当 `Kind == CreateStore` 时，
// [`CreateError.Unwrap`] 让 `errors.Is/As` 能穿透到 underlying 的 store 错误。
type CreateError struct {
	// Kind 错误类别。
	Kind CreateErrorKind
	// Detail 附加信息（InvalidURL 时为原 url；InvalidKind 时为原 kind）。
	Detail string
	// Err underlying 的存储层错误（仅 CreateStore）。
	Err error
}

// Error 实现 error。文案对齐 Rust 侧 `CreateError` 的 `#[error(...)]`。
func (e *CreateError) Error() string {
	switch e.Kind {
	case CreateInvalidURL:
		return "url 非法: " + e.Detail
	case CreateInvalidKind:
		return "kind 非法: " + e.Detail
	case CreateGIDExhausted:
		return "GID 连续冲突过多"
	case CreateStore:
		if e.Err != nil {
			return "存储层错误: " + e.Err.Error()
		}
		return "存储层错误"
	default:
		return "创建任务失败"
	}
}

// Unwrap 暴露 underlying 存储错误，供 errors.Is/As 穿透。
func (e *CreateError) Unwrap() error { return e.Err }

// gidAttempts 是 GID 冲突时的最大尝试次数。
//
// Rust 侧注释逐字：「GID 冲突：单语句语义 —— INSERT 撞 `gid UNIQUE` 就换一个 GID
// 重试，**≤8 次**」。**无退避**（Rust 里没有 sleep/backoff）—— 不要顺手加。
const gidAttempts = 8

// CreateTask 是**唯一入库入口**（对齐 Rust `create_task`）。
//
// 三个入口全部走它，禁止任何地方另写 `INSERT INTO task`：
// Aria2 兼容面 `aria2.addUri`、BitComet 兼容面 `http/add`·`bt/add`·`torrent_links/add`、
// 管理台 `POST /api/admin/tasks`。
//
// 语义 = **受理 + 入库 + 排队**（`internal_state='queued'`, `node_id=NULL`,
// `dispatch_reason=NULL`）。本轮无节点 ⇒ 任务会长期停在排队态，这是**有意的诚实行为**，
// 接入面绝不假装已下发。
//
// # 事务取舍（design-1 §3.0(c)）
//
// 本轮唯一的多语句写是 `INSERT task`（权威）+ `INSERT event_log`（审计），二者
// **无共同不变量** ⇒ 不用事务。强制顺序：先写 `task`（权威），成功后再写 `event_log`；
// 第二步失败只告警，**不影响**接口返回成功（用户已受理）。反之第一步失败 ⇒ 无部分状态，直接报错。
func CreateTask(ctx context.Context, s *store.Store, input TaskCreateInput) (TaskCreated, error) {
	// 与 Rust `create_task` → `create_task_using(store, input, ids::new_gid)` 同形。
	// seam A：GID 生成器**以参数注入**（测试可注入确定性 GID），核心逻辑不硬编码 ids。
	return CreateTaskUsing(ctx, s, input, ids.NewGID)
}

// CreateTaskUsing 是内部实现，允许注入 GID 生成器（便于单测「GID 冲突重试」）。
// 对应 Rust 侧 `create_task_using`。
//
// gidGen 每次调用应产出一个 GID；撞 `task.gid UNIQUE` 时会再次调用它（至多 [`gidAttempts`] 次）。
func CreateTaskUsing(
	ctx context.Context,
	s *store.Store,
	input TaskCreateInput,
	gidGen func() string,
) (TaskCreated, error) {
	urlRaw := strings.TrimSpace(input.URLRaw)
	if urlRaw == "" {
		return TaskCreated{}, &CreateError{Kind: CreateInvalidURL, Detail: input.URLRaw}
	}

	// ⚠️ 与 Rust 的**有意差异**（已报 team-lead，逐字理由如下）：
	//
	//	Rust 的 `TaskKind` 是 **enum** ⇒ `kind` 恒合法，故 `create_task` **不校验**、
	//	`CreateError::InvalidKind` 变体在 Rust 里**不可达**。
	//	Go 的 `domain.TaskKind` 是**开放字符串类型**，非法值可表示；若不在此拦住，
	//	非法 kind 会在 SQL 层撞 `task.kind` 的 CHECK 约束、被包成 `CreateStore`——
	//	那与 Rust 的**可观测行为不同**（Rust 根本走不到那里）。
	//	⇒ 此处做一次运行时校验，**等价于把 Rust 的编译期类型保证在 Go 里补成运行时检查**，
	//	**不新增任何值域**（仍只允许 DDL CHECK 里的 http/bt/magnet/torrent）。
	if !input.Kind.Valid() {
		return TaskCreated{}, &CreateError{Kind: CreateInvalidKind, Detail: string(input.Kind)}
	}

	now := ids.NowMs()
	taskID := ids.NewTaskID(now)

	// start_later ⇒ 用户未点开始：aria_status='paused' 且 error_class='not_started'。
	ariaStatus := "waiting"
	var errorClass *string
	if input.StartLater {
		ariaStatus = "paused"
		ec := string(domain.ErrNotStarted)
		errorClass = &ec
	}
	groupID := input.GroupID

	for i := 0; i < gidAttempts; i++ {
		gid := gidGen()
		// 列序与 insertTaskSQL 的 ?1..?24 一一对应（与 Rust 侧 params 向量同序）。
		args := []any{
			taskID,                   // 1  task_id
			gid,                      // 2  gid
			optI64(groupID),          // 3  group_id
			optText(input.SourceKey), // 4  source_key
			string(input.Kind),       // 5  kind
			urlRaw,                   // 6  url_raw
			optText(input.URLNorm),   // 7  url_norm
			optText(input.Filename),  // 8  name
			"unknown",                // 9  size_state
			nil,                      // 10 total_size
			int64(0),                 // 11 downloaded_size
			int64(0),                 // 12 permillage
			ariaStatus,               // 13 aria_status
			"queued",                 // 14 internal_state（受理语义，恒 queued）
			optText(errorClass),      // 15 error_class
			nil,                      // 16 error_message
			nil,                      // 17 node_id（本轮恒 NULL）
			nil,                      // 18 node_task_id
			int64(0),                 // 19 attempt_no
			int64(3),                 // 20 max_attempts
			int64(0),                 // 21 next_retry_at
			int64(0),                 // 22 possible_duplicate
			now,                      // 23 created_at
			now,                      // 24 updated_at
		}

		if _, err := s.Execute(ctx, insertTaskSQL, args...); err != nil {
			if isGIDConflict(err) {
				logf("GID 冲突，换新 GID 重试: gid=%s", gid)
				continue
			}
			return TaskCreated{}, &CreateError{Kind: CreateStore, Err: err}
		}
		// 权威行已写入，接口即视为成功。审计 best-effort（失败只告警）。
		writeAcceptAudit(ctx, s, taskID, input, now)
		return TaskCreated{TaskID: taskID, GID: gid, GroupID: groupID}, nil
	}
	return TaskCreated{}, &CreateError{Kind: CreateGIDExhausted}
}

// insertTaskSQL 是 `INSERT INTO task` 的**唯一** SQL 文案（与 `V1__init.sql` 的列一一对应，无新列）。
// 逐字移植自 Rust 侧 `tasks/create.rs::INSERT_TASK_SQL`。
//
// ⚠️ 刻意**不写** `dispatch_reason`（本轮恒 NULL）、`dedup_key`/`dedup_level`/`episode_no`/
// `progress_at`/`batch_pending`/`completed_at` —— 它们走 DDL 默认值（与 Rust 一致）。
const insertTaskSQL = `INSERT INTO task (
  task_id, gid, group_id, source_key, kind, url_raw, url_norm, name,
  size_state, total_size, downloaded_size, permillage, aria_status, internal_state,
  error_class, error_message, node_id, node_task_id, attempt_no, max_attempts,
  next_retry_at, possible_duplicate, created_at, updated_at
) VALUES (
  ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8,
  ?9, ?10, ?11, ?12, ?13, ?14,
  ?15, ?16, ?17, ?18, ?19, ?20,
  ?21, ?22, ?23, ?24
)`

// isGIDConflict 判断错误是否为 `task.gid` 的唯一约束冲突（而非其它约束错误）。
//
// ⚠️ 与 Rust 的差异（**行为面收紧**，已报 team-lead）：
//
//	Rust `is_gid_conflict` = `matches!(code, ConstraintViolation) && e.to_string().contains("task.gid")`
//	—— 只要求错误文本**包含** `"task.gid"`。
//	Go 侧走 [`store.IsUniqueViolation`]`(err, "task.gid")`，要求文本包含
//	`"UNIQUE constraint failed: task.gid"`（**多了固定前缀**）。
//
// 现实中两者等价（SQLite 的唯一冲突消息恒为该前缀形式）；此处收紧是为避免把
// "引用了 task.gid 的其它错误" 误判成冲突。另：不再单独判 `ErrorCode::ConstraintViolation`
// —— modernc 的扩展错误码在不同约束类型下不稳定（见 store.IsUniqueViolation 的注释）。
func isGIDConflict(err error) bool {
	return store.IsUniqueViolation(err, "task.gid")
}

// writeAcceptAudit 在受理成功后写一条 `event_log`（level='info', category='task',
// message='task_accepted'）。**best-effort**：失败只告警，不影响受理结果（对齐 Rust）。
//
// detail 的**字段与顺序**逐字对齐 Rust：origin / kind / save_folder_hint / max_connection_count
// （后两者为 Option ⇒ nil 时序列化为 JSON null，与 serde_json 一致）。
func writeAcceptAudit(ctx context.Context, s *store.Store, taskID string, input TaskCreateInput, now int64) {
	detail := struct {
		Origin             string  `json:"origin"`
		Kind               string  `json:"kind"`
		SaveFolderHint     *string `json:"save_folder_hint"`
		MaxConnectionCount *int64  `json:"max_connection_count"`
	}{
		Origin:             string(input.Origin),
		Kind:               string(input.Kind),
		SaveFolderHint:     input.SaveFolderHint,
		MaxConnectionCount: input.MaxConnectionCount,
	}
	b, err := json.Marshal(detail)
	if err != nil {
		logf("序列化 event_log detail 失败（不影响受理结果）: task_id=%s err=%v", taskID, err)
		return
	}
	if _, err := s.Execute(ctx,
		"INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at) "+
			"VALUES ('info', 'task', NULL, ?1, 'task_accepted', ?2, ?3)",
		taskID, string(b), now); err != nil {
		logf("写 event_log 审计失败（不影响受理结果）: task_id=%s err=%v", taskID, err)
	}
}

// optText 把可空字符串转成 SQL 绑定值（nil ⇒ NULL）。对齐 Rust 侧 `opt_text`。
func optText(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// optI64 把可空整数转成 SQL 绑定值（nil ⇒ NULL）。对齐 Rust 侧 `opt_i64`。
func optI64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
