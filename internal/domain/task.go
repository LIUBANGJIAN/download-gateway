// Package domain 是 Go 版的领域模型层：任务类型、状态、错误分类、行模型。
//
// 本包**只有纯数据与纯函数**，不依赖数据库、HTTP、时钟之外的任何东西。
// 它是 Go 重构的第一块砖：`store` / `scheduler` / `ingress` / `admin` 全部依赖它。
//
// # 与 DDL 的关系
//
// 这里所有「枚举」的取值集**不是手抄的**，而是由
// `TestEnumSetsMatchFrozenDDL` 直接从 `migrations/V1__init.sql` 的 `CHECK (...)`
// 子句里抽出来比对的。DDL 是冻结的契约金样，任何一侧漂移都会让测试红。
//
// # 与 `internal/state` 的分工（防止后来人读串）
//
//   - **本包**（`internal/domain`）：纯领域模型与纯函数（`internal_state ↔ aria_status`
//     映射、任务枚举、行投影），**不持有任何运行时对象**。
//   - `internal/state`：两端口共享**状态容器**（`PublicState`/`HandshakeState`/`AdminState`），
//     持有 DB 句柄、握手令牌表、会话表、策略、密钥等运行时对象。
//     两个包各有一个 `state.go`，**同名但不同包**（`package domain` vs `package state`），不是一回事。
package domain

import "fmt"

// ---------------------------------------------------------------------------
// 任务类型 ↦ task.kind 的 CHECK 取值
// ---------------------------------------------------------------------------

// TaskKind 任务类型。对应 Rust 侧 `tasks::TaskKind`。
type TaskKind string

const (
	// KindHTTP http / https / ftp 直链。
	KindHTTP TaskKind = "http"
	// KindBT BitTorrent 种子（种子文件）。
	KindBT TaskKind = "bt"
	// KindMagnet 磁力链接。
	KindMagnet TaskKind = "magnet"
	// KindTorrent .torrent 链接。
	KindTorrent TaskKind = "torrent"
)

// allKinds 必须与 DDL 的 `kind IN (...)` 完全一致。
var allKinds = []TaskKind{KindHTTP, KindBT, KindMagnet, KindTorrent}

// AllKinds 返回全部合法任务类型（副本，防调用方改写内部表）。
func AllKinds() []TaskKind {
	out := make([]TaskKind, len(allKinds))
	copy(out, allKinds)
	return out
}

// Valid 报告该取值是否落在 DDL 允许的集合内。
func (k TaskKind) Valid() bool {
	return contains(allKinds, k)
}

// ParseTaskKind 把外部字符串收敛成 TaskKind，非法值返回错误。
func ParseTaskKind(s string) (TaskKind, error) {
	k := TaskKind(s)
	if !k.Valid() {
		return "", fmt.Errorf("kind 非法: %q（允许 %v）", s, allKinds)
	}
	return k, nil
}

// ---------------------------------------------------------------------------
// 调用来源（写 event_log 的 detail.origin 用）
// ---------------------------------------------------------------------------

// IngressOrigin 调用来源。对应 Rust 侧 `tasks::IngressOrigin`。
type IngressOrigin string

const (
	// OriginAria2 Aria2 JSON-RPC 面。
	OriginAria2 IngressOrigin = "aria2"
	// OriginBitCometHTTP BitComet `http/add`。
	OriginBitCometHTTP IngressOrigin = "bitcomet_http"
	// OriginBitCometBT BitComet `bt/add`。
	OriginBitCometBT IngressOrigin = "bitcomet_bt"
	// OriginBitCometTorrent BitComet `torrent_links/add`。
	OriginBitCometTorrent IngressOrigin = "bitcomet_torrent"
	// OriginAdmin 管理台 `POST /api/admin/tasks`。
	OriginAdmin IngressOrigin = "admin"
)

// ---------------------------------------------------------------------------
// 内部状态 ↦ task.internal_state
// ---------------------------------------------------------------------------

// InternalState 内部状态。
//
// ⚠️ DDL 对 `internal_state` **没有** CHECK 约束（只有 `NOT NULL DEFAULT 'queued'`），
// 所以这份取值集是**代码侧的权威口径**，不是数据库强制的。
// 它虽然不进 DDL 比对测试，但与之配套的 `aria_status` 派生表在
// `TestAriaStatusTableIsTotal` 里被要求**对每个取值都有确定输出**。
type InternalState string

