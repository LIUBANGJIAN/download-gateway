package tasks

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// PageDefault 是默认分页大小。对应 Rust 侧 `tasks/query.rs::PAGE_DEFAULT`。
const PageDefault = 50

// selectCols 是 SELECT 的列出串（与 row.go 的 [`taskColumns`] **同序**）。
// 对应 Rust 侧 `tasks/query.rs::SELECT_COLS`。
//
// 所有 SELECT 都用它，**禁止 `SELECT *`** —— 列序漂移会被 [`DecodeTask`] 捕获。
const selectCols = "task_id, gid, group_id, source_key, kind, url_raw, url_norm, name, " +
	"size_state, total_size, downloaded_size, permillage, aria_status, internal_state, " +
	"error_class, error_message, node_id, node_task_id, attempt_no, max_attempts, " +
	"next_retry_at, possible_duplicate, created_at, updated_at, completed_at"

// TaskListQuery 是列表查询条件（管理台 `GET /api/admin/tasks` 直接反序列化）。
// 对应 Rust 侧 `TaskListQuery`。
type TaskListQuery struct {
	// Group 按组过滤。
	Group *int64
	// Source 按来源过滤。
	Source *string
	// Node 按节点过滤。
	Node *int64
	// State 按内部状态过滤。
	State *string
	// Q 关键字（匹配 url_raw / name）。
	Q *string
	// Page 页码（从 1 开始）。
	Page int64
	// Size 每页条数。
	Size int64
}

// DefaultTaskListQuery 返回默认查询（page=1, size=[`PageDefault`]）。
// 对应 Rust 侧 `TaskListQuery::default()`（serde 默认值 page=1 / size=PAGE_DEFAULT）。
func DefaultTaskListQuery() TaskListQuery {
	return TaskListQuery{Page: 1, Size: PageDefault}
}

// TaskListPage 是一页列表结果。对应 Rust 侧 `TaskListPage`。
type TaskListPage struct {
	// Items 当前页条目。
	Items []domain.TaskRow
	// Page 页码。
	Page int64
	// Size 每页条数（已 clamp 到 [1,500]）。
	Size int64
	// Total 过滤后的总数。
	Total int64
}

// StateCount 是「某内部状态 → 计数」。对应 Rust 侧 `count_by_internal_state` 的
// `Vec<(String, i64)>` 元素。
type StateCount struct {
	// State 内部状态。
	State string
	// Count 该状态的任务数。
	Count int64
}

// rowsToTasks 把 `store.Query` 的多行结果解码为 `[]domain.TaskRow`。
//
// 列数/类型不符是**编程错误**（SELECT 列清单被改坏），用错误上抛，避免静默错位
// （对齐 Rust 侧 `rows_to_tasks` 用存储错误包装上抛）。
func rowsToTasks(rows [][]any) ([]domain.TaskRow, error) {
	out := make([]domain.TaskRow, 0, len(rows))
	for _, r := range rows {
		t, err := DecodeTask(r)
		if err != nil {
			return nil, fmt.Errorf("任务行解码失败: %w", err)
		}
		out = append(out, t)
	}
	return out, nil
}

// placeholdersFrom 生成 `?start, ?start+1, …, ?start+n-1`。
func placeholdersFrom(start, n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = "?" + strconv.Itoa(start+i)
	}
	return strings.Join(parts, ", ")
}

