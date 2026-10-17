// BitComet 兼容面：8 条复刻路径 + 统一信封 + 可选 token。
//
// 三个入库入口（`http/add`、`bt/add`、`torrent_links/add`）**全部**走
// `tasks.CreateTask`，禁止另写 INSERT —— 这是「单写入口径」的承重约束。
//
// 鉴权统一调用 `Authorize`（判定表只有一份实现，见 envelope.go）。
package ingress

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
	"github.com/LIUBANGJIAN/download-gateway/internal/tasks"
)

// denyIfUnauthorized 是**统一前置**：鉴权失败回 401 `INVALID_TOKEN` 并写安全审计。
//
// 返回 true 表示已写出响应，调用方应立即 return。
func (s *server) denyIfUnauthorized(w http.ResponseWriter, r *http.Request, route string) bool {
	if Authorize(s.st, r.Header) != AuthzDeny {
		return false
	}
	hadBearer := BearerOf(r.Header) != nil
	slog.Warn("invalid_bearer", "route", route, "had_bearer", hadBearer, "reason", "mismatch")
	s.writeSecurityLog(r, "invalid_bearer", map[string]any{
		"route":      route,
		"had_bearer": hadBearer,
		"reason":     "mismatch",
	})
	status, body := BitcometErr(s.version, http.StatusUnauthorized, ECInvalidToken, "Invalid token")
	writeJSON(w, status, body)
	return true
}

// writeSecurityLog 把安全事件落 `event_log`（失败只告警，不影响受理结果）。
//
// 默认级别 `warn`：调用它的大多是「鉴权失败」这类需要留痕的拒服事件。
func (s *server) writeSecurityLog(r *http.Request, message string, detail map[string]any) {
	s.writeSecurityLogLevel(r, "warn", message, detail)
}

// writeSecurityLogLevel 与 [writeSecurityLog] 相同，但可指定级别。
//
// 握手成功路径需要 `info`（`handshake_login_ok` / `device_token_issued`）：
// 把成功也记成 `warn` 会让「翻 warn 日志」这件事失去意义 —— 那本来是排障时的第一入口。
func (s *server) writeSecurityLogLevel(r *http.Request, level, message string, detail map[string]any) {
	payload := detailJSON(detail)
	_, err := s.st.Store.Execute(r.Context(),
		"INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at) "+
			"VALUES (?1, 'security', NULL, NULL, ?2, ?3, ?4)",
		level, message, payload, ids.NowSecs())
	if err != nil {
		slog.Warn("写安全审计失败", "err", err)
	}
}

// postBody 读 body 并解析为 dst；失败时直接回 400 `INVALID_REQUEST` 信封。
func (s *server) postBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, ok := readBody(w, r)
	if !ok {
		status, resp := BitcometErr(s.version, http.StatusRequestEntityTooLarge, ECInvalidRequest, "请求体过大")
		writeJSON(w, status, resp)
		return false
	}
	if err := decodeJSON(body, dst); err != nil {
		status, resp := BitcometErr(s.version, http.StatusBadRequest, ECInvalidRequest, err.Error())
		writeJSON(w, status, resp)
		return false
	}
	return true
}

// httpAddReq 是 `POST /api/task/http/add` 请求体。
type httpAddReq struct {
	URL                string  `json:"url"`
	SaveFolder         *string `json:"save_folder"`
	Filename           *string `json:"filename"`
	StartLater         *bool   `json:"start_later"`
	MaxConnectionCount *int64  `json:"max_connection_count"`
}

// btAddReq 是 `POST /api/task/bt/add` 请求体。
type btAddReq struct {
	TorrentURL  *string `json:"torrent_url"`
	TorrentFile *string `json:"torrent_file"`
	SaveFolder  *string `json:"save_folder"`
	StartLater  *bool   `json:"start_later"`
}

// torrentLinksReq 是 `POST /api/task/torrent_links/add` 请求体。
type torrentLinksReq struct {
	TorrentLinks string  `json:"torrent_links"`
	SaveFolder   *string `json:"save_folder"`
	StartLater   *bool   `json:"start_later"`
}

