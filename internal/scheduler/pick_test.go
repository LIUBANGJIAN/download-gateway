package scheduler

import (
	"testing"
)

// healthy 返回一个"各项都合格"的节点，便于逐条注入单一违约。
func healthy(id int64) Candidate {
	return Candidate{
		NodeID: id, Alias: "n", Enabled: true, State: "online",
		Weight: 1.0, MaxConcurrent: 3, Active: 0,
	}
}

func baseInput(nodes ...Candidate) PickInput {
	return PickInput{Nodes: nodes, Now: 1000, Cfg: DefaultPolicy()}
}

// TestHardFilterEachConstraint 逐条守 C1–C5。
//
// 每条都是"删掉该约束就会红"的形态：违约节点若被放行，`len(got)` 会变成 2。
func TestHardFilterEachConstraint(t *testing.T) {
	pin := int64(2)

	cases := []struct {
		name string
		mut  func(*PickInput, *[]Candidate)
		want int
	}{
		{
			name: "C1 disabled 被剔除",
			mut: func(in *PickInput, ns *[]Candidate) {
				(*ns)[1].Enabled = false
				in.Nodes = *ns
			},
			want: 1,
		},
		{
			name: "C2 degraded 被剔除",
			mut: func(in *PickInput, ns *[]Candidate) {
				(*ns)[1].State = "degraded"
				in.Nodes = *ns
			},
			want: 1,
		},
		{
			name: "C2 unknown 被剔除",
			mut: func(in *PickInput, ns *[]Candidate) {
				(*ns)[1].State = "unknown"
				in.Nodes = *ns
			},
			want: 1,
		},
		{
			name: "C3 限流冷却中 被剔除",
			mut: func(in *PickInput, ns *[]Candidate) {
				(*ns)[1].ThrottleUntil = in.Now + 60
				in.Nodes = *ns
			},
			want: 1,
		},
		{
			name: "C3 冷却已过期 应放行",
			mut: func(in *PickInput, ns *[]Candidate) {
				(*ns)[1].ThrottleUntil = in.Now // now >= throttle_until（含等号）
				in.Nodes = *ns
			},
			want: 2,
		},
		{
			name: "C4 容量满 被剔除",
			mut: func(in *PickInput, ns *[]Candidate) {
				(*ns)[1].Active = (*ns)[1].MaxConcurrent
				in.Nodes = *ns
			},
			want: 1,
		},
		{
			name: "C5 来源钉住别处 被剔除",
			mut: func(in *PickInput, ns *[]Candidate) {
				in.SourceStrategy = "pinned"
				in.PinnedNodeID = &pin
				in.Nodes = *ns
			},
			want: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ns := []Candidate{healthy(1), healthy(2)}
			in := baseInput(ns...)
			if c.mut != nil {
				c.mut(&in, &ns)
			}
			if got := len(HardFilter(in)); got != c.want {
				t.Fatalf("HardFilter 通过数 = %d，期望 %d", got, c.want)
			}
		})
	}
}

// TestPickSkipsViolatingNode 是 B1-AC3 的纯函数形态：
// 违约节点必须不被选中（真节点版本在 e2e 里）。
func TestPickSkipsViolatingNode(t *testing.T) {
	bad := healthy(1)
	bad.State = "degraded"
	good := healthy(2)

	d := Pick(baseInput(bad, good))
	if d.Kind != DecScored {
		t.Fatalf("期望 DecScored，实得 %v", d.Kind)
	}
	if d.NodeID != 2 {
		t.Fatalf("选中的是 %d，期望 2（违约节点不得被选）", d.NodeID)
	}
	// 淘汰原因必须留痕，否则「为什么不是它」无法回答
	var found bool
	for _, e := range d.Evaluated {
		if e.NodeID == 1 && e.Excluded != "" {
			found = true
		}
	}
	if !found {
		t.Error("被淘汰的节点没有记录淘汰原因 —— dispatch_log 将无法追溯")
	}
}

