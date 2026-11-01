// Package scheduler 是**纯决策**的选点内核：不读库、不发网络、不看时钟。
//
// 纯函数不是洁癖，而是 B1 验收的前提（`design/11 §3.6` 总则 2「必须可证伪」）：
// 只有把决策与 I/O 分开，才能用一张表覆盖 C1–C5 与评分，而不需要三台真节点；
// 也只有这样，`dispatch_log.score_json` 才能被复算出来核对。
//
// 决策链严格按 `design/02 §2.1.1`：
//
//	阶段 0 · 亲和短路（组已绑定 ⇒ 命中就用绑定节点，满容/不健康就排队，绝不改绑）
//	阶段 1 · 硬约束 C1–C5（逐节点布尔淘汰）
//	阶段 2 · 软评分（值越小越优先，tie-break 保证可复现）
package scheduler

import (
	"fmt"
	"math"
)

// Candidate 是一个候选节点的**决策视图**（不是数据库行）。
type Candidate struct {
	// NodeID 节点 id（tie-break 的最后一级，保证确定性）。
	NodeID int64
	// Alias 节点别名（只用于日志与 score_json 可读性）。
	Alias string
	// Enabled ↦ `node.enabled`（C1）。
	Enabled bool
	// State ↦ `node.state`（C2，判据是**等于** online；degraded/unknown 一律不可下发）。
	State string
	// ThrottleUntil ↦ `node.throttle_until`（C3，`now >= throttle_until` 才可选）。
	ThrottleUntil int64
	// Weight ↦ `node.weight`（负载项的除数；<=0 时按 1 处理，避免除零）。
	Weight float64
	// MaxConcurrent ↦ `node.max_concurrent`（C4）。
	MaxConcurrent int64
	// Active 是 `polled_active`：该节点上 `internal_state ∈ {running, dispatching}` 的任务数。
	Active int64
	// PendingBytes 是该节点在途任务的**剩余字节估算和**（评分用，由调用方聚合）。
	PendingBytes float64
	// LastAssignedAt 是该节点最近一次被选中的时刻（秒）。
	//
	// ⚠️ 它不是 DDL 列（`design/02 §2.1.1` 的 tie-break 引用了它，但 `V1__init.sql` 没有）。
	// B1 按规格 C-04 把它放在**内存态**，随 `reserved` 一起维护；进程重启后重置。
	// 这只影响"分数完全相同时选谁"，不影响正确性。
	LastAssignedAt int64
	// Role ↦ `node.role`（来源加分用：netdisk 角色的节点天然更适合网盘来源）。
	Role string
	// RateLimited 表示该节点在本轮已触及"按节点限速"的上限（C6）。
	//
	// 它不是节点自身状态（库里的 `node.state` 不受影响）：限速是**代理侧**的
	// 节制，改库会把"我们主动放慢"误记成"节点不健康"。
	RateLimited bool
}

// GroupRef 是组的**决策视图**（nil 表示任务不属于任何组）。
type GroupRef struct {
	GroupID int64
	// AffinityKey 组的亲和键（`dispatch_one` 用它补齐"按 group_id 读到的组"的键）。
	//
	// 没有它会产生一个隐蔽 bug：任务失败重试后 `task.group_id` 已非空，
	// 调度时不再派生键 ⇒ `BindTx` 拿到的键是空串 ⇒ 既不写绑定日志、
	// 还会把 `task.group_id` 更新成 NULL（见 `grouping.BindTx` 的 nullIfZero）。
	AffinityKey string
	// BoundNodeID 已绑定节点；nil 表示未绑定（首次建绑）。
	BoundNodeID *int64
	// BindingEpoch 当前绑定代数（建绑时 +1，写进 group_binding_log）。
	BindingEpoch int64
	// GroupState ∈ unbound/active/split/draining/closed。
	GroupState string
	// MaxConcurrentInGroup ↦ `task_group.max_concurrent_in_group`（默认 2）。
	MaxConcurrentInGroup int64
	// ActiveInGroup 是该组**在途**任务数（running + dispatching）。
	ActiveInGroup int64
	// BindingPolicy ∈ strong/weak/manual（B2 分流：strong 等宽限、weak 立即拆、manual 只告警）。
	BindingPolicy string
	// SplitPolicy ∈ none/capacity（capacity 时满容允许拆组）。
	SplitPolicy string
	// WaitDeadline 是 strong 策略的等待截止时刻（0 = 尚未开始等待）。
	//
	// 落地列是 `task_group.wait_deadline`（DDL 里本来就是给这个用途留的）。
	WaitDeadline int64
	// BoundAt 是绑定建立时刻（只用于日志与"绑了多久"的可观测性）。
	BoundAt int64
}

