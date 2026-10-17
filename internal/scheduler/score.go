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
}

// GroupRef 是组的**决策视图**（nil 表示任务不属于任何组）。
type GroupRef struct {
	GroupID int64
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
	// BindingPolicy ∈ strong/weak/manual（B1 只读不分支，分流归 B2）。
	BindingPolicy string
	// SplitPolicy ∈ none/capacity（B1 只支持 none 的语义：满容即排队）。
	SplitPolicy string
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
	if n.Role == "netdisk" {
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
		if n.Active >= n.MaxConcurrent { // C4
			continue
		}
		if in.SourceStrategy == "pinned" && in.PinnedNodeID != nil && n.NodeID != *in.PinnedNodeID { // C5
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
	case n.Active >= n.MaxConcurrent:
		return "C4_capacity_full"
	case in.SourceStrategy == "pinned" && in.PinnedNodeID != nil && n.NodeID != *in.PinnedNodeID:
		return "C5_source_pinned_elsewhere"
	default:
		return ""
	}
}
