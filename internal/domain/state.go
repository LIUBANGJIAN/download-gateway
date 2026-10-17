package domain

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
	case StateFailed:
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
// 纯函数，当前**不参与运行期逻辑**（调度内核 T03–T07 尚未落地），
// 但它的值域被 DDL 的 CHECK 约束钉住，所以先实现、先测，避免将来写调度器时随手造状态值。
//
// `error_code` 为 nil / 空串 / `"none"` 时视为"无错误上报"。
func ClassifyNodeStatus(nodeStatus string, errorCode *string, permillage int64) (InternalState, *ErrorClass) {
	var ec *ErrorClass
	if errorCode != nil && *errorCode != "" && *errorCode != string(ErrNone) {
		mapped := MapErrorClass(*errorCode)
		ec = &mapped
	}

	switch nodeStatus {
	case "complete", "completed":
		return StateCompleted, nil
	case "error", "failed":
		raw := string(ErrUnknown)
		if ec != nil {
			raw = string(*ec)
		}
		mapped := MapErrorClass(raw)
		return StateFailed, &mapped
	case "paused":
		if permillage == 0 {
			ns := ErrNotStarted
			return StatePaused, &ns
		}
		return StatePaused, nil
	case "active", "running":
		return StateRunning, nil
	case "waiting", "queued":
		return StateQueued, nil
	case "removed":
		return StateRemoved, nil
	default:
		return StateQueued, nil
	}
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
