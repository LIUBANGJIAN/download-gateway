// Package engine 是把选点、建绑、投递、回读串起来的**驱动循环**。
//
// 归属说明：`design/12 §7` 的 B1 文件清单只列了 6 个文件，没有"谁去定时扫排队任务"。
// 按规格 C-14 的建议新增本包（而不是把循环塞进 `main.go`），理由是：
// 循环需要可关闭、可单测、可优雅退出；塞进 main 会让这三点都变成手工活。
//
// 两条循环各司其职：
//
//	调度循环（每 Tick）：扫 queued → 选点 → 事务内建绑 → 事务外投递 → 写回结果
//	轮询循环（每 PollInterval）：拉节点任务列表 → 对账 → 写进度
//
// 两者**不共享锁**：调度循环只写"决策与意图"，轮询循环只写"观测事实"。
// 唯一共享的是 `reserved`（内存预留），它有自己的互斥量。
package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/dispatch"
	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/grouping"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
	"github.com/LIUBANGJIAN/download-gateway/internal/naming"
	"github.com/LIUBANGJIAN/download-gateway/internal/scheduler"
	"github.com/LIUBANGJIAN/download-gateway/internal/secret"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
	syncpkg "github.com/LIUBANGJIAN/download-gateway/internal/sync"
)

// Config 是引擎配置。
type Config struct {
	// Tick 是调度循环间隔（默认 1s）。
	Tick time.Duration
	// Batch 是每轮最多处理的任务数（默认 20）。
	Batch int64
	// PollInterval 是回读间隔（默认 15s）。
	PollInterval time.Duration
	// PollLimit 是单次拉取上限（默认 500）。
	PollLimit int
	// VerifyTimeout 是无 task_id 时的回查上限（默认 10s）。
	VerifyTimeout time.Duration
	// ProbeInterval 是节点健康探测间隔（默认 30s）。
	ProbeInterval time.Duration
	// KeyOptions 是分组识别开关。
	KeyOptions grouping.KeyOptions
	// Policy 是选点策略（权重 / 折算参数 / 组内并发）。
	Policy scheduler.Policy
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Tick:          1 * time.Second,
		Batch:         20,
		PollInterval:  15 * time.Second,
		PollLimit:     500,
		VerifyTimeout: 10 * time.Second,
		ProbeInterval: 30 * time.Second,
		KeyOptions:    grouping.DefaultKeyOptions(),
		Policy:        scheduler.DefaultPolicy(),
	}
}

// Engine 是调度内核的运行体。
type Engine struct {
	st      *store.Store
	secrets *secret.SecretBox
	cfg     Config
	deps    *dispatch.Deps
	poller  *syncpkg.Poller
	log     *slog.Logger

	mu           sync.Mutex
	reserved     map[int64]int64
	lastAssigned map[int64]int64
	// nodeSend 是"按节点限速"（策略 `node_rate_limit`）的滑动窗口：
	// node_id → 最近 60 秒内的投递时刻（升序）。
	//
	// 放在内存而不是库里，理由与 `lastAssigned` 相同（规格 C-04）：它只是
	// "过去一分钟我们发了多少"的近似，重启后从零开始最多让第一个分钟多发几个，
	// 不值得为它加一列并承担写放大。
	nodeSend map[int64][]int64

	// now 是可注入的时钟（测试用）。
	now func() int64
}

