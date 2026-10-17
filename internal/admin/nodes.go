// 管理台「节点」页：列表 / 新增 / 编辑 / 删除 / 启停 / 密码回看 + 在线探测。
//
// # 与 Rust 侧的关系
//
// 逐条对齐 `crates/dispatch-core/src/admin/nodes.rs`：同一个 `dispatch.db`，
// 两套实现必须能互相接管。校验文案、HTTP 状态、错误码、字段名都刻意保持一致，
// `smoke.sh` 第 10 段是两侧共用的验收契约。
//
// # 「启用/关闭」是**调度开关**，不是远程开关机
//
// `POST /api/admin/nodes/{id}/enabled` 只改 `node.enabled` 一个字段，
// 不触碰节点机器上跑着的任何程序。
//
// # 「在线/离线」是**探测结果**，与 `enabled` 正交
//
// `enabled` 是人工开关（要不要往它派活），探测结果是客观状态（它现在通不通）。
// 一台机器可以「已关闭但在线」，也可以「已启用但离线」——界面必须分列显示。
//
// 探测规则：只要 `base_url` 有**任何** HTTP 响应（含 401，BitComet 未认证时正是 401）
// 就算在线；连接被拒 / 超时 / DNS 失败才算离线。
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
	"github.com/LIUBANGJIAN/download-gateway/internal/session"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

const (
	// probeTimeout 是探测超时。刻意很短：这是给**界面刷新**用的，
	// 不能因为某台机器挂了就让整页卡住几十秒。
	probeTimeout = 1500 * time.Millisecond
	// aliasMax 是别名最大长度（按**字符**计，不是字节）。
	aliasMax = 64
	// maxConcurrentCap 是单机并发上限的允许上界（防手滑填天文数字）。
	maxConcurrentCap = 1024
)

// nodeRoles 是允许的节点角色（与 `node.role` 的 CHECK 约束逐字一致）。
var nodeRoles = []string{"generic", "series", "netdisk"}

// nodeRow 是库里的节点行（只取本模块用得到的列）。
type nodeRow struct {
	nodeID        int64
	alias         string
	baseURL       string
	user          string
	passEnc       string
	weight        float64
	maxConcurrent int64
	maxRateBytes  int64
	role          string
	tags          string
	enabled       bool
	state         string
	lastProbeAt   int64
	lastSeenAt    int64
	createdAt     int64
	updatedAt     int64
}

// probeResult 是一次探测的结果。
type probeResult struct {
	online    bool
	httpCode  *int
	latencyMs int64
	errMsg    string
}

// nodeView 是对外返回的节点视图。
//
// ⚠️ **永远不含** `pass_enc` / `pass` / `token`。要看密码只能走
// `POST /api/admin/nodes/{id}/secret` 这个单独的、会写审计的接口。
type nodeView struct {
	NodeID        int64   `json:"node_id"`
	Alias         string  `json:"alias"`
	BaseURL       string  `json:"base_url"`
	User          string  `json:"user"`
	PasswordSet   bool    `json:"password_set"`
	Weight        float64 `json:"weight"`
	MaxConcurrent int64   `json:"max_concurrent"`
	MaxRateBytes  int64   `json:"max_rate_bytes"`
	MaxRateKbps   int64   `json:"max_rate_kbps"`
	Role          string  `json:"role"`
	Tags          string  `json:"tags"`
	Enabled       bool    `json:"enabled"`
	State         string  `json:"state"`
	Online        bool    `json:"online"`
	HTTPCode      *int    `json:"http_code"`
	LatencyMs     *int64  `json:"latency_ms"`
	ProbeError    *string `json:"probe_error"`
	LastProbeAt   int64   `json:"last_probe_at"`
	LastSeenAt    int64   `json:"last_seen_at"`
	CreatedAt     int64   `json:"created_at"`
	UpdatedAt     int64   `json:"updated_at"`
}

// bytesToKbps 把字节/秒折算成界面口径的 KB/s（向下取整，负值当 0）。
func bytesToKbps(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return bytes / 1024
}

// kbpsToBytes 把界面口径的 KB/s 折算成存储口径的字节/秒（0 表示不限速）。
func kbpsToBytes(kbps int64) int64 {
	if kbps <= 0 {
		return 0
	}
	return kbps * 1024
}

