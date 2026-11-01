package scheduler

// 本文件是「管理台派发策略」→「选点内核参数」的**唯一映射表**。
//
// # 为什么需要它
//
// 管理台的「设置」页把 12 条策略存进了 `app_config`（`policy.<键>`），但内核读的是
// 编译期默认的 `DefaultPolicy()` —— 于是那些开关与优先级**存下去没有任何组件消费**。
// 用户调完策略发现下载分布毫无变化，会去查一个不存在的调度 bug。
//
// # 设计取舍：只应用「用户改过」的键
//
// `ApplyOverrides` 只遍历传入的 override 映射。调用方（`engine`）只把库里**显式覆盖**
// 过的行放进来，因此：
//
//	零配置 ⇒ 返回的 Policy 与 DefaultPolicy() 逐字段相等 ⇒ 调度结果与 B1 验收完全一致
//
// 这很重要。如果按「catalog 默认值」去推导，`source_affinity`（catalog 默认关）会把
// W4 归零、`weighted_least_tasks`（catalog 默认关）会把 Beta 归零，等于悄悄改掉了
// B1 已经验收过的评分公式。默认值归默认值，改动归改动，两者不混。
//
// # 优先级怎么变成权重
//
// 管理台的 `priority` 是 1..999，**数值越小优先级越高**。内核的权重是正实数，
// 两者用「以默认优先级为 1 的比值」对齐：
//
//	scale = basePriority / currentPriority
//
// 于是 priority 等于 catalog 默认值 ⇒ scale=1（权重不变）；把一条策略从 30 调到 15
// ⇒ scale=2（该项影响力翻倍）。比值夹在 [0.1, 10]，避免一条极端优先级把其余项彻底压死。

// PolicyOverride 是用户在管理台对一条策略的改动（对应 `app_config` 的
// `policy.<键> = {"enabled":bool,"priority":int}`）。
type PolicyOverride struct {
	Enabled  bool
	Priority int64
}

// PolicyKeyMeta 描述一条策略映射到内核的哪个环节。
//
// `BasePriority` 必须与 `admin.catalog` 里的 `DefaultPriority` 一致 —— 它是权重缩放的
// 基准，写歪了会让「没改过的策略」也产生缩放。两边的一致性由
// `TestPolicyMetaMatchesAdminCatalog` 站在 `engine` 侧交叉断言。
type PolicyKeyMeta struct {
	Key          string
	BasePriority int64
	// Drives 一句话说明作用于哪个决策环节，直接展示给用户。
	Drives string
}

// supportedPolicyKeys 是内核**真正会读**的策略键，顺序与管理台 catalog 一致。
//
// 不在表里的键（`group_affinity` / `retry_backoff` / `throttle_backoff` /
// `node_rate_limit`）改动后不生效，管理台必须对它们显示 `effective:false` 与具体原因。
var supportedPolicyKeys = []PolicyKeyMeta{
	{"max_concurrent", 10, "C4 节点并发上限（安全阀）"},
	{"group_affinity", 20, "阶段 0 亲和短路（关闭即显式拆组）"},
	{"least_tasks", 30, "软评分 w1：活跃任务数"},
	{"retry_backoff", 40, "投递失败的重试退避基准与上限"},
	{"throttle_backoff", 50, "限流冷却的额外倍率"},
	{"node_weight", 60, "软评分 w1：按节点权重折算负载"},
	{"max_concurrent_in_group", 70, "组内并发上限"},
	{"weighted_least_tasks", 80, "软评分 w1：在途字节项"},
	{"source_affinity", 90, "软评分 w4：来源加分"},
	{"source_pinned", 100, "C5 来源钉住节点"},
	{"role_routing", 110, "软评分 w4：节点角色加分"},
	{"node_rate_limit", 120, "C6 按节点限速（每分钟下发上限）"},
}

// SupportedPolicyKeys 返回内核会读取的策略键（供管理台判定 `effective`）。
func SupportedPolicyKeys() []string {
	out := make([]string, len(supportedPolicyKeys))
	for i, m := range supportedPolicyKeys {
		out[i] = m.Key
	}
	return out
}

// PolicyKeySpec 返回某条策略的映射说明；第二个返回值为 false 表示内核不消费这条策略。
func PolicyKeySpec(key string) (PolicyKeyMeta, bool) {
	for _, m := range supportedPolicyKeys {
		if m.Key == key {
			return m, true
		}
	}
	return PolicyKeyMeta{}, false
}

// ApplyOverrides 把用户改动叠加到基准策略上，返回新策略**以及实际生效的键**。
//
// 纯函数：不读库、不读时钟。调用方负责从 `app_config` 取出覆盖行。
func ApplyOverrides(base Policy, ov map[string]PolicyOverride) (Policy, []string) {
	p := base
	applied := make([]string, 0, len(ov))
	for _, m := range supportedPolicyKeys {
		o, ok := ov[m.Key]
		if !ok {
			continue
		}
		applied = append(applied, m.Key)
		p = applyOne(p, m, o)
	}
	return p, applied
}