// New 构造引擎。
func New(st *store.Store, secrets *secret.SecretBox, cfg Config) *Engine {
	d := DefaultConfig()
	if cfg.Tick > 0 {
		d.Tick = cfg.Tick
	}
	if cfg.Batch > 0 {
		d.Batch = cfg.Batch
	}
	if cfg.PollInterval > 0 {
		d.PollInterval = cfg.PollInterval
	}
	if cfg.PollLimit > 0 {
		d.PollLimit = cfg.PollLimit
	}
	if cfg.VerifyTimeout > 0 {
		d.VerifyTimeout = cfg.VerifyTimeout
	}
	if cfg.ProbeInterval > 0 {
		d.ProbeInterval = cfg.ProbeInterval
	}
	if cfg.KeyOptions.MinStem > 0 || cfg.KeyOptions.ConfMin > 0 {
		d.KeyOptions = cfg.KeyOptions
	}
	if cfg.Policy.Weights != (scheduler.ScoreWeights{}) {
		d.Policy = cfg.Policy
	}

	poller := syncpkg.NewPoller(st, secrets, syncpkg.PollConfig{
		Interval: d.PollInterval, Limit: d.PollLimit,
	})

	return &Engine{
		st:      st,
		secrets: secrets,
		cfg:     d,
		deps: &dispatch.Deps{
			Store: st, Secrets: secrets, VerifyTimeout: d.VerifyTimeout,
		},
		poller:       poller,
		log:          slog.Default(),
		reserved:     map[int64]int64{},
		lastAssigned: map[int64]int64{},
		nodeSend:     map[int64][]int64{},
		now:          ids.NowSecs,
	}
}

// Poller 暴露内部轮询器（测试与冒烟脚本用）。
func (e *Engine) Poller() *syncpkg.Poller { return e.poller }

// Run 启动两条循环并阻塞到 ctx 取消。
func (e *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		e.scheduleLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		e.pollLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		e.probeLoop(ctx)
	}()
	<-ctx.Done()
	wg.Wait()
	e.log.Info("调度内核已停止")
}

// scheduleLoop 是调度循环。
func (e *Engine) scheduleLoop(ctx context.Context) {
	t := time.NewTicker(e.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.TickOnce(ctx); err != nil && ctx.Err() == nil {
				e.log.Warn("调度循环本轮失败", "err", err)
			}
		}
	}
}

// probeLoop 是节点健康探测循环。
//
// **启动时立刻探一轮**（而不是等 30 秒）：新部署的库里节点 `state` 全是 `unknown`，
// 而 C2 只允许 `online` —— 不先探一次的话，用户推的第一个任务会白等一个探测周期。
func (e *Engine) probeLoop(ctx context.Context) {
	e.probeOnce(ctx)
	t := time.NewTicker(e.cfg.ProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.probeOnce(ctx)
		}
	}
}

// probeOnce 探测全部节点并汇总日志。
func (e *Engine) probeOnce(ctx context.Context) {
	results, err := dispatch.ProbeAll(ctx, e.st, e.secrets)
	if err != nil && ctx.Err() == nil {
		e.log.Warn("节点探测失败", "err", err)
		return
	}
	online, degraded, offline := 0, 0, 0
	for _, r := range results {
		switch r.State {
		case "online":
			online++
		case "degraded":
			degraded++
		case "offline":
			offline++
		}
	}
	if len(results) > 0 {
		e.log.Debug("节点探测完成", "online", online, "degraded", degraded, "offline", offline)
	}
}

// pollLoop 是回读循环。
func (e *Engine) pollLoop(ctx context.Context) {
	t := time.NewTicker(e.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, err := e.poller.RunOnce(ctx)
			if err != nil && ctx.Err() == nil {
				e.log.Warn("回读循环本轮失败", "err", err)
				continue
			}
			if st.Applied > 0 || st.Missing > 0 || st.Errors > 0 {
				e.log.Debug("回读完成",
					"nodes", st.Nodes, "observed", st.Snapshots,
					"applied", st.Applied, "missing", st.Missing, "errors", st.Errors)
			}
		}
	}
}