// toView 把库行转成对外视图。`probe == nil` 时用库里的历史状态。
func (r nodeRow) toView(probe *probeResult) nodeView {
	state := r.state
	online := r.state == "online"
	v := nodeView{
		NodeID:        r.nodeID,
		Alias:         r.alias,
		BaseURL:       r.baseURL,
		User:          r.user,
		PasswordSet:   r.passEnc != "",
		Weight:        r.weight,
		MaxConcurrent: r.maxConcurrent,
		MaxRateBytes:  r.maxRateBytes,
		MaxRateKbps:   bytesToKbps(r.maxRateBytes),
		Role:          r.role,
		Tags:          r.tags,
		Enabled:       r.enabled,
		State:         state,
		Online:        online,
		LastProbeAt:   r.lastProbeAt,
		LastSeenAt:    r.lastSeenAt,
		CreatedAt:     r.createdAt,
		UpdatedAt:     r.updatedAt,
	}
	if probe != nil {
		if probe.online {
			v.State, v.Online = "online", true
		} else {
			v.State, v.Online = "offline", false
		}
		v.HTTPCode = probe.httpCode
		latency := probe.latencyMs
		v.LatencyMs = &latency
		if probe.errMsg != "" {
			msg := probe.errMsg
			v.ProbeError = &msg
		}
	}
	return v
}

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

const nodeSelectColumns = "SELECT node_id, alias, base_url, user, pass_enc, weight, " +
	"max_concurrent, max_rate_bytes, role, tags, enabled, state, last_probe_at, " +
	"last_seen_at, created_at, updated_at FROM node"

// decodeNodeRow 按 nodeSelectColumns 的列序解码一行。
func decodeNodeRow(row []any) (nodeRow, bool) {
	if len(row) < 16 {
		return nodeRow{}, false
	}
	return nodeRow{
		nodeID:        asInt64(row[0]),
		alias:         asString(row[1]),
		baseURL:       asString(row[2]),
		user:          asString(row[3]),
		passEnc:       asString(row[4]),
		weight:        asFloat(row[5]),
		maxConcurrent: asInt64(row[6]),
		maxRateBytes:  asInt64(row[7]),
		role:          asString(row[8]),
		tags:          asString(row[9]),
		enabled:       asInt64(row[10]) != 0,
		state:         asString(row[11]),
		lastProbeAt:   asInt64(row[12]),
		lastSeenAt:    asInt64(row[13]),
		createdAt:     asInt64(row[14]),
		updatedAt:     asInt64(row[15]),
	}, true
}

func loadNodeRows(ctx context.Context, st *store.Store) ([]nodeRow, error) {
	rows, err := st.Query(ctx, nodeSelectColumns+" ORDER BY node_id ASC")
	if err != nil {
		return nil, err
	}
	out := make([]nodeRow, 0, len(rows))
	for _, row := range rows {
		if r, okRow := decodeNodeRow(row); okRow {
			out = append(out, r)
		}
	}
	return out, nil
}

func loadOneNodeRow(ctx context.Context, st *store.Store, id int64) (nodeRow, bool, error) {
	rows, err := st.Query(ctx, nodeSelectColumns+" WHERE node_id = ?1", id)
	if err != nil {
		return nodeRow{}, false, err
	}
	if len(rows) == 0 {
		return nodeRow{}, false, nil
	}
	r, okRow := decodeNodeRow(rows[0])
	return r, okRow, nil
}

// nodesList 实现 `GET /api/admin/nodes`。
//
// `?probe=0` 跳过探测，只回库里的历史状态（界面首次渲染、或调试时用）。
func (s *adminServer) nodesList(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardRead(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	rows, err := loadNodeRows(r.Context(), s.st.Store)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}

	doProbe := true
	if raw := r.URL.Query().Get("probe"); raw != "" {
		if v, convErr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); convErr == nil && v == 0 {
			doProbe = false
		}
	}

	var probes map[int64]probeResult
	if doProbe && len(rows) > 0 {
		probes = probeAll(r.Context(), rows)
		// 探测结果写回失败**不该**让列表打不开：状态是锦上添花，节点清单才是主体。
		// 失败只在响应里体现为「state 仍是库里的历史值」，不阻断。
		_ = persistProbes(r.Context(), s.st.Store, rows, probes)
	}

	items := make([]nodeView, 0, len(rows))
	for _, row := range rows {
		if doProbe {
			if p, found := probes[row.nodeID]; found {
				items = append(items, row.toView(&p))
				continue
			}
		}
		items = append(items, row.toView(nil))
	}

	ok(w, map[string]any{"items": items, "total": len(items), "probed": doProbe})
	s.renewCookie(w, r, exp)
}