// TestPickNoCandidateQueues 守「候选为空 ⇒ 排队而非失败」：
// 一旦实现成"标 failed"，用户在界面上会看到失败，而实际上是没机器可用。
func TestPickNoCandidateQueues(t *testing.T) {
	n := healthy(1)
	n.Enabled = false
	d := Pick(baseInput(n))
	if d.Kind != DecAffinityWait {
		t.Fatalf("期望 DecAffinityWait，实得 %v", d.Kind)
	}
	if d.Reason != "no_candidate" {
		t.Fatalf("原因 = %q，期望 no_candidate", d.Reason)
	}
	if d.NodeID != 0 {
		t.Fatalf("排队时不得有选中节点，实得 %d", d.NodeID)
	}
}

// TestPickAffinityHitReusesBoundNode 守阶段 0：命中后**跳过评分**，
// 哪怕另一个节点分数明显更低也不许换 —— 换了就是静默改绑。
func TestPickAffinityHitReusesBoundNode(t *testing.T) {
	bound := healthy(1)
	bound.Active = 2    // 负载更高，评分更差
	other := healthy(2) // 完全空闲，评分更好

	bn := int64(1)
	in := baseInput(bound, other)
	in.Group = &GroupRef{GroupID: 7, BoundNodeID: &bn, GroupState: "active", MaxConcurrentInGroup: 2}

	d := Pick(in)
	if d.Kind != DecAffinityHit {
		t.Fatalf("期望 DecAffinityHit，实得 %v(%s)", d.Kind, d.Reason)
	}
	if d.NodeID != 1 {
		t.Fatalf("选中 %d，期望绑定节点 1（不得因评分更低而改绑）", d.NodeID)
	}
}

// TestNoSilentRebind_WhenBoundNodeUnhealthy 是 B1 规格 §9 的 R-3 行（纯函数形态）。
//
// 判据：绑定节点不健康时，**绝不**产出 DecScored 去选别的节点。
// 把 pickBound 改成 fallback 到评分 ⇒ 这条会红（Kind 变 DecScored、NodeID 变 2）。
func TestNoSilentRebind_WhenBoundNodeUnhealthy(t *testing.T) {
	unhealthy := healthy(1)
	unhealthy.State = "offline"
	other := healthy(2)

	bn := int64(1)
	in := baseInput(unhealthy, other)
	in.Group = &GroupRef{GroupID: 7, BoundNodeID: &bn, GroupState: "active"}

	d := Pick(in)
	if d.Kind == DecScored {
		t.Fatalf("绑定节点不健康时产出了 DecScored(node=%d) —— 这是静默改绑", d.NodeID)
	}
	if d.NodeID == 2 {
		t.Fatal("选中了非绑定节点 2 —— 违反「禁止静默改绑」")
	}
	if d.Kind != DecBindingInvalid {
		t.Fatalf("期望 DecBindingInvalid（交给 B2 的接口），实得 %v", d.Kind)
	}
}

// TestBoundCapacityWaits 守「绑定节点满容 ⇒ 组内排队，不拆分」（默认 split_policy=none）。
func TestBoundCapacityWaits(t *testing.T) {
	bound := healthy(1)
	bound.Active = 3 // = max_concurrent
	other := healthy(2)

	bn := int64(1)
	in := baseInput(bound, other)
	in.Group = &GroupRef{GroupID: 7, BoundNodeID: &bn, GroupState: "active", SplitPolicy: "none"}

	d := Pick(in)
	if d.Kind != DecAffinityWait {
		t.Fatalf("期望 DecAffinityWait，实得 %v(%s)", d.Kind, d.Reason)
	}
	if d.Reason != "bound_capacity" {
		t.Fatalf("原因 = %q，期望 bound_capacity", d.Reason)
	}
	if d.NodeID != 1 {
		t.Fatalf("排队时应指向绑定节点（解释用），实得 %d", d.NodeID)
	}
}