// TickOnce 执行一轮调度（单独导出，便于测试与"发一条任务后立刻推进一轮"）。
//
// 事务边界严格按 `design/12 §9`：
//   - 事务内**只**写 task / task_attempt / task_group / group_binding_log / dispatch_log；
//   - 网络投递在**提交之后**；
//   - 投递结果用独立语句写回（不是共同不变量，best-effort + 可被 T06 纠正）。
func (e *Engine) TickOnce(ctx context.Context) error {
	enabled, err := e.schedulerEnabled(ctx)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}

	// 先按管理台的当前配置刷新策略：用户改完下一秒就生效，不需要重启。
	e.reloadPolicy(ctx)

	now := e.now()
	tasks, err := dispatch.LoadQueuedTasks(ctx, e.st, now, e.cfg.Batch)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}

	// tick 级预留：本 tick 内派发出去的任务要**一直占着**预留位，直到整轮结束。
	//
	// 为什么不能"派一个就回滚一个"（B1 的写法）：`nodes` 是本 tick 开头读的快照，
	// 里面的 `active` 不含本轮新派出的任务。若立刻回滚预留，同一 tick 的后续任务
	// 会认为节点仍然空闲 ⇒ 24 集同毫秒到达时会一起挤过 `max_concurrent`（B2-AC5）。
	baseline := e.snapshotReserved()
	defer e.restoreReserved(baseline)

	nodes, err := dispatch.LoadCandidates(ctx, e.st, e.cfg.Policy.Params.DLUnknownBytes)
	if err != nil {
		return err
	}
	nodes = e.applyRuntimeHints(nodes)
	nodes = e.applyRateLimit(nodes, now)
	if len(nodes) == 0 {
		// 一个节点都没有：不写 dispatch_log（否则每 tick 都往库里塞一行 queued）。
		// 用户看到的现象是"任务一直在排队"，这正是事实。
		return nil
	}

	for _, t := range tasks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.dispatchOne(ctx, t, nodes, now); err != nil {
			e.log.Warn("派发任务失败", "task", t.TaskID, "err", err)
		}
	}
	return nil
}