// ---------------------------------------------------------------------------
// 探测
// ---------------------------------------------------------------------------

// probeClient 构造探测用 HTTP 客户端。
//
// ⚠️ `Proxy: nil` 是**必须**的：节点在内网，而容器/宿主环境可能注入了
// `http_proxy`（本项目实测环境就有）。一旦被代理劫持，探测会全部失败，
// 症状是「所有节点都显示离线」——而机器其实好好的。
// 这与 `internal/protocol` 里对节点直连的约束是同一条。
func probeClient() *http.Client {
	return &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			Proxy:               nil, // 显式禁用环境代理
			MaxIdleConns:        8,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: probeTimeout,
		},
	}
}

// probeOne 探测一台节点。**任何** HTTP 响应（含 401/403）都算在线。
func probeOne(ctx context.Context, c *http.Client, baseURL string) probeResult {
	url := strings.TrimRight(baseURL, "/") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return probeResult{online: false, errMsg: err.Error()}
	}
	req.Header.Set("User-Agent", protocol.UserAgent)
	started := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		elapsed := time.Since(started).Milliseconds()
		reason := err.Error()
		switch {
		case strings.Contains(reason, "context deadline exceeded"),
			strings.Contains(reason, "Client.Timeout"):
			reason = "超时（>1500ms）"
		case strings.Contains(reason, "connection refused"),
			strings.Contains(reason, "no route to host"),
			strings.Contains(reason, "network is unreachable"):
			reason = "连接被拒绝或不可达"
		}
		return probeResult{online: false, latencyMs: elapsed, errMsg: reason}
	}
	defer func() { _ = resp.Body.Close() }()
	code := resp.StatusCode
	return probeResult{
		online:    true,
		httpCode:  &code,
		latencyMs: time.Since(started).Milliseconds(),
	}
}

// probeAll 并发探测一批节点（键为 node_id）。
//
// 并发而不是顺序：3 台机器全挂时，顺序要 4.5 秒，并发只要 1.5 秒。
func probeAll(ctx context.Context, rows []nodeRow) map[int64]probeResult {
	client := probeClient()
	out := make(map[int64]probeResult, len(rows))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, row := range rows {
		wg.Add(1)
		go func(id int64, url string) {
			defer wg.Done()
			p := probeOne(ctx, client, url)
			mu.Lock()
			out[id] = p
			mu.Unlock()
		}(row.nodeID, row.baseURL)
	}
	wg.Wait()
	return out
}

