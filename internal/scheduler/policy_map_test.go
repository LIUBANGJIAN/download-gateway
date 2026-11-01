package scheduler

import (
	"math"
	"testing"
)

// TestApplyOverridesEmptyIsDefault 零覆盖必须与默认策略**逐字段相等**。
//
// 这是整条接线的安全底线：库里没有任何 policy.* 行时，调度结果必须与 B1
// 验收时一模一样，否则「接线」就等于偷偷改了已经验过的行为。
func TestApplyOverridesEmptyIsDefault(t *testing.T) {
	got, applied := ApplyOverrides(DefaultPolicy(), nil)
	if len(applied) != 0 {
		t.Fatalf("空覆盖不该报告任何生效键，得到 %v", applied)
	}
	if got != DefaultPolicy() {
		t.Fatalf("空覆盖改变了策略：\n got=%+v\nwant=%+v", got, DefaultPolicy())
	}
}

// TestApplyOverridesOnlyTouchesGivenKeys 只处理传进来的键，没传的保持原样。
func TestApplyOverridesOnlyTouchesGivenKeys(t *testing.T) {
	base := DefaultPolicy()
	ov := map[string]PolicyOverride{
		"least_tasks": {Enabled: false, Priority: 30}, // 默认优先级 ⇒ 不缩放
	}
	got, applied := ApplyOverrides(base, ov)

	if len(applied) != 1 || applied[0] != "least_tasks" {
		t.Fatalf("生效键 = %v，期望只有 least_tasks", applied)
	}
	if got.Params.Alpha != 0 {
		t.Fatalf("least_tasks 关闭后 Alpha = %v，期望 0", got.Params.Alpha)
	}
	if got.Weights.W1 != base.Weights.W1 {
		t.Fatalf("默认优先级不该缩放 W1：got=%v want=%v", got.Weights.W1, base.Weights.W1)
	}
	// 其余字段一个都不能动。
	if got.Weights.W2 != base.Weights.W2 || got.Weights.W4 != base.Weights.W4 ||
		got.Params.Beta != base.Params.Beta || got.MaxConcurrentInGroup != base.MaxConcurrentInGroup {
		t.Fatalf("未传的策略被改动：got=%+v", got)
	}
}

// TestPriorityScalesWeight 优先级越高（数值越小）权重越大。
func TestPriorityScalesWeight(t *testing.T) {
	cases := []struct {
		name     string
		priority int64
		wantW1   float64
	}{
		{"默认 30 不缩放", 30, 1.00},
		{"提到 15 权重翻倍", 15, 2.00},
		{"降到 60 权重减半", 60, 0.50},
		{"极端 1 → 夹到上限 10", 1, 10.00},
		{"极端 999 → 夹到下限 0.1", 999, 0.10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := ApplyOverrides(DefaultPolicy(), map[string]PolicyOverride{
				"least_tasks": {Enabled: true, Priority: c.priority},
			})
			if math.Abs(got.Weights.W1-c.wantW1) > 1e-9 {
				t.Fatalf("priority=%d ⇒ W1=%v，期望 %v", c.priority, got.Weights.W1, c.wantW1)
			}
		})
	}
}