// dispatchOne 处理单个任务：分组识别 → 选点 → 建绑 → 投递 → 写回。
func (e *Engine) dispatchOne(ctx context.Context, t dispatch.QueuedTask, nodes []scheduler.Candidate, now int64) error {
	// 分组识别与名字兜底匹配都需要一个"可读名字"。
	//
	// ⚠️ `task.name` 可能是 NULL：aria2 `addUri` 不传 `out` 时，入库的
	// `Filename` 就是 nil（A 类刻意不拿 URL 猜文件名，免得 `files[].path` 谎报）。
	// 但**分组识别必须能拿到名字**，否则整条 URL 推送都不建组，
	// "同剧同节点"就成了空话。这里用 `url_raw` 兜底 —— `NormalizeName` 会
	// 从 URL 里取 basename，所以两种来源得到同一个 stem。
	nameForGroup := deref(t.Name)
	if nameForGroup == "" {
		nameForGroup = t.URLRaw
	}

	// ── 分组识别 ────────────────────────────────────────────────────────
	key, layer, conf := "", "", 0.0
	var group *scheduler.GroupRef
	if t.GroupID != nil {
		g, err := dispatch.LoadGroup(ctx, e.st, *t.GroupID)
		if err != nil {
			return err
		}
		group = g
		if g != nil {
			// 组已存在（失败重试 / 已被别的集建好）⇒ 直接沿用它的键。
			// 不补这一步会让 BindTx 既不写绑定日志、又把 task.group_id 清成 NULL。
			key = g.AffinityKey
		}
	}
	// ⚠️ 与规格 §1.3 的差异（有意）：规格要求"接单时"派生组键，本实现放在**调度时**。
	// 信息源完全相同（`task.name` 是推送请求里用户给的名字，入库后不会再变），
	// 但放在这里能避免在 4 个入库入口重复实现、也能不改 A 类的 `CreateTask`
	// （它刻意不用事务）。规格要防的是"事后去节点读任务名反推"，本实现不读节点。
	//
	// 两级尝试（第二级是 B3 补名需要的）：
	//
	//	① `task.name`（上游给的名字，最准）
	//	② `url_raw` 的 basename —— 上游只发 URL 时名字可能是 `01.ts`，
	//	   而 URL 里往往带着完整剧名。不回退这一级，"补名"就永远拿不到剧名。
	derived := false
	for _, src := range []string{nameForGroup, t.URLRaw} {
		if group != nil || derived || src == "" {
			continue
		}
		kr := grouping.DeriveKey(src, deref(t.SourceKey), e.cfg.KeyOptions)
		if kr.Key == "" {
			continue
		}
		key, layer, conf = kr.Key, kr.Layer, kr.Confidence
		derived = true
		g, err := dispatch.LoadGroupByKey(ctx, e.st, key)
		if err != nil {
			return err
		}
		group = g
		if kr.EpisodeNo != nil && t.EpisodeNo == nil {
			t.EpisodeNo = kr.EpisodeNo
		}
		_ = conf
	}

	// ── B3 判重 ─────────────────────────────────────────────────────────
	//
	// 位置有讲究：必须在**选点之前**。判重命中而要跳过的任务不该占用任何候选名额，
	// 也不该在 `dispatch_log` 里留下 `assigned`（那会让"这集下过没有"更难回答）。
	dkeys := dedupKeys(t)
	if skip, err := e.decideDedup(ctx, t, dkeys, now); err != nil {
		return err
	} else if skip {
		return nil
	}

	// ── 选点 ────────────────────────────────────────────────────────────
	route, err := dispatch.LoadSourceRoute(ctx, e.st, t.SourceKey)
	if err != nil {
		return err
	}

	in := scheduler.PickInput{
		TaskID:         t.TaskID,
		SourceKey:      t.SourceKey,
		SourceStrategy: route.Strategy,
		PinnedNodeID:   route.PinnedNodeID,
		Group:          group,
		Nodes:          nodes,
		Reserved:       e.snapshotReserved(),
		Now:            now,
		Cfg:            e.cfg.Policy,
	}
	in.BoundNode = boundCandidate(group, nodes)
	dec := scheduler.Pick(in)

	// ── B2 拆组：必须先留痕，再改走评分 ──────────────────────────────────
	//
	// 顺序不可换。若先选点、后补日志，那么"决策依据的组状态"与"库里的组状态"
	// 就不一致，B2-AC1（库字段与最新日志一致）会在并发时随机为红。
	if dec.Kind == scheduler.DecBindingInvalid && dec.Binding.Action == scheduler.BindSplit {
		if err := e.applySplit(ctx, dec.Binding, group, now); err != nil {
			return err
		}
		in.Group = group
		dec = scheduler.Pick(in)
	}

	// ── 排队分支 ────────────────────────────────────────────────────────
	if dec.Kind != scheduler.DecScored && dec.Kind != scheduler.DecAffinityHit {
		reason := dec.Reason
		if dec.Kind == scheduler.DecBindingInvalid || dec.Kind == scheduler.DecAffinityWait {
			if dec.Binding.Reason != "" {
				reason = dec.Binding.Reason
			}
		}
		var gid int64
		if group != nil {
			gid = group.GroupID
		}
		if err := e.st.InTx(ctx, func(txCtx context.Context, tx *sql.Tx) error {
			return grouping.QueueTx(txCtx, tx, grouping.QueueInput{
				TaskID: t.TaskID, GroupID: gid, Reason: reason,
				Candidates: len(dec.Evaluated), ScoreJSON: scoreJSON(dec), Now: now,
				WaitDeadline: dec.Binding.Deadline,
			})
		}); err != nil {
			return err
		}
		// 告警在事务之后写：`Alert` 只在"首次发现失效"时为真，所以不会刷屏。
		if dec.Binding.Alert {
			dispatch.LogEvent(ctx, e.st, "warn", "group", dec.NodeID, t.TaskID, dec.Binding.Note)
		}
		return nil
	}

	// ── 建绑 + 投递意图（事务）─────────────────────────────────────────
	var attemptNo int64
	dispatchReason := domain.ReasonScored
	if dec.Kind == scheduler.DecAffinityHit {
		dispatchReason = domain.ReasonAffinityHit
	}
	detail := detailJSON(key, layer, group)

	var groupAction *grouping.GroupAction
	if dec.Binding.Action == scheduler.BindRejoin {
		groupAction = rejoinAction(dec.Binding, group, dec.NodeID)
	}
	err = e.st.InTx(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := grouping.BindTx(txCtx, tx, grouping.BindInput{
			TaskID:         t.TaskID,
			AffinityKey:    key,
			Name:           nameForGroup,
			Season:         seasonOf(key),
			EpisodeNo:      t.EpisodeNo,
			NodeID:         dec.NodeID,
			DispatchReason: dispatchReason,
			Candidates:     len(dec.Evaluated),
			ScoreJSON:      scoreJSON(dec),
			Detail:         detail,
			GroupAction:    groupAction,
			DedupKeys:      toGroupingDedupKeys(dkeys),
			Now:            now,
		})
		if err != nil {
			return err
		}
		attemptNo = res.AttemptNo
		return nil
	})
	if err != nil {
		if err == grouping.ErrTaskAlreadyDispatched {
			return nil // 别的 tick 已经派发了，不是错误
		}
		return err
	}

	// 提交后才占位：`reserved_pending` 的定义是"已决定但未确认"。
	// 回滚交给 [TickOnce] 的 tick 级 defer（理由见那里的注释）。
	e.reserve(dec.NodeID, +1)
	e.markAssigned(dec.NodeID, now)
	e.recordSend(dec.NodeID, now)

	// ── 补名（B3，代理前置接收）──────────────────────────────────────────
	//
	// 只对 HTTP/FTP 补：BT 的落盘名由 torrent 元信息决定，传 filename 没有意义。
	// 纪律 5：**先落盘"本次补名的决策"，再下发** —— 否则补错了（把 A 剧的剧名
	// 补到 B 剧上）就无法事后追溯与批量纠正。
	filename := ""
	if t.Kind == "http" || t.Kind == "ftp" || t.Kind == "" {
		nm := naming.Build(naming.Input{
			Stem:       stemOf(key),
			Season:     seasonOf(key),
			Episode:    derefI64(t.EpisodeNo),
			OriginName: deref(t.Name),
			URL:        t.URLRaw,
		})
		filename = nm.Filename
		if filename != "" {
			dispatch.LogEventDetail(ctx, e.st, "info", "group", dec.NodeID, t.TaskID,
				fmt.Sprintf("补名：%s → %s（来源=grouping_stem）", nm.Origin, filename),
				nm.Origin)
		}
	}

	// ── 投递（事务外，网络 I/O）─────────────────────────────────────────
	res := dispatch.Deliver(ctx, e.deps, dispatch.DeliverInput{
		NodeID: dec.NodeID, TaskID: t.TaskID, URL: t.URLRaw,
		Name: nameForGroup, Filename: filename,
		AttemptNo: attemptNo, MaxAttempts: t.MaxAttempts, Now: now,
		RetryBaseSec:       e.cfg.Policy.RetryBaseSec,
		RetryCapSec:        e.cfg.Policy.RetryCapSec,
		ThrottleMultiplier: e.cfg.Policy.ThrottleMultiplier,
	})

	// ── 结果写回 ────────────────────────────────────────────────────────
	if res.ErrorClass == nil {
		if err := dispatch.MarkDelivered(ctx, e.st, t.TaskID, attemptNo, res, e.now()); err != nil {
			e.log.Warn("写回投递成功失败（T06 会纠正）", "task", t.TaskID, "err", err)
		}
		dispatch.LogEvent(ctx, e.st, "info", "task", dec.NodeID, t.TaskID,
			fmt.Sprintf("已投递到节点 #%d，node_task_id=%s", dec.NodeID, res.NodeTaskID))
		return nil
	}

	if err := dispatch.MarkDeliverFailed(ctx, e.st, t.TaskID, attemptNo, res, e.now()); err != nil {
		e.log.Warn("写回投递失败失败", "task", t.TaskID, "err", err)
	}
	level := "warn"
	if !res.Retryable {
		level = "error"
	}
	dispatch.LogEvent(ctx, e.st, level, "task", dec.NodeID, t.TaskID,
		fmt.Sprintf("投递失败 %s（可重试=%v）：%s", derefClass(res.ErrorClass), res.Retryable, res.Detail))
	return nil
}