// ScoreWeights 是软评分权重（`design/02 §2.1.1` 的 v2 五项）。
//
// ⚠️ 与 `design/04 §7.1` 的 v1 六项**不是同一套**（那套含已删除的 `w4 affinity_bonus=1.5`）。
// 按 team-lead 裁定 R-4 采 v2；两套混用会让选点结果无法解释。
type ScoreWeights struct {
	W1, W2, W3, W4, W5 float64
}

// DefaultWeights 返回 v2 默认权重：1.00 / 0.50 / 2.00 / 0.75 / 0.10。
func DefaultWeights() ScoreWeights {
	return ScoreWeights{W1: 1.00, W2: 0.50, W3: 2.00, W4: 0.75, W5: 0.10}
}

// ScoreParams 是负载项的折算参数。
type ScoreParams struct {
	// Alpha 任务数项的系数（默认 1.0）。
	Alpha float64
	// Beta 字节项的系数（默认 1e-9，把字节折算到与任务数同量级）。
	Beta float64
	// DLUnknownBytes 未知大小时假定的字节数（默认 1 GiB）。
	DLUnknownBytes float64
}

// DefaultParams 返回默认折算参数。
func DefaultParams() ScoreParams {
	return ScoreParams{Alpha: 1.0, Beta: 1e-9, DLUnknownBytes: 1 << 30}
}

