package admin

// 告警/审计查询：`GET /api/admin/events`。
//
// # 为什么必须有这个接口
//
// `event_log` 从 T03 起就在写（节点探测失败、投递失败、去重命中、组拆解…），
// 但一直没有读的入口 —— 于是那些"已经记下来了"的事实只有 `sqlite3` 能看见。
// 用户报"任务卡住了"时，运维的第一反应是"有没有告警"，而当时我们只能回答
// "库里有，你自己查"。这不是数据缺失，是**入口缺失**。
//
// 设计约束：
//   - 只读接口（GET），与其它读接口同一套 `guardRead`（会话 + 口令守卫）；
//   - 分页用 `before`（id 游标）而不是 offset：`event_log` 是持续追加的，
//     offset 分页在翻页时会重复/漏行；
//   - `limit` 上限 500 —— 一次拉一万行只会让浏览器卡死，不会让人看得更清楚。

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/session"
)

// eventLevels / eventCategories 是 `event_log` 的 CHECK 取值集（DDL 同源）。
//
// 校验放在这里而不是靠数据库报错：非法值应该得到 422 与一句能读懂的话，
// 而不是 500 + SQLite 的约束消息。
var eventLevels = map[string]bool{"info": true, "warn": true, "error": true}

var eventCategories = map[string]bool{
	"node": true, "task": true, "dedup": true,
	"group": true, "system": true, "security": true,
}

// eventsList 实现 `GET /api/admin/events`。
//
// 查询参数（全部可选）：
//
//	level    ∈ info/warn/error
//	category ∈ node/task/dedup/group/system/security
//	task_id  精确匹配
//	before   只返回 id < before 的行（游标分页）
//	limit    默认 50，上限 500
func (s *adminServer) eventsList(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardRead(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}

	q := r.URL.Query()
	level := strings.TrimSpace(q.Get("level"))
	category := strings.TrimSpace(q.Get("category"))
	taskID := strings.TrimSpace(q.Get("task_id"))

	if level != "" && !eventLevels[level] {
		fail(w, http.StatusUnprocessableEntity, CodeValidationError,
			"level 只能是 info / warn / error")
		return
	}
	if category != "" && !eventCategories[category] {
		fail(w, http.StatusUnprocessableEntity, CodeValidationError,
			"category 只能是 node / task / dedup / group / system / security")
		return
	}

	limit := int64(50)
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			fail(w, http.StatusUnprocessableEntity, CodeValidationError, "limit 必须是正整数")
			return
		}
		if n > 500 {
			n = 500
		}
		limit = n
	}

	// 条件拼接只拼**列名与占位符**，值一律走参数绑定 —— 用户输入永远不进 SQL 文本。
	where := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if level != "" {
		where = append(where, "level = ?")
		args = append(args, level)
	}
	if category != "" {
		where = append(where, "category = ?")
		args = append(args, category)
	}
	if taskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, taskID)
	}
	if v := strings.TrimSpace(q.Get("before")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			fail(w, http.StatusUnprocessableEntity, CodeValidationError, "before 必须是正整数（id 游标）")
			return
		}
		where = append(where, "id < ?")
		args = append(args, n)
	}

	sqlText := `SELECT id, level, category, node_id, task_id, message,
	                   COALESCE(detail, ''), created_at
	              FROM event_log`
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.st.Store.Query(r.Context(), sqlText, args...)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}

	items := make([]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"id":         asInt64Col(row[0]),
			"level":      asStringCol(row[1]),
			"category":   asStringCol(row[2]),
			"node_id":    nullableInt(row[3]),
			"task_id":    nullableStr(row[4]),
			"message":    asStringCol(row[5]),
			"detail":     asStringCol(row[6]),
			"created_at": asInt64Col(row[7]),
		})
	}

	ok(w, map[string]any{
		"items": items,
		// `next_before` 给前端做"加载更早"：没有更多行时返回 0，前端据此停手。
		"next_before": nextBefore(rows),
	})
	s.renewCookie(w, r, exp)
}

// nextBefore 从结果集里算出下一页的游标（最后一行 id）。
func nextBefore(rows [][]any) int64 {
	if len(rows) == 0 {
		return 0
	}
	return asInt64Col(rows[len(rows)-1][0])
}

// asInt64Col / asStringCol / nullableInt / nullableStr 是列值转换（驱动可能给 []byte）。
func asInt64Col(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case []byte:
		n, _ := strconv.ParseInt(string(x), 10, 64)
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	default:
		return 0
	}
}

func asStringCol(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return ""
	}
}

func nullableInt(v any) any {
	if v == nil {
		return nil
	}
	return asInt64Col(v)
}

func nullableStr(v any) any {
	if v == nil {
		return nil
	}
	s := asStringCol(v)
	if s == "" {
		return nil
	}
	return s
}
