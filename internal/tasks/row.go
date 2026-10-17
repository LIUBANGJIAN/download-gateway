// Package tasks 是「任务」领域的读写层：唯一入库入口 CreateTask + 读路径 query。
//
// # 1:1 移植来源
//
// 本包是 Rust 版 `crates/dispatch-core/src/tasks/`（tag `rust-final-0.1.3`，冻结）的
// **行为等价**移植，逐函数对齐：
//
//   - `tasks/create.rs` → 本包 `create.go`（受理语义 / GID 冲突重试 / seam A / event_log 审计）
//   - `tasks/query.rs`  → 本包 `query.go`（查询 / 分页 / 状态变更 / 删除）
//   - `tasks/row.rs`    → 本包 `row.go`（列清单 + 行解码）
//
// # 分层说明（Go 侧与 Rust 的一处**结构性**差异）
//
// Rust 把纯数据模型（`TaskKind` / `IngressOrigin` / `TaskRow`）放在 `tasks/` 模块里；
// Go 侧这些**纯数据/纯函数已下沉到 `internal/domain`**（domain 的包文档明确：
// domain 只有纯数据与纯函数，不依赖 DB/HTTP）。因此：
//
//	Rust `tasks::row::TaskRow`      → Go `internal/domain.TaskRow`
//	Rust `tasks::create::TaskKind`  → Go `internal/domain.TaskKind`
//	Rust `tasks::create::IngressOrigin` → Go `internal/domain.IngressOrigin`
//
// 这样做**没有语义改动**：字段与取值集逐条一致（domain 的取值集由
// `TestEnumSetsMatchFrozenDDL` 直接从 `migrations/V1__init.sql` 抽取比对）。
// 本包只负责「与 store 交互的读写路径」。
//
// # 列清单冻结
//
// 所有 `SELECT` 显式列出 25 个列名、顺序与 [`taskColumns`] **逐字一致**（禁止 `SELECT *`）。
// 列序漂移会被 [`DecodeTask`] 的元数/类型断言在测试里捕获（对齐 Rust 的
// `debug_assert_eq!(row.len(), N_TASK_COLUMNS)`）。
package tasks

import (
	"fmt"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
)

// taskColumns 是 `task` 表被 SELECT 的 25 列（顺序冻结）。
//
// 与 Rust 侧 `tasks/row.rs::TASK_COLUMNS` **逐字一致**；必须与 query.go 的
// [`selectCols`] 及每条 SELECT 语句同序。
var taskColumns = []string{
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
}

// nTaskColumns 是列数（= len(taskColumns)）。对应 Rust 侧 `N_TASK_COLUMNS`。
const nTaskColumns = 25

// 列索引常量（顺序冻结）。对应 Rust 侧 `row.rs::I_*`。
const (
	iTaskID            = 0
	iGID               = 1
	iGroupID           = 2
	iSourceKey         = 3
	iKind              = 4
	iURLRaw            = 5
	iURLNorm           = 6
	iName              = 7
	iSizeState         = 8
	iTotalSize         = 9
	iDownloadedSize    = 10
	iPermillage        = 11
	iAriaStatus        = 12
	iInternalState     = 13
	iErrorClass        = 14
	iErrorMessage      = 15
	iNodeID            = 16
	iNodeTaskID        = 17
	iAttemptNo         = 18
	iMaxAttempts       = 19
	iNextRetryAt       = 20
	iPossibleDuplicate = 21
	iCreatedAt         = 22
	iUpdatedAt         = 23
	iCompletedAt       = 24
)

// DecodeErrorKind 区分解码错误的两类。对应 Rust 侧 `DecodeError` 的两个变体。
type DecodeErrorKind int

const (
	// DecodeArity 列数不符。
	DecodeArity DecodeErrorKind = iota
	// DecodeType 指定列类型不符。
	DecodeType
)

// DecodeError 是行解码错误。对应 Rust 侧 `tasks/row.rs::DecodeError`。
//
// 用 `errors.As(err, &*DecodeError)` 判读；`Kind` 区分两类。
type DecodeError struct {
	// Kind 错误类别。
	Kind DecodeErrorKind
	// Want Arity 时：期望列数。
	Want int
	// Got Arity 时：实得列数。
	Got int
	// Column Type 时：出错的列索引。
	Column int
}

// Error 实现 error。文案对齐 Rust 侧 `Display`。
func (e *DecodeError) Error() string {
	if e.Kind == DecodeArity {
		return fmt.Sprintf("列数不符: 期望 %d 实得 %d", e.Want, e.Got)
	}
	return fmt.Sprintf("列 %d 类型不符", e.Column)
}

