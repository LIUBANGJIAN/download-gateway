// Aria2 兼容面：`POST /jsonrpc`（JSON-RPC 2.0）。
//
// 语义逐条对齐 `crates/dispatch-core/src/ingress/aria2.rs`：
//
//	· HTTP 状态恒 200，错误只在 JSON-RPC `error` 对象里表达；
//	· 未实现方法返 `-32601`，**绝不** 404；
//	· `token:` 参数由 `TokenParamOf` 统一剥离并校验；
//	· `system.multicall` 外层**不校验** token（官方手册：token 由每个子调用各自携带），
//	  子调用上限 32 条；
//	· `remove` 是**软删**（推进 `removed` 终态），`removeDownloadResult` 才硬删行。
package ingress

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/tasks"
)

// contextQ 只是 `context.Context` 的短别名，让下面这些 handler 的签名短一些。
type contextQ = context.Context

// JSON-RPC / aria2 错误码（逐字不变）。
const (
	rpcParseError     int64 = -32700
	rpcInvalidRequest int64 = -32600
	rpcInvalidParams  int64 = -32602
	rpcMethodNotFound int64 = -32601
	rpcAriaGeneric    int64 = 1
)

// multicallMax 是 `system.multicall` 的子请求上限。
const multicallMax = 32

// stoppedStates 是 aria2 `tellStopped` 覆盖的终态集合。
var stoppedStates = []string{"completed", "failed", "removed"}

// implementedMethods 是 `system.listMethods` 的返回清单（与 Rust 逐字同序）。
var implementedMethods = []string{
	"aria2.addUri",
	"aria2.remove",
	"aria2.forceRemove",
	"aria2.removeDownloadResult",
	"aria2.pause",
	"aria2.forcePause",
	"aria2.pauseAll",
	"aria2.forcePauseAll",
	"aria2.unpause",
	"aria2.unpauseAll",
	"aria2.tellStatus",
	"aria2.getUris",
	"aria2.getFiles",
	"aria2.tellActive",
	"aria2.tellWaiting",
	"aria2.tellStopped",
	"aria2.getOption",
	"aria2.getGlobalStat",
	"aria2.getVersion",
	"aria2.getSessionInfo",
	"system.multicall",
	"system.listMethods",
}

// rpcErr 是方法级错误。
type rpcErr struct {
	code    int64
	message string
}

func newRPCErr(code int64, msg string) *rpcErr { return &rpcErr{code: code, message: msg} }