// taskListGetReq 是 `POST /api_v2/task_list/get` 请求体。
type taskListGetReq struct {
	StateGroup *string `json:"state_group"`
	TaskType   *string `json:"task_type"`
	Keyword    *string `json:"keyword"`
	Start      *int64  `json:"start"`
	Limit      *int64  `json:"limit"`
}

// tasksActionReq 是 `POST /api_v2/tasks/action` 请求体。
type tasksActionReq struct {
	TaskIDs []string `json:"task_ids"`
	TaskID  *string  `json:"task_id"`
	Action  string   `json:"action"`
}

// tasksDeleteReq 是 `POST /api_v2/tasks/delete` 请求体。
type tasksDeleteReq struct {
	TaskIDs []string `json:"task_ids"`
	TaskID  *string  `json:"task_id"`
	Action  string   `json:"action"`
}

func (s *server) httpAdd(w http.ResponseWriter, r *http.Request) {
	if s.denyIfUnauthorized(w, r, "/api/task/http/add") {
		return
	}
	var req httpAddReq
	if !s.postBody(w, r, &req) {
		return
	}
	created, err := tasks.CreateTask(r.Context(), s.st.Store, tasks.TaskCreateInput{
		Kind:               domain.KindHTTP,
		URLRaw:             req.URL,
		Filename:           req.Filename,
		MaxConnectionCount: req.MaxConnectionCount,
		StartLater:         req.StartLater != nil && *req.StartLater,
		SaveFolderHint:     req.SaveFolder,
		Origin:             domain.OriginBitCometHTTP,
	})
	if err != nil {
		s.validationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BitcometOK(s.version, map[string]any{
		"proxy_task_id": created.TaskID,
		"gid":           created.GID,
	}))
}

func (s *server) btAdd(w http.ResponseWriter, r *http.Request) {
	if s.denyIfUnauthorized(w, r, "/api/task/bt/add") {
		return
	}
	var req btAddReq
	if !s.postBody(w, r, &req) {
		return
	}
	target := ""
	if req.TorrentURL != nil && *req.TorrentURL != "" {
		target = *req.TorrentURL
	} else if req.TorrentFile != nil && *req.TorrentFile != "" {
		target = *req.TorrentFile
	}
	if target == "" {
		status, body := BitcometErr(s.version, http.StatusBadRequest, ECInvalidRequest, "缺少 torrent_url / torrent_file")
		writeJSON(w, status, body)
		return
	}
	created, err := tasks.CreateTask(r.Context(), s.st.Store, tasks.TaskCreateInput{
		Kind:           domain.KindBT,
		URLRaw:         target,
		StartLater:     req.StartLater != nil && *req.StartLater,
		SaveFolderHint: req.SaveFolder,
		Origin:         domain.OriginBitCometBT,
	})
	if err != nil {
		s.validationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BitcometOK(s.version, map[string]any{
		"proxy_task_id": created.TaskID,
		"gid":           created.GID,
	}))
}

func (s *server) torrentLinksAdd(w http.ResponseWriter, r *http.Request) {
	if s.denyIfUnauthorized(w, r, "/api/task/torrent_links/add") {
		return
	}
	var req torrentLinksReq
	if !s.postBody(w, r, &req) {
		return
	}
	links := protocol.SplitAddLinks(req.TorrentLinks)
	if len(links) == 0 {
		status, body := BitcometErr(s.version, http.StatusBadRequest, ECInvalidRequest, "torrent_links 为空")
		writeJSON(w, status, body)
		return
	}
	startLater := req.StartLater != nil && *req.StartLater
	accepted := make([]any, 0, len(links))
	for _, link := range links {
		kind := domain.KindHTTP
		if protocol.ClassifyAddTarget(link) == protocol.AddKindTorrent {
			if strings.HasPrefix(strings.ToLower(strings.TrimLeft(link, " \t\r\n")), "magnet:") {
				kind = domain.KindMagnet
			} else {
				kind = domain.KindTorrent
			}
		}
		created, err := tasks.CreateTask(r.Context(), s.st.Store, tasks.TaskCreateInput{
			Kind:           kind,
			URLRaw:         link,
			StartLater:     startLater,
			SaveFolderHint: req.SaveFolder,
			Origin:         domain.OriginBitCometTorrent,
		})
		if err != nil {
			accepted = append(accepted, map[string]any{"url": link, "error": err.Error()})
			continue
		}
		accepted = append(accepted, map[string]any{
			"url":           link,
			"proxy_task_id": created.TaskID,
			"gid":           created.GID,
		})
	}
	writeJSON(w, http.StatusOK, BitcometOK(s.version, map[string]any{"accepted": accepted}))
}

