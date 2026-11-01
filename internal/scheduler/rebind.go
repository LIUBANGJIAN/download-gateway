package scheduler

import "fmt"

// 本文件是 B2「已建立的绑定如何失效与重绑」的**纯决策**部分。
//
// # 为什么失效处置必须是纯函数
//
// B2-AC1 要求「每次重绑/拆组/rejoin/释放，`group_binding_log` 新增一行且 `binding_epoch` 递增」。
// 把「该不该改绑」混进写库代码里，判据就无法在不碰数据库的情况下被证伪 ——
// 而恰恰是「什么条件下允许改绑」这一半最容易出错（strong 未到期就改 = 静默改绑）。
//
// 所以这里只回答一个问题：**给定组、绑定节点、策略与时刻，该怎么处置**。
// 执行（写 `task_group` / `group_binding_log` / `event_log`）在 `internal/engine/rebind.go`。

// BindingAction 是绑定失效后的处置动作。
type BindingAction int

const (
	// BindHold 绑定仍然有效（或当前状态不需要处置），调用方按正常亲和短路走。
	BindHold BindingAction = iota
	// BindWait 失效但按策略**等待**：任务排队、`group_state` 不变、必须有 reason。
	BindWait
	// BindSplit 拆组：`group_state→split`，组内**新**成员改走评分另选节点。
	// 已落点的成员**不迁移**（`design/02 §2.1.2`：requeue 而非迁移，不删已下载内容）。
	BindSplit
	// BindRejoin 追平：组处于 split 且原绑定节点已恢复健康 ⇒ 新成员回到原节点。
	//
	// 只对**后续新成员**生效。已经分散出去的成员留在原处 —— 迁移它们等于重新下载，
	// 而"下载中跨节点热迁移"是 BitComet 明确不支持的能力（设计 §6 第 ⑦ 条）。
	BindRejoin
)

// String 便于日志与 `dispatch_log.reason` 可读。
func (a BindingAction) String() string {
	switch a {
	case BindHold:
		return "hold"
	case BindWait:
		return "wait"
	case BindSplit:
		return "split"
	case BindRejoin:
		return "rejoin"
	default:
		return "unknown"
	}
}

// BindingResolution 是处置结论。
type BindingResolution struct {
	Action BindingAction
	// Reason 写 `dispatch_log.reason` / `group_binding_log.reason`（机器可读，snake_case）。
	Reason string
	// Note 人读说明，写 `event_log.message`（用户要能看懂"为什么这集没下发"）。
	Note string
	// Deadline >0 时调用方应把它写进 `task_group.wait_deadline`（strong 的宽限起点）。
	Deadline int64
	// Alert 表示"这是本次失效的第一次发现"，调用方应写一条告警。
	//
	// 去重机制：靠 `wait_deadline` 是否已设。没设 ⇒ 首次 ⇒ 告警并记住；
	// 已设 ⇒ 后续 tick ⇒ 静默等待。否则一个离线节点会让 `event_log` 每秒钟多一行，
	// 真正的告警会被淹掉（`design/02 §2.1.2` 明确要求"一次性、去重"）。
	Alert bool
}

// 组状态取值（与 DDL 的 CHECK 一致）。
const (
	GroupStateUnbound  = "unbound"
	GroupStateActive   = "active"
	GroupStateSplit    = "split"
	GroupStateDraining = "draining"
	GroupStateClosed   = "closed"
)

// 失效原因（拼进 reason 后缀，便于按原因聚合排障）。
const (
	InvalidNodeMissing  = "node_missing"
	InvalidNodeDisabled = "node_disabled"
	InvalidNodeOffline  = "node_offline"
	InvalidThrottled    = "node_throttled"
)

// DefaultBindingGraceSec 是 strong 策略的默认宽限时长（5 分钟）。
//
// 取 5 分钟的理由：短于一个典型健康探测周期（默认 30s）的 10 倍会让"节点抖一下"就拆组，
// 长于 30 分钟则等于"没有失效路径"（用户看到整组卡死半小时）。
const DefaultBindingGraceSec int64 = 300