// schedulerEnabled 读运行期开关（`app_config` 的 `scheduler.enabled`）。
//
// 默认 **on**：这是与规格 C-09 建议值（缺省 off）的**有意不同**。
// 理由：这个开关存在的目的是"出问题能一键停下"，不是"默认别动"——
// 默认关着会让用户装完镜像、推了任务、看到一直排队，然后以为项目没做完。
// 关掉的方式是 `INSERT OR REPLACE INTO app_config VALUES('scheduler.enabled','off',...)`，
// 或环境变量 `DISPATCH_SCHEDULER=off`（优先级更高，便于容器一键停）。
func (e *Engine) schedulerEnabled(ctx context.Context) (bool, error) {
	if v, ok := os.LookupEnv("DISPATCH_SCHEDULER"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "off", "0", "false", "no":
			return false, nil
		case "on", "1", "true", "yes":
			return true, nil
		}
	}
	rows, err := e.st.Query(ctx,
		`SELECT value FROM app_config WHERE key = 'scheduler.enabled'`)
	if err != nil {
		return true, nil // 读配置失败不该让整个调度停摆；按默认 on 继续
	}
	if len(rows) == 0 {
		return true, nil
	}
	v := ""
	if s, ok := rows[0][0].(string); ok {
		v = s
	}
	switch v {
	case "off", "0", "false", "no":
		return false, nil
	default:
		return true, nil
	}
}

