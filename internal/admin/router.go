// 管理口（:8080）：会话 + REST + 内嵌零构建 Web 管理台。
//
// # 职责边界（承重墙，勿破）
//
// **管理台只负责「看」和「管」，永不产生任务。** 任务提交的唯一入口是面向外部的
// 协议面（:6800）。因此这里**刻意没有** `POST /api/admin/tasks`，也没有 `/tasks/new`
// 页面：前者落到 405、后者落到 404 —— 两者都是「不该能用」的可观测证据。
//
// # 与 Rust 的一致性
//
// 路由表、错误码、`{code,data,message}` 信封、CSRF/会话语义均对齐
// `crates/dispatch-core/src/admin/mod.rs`。
package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/LIUBANGJIAN/download-gateway/internal/admin/web"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/state"
)

// AdminBodyLimit 是管理口请求体上限（1 MiB，比对外口更严）。
const AdminBodyLimit int64 = 1024 * 1024

// 统一错误码（逐字不变）。
const (
	// CodeUnauthenticated 未认证。
	CodeUnauthenticated int64 = 40100
	// CodeCSRFInvalid CSRF 校验失败。
	CodeCSRFInvalid int64 = 40301
	// CodeNotFound 资源不存在。
	CodeNotFound int64 = 40400
	// CodePreconditionFailed 前置条件失败。
	CodePreconditionFailed int64 = 40900
	// CodeValidationError 校验错误。
	CodeValidationError int64 = 42200
	// CodeTooManyRequests 请求过于频繁。
	CodeTooManyRequests int64 = 42900
	// CodeInternal 服务器内部错误。
	CodeInternal int64 = 50000
)

// adminServer 持有管理口 handler 依赖。
type adminServer struct {
	st      *state.AdminState
	version string
}

// NewRouter 装配管理口全部路由。
func NewRouter(st *state.AdminState, version string) http.Handler {
	s := &adminServer{st: st, version: version}

	mux := http.NewServeMux()
	// ---- 健康检查 ----
	mux.HandleFunc("GET /healthz", s.healthz)
	// ---- Web 壳（SPA 路由都回同一页面）----
	// `{$}` 精确匹配根路径：若写成 `/`，它会变成 catch-all，
	// `/tasks/new` 就会被当成页面回 200 —— 那正好掩盖了「添加任务页已删除」这条契约。
	mux.HandleFunc("GET /{$}", s.page(web.Index))
	mux.HandleFunc("GET /login", s.page(web.Index))
	mux.HandleFunc("GET /tasks", s.page(web.Index))
	mux.HandleFunc("GET /nodes", s.page(web.Index))
	mux.HandleFunc("GET /settings", s.page(web.Index))
	mux.HandleFunc("GET /app.css", s.asset(web.CSS, web.ContentTypeCSS))
	mux.HandleFunc("GET /app.js", s.asset(web.JS, web.ContentTypeJS))
	// ---- 会话 ----
	mux.HandleFunc("POST /api/admin/login", s.login)
	mux.HandleFunc("POST /api/admin/logout", s.logout)
	// ---- 管理口令（用户可自行设置）----
	// GET 只回「来源 + 上次修改时刻」，绝不回口令本身；
	// POST 要求已登录 + CSRF + **旧口令正确**，改完立即生效。
	mux.HandleFunc("GET /api/admin/password", s.passwordStatus)
	mux.HandleFunc("POST /api/admin/password", s.changePassword)
	// ---- 自助找回（**匿名**，登录页的「忘记口令」按钮）----
	// 生成一次性临时口令，只写服务端日志、绝不回传；
	// 凭「能读日志 = 能控制容器」证明身份，见 selfreset.go 的说明。
	mux.HandleFunc("POST /api/admin/password/reset-request", s.requestTempPassword)
	// ---- 总览 ----
	mux.HandleFunc("GET /api/admin/summary", s.summary)
	// ---- 任务：只读 + 干预，**没有创建** ----
	mux.HandleFunc("GET /api/admin/tasks", s.tasksList)
	mux.HandleFunc("POST /api/admin/tasks/{id}/{action}", s.taskAction)
	// ---- 节点管理（列表 + 增删改 + 启停 + 密码回看）----
	// 列表默认实时探测（`?probe=0` 可跳过）；写操作全部要求会话 + CSRF。
	// 密码回看刻意用 POST（GET 会把口令写进 URL / 浏览器历史 / 访问日志）。
	mux.HandleFunc("GET /api/admin/nodes", s.nodesList)
	mux.HandleFunc("POST /api/admin/nodes", s.nodesCreate)
	mux.HandleFunc("PUT /api/admin/nodes/{id}", s.nodesUpdate)
	mux.HandleFunc("DELETE /api/admin/nodes/{id}", s.nodesDelete)
	mux.HandleFunc("POST /api/admin/nodes/{id}/enabled", s.nodesSetEnabled)
	mux.HandleFunc("POST /api/admin/nodes/{id}/secret", s.nodesRevealSecret)
	// ---- 派发 / 负载策略 ----
	mux.HandleFunc("GET /api/admin/config", s.configGet)
	mux.HandleFunc("PUT /api/admin/config", s.configPut)

	return limitBody(mux, AdminBodyLimit)
}

// healthz 实现管理口 `/healthz`：同样**真查一次库**，不通回 503。
func (s *adminServer) healthz(w http.ResponseWriter, r *http.Request) {
	dbOK := true
	if _, err := s.st.Store.Query(r.Context(), "SELECT 1"); err != nil {
		dbOK = false
	}
	status := http.StatusOK
	body := map[string]any{
		"status":         "ok",
		"port":           "admin",
		"db":             true,
		"uptime_seconds": ids.NowSecs() - s.st.StartedAt,
		"version":        s.version,
	}
	if !dbOK {
		status = http.StatusServiceUnavailable
		body["status"] = "degraded"
		body["db"] = false
	}
	writeJSON(w, status, body)
}

// page 返回壳页面（优先磁盘覆盖目录，回落内嵌）。
func (s *adminServer) page(render func(*string) []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", web.ContentTypeHTML)
		_, _ = w.Write(render(s.st.WebDir))
	}
}

// asset 返回静态资源。
func (s *adminServer) asset(render func(*string) []byte, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(render(s.st.WebDir))
	}
}

// limitBody 压住读 body 的上限。
func limitBody(next http.Handler, n int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, n)
		}
		next.ServeHTTP(w, r)
	})
}

// ok 是成功信封 `{code:0,data,message:"ok"}`。
func ok(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": data, "message": "ok"})
}

// fail 是失败信封。
func fail(w http.ResponseWriter, status int, code int64, message string) {
	writeJSON(w, status, map[string]any{"code": code, "data": nil, "message": message})
}

// writeJSON 写 JSON 响应（map 键按字典序输出，与 Rust 口径一致）。
func writeJSON(w http.ResponseWriter, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		slog.Error("序列化管理口响应失败", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":50000,"data":null,"message":"内部错误"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
