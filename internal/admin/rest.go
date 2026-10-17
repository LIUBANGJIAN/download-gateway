// 管理 API handler：任务的读 / 干预 + 会话（login/logout）。
//
// 统一信封 `{code, data, message}`；写操作要求会话 + CSRF，读操作仅会话。
//
// # 这里**没有**「添加任务」
//
// 任务提交的唯一入口是对外口（:6800）。`POST /api/admin/tasks` 已删除，
// 现在落到 405；`/tasks/new` 页面也已删除，落到 404。
package admin

import (
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/session"
	"github.com/LIUBANGJIAN/download-gateway/internal/tasks"
)

// sessionMaxAge 是会话有效期（秒），同时用于 `expires_in`。
const sessionMaxAge int64 = 1800

// loginReq 是 `POST /api/admin/login` 请求体。
type loginReq struct {
	Password string `json:"password"`
}

// actionReq 是 `POST /api/admin/tasks/{id}/{action}` 请求体。
type actionReq struct {
	DeleteFiles *bool `json:"delete_files"`
}

// login 实现 `POST /api/admin/login`：先查退避，再比对口令，成功发 `sid` Cookie。
func (s *adminServer) login(w http.ResponseWriter, r *http.Request) {
	now := ids.NowSecs()
	ip := clientAddr(r)

	if until, locked := s.st.Guard.LockedUntil(ip, now); locked {
		remain := until - now
		if remain < 0 {
			remain = 0
		}
		fail(w, http.StatusTooManyRequests, CodeTooManyRequests,
			"登录尝试过多，请 "+strconv.FormatInt(remain, 10)+" 秒后重试")
		return
	}

	var req loginReq
	if err := decodeBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, CodeValidationError, err.Error())
		return
	}
	// 口令来源有两条（见 `adminpass.Credential`）：库里的 argon2id 哈希优先，
	// 没有哈希时才与环境变量/随机口令做定长定时比较。由 Credential 统一判定，
	// 登录处不关心当前是哪一路 —— 否则「改过口令但登录仍比旧明文」会静默失效。
	if !s.st.Credential.Verify(req.Password) {
		count := s.st.Guard.RecordFailure(ip, now)
		s.logSecurity(r, now, "admin_login_failed", map[string]any{"failures": count})
		fail(w, http.StatusUnauthorized, CodeUnauthenticated, "口令错误")
		return
	}

	s.st.Guard.RecordSuccess(ip)
	sid := s.st.Sessions.Create(now)
	secure := session.CookieSecureFor(s.st.Policy.CookieSecure, r.Header)
	w.Header().Set("Set-Cookie", session.SetCookieHeader(sid, sessionMaxAge, secure))
	ok(w, map[string]any{"expires_in": sessionMaxAge})
}

// logout 实现 `POST /api/admin/logout`：撤销会话并清 Cookie。
func (s *adminServer) logout(w http.ResponseWriter, r *http.Request) {
	if _, g := s.guardWrite(r); g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	if sid, found := session.CookieValue(r.Header, session.CookieName); found {
		s.st.Sessions.Revoke(sid)
	}
	secure := session.CookieSecureFor(s.st.Policy.CookieSecure, r.Header)
	w.Header().Set("Set-Cookie", session.ClearCookieHeader(secure))
	ok(w, nil)
}

// summary 实现 `GET /api/admin/summary`。
func (s *adminServer) summary(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardRead(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	counts, err := tasks.CountByInternalState(r.Context(), s.st.Store)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	var total int64
	items := make([]any, 0, len(counts))
	for _, c := range counts {
		total += c.Count
		items = append(items, map[string]any{"state": c.State, "count": c.Count})
	}

	migrations := []int64{}
	if rows, err := s.st.Store.Query(r.Context(),
		"SELECT version FROM schema_migrations ORDER BY version"); err == nil {
		for _, row := range rows {
			if len(row) > 0 {
				if v, ok := row[0].(int64); ok {
					migrations = append(migrations, v)
				}
			}
		}
	}

	env := s.st.Env
	ok(w, map[string]any{
		"version":            s.version,
		"uptime_seconds":     ids.NowSecs() - s.st.StartedAt,
		"ports":              map[string]any{"public": env.PublicAddr, "admin": env.AdminAddr},
		"db_path":            env.DBPath,
		"migration_versions": migrations,
		"task_counts":        items,
		"task_total":         total,
		"nodes":              []any{},
		"env_flags": map[string]any{
			"public_token_configured":   env.PublicTokenConfigured,
			"admin_password_configured": env.AdminPasswordConfigured,
			"allow_file_delete":         env.AllowFileDelete,
			"public_cors":               env.PublicCORS,
			"admin_cookie_secure":       env.AdminCookieSecure.String(),
		},
		"scheduler_enabled": false,
	})
	s.renewCookie(w, r, exp)
}

// tasksList 实现 `GET /api/admin/tasks`（分页参数非法必须 422，**不得**静默 clamp）。
func (s *adminServer) tasksList(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardRead(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	q := parseTaskListQuery(r)
	if q.Page < 1 {
		fail(w, http.StatusUnprocessableEntity, CodeValidationError,
			"page 必须 ≥ 1（收到 "+strconv.FormatInt(q.Page, 10)+"）")
		return
	}
	page, err := tasks.ListTasks(r.Context(), s.st.Store, q)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	items := make([]any, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, taskView(&page.Items[i]))
	}
	ok(w, map[string]any{
		"items": items,
		"page":  page.Page,
		"size":  page.Size,
		"total": page.Total,
	})
	s.renewCookie(w, r, exp)
}

// taskAction 实现 `POST /api/admin/tasks/{id}/{action}`。
func (s *adminServer) taskAction(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardWrite(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	var req actionReq
	if err := decodeBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, CodeValidationError, err.Error())
		return
	}
	s.runAction(w, r, r.PathValue("id"), r.PathValue("action"), &req)
	s.renewCookie(w, r, exp)
}

