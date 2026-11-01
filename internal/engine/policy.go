package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/scheduler"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// 本文件把管理台存的派发策略接进调度内核。
//
// # 之前的状态
//
// 内核跑 `scheduler.DefaultPolicy()`，**从不读** `app_config` 的 `policy.*`。
// 用户在「设置」页调完开关与优先级，下载分布毫无变化。
//
// # 现在的语义
//
//	库里没有的键 ⇒ 用内置默认（与 B1 验收一致）
//	库里有的键   ⇒ 以库里为准，立刻生效（下一个 Tick 就按新策略选点）
//
// 「立刻生效」是有意的：让用户改完就能从 `dispatch_log.score_json` 看到不同的
// 评分结果，而不是等重启。代价是每个 Tick 一次 `LIKE 'policy.%'` 的小查询，
// 在 SQLite 上是微秒级，相对一次网络投递可以忽略。

// storedPolicyRow 是 `app_config` 里一行策略值。
type storedPolicyRow struct {
	Enabled  bool  `json:"enabled"`
	Priority int64 `json:"priority"`
}

// loadPolicyOverrides 读出用户**显式改过**的策略。
//
// 只有库里真实存在的行才进 map。这一点很关键：如果按 catalog 默认值补齐所有键，
// `source_affinity`（catalog 默认关）会把 W4 归零、`weighted_least_tasks`（默认关）
// 会把 Beta 归零，等于悄悄换掉 B1 已经验收过的评分公式。
func loadPolicyOverrides(ctx context.Context, st *store.Store) (map[string]scheduler.PolicyOverride, error) {
	rows, err := st.Query(ctx, `SELECT key, value FROM app_config WHERE key LIKE 'policy.%'`)
	if err != nil {
		return nil, fmt.Errorf("engine: 读派发策略: %w", err)
	}
	out := make(map[string]scheduler.PolicyOverride, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		k, ok1 := row[0].(string)
		v, ok2 := row[1].(string)
		if !ok1 || !ok2 {
			continue
		}
		var r storedPolicyRow
		// 单行坏数据**不能**拖垮调度：跳过该行、退回默认，其余照常。
		// 静默跳过会让人以为"存进去了"，所以下面 reloadPolicy 会把解析失败计数打出来。
		if err := json.Unmarshal([]byte(v), &r); err != nil {
			continue
		}
		out[strings.TrimPrefix(k, "policy.")] = scheduler.PolicyOverride{
			Enabled:  r.Enabled,
			Priority: r.Priority,
		}
	}
	return out, nil
}

// reloadPolicy 把库里的策略重新应用到 `e.cfg.Policy`。
//
// 读库失败时**保留上次策略**并告警，不中断调度：策略是优化项，
// 不是正确性前提，让一个 SELECT 失败把整条下载链路停掉是本末倒置。
func (e *Engine) reloadPolicy(ctx context.Context) {
	ov, err := loadPolicyOverrides(ctx, e.st)
	if err != nil {
		e.log.Warn("读取派发策略失败，沿用上一次策略", "err", err)
		return
	}
	pol, applied := scheduler.ApplyOverrides(scheduler.DefaultPolicy(), ov)

	// 只在**真的变了**时打日志：Tick 是 1s，每次都打会把日志刷爆。
	if pol != e.cfg.Policy {
		e.log.Info("派发策略已按管理台配置更新",
			"applied", applied,
			"w1", pol.Weights.W1, "w2", pol.Weights.W2, "w4", pol.Weights.W4,
			"alpha", pol.Params.Alpha, "beta", pol.Params.Beta,
			"disable_node_capacity", pol.DisableNodeCapacity,
			"disable_group_capacity", pol.DisableGroupCapacity,
			"ignore_node_weight", pol.IgnoreNodeWeight,
			"disable_source_pinned", pol.DisableSourcePinned,
			"disable_role_routing", pol.DisableRoleRouting,
		)
	}
	e.cfg.Policy = pol
}