// jsonrpc 是 `POST /jsonrpc` 入口：支持单条、批量数组、`system.multicall`。
func (s *server) jsonrpc(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		writeJSON(w, http.StatusOK, errorObj(nil, rpcParseError, "Parse error"))
		return
	}
	var raw json.RawMessage = body
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		writeJSON(w, http.StatusOK, errorObj(nil, rpcParseError, "Parse error"))
		return
	}
	if arr, isArr := probe.([]any); isArr {
		out := make([]any, 0, len(arr))
		for _, item := range arr {
			out = append(out, s.dispatchCall(r, item))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if _, isObj := probe.(map[string]any); !isObj {
		writeJSON(w, http.StatusOK, errorObj(nil, rpcInvalidRequest, "Invalid Request"))
		return
	}
	writeJSON(w, http.StatusOK, s.dispatchCall(r, probe))
}

// dispatchCall 处理单个 RPC 调用对象。
func (s *server) dispatchCall(r *http.Request, call any) any {
	obj, _ := call.(map[string]any)
	id := obj["id"]

	// JSON-RPC 2.0 §4：顶层对象必须带 `"jsonrpc":"2.0"`。
	if v, _ := obj["jsonrpc"].(string); v != "2.0" {
		return errorObj(id, rpcInvalidRequest, `Invalid Request: missing or invalid jsonrpc (must be "2.0")`)
	}
	method, ok := obj["method"].(string)
	if !ok {
		return errorObj(id, rpcInvalidRequest, "Invalid Request: missing method")
	}
	params, _ := obj["params"].([]any)
	if params == nil {
		params = []any{}
	}

	if method == "system.multicall" {
		// 外层只做「剥离」不做判定：官方手册明确 token 由每个子调用各自携带。
		if len(params) > 0 {
			if str, ok := params[0].(string); ok && len(str) >= 6 && str[:6] == "token:" {
				params = params[1:]
			}
		}
		calls, _ := params[0].([]any)
		if calls == nil {
			return errorObj(id, rpcInvalidParams, "system.multicall 需要数组参数")
		}
		if len(calls) > multicallMax {
			return errorObj(id, rpcInvalidRequest, "system.multicall 最多 "+strconv.Itoa(multicallMax)+" 条")
		}
		results := make([]any, 0, len(calls))
		for _, c := range calls {
			sub, _ := c.(map[string]any)
			mname, _ := sub["methodName"].(string)
			cparams, _ := sub["params"].([]any)
			if cparams == nil {
				cparams = []any{}
			}
			if mname == "" {
				results = append(results, map[string]any{"code": rpcInvalidRequest, "message": "Invalid Request"})
				continue
			}
			if v, e := s.handleMethod(r, mname, cparams); e != nil {
				results = append(results, map[string]any{"code": e.code, "message": e.message})
			} else {
				results = append(results, []any{v})
			}
		}
		return okObj(id, results)
	}

	if method == "system.listMethods" {
		return okObj(id, implementedMethods)
	}

	if v, e := s.handleMethod(r, method, params); e != nil {
		return errorObj(id, e.code, e.message)
	} else {
		return okObj(id, v)
	}
}

// handleMethod 分派并执行单个 aria2 方法（含 `token:` 校验）。
func (s *server) handleMethod(r *http.Request, method string, params []any) (any, *rpcErr) {
	if err := TokenParamOf(&params, s.st.PublicToken); err != nil {
		return nil, newRPCErr(Aria2Unauthorized, "Unauthorized")
	}
	ctx := r.Context()
	db := s.st.Store

	switch method {
	case "aria2.addUri":
		return s.addURI(ctx, params)
	case "aria2.remove", "aria2.forceRemove":
		gid, e := strParam(params, 0)
		if e != nil {
			return nil, e
		}
		if e := s.setByGID(ctx, gid, "removed", "removed", nil); e != nil {
			return nil, e
		}
		return gid, nil
	case "aria2.removeDownloadResult":
		gid, e := strParam(params, 0)
		if e != nil {
			return nil, e
		}
		row, err := tasks.GetByGID(ctx, db, gid)
		if err != nil {
			return nil, storeErr(err)
		}
		if row == nil {
			return nil, newRPCErr(rpcAriaGeneric, "GID "+gid+" 不存在")
		}
		if _, err := tasks.DeleteTask(ctx, db, row.TaskID); err != nil {
			return nil, storeErr(err)
		}
		return "OK", nil
	case "aria2.pause", "aria2.forcePause":
		gid, e := strParam(params, 0)
		if e != nil {
			return nil, e
		}
		notStarted := "not_started"
		if e := s.setByGID(ctx, gid, "paused", "paused", &notStarted); e != nil {
			return nil, e
		}
		return gid, nil
	case "aria2.unpause":
		gid, e := strParam(params, 0)
		if e != nil {
			return nil, e
		}
		if e := s.setByGID(ctx, gid, "queued", "waiting", nil); e != nil {
			return nil, e
		}
		return gid, nil
	case "aria2.pauseAll", "aria2.forcePauseAll":
		rows, err := tasks.ListByStates(ctx, db, []string{"queued", "running", "dispatching"}, 10000)
		if err != nil {
			return nil, storeErr(err)
		}
		notStarted := "not_started"
		for i := range rows {
			_, _ = tasks.SetState(ctx, db, rows[i].TaskID, "paused", "paused", &notStarted)
		}
		return "OK", nil
	case "aria2.unpauseAll":
		rows, err := tasks.ListByStates(ctx, db, []string{"paused"}, 10000)
		if err != nil {
			return nil, storeErr(err)
		}
		for i := range rows {
			_, _ = tasks.SetState(ctx, db, rows[i].TaskID, "queued", "waiting", nil)
		}
		return "OK", nil
	case "aria2.tellStatus":
		row, e := s.rowByGID(ctx, params)
		if e != nil {
			return nil, e
		}
		return statusObj(row), nil
	case "aria2.getUris":
		row, e := s.rowByGID(ctx, params)
		if e != nil {
			return nil, e
		}
		return []any{map[string]any{"uri": row.URLRaw, "status": "waiting"}}, nil
	case "aria2.getFiles":
		row, e := s.rowByGID(ctx, params)
		if e != nil {
			return nil, e
		}
		return []any{fileObj(row)}, nil
	case "aria2.getOption":
		row, e := s.rowByGID(ctx, params)
		if e != nil {
			return nil, e
		}
		return map[string]any{"gid": derefString(row.GID), "dir": "", "out": derefString(row.Name)}, nil
	case "aria2.tellActive":
		rows, err := tasks.ListByStates(ctx, db, []string{"running", "dispatching"}, 100)
		if err != nil {
			return nil, storeErr(err)
		}
		return statusList(rows), nil
	case "aria2.tellWaiting":
		rows, err := tasks.ListByStates(ctx, db, []string{"queued", "paused"}, 100000)
		if err != nil {
			return nil, storeErr(err)
		}
		return statusList(sliceByOffsetNum(rows, params)), nil
	case "aria2.tellStopped":
		rows, err := tasks.ListByStates(ctx, db, stoppedStates, 100000)
		if err != nil {
			return nil, storeErr(err)
		}
		return statusList(sliceByOffsetNum(rows, params)), nil
	case "aria2.getGlobalStat":
		waiting, err := tasks.ListByStates(ctx, db, []string{"queued", "paused", "dispatching"}, 1000000)
		if err != nil {
			return nil, storeErr(err)
		}
		stopped, err := tasks.ListByStates(ctx, db, stoppedStates, 1000000)
		if err != nil {
			return nil, storeErr(err)
		}
		nw := strconv.Itoa(len(waiting))
		ns := strconv.Itoa(len(stopped))
		return map[string]any{
			"downloadSpeed":   "0",
			"uploadSpeed":     "0",
			"numActive":       "0",
			"numWaiting":      nw,
			"numStopped":      ns,
			"numStoppedTotal": ns,
		}, nil
	case "aria2.getVersion":
		return map[string]any{
			"version":         s.version,
			"enabledFeatures": []string{"BitTorrent", "MessageDigest", "RPC"},
		}, nil
	case "aria2.getSessionInfo":
		return map[string]any{"sessionId": s.st.SessionID}, nil
	default:
		return nil, newRPCErr(rpcMethodNotFound, "Method not found: "+method)
	}
}

// addURI 实现 `aria2.addUri`：只取首个 URL，`out`/`dir`/`pause` 映射到入库字段。
func (s *server) addURI(ctx contextQ, params []any) (any, *rpcErr) {
	// ⚠️ 必须先判长度再索引：`aria2.addUri` 的 `params` 可以是**只有一段**的
	// `[["url"]]`（绝大多数客户端就是这么发的）。直接写 `params[1]` 会在这里
	// panic —— 而 panic 发生在 HTTP handler 里，症状是「连接被重置 + 500」，
	// 从客户端看像"网关挂了"，从日志看才发现是索引越界。
	// 这不是假设：2026-10-18 用 `[["https://...r.mkv"]]` 实测触发过。
	if len(params) == 0 {
		return nil, newRPCErr(rpcInvalidParams, "aria2.addUri 需要 [urls] 参数")
	}
	uris, _ := params[0].([]any)
	if len(uris) == 0 {
		return nil, newRPCErr(rpcInvalidParams, "aria2.addUri 需要 [urls] 参数")
	}
	first, ok := uris[0].(string)
	if !ok || first == "" {
		return nil, newRPCErr(rpcInvalidParams, "aria2.addUri 的 urls 不能为空")
	}
	var (
		out, dir   *string
		startLater bool
	)
	if len(params) > 1 {
		if opts, ok := params[1].(map[string]any); ok {
			if v, ok := opts["out"].(string); ok {
				out = &v
			}
			if v, ok := opts["dir"].(string); ok {
				dir = &v
			}
			if v, ok := opts["pause"].(string); ok && v == "true" {
				startLater = true
			}
		}
	}
	created, err := tasks.CreateTask(ctx, s.st.Store, tasks.TaskCreateInput{
		Kind:           domain.KindHTTP,
		URLRaw:         first,
		Filename:       out,
		StartLater:     startLater,
		SaveFolderHint: dir,
		Origin:         domain.OriginAria2,
	})
	if err != nil {
		return nil, newRPCErr(rpcAriaGeneric, "入库失败: "+err.Error())
	}
	return created.GID, nil
}

// setByGID 按 GID 改状态；GID 不存在 ⇒ aria2 业务错误 `1`。
func (s *server) setByGID(ctx contextQ, gid, internal, aria string, errClass *string) *rpcErr {
	row, err := tasks.GetByGID(ctx, s.st.Store, gid)
	if err != nil {
		return storeErr(err)
	}
	if row == nil {
		return newRPCErr(rpcAriaGeneric, "GID "+gid+" 不存在")
	}
	if _, err := tasks.SetState(ctx, s.st.Store, row.TaskID, internal, aria, errClass); err != nil {
		return storeErr(err)
	}
	return nil
}

// rowByGID 取首个参数为 GID 的任务行。
func (s *server) rowByGID(ctx contextQ, params []any) (*domain.TaskRow, *rpcErr) {
	gid, e := strParam(params, 0)
	if e != nil {
		return nil, e
	}
	row, err := tasks.GetByGID(ctx, s.st.Store, gid)
	if err != nil {
		return nil, storeErr(err)
	}
	if row == nil {
		return nil, newRPCErr(rpcAriaGeneric, "GID "+gid+" 不存在")
	}
	return row, nil
}

func storeErr(err error) *rpcErr { return newRPCErr(rpcAriaGeneric, "存储错误: "+err.Error()) }

// strParam 取第 idx 个字符串参数（idx 从 0 计）。
func strParam(params []any, idx int) (string, *rpcErr) {
	if idx < len(params) {
		if v, ok := params[idx].(string); ok {
			return v, nil
		}
	}
	return "", newRPCErr(rpcInvalidParams, "缺少第 "+strconv.Itoa(idx+1)+" 个字符串参数")
}

// sliceByOffsetNum 应用 aria2 `offset`/`num`：offset 缺省或负值 ⇒ 0；
// num 缺省或负值 ⇒ 不限；`0` ⇒ 空。
func sliceByOffsetNum(rows []domain.TaskRow, params []any) []domain.TaskRow {
	offset := int64(0)
	if len(params) > 0 {
		if v, ok := toInt64(params[0]); ok && v > 0 {
			offset = v
		}
	}
	num := int64(-1)
	if len(params) > 1 {
		if v, ok := toInt64(params[1]); ok {
			num = v
		}
	}
	if offset >= int64(len(rows)) {
		return []domain.TaskRow{}
	}
	out := rows[offset:]
	if num >= 0 && num < int64(len(out)) {
		out = out[:num]
	}
	return out
}

// toInt64 兼容 JSON 数字（float64）与字符串两种形态。
func toInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func statusList(rows []domain.TaskRow) []any {
	out := make([]any, 0, len(rows))
	for i := range rows {
		out = append(out, statusObj(&rows[i]))
	}
	return out
}

// statusObj 组装 aria2 `tellStatus` 风格对象。
//
// `dispatched` / `dispatchState` 由 [domain.DispatchView] **按库里的真实状态派生**：
// 调度内核落地后，这两个字段如果还是常量 false/queued_no_node，就成了谎报
// （任务已在节点上下载，客户端却看到"未派发"）。
//
// `dir:""` + `dirKnown:false` 仍是刻意的诚实字段：代理确实不知道节点落盘路径，
// 绝不拿 url 猜一个。
func statusObj(t *domain.TaskRow) map[string]any {
	view := domain.AriaViewFrom(t)
	total := "0"
	if t.TotalSize != nil {
		total = strconv.FormatInt(*t.TotalSize, 10)
	}
	dispatched, dispatchState := domain.DispatchView(t.NodeID, t.InternalState)
	return map[string]any{
		"gid":                    derefString(t.GID),
		"status":                 string(view.Status),
		"totalLength":            total,
		"totalLengthKnown":       t.TotalSize != nil,
		"completedLength":        strconv.FormatInt(t.DownloadedSize, 10),
		"uploadLength":           "0",
		"downloadSpeed":          "0",
		"uploadSpeed":            "0",
		"connections":            "0",
		"numSeeders":             "0",
		"seeder":                 "false",
		"pieceLength":            "0",
		"numPieces":              "0",
		"dir":                    "",
		"dirKnown":               false,
		"dispatched":             dispatched,
		"dispatchState":          dispatchState,
		"errorCode":              strconv.FormatInt(view.ErrorCode, 10),
		"errorMessage":           view.ErrorMessage,
		"verifiedLength":         "0",
		"verifyIntegrityPending": "false",
		"belongsTo":              "",
		"files":                  []any{fileObj(t)},
	}
}

// fileObj 组装文件对象：`path` 只在**已知文件名**时给文件名，未知时留空串
// （绝不拿 `url_raw` 冒充路径）。
func fileObj(t *domain.TaskRow) map[string]any {
	path := derefString(t.Name)
	length := "0"
	if t.TotalSize != nil {
		length = strconv.FormatInt(*t.TotalSize, 10)
	}
	return map[string]any{
		"index":           "1",
		"path":            path,
		"length":          length,
		"completedLength": strconv.FormatInt(t.DownloadedSize, 10),
		"selected":        "true",
		"uris":            []any{map[string]any{"uri": t.URLRaw, "status": "waiting"}},
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// okObj / errorObj 是 JSON-RPC 2.0 响应骨架。
func okObj(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func errorObj(id any, code int64, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
}