// runAction 是动作执行体（鉴权已在调用方完成）。
func (s *adminServer) runAction(w http.ResponseWriter, r *http.Request, id, action string, req *actionReq) {
	row, err := tasks.GetByID(r.Context(), s.st.Store, id)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if row == nil {
		fail(w, http.StatusNotFound, CodeNotFound, "任务不存在")
		return
	}
	ctx := r.Context()

	switch action {
	case "pause":
		notStarted := "not_started"
		if _, err := tasks.SetState(ctx, s.st.Store, row.TaskID, "paused", "paused", &notStarted); err != nil {
			fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
			return
		}
		ok(w, map[string]any{"task_id": row.TaskID, "result": "paused"})
	case "unpause":
		if _, err := tasks.SetState(ctx, s.st.Store, row.TaskID, "queued", "waiting", nil); err != nil {
			fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
			return
		}
		ok(w, map[string]any{"task_id": row.TaskID, "result": "queued"})
	case "retry":
		// ★ 承重怪癖（A 类必须保留）：只放行 failed / queued，其余一律 409。
		// 根因：A 类里 `failed` 实际不可达，但**不得"修正"** —— 见 PRD §3.4-I14。
		if row.InternalState != domain.StateFailed && row.InternalState != domain.StateQueued {
			fail(w, http.StatusConflict, CodePreconditionFailed, "仅失败/排队任务可重试")
			return
		}
		if _, err := tasks.SetState(ctx, s.st.Store, row.TaskID, "queued", "waiting", nil); err != nil {
			fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
			return
		}
		ok(w, map[string]any{"task_id": row.TaskID, "result": "retrying"})
	case "remove":
		if req.DeleteFiles != nil && *req.DeleteFiles {
			if !s.st.Policy.AllowFileDelete {
				fail(w, http.StatusUnprocessableEntity, CodeValidationError,
					"无法删除节点文件：网关未开启该能力。请在部署配置中把 DISPATCH_ALLOW_FILE_DELETE 设为 true（确认确实需要后）。")
				return
			}
			fail(w, http.StatusUnprocessableEntity, CodeValidationError,
				"无法删除节点文件：本任务在节点侧没有文件记录（调度/下发尚未落地）。若只想移除网关侧的任务记录，请不要勾选『同时删除节点上的文件』。")
			return
		}
		// 管理台的「删除」是运维动作：**硬删**整行（与 aria2 的软删语义不同，有意为之）。
		if _, err := tasks.DeleteTask(ctx, s.st.Store, row.TaskID); err != nil {
			fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
			return
		}
		ok(w, map[string]any{"task_id": row.TaskID, "result": "removed"})
	default:
		fail(w, http.StatusUnprocessableEntity, CodeValidationError, "未知动作: "+action)
	}
}

// guardRead 读守卫：只校验会话。
func (s *adminServer) guardRead(r *http.Request) (int64, session.Guard) {
	sid, found := session.CookieValue(r.Header, session.CookieName)
	if !found {
		return 0, session.GuardUnauthenticated
	}
	exp, live := s.st.Sessions.Touch(sid, ids.NowSecs())
	if !live {
		return 0, session.GuardUnauthenticated
	}
	return exp, session.GuardOK
}

// guardWrite 写守卫：会话 + 同源（CSRF）。
func (s *adminServer) guardWrite(r *http.Request) (int64, session.Guard) {
	exp, g := s.guardRead(r)
	if g != session.GuardOK {
		return 0, g
	}
	if !session.SameOrigin(r.Header) {
		return exp, session.GuardCSRFInvalid
	}
	return exp, session.GuardOK
}

