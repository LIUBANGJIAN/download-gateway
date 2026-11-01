package tasks

// `internal/tasks` 是**唯一**的任务入库入口与读路径，长期没有测试
// （旧账第 4 条）。这里补的不是"覆盖率"，而是三条会静默退化、且出事很难查的性质：
//
//  1. 入库的字段映射 —— `StartLater` / `Filename` / `Origin` 落错列，
//     现象是"任务能建但行为不对"（例如 start_later 被忽略 ⇒ 用户没点开始就开始下载）；
//  2. 读路径与写入的自洽 —— 建完立刻能按 id/GID 查回同样的值（read-after-write，
//     这是 Aria2 面 `addUri` 立刻返回可查的前提）；
//  3. 非法入参的分类 —— 不合法必须回 `*CreateError` 且 `Kind` 正确，
//     而不是一句 `sql: constraint failed`（那会把用户错误报成系统错误）。

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// openTasksStore 建一个带真实迁移的临时库。
func openTasksStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"), filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// ptr 取字符串地址。
func ptr(s string) *string { return &s }

// TestCreateTaskRoundTrip 守字段映射与 read-after-write。
func TestCreateTaskRoundTrip(t *testing.T) {
	st := openTasksStore(t)
	ctx := context.Background()

	in := TaskCreateInput{
		Kind:       domain.KindHTTP,
		URLRaw:     "https://pan.example.com/d/abc/01.ts",
		Filename:   ptr("Some.Show.S01E01.1080p.mkv"),
		StartLater: true,
		Origin:     domain.OriginAria2,
	}
	got, err := CreateTask(ctx, st, in)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if got.TaskID == "" || got.GID == "" {
		t.Fatalf("返回的 id/gid 为空: %+v", got)
	}

	row, err := GetByID(ctx, st, got.TaskID)
	if err != nil {
		t.Fatalf("按 id 回读失败: %v", err)
	}
	if row == nil {
		t.Fatal("刚建的任务按 id 查不到 —— read-after-write 失效")
	}
	if row.URLRaw != in.URLRaw {
		t.Fatalf("url_raw = %q，期望 %q", row.URLRaw, in.URLRaw)
	}
	if row.Name == nil || *row.Name != *in.Filename {
		t.Fatalf("name 未落库：%v", row.Name)
	}
	if row.InternalState != domain.StateQueued {
		t.Fatalf("internal_state = %q，期望 queued（受理语义）", row.InternalState)
	}
	// `StartLater` ⇒ 用户没点开始：必须是 paused + not_started，
	// 否则调度内核会把这个任务派出去（违背用户意图）。
	if row.AriaStatus != string(domain.AriaPaused) {
		t.Fatalf("start_later 任务 aria_status = %q，期望 paused", row.AriaStatus)
	}
	if row.ErrorClass == nil || *row.ErrorClass != domain.ErrNotStarted {
		t.Fatalf("start_later 任务 error_class = %v，期望 not_started", row.ErrorClass)
	}
	// `node_id` 必须为 NULL：入库阶段绝不落点（落点是调度内核的事）。
	if row.NodeID != nil {
		t.Fatalf("受理阶段 node_id 应保持 NULL，实际 %v", *row.NodeID)
	}

	// 按 GID 也要能查到（Aria2 面用 gid 通信）。
	byGID, err := GetByGID(ctx, st, got.GID)
	if err != nil {
		t.Fatalf("按 gid 回读失败: %v", err)
	}
	if byGID == nil || byGID.TaskID != got.TaskID {
		t.Fatalf("按 gid 查到的任务不匹配：%+v", byGID)
	}

	// 受理审计必须落一条（`task_accepted`）。
	evs, err := st.Query(ctx, `SELECT message FROM event_log WHERE task_id = ?1`, got.TaskID)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("受理后没有写 event_log（审计丢失）")
	}
	if msg, _ := evs[0][0].(string); msg != "task_accepted" {
		t.Fatalf("审计 message = %q，期望 task_accepted", msg)
	}
}

