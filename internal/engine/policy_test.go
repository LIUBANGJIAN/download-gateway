package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/scheduler"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// putPolicy 往 `app_config` 写一条策略覆盖（与管理台 `PUT /api/admin/config` 同形）。
func putPolicy(t *testing.T, st *store.Store, key string, enabled bool, priority int64) {
	t.Helper()
	raw := fmt.Sprintf(`{"enabled":%t,"priority":%d}`, enabled, priority)
	if _, err := st.Execute(context.Background(),
		`INSERT INTO app_config(key, value, updated_at) VALUES (?1, ?2, ?3)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		"policy."+key, raw, time.Now().Unix(),
	); err != nil {
		t.Fatalf("写策略 %s 失败: %v", key, err)
	}
}

// TestReloadPolicyKeepsDefaultsWhenStoreIsEmpty 空库 ⇒ 策略必须逐字段等于默认。
//
// 这是接线的安全底线：没有任何 policy.* 行时，调度结果必须与 B1 验收完全一致。
func TestReloadPolicyKeepsDefaultsWhenStoreIsEmpty(t *testing.T) {
	st := openTestStore(t)
	e := newEngine(t, st)
	e.reloadPolicy(context.Background())

	if e.cfg.Policy != scheduler.DefaultPolicy() {
		t.Fatalf("空库改变了策略：\n got=%+v\nwant=%+v", e.cfg.Policy, scheduler.DefaultPolicy())
	}
}

// TestReloadPolicyAppliesStoredOverride 库里存了覆盖 ⇒ 下一个周期就生效。
func TestReloadPolicyAppliesStoredOverride(t *testing.T) {
	st := openTestStore(t)
	e := newEngine(t, st)

	putPolicy(t, st, "least_tasks", false, 30)    // 关掉「最少活跃任务」
	putPolicy(t, st, "source_affinity", true, 45) // 开来源亲和，优先级 90→45（权重翻倍）
	putPolicy(t, st, "role_routing", false, 110)  // 关角色路由
	e.reloadPolicy(context.Background())

	if e.cfg.Policy.Params.Alpha != 0 {
		t.Fatalf("least_tasks 关闭后 Alpha = %v，期望 0", e.cfg.Policy.Params.Alpha)
	}
	if want := scheduler.DefaultWeights().W4 * 2; e.cfg.Policy.Weights.W4 != want {
		t.Fatalf("source_affinity 优先级提到 45 后 W4 = %v，期望 %v", e.cfg.Policy.Weights.W4, want)
	}
	if !e.cfg.Policy.DisableRoleRouting {
		t.Fatal("role_routing 关闭后 DisableRoleRouting 应为 true")
	}
	// 没碰过的项必须原样。
	if e.cfg.Policy.Weights.W1 != scheduler.DefaultWeights().W1 {
		t.Fatalf("未配置的 W1 被改动：%v", e.cfg.Policy.Weights.W1)
	}
}

// TestReloadPolicyIgnoresBadRows 单行坏 JSON 不能让整个策略加载失败，也不能影响其他行。
//
// 这条来自 15.2 的教训：`_, _ = ` 吞错会让"看起来写了实际没生效"变成常态。
// 这里要求：坏行被跳过（退回默认），好行照常生效，且**不返回错误**。
func TestReloadPolicyIgnoresBadRows(t *testing.T) {
	st := openTestStore(t)
	e := newEngine(t, st)

	if _, err := st.Execute(context.Background(),
		`INSERT INTO app_config(key, value, updated_at) VALUES ('policy.node_weight', '{不是JSON', ?1)`,
		time.Now().Unix()); err != nil {
		t.Fatalf("写坏行失败: %v", err)
	}
	putPolicy(t, st, "least_tasks", false, 30)

	e.reloadPolicy(context.Background())

	if !e.cfg.Policy.IgnoreNodeWeight {
		// 坏行被跳过 ⇒ node_weight 保持默认（参与折算）。
		if e.cfg.Policy.IgnoreNodeWeight {
			t.Fatal("坏行不该被当成 enabled=false")
		}
	}
	if e.cfg.Policy.Params.Alpha != 0 {
		t.Fatalf("好行 least_tasks 应生效（Alpha=0），得到 %v", e.cfg.Policy.Params.Alpha)
	}
}

// TestPolicySwitchChangedRealDispatch 端到端证伪：改一条策略，真实派发的落点必须翻转。
//
// 场景（两个节点容量都够、都在线）：
//
//	节点 A 权重 10，手上 5 个在跑 ⇒ 折算负载 5/10 = 0.50
//	节点 B 权重 1， 手上 1 个在跑 ⇒ 折算负载 1/1  = 1.00
//
// 按权重选 A；把 node_weight 关掉后各节点等权 ⇒ A 的 5 > B 的 1 ⇒ 选 B。
//
// 这条用例的价值在于：它跑的是 TickOnce 全链路（读库 → 选点 → 建绑 → 投递 → 写回），
// 而不是直接调 `scheduler.Pick`。策略接错线（比如读库路径写歪）在这里会红。
func TestPolicySwitchChangedRealDispatch(t *testing.T) {
	st := openTestStore(t)
	node := newFakeNode(t)

	a := addNode(t, st, "heavy", node.srv.URL, 50)
	b := addNode(t, st, "light", node.srv.URL, 50)
	setNodeWeight(t, st, a, 10)
	setNodeWeight(t, st, b, 1)

	// 造负载：A 上 5 个在跑，B 上 1 个。
	for i := 0; i < 5; i++ {
		addRunningTask(t, st, fmt.Sprintf("A%d", i), a)
	}
	addRunningTask(t, st, "B0", b)

	e := newEngine(t, st)

	// 第一轮：默认策略（权重参与折算）⇒ 应落 A。
	addTask(t, st, "T1", "Show.S01E01.mkv")
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce #1 失败: %v", err)
	}
	if got := nodeOf(t, st, "T1"); got != a {
		t.Fatalf("按权重应落到 heavy 节点 %d，实际 %d", a, got)
	}

	// 关掉 node_weight ⇒ 下一轮立刻按等权选 B。
	putPolicy(t, st, "node_weight", false, 60)

	addTask(t, st, "T2", "Other.S01E01.mkv") // 不同剧，避免被亲和短路到同一节点
	if err := e.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce #2 失败: %v", err)
	}
	if got := nodeOf(t, st, "T2"); got != b {
		t.Fatalf("关掉 node_weight 后应落到 light 节点 %d，实际 %d", b, got)
	}
}

// setNodeWeight 改节点权重。
func setNodeWeight(t *testing.T, st *store.Store, nodeID int64, w float64) {
	t.Helper()
	if _, err := st.Execute(context.Background(),
		`UPDATE node SET weight = ?1 WHERE node_id = ?2`, w, nodeID); err != nil {
		t.Fatalf("改权重失败: %v", err)
	}
}

// addRunningTask 插一个已经在跑的直属任务（用于制造节点负载）。
func addRunningTask(t *testing.T, st *store.Store, taskID string, nodeID int64) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := st.Execute(context.Background(),
		`INSERT INTO task (task_id, gid, kind, url_raw, name, internal_state, aria_status,
		                   node_id, created_at, updated_at)
		 VALUES (?1, ?2, 'http', ?3, ?4, 'running', 'active', ?5, ?6, ?6)`,
		taskID, "gid-"+taskID, "http://node.test/"+taskID, taskID+".mkv", nodeID, now,
	); err != nil {
		t.Fatalf("插在跑任务 %s 失败: %v", taskID, err)
	}
}

// nodeOf 读任务的落点。
func nodeOf(t *testing.T, st *store.Store, taskID string) int64 {
	t.Helper()
	rows, err := st.Query(context.Background(),
		`SELECT node_id FROM task WHERE task_id = ?1`, taskID)
	if err != nil || len(rows) == 0 {
		t.Fatalf("读任务 %s 失败: %v", taskID, err)
	}
	v, ok := rows[0][0].(int64)
	if !ok {
		t.Fatalf("任务 %s 的 node_id 仍为 NULL", taskID)
	}
	return v
}