// DecodeTask 把一行列值（`store.Query` 返回的 `[][]any` 里的一行）解码为 [`domain.TaskRow`]。
//
// 对应 Rust 侧 `tasks/row.rs::decode_task`：列数不符 ⇒ [`DecodeArity`]；
// 任一列类型不符 ⇒ [`DecodeType`]。SQL NULL 映射为 nil 指针 / 零值 bool。
func DecodeTask(row []any) (domain.TaskRow, error) {
	if len(row) != nTaskColumns {
		return domain.TaskRow{}, &DecodeError{Kind: DecodeArity, Want: nTaskColumns, Got: len(row)}
	}
	var (
		t   domain.TaskRow
		err error
	)
	if t.TaskID, err = asText(row, iTaskID); err != nil {
		return domain.TaskRow{}, err
	}
	if t.GID, err = asOptText(row, iGID); err != nil {
		return domain.TaskRow{}, err
	}
	if t.GroupID, err = asOptInt(row, iGroupID); err != nil {
		return domain.TaskRow{}, err
	}
	if t.SourceKey, err = asOptText(row, iSourceKey); err != nil {
		return domain.TaskRow{}, err
	}
	{
		var s string
		if s, err = asText(row, iKind); err != nil {
			return domain.TaskRow{}, err
		}
		t.Kind = domain.TaskKind(s)
	}
	if t.URLRaw, err = asText(row, iURLRaw); err != nil {
		return domain.TaskRow{}, err
	}
	if t.URLNorm, err = asOptText(row, iURLNorm); err != nil {
		return domain.TaskRow{}, err
	}
	if t.Name, err = asOptText(row, iName); err != nil {
		return domain.TaskRow{}, err
	}
	{
		var s string
		if s, err = asText(row, iSizeState); err != nil {
			return domain.TaskRow{}, err
		}
		t.SizeState = domain.SizeState(s)
	}
	if t.TotalSize, err = asOptInt(row, iTotalSize); err != nil {
		return domain.TaskRow{}, err
	}
	if t.DownloadedSize, err = asInt(row, iDownloadedSize); err != nil {
		return domain.TaskRow{}, err
	}
	if t.Permillage, err = asInt(row, iPermillage); err != nil {
		return domain.TaskRow{}, err
	}
	if t.AriaStatus, err = asText(row, iAriaStatus); err != nil {
		return domain.TaskRow{}, err
	}
	{
		var s string
		if s, err = asText(row, iInternalState); err != nil {
			return domain.TaskRow{}, err
		}
		t.InternalState = domain.InternalState(s)
	}
	{
		var ec *string
		if ec, err = asOptText(row, iErrorClass); err != nil {
			return domain.TaskRow{}, err
		}
		if ec != nil {
			v := domain.ErrorClass(*ec)
			t.ErrorClass = &v
		}
	}
	if t.ErrorMessage, err = asOptText(row, iErrorMessage); err != nil {
		return domain.TaskRow{}, err
	}
	if t.NodeID, err = asOptInt(row, iNodeID); err != nil {
		return domain.TaskRow{}, err
	}
	if t.NodeTaskID, err = asOptText(row, iNodeTaskID); err != nil {
		return domain.TaskRow{}, err
	}
	if t.AttemptNo, err = asInt(row, iAttemptNo); err != nil {
		return domain.TaskRow{}, err
	}
	if t.MaxAttempts, err = asInt(row, iMaxAttempts); err != nil {
		return domain.TaskRow{}, err
	}
	if t.NextRetryAt, err = asInt(row, iNextRetryAt); err != nil {
		return domain.TaskRow{}, err
	}
	if t.PossibleDuplicate, err = asBool(row, iPossibleDuplicate); err != nil {
		return domain.TaskRow{}, err
	}
	if t.CreatedAt, err = asInt(row, iCreatedAt); err != nil {
		return domain.TaskRow{}, err
	}
	if t.UpdatedAt, err = asInt(row, iUpdatedAt); err != nil {
		return domain.TaskRow{}, err
	}
	if t.CompletedAt, err = asOptInt(row, iCompletedAt); err != nil {
		return domain.TaskRow{}, err
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// 值提取器（对应 Rust 侧 `row.rs::ValueExt` 的五个方法）
// ---------------------------------------------------------------------------

// asInt 取 INTEGER 列；非 int64 ⇒ DecodeType。
func asInt(row []any, idx int) (int64, error) {
	if v, ok := row[idx].(int64); ok {
		return v, nil
	}
	return 0, &DecodeError{Kind: DecodeType, Column: idx}
}

// asOptInt 取可空 INTEGER 列；NULL ⇒ nil。
func asOptInt(row []any, idx int) (*int64, error) {
	if row[idx] == nil {
		return nil, nil
	}
	if v, ok := row[idx].(int64); ok {
		return &v, nil
	}
	return nil, &DecodeError{Kind: DecodeType, Column: idx}
}

// asText 取 TEXT 列；非文本 ⇒ DecodeType。
//
// 同时接受 `string` 与 `[]byte`：modernc 驱动对 TEXT 列返回 `string`，
// 但不同驱动/版本可能返回 `[]byte`；两者都当作文本，避免无谓的驱动耦合。
func asText(row []any, idx int) (string, error) {
	switch v := row[idx].(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	default:
		return "", &DecodeError{Kind: DecodeType, Column: idx}
	}
}

// asOptText 取可空 TEXT 列；NULL ⇒ nil。
func asOptText(row []any, idx int) (*string, error) {
	if row[idx] == nil {
		return nil, nil
	}
	s, err := asText(row, idx)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// asBool 取布尔列（DDL 里的 `possible_duplicate` 存 0/1）；非 INTEGER ⇒ DecodeType。
// 对齐 Rust 侧 `as_bool`（`*v != 0`）。
func asBool(row []any, idx int) (bool, error) {
	if v, ok := row[idx].(int64); ok {
		return v != 0, nil
	}
	return false, &DecodeError{Kind: DecodeType, Column: idx}
}