// reserve 调整内存预留（可正可负）。
func (e *Engine) reserve(nodeID, delta int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.reserved[nodeID] + delta
	if v <= 0 {
		delete(e.reserved, nodeID)
		return
	}
	e.reserved[nodeID] = v
}

// markAssigned 记录节点最近被选中时刻（tie-break 与防抖动用）。
func (e *Engine) markAssigned(nodeID, now int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastAssigned[nodeID] = now
}

// applyRateLimit 把"本分钟已发够"的节点标成 `RateLimited`（C6）。
//
// 只在策略给出的上限 >0 时生效；上限为 0 表示"用户关掉了这条策略"，
// 此时一个标记都不打（连 map 都不碰，避免留下永远不清理的键）。
func (e *Engine) applyRateLimit(nodes []scheduler.Candidate, now int64) []scheduler.Candidate {
	perMin := e.cfg.Policy.NodeRateLimitPerMin
	if perMin <= 0 {
		return nodes
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := now - 60
	for i := range nodes {
		sends := e.nodeSend[nodes[i].NodeID]
		n := 0
		for _, ts := range sends {
			if ts > cutoff {
				n++
			}
		}
		if int64(n) >= perMin {
			nodes[i].RateLimited = true
		}
	}
	return nodes
}

// recordSend 记一次投递（滑动窗口用）。
func (e *Engine) recordSend(nodeID, now int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := now - 60
	sends := e.nodeSend[nodeID]
	// 顺手裁掉过期项：窗口最多 60 秒，元素个数上限就是 perMin（≤600），不会涨。
	keep := sends[:0]
	for _, ts := range sends {
		if ts > cutoff {
			keep = append(keep, ts)
		}
	}
	e.nodeSend[nodeID] = append(keep, now)
}

// restoreReserved 把预留表恢复成 tick 开始时的快照。
//
// 为什么不逐个 `reserve(-1)`：本轮可能因为错误提前返回，逐条回滚会漏。
// 直接覆盖成快照是幂等的，且语义正好是"本轮的所有临时占位作废"。
func (e *Engine) restoreReserved(baseline map[int64]int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reserved = baseline
}

// boundCandidate 找出组绑定节点对应的候选（无绑定或节点已删 ⇒ nil）。
func boundCandidate(g *scheduler.GroupRef, nodes []scheduler.Candidate) *scheduler.Candidate {
	if g == nil || g.BoundNodeID == nil {
		return nil
	}
	for i := range nodes {
		if nodes[i].NodeID == *g.BoundNodeID {
			return &nodes[i]
		}
	}
	return nil
}

// snapshotReserved 复制一份预留表（选点是纯函数，不该持有锁）。
func (e *Engine) snapshotReserved() map[int64]int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[int64]int64, len(e.reserved))
	for k, v := range e.reserved {
		out[k] = v
	}
	return out
}

