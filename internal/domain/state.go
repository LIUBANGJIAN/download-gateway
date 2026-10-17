package domain

import "strings"

// internal_state ↔ Aria2 status 的映射。
//
// 关键点：Aria2 的 `status` 只有 6 个取值（waiting/paused/active/complete/error/removed），
// 而我们的内部状态更丰富。映射是**单向、确定性**的纯函数，便于单测与跨入口一致。
//
// 命名说明：Rust 侧函数名为 `aria_status`，但 Go 的同包内已有类型 `AriaStatus`，
// 函数与类型**不能同名**，故加 `Derive` 前缀。这是纯粹的 Go 语法约束，
// 不是语义改动 —— 逐条对照关系见 state_test.go。
//
// ⚠️ 别看串：本文件是 `package domain`（纯领域模型/纯函数，无运行时对象）。
// 同名文件 `internal/state/state.go` 是**另一个包**（`package state`：两端口共享
// 状态容器 `PublicState`/`HandshakeState`/`AdminState`，持有 DB 句柄、令牌表、
// 会话表等运行时对象）。二者的分工说明见本包 `task.go` 的包文档。

// DeriveAriaStatus 由 `(internal_state, permillage, error_class)` 得 Aria2 `status`。
//
// `stopped` 是**双义**（`02 §4.1.6`）：既可能是「已完成」也可能是「已暂停」，还可能带错误。
// 因此需要 `permillage` 与 `error_class` 辅助反解，而**不能**只看 `internal_state`。
//
// 未知取值一律回落 `waiting`（与 Rust 侧 `_ => "waiting"` 一致）——
// 这是**有意的宽松**：宁可把陌生状态显示成"排队中"，也不要让管理台因一个新状态值而 500。
func DeriveAriaStatus(internalState InternalState, permillage int64, errorClass *ErrorClass) AriaStatus {
	switch internalState {
	case StateQueued, StateDispatching:
		return AriaWaiting
	case StateRunning:
		return AriaActive
	case StatePaused:
		return AriaPaused
	case StateCompleted:
		return AriaComplete
	case StateFailed, StateFailedTerminal:
		return AriaError
	case StateRemoved:
		return AriaRemoved
	case StateStopped:
		if permillage >= 1000 {
			return AriaComplete
		}
		if errorClass != nil && errorClass.IsError() {
			return AriaError
		}
		return AriaPaused
	default:
		return AriaWaiting
	}
}

// AriaView 是完整状态视图（供 `tellStatus` / `tellActive` / … 组装响应）。
type AriaView struct {
	// Status Aria2 `status` 字符串。
	Status AriaStatus
	// ErrorCode Aria2 数字错误码（0 = 无错误，1 = 失败）。
	ErrorCode int64
	// ErrorMessage 人类可读错误信息（无错误时为空串，**不是** nil）。
	ErrorMessage string
}

// AriaViewFrom 由 TaskRow 组装 Aria2 视图。
//
// `status` **优先取 `task.aria_status` 列**（写路径已落库的权威口径，
// `NOT NULL DEFAULT 'waiting'`；仅当列为空白时才回退到 DeriveAriaStatus 的派生映射）。
// 这样读路径与写口径一致，避免「列里写着 `paused` 却因派生出 `waiting`」这类漂移（BUG-4）。
//
// `error_code` 语义对齐 Aria2：0 表示无错误；`not_started`（用户未点开始）不算错误 ⇒ 0。
func AriaViewFrom(t *TaskRow) AriaView {
	status := AriaStatus(t.AriaStatus)
	if isBlank(t.AriaStatus) {
		status = DeriveAriaStatus(t.InternalState, t.Permillage, t.ErrorClass)
	}
	var msg string
	if t.ErrorMessage != nil {
		msg = *t.ErrorMessage
	}
	return AriaView{
		Status:       status,
		ErrorCode:    boolToInt64(t.ErrorClass != nil && t.ErrorClass.IsError()),
		ErrorMessage: msg,
	}
}

