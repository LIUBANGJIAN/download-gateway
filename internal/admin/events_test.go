package admin

// `GET /api/admin/events` 的端到端用例：真库、真 handler、真会话。
//
// 这个接口存在的意义是"把已经记下来的告警变得可查"，所以测试重点不在字段齐全，
// 而在三条容易退化的性质：
//  1. 未登录不可读（否则等于把审计日志敞开给内网任何人）；
//  2. 非法过滤值给出 422 而不是 500（SQL 约束消息不该泄漏给用户）；
//  3. 过滤与游标分页真的生效（翻页不重复、不丢行）。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// insertEvent 直接写一行 `event_log`（不经过 engine，隔离被测对象）。
func insertEvent(t *testing.T, st *store.Store, level, category, message string, id int64) {
	t.Helper()
	if _, err := st.Execute(context.Background(),
		`INSERT INTO event_log (id, level, category, node_id, task_id, message, detail, created_at)
		 VALUES (?1, ?2, ?3, NULL, NULL, ?4, NULL, ?5)`,
		id, level, category, message, 1000+id,
	); err != nil {
		t.Fatalf("写事件失败: %v", err)
	}
}

// TestEventsListGates 守"未登录不可读"。
func TestEventsListGates(t *testing.T) {
	srv, client, _ := newAdminEnv(t)
	status, body := doReq(t, client, http.MethodGet, srv.URL+"/api/admin/events", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("未登录读告警应 401，实际 %d %s", status, body)
	}
}

// TestEventsListFilterAndPaging 守过滤、游标分页与 422 校验。
func TestEventsListFilterAndPaging(t *testing.T) {
	srv, client, st := newAdminEnv(t)
	if status, body := doReq(t, client, http.MethodPost, srv.URL+"/api/admin/login",
		`{"password":"`+testAdminPassword+`"}`); status != http.StatusOK {
		t.Fatalf("登录失败: %d %s", status, body)
	}

	// 3 条 warn/group + 2 条 info/task
	for i := int64(1); i <= 3; i++ {
		insertEvent(t, st, "warn", "group", "组已拆组", i)
	}
	for i := int64(4); i <= 5; i++ {
		insertEvent(t, st, "info", "task", "已投递", i)
	}

	// ── 无过滤：拿到全部 5 条，按 id 降序 ────────────────────────────────
	status, body := doReq(t, client, http.MethodGet, srv.URL+"/api/admin/events", "")
	if status != http.StatusOK {
		t.Fatalf("读告警失败: %d %s", status, body)
	}
	var resp struct {
		Data struct {
			Items []struct {
				ID       int64  `json:"id"`
				Level    string `json:"level"`
				Category string `json:"category"`
				Message  string `json:"message"`
			} `json:"items"`
			NextBefore int64 `json:"next_before"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析响应失败: %v（%s）", err, body)
	}
	if len(resp.Data.Items) != 5 {
		t.Fatalf("应有 5 条，实际 %d", len(resp.Data.Items))
	}
	if resp.Data.Items[0].ID != 5 {
		t.Fatalf("应按 id 降序，首条 id=%d", resp.Data.Items[0].ID)
	}
	if resp.Data.NextBefore != 1 {
		t.Fatalf("next_before 应为最后一行的 id=1，实际 %d", resp.Data.NextBefore)
	}

	// ── 按 level 过滤 ────────────────────────────────────────────────────
	_, body = doReq(t, client, http.MethodGet, srv.URL+"/api/admin/events?level=warn", "")
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Data.Items) != 3 {
		t.Fatalf("warn 应有 3 条，实际 %d", len(resp.Data.Items))
	}
	for _, it := range resp.Data.Items {
		if it.Level != "warn" {
			t.Fatalf("过滤失效：拿到一条 %s", it.Level)
		}
	}

	// ── 游标分页：limit=2 连翻两次，不重复不丢 ───────────────────────────
	seen := map[int64]bool{}
	before := int64(0)
	for page := 0; page < 3; page++ {
		u := srv.URL + "/api/admin/events?limit=2"
		if before > 0 {
			u += "&before=" + jsonNum(before)
		}
		_, body = doReq(t, client, http.MethodGet, u, "")
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		for _, it := range resp.Data.Items {
			if seen[it.ID] {
				t.Fatalf("分页重复了 id=%d", it.ID)
			}
			seen[it.ID] = true
		}
		if resp.Data.NextBefore == 0 {
			break
		}
		before = resp.Data.NextBefore
	}
	if len(seen) != 5 {
		t.Fatalf("翻页应覆盖全部 5 条，实际 %d 条", len(seen))
	}

	// ── 非法过滤值必须是 422（而不是 500 + SQL 约束消息）─────────────────
	for _, bad := range []string{
		"/api/admin/events?level=fatal",
		"/api/admin/events?category=whatever",
		"/api/admin/events?limit=abc",
		"/api/admin/events?limit=0",
		"/api/admin/events?before=-3",
	} {
		status, body = doReq(t, client, http.MethodGet, srv.URL+bad, "")
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("%s 应 422，实际 %d %s", bad, status, body)
		}
		if !contains(body, "42200") {
			t.Fatalf("%s 应回 42200 信封，实际 %s", bad, body)
		}
	}

	// ── limit 超上限被夹到 500（不报错，避免一次拉爆浏览器）───────────────
	status, body = doReq(t, client, http.MethodGet, srv.URL+"/api/admin/events?limit=99999", "")
	if status != http.StatusOK {
		t.Fatalf("limit 超上限应被夹取而不是报错，实际 %d %s", status, body)
	}
}

// jsonNum 把整数转成 URL 参数（避免为一行引入 strconv）。
func jsonNum(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// contains 是 strings.Contains 的薄封装（避免为了一个调用加 import）。
func contains(b []byte, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return true
		}
	}
	return false
}