// GetByGID 按 GID 查询单条；不存在返回 (nil, nil)。对应 Rust 侧 `get_by_gid`。
func GetByGID(ctx context.Context, s *store.Store, gid string) (*domain.TaskRow, error) {
	rows, err := s.Query(ctx, "SELECT "+selectCols+" FROM task WHERE gid = ?1 LIMIT 1", gid)
	if err != nil {
		return nil, err
	}
	list, err := rowsToTasks(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

// GetByID 按 task_id 查询单条；不存在返回 (nil, nil)。对应 Rust 侧 `get_by_id`。
func GetByID(ctx context.Context, s *store.Store, taskID string) (*domain.TaskRow, error) {
	rows, err := s.Query(ctx, "SELECT "+selectCols+" FROM task WHERE task_id = ?1 LIMIT 1", taskID)
	if err != nil {
		return nil, err
	}
	list, err := rowsToTasks(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

// ListByIDs 按一批 task_id 查询（顺序不保证）。对应 Rust 侧 `list_by_ids`。
// 空入参 ⇒ 返回空切片（nil），不查库。
func ListByIDs(ctx context.Context, s *store.Store, taskIDs []string) ([]domain.TaskRow, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	args := make([]any, len(taskIDs))
	for i, id := range taskIDs {
		args[i] = id
	}
	sql := "SELECT " + selectCols + " FROM task WHERE task_id IN (" +
		placeholdersFrom(1, len(taskIDs)) + ")"
	rows, err := s.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rowsToTasks(rows)
}

// ListTasks 分页列表（带可选过滤）。对应 Rust 侧 `list_tasks`。
//
// 排序键 = `ORDER BY created_at DESC, task_id DESC`：第二键 `task_id` 是**必定的稳定序**
// （同一 `created_at` 的两行，顺序必须由 `task_id` 决定），省掉它会造成分页抖动。
//
// `size` 被 clamp 到 [1,500]；`page`（≥1）的合法性由上游 admin 层校验，此处只对 OFFSET
// 做非负防御 —— 与 Rust 逐字一致。
func ListTasks(ctx context.Context, s *store.Store, q TaskListQuery) (TaskListPage, error) {
	var (
		wheres []string
		args   []any
	)
	// push 把片段里的单个 `?` 替换成带序号的 `?N`（对齐 Rust 的 `frag.replace("?", "?N")`）。
	push := func(frag string, p any) {
		args = append(args, p)
		wheres = append(wheres, strings.Replace(frag, "?", "?"+strconv.Itoa(len(args)), 1))
	}

	if q.Group != nil {
		push("group_id = ?", *q.Group)
	}
	if q.Source != nil {
		push("source_key = ?", *q.Source)
	}
	if q.Node != nil {
		push("node_id = ?", *q.Node)
	}
	if q.State != nil {
		push("internal_state = ?", *q.State)
	}
	if q.Q != nil {
		like := "%" + *q.Q + "%"
		args = append(args, like)
		i := len(args)
		// 一个参数在 url_raw / name 两处复用同一序号 `?i`（对齐 Rust）。
		wheres = append(wheres, fmt.Sprintf("(url_raw LIKE ?%d OR name LIKE ?%d)", i, i))
	}

	whereClause := ""
	if len(wheres) > 0 {
		whereClause = " WHERE " + strings.Join(wheres, " AND ")
	}

	// 总数
	countRows, err := s.Query(ctx, "SELECT COUNT(*) FROM task"+whereClause, args...)
	if err != nil {
		return TaskListPage{}, err
	}
	var total int64
	if len(countRows) > 0 && len(countRows[0]) > 0 {
		if n, ok := countRows[0][0].(int64); ok {
			total = n
		}
	}

	size := min(max(q.Size, 1), 500)
	// page 合法性（≥ 1）由上游校验；此处仅对 OFFSET 做非负防御。
	offset := max(q.Page-1, 0) * size
	listSQL := "SELECT " + selectCols + " FROM task" + whereClause +
		" ORDER BY created_at DESC, task_id DESC LIMIT " + strconv.FormatInt(size, 10) +
		" OFFSET " + strconv.FormatInt(offset, 10)
	rows, err := s.Query(ctx, listSQL, args...)
	if err != nil {
		return TaskListPage{}, err
	}
	items, err := rowsToTasks(rows)
	if err != nil {
		return TaskListPage{}, err
	}
	return TaskListPage{Items: items, Page: q.Page, Size: size, Total: total}, nil
}

// ListByStates 按内部状态集合列出（取最新 `limit` 条）。
// 对应 Rust 侧 `list_by_states`：空入参 ⇒ 空切片；`limit` clamp 到 ≥0。
func ListByStates(ctx context.Context, s *store.Store, states []string, limit int64) ([]domain.TaskRow, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(states)+1)
	for _, st := range states {
		args = append(args, st)
	}
	args = append(args, max(limit, 0))
	limIdx := len(args)
	sql := "SELECT " + selectCols + " FROM task WHERE internal_state IN (" +
		placeholdersFrom(1, len(states)) + ") " +
		"ORDER BY created_at DESC, task_id DESC LIMIT ?" + strconv.Itoa(limIdx)
	rows, err := s.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rowsToTasks(rows)
}

// CountByInternalState 各内部状态计数（管理台总览用）。对应 Rust 侧 `count_by_internal_state`。
//
// 返回顺序由 SQLite 的 GROUP BY 决定（不保证），调用方应按 `State` 建索引，勿依赖顺序。
func CountByInternalState(ctx context.Context, s *store.Store) ([]StateCount, error) {
	rows, err := s.Query(ctx, "SELECT internal_state, COUNT(*) FROM task GROUP BY internal_state")
	if err != nil {
		return nil, err
	}
	out := make([]StateCount, 0, len(rows))
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		st, ok := r[0].(string)
		if !ok {
			continue
		}
		n, ok := r[1].(int64)
		if !ok {
			continue
		}
		out = append(out, StateCount{State: st, Count: n})
	}
	return out, nil
}

// CountAll 全表任务数。对应 Rust 侧 `count_all`。
func CountAll(ctx context.Context, s *store.Store) (int64, error) {
	rows, err := s.Query(ctx, "SELECT COUNT(*) FROM task")
	if err != nil {
		return 0, err
	}
	if len(rows) > 0 && len(rows[0]) > 0 {
		if n, ok := rows[0][0].(int64); ok {
			return n, nil
		}
	}
	return 0, nil
}

// SetState 状态变更（pause/unpause/retry）：更新内部状态 + Aria2 状态 + 错误类。
// 返回受影响行数。对应 Rust 侧 `set_state`。
//
// ⚠️ A 类阶段**实际可写集合 = {queued, paused, removed}**（`failed` 无写入点，
// **不在此"顺手补上"**）。本函数**不校验** internal/aria/errClass 的取值（与 Rust 的
// `&str` / `Option<&str>` 签名一致）—— 值域约束由调用方（admin 层）保证；
// 在此处新增校验会**收紧**行为面，不是 1:1 移植。
func SetState(ctx context.Context, s *store.Store, taskID, internal, aria string, errClass *string) (int64, error) {
	return s.Execute(ctx,
		"UPDATE task SET internal_state = ?1, aria_status = ?2, error_class = ?3, updated_at = ?4 "+
			"WHERE task_id = ?5",
		internal, aria, optText(errClass), ids.NowMs(), taskID)
}

// DeleteTask 删除任务行（`task_file`/`task_progress` 由 `ON DELETE CASCADE` 清理）。
// 返回受影响行数。对应 Rust 侧 `delete_task`。
//
// 注意：**只删库行，不删节点文件**（`02 §4.1.2` + Q5）。
func DeleteTask(ctx context.Context, s *store.Store, taskID string) (int64, error) {
	return s.Execute(ctx, "DELETE FROM task WHERE task_id = ?1", taskID)
}
