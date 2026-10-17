package dispatch

import (
	"context"
	"fmt"

	"github.com/LIUBANGJIAN/download-gateway/internal/scheduler"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// NodeCredential 是投递一个任务所需的节点连接信息（已含密文，未解密）。
type NodeCredential struct {
	NodeID     int64
	Alias      string
	BaseURL    string
	User       string
	PassEnc    string
	DeviceName string
	ClientID   string
}

// LoadNodeCredential 读单个节点的连接参数。
//
// 用 `nodeID` 直查而不是从内存缓存取：节点密码可以在管理台被改，
// 缓存会让"改了密码但投递还用旧密码"变成一类要重启才能好的故障。
func LoadNodeCredential(ctx context.Context, st *store.Store, nodeID int64) (NodeCredential, error) {
	rows, err := st.Query(ctx,
		`SELECT node_id, alias, base_url, user, pass_enc, device_name, COALESCE(client_id, '')
		   FROM node WHERE node_id = ?1`, nodeID)
	if err != nil {
		return NodeCredential{}, fmt.Errorf("dispatch: 读节点: %w", err)
	}
	if len(rows) == 0 {
		return NodeCredential{}, fmt.Errorf("dispatch: 节点 %d 不存在", nodeID)
	}
	r := rows[0]
	return NodeCredential{
		NodeID:     asInt64(r[0]),
		Alias:      asString(r[1]),
		BaseURL:    asString(r[2]),
		User:       asString(r[3]),
		PassEnc:    asString(r[4]),
		DeviceName: asString(r[5]),
		ClientID:   asString(r[6]),
	}, nil
}

// LoadCandidates 读全部节点的决策视图。
//
// `active` 与 `pending_bytes` 都是**自聚合**（`design/02 §2.1.1` 的 F6）：
// 不用内存计数，因为它会在重启/漏写时漂移；直接查库只多一条语句。
//
// `pending_bytes` 的三档估算严格按 `design/02 §2.1.1`：
// resolved 用 total-downloaded；estimated 用 total×(1-p)；其余（unknown/probing）
// 用 DL_UNKNOWN_BYTES×(1-p) —— 把"未知"当 0 字节会让一台正在下大包的机器看起来最闲。
func LoadCandidates(ctx context.Context, st *store.Store, dlUnknownBytes float64) ([]scheduler.Candidate, error) {
	rows, err := st.Query(ctx, `
		SELECT n.node_id, n.alias, n.enabled, n.state, n.throttle_until,
		       ROUND(n.weight, 6), n.max_concurrent, n.role, n.last_seen_at,
		       COALESCE(a.active, 0) AS active,
		       COALESCE(p.pending, 0) AS pending
		  FROM node n
		  LEFT JOIN (
		        SELECT node_id, COUNT(1) AS active FROM task
		         WHERE internal_state IN ('running','dispatching') AND node_id IS NOT NULL
		         GROUP BY node_id
		       ) a ON a.node_id = n.node_id
		  LEFT JOIN (
		        SELECT node_id, SUM(
		                 CASE
		                   WHEN size_state = 'resolved' AND total_size IS NOT NULL
		                     THEN MAX(total_size - downloaded_size, 0)
		                   WHEN size_state = 'estimated' AND total_size IS NOT NULL
		                     THEN total_size * (1000 - permillage) / 1000.0
		                   ELSE ?1 * (1000 - permillage) / 1000.0
		                 END) AS pending
		          FROM task
		         WHERE internal_state IN ('running','dispatching') AND node_id IS NOT NULL
		         GROUP BY node_id
		       ) p ON p.node_id = n.node_id
		 ORDER BY n.node_id`, dlUnknownBytes)
	if err != nil {
		return nil, fmt.Errorf("dispatch: 读候选节点: %w", err)
	}

	out := make([]scheduler.Candidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, scheduler.Candidate{
			NodeID:         asInt64(r[0]),
			Alias:          asString(r[1]),
			Enabled:        asInt64(r[2]) != 0,
			State:          asString(r[3]),
			ThrottleUntil:  asInt64(r[4]),
			Weight:         asFloat(r[5]),
			MaxConcurrent:  asInt64(r[6]),
			Role:           asString(r[7]),
			LastAssignedAt: asInt64(r[8]),
			Active:         asInt64(r[9]),
			PendingBytes:   asFloat(r[10]),
		})
	}
	return out, nil
}

