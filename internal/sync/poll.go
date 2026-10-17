// Package sync 是 T06 回读同步：把节点上的真实进度搬回代理库。
//
// 为什么必须有它：B1 之前，"任务已下发"只是代理侧的一厢情愿。没有回读，
// `task.permillage` 永远是 0、`internal_state` 永远停在 `dispatching`，
// 用户看到的是"投出去了但没有任何进展"。这一包是把"投递成功"变成"真的在下载"的那一步。
//
// 设计依据：`design/02 §2.1.5`（分级轮询）、`design/02 §4.1.6`（状态映射）、
// `design/02 §2.1.5`（进度写入时间戳单调性，防 D-04）。
package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
	"github.com/LIUBANGJIAN/download-gateway/internal/secret"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// PollConfig 是轮询配置。
type PollConfig struct {
	// Interval 是单层轮询间隔（默认 15s）。
	//
	// ⚠️ 与设计的**有意差异**：`design/02 §2.1.5` 要求三级轮询
	// （ACTIVE 15s / UNCOMPLETED 60s / ALL 300s）。B1 先做**单层**
	// （所有 online 节点每 15s 拉一次 ALL），理由是分级收益只在「节点数 × 任务数」
	// 很大时才显现，而三级状态机本身会引入"某任务落在哪个层级"的额外状态。
	// 这一取舍写在 `doc/远端构建验证报告.md` 的已知限制里，不假装做了分级。
	Interval time.Duration
	// Limit 是单次拉取的任务上限（默认 500，`design/02 §2.1.5` 的静默层分页大小）。
	Limit int
}

// DefaultPollConfig 返回默认轮询配置。
func DefaultPollConfig() PollConfig {
	return PollConfig{Interval: 15 * time.Second, Limit: 500}
}

// Poller 按节点拉任务列表并对账。
type Poller struct {
	st        *store.Store
	secrets   *secret.SecretBox
	newClient func(p protocol.NodeProfile, timeoutSecs uint64) (*protocol.Client, error)
	cfg       PollConfig
	// logf 是日志 seam（测试可静音）。
	logf func(format string, args ...any)
}

// NewPoller 构造轮询器。
func NewPoller(st *store.Store, secrets *secret.SecretBox, cfg PollConfig) *Poller {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 500
	}
	return &Poller{
		st: st, secrets: secrets, cfg: cfg,
		newClient: protocol.NewClient,
		logf:      func(f string, a ...any) { slog.Info(fmt.Sprintf(f, a...)) },
	}
}

// SetClientFactory 注入客户端构造函数（测试用）。
func (p *Poller) SetClientFactory(f func(protocol.NodeProfile, uint64) (*protocol.Client, error)) {
	if f != nil {
		p.newClient = f
	}
}

// SetLogf 注入日志函数（测试用）。
func (p *Poller) SetLogf(f func(string, ...any)) {
	if f != nil {
		p.logf = f
	}
}

// Stats 是一轮对账的统计（给日志与冒烟断言用）。
type Stats struct {
	// Nodes 本轮拉取成功的节点数。
	Nodes int
	// Snapshots 本轮观察到的节点任务总数。
	Snapshots int
	// Applied 被写入库的任务数。
	Applied int
	// Missing 在节点列表里找不到的代理任务数（只记日志，不改状态）。
	Missing int
	// Errors 拉取失败的节点数。
	Errors int
}

// RunOnce 对所有 enabled 且 online 的节点各拉一次并逐个对账。
//
// 只拉 `online`：`degraded`/`offline` 的节点连不上，每轮都去撞一遍只会把
// 日志刷满并拖慢整轮（它们的恢复由健康探测负责，不归这里）。
func (p *Poller) RunOnce(ctx context.Context) (Stats, error) {
	var st Stats

	nodes, err := p.loadOnlineNodes(ctx)
	if err != nil {
		return st, err
	}

	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		snap, err := p.fetchOne(ctx, n)
		if err != nil {
			st.Errors++
			p.logf("回读节点 %s(#%d) 失败：%v", n.Alias, n.NodeID, err)
			continue
		}
		st.Nodes++
		st.Snapshots += len(snap.Tasks)

		applied, missing, err := Reconcile(ctx, p.st, n.NodeID, snap, p.cfg.Interval)
		if err != nil {
			return st, err
		}
		st.Applied += applied
		st.Missing += missing
	}
	return st, nil
}

// nodeEndpoint 是拉取所需的节点连接信息（含密文）。
type nodeEndpoint struct {
	NodeID     int64
	Alias      string
	BaseURL    string
	User       string
	PassEnc    string
	DeviceName string
}

// loadOnlineNodes 读全部"启用且在线上"的节点。
func (p *Poller) loadOnlineNodes(ctx context.Context) ([]nodeEndpoint, error) {
	rows, err := p.st.Query(ctx,
		`SELECT node_id, alias, base_url, user, pass_enc, device_name
		   FROM node WHERE enabled = 1 AND state = 'online' ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("sync: 读在线节点: %w", err)
	}
	out := make([]nodeEndpoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, nodeEndpoint{
			NodeID:     toInt64(r[0]),
			Alias:      toString(r[1]),
			BaseURL:    toString(r[2]),
			User:       toString(r[3]),
			PassEnc:    toString(r[4]),
			DeviceName: toString(r[5]),
		})
	}
	return out, nil
}

// fetchOne 拉一个节点的任务快照。
func (p *Poller) fetchOne(ctx context.Context, n nodeEndpoint) (Snapshot, error) {
	plain, err := p.secrets.Open(n.PassEnc)
	if err != nil {
		return Snapshot{}, fmt.Errorf("解密节点密码: %w", err)
	}
	profile := protocol.NewNodeProfile(n.BaseURL, n.User, plain)
	if n.DeviceName != "" {
		profile.DeviceName = n.DeviceName
	}
	cl, err := p.newClient(profile, 30)
	if err != nil {
		return Snapshot{}, fmt.Errorf("构造客户端: %w", err)
	}
	return FetchSnapshot(ctx, cl, n.NodeID, SGAll, p.cfg.Limit)
}

// StateGroup 是 `task_list/get` 的 `state_group` 取值。
type StateGroup string

const (
	// SGActive 下载中/做种中。
	SGActive StateGroup = "ACTIVE"
	// SGUncompleted 未完成（排队/暂停）。
	SGUncompleted StateGroup = "UNCOMPLETED"
	// SGAll 全量。
	SGAll StateGroup = "ALL"
)

// Snapshot 是一次节点列表读取的结果。
type Snapshot struct {
	NodeID     int64
	Group      StateGroup
	ObservedAt int64
	Tasks      []protocol.RemoteTask
}

// FetchSnapshot 拉一页快照并打上观察时刻。
//
// `ObservedAt` 必须由**代理侧**决定（不是节点上报的），否则节点时钟漂移
// 会让进度单调性判断（§7.3）失效：一个慢 5 分钟的节点会永远"不更新"。
func FetchSnapshot(ctx context.Context, cl *protocol.Client, nodeID int64, sg StateGroup, limit int) (Snapshot, error) {
	resp, err := protocol.FetchTaskListParams(ctx, cl, protocol.TaskListParams{
		StateGroup: string(sg), Limit: limit,
	})
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		NodeID:     nodeID,
		Group:      sg,
		ObservedAt: time.Now().Unix(),
		Tasks:      protocol.ParseTaskList(resp),
	}, nil
}

// toString 把列值转字符串。
func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

// toInt64 把列值转 int64。
func toInt64(v any) int64 {
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
	default:
		return 0
	}
}
