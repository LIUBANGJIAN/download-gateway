// 本文件是对外口（:6800）的**路由装配**：把 `internal/tasks` 真正接到 HTTP 上。
//
// # 为什么需要它（接线背景）
//
// `internal/tasks` 此前是「未接线的死代码」：12 个导出函数写好了、有测试，
// 但全仓零 import —— 任务永远进不了库。本包补上入口，让业务链路真正跑通：
//
//	aria2.addUri / BitComet http|bt|torrent_links add  →  tasks.CreateTask  →  SQLite
//	aria2.tellStatus / task_list_get                   ←  tasks.Query       ←  SQLite
//	aria2.remove / tasks_action|delete                 →  tasks.SetState / DeleteTask
//
// # 契约来源
//
// 逐条对齐 `crates/dispatch-core/src/ingress/{aria2,bitcomet}.rs`：信封、错误码、
// 「未实现方法返 -32601 而不是 404」、「管理台不得产生任务」等承重行为全部保留。
package ingress

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/state"
)

// PublicBodyLimit 是对外口请求体上限（8 MiB，对齐 Rust `ingress/mod.rs`）。
const PublicBodyLimit int64 = 8 << 20

// server 持有对外口的 handler 依赖。用值类型 + 方法集合，避免全局变量。
type server struct {
	st      *state.PublicState
	version string
}

// NewRouter 装配对外口全部路由，并套上 CORS 与体积上限中间件。
//
// 版本号由 `main` 通过 ldflags 注入后传入：`/healthz` 自报的 version 必须与
// 镜像 OCI 标签一致，否则冒烟会红（这是证明「版本号真的进了二进制」的唯一手段）。
func NewRouter(st *state.PublicState, version string) http.Handler {
	s := &server{st: st, version: version}

	mux := http.NewServeMux()
	// 健康检查（两口各一，契约逐字节一致）。
	mux.HandleFunc("GET /healthz", s.healthz)
	// aria2 原生协议面。
	mux.HandleFunc("POST /jsonrpc", s.jsonrpc)
	// BitComet 原生协议面（8 条复刻路径）。
	mux.HandleFunc("POST /api/task/http/add", s.httpAdd)
	mux.HandleFunc("POST /api/task/bt/add", s.btAdd)
	mux.HandleFunc("POST /api/task/torrent_links/add", s.torrentLinksAdd)
	mux.HandleFunc("POST /api_v2/task_list/get", s.taskListGet)
	mux.HandleFunc("POST /api_v2/tasks/action", s.tasksAction)
	mux.HandleFunc("POST /api_v2/tasks/delete", s.tasksDelete)
	mux.HandleFunc("POST /api/config/about/get", s.aboutGet)
	mux.HandleFunc("POST /api/task/summary/get", s.summaryGet)
	// 三段式握手：第 1 段**不需要凭据**，第 2/3 段自校验（第 2 段还要按 IP 限速）。
	// 这三条是真实 BitComet 客户端探测阶段就会打的入口，缺一条真客户端就走不到
	// `/api/task/http/add`。详见 webui_login.go 的包注释。
	mux.HandleFunc("POST /api/webui/ip_verify", s.ipVerify)
	mux.HandleFunc("POST /api/webui/login", s.webuiLogin)
	mux.HandleFunc("POST /api/device_token/get", s.deviceTokenGet)

	// ⚠️ 顺序：先 CORS（要能应答预检），再体积上限（预检无 body，不受影响）。
	var h http.Handler = mux
	h = limitBody(h, PublicBodyLimit)
	if st.CORSEnabled {
		h = corsAllowAll(h)
	}
	return h
}

// healthz 实现 `GET /healthz`：**真查一次库**（`SELECT 1`），库不通回 503。
//
// 对齐 Rust `health::health_body`：字段名与语义一致；`port` 区分两个监听口。
func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	dbOK := true
	if _, err := s.st.Store.Query(r.Context(), "SELECT 1"); err != nil {
		dbOK = false
	}
	status := http.StatusOK
	body := map[string]any{
		"status":         "ok",
		"port":           "public",
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

// corsAllowAll 只在 `DISPATCH_PUBLIC_CORS` 开启时挂上（关闭时**不得**加任何头）。
func corsAllowAll(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody 把读 body 的上限压到 n 字节（超限时 `ReadAll` 返错，由各 handler 统一回信封）。
func limitBody(next http.Handler, n int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, n)
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON 写 JSON 响应。`encoding/json` 对 map 键按字典序输出，与 Rust
// `serde_json`（默认 BTreeMap）口径一致，便于逐字比对。
func writeJSON(w http.ResponseWriter, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		slog.Error("序列化响应失败", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// readBody 读全请求体；失败（含超限）返回 false，由调用方回统一的「请求体非法」信封。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		// `http.MaxBytesReader` 超限时也走这里。
		return nil, false
	}
	return buf, true
}

// decodeJSON 解析请求体；空体按 `{}` 处理（多个 BitComet 路由的 body 可为空）。
func decodeJSON(body []byte, dst any) error {
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("请求体非法: %w", err)
	}
	return nil
}