// rejectGuard 把守卫结论渲染为 401 / 403。
func (s *adminServer) rejectGuard(w http.ResponseWriter, g session.Guard) {
	if g == session.GuardCSRFInvalid {
		fail(w, http.StatusForbidden, CodeCSRFInvalid, "CSRF 校验失败")
		return
	}
	fail(w, http.StatusUnauthorized, CodeUnauthenticated, "未认证")
}

// renewCookie 做会话滑动续期：活跃请求刷新 `last_seen_at` 的同时重发 Cookie。
//
// ⚠️ 每个新 handler 都必须调用它。漏掉的症状很隐蔽：接口能通、功能正常，
// 只是那个页面的会话不会随操作续期，用户静置一会儿就被踢回登录页。
func (s *adminServer) renewCookie(w http.ResponseWriter, r *http.Request, expiry int64) {
	if expiry == 0 {
		return
	}
	sid, found := session.CookieValue(r.Header, session.CookieName)
	if !found {
		return
	}
	remaining := expiry - ids.NowSecs()
	if remaining < 0 {
		remaining = 0
	}
	secure := session.CookieSecureFor(s.st.Policy.CookieSecure, r.Header)
	w.Header().Set("Set-Cookie", session.SetCookieHeader(sid, remaining, secure))
}

// logSecurity 写安全审计（失败只告警，不影响响应）。
func (s *adminServer) logSecurity(r *http.Request, now int64, message string, detail map[string]any) {
	payload, _ := json.Marshal(detail)
	_, err := s.st.Store.Execute(r.Context(),
		"INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at) "+
			"VALUES ('warn', 'security', NULL, NULL, ?1, ?2, ?3)",
		message, string(payload), now)
	if err != nil {
		// 审计失败不阻断登录流程，但必须留痕。
		_ = err
	}
}

// taskView 是管理台任务视图。
func taskView(t *domain.TaskRow) map[string]any {
	name := ""
	if t.Name != nil {
		name = *t.Name
	}
	if name == "" {
		name = t.URLRaw
	}
	var total int64
	if t.TotalSize != nil {
		total = *t.TotalSize
	}
	dispatched, dispatchState := domain.DispatchView(t.NodeID, t.InternalState)
	return map[string]any{
		"task_id":         t.TaskID,
		"gid":             derefString(t.GID),
		"name":            name,
		"url":             t.URLRaw,
		"kind":            string(t.Kind),
		"internal_state":  string(t.InternalState),
		"aria_status":     t.AriaStatus,
		"node_id":         t.NodeID,
		"total_size":      total,
		"downloaded_size": t.DownloadedSize,
		"permillage":      t.Permillage,
		"created_at":      t.CreatedAt,
		"updated_at":      t.UpdatedAt,
		"dispatched":      dispatched,
		"dispatch_state":  dispatchState,
	}
}

// parseTaskListQuery 解析列表查询参数；`page`/`size` 缺省时回落默认值，
// 但**非法值原样保留**（由调用方显式 422，绝不静默 clamp）。
func parseTaskListQuery(r *http.Request) tasks.TaskListQuery {
	q := tasks.DefaultTaskListQuery()
	v := r.URL.Query()
	if s := v.Get("page"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			q.Page = n
		} else {
			q.Page = 0 // 非法 ⇒ 让上层按「page < 1」拒绝
		}
	}
	if s := v.Get("size"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			q.Size = n
		}
	}
	if s := v.Get("q"); s != "" {
		q.Q = &s
	}
	if s := v.Get("state"); s != "" {
		q.State = &s
	}
	if s := v.Get("group"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			q.Group = &n
		}
	}
	if s := v.Get("node"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			q.Node = &n
		}
	}
	if s := v.Get("source"); s != "" {
		q.Source = &s
	}
	return q
}

// decodeBody 解析 JSON 请求体；空体视为 `{}`。
func decodeBody(r *http.Request, dst any) error {
	var raw json.RawMessage
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		if strings.Contains(err.Error(), "EOF") {
			return nil
		}
		return errJSON("请求体非法: " + err.Error())
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return errJSON("请求体非法: " + err.Error())
	}
	return nil
}

// errJSON 是一个极薄的 error 实现，避免为了一个字符串引入 fmt。
type errJSON string

func (e errJSON) Error() string { return string(e) }

// clientAddr 取来源 IP（不信任 X-Forwarded-For：与 Rust 侧 `ConnectInfo` 口径一致）。
func clientAddr(r *http.Request) netip.Addr {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr
	}
	return netip.Addr{}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