// TestApplyOverridesSwitches 逐条验证 8 个已接线的开关。
func TestApplyOverridesSwitches(t *testing.T) {
	cases := []struct {
		key   string
		check func(t *testing.T, p Policy)
	}{
		{"max_concurrent", func(t *testing.T, p Policy) {
			if !p.DisableNodeCapacity {
				t.Fatal("关闭 max_concurrent 应置 DisableNodeCapacity")
			}
		}},
		{"max_concurrent_in_group", func(t *testing.T, p Policy) {
			if !p.DisableGroupCapacity {
				t.Fatal("关闭 max_concurrent_in_group 应置 DisableGroupCapacity")
			}
		}},
		{"node_weight", func(t *testing.T, p Policy) {
			if !p.IgnoreNodeWeight {
				t.Fatal("关闭 node_weight 应置 IgnoreNodeWeight")
			}
		}},
		{"source_pinned", func(t *testing.T, p Policy) {
			if !p.DisableSourcePinned {
				t.Fatal("关闭 source_pinned 应置 DisableSourcePinned")
			}
		}},
		{"role_routing", func(t *testing.T, p Policy) {
			if !p.DisableRoleRouting {
				t.Fatal("关闭 role_routing 应置 DisableRoleRouting")
			}
		}},
		{"least_tasks", func(t *testing.T, p Policy) {
			if p.Params.Alpha != 0 {
				t.Fatalf("关闭 least_tasks 应把 Alpha 置 0，得到 %v", p.Params.Alpha)
			}
		}},
		{"weighted_least_tasks", func(t *testing.T, p Policy) {
			if p.Params.Beta != 0 {
				t.Fatalf("关闭 weighted_least_tasks 应把 Beta 置 0，得到 %v", p.Params.Beta)
			}
		}},
		{"source_affinity", func(t *testing.T, p Policy) {
			if p.Weights.W4 != 0 {
				t.Fatalf("关闭 source_affinity 应把 W4 置 0，得到 %v", p.Weights.W4)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			meta, ok := PolicyKeySpec(c.key)
			if !ok {
				t.Fatalf("%s 不在 SupportedPolicyKeys 里", c.key)
			}
			got, applied := ApplyOverrides(DefaultPolicy(), map[string]PolicyOverride{
				c.key: {Enabled: false, Priority: meta.BasePriority},
			})
			if len(applied) != 1 || applied[0] != c.key {
				t.Fatalf("生效键 = %v，期望只有 %s", applied, c.key)
			}
			c.check(t, got)
		})
	}
}

// TestUnknownKeyIsIgnored 未知键必须被**明确忽略**，而不是悄悄当成「已生效」。
//
// B1.5 收尾后 12 条 catalog 策略**全部**被内核消费了，所以这里不再列举
// "已知但未接"的键，而是用一个人为的假键守住同一条性质：
// 将来若有人往库里塞一个内核不认识的 `policy.xxx`，它既不能改策略、
// 也不能被报告为"已生效"（否则管理台会替它撒谎）。
func TestUnknownKeyIsIgnored(t *testing.T) {
	const k = "not_a_real_policy"
	if _, ok := PolicyKeySpec(k); ok {
		t.Fatalf("%s 不该出现在 SupportedPolicyKeys 里", k)
	}
	base := DefaultPolicy()
	got, applied := ApplyOverrides(base, map[string]PolicyOverride{
		k: {Enabled: false, Priority: 1},
	})
	if len(applied) != 0 {
		t.Fatalf("未知键 %s 不该被报告为生效，得到 %v", k, applied)
	}
	if got != base {
		t.Fatalf("未知键 %s 不该改动策略", k)
	}
}

// TestEveryCatalogKeyIsWired 守"新增策略必须同时接线"。
//
// 判据：管理台 catalog 的 12 个键，逐个必须出现在支持表里。
// 少接一条 ⇒ 那条在界面上会显示"未生效"，而用户读到的原因本该是"还没做"——
// 与其让界面解释，不如让这条断言红在开发者的机器上。
func TestEveryCatalogKeyIsWired(t *testing.T) {
	want := []string{
		"max_concurrent", "group_affinity", "least_tasks", "retry_backoff",
		"throttle_backoff", "node_weight", "max_concurrent_in_group",
		"weighted_least_tasks", "source_affinity", "source_pinned",
		"role_routing", "node_rate_limit",
	}
	if len(want) != 12 {
		t.Fatalf("catalog 应有 12 条，这里列了 %d 条", len(want))
	}
	for _, k := range want {
		if _, ok := PolicyKeySpec(k); !ok {
			t.Errorf("%s 未接进内核（PolicyKeySpec 不认）", k)
		}
	}
	if got := len(SupportedPolicyKeys()); got != 12 {
		t.Fatalf("支持表应有 12 条，实得 %d：%v", got, SupportedPolicyKeys())
	}
}

// TestNodeWeightOverrideFlipsPick 是端到端证伪：关掉 node_weight 后，
// 同一组候选的选点结果必须**翻转**。
//
// 场景：节点 1 权重 10 但手上有 5 个任务；节点 2 权重 1 只有 1 个任务。
//
//	· 看权重（默认）⇒ 1 的折算负载 5/10=0.5，比 2 的 1/1=1.0 低 ⇒ 选 1
//	· 不看权重（IgnoreNodeWeight）⇒ 1 的负载 5，比 2 的 1 高 ⇒ 选 2
func TestNodeWeightOverrideFlipsPick(t *testing.T) {
	nodes := []Candidate{
		{NodeID: 1, Alias: "big", Enabled: true, State: "online", Weight: 10, MaxConcurrent: 20, Active: 5},
		{NodeID: 2, Alias: "small", Enabled: true, State: "online", Weight: 1, MaxConcurrent: 20, Active: 1},
	}
	in := PickInput{Nodes: nodes, Now: 1000, Cfg: DefaultPolicy()}

	d := Pick(in)
	if d.Kind != DecScored || d.NodeID != 1 {
		t.Fatalf("按权重应选节点 1，得到 kind=%s node=%d", d.Kind, d.NodeID)
	}

	in.Cfg.IgnoreNodeWeight = true
	d = Pick(in)
	if d.Kind != DecScored || d.NodeID != 2 {
		t.Fatalf("忽略权重后应选节点 2，得到 kind=%s node=%d", d.Kind, d.NodeID)
	}
}

// TestDisableNodeCapacityLetsFullNodeBePicked 关闭节点并发上限后，
// 已经满容的节点重新进入候选。
func TestDisableNodeCapacityLetsFullNodeBePicked(t *testing.T) {
	nodes := []Candidate{
		{NodeID: 1, Enabled: true, State: "online", Weight: 1, MaxConcurrent: 2, Active: 2},
	}
	in := PickInput{Nodes: nodes, Now: 1000, Cfg: DefaultPolicy()}

	if d := Pick(in); d.Kind != DecAffinityWait || d.Reason != "no_candidate" {
		t.Fatalf("满容节点应被 C4 淘汰并排队，得到 kind=%s reason=%s", d.Kind, d.Reason)
	}

	in.Cfg.DisableNodeCapacity = true
	if d := Pick(in); d.Kind != DecScored || d.NodeID != 1 {
		t.Fatalf("关闭 C4 后应选中满容节点，得到 kind=%s node=%d", d.Kind, d.NodeID)
	}
}

// TestDisableGroupCapacityLetsGroupExceedLimit 关闭组内并发上限后，
// 已绑定节点上的组不再因组容量排队。
func TestDisableGroupCapacityLetsGroupExceedLimit(t *testing.T) {
	bound := int64(1)
	in := PickInput{
		Nodes: []Candidate{{NodeID: 1, Enabled: true, State: "online", Weight: 1, MaxConcurrent: 10, Active: 1}},
		Group: &GroupRef{BoundNodeID: &bound, MaxConcurrentInGroup: 2, ActiveInGroup: 5},
		Now:   1000,
		Cfg:   DefaultPolicy(),
	}

	if d := Pick(in); d.Kind != DecAffinityWait || d.Reason != "group_capacity" {
		t.Fatalf("组满容应排队，得到 kind=%s reason=%s", d.Kind, d.Reason)
	}

	in.Cfg.DisableGroupCapacity = true
	if d := Pick(in); d.Kind != DecAffinityHit {
		t.Fatalf("关闭组容量后应命中亲和，得到 kind=%s", d.Kind)
	}
}

// TestDisableSourcePinnedWidensCandidates 关闭来源钉住后，C5 不再收窄候选。
func TestDisableSourcePinnedWidensCandidates(t *testing.T) {
	pinned := int64(9) // 钉在一个不存在的节点上
	nodes := []Candidate{
		{NodeID: 1, Enabled: true, State: "online", Weight: 1, MaxConcurrent: 10},
	}
	in := PickInput{
		Nodes: nodes, SourceStrategy: "pinned", PinnedNodeID: &pinned,
		Now: 1000, Cfg: DefaultPolicy(),
	}

	if d := Pick(in); d.Kind != DecAffinityWait {
		t.Fatalf("C5 生效时应无候选，得到 kind=%s", d.Kind)
	}

	in.Cfg.DisableSourcePinned = true
	if d := Pick(in); d.Kind != DecScored || d.NodeID != 1 {
		t.Fatalf("关闭 C5 后应选中节点 1，得到 kind=%s node=%d", d.Kind, d.NodeID)
	}
}

// TestThreeRuntimePoliciesMapToFields 守 4 条"运行时策略"的映射（B1.5 收尾）。
//
// 它们与前面几条的区别：不是权重项，也不是硬约束开关，而是**投递层参数**。
// 断言具体数值而不是"变了"，因为数值本身是用户会看到的行为差异
// （关掉退避后间隔从 30s·2^n 变成恒定 30s）。
func TestThreeRuntimePoliciesMapToFields(t *testing.T) {
	base := DefaultPolicy()

	// group_affinity 关闭 ⇒ 只翻 DisableGroupAffinity 一个字段。
	got, _ := ApplyOverrides(base, map[string]PolicyOverride{
		"group_affinity": {Enabled: false, Priority: 20},
	})
	if !got.DisableGroupAffinity {
		t.Fatal("关闭 group_affinity 应置 DisableGroupAffinity")
	}
	if got.Weights != base.Weights {
		t.Fatal("group_affinity 不该动权重")
	}

	// retry_backoff 关闭 ⇒ base 与 cap 相等（固定间隔），且仍 >0（不是不重试）。
	got, _ = ApplyOverrides(base, map[string]PolicyOverride{
		"retry_backoff": {Enabled: false, Priority: 40},
	})
	if got.RetryBaseSec <= 0 || got.RetryBaseSec != got.RetryCapSec {
		t.Fatalf("关闭 retry_backoff 应得固定间隔，实际 base=%d cap=%d",
			got.RetryBaseSec, got.RetryCapSec)
	}

	// 优先级 20（比默认 40 更"高"）⇒ 退避缩短（scale=2 ⇒ base=15）。
	got, _ = ApplyOverrides(base, map[string]PolicyOverride{
		"retry_backoff": {Enabled: true, Priority: 20},
	})
	if got.RetryBaseSec != 15 {
		t.Fatalf("优先级 40→20 应把 base 从 30s 缩到 15s，实际 %d", got.RetryBaseSec)
	}

	// throttle_backoff 关闭 ⇒ 倍率恒 1.0。
	got, _ = ApplyOverrides(base, map[string]PolicyOverride{
		"throttle_backoff": {Enabled: false, Priority: 50},
	})
	if got.ThrottleMultiplier != 1.0 {
		t.Fatalf("关闭 throttle_backoff 应是 1.0 倍率，实际 %v", got.ThrottleMultiplier)
	}
	// 开启且优先级更高 ⇒ 冷却更短（<1）。
	got, _ = ApplyOverrides(base, map[string]PolicyOverride{
		"throttle_backoff": {Enabled: true, Priority: 25},
	})
	if !(got.ThrottleMultiplier > 0 && got.ThrottleMultiplier < 1) {
		t.Fatalf("高优先级应缩短限流冷却（倍率 <1），实际 %v", got.ThrottleMultiplier)
	}

	// node_rate_limit 关闭 ⇒ 0（不限速）；默认开启优先级 ⇒ 30/min。
	got, _ = ApplyOverrides(base, map[string]PolicyOverride{
		"node_rate_limit": {Enabled: false, Priority: 120},
	})
	if got.NodeRateLimitPerMin != 0 {
		t.Fatalf("关闭 node_rate_limit 应不限速，实际 %d", got.NodeRateLimitPerMin)
	}
	got, _ = ApplyOverrides(base, map[string]PolicyOverride{
		"node_rate_limit": {Enabled: true, Priority: 60},
	})
	if got.NodeRateLimitPerMin != 60 {
		t.Fatalf("优先级 120→60 应把速率从 30 提到 60，实际 %d", got.NodeRateLimitPerMin)
	}
}