// Policy 是选点策略（权重 + 开关）。
type Policy struct {
	Weights ScoreWeights
	Params  ScoreParams
	// SplitPolicy ∈ none/capacity（默认 none）。
	SplitPolicy string
	// MaxConcurrentInGroup 组内并发上限（默认 2；0 表示不限制）。
	MaxConcurrentInGroup int64

	// —— 以下五项由管理台的派发策略**开关**驱动（B1.5 接线）——
	//
	// 全部是「Disable 型」布尔：false = 该策略生效（与 B1 验收时的行为完全一致）。
	// 用「禁用」而不是「启用」做字段，是为了让零值 `Policy{}` 也等于默认行为，
	// 任何构造 Policy 的调用点都不会因为漏填字段而意外改变调度结果。

	// DisableNodeCapacity ↦ `policy.max_concurrent` 关闭 ⇒ C4 不再按节点并发上限淘汰。
	//
	// ⚠️ 这是最后一道安全阀（管理台标 `dangerous`、`can_disable=false`），
	// 接口层会拒绝关闭它；此字段存在的意义是被策略层显式测试覆盖，
	// 而不是给用户一个"把集群压垮"的开关。
	DisableNodeCapacity bool
	// DisableGroupCapacity ↦ `policy.max_concurrent_in_group` 关闭 ⇒ 忽略组内并发上限。
	DisableGroupCapacity bool
	// IgnoreNodeWeight ↦ `policy.node_weight` 关闭 ⇒ 折算负载时分母按 1（各节点等权）。
	IgnoreNodeWeight bool
	// DisableSourcePinned ↦ `policy.source_pinned` 关闭 ⇒ C5 不再按来源钉住的节点收窄候选。
	DisableSourcePinned bool
	// DisableRoleRouting ↦ `policy.role_routing` 关闭 ⇒ 来源加分不再看节点 `role`。
	DisableRoleRouting bool
	// DisableGroupAffinity ↦ `policy.group_affinity` 关闭 ⇒ 不做组亲和：
	// 已绑定的组会被**显式拆组**（写 `group_binding_log`），新成员按负载分散。
	//
	// ⚠️ 它必须走"拆组"而不是"直接用评分"：后者等于静默改绑（组还绑着 A、任务落 B、
	// 日志里没有一行解释），违反 R-3。
	DisableGroupAffinity bool
	// BindingGraceSec 是 strong 策略的等待宽限（默认 [DefaultBindingGraceSec]）。
	BindingGraceSec int64

	// —— 以下四项由 `retry_backoff` / `throttle_backoff` / `node_rate_limit` 驱动 ——
	//
	// ⚠️ 与前面五个开关不同，这三个策略**不能被"关闭"成"不重试"**：
	// 不重试等于失败即丢弃，对用户来说是数据丢失。所以它们的"关闭"语义是
	// "退回不优化的固定行为"，而不是"把能力去掉"。

	// RetryBaseSec 是重试退避的基准间隔（秒，默认 30）。
	RetryBaseSec int64
	// RetryCapSec 是退避上限（秒，默认 1800）。
	RetryCapSec int64
	// ThrottleMultiplier 是限流冷却的额外倍率（默认 1.0；策略关闭时恒为 1.0）。
	//
	// >1 表示"退得更久"（节点被限流时更保守），<1 表示"更快回来试"。
	ThrottleMultiplier float64
	// NodeRateLimitPerMin >0 时启用按节点限速（每分钟最多下发多少个）。
	NodeRateLimitPerMin int64
}

// DefaultPolicy 返回设计默认策略。
func DefaultPolicy() Policy {
	return Policy{
		Weights:              DefaultWeights(),
		Params:               DefaultParams(),
		SplitPolicy:          "none",
		MaxConcurrentInGroup: 2,
	}
}

// PickInput 是一次选点的全部输入（可完整序列化进 `dispatch_log.score_json`）。
type PickInput struct {
	// TaskID 只用于日志与 score_json。
	TaskID string
	// SourceKey 来源键（C5 与来源加分用）。
	SourceKey *string
	// SourceStrategy ∈ round_robin/affinity/pinned（C5 只在 pinned 时收窄候选）。
	SourceStrategy string
	// PinnedNodeID 来源钉住的节点（strategy=pinned 时必填）。
	PinnedNodeID *int64
	// Group 组视图；nil = 任务不属于任何组。
	Group *GroupRef
	// BoundNode 是组绑定节点的候选视图（`Group.BoundNodeID` 对应的那一台；找不到为 nil）。
	//
	// 单独给一个字段而不是让 [ResolveBinding] 自己遍历：失效判定需要的是
	// "绑定节点现在能不能用"，而不是"全量候选里有没有它"——后者在有过滤时会被混淆。
	BoundNode *Candidate
	// Nodes 全部节点（未过滤）。
	Nodes []Candidate
	// Reserved 是"已决定但节点尚未确认"的内存预留（design/04 §8.3 的 `reserved_pending`）。
	Reserved map[int64]int64
	// Now 当前时刻（秒）。由调用方注入，纯函数不自己看钟。
	Now int64
	// Cfg 策略。
	Cfg Policy
}

// ScoreParts 是评分的各分项（写进 score_json，供复盘逐项复算）。
type ScoreParts struct {
	Load   float64 `json:"load"`
	Conn   float64 `json:"conn"`
	Health float64 `json:"health"`
	Source float64 `json:"source"`
	Spread float64 `json:"spread"`
}

