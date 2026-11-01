package ingress

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/handshake"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/state"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
	"github.com/LIUBANGJIAN/download-gateway/internal/tasks"
)

// newAriaEnv 建「真库 + 真 handler」，并把 store 一起返回（要自己造数据的用例需要它）。
func newAriaEnv(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "dispatch.db")
	st, err := store.Open(dbPath, filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("打开真实库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	tok := testToken
	ps := state.NewPublicState(st, ids.NowSecs(), nil, &tok, false, state.HandshakeState{
		Tokens: handshake.NewStore(),
	})
	srv := httptest.NewServer(NewRouter(ps, "0.1.3"))
	t.Cleanup(srv.Close)
	return srv, st
}

// seedTaskWithState 用**唯一入库入口**建一条任务，再把它推到指定内部状态。
//
// 为什么不用裸 INSERT：`tasks.CreateTask` 是项目里唯一被允许建任务的地方，
// 测试绕过它就等于在验一份生产上不存在的行。推到目标状态则只能 UPDATE ——
// 生产上这一步由 `internal/sync` 的回读写入，两个列（internal_state / aria_status）
// 是**一起**写的，所以这里也一起写：只改一列会造出生产上不可能出现的组合，
// 断言就变成了验一个假场景。
func seedTaskWithState(t *testing.T, st *store.Store, want domain.InternalState) string {
	t.Helper()
	created, err := tasks.CreateTask(t.Context(), st, tasks.TaskCreateInput{
		Kind:   domain.KindHTTP,
		URLRaw: "http://example.test/aria-stopped-" + string(want) + ".iso",
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	aria := domain.DeriveAriaStatus(want, 1000, nil)
	var ec any
	if aria == domain.AriaError {
		ec = string(domain.ErrOutOfRetry)
	}
	if _, err := st.Execute(t.Context(),
		"UPDATE task SET internal_state = ?1, aria_status = ?2, error_class = ?3 WHERE task_id = ?4",
		string(want), string(aria), ec, created.TaskID); err != nil {
		t.Fatalf("把任务推到 %s 失败: %v", want, err)
	}
	return created.TaskID
}