func (s *server) taskListGet(w http.ResponseWriter, r *http.Request) {
	if s.denyIfUnauthorized(w, r, "/api_v2/task_list/get") {
		return
	}
	var req taskListGetReq
	if !s.postBody(w, r, &req) {
		return
	}
	size := int64(tasks.PageDefault)
	if req.Limit != nil {
		size = *req.Limit
	}
	if size < 1 {
		size = 1
	}
	if size > 500 {
		size = 500
	}
	q := tasks.DefaultTaskListQuery()
	q.Size = size
	if req.Keyword != nil && *req.Keyword != "" {
		kw := *req.Keyword
		q.Q = &kw
	}
	page, err := tasks.ListTasks(r.Context(), s.st.Store, q)
	if err != nil {
		status, body := BitcometErr(s.version, http.StatusInternalServerError, "INTERNAL", err.Error())
		writeJSON(w, status, body)
		return
	}
	items := make([]any, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, bitcometTaskObj(&page.Items[i]))
	}
	// 分页偏移：本轮仅首页，按 start 裁剪（与 Rust 实现一致）。
	if req.Start != nil && *req.Start > 0 {
		start := int(*req.Start)
		if start >= len(items) {
			items = []any{}
		} else {
			items = items[start:]
		}
	}
	writeJSON(w, http.StatusOK, BitcometTaskList(s.version, page.Total, items))
}

func (s *server) tasksAction(w http.ResponseWriter, r *http.Request) {
	if s.denyIfUnauthorized(w, r, "/api_v2/tasks/action") {
		return
	}
	var req tasksActionReq
	if !s.postBody(w, r, &req) {
		return
	}
	idsList := collectIDs(req.TaskIDs, req.TaskID)
	var internal, aria string
	var errClass *string
	switch req.Action {
	case "pause":
		internal, aria = "paused", "paused"
		v := "not_started"
		errClass = &v
	case "unpause", "resume", "retry":
		internal, aria = "queued", "waiting"
	default:
		status, body := BitcometErr(s.version, http.StatusBadRequest, ECInvalidRequest, "不支持的动作: "+req.Action)
		writeJSON(w, status, body)
		return
	}
	done := 0
	for _, id := range idsList {
		row, err := tasks.GetByID(r.Context(), s.st.Store, id)
		if err != nil || row == nil {
			continue
		}
		if _, err := tasks.SetState(r.Context(), s.st.Store, id, internal, aria, errClass); err == nil {
			done++
		}
	}
	writeJSON(w, http.StatusOK, BitcometOK(s.version, map[string]any{"affected": done}))
}

func (s *server) tasksDelete(w http.ResponseWriter, r *http.Request) {
	if s.denyIfUnauthorized(w, r, "/api_v2/tasks/delete") {
		return
	}
	var req tasksDeleteReq
	if !s.postBody(w, r, &req) {
		return
	}
	if req.Action == "delete_all" {
		status, body := BitcometErr(s.version, http.StatusBadRequest, ECInvalidRequest, "delete_all 本轮不支持（无节点文件）")
		writeJSON(w, status, body)
		return
	}
	idsList := collectIDs(req.TaskIDs, req.TaskID)
	var total int64
	for _, id := range idsList {
		if n, err := tasks.DeleteTask(r.Context(), s.st.Store, id); err == nil && n > 0 {
			total += n
		}
	}
	writeJSON(w, http.StatusOK, BitcometOK(s.version, map[string]any{"deleted": total}))
}