// ScoredNode 是一个候选的评分结果。
type ScoredNode struct {
	NodeID int64      `json:"node_id"`
	Alias  string     `json:"alias,omitempty"`
	Score  float64    `json:"score"`
	Parts  ScoreParts `json:"parts"`
}

// LoadNorm 是负载项：`α·active/weight + β·pending_bytes/weight`。
func LoadNorm(n Candidate, in PickInput) float64 {
	w := n.Weight
	// `policy.node_weight` 关闭 ⇒ 各节点等权，权重字段退化成 1。
	if in.Cfg.IgnoreNodeWeight {
		w = 1
	}
	if w <= 0 {
		w = 1
	}
	p := in.Cfg.Params
	if p.Alpha == 0 && p.Beta == 0 {
		p = DefaultParams()
	}
	return p.Alpha*float64(n.Active)/w + p.Beta*n.PendingBytes/w
}

// ConnPenalty 是连接/容量占用：`clamp(active/max_concurrent, 0, 1)`。
func ConnPenalty(n Candidate) float64 {
	if n.MaxConcurrent <= 0 {
		return 1
	}
	v := float64(n.Active) / float64(n.MaxConcurrent)
	return math.Max(0, math.Min(1, v))
}

// HealthPenalty **恒为 0**。
//
// 理由：C2 收紧后候选集里只有 `online` 节点（`design/12 §1.9.7`），
// 健康度已经由硬约束处理掉了。这个函数保留是为了让 `score_json` 的分项结构
// 与设计文档的公式一一对应，**不是**给 degraded 节点留后门。
func HealthPenalty(_ Candidate) float64 { return 0 }

// SourceBonus 是来源亲和加分（命中给 1.0，否则 0）。
//
// 判据（`design/02 §2.1.1`）：来源策略为 `affinity` 且「历史多数落在该节点
// **或** 该节点被标为 `netdisk` 角色」。
//
// ⚠️ B1 **没有**"历史落点统计"表（`task_attempt` 可以算，但每次选点做一次聚合查询
// 会把纯函数变成 I/O）。B1 只用可静态判定的那一半：`role == 'netdisk'`。
// 这一取舍写在 §4.2 的实现注释里，不假装两条判据都做了。
func SourceBonus(n Candidate, in PickInput) float64 {
	if in.SourceStrategy != "affinity" {
		return 0
	}
	// `policy.role_routing` 关闭 ⇒ 不再按节点角色给来源加分。
	if n.Role == "netdisk" && !in.Cfg.DisableRoleRouting {
		return 1.0
	}
	// 来源被钉在某个节点时，那个节点也拿加分（与 C5 同向，不冲突）。
	if in.PinnedNodeID != nil && *in.PinnedNodeID == n.NodeID {
		return 1.0
	}
	return 0
}

// SpreadPenalty 是防抖动惩罚。
//
// ⚠️ 设计文档只写"刚被选过 → 轻微惩罚"（`design/02 §2.1.1` 权重表 w5 行），
// **没有给公式**。按规格 C-03 的建议取唯一线索（"1 秒内"）：
// `1.0 if now - last_assigned_at < 1s else 0.0`。
func SpreadPenalty(n Candidate, in PickInput) float64 {
	if n.LastAssignedAt > 0 && in.Now-n.LastAssignedAt < 1 {
		return 1.0
	}
	return 0
}

// Score 计算单个候选的总分：`w1·load + w2·conn + w3·health − w4·source + w5·spread`。
func Score(n Candidate, in PickInput) (float64, ScoreParts) {
	w := in.Cfg.Weights
	if w == (ScoreWeights{}) {
		w = DefaultWeights()
	}
	parts := ScoreParts{
		Load:   LoadNorm(n, in),
		Conn:   ConnPenalty(n),
		Health: HealthPenalty(n),
		Source: SourceBonus(n, in),
		Spread: SpreadPenalty(n, in),
	}
	total := w.W1*parts.Load + w.W2*parts.Conn + w.W3*parts.Health -
		w.W4*parts.Source + w.W5*parts.Spread
	return total, parts
}

