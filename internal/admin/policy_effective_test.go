package admin

import (
	"strings"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/scheduler"
)

// TestPolicyCatalogMatchesScheduler 双向锁定「管理台策略表」与「内核消费表」。
//
// 两张表分处两个包（`admin` 的 catalog、`scheduler` 的 supportedPolicyKeys），
// 中间没有任何编译期约束。一旦有人加了一条策略却没接线（或反之），
// 用户就会看到"调了没反应"——正是这次修的那个 bug。所以必须双向断言：
//
//	方向 1：内核认的每条都在 catalog 里，且基准优先级一致（否则权重缩放基准会漂）
//	方向 2：catalog 里每条要么内核认，要么必须写明不生效的**具体原因**
//	方向 3：原因表不许留孤儿（策略已删、说明还在）
func TestPolicyCatalogMatchesScheduler(t *testing.T) {
	inCatalog := make(map[string]int64, len(catalog))
	for _, d := range catalog {
		inCatalog[d.Key] = d.DefaultPriority
	}

	// 方向 1
	for _, key := range scheduler.SupportedPolicyKeys() {
		prio, ok := inCatalog[key]
		if !ok {
			t.Fatalf("内核支持策略 %s，但管理台 catalog 里没有这一条", key)
		}
		spec, _ := scheduler.PolicyKeySpec(key)
		if spec.BasePriority != prio {
			t.Fatalf("策略 %s 的基准优先级不一致：scheduler=%d admin=%d（权重缩放会把没改过的策略也缩放）",
				key, spec.BasePriority, prio)
		}
		if strings.TrimSpace(spec.Drives) == "" {
			t.Fatalf("策略 %s 缺少 Drives 说明，管理台无法告诉用户它作用在哪", key)
		}
	}

	// 方向 2
	for _, d := range catalog {
		if _, ok := scheduler.PolicyKeySpec(d.Key); ok {
			continue
		}
		note, ok := unsupportedPolicyNotes[d.Key]
		if !ok || strings.TrimSpace(note) == "" {
			t.Fatalf("策略 %s 内核不消费，但没写 effective_note 原因", d.Key)
		}
		// 原因必须是具体的：至少要说清"没生效"以及"为什么"。
		if !strings.Contains(note, "暂未生效") {
			t.Fatalf("策略 %s 的说明没说清未生效：%s", d.Key, note)
		}
		if !strings.Contains(note, "：") {
			t.Fatalf("策略 %s 的说明没有给出原因（缺少冒号后的解释）：%s", d.Key, note)
		}
	}

	// 方向 3
	for key := range unsupportedPolicyNotes {
		if _, ok := inCatalog[key]; !ok {
			t.Fatalf("unsupportedPolicyNotes 里的 %s 已不在 catalog 中，该删掉", key)
		}
	}
}

// TestPolicyViewEffectiveFlag 接口层逐条验证 `effective`。
//
// 不能出现「文档说生效、响应说没生效」或者相反的错位——用户只会看响应。
func TestPolicyViewEffectiveFlag(t *testing.T) {
	// 12 条全部生效（B1.5 的收尾：group_affinity 随 B2 的拆组落地，
	// retry_backoff / throttle_backoff / node_rate_limit 在投递层接上了参数）。
	supported := []string{
		"max_concurrent", "group_affinity", "least_tasks", "retry_backoff",
		"throttle_backoff", "node_weight", "max_concurrent_in_group",
		"weighted_least_tasks", "source_affinity", "source_pinned",
		"role_routing", "node_rate_limit",
	}
	for _, key := range supported {
		t.Run("生效_"+key, func(t *testing.T) {
			def := findPolicy(key)
			if def == nil {
				t.Fatalf("catalog 里没有 %s", key)
			}
			m := policyView{def: def}.toJSON()
			if m["effective"] != true {
				t.Fatalf("%s 应 effective=true，得到 %v", key, m["effective"])
			}
			note, _ := m["effective_note"].(string)
			if !strings.Contains(note, "已生效") {
				t.Fatalf("%s 的 note 没说明已生效：%s", key, note)
			}
		})
	}

	// 反向守卫：**catalog 里每一条都必须生效**。
	// 留着这段是为了防"新增了一条策略但忘了接线"——那种情况下它会红。
	for i := range catalog {
		key := catalog[i].Key
		t.Run("catalog全覆盖_"+key, func(t *testing.T) {
			def := findPolicy(key)
			if def == nil {
				t.Fatalf("catalog 里没有 %s", key)
			}
			m := policyView{def: def}.toJSON()
			if m["effective"] != true {
				t.Fatalf("%s 应 effective=true，得到 %v（note=%v）", key, m["effective"], m["effective_note"])
			}
			note, _ := m["effective_note"].(string)
			if !strings.Contains(note, "已生效") {
				t.Fatalf("%s 的 note 没说清已生效：%s", key, note)
			}
		})
	}
}

// TestPolicyCatalogHasTwelveEntries catalog 是用户看到的完整清单，数量变了要显式改文档。
//
// 这个断言很"笨"，但它防的是一件真事：往 catalog 加一条策略只要一行代码，
// 而 README、设计文档、冒烟断言里都写着"12 条"。数字对不上时，
// 让人在这里红一次，比让文档悄悄过期好。
func TestPolicyCatalogHasTwelveEntries(t *testing.T) {
	if len(catalog) != 12 {
		t.Fatalf("catalog 现有 %d 条策略，文档与冒烟断言写的是 12 条；改数量请同步更新文档", len(catalog))
	}
}