// TestGroupConcurrencyCapWaits 守 C-12：组内并发上限也要判。
// 不看这一条，24 集会一起涌向同一节点把它拖垮（INV-A7）。
func TestGroupConcurrencyCapWaits(t *testing.T) {
	bound := healthy(1) // 节点还有余量
	bn := int64(1)
	in := baseInput(bound)
	in.Group = &GroupRef{
		GroupID: 7, BoundNodeID: &bn, GroupState: "active",
		MaxConcurrentInGroup: 2, ActiveInGroup: 2,
	}
	d := Pick(in)
	if d.Kind != DecAffinityWait || d.Reason != "group_capacity" {
		t.Fatalf("期望 group_capacity 排队，实得 %v(%s)", d.Kind, d.Reason)
	}
}

// TestReservedCountsTowardCapacity 守 reserved_pending 参与容量判断：
// 事务已提交但节点还没确认的那一小段时间，任务**必须**占位；
// 否则同一 tick 里连投 N 个任务会把节点打过载。
func TestReservedCountsTowardCapacity(t *testing.T) {
	bound := healthy(1)
	bound.Active = 2
	bound.MaxConcurrent = 3
	bn := int64(1)
	in := baseInput(bound)
	in.Group = &GroupRef{GroupID: 7, BoundNodeID: &bn, GroupState: "active"}
	in.Reserved = map[int64]int64{1: 1} // 2 + 1 >= 3

	d := Pick(in)
	if d.Kind != DecAffinityWait {
		t.Fatalf("预留占位未计入容量，期望排队，实得 %v(%s)", d.Kind, d.Reason)
	}
}

// TestArgMinTieBreak 守可复现性：分数相同时按 (active↑, last_assigned↑, node_id↑)。
func TestArgMinTieBreak(t *testing.T) {
	a := healthy(9)
	b := healthy(3)
	in := baseInput(a, b)

	// 分数完全相同、active 相同 ⇒ 取 node_id 小的
	got, ok := ArgMin([]Candidate{a, b}, in)
	if !ok || got.NodeID != 3 {
		t.Fatalf("tie-break 应按 node_id 取 3，实得 %d", got.NodeID)
	}

	// active 更小的优先于 node_id
	c := healthy(3)
	c.Active = 1
	got, _ = ArgMin([]Candidate{c, healthy(9)}, in)
	if got.NodeID != 9 {
		t.Fatalf("active 更小的 9 应胜出，实得 %d", got.NodeID)
	}
}

// TestScoreFormula 手工复算一次评分，防止"公式被悄悄改号/改权重"。
func TestScoreFormula(t *testing.T) {
	n := Candidate{
		NodeID: 1, Enabled: true, State: "online", Weight: 1.0,
		MaxConcurrent: 4, Active: 2, PendingBytes: 1e9,
	}
	in := baseInput(n)
	in.Cfg = DefaultPolicy()

	s, parts := Score(n, in)
	wantLoad := 1.0*2/1.0 + 1e-9*1e9/1.0 // 2 + 1 = 3
	wantConn := 0.5                      // 2/4
	want := 1.00*wantLoad + 0.50*wantConn + 2.00*0 - 0.75*0 + 0.10*0

	if !almostEq(parts.Load, wantLoad) {
		t.Errorf("load = %v，期望 %v", parts.Load, wantLoad)
	}
	if !almostEq(parts.Conn, wantConn) {
		t.Errorf("conn = %v，期望 %v", parts.Conn, wantConn)
	}
	if !almostEq(s, want) {
		t.Errorf("score = %v，期望 %v", s, want)
	}
}

// TestSpreadPenaltyWindow 守 C-03 的 1 秒窗口。
func TestSpreadPenaltyWindow(t *testing.T) {
	n := healthy(1)
	in := baseInput(n)
	n.LastAssignedAt = in.Now
	if got := SpreadPenalty(n, in); got != 1.0 {
		t.Fatalf("刚被选中应罚 1.0，实得 %v", got)
	}
	n.LastAssignedAt = in.Now - 1
	if got := SpreadPenalty(n, in); got != 0 {
		t.Fatalf("1 秒后应无罚，实得 %v", got)
	}
}

func almostEq(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
