// 管理台「设置」页的数据源：派发 / 负载策略的**清单 + 开关 + 优先级**，以及运行环境快照。
//
// # 本轮它只是「可配置」，**不会真的生效**
//
// 真正按这些策略挑机器的调度内核（B 类）尚未落地。因此每条策略都带
// `effective:false` + 一句 `effective_note`，界面必须如实标注 —— 让用户以为
// 「改完行为立刻变化」比不做这个页面更糟。
//
// # 存储：复用 `app_config` KV，**不新建表**
//
// key = `policy.<策略键>`，value = JSON `{"enabled":bool,"priority":int}`。
// 代码里内置完整默认表（[catalog]），库里**只存用户改过的差异** —— 这样将来
// 策略增删不需要写迁移。
package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/session"
)

// 优先级取值范围。
const (
	priorityMin int64 = 1
	priorityMax int64 = 999
)

// effectiveNote 是所有策略统一的生效说明。
const effectiveNote = "已保存，当前尚未生效 —— 策略由调度内核应用，而调度内核（T03–T07）尚未落地"

// policyDef 是单条策略的静态定义。
type policyDef struct {
	Key             string
	Name            string
	Category        string
	Description     string
	DefaultEnabled  bool
	CanDisable      bool
	DefaultPriority int64
	Dangerous       bool
}

// catalog 是策略总表（12 条）。**唯一事实源**：界面、默认值、校验都读它。
var catalog = []policyDef{
	{"max_concurrent", "节点并发上限", "容量安全阀", "一台机器同时在跑的任务数达到上限后，不再往它派新任务。这是防压垮机器的最后一道闸。", true, false, 10, true},
	{"group_affinity", "同组同节点", "亲和", "同一部剧集的各集尽量落在同一台机器上，避免把一部剧拆得七零八落。", true, true, 20, false},
	{"least_tasks", "最少活跃任务优先", "负载均衡", "新任务优先派给当前手上活最少的那台机器。", true, true, 30, false},
	{"retry_backoff", "失败重试退避", "容错", "任务失败后按递增间隔重试，而不是死循环硬打。", true, true, 40, false},
	{"throttle_backoff", "限流退避", "容错", "某台机器被限流时，暂时降低对它的调用频率，等它缓过来。", true, true, 50, false},
	{"node_weight", "节点权重", "负载均衡", "按每台机器配置的权重加权分配，性能强的机器多分一些活。", true, true, 60, false},
	{"max_concurrent_in_group", "组内并发上限", "容量安全阀", "同一部剧集在单台机器上同时下载的集数上限。", true, true, 70, false},
	{"weighted_least_tasks", "加权最少任务", "负载均衡", "在「最少活跃任务」的基础上按权重折算，避免弱机器被塞满。", false, true, 80, false},
	{"source_affinity", "同来源同节点", "亲和", "来自同一个网盘的任务尽量落在同一台机器上。", false, true, 90, false},
	{"source_pinned", "来源固定节点", "约束", "指定某个来源的任务**只**派到某一台机器，其余机器一概不用。", false, true, 100, false},
	{"role_routing", "按节点角色路由", "路由", "按机器上标注的角色分派：剧集任务给「剧集机」、网盘任务给「网盘机」。", false, true, 110, false},
	{"node_rate_limit", "节点限速", "限速", "按每台机器配置的限速值限制下发速率，避免打满带宽。", false, true, 120, false},
}

// findPolicy 按 key 查静态定义。
func findPolicy(key string) *policyDef {
	for i := range catalog {
		if catalog[i].Key == key {
			return &catalog[i]
		}
	}
	return nil
}

// storedPolicy 是库里存的一行覆盖值。
type storedPolicy struct {
	Enabled  bool  `json:"enabled"`
	Priority int64 `json:"priority"`
}

// policyView 是接口返回的单条策略（静态定义 + 当前值）。
type policyView struct {
	def        *policyDef
	Enabled    bool
	Priority   int64
	Customized bool
}

// loadPolicies 读全部策略：默认值 + 库内差异合并，顺序恒为 catalog 顺序。
func (s *adminServer) loadPolicies(r *http.Request) ([]policyView, error) {
	rows, err := s.st.Store.Query(r.Context(),
		"SELECT key, value FROM app_config WHERE key LIKE 'policy.%'")
	if err != nil {
		return nil, err
	}
	stored := map[string]storedPolicy{}
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		k, ok1 := row[0].(string)
		v, ok2 := row[1].(string)
		if !ok1 || !ok2 {
			continue
		}
		name := strings.TrimPrefix(k, "policy.")
		var sp storedPolicy
		// 解析失败**不能**让整个页面挂掉：忽略该行、退回默认值。
		if err := json.Unmarshal([]byte(v), &sp); err == nil {
			stored[name] = sp
		}
	}

	out := make([]policyView, 0, len(catalog))
	for i := range catalog {
		d := &catalog[i]
		hit, customized := stored[d.Key]
		v := policyView{def: d, Enabled: d.DefaultEnabled, Priority: d.DefaultPriority, Customized: customized}
		if customized {
			v.Enabled = hit.Enabled
			v.Priority = hit.Priority
		}
		out = append(out, v)
	}
	return out, nil
}