func (s *server) aboutGet(w http.ResponseWriter, r *http.Request) {
	if s.denyIfUnauthorized(w, r, "/api/config/about/get") {
		return
	}
	writeJSON(w, http.StatusOK, BitcometOK(s.version, map[string]any{
		"about_info": map[string]any{
			"version":  s.version,
			"platform": Platform,
			"proxy":    true,
			"note":     "仅受理任务，调度/下发未启用",
		},
	}))
}

func (s *server) summaryGet(w http.ResponseWriter, r *http.Request) {
	if s.denyIfUnauthorized(w, r, "/api/task/summary/get") {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		status, resp := BitcometErr(s.version, http.StatusRequestEntityTooLarge, ECInvalidRequest, "请求体过大")
		writeJSON(w, status, resp)
		return
	}
	var req struct {
		TaskID *string `json:"task_id"`
	}
	_ = decodeJSON(body, &req)
	addedTime := int64(0)
	name := ""
	if req.TaskID != nil {
		if row, err := tasks.GetByID(r.Context(), s.st.Store, *req.TaskID); err == nil && row != nil {
			addedTime = row.CreatedAt
			name = derefString(row.Name)
		}
	}
	writeJSON(w, http.StatusOK, BitcometOK(s.version, map[string]any{
		"referrer":    "",
		"save_folder": "",
		"added_time":  addedTime,
		"name":        name,
	}))
}

// ipVerify 是自签三段式握手的第 1 段：**不需要凭据**。
//
// 恒返回 `bypass_eligible:false` —— 我们从不实现免密码登录，
// 真实客户端据此会走第 2 段（`/api/webui/login`）的正常口令路径。
// 第 2/3 段见 webui_login.go。
func (s *server) ipVerify(w http.ResponseWriter, r *http.Request) {
	if body, ok := readBody(w, r); !ok || len(body) > 64*1024 {
		status, resp := BitcometErr(s.version, http.StatusRequestEntityTooLarge, ECInvalidRequest, "请求体过大")
		writeJSON(w, status, resp)
		return
	}
	writeJSON(w, http.StatusOK, BitcometOK(s.version, map[string]any{"bypass_eligible": false}))
}

// validationErr 把入库错误渲染为 422（校验类）或 500（存储类）。
func (s *server) validationErr(w http.ResponseWriter, err error) {
	status, body := BitcometErr(s.version, http.StatusUnprocessableEntity, ECInvalidRequest, err.Error())
	writeJSON(w, status, body)
}

// collectIDs 合并 `task_ids` 与单个 `task_id`（去重，保持顺序）。
func collectIDs(list []string, single *string) []string {
	out := make([]string, 0, len(list)+1)
	seen := map[string]struct{}{}
	for _, id := range list {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if single != nil {
		if _, ok := seen[*single]; !ok {
			out = append(out, *single)
		}
	}
	return out
}

// bitcometTaskObj 是 BitComet 侧任务对象（代理字段 + 扩展 `proxy_task_id`）。
func bitcometTaskObj(t *domain.TaskRow) map[string]any {
	name := derefString(t.Name)
	if name == "" {
		name = t.URLRaw
	}
	var total int64
	if t.TotalSize != nil {
		total = *t.TotalSize
	}
	return map[string]any{
		"task_id":         t.TaskID,
		"proxy_task_id":   t.TaskID,
		"gid":             derefString(t.GID),
		"name":            name,
		"url":             t.URLRaw,
		"type":            string(t.Kind),
		"state":           string(t.InternalState),
		"aria_status":     t.AriaStatus,
		"total_size":      total,
		"downloaded_size": t.DownloadedSize,
		"permillage":      t.Permillage,
		"created_at":      t.CreatedAt,
	}
}

// detailJSON 把审计明细序列化为字符串（`event_log.detail` 是 TEXT）。
// 序列化失败回退 `{}` —— 审计失败绝不阻断业务。
func detailJSON(detail map[string]any) string {
	b, err := json.Marshal(detail)
	if err != nil {
		return "{}"
	}
	return string(b)
}