const (
	// StateQueued 已受理、排队中（尚未派发）。
	StateQueued InternalState = "queued"
	// StateDispatching 正在派发到某个节点（已选定，尚未确认）。
	StateDispatching InternalState = "dispatching"
	// StateRunning 节点已确认在下载。
	StateRunning InternalState = "running"
	// StatePaused 用户主动暂停（或 `not_started` 未点开始）。
	StatePaused InternalState = "paused"
	// StateCompleted 已完成。
	StateCompleted InternalState = "completed"
	// StateFailed 失败（重试耗尽或不可恢复错误）。
	StateFailed InternalState = "failed"
	// StateRemoved 已移除。
	StateRemoved InternalState = "removed"
	// StateStopped 停止 —— **双义**：既可能已完成、也可能已暂停、还可能带错误。
	// 反解规则见 AriaStatus。
	StateStopped InternalState = "stopped"
)

// allInternalStates 必须与 Rust 侧 `tasks::state::aria_status` 的 match 分支一一对应。
var allInternalStates = []InternalState{
	StateQueued, StateDispatching, StateRunning, StatePaused,
	StateCompleted, StateFailed, StateRemoved, StateStopped,
}

// AllInternalStates 返回全部内部状态（副本）。
func AllInternalStates() []InternalState {
	out := make([]InternalState, len(allInternalStates))
	copy(out, allInternalStates)
	return out
}

// ---------------------------------------------------------------------------
// Aria2 对外的 status 取值（只有 6 个）
// ---------------------------------------------------------------------------

// AriaStatus 是 Aria2 协议里 `status` 字段的取值集。
// 它比 InternalState 窄得多，映射是单向的 —— 见 AriaStatus 函数。
type AriaStatus string

const (
	// AriaWaiting 排队/等待中。
	AriaWaiting AriaStatus = "waiting"
	// AriaPaused 已暂停。
	AriaPaused AriaStatus = "paused"
	// AriaActive 下载中。
	AriaActive AriaStatus = "active"
	// AriaComplete 已完成。
	AriaComplete AriaStatus = "complete"
	// AriaError 出错。
	AriaError AriaStatus = "error"
	// AriaRemoved 已移除。
	AriaRemoved AriaStatus = "removed"
)

// allAriaStatuses 全部 Aria2 对外状态（供穷举测试用）。
var allAriaStatuses = []AriaStatus{
	AriaWaiting, AriaPaused, AriaActive, AriaComplete, AriaError, AriaRemoved,
}

// AllAriaStatuses 返回全部 Aria2 对外状态（副本）。
func AllAriaStatuses() []AriaStatus {
	out := make([]AriaStatus, len(allAriaStatuses))
	copy(out, allAriaStatuses)
	return out
}

// ---------------------------------------------------------------------------
// 错误分类 ↦ task.error_class 的 CHECK 取值
// ---------------------------------------------------------------------------

// ErrorClass 错误分类。对应 Rust 侧 `tasks::state::map_error_class` 的值域。
type ErrorClass string

const (
	// ErrNone 无错误。
	ErrNone ErrorClass = "none"
	// ErrNotStarted 用户尚未点开始（**不是错误**）。
	ErrNotStarted ErrorClass = "not_started"
	// ErrServerFileNotFound 服务端文件不存在。
	ErrServerFileNotFound ErrorClass = "server_file_not_found"
	// ErrOutOfRetry 重试次数耗尽。
	ErrOutOfRetry ErrorClass = "out_of_retry"
	// ErrNodeDown 节点离线。
	ErrNodeDown ErrorClass = "node_down"
	// ErrSilentlyNotCreated 节点"静默未创建"（返回 OK 但任务不存在）。
	ErrSilentlyNotCreated ErrorClass = "silently_not_created"
	// ErrThrottled 被限速/限流。
	ErrThrottled ErrorClass = "throttled"
	// ErrAgentTimeout 代理侧超时。
	ErrAgentTimeout ErrorClass = "agent_timeout"
	// ErrUnknown 未知错误（兜底）。
	ErrUnknown ErrorClass = "unknown"
)

// allErrorClasses 必须与 DDL 的 `error_class IN (...)` 完全一致（含顺序无关）。
var allErrorClasses = []ErrorClass{
	ErrNone, ErrNotStarted, ErrServerFileNotFound, ErrOutOfRetry,
	ErrNodeDown, ErrSilentlyNotCreated, ErrThrottled, ErrAgentTimeout, ErrUnknown,
}

// AllErrorClasses 返回全部错误分类（副本）。
func AllErrorClasses() []ErrorClass {
	out := make([]ErrorClass, len(allErrorClasses))
	copy(out, allErrorClasses)
	return out
}

// Valid 报告该取值是否落在 DDL 允许的集合内。
func (e ErrorClass) Valid() bool { return contains(allErrorClasses, e) }