// ClassifyNodeStatus 是 §4.1.6「stopped 双义」的**反解**。
//
// 输入是节点上报的 `status` / `errorCode`，输出我们内部的 `(internal_state, error_class)`。
// 纯函数；B1 起由 `internal/sync/reconcile.go` **真正调用**（此前只测不用）。
//
// `error_code` 为 nil / 空串 / `"none"` / `"OK"` 时视为"无错误上报"——
// 最后一项是 BitComet 的成功码，漏掉它会把每一次成功都判成 `unknown` 错误。
//
// # `stopped` 的双义（design/02 §4.1.6，v2 关键修正）
//
// | 输入 | 产出 |
// |---|---|
// | `stopped` + `error_code` 非空 | `failed_terminal` + 映射后分类 —— **真的失败了** |
// | `stopped` + 无错误 + `permillage ≥ 1000` | `completed` |
// | `stopped` + 无错误 + `permillage == 0` | `paused` + `not_started`（`start_later` 的正常未启动） |
// | `stopped` + 无错误 + `0 < permillage < 1000` | `paused` |
//
// 这段分支**曾经完全不存在**：节点报 `stopped` 一律落 `default: queued`，
// 于是失败任务被调度循环反复扫到、反复重投，对外还永远不显 `error`。
// 这是 `design/02 §4.1.6` 点名要修的静默错误，也是 B1 必须补齐的一条。
func ClassifyNodeStatus(nodeStatus string, errorCode *string, permillage int64) (InternalState, *ErrorClass) {
	ec := mapNodeError(errorCode)

	switch strings.ToLower(strings.TrimSpace(nodeStatus)) {
	case "complete", "completed", "seeding", "seed":
		return StateCompleted, nil
	case "error", "failed":
		// 节点自己说失败：一律终态（B1 写 `failed_terminal`，不用遗留值 `failed`）。
		return StateFailedTerminal, nonNilClass(ec)
	case "paused":
		if permillage == 0 {
			ns := ErrNotStarted
			return StatePaused, &ns
		}
		return StatePaused, nil
	case "stopped":
		// ★R-2：双义必须按三条判据分流，顺序不可颠倒（先看错误，再看完成度）。
		if ec != nil {
			return StateFailedTerminal, ec
		}
		if permillage >= 1000 {
			return StateCompleted, nil
		}
		if permillage <= 0 {
			ns := ErrNotStarted
			return StatePaused, &ns
		}
		return StatePaused, nil
	case "active", "running", "downloading", "hashing", "checking":
		return StateRunning, nil
	case "waiting", "queued", "pending":
		return StateQueued, nil
	case "removed", "deleted":
		return StateRemoved, nil
	default:
		return StateQueued, nil
	}
}

// mapNodeError 把节点错误串归一成 `*ErrorClass`；"无错误"返回 nil。
func mapNodeError(errorCode *string) *ErrorClass {
	if errorCode == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(*errorCode)) {
	case "", "none", "ok", "0", "success":
		return nil
	}
	mapped := MapErrorClass(*errorCode)
	return &mapped
}

// nonNilClass 保证"节点说失败"时一定带回一个错误分类（未知也归 `unknown`）。
func nonNilClass(ec *ErrorClass) *ErrorClass {
	if ec != nil {
		return ec
	}
	unknown := ErrUnknown
	return &unknown
}

// MapErrorClass 把节点错误串归一到 `task.error_class` 的受约束取值集（DDL CHECK）。
//
// 未知串一律归 `unknown` —— 这是 **DDL CHECK 的硬要求**：
// 若原样透传一个 DDL 不认识的串，写库会直接违反约束而失败。
// 换句话说，这个函数的"过度归一"不是保守，是正确性。
func MapErrorClass(e string) ErrorClass {
	switch ErrorClass(e) {
	case ErrNotStarted, ErrServerFileNotFound, ErrOutOfRetry, ErrNodeDown,
		ErrSilentlyNotCreated, ErrThrottled, ErrAgentTimeout:
		return ErrorClass(e)
	default:
		return ErrUnknown
	}
}

// isBlank 报告字符串是否为空或全空白。
// Rust 侧用 `t.aria_status.trim().is_empty()`，Go 保持同一语义。
func isBlank(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

// boolToInt64 把 bool 转成 0/1（Aria2 的 errorCode 口径）。
func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// DispatchView 派生对外的「派发视图」：`dispatched` 与 `dispatchState`。
//
// # 为什么它必须是派生的，而不是常量
//
// 调度内核落地前，这两个字段在管理台与 aria2 面都是**硬编码**
// （`dispatched:false` / `dispatchState:"queued_no_node"`）—— 那时确实没有组件会下发，
// 所以是诚实的。但内核落地后继续硬编码就成了**谎报**：
// 任务明明已经落到 `node_id=1`、正在下载，界面上却写着"排队、未派发"。
// 用户据此判断"没生效"，排障方向会完全跑偏。
//
// 判据以 `internal_state` 为主、`node_id` 为旁证：
//   - `queued` + 无 node ⇒ `queued_no_node`（还没选到机器）
//   - `queued` + 有 node ⇒ `queued_on_node`（已落点，节点侧在排队）
//   - 其余按状态名直出
func DispatchView(nodeID *int64, state InternalState) (dispatched bool, dispatchState string) {
	dispatched = nodeID != nil
	switch state {
	case StateQueued:
		if dispatched {
			return true, "queued_on_node"
		}
		return false, "queued_no_node"
	case StateDispatching:
		return dispatched, "dispatching"
	case StateRunning:
		return dispatched, "running"
	case StatePaused:
		return dispatched, "paused"
	case StateCompleted:
		return dispatched, "completed"
	case StateFailed, StateFailedTerminal:
		return dispatched, "failed"
	case StateRemoved:
		return dispatched, "removed"
	case StateStopped:
		return dispatched, "stopped"
	default:
		if dispatched {
			return true, "dispatched"
		}
		return false, "queued_no_node"
	}
}