// TestCreateTaskRejectsBadInput 守非法入参分类。
//
// 反证：把校验删掉，SQLite 会给一句约束错误，调用方只能按字符串匹配判断
// —— 那正是"用户错误被报成系统错误"的形态。
func TestCreateTaskRejectsBadInput(t *testing.T) {
	st := openTasksStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   TaskCreateInput
		want CreateErrorKind
	}{
		{
			name: "空白 URL",
			in:   TaskCreateInput{Kind: domain.KindHTTP, URLRaw: "   "},
			want: CreateInvalidURL,
		},
		{
			name: "非法 kind",
			in:   TaskCreateInput{Kind: domain.TaskKind("not-a-kind"), URLRaw: "http://h/a.ts"},
			want: CreateInvalidKind,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := CreateTask(ctx, st, c.in)
			if err == nil {
				t.Fatal("非法入参应报错")
			}
			var ce *CreateError
			if !errors.As(err, &ce) {
				t.Fatalf("应回 *CreateError，实得 %T: %v", err, err)
			}
			if ce.Kind != c.want {
				t.Fatalf("Kind = %v，期望 %v", ce.Kind, c.want)
			}
		})
	}
}

// TestListTasksFiltersAndPaging 守读路径的过滤与分页。
//
// 这两件事一旦错，现象是"管理台列表少了几行"或"翻到第二页看到重复行"——
// 都属于用户会立刻发现、但开发者本地难以复现的问题。
func TestListTasksFiltersAndPaging(t *testing.T) {
	st := openTasksStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := CreateTask(ctx, st, TaskCreateInput{
			Kind:   domain.KindHTTP,
			URLRaw: "http://h/" + string(rune('a'+i)) + ".ts",
		}); err != nil {
			t.Fatalf("建任务失败: %v", err)
		}
	}

	q := DefaultTaskListQuery()
	q.Page, q.Size = 1, 2
	page, err := ListTasks(ctx, st, q)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if page.Total != 5 {
		t.Fatalf("Total = %d，期望 5", page.Total)
	}
	if len(page.Items) != 2 {
		t.Fatalf("第 1 页应有 2 条，实际 %d", len(page.Items))
	}

	// 第 3 页只剩 1 条（5 = 2+2+1）
	q.Page = 3
	page, err = ListTasks(ctx, st, q)
	if err != nil {
		t.Fatalf("第 3 页失败: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("第 3 页应有 1 条，实际 %d", len(page.Items))
	}

	// 状态过滤：全部是 queued
	st1 := "queued"
	q2 := DefaultTaskListQuery()
	q2.State = &st1
	page, err = ListTasks(ctx, st, q2)
	if err != nil {
		t.Fatalf("按状态过滤失败: %v", err)
	}
	if page.Total != 5 {
		t.Fatalf("queued 应有 5 条，实际 %d", page.Total)
	}

	// 过滤一个不存在状态 ⇒ 0 条（不是全量）
	st2 := "completed"
	q3 := DefaultTaskListQuery()
	q3.State = &st2
	page, err = ListTasks(ctx, st, q3)
	if err != nil {
		t.Fatalf("过滤失败: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("completed 应为 0 条，实际 %d（过滤没生效）", page.Total)
	}
}

// TestSetStateAndDelete 守两条写路径。
func TestSetStateAndDelete(t *testing.T) {
	st := openTasksStore(t)
	ctx := context.Background()

	got, err := CreateTask(ctx, st, TaskCreateInput{Kind: domain.KindHTTP, URLRaw: "http://h/x.ts"})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}

	cls := string(domain.ErrOutOfRetry)
	n, err := SetState(ctx, st, got.TaskID, "failed_terminal", "error", &cls)
	if err != nil {
		t.Fatalf("改状态失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应更新 1 行，实际 %d", n)
	}
	row, err := GetByID(ctx, st, got.TaskID)
	if err != nil || row == nil {
		t.Fatalf("回读失败: %v", err)
	}
	if row.InternalState != domain.StateFailedTerminal {
		t.Fatalf("internal_state = %q，期望 failed_terminal", row.InternalState)
	}

	n, err = DeleteTask(ctx, st, got.TaskID)
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应删除 1 行，实际 %d", n)
	}
	row, err = GetByID(ctx, st, got.TaskID)
	if err != nil {
		t.Fatalf("删除后回读失败: %v", err)
	}
	if row != nil {
		t.Fatal("删除后仍能查到任务")
	}
}