// toJSON 转成接口形态（字段名对齐 Rust `PolicyView` 的 snake_case）。
func (v policyView) toJSON() map[string]any {
	return map[string]any{
		"key":            v.def.Key,
		"name":           v.def.Name,
		"category":       v.def.Category,
		"description":    v.def.Description,
		"enabled":        v.Enabled,
		"priority":       v.Priority,
		"can_disable":    v.def.CanDisable,
		"dangerous":      v.def.Dangerous,
		"customized":     v.Customized,
		"effective":      false,
		"effective_note": effectiveNote,
	}
}

// configGet 实现 `GET /api/admin/config`。
func (s *adminServer) configGet(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardRead(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	policies, err := s.loadPolicies(r)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	items := make([]any, 0, len(policies))
	for _, p := range policies {
		items = append(items, p.toJSON())
	}
	ok(w, map[string]any{
		"policies": items,
		"env":      s.envItems(),
	})
	s.renewCookie(w, r, exp)
}

// envItems 把 `EnvSnapshot` 拍平为配置项列表（敏感键一律掩码 `****`）。
func (s *adminServer) envItems() []any {
	items := s.st.Env.Items()
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"key":         it.Key,
			"value":       it.Value,
			"mutable":     it.Mutable,
			"effective":   it.Effective,
			"risk":        it.Risk,
			"description": it.Description,
		})
	}
	return out
}

// policyUpdate 是单条提交项。
type policyUpdate struct {
	Key      string `json:"key"`
	Enabled  *bool  `json:"enabled"`
	Priority *int64 `json:"priority"`
}

// configPut 实现 `PUT /api/admin/config`：校验 → 落库 → 回读。
//
// 校验规则（与 Rust 逐条一致，**一律显式 422，绝不静默夹取/忽略**）：
//   - 未知策略键 ⇒ 422；
//   - 优先级越界 ⇒ 422；
//   - 关闭不可关闭项（容量安全阀）⇒ 422。
func (s *adminServer) configPut(w http.ResponseWriter, r *http.Request) {
	exp, g := s.guardWrite(r)
	if g != session.GuardOK {
		s.rejectGuard(w, g)
		return
	}
	var req struct {
		Policies []policyUpdate `json:"policies"`
	}
	if err := decodeBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, CodeValidationError, err.Error())
		return
	}

	// 先整体校验，再落库：避免「前面几条写进去、后面一条报错」的半截状态。
	type planned struct {
		key   string
		value storedPolicy
	}
	plan := make([]planned, 0, len(req.Policies))
	for _, u := range req.Policies {
		def := findPolicy(u.Key)
		if def == nil {
			fail(w, http.StatusUnprocessableEntity, CodeValidationError,
				"未知策略："+u.Key+"（不在当前策略表内）")
			return
		}
		next := storedPolicy{Enabled: def.DefaultEnabled, Priority: def.DefaultPriority}
		if u.Enabled != nil {
			next.Enabled = *u.Enabled
		}
		if u.Priority != nil {
			next.Priority = *u.Priority
		}
		if next.Priority < priorityMin || next.Priority > priorityMax {
			fail(w, http.StatusUnprocessableEntity, CodeValidationError,
				"策略 "+u.Key+" 的优先级越界：允许范围 1–999（收到 "+strconv.FormatInt(next.Priority, 10)+"）")
			return
		}
		if !next.Enabled && !def.CanDisable {
			fail(w, http.StatusUnprocessableEntity, CodeValidationError,
				"策略 "+u.Key+" 不允许关闭：它是容量安全阀，关掉会把节点压垮")
			return
		}
		plan = append(plan, planned{key: "policy." + def.Key, value: next})
	}

	now := ids.NowSecs()
	for _, p := range plan {
		buf, _ := json.Marshal(p.value)
		if _, err := s.st.Store.Execute(r.Context(),
			"INSERT INTO app_config(key, value, updated_at) VALUES (?1, ?2, ?3) "+
				"ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
			p.key, string(buf), now); err != nil {
			fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
			return
		}
	}

	policies, err := s.loadPolicies(r)
	if err != nil {
		fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	items := make([]any, 0, len(policies))
	for _, p := range policies {
		items = append(items, p.toJSON())
	}
	ok(w, map[string]any{"policies": items, "env": s.envItems()})
	s.renewCookie(w, r, exp)
}