// scoreAll 给所有候选打分（保持输入顺序，便于 score_json 可读）。
func scoreAll(cands []Candidate, in PickInput) []ScoredNode {
	out := make([]ScoredNode, 0, len(cands))
	for _, c := range cands {
		s, parts := Score(c, in)
		out = append(out, ScoredNode{NodeID: c.NodeID, Alias: c.Alias, Score: s, Parts: parts})
	}
	return out
}

// ArgMin 返回分数最小的候选。
//
// tie-break（`design/02 §2.1.1` 末）：`(score, active, last_assigned_at, node_id)` 字典序升序。
// 这一条不是"精益求精"：没有它，同样的输入在不同次运行里可能选不同节点，
// 而 B1-AC4（同组同节点）与 AC5（决策可追溯）都会因此不可复现。
func ArgMin(cands []Candidate, in PickInput) (Candidate, bool) {
	best := -1
	for i := range cands {
		if best < 0 {
			best = i
			continue
		}
		if less(cands[i], cands[best], in) {
			best = i
		}
	}
	if best < 0 {
		return Candidate{}, false
	}
	return cands[best], true
}

// less 实现 tie-break 的字典序比较。
func less(a, b Candidate, in PickInput) bool {
	sa, _ := Score(a, in)
	sb, _ := Score(b, in)
	if sa != sb {
		return sa < sb
	}
	if a.Active != b.Active {
		return a.Active < b.Active
	}
	if a.LastAssignedAt != b.LastAssignedAt {
		return a.LastAssignedAt < b.LastAssignedAt
	}
	return a.NodeID < b.NodeID
}

// inflight 是 `polled_active + reserved_pending`（`design/04 §8.3`）。
func inflight(n Candidate, in PickInput) int64 {
	return n.Active + in.Reserved[n.NodeID]
}

// HardFilter 执行阶段 1 的硬约束 C1–C5。
//
// C6（重试协调）**不在这里**：它是全局的重试阈值约束，不是逐节点淘汰，
// 落在投递失败后的退避上（见 `internal/dispatch`）。
func HardFilter(in PickInput) []Candidate {
	out := make([]Candidate, 0, len(in.Nodes))
	for _, n := range in.Nodes {
		if !n.Enabled { // C1
			continue
		}
		if n.State != "online" { // C2（degraded/unknown 一律不可下发）
			continue
		}
		if in.Now < n.ThrottleUntil { // C3
			continue
		}
		if !in.Cfg.DisableNodeCapacity && n.Active >= n.MaxConcurrent { // C4
			continue
		}
		if !in.Cfg.DisableSourcePinned && in.SourceStrategy == "pinned" && in.PinnedNodeID != nil && n.NodeID != *in.PinnedNodeID { // C5
			continue
		}
		if n.RateLimited { // C6（代理侧限速）
			continue
		}
		out = append(out, n)
	}
	return out
}

// excludedReason 是给 `dispatch_log` 用的候选淘汰解释（不含分数）。
func excludedReason(n Candidate, in PickInput) string {
	switch {
	case !n.Enabled:
		return "C1_disabled"
	case n.State != "online":
		return "C2_not_online:" + n.State
	case in.Now < n.ThrottleUntil:
		return fmt.Sprintf("C3_throttled_until:%d", n.ThrottleUntil)
	case !in.Cfg.DisableNodeCapacity && n.Active >= n.MaxConcurrent:
		return "C4_capacity_full"
	case !in.Cfg.DisableSourcePinned && in.SourceStrategy == "pinned" && in.PinnedNodeID != nil && n.NodeID != *in.PinnedNodeID:
		return "C5_source_pinned_elsewhere"
	case n.RateLimited:
		return "C6_node_rate_limited"
	default:
		return ""
	}
}
