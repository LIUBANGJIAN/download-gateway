package scheduler

import "fmt"

// DecisionKind 是决策类型。
type DecisionKind int

const (
	// DecAffinityHit 组已绑定且绑定节点可用 ⇒ 直接用绑定节点（跳过全部评分）。
	DecAffinityHit DecisionKind = iota
	// DecScored 经硬约束 + 软评分选出节点（首次建绑 / 无组普通调度）。
	DecScored
	// DecAffinityWait 排队等待（无候选 / 绑定节点满容 / 组内并发已满）。
	DecAffinityWait
	// DecSplitForced 强制拆分（`split_policy=capacity`）。**B1 不实现拆分**：
	// 产生这个决策时调用方必须按「排队 + 告警」处理，不得自行改派。
	DecSplitForced
	// DecBindingInvalid 绑定节点不健康。B1 只把它当「排队」处理，
	// strong/weak/manual 的分流由 B2 的 `on_binding_invalid()` 填充。
	DecBindingInvalid
)

// String 便于日志与 score_json 可读。
func (k DecisionKind) String() string {
	switch k {
	case DecAffinityHit:
		return "affinity_hit"
	case DecScored:
		return "scored"
	case DecAffinityWait:
		return "affinity_wait"
	case DecSplitForced:
		return "split_forced"
	case DecBindingInvalid:
		return "binding_invalid"
	default:
		return "unknown"
	}
}

// NodeEval 是单个节点的评估记录（**含被淘汰的**）。
//
// 为什么要记被淘汰的：B1-AC5 要求 `dispatch_log` 可追溯。只记"谁赢了"无法回答
// "为什么另一台没被选" —— 而那正是排障时唯一想知道的事。
type NodeEval struct {
	NodeID   int64      `json:"node_id"`
	Alias    string     `json:"alias,omitempty"`
	Score    float64    `json:"score"`
	Parts    ScoreParts `json:"parts"`
	Active   int64      `json:"active"`
	Excluded string     `json:"excluded,omitempty"`
}

// Decision 是选点结果。
type Decision struct {
	Kind DecisionKind
	// NodeID 选中的节点（DecAffinityHit / DecScored 有意义）。
	NodeID int64
	// Reason 决策原因（写 `dispatch_log.reason`，**不**写 `task.dispatch_reason`：
	// 后者的 CHECK 只允许 4 值，而 `no_candidate` 这类词不合法）。
	Reason string
	// Score 选中节点的总分。
	Score float64
	// Evaluated 全部节点的评估记录（含淘汰原因）。
	Evaluated []NodeEval
}

// Pick 是唯一入口：阶段 0 → 阶段 1 → 阶段 2。
//
// 🔒 禁止静默改绑（team-lead 裁定 R-3）：
// 组已绑定时**只可能**产出 `DecAffinityHit` / `DecAffinityWait` / `DecBindingInvalid` /
// `DecSplitForced`，**永远不会**产出 `DecScored` 去选别的节点。
// 唯一合法的换节点路径是 B2 的失效重绑（会写 `group_binding_log` 并递增 `binding_epoch`）。
func Pick(in PickInput) Decision {
	if in.Group != nil && in.Group.BoundNodeID != nil {
		return pickBound(in)
	}
	return pickScored(in)
}

// pickBound 是阶段 0 的亲和短路。
func pickBound(in PickInput) Decision {
	bn := *in.Group.BoundNodeID

	var node *Candidate
	for i := range in.Nodes {
		if in.Nodes[i].NodeID == bn {
			node = &in.Nodes[i]
			break
		}
	}
	if node == nil {
		// `task_group.bound_node_id` 是 `ON DELETE SET NULL`，正常不会走到这里。
		// 真走到了（比如外部改库），排队比乱选安全。
		return Decision{Kind: DecAffinityWait, Reason: "bound_node_missing", NodeID: bn}
	}

	// C1/C2/C3 —— 不健康 ⇒ 交给 B2（B1 按排队处置）。
	if !node.Enabled {
		return Decision{Kind: DecBindingInvalid, NodeID: bn, Reason: "bound_node_disabled"}
	}
	if node.State != "online" {
		return Decision{Kind: DecBindingInvalid, NodeID: bn, Reason: "bound_node_" + node.State}
	}
	if in.Now < node.ThrottleUntil {
		return Decision{Kind: DecBindingInvalid, NodeID: bn, Reason: "bound_node_throttled"}
	}

	// 容量：节点级 + 组级（C-12 建议**两者都判**）。
	if inflight(*node, in) >= node.MaxConcurrent {
		if in.Group.SplitPolicy == "capacity" || in.Cfg.SplitPolicy == "capacity" {
			return Decision{Kind: DecSplitForced, NodeID: bn, Reason: "bound_capacity"}
		}
		return Decision{Kind: DecAffinityWait, NodeID: bn, Reason: "bound_capacity"}
	}
	if in.Group.MaxConcurrentInGroup > 0 && in.Group.ActiveInGroup >= in.Group.MaxConcurrentInGroup {
		return Decision{Kind: DecAffinityWait, NodeID: bn, Reason: "group_capacity"}
	}

	score, parts := Score(*node, in)
	return Decision{
		Kind:   DecAffinityHit,
		NodeID: bn,
		Reason: "affinity_hit",
		Score:  score,
		Evaluated: []NodeEval{{
			NodeID: bn, Alias: node.Alias, Score: score, Active: node.Active, Parts: parts,
		}},
	}
}

// pickScored 是阶段 1（硬约束）→ 阶段 2（软评分）。
func pickScored(in PickInput) Decision {
	cands := HardFilter(in)

	evals := make([]NodeEval, 0, len(in.Nodes))
	passed := make(map[int64]bool, len(cands))
	for _, c := range cands {
		passed[c.NodeID] = true
	}
	for _, n := range in.Nodes {
		if !passed[n.NodeID] {
			evals = append(evals, NodeEval{
				NodeID: n.NodeID, Alias: n.Alias, Active: n.Active,
				Excluded: excludedReason(n, in),
			})
			continue
		}
		s, parts := Score(n, in)
		evals = append(evals, NodeEval{
			NodeID: n.NodeID, Alias: n.Alias, Score: s, Parts: parts, Active: n.Active,
		})
	}

	if len(cands) == 0 {
		// 候选为空 ⇒ 排队（**绝不静默丢弃**，也不把任务标失败）。
		return Decision{Kind: DecAffinityWait, Reason: "no_candidate", Evaluated: evals}
	}

	best, ok := ArgMin(cands, in)
	if !ok {
		return Decision{Kind: DecAffinityWait, Reason: "no_candidate", Evaluated: evals}
	}
	s, _ := Score(best, in)
	return Decision{
		Kind:      DecScored,
		NodeID:    best.NodeID,
		Reason:    "scored",
		Score:     s,
		Evaluated: evals,
	}
}

// Describe 是给日志/审计用的一行摘要。
func (d Decision) Describe() string {
	switch d.Kind {
	case DecAffinityHit, DecScored:
		return fmt.Sprintf("%s→node=%d score=%.4f", d.Kind, d.NodeID, d.Score)
	default:
		return fmt.Sprintf("%s(%s) node=%d", d.Kind, d.Reason, d.NodeID)
	}
}