// NodeHealthy 判断绑定节点是否可用于继续喂给该组。
//
// 注意判据是**能下发**而不是"TCP 通"：C1/C2/C3 任一不满足都算不可用，
// 因为此时把新任务投过去只会立刻失败。
func NodeHealthy(n *Candidate, now int64) (bool, string) {
	if n == nil {
		return false, InvalidNodeMissing
	}
	if !n.Enabled {
		return false, InvalidNodeDisabled
	}
	if n.State != "online" {
		return false, InvalidNodeOffline
	}
	if now < n.ThrottleUntil {
		return false, InvalidThrottled
	}
	return true, ""
}

// ResolveBinding 判定绑定组的处置动作。
//
// 分流规则严格按 `design/02 §2.1.2` + `design/11 §3.6` 的 B2：
//
//	manual  ⇒ 永不自动改绑，只等待 + 告警（人不管，就停在那儿 —— 这是刻意的）
//	weak    ⇒ 绑定节点一不可用就立即拆组（新成员分散，旧的留原处）
//	strong  ⇒ 先等 `wait_deadline`（首次失效时把 deadline 设为 now+grace），到期才拆组
//	容量满  ⇒ `split_policy=capacity` 拆组，否则排队（拆了也一样是同一个组在抢并发）
//
// `in.Group` 必须是已绑定的组（`BoundNodeID != nil`）；否则返回 BindHold。
func ResolveBinding(in PickInput) BindingResolution {
	g := in.Group
	if g == nil || g.BoundNodeID == nil {
		return BindingResolution{Action: BindHold, Reason: "unbound"}
	}

	// 已拆的组：只在原节点恢复时追平，不做别的处置。
	if g.GroupState == GroupStateSplit {
		if ok, _ := NodeHealthy(in.BoundNode, in.Now); ok && !nodeSaturated(in.BoundNode, in) {
			return BindingResolution{
				Action: BindRejoin,
				Reason: "rejoin_bound_node_recovered",
				Note: fmt.Sprintf("组 #%d 的原绑定节点 #%d 已恢复，新成员回到该节点",
					g.GroupID, *g.BoundNodeID),
			}
		}
		return BindingResolution{Action: BindHold, Reason: "split"}
	}
	if g.GroupState == GroupStateDraining || g.GroupState == GroupStateClosed {
		// 人工介入过的组（下线/关闭）不自动改绑：那是运维的决定，不是调度能推翻的。
		return BindingResolution{Action: BindHold, Reason: "group_" + g.GroupState}
	}

	// `policy.group_affinity` 关闭 ⇒ 不做组亲和。这里**必须**走拆组而不是"直接用评分"：
	// 后者等于静默改绑（组还绑着 A，任务却落 B，日志里没有一行解释）。
	if in.Cfg.DisableGroupAffinity {
		return BindingResolution{
			Action: BindSplit,
			Reason: "policy_group_affinity_off",
			Note:   fmt.Sprintf("派发策略「组亲和」已关闭，组 #%d 解除绑定（新成员按负载分散）", g.GroupID),
		}
	}

	if ok, why := NodeHealthy(in.BoundNode, in.Now); !ok {
		return resolveInvalid(in, g, why)
	}

	// 健康但满容：这是"暂时喂不进去"，不是"绑定失效"。
	if !in.Cfg.DisableNodeCapacity && nodeSaturated(in.BoundNode, in) {
		if g.SplitPolicy == "capacity" || in.Cfg.SplitPolicy == "capacity" {
			return BindingResolution{
				Action: BindSplit,
				Reason: "bound_capacity_split",
				Note: fmt.Sprintf("组 #%d 的绑定节点 #%d 已满容且 split_policy=capacity，允许拆分到其他节点",
					g.GroupID, *g.BoundNodeID),
			}
		}
		return BindingResolution{
			Action: BindWait,
			Reason: "bound_capacity",
			Note: fmt.Sprintf("组 #%d 的绑定节点 #%d 已满容（split_policy=none），等待空位",
				g.GroupID, *g.BoundNodeID),
		}
	}
	if !in.Cfg.DisableGroupCapacity && g.MaxConcurrentInGroup > 0 && g.ActiveInGroup >= g.MaxConcurrentInGroup {
		return BindingResolution{
			Action: BindWait,
			Reason: "group_capacity",
			Note: fmt.Sprintf("组 #%d 在途已达组内上限 %d，等待空位",
				g.GroupID, g.MaxConcurrentInGroup),
		}
	}
	return BindingResolution{Action: BindHold, Reason: "affinity_hit"}
}