// persistProbes 把探测结果写回 `node`（状态机字段）。
//
// 单条 SQL 完成读改写，避免「先读后写」在两个请求交错时把 streak 算错：
// `state_since` 只在状态真的变了时才更新；`ok_streak`/`fail_streak` 互斥地一增一清零；
// `last_seen_at` 只在在线时推进。
func persistProbes(ctx context.Context, st *store.Store, rows []nodeRow, probes map[int64]probeResult) error {
	now := ids.NowSecs()
	var firstErr error
	for _, row := range rows {
		p, found := probes[row.nodeID]
		if !found {
			continue
		}
		newState := "offline"
		onlineFlag := int64(0)
		if p.online {
			newState, onlineFlag = "online", 1
		}
		if _, err := st.Execute(ctx,
			"UPDATE node SET "+
				"state = ?1, "+
				"state_since = CASE WHEN state = ?1 THEN state_since ELSE ?2 END, "+
				"last_probe_at = ?2, "+
				"last_seen_at = CASE WHEN ?3 = 1 THEN ?2 ELSE last_seen_at END, "+
				"ok_streak = CASE WHEN ?3 = 1 THEN ok_streak + 1 ELSE 0 END, "+
				"fail_streak = CASE WHEN ?3 = 1 THEN 0 ELSE fail_streak + 1 END, "+
				"updated_at = ?2 "+
				"WHERE node_id = ?4",
			newState, now, onlineFlag, row.nodeID,
		); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ---------------------------------------------------------------------------
// 写：新增 / 编辑 / 删除 / 启停 / 密码回看
// ---------------------------------------------------------------------------

// nodeCreateReq 是新增请求体。指针字段 = 未传时用默认值。
type nodeCreateReq struct {
	Alias         string   `json:"alias"`
	BaseURL       string   `json:"base_url"`
	User          *string  `json:"user"`
	Password      string   `json:"password"`
	Weight        *float64 `json:"weight"`
	MaxConcurrent *int64   `json:"max_concurrent"`
	MaxRateKbps   *int64   `json:"max_rate_kbps"`
	Role          *string  `json:"role"`
	Tags          *string  `json:"tags"`
	Enabled       *bool    `json:"enabled"`
}

// nodeUpdateReq 是编辑请求体：字段全可选；`password` 留空 = **不改原密码**。
type nodeUpdateReq struct {
	Alias         *string  `json:"alias"`
	BaseURL       *string  `json:"base_url"`
	User          *string  `json:"user"`
	Password      *string  `json:"password"`
	Weight        *float64 `json:"weight"`
	MaxConcurrent *int64   `json:"max_concurrent"`
	MaxRateKbps   *int64   `json:"max_rate_kbps"`
	Role          *string  `json:"role"`
	Tags          *string  `json:"tags"`
}

// nodeErr 是节点写操作的错误（带 HTTP 状态与错误码，方便一处渲染）。
type nodeErr struct {
	status  int
	code    int64
	message string
}

func (e nodeErr) Error() string { return e.message }

func nodeInvalid(format string, args ...any) *nodeErr {
	return &nodeErr{
		status:  http.StatusUnprocessableEntity,
		code:    CodeValidationError,
		message: fmt.Sprintf(format, args...),
	}
}

func nodeNotFound(id int64) *nodeErr {
	return &nodeErr{
		status:  http.StatusNotFound,
		code:    CodeNotFound,
		message: "节点不存在：id=" + strconv.FormatInt(id, 10),
	}
}

func nodeAliasTaken(alias string) *nodeErr {
	return &nodeErr{
		status:  http.StatusConflict,
		code:    CodePreconditionFailed,
		message: "别名「" + alias + "」已被占用",
	}
}

func nodeStoreErr(err error) *nodeErr {
	return &nodeErr{status: http.StatusInternalServerError, code: CodeInternal, message: err.Error()}
}

// mapNodeUniqueViolation 把 SQLite 的唯一约束冲突翻译成「别名重复」。
func mapNodeUniqueViolation(err error, alias string) *nodeErr {
	if store.IsUniqueViolation(err, "node.alias") {
		return nodeAliasTaken(alias)
	}
	return nodeStoreErr(err)
}

// validateBaseURL 校验基址：必须带 http/https 方案，且不含空白。
func validateBaseURL(u string) (string, *nodeErr) {
	t := strings.TrimSpace(u)
	if t == "" {
		return "", nodeInvalid("地址不能为空")
	}
	if !strings.HasPrefix(t, "http://") && !strings.HasPrefix(t, "https://") {
		return "", nodeInvalid("地址必须以 http:// 或 https:// 开头（例如 http://203.0.113.10:9085）")
	}
	if strings.ContainsAny(t, " \t\r\n") {
		return "", nodeInvalid("地址不能含空格")
	}
	return strings.TrimRight(t, "/"), nil
}

// validateAlias 校验别名（长度按字符计）。
func validateAlias(a string) (string, *nodeErr) {
	t := strings.TrimSpace(a)
	if t == "" {
		return "", nodeInvalid("别名不能为空")
	}
	if n := len([]rune(t)); n > aliasMax {
		return "", nodeInvalid("别名过长（%d 字符，上限 %d）", n, aliasMax)
	}
	return t, nil
}

// validateRole 校验角色（大小写不敏感，存小写）。
func validateRole(role string) (string, *nodeErr) {
	t := strings.ToLower(strings.TrimSpace(role))
	for _, allowed := range nodeRoles {
		if t == allowed {
			return t, nil
		}
	}
	return "", nodeInvalid("角色必须是 %s 之一（收到「%s」）", strings.Join(nodeRoles, " / "), role)
}

// validateNodeNumbers 校验数值类字段。
func validateNodeNumbers(weight float64, maxConcurrent, maxRateKbps int64) *nodeErr {
	if weight != weight || weight < 0 || weight > 1e12 {
		return nodeInvalid("权重必须是不小于 0 的数（收到 %v）", weight)
	}
	if maxConcurrent < 1 || maxConcurrent > maxConcurrentCap {
		return nodeInvalid("并发上限必须在 1–%d 之间（收到 %d）", maxConcurrentCap, maxConcurrent)
	}
	if maxRateKbps < 0 {
		return nodeInvalid("限速不能为负数（收到 %d KB/s）", maxRateKbps)
	}
	return nil
}

// nodesCreate 实现 `POST /api/admin/nodes`。
func (s *adminServer) nodesCreate(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardWrite(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	var req nodeCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusUnprocessableEntity, CodeValidationError, "请求体格式错误："+err.Error())
		return
	}
	view, nerr := s.createNode(r.Context(), req)
	if nerr != nil {
		fail(w, nerr.status, nerr.code, nerr.message)
		return
	}
	ok(w, map[string]any{"node": view})
	s.renewCookie(w, r, exp)
}

func (s *adminServer) createNode(ctx context.Context, req nodeCreateReq) (nodeView, *nodeErr) {
	alias, aerr := validateAlias(req.Alias)
	if aerr != nil {
		return nodeView{}, aerr
	}
	baseURL, uerr := validateBaseURL(req.BaseURL)
	if uerr != nil {
		return nodeView{}, uerr
	}
	role := "generic"
	if req.Role != nil {
		var rerr *nodeErr
		if role, rerr = validateRole(*req.Role); rerr != nil {
			return nodeView{}, rerr
		}
	}
	if req.Password == "" {
		return nodeView{}, nodeInvalid("新增节点时密码必填")
	}
	weight := 1.0
	if req.Weight != nil {
		weight = *req.Weight
	}
	maxConcurrent := int64(3)
	if req.MaxConcurrent != nil {
		maxConcurrent = *req.MaxConcurrent
	}
	maxRateKbps := int64(0)
	if req.MaxRateKbps != nil {
		maxRateKbps = *req.MaxRateKbps
	}
	if nerr := validateNodeNumbers(weight, maxConcurrent, maxRateKbps); nerr != nil {
		return nodeView{}, nerr
	}
	user := "admin"
	if req.User != nil {
		if t := strings.TrimSpace(*req.User); t != "" {
			user = t
		}
	}
	tags := ""
	if req.Tags != nil {
		tags = *req.Tags
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	passEnc, err := s.st.Secrets.Wrap(req.Password)
	if err != nil {
		return nodeView{}, nodeInvalid("密码加密失败：%s", err.Error())
	}

	now := ids.NowSecs()
	enabledFlag := int64(0)
	if enabled {
		enabledFlag = 1
	}
	if _, err := s.st.Store.Execute(ctx,
		"INSERT INTO node (alias, base_url, user, pass_enc, device_name, weight, "+
			"max_concurrent, max_rate_bytes, role, tags, enabled, "+
			"state, state_since, fail_streak, ok_streak, throttle_until, "+
			"token_expire_at, last_probe_at, last_seen_at, created_at, updated_at) "+
			"VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, "+
			"'unknown', 0, 0, 0, 0, 0, 0, 0, ?12, ?12)",
		alias, baseURL, user, passEnc, protocol.DefaultDeviceName,
		weight, maxConcurrent, kbpsToBytes(maxRateKbps), role, tags, enabledFlag, now,
	); err != nil {
		return nodeView{}, mapNodeUniqueViolation(err, alias)
	}

	rows, err := s.st.Store.Query(ctx, "SELECT last_insert_rowid()")
	if err != nil {
		return nodeView{}, nodeStoreErr(err)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nodeView{}, nodeInvalid("无法取得新建节点的 id")
	}
	id := asInt64(rows[0][0])
	row, found, err := loadOneNodeRow(ctx, s.st.Store, id)
	if err != nil {
		return nodeView{}, nodeStoreErr(err)
	}
	if !found {
		return nodeView{}, nodeNotFound(id)
	}
	s.auditNode(ctx, id, "新增节点「"+row.alias+"」")
	return row.toView(nil), nil
}

// nodesUpdate 实现 `PUT /api/admin/nodes/{id}`。
func (s *adminServer) nodesUpdate(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardWrite(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	id, convErr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if convErr != nil {
		fail(w, http.StatusNotFound, CodeNotFound, "节点不存在：id 非法")
		return
	}
	var req nodeUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusUnprocessableEntity, CodeValidationError, "请求体格式错误："+err.Error())
		return
	}
	view, nerr := s.updateNode(r.Context(), id, req)
	if nerr != nil {
		fail(w, nerr.status, nerr.code, nerr.message)
		return
	}
	ok(w, map[string]any{"node": view})
	s.renewCookie(w, r, exp)
}

func (s *adminServer) updateNode(ctx context.Context, id int64, req nodeUpdateReq) (nodeView, *nodeErr) {
	cur, found, err := loadOneNodeRow(ctx, s.st.Store, id)
	if err != nil {
		return nodeView{}, nodeStoreErr(err)
	}
	if !found {
		return nodeView{}, nodeNotFound(id)
	}

	alias := cur.alias
	if req.Alias != nil {
		next, aerr := validateAlias(*req.Alias)
		if aerr != nil {
			return nodeView{}, aerr
		}
		alias = next
	}
	baseURL := cur.baseURL
	if req.BaseURL != nil {
		var uerr *nodeErr
		if baseURL, uerr = validateBaseURL(*req.BaseURL); uerr != nil {
			return nodeView{}, uerr
		}
	}
	role := cur.role
	if req.Role != nil {
		var rerr *nodeErr
		if role, rerr = validateRole(*req.Role); rerr != nil {
			return nodeView{}, rerr
		}
	}
	weight := cur.weight
	if req.Weight != nil {
		weight = *req.Weight
	}
	maxConcurrent := cur.maxConcurrent
	if req.MaxConcurrent != nil {
		maxConcurrent = *req.MaxConcurrent
	}
	maxRateKbps := bytesToKbps(cur.maxRateBytes)
	if req.MaxRateKbps != nil {
		maxRateKbps = *req.MaxRateKbps
	}
	if nerr := validateNodeNumbers(weight, maxConcurrent, maxRateKbps); nerr != nil {
		return nodeView{}, nerr
	}

	// ⚠️ 密码留空 = **不改**。这是本接口最容易写错的一条：
	// 若把空串当成新密码写进去，用户「只改别名」就会把密码清掉，
	// 而症状要到下次连节点时才暴露。
	passEnc := cur.passEnc
	if req.Password != nil && *req.Password != "" {
		enc, wrapErr := s.st.Secrets.Wrap(*req.Password)
		if wrapErr != nil {
			return nodeView{}, nodeInvalid("密码加密失败：%s", wrapErr.Error())
		}
		passEnc = enc
	}

	user := cur.user
	if req.User != nil {
		if t := strings.TrimSpace(*req.User); t != "" {
			user = t
		}
	}
	tags := cur.tags
	if req.Tags != nil {
		tags = *req.Tags
	}

	now := ids.NowSecs()
	if _, err := s.st.Store.Execute(ctx,
		"UPDATE node SET alias = ?1, base_url = ?2, user = ?3, pass_enc = ?4, "+
			"weight = ?5, max_concurrent = ?6, max_rate_bytes = ?7, "+
			"role = ?8, tags = ?9, updated_at = ?10 WHERE node_id = ?11",
		alias, baseURL, user, passEnc, weight, maxConcurrent,
		kbpsToBytes(maxRateKbps), role, tags, now, id,
	); err != nil {
		return nodeView{}, mapNodeUniqueViolation(err, alias)
	}

	row, found, err := loadOneNodeRow(ctx, s.st.Store, id)
	if err != nil {
		return nodeView{}, nodeStoreErr(err)
	}
	if !found {
		return nodeView{}, nodeNotFound(id)
	}
	s.auditNode(ctx, id, "编辑节点「"+row.alias+"」")
	return row.toView(nil), nil
}

// nodesDelete 实现 `DELETE /api/admin/nodes/{id}`。
func (s *adminServer) nodesDelete(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardWrite(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	id, convErr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if convErr != nil {
		fail(w, http.StatusNotFound, CodeNotFound, "节点不存在：id 非法")
		return
	}
	cur, found, err := loadOneNodeRow(r.Context(), s.st.Store, id)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if !found {
		nerr := nodeNotFound(id)
		fail(w, nerr.status, nerr.code, nerr.message)
		return
	}
	n, err := s.st.Store.Execute(r.Context(), "DELETE FROM node WHERE node_id = ?1", id)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if n == 0 {
		nerr := nodeNotFound(id)
		fail(w, nerr.status, nerr.code, nerr.message)
		return
	}
	s.auditNode(r.Context(), id, "删除节点「"+cur.alias+"」")
	ok(w, map[string]any{"node_id": id, "alias": cur.alias, "result": "deleted"})
	s.renewCookie(w, r, exp)
}

// nodesSetEnabled 实现 `POST /api/admin/nodes/{id}/enabled`。
//
// 单独做一个端点（而不是复用 PUT）的理由：语义单一。若复用 PUT，
// 前端为了切一个开关就得把整个节点对象回填一遍，任何一次回填遗漏
// 都会**静默改掉别的字段**。
func (s *adminServer) nodesSetEnabled(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardWrite(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	id, convErr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if convErr != nil {
		fail(w, http.StatusNotFound, CodeNotFound, "节点不存在：id 非法")
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusUnprocessableEntity, CodeValidationError, "请求体格式错误："+err.Error())
		return
	}
	if req.Enabled == nil {
		fail(w, http.StatusUnprocessableEntity, CodeValidationError, "缺少 enabled 字段")
		return
	}
	cur, found, err := loadOneNodeRow(r.Context(), s.st.Store, id)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if !found {
		nerr := nodeNotFound(id)
		fail(w, nerr.status, nerr.code, nerr.message)
		return
	}
	flag := int64(0)
	nextState := "关闭"
	if *req.Enabled {
		flag, nextState = 1, "启用"
	}
	if _, err := s.st.Store.Execute(r.Context(),
		"UPDATE node SET enabled = ?1, updated_at = ?2 WHERE node_id = ?3",
		flag, ids.NowSecs(), id,
	); err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	row, found, err := loadOneNodeRow(r.Context(), s.st.Store, id)
	if err != nil || !found {
		fail(w, http.StatusInternalServerError, CodeInternal, "更新后读取节点失败")
		return
	}
	s.auditNode(r.Context(), id,
		"节点「"+cur.alias+"」调度开关 → "+nextState+"（仅停止/恢复派发，不触碰机器上的下载程序）")
	ok(w, map[string]any{"node": row.toView(nil)})
	s.renewCookie(w, r, exp)
}

// nodesRevealSecret 实现 `POST /api/admin/nodes/{id}/secret`（界面「眼睛」按钮）。
//
// 用 POST 而不是 GET：密码绝不能出现在 URL 里（会进浏览器历史、反代访问日志、
// Referer）。响应同时带 `Cache-Control: no-store`。每次调用都写一条 security 审计。
func (s *adminServer) nodesRevealSecret(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardWrite(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	id, convErr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if convErr != nil {
		fail(w, http.StatusNotFound, CodeNotFound, "节点不存在：id 非法")
		return
	}
	cur, found, err := loadOneNodeRow(r.Context(), s.st.Store, id)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if !found {
		nerr := nodeNotFound(id)
		fail(w, nerr.status, nerr.code, nerr.message)
		return
	}
	plain, err := s.st.Secrets.Open(cur.passEnc)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, CodeValidationError,
			"解密节点「"+cur.alias+"」的密码失败："+err.Error()+
				"。常见原因：主密钥变过（检查 DISPATCH_SECRET_KEY 或密钥文件是否仍在）")
		return
	}
	s.auditNode(r.Context(), id, "查看节点「"+cur.alias+"」的密码")
	w.Header().Set("Cache-Control", "no-store")
	ok(w, map[string]any{"node_id": id, "alias": cur.alias, "password": plain})
	s.renewCookie(w, r, exp)
}

// auditNode 写一条安全审计（失败只忽略，不影响主流程）。
//
// 不带 logger：与 `auditPassword` 的处理方式保持一致 —— 审计失败绝不能
// 让「节点已经改成功了」这个事实变成一次 500。
func (s *adminServer) auditNode(ctx context.Context, nodeID int64, message string) {
	if _, err := s.st.Store.Execute(ctx,
		"INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at) "+
			"VALUES ('info', 'security', ?1, NULL, ?2, NULL, ?3)",
		nodeID, message, ids.NowSecs(),
	); err != nil {
		_ = err
	}
}

// ---------------------------------------------------------------------------
// 列值转换
// ---------------------------------------------------------------------------

// asString 把库列值转字符串。
func asString(v any) string {
	if s, isStr := v.(string); isStr {
		return s
	}
	return ""
}

// asInt64 把库列值转 int64（SQLite 的 INTEGER 通常解码为 int64，兼容 float64）。
func asInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case []byte:
		return 0
	case string:
		var n int64
		for _, c := range strings.TrimSpace(t) {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int64(c-'0')
		}
		return n
	default:
		return 0
	}
}

// asFloat 把库列值转 float64（`weight` 是 REAL）。
func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case int:
		return float64(t)
	default:
		return 0
	}
}