// LoadGroup 读组视图。`groupID<=0` 返回 nil（任务不属于任何组）。
func LoadGroup(ctx context.Context, st *store.Store, groupID int64) (*scheduler.GroupRef, error) {
	if groupID <= 0 {
		return nil, nil
	}
	rows, err := st.Query(ctx,
		`SELECT g.group_id, g.bound_node_id, g.binding_epoch, g.group_state,
		        g.max_concurrent_in_group, g.binding_policy, g.split_policy,
		        COALESCE((SELECT COUNT(1) FROM task t
		                   WHERE t.group_id = g.group_id
		                     AND t.internal_state IN ('running','dispatching')), 0)
		   FROM task_group g WHERE g.group_id = ?1`, groupID)
	if err != nil {
		return nil, fmt.Errorf("dispatch: 读组: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	g := &scheduler.GroupRef{
		GroupID:              asInt64(r[0]),
		BindingEpoch:         asInt64(r[2]),
		GroupState:           asString(r[3]),
		MaxConcurrentInGroup: asInt64(r[4]),
		BindingPolicy:        asString(r[5]),
		SplitPolicy:          asString(r[6]),
		ActiveInGroup:        asInt64(r[7]),
	}
	if v, ok := r[1].(int64); ok {
		g.BoundNodeID = &v
	}
	return g, nil
}

// SourceRoute 是来源路由信息（C5 与来源加分）。
type SourceRoute struct {
	Strategy     string
	PinnedNodeID *int64
}

// LoadSourceRoute 读来源档案。`sourceKey` 为空或无对应行 ⇒ 默认 round_robin。
//
// 默认值不是"兜底凑数"：`source_profile` 的种子行并不是为每个来源预建的，
// 绝大多数任务没有来源键，它们就该走普通负载调度。
func LoadSourceRoute(ctx context.Context, st *store.Store, sourceKey *string) (SourceRoute, error) {
	def := SourceRoute{Strategy: "round_robin"}
	if sourceKey == nil || *sourceKey == "" {
		return def, nil
	}
	rows, err := st.Query(ctx,
		`SELECT strategy, pinned_node_id FROM source_profile
		  WHERE source_key = ?1 AND enabled = 1`, *sourceKey)
	if err != nil {
		return def, fmt.Errorf("dispatch: 读来源档案: %w", err)
	}
	if len(rows) == 0 {
		return def, nil
	}
	out := SourceRoute{Strategy: asString(rows[0][0])}
	if v, ok := rows[0][1].(int64); ok {
		out.PinnedNodeID = &v
	}
	if out.Strategy == "" {
		out.Strategy = "round_robin"
	}
	return out, nil
}

// QueuedTask 是一个待派发任务的最小视图。
type QueuedTask struct {
	TaskID      string
	Name        *string
	URLRaw      string
	Kind        string
	GroupID     *int64
	SourceKey   *string
	AttemptNo   int64
	MaxAttempts int64
	NextRetryAt int64
	EpisodeNo   *int64
}

// LoadQueuedTasks 扫待派发任务。
//
// 条件里三条都不能少：
//   - `internal_state='queued'` —— 只扫排队态；
//   - `node_id IS NULL` —— 已落点的任务不在本轮范围（T06 负责它）；
//   - `error_class <> 'not_started'`（或 NULL）—— `start_later` 的任务是**用户没点开始**，
//     把它们派出去等于违背用户意图，而且会在节点上立刻变成 paused 又被回读成
//     `not_started`，形成无意义的往返。
func LoadQueuedTasks(ctx context.Context, st *store.Store, now int64, limit int64) ([]QueuedTask, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := st.Query(ctx, `
		SELECT task_id, name, url_raw, kind, group_id, source_key,
		       attempt_no, max_attempts, next_retry_at, episode_no
		  FROM task
		 WHERE internal_state = 'queued'
		   AND node_id IS NULL
		   AND next_retry_at <= ?1
		   AND (error_class IS NULL OR error_class <> 'not_started')
		 ORDER BY created_at ASC
		 LIMIT ?2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("dispatch: 扫排队任务: %w", err)
	}
	out := make([]QueuedTask, 0, len(rows))
	for _, r := range rows {
		t := QueuedTask{
			TaskID:      asString(r[0]),
			URLRaw:      asString(r[2]),
			Kind:        asString(r[3]),
			AttemptNo:   asInt64(r[6]),
			MaxAttempts: asInt64(r[7]),
			NextRetryAt: asInt64(r[8]),
		}
		if s, ok := r[1].(string); ok {
			t.Name = &s
		}
		if v, ok := r[4].(int64); ok {
			t.GroupID = &v
		}
		if s, ok := r[5].(string); ok {
			t.SourceKey = &s
		}
		if v, ok := r[9].(int64); ok {
			t.EpisodeNo = &v
		}
		out = append(out, t)
	}
	return out, nil
}

// tryInt64 是 sql.Null* 之外的兜底转换（某些驱动会把整列给 int64、另一些给 []byte）。
func asInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case []byte:
		var n int64
		_, _ = fmt.Sscan(string(x), &n)
		return n
	case string:
		var n int64
		_, _ = fmt.Sscan(x, &n)
		return n
	case nil:
		return 0
	default:
		return 0
	}
}

// asFloat 把列值转 float64。
func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case []byte:
		var f float64
		_, _ = fmt.Sscan(string(x), &f)
		return f
	case string:
		var f float64
		_, _ = fmt.Sscan(x, &f)
		return f
	case nil:
		return 0
	default:
		return 0
	}
}

// asString 把列值转字符串。
func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

// ---------------------------------------------------------------------------
// 投递结果落库
// ---------------------------------------------------------------------------

// MarkDelivered 把"投递成功"写回库：`task_attempt.state` 从 `dispatched` 迁移到
// `created`/`running`，并写 `node_task_id`。
//
// 这次迁移**不是**共同不变量（`design/12 §9` 允许 best-effort），但它必须能被
// T06 回读纠正 —— 所以这里失败只记日志、不回滚已提交的建绑事务：
// 回滚会把"任务已经在节点上下载"这个事实抹掉，而库里的状态是可以再纠正的。
func MarkDelivered(ctx context.Context, st *store.Store, taskID string, attemptNo int64, res DeliverResult, now int64) error {
	if _, err := st.Execute(ctx,
		`UPDATE task_attempt SET state = ?1, node_task_id = ?2
		  WHERE task_id = ?3 AND attempt_no = ?4`,
		res.AttemptState, res.NodeTaskID, taskID, attemptNo,
	); err != nil {
		return fmt.Errorf("dispatch: 更新尝试(成功): %w", err)
	}

	internal, aria := "dispatching", "waiting"
	if res.AttemptState == "running" {
		internal, aria = "running", "active"
	}
	if _, err := st.Execute(ctx,
		`UPDATE task
		    SET node_task_id = ?1, internal_state = ?2, aria_status = ?3,
		        error_class = NULL, error_message = NULL, next_retry_at = 0, updated_at = ?4
		  WHERE task_id = ?5`,
		res.NodeTaskID, internal, aria, now, taskID,
	); err != nil {
		return fmt.Errorf("dispatch: 更新任务(成功): %w", err)
	}
	return nil
}

// MarkDeliverFailed 把"投递失败"写回库，并按可重试性分流。
//
// 两条分支的终态**必须不同**（B1-AC6）：
//   - 可重试 ⇒ 回 `queued` + `node_id=NULL` + `next_retry_at>0`，且 `attempt_no>=1`；
//   - 不可重试 ⇒ `failed_terminal` + `error_class<>none`。
//
// 判据要防的是第三种形态：`queued` + `attempt_no=0` + `error_class IS NULL`
// —— 那才是"静默失败"，用户看到排队、实际永远不会再动。
func MarkDeliverFailed(ctx context.Context, st *store.Store, taskID string, attemptNo int64, res DeliverResult, now int64) error {
	errClass, message := "", res.Detail
	if res.ErrorClass != nil {
		errClass = string(*res.ErrorClass)
	}

	if _, err := st.Execute(ctx,
		`UPDATE task_attempt SET state = 'failed', ended_at = ?1, reason = ?2
		  WHERE task_id = ?3 AND attempt_no = ?4`,
		now, errClass, taskID, attemptNo,
	); err != nil {
		return fmt.Errorf("dispatch: 更新尝试(失败): %w", err)
	}

	if res.Retryable {
		aria := "waiting"
		if _, err := st.Execute(ctx,
			`UPDATE task
			    SET internal_state = 'queued', node_id = NULL, dispatch_reason = NULL,
			        error_class = ?1, error_message = ?2, next_retry_at = ?3,
			        aria_status = ?4, updated_at = ?5
			  WHERE task_id = ?6`,
			errClass, message, res.NextRetryAt, aria, now, taskID,
		); err != nil {
			return fmt.Errorf("dispatch: 更新任务(待重试): %w", err)
		}
		return nil
	}

	if _, err := st.Execute(ctx,
		`UPDATE task
		    SET internal_state = 'failed_terminal', error_class = ?1, error_message = ?2,
		        aria_status = 'error', next_retry_at = 0, updated_at = ?3
		  WHERE task_id = ?4`,
		errClass, message, now, taskID,
	); err != nil {
		return fmt.Errorf("dispatch: 更新任务(终态失败): %w", err)
	}
	return nil
}

// LogEvent 写一条 `event_log`（失败只忽略：审计绝不能影响主流程）。
func LogEvent(ctx context.Context, st *store.Store, level, category string, nodeID int64, taskID, message string) {
	var nodeArg any
	if nodeID > 0 {
		nodeArg = nodeID
	}
	var taskArg any
	if taskID != "" {
		taskArg = taskID
	}
	_, _ = st.Execute(ctx,
		`INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, NULL, strftime('%s','now'))`,
		level, category, nodeArg, taskArg, message,
	)
}

// LoadGroupByKey 按 `affinity_key` 读组（存在则返回视图与 group_id）。
//
// 为什么需要它：任务的 `group_id` 在**首次派发成功前**是 NULL，而它所属的组
// 可能已经由别的集建好了（正是"同组落在同一节点"要处理的情形）。
// 只按 `task.group_id` 读会把这种情况当成"新组"，于是每个集都以为自己是第一个成员。
func LoadGroupByKey(ctx context.Context, st *store.Store, affinityKey string) (*scheduler.GroupRef, error) {
	if affinityKey == "" {
		return nil, nil
	}
	rows, err := st.Query(ctx,
		`SELECT g.group_id, g.bound_node_id, g.binding_epoch, g.group_state,
		        g.max_concurrent_in_group, g.binding_policy, g.split_policy,
		        COALESCE((SELECT COUNT(1) FROM task t
		                   WHERE t.group_id = g.group_id
		                     AND t.internal_state IN ('running','dispatching')), 0)
		   FROM task_group g WHERE g.affinity_key = ?1`, affinityKey)
	if err != nil {
		return nil, fmt.Errorf("dispatch: 按键读组: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	g := &scheduler.GroupRef{
		GroupID:              asInt64(r[0]),
		BindingEpoch:         asInt64(r[2]),
		GroupState:           asString(r[3]),
		MaxConcurrentInGroup: asInt64(r[4]),
		BindingPolicy:        asString(r[5]),
		SplitPolicy:          asString(r[6]),
		ActiveInGroup:        asInt64(r[7]),
	}
	if v, ok := r[1].(int64); ok {
		g.BoundNodeID = &v
	}
	return g, nil
}