// resolveInvalid 处理"绑定节点不可用"的 strong/weak/manual 分流。
func resolveInvalid(in PickInput, g *GroupRef, why string) BindingResolution {
	bn := *g.BoundNodeID
	switch g.BindingPolicy {
	case "manual":
		// manual：永不自动改绑。首次发现时告警一次（用 WaitDeadline 记住"已告警"），
		// 之后静默等待 —— "人不管就停在那儿"是这个策略的定义，不是缺陷。
		if g.WaitDeadline == 0 {
			return BindingResolution{
				Action: BindWait,
				Reason: "manual_binding_invalid_" + why,
				Note: fmt.Sprintf("组 #%d 是手动绑定，绑定节点 #%d 不可用（%s）；按 manual 策略只告警不改绑，请人工处理",
					g.GroupID, bn, why),
				Deadline: in.Now + graceOf(in),
				Alert:    true,
			}
		}
		return BindingResolution{
			Action: BindWait,
			Reason: "manual_binding_invalid_waiting_" + why,
			Note: fmt.Sprintf("组 #%d 手动绑定仍不可用（%s），已告警过，继续等待人工处理",
				g.GroupID, why),
		}
	case "weak":
		return BindingResolution{
			Action: BindSplit,
			Reason: "weak_immediate_split_" + why,
			Note: fmt.Sprintf("组 #%d 的绑定节点 #%d 不可用（%s），weak 策略立即拆组，新成员改按负载选点",
				g.GroupID, bn, why),
		}
	}

	// strong（默认）：先等宽限期，到期才拆。
	grace := in.Cfg.BindingGraceSec
	if grace <= 0 {
		grace = DefaultBindingGraceSec
	}
	if g.WaitDeadline == 0 {
		return BindingResolution{
			Action:   BindWait,
			Reason:   "strong_wait_started_" + why,
			Note:     fmt.Sprintf("组 #%d 的绑定节点 #%d 不可用（%s），strong 策略开始等待 %ds", g.GroupID, bn, why, grace),
			Deadline: in.Now + grace,
			Alert:    true,
		}
	}
	if in.Now >= g.WaitDeadline {
		return BindingResolution{
			Action: BindSplit,
			Reason: "strong_wait_expired_" + why,
			Note: fmt.Sprintf("组 #%d 的绑定节点 #%d 不可用（%s）已超过宽限（deadline=%d），strong 策略拆组",
				g.GroupID, bn, why, g.WaitDeadline),
		}
	}
	return BindingResolution{
		Action: BindWait,
		Reason: "strong_waiting_" + why,
		Note: fmt.Sprintf("组 #%d 的绑定节点 #%d 不可用（%s），strong 策略等待中（还剩 %ds）",
			g.GroupID, bn, why, g.WaitDeadline-in.Now),
	}
}

// graceOf 返回 strong 的宽限时长（配置非法时回落默认值）。
func graceOf(in PickInput) int64 {
	if in.Cfg.BindingGraceSec > 0 {
		return in.Cfg.BindingGraceSec
	}
	return DefaultBindingGraceSec
}

// nodeSaturated 判断绑定节点是否已满容（含内存预留）。
func nodeSaturated(n *Candidate, in PickInput) bool {
	if n == nil {
		return true
	}
	return inflight(*n, in) >= n.MaxConcurrent
}