// IsError 报告该分类是否代表「真的出错了」。
//
// `none`（无错误）与 `not_started`（用户未点开始）都**不算**错误 ——
// 这正是 Aria2 `errorCode` 取 0 的判据，见 AriaViewFrom。
func (e ErrorClass) IsError() bool { return e != ErrNone && e != ErrNotStarted }

// ---------------------------------------------------------------------------
// 尺寸状态 ↦ task.size_state 的 CHECK 取值
// ---------------------------------------------------------------------------

// SizeState 尺寸解析状态。对应 Rust 侧 `create_task` 恒写 `unknown`。
type SizeState string

const (
	// SizeUnknown 未知（新建任务的初始值）。
	SizeUnknown SizeState = "unknown"
	// SizeProbing 探测中。
	SizeProbing SizeState = "probing"
	// SizeResolved 已解析。
	SizeResolved SizeState = "resolved"
	// SizeUnresolved 无法解析。
	SizeUnresolved SizeState = "unresolved"
	// SizeEstimated 估算值。
	SizeEstimated SizeState = "estimated"
)

// allSizeStates 必须与 DDL 的 `size_state IN (...)` 完全一致。
var allSizeStates = []SizeState{
	SizeUnknown, SizeProbing, SizeResolved, SizeUnresolved, SizeEstimated,
}

// AllSizeStates 返回全部尺寸状态（副本）。
func AllSizeStates() []SizeState {
	out := make([]SizeState, len(allSizeStates))
	copy(out, allSizeStates)
	return out
}

// Valid 报告该取值是否落在 DDL 允许的集合内。
func (s SizeState) Valid() bool { return contains(allSizeStates, s) }

// ---------------------------------------------------------------------------
// 派发原因 ↦ task.dispatch_reason 的 CHECK 取值
// ---------------------------------------------------------------------------

// DispatchReason 派发原因。**当前代码从不写它**（调度内核未落地，见 crate 文档），
// 但取值集是 DDL 冻结契约的一部分，先钉住以免将来漂移。
type DispatchReason string

const (
	// ReasonAffinityHit 命中组亲和（同组任务复用已绑定节点）。
	ReasonAffinityHit DispatchReason = "affinity_hit"
	// ReasonScored 经软评分选出节点。
	ReasonScored DispatchReason = "scored"
	// ReasonAffinityWait 组已绑定节点但该节点暂不可用，**等待而非拆分**。
	ReasonAffinityWait DispatchReason = "affinity_wait"
	// ReasonSplitForced 强制拆分（亲和等待超时）。
	ReasonSplitForced DispatchReason = "split_forced"
)

// allDispatchReasons 必须与 DDL 的 `dispatch_reason IN (...)` 完全一致。
var allDispatchReasons = []DispatchReason{
	ReasonAffinityHit, ReasonScored, ReasonAffinityWait, ReasonSplitForced,
}

// AllDispatchReasons 返回全部派发原因（副本）。
func AllDispatchReasons() []DispatchReason {
	out := make([]DispatchReason, len(allDispatchReasons))
	copy(out, allDispatchReasons)
	return out
}

// Valid 报告该取值是否落在 DDL 允许的集合内。
func (d DispatchReason) Valid() bool { return contains(allDispatchReasons, d) }

// ---------------------------------------------------------------------------
// 读模型
// ---------------------------------------------------------------------------

// TaskRow 是 `task` 表的读取投影。
//
// ⚠️ **字段是 Rust 侧 `tasks::row::TaskRow` 的子集**：不含 `dedup_key` / `dedup_level` /
// `episode_no` / `progress_at` / `batch_pending` / `dispatch_reason`。
// 这不是遗漏 —— Rust 的 `SELECT_COLS`（`tasks/query.rs`）同样不选这些列，
// Go 版按同一份列清单投影，保证读路径与写口径一致（BUG-4 的教训）。
//
// 可选列用指针表达 Rust 的 `Option<T>`；SQL NULL ⇒ nil。
type TaskRow struct {
	TaskID            string
	GID               *string
	GroupID           *int64
	SourceKey         *string
	Kind              TaskKind
	URLRaw            string
	URLNorm           *string
	Name              *string
	SizeState         SizeState
	TotalSize         *int64
	DownloadedSize    int64
	Permillage        int64
	AriaStatus        string // 对应 task.aria_status 列（写路径落库的权威口径）
	InternalState     InternalState
	ErrorClass        *ErrorClass
	ErrorMessage      *string
	NodeID            *int64
	NodeTaskID        *string
	AttemptNo         int64
	MaxAttempts       int64
	NextRetryAt       int64
	PossibleDuplicate bool
	CreatedAt         int64
	UpdatedAt         int64
	CompletedAt       *int64
}

// contains 用 == 判定成员（取值集都很小，线性扫描足够且无哈希开销）。
func contains[T comparable](set []T, v T) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