// applyOne 把单条策略落到具体字段上。
func applyOne(p Policy, m PolicyKeyMeta, o PolicyOverride) Policy {
	scale := priorityScale(m.BasePriority, o.Priority)
	switch m.Key {
	case "max_concurrent":
		p.DisableNodeCapacity = !o.Enabled
		p.Weights.W2 *= scale

	case "max_concurrent_in_group":
		p.DisableGroupCapacity = !o.Enabled

	case "least_tasks":
		if !o.Enabled {
			p.Params.Alpha = 0
		}
		p.Weights.W1 *= scale

	case "weighted_least_tasks":
		if !o.Enabled {
			p.Params.Beta = 0
		}

	case "node_weight":
		p.IgnoreNodeWeight = !o.Enabled

	case "source_affinity":
		if !o.Enabled {
			p.Weights.W4 = 0
		}
		p.Weights.W4 *= scale

	case "source_pinned":
		p.DisableSourcePinned = !o.Enabled

	case "role_routing":
		p.DisableRoleRouting = !o.Enabled

	case "group_affinity":
		// 关闭 ⇒ 显式拆组（写 `group_binding_log`），而不是"静默直接用评分选别的节点"。
		p.DisableGroupAffinity = !o.Enabled

	case "retry_backoff":
		// 关闭 ⇒ 退回固定间隔（base 与 cap 相等）。**不是**不重试：
		// 不重试等于失败即丢弃，那是数据丢失，不能做成一个开关。
		base := defaultRetryBaseSec
		capSec := int64(defaultRetryCapSec)
		if o.Enabled {
			// 优先级越高（数值越小）⇒ scale 越大 ⇒ 退避越短（更愿意快速重试）。
			base = scaleSec(base, 1/scale)
			capSec = scaleSec(capSec, 1/scale)
		} else {
			capSec = base
		}
		p.RetryBaseSec = base
		p.RetryCapSec = capSec

	case "throttle_backoff":
		// 关闭 ⇒ 倍率 1.0（只等节点自己给的 throttle_until，不额外加码）。
		mul := 1.0
		if o.Enabled {
			mul = 1 / scale // 优先级越高 ⇒ 冷却越短
		}
		p.ThrottleMultiplier = clampMul(mul)

	case "node_rate_limit":
		// 关闭 ⇒ 不限速（0）。
		perMin := int64(0)
		if o.Enabled {
			perMin = scaleRate(defaultNodeRatePerMin, scale)
		}
		p.NodeRateLimitPerMin = perMin
	}
	return p
}

// 三策略的默认参数（与设计默认值一致；写在这里是因为它们是**内核**的默认，
// 不是管理台 catalog 的默认 —— 后者只描述"开关默认开还是关"）。
const (
	defaultRetryBaseSec   int64 = 30
	defaultRetryCapSec    int64 = 1800
	defaultNodeRatePerMin int64 = 30
	minNodeRatePerMin     int64 = 1
	maxNodeRatePerMin     int64 = 600
	minBackoffSec         int64 = 1
	maxBackoffSec         int64 = 86400
)

// scaleSec 按倍数缩放秒数并夹到合理区间（防 0 与天文数字）。
func scaleSec(v int64, scale float64) int64 {
	if scale <= 0 {
		scale = 1
	}
	out := int64(float64(v) * scale)
	switch {
	case out < minBackoffSec:
		return minBackoffSec
	case out > maxBackoffSec:
		return maxBackoffSec
	default:
		return out
	}
}

// scaleRate 按倍数缩放每分钟速率并夹到 [1, 600]。
//
// 上限 600/min 的理由：再高就等于"不限速"，那不如直接关掉这条策略 ——
// 留一个假的可调范围比不留更糟。
func scaleRate(base int64, scale float64) int64 {
	if scale <= 0 {
		scale = 1
	}
	out := int64(float64(base) * scale)
	switch {
	case out < minNodeRatePerMin:
		return minNodeRatePerMin
	case out > maxNodeRatePerMin:
		return maxNodeRatePerMin
	default:
		return out
	}
}

// clampMul 把限流倍率夹到 [0.1, 10]（与权重缩放同一个量级上界）。
func clampMul(v float64) float64 {
	switch {
	case v < 0.1:
		return 0.1
	case v > 10:
		return 10
	default:
		return v
	}
}

// priorityScale 把 1..999 的优先级换成以默认值为 1 的倍数。
//
// 非法输入（0 / 负数）一律返回 1：宁可「不缩放」也不要把权重变成 NaN，
// 那会让 `ArgMin` 在全部候选上得到 NaN，选点集体失效。
func priorityScale(base, cur int64) float64 {
	if base <= 0 || cur <= 0 {
		return 1
	}
	s := float64(base) / float64(cur)
	const lo, hi = 0.1, 10.0
	switch {
	case s < lo:
		return lo
	case s > hi:
		return hi
	default:
		return s
	}
}