// applyRuntimeHints 把内存态（last_assigned）贴到候选上。
//
// 数据库里没有 `last_assigned_at` 这一列（规格 C-04），所以它只能从内存补。
func (e *Engine) applyRuntimeHints(nodes []scheduler.Candidate) []scheduler.Candidate {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range nodes {
		if v, ok := e.lastAssigned[nodes[i].NodeID]; ok {
			nodes[i].LastAssignedAt = v
		}
	}
	return nodes
}

// scoreJSON 序列化决策留痕。
//
// 额外带 `binding` 一栏：B2 之后"为什么这集没落到绑定节点"的原因（strong 等待中 /
// weak 立即拆 / manual 只告警）必须能从 `dispatch_log` 里读出来，
// 否则排障只能靠猜。
func scoreJSON(d scheduler.Decision) string {
	extra := map[string]any{"weights": scheduler.DefaultWeights()}
	if d.Binding.Action != scheduler.BindHold || d.Binding.Reason != "" {
		extra["binding"] = map[string]any{
			"action": d.Binding.Action.String(),
			"reason": d.Binding.Reason,
		}
	}
	return grouping.MarshalEvaluated(d.Kind.String(), d.Reason, d.NodeID, d.Evaluated, extra)
}

// detailJSON 生成 `group_binding_log.detail`（规格 C-17 建议的 JSON）。
func detailJSON(key, layer string, g *scheduler.GroupRef) *string {
	if key == "" {
		return nil
	}
	m := map[string]any{"affinity_key": key, "layer": layer}
	if g != nil {
		m["group_id"] = g.GroupID
		m["binding_epoch_before"] = g.BindingEpoch
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

// stemOf 从 affinity_key（`name:{stem}|{season}`）里取剧名段。
func stemOf(key string) string {
	if key == "" {
		return ""
	}
	s := strings.TrimPrefix(key, "name:")
	if i := lastPipe(s); i >= 0 {
		s = s[:i]
	}
	return s
}

// derefI64 取 int64 指针的值（nil ⇒ 0）。
func derefI64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// seasonOf 从 affinity_key 里取季段（`...|S01`）。
func seasonOf(key string) string {
	if key == "" {
		return ""
	}
	// 键里最后一个 `|` 之后就是季（无季时为空）
	if i := lastPipe(key); i >= 0 {
		s := key[i+1:]
		if len(s) == 3 && (s[0] == 'S' || s[0] == 's') {
			return s
		}
	}
	return ""
}

// lastPipe 返回最后一个 `|` 的下标（无则 -1）。
func lastPipe(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '|' {
			return i
		}
	}
	return -1
}

// groupIDOf 取组 id（nil 安全）。
func groupIDOf(g *scheduler.GroupRef) int64 {
	if g == nil {
		return 0
	}
	return g.GroupID
}

// deref 取字符串指针的值（nil ⇒ 空串）。
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefClass 取错误分类的字符串（nil ⇒ "无"）。
func derefClass(c *domain.ErrorClass) string {
	if c == nil {
		return "无"
	}
	return string(*c)
}

// SchedulerEnabledByEnv 只报告**环境变量**层面的开关（不查库）。
//
// 单独导出是为了让启动日志能立刻说清"这次启动会不会下发" ——
// 而库里的 `app_config` 要等 Store 打开后才能读，那时日志早就打完了。
func SchedulerEnabledByEnv() bool {
	v, ok := os.LookupEnv("DISPATCH_SCHEDULER")
	if !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no":
		return false
	default:
		return true
	}
}
