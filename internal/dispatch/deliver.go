package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
	"github.com/LIUBANGJIAN/download-gateway/internal/secret"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// Deps 是投递所需的依赖（全部可注入，便于用假客户端做单测）。
type Deps struct {
	Store   *store.Store
	Secrets *secret.SecretBox
	// NewClient 构造节点客户端（默认 [protocol.NewClient]）。
	NewClient func(p protocol.NodeProfile, timeoutSecs uint64) (*protocol.Client, error)
	// VerifyTimeout 是 read-after-write 的等待上限（默认 10s，`design/02 §2.1.4`）。
	VerifyTimeout time.Duration
	// VerifyPoll 是回查间隔（默认 1s）。
	VerifyPoll time.Duration
	// Jitter 返回 [0.8,1.2) 的抖动系数（测试可注入定值）。
	Jitter func() float64
	// SaveFolders 缓存节点的默认下载目录（可为 nil，表示不缓存）。
	//
	// BitComet 2.22+ 要求 `add` 显式带 `save_folder`，而目录只能问节点。
	// 缓存把"每任务一次往返"摊成"每节点 5 分钟一次"。
	SaveFolders *SaveFolderCache
}

// DeliverInput 是一次投递的输入。
type DeliverInput struct {
	NodeID int64
	// TaskID 是代理侧任务 id（写日志用）。
	TaskID string
	URL    string
	// Name 用于"没有拿到 task_id 时"的名字兜底匹配。
	Name string
	// Filename 是**代理前置接收的补名**（B3）：非空时随 add 请求下发，只改文件名。
	//
	// 空串表示不补（拿不到剧名时的正确行为 —— `design/02 §4.5(5)` 纪律 1）。
	Filename string
	// RetryBaseSec / RetryCapSec 是退避参数（策略 `retry_backoff` 驱动；0 ⇒ 用默认 30/1800）。
	RetryBaseSec int64
	RetryCapSec  int64
	// ThrottleMultiplier 是限流冷却的额外倍率（策略 `throttle_backoff` 驱动；0 ⇒ 1.0）。
	ThrottleMultiplier float64
	// StartLater 是否延迟启动。
	StartLater bool
	// MaxConnections 最大连接数（0 表示不传）。
	MaxConnections int64
	// AttemptNo 本次尝试号（退避计算用）。
	AttemptNo int64
	// MaxAttempts 最大尝试次数（超过即终态失败）。
	MaxAttempts int64
	// Now 当前时刻（秒）。
	Now int64
	// jitter 是退避抖动源（由 [Deliver] 从 Deps 注入，不对外暴露）。
	jitter func() float64
}

// DeliverResult 是投递结果。
type DeliverResult struct {
	// NodeTaskID 节点侧任务 id（规范化成字符串；空表示没拿到）。
	NodeTaskID string
	// AttemptState ∈ created / running（仅成功时有意义）。
	AttemptState string
	// ErrorClass 失败分类（成功时为 nil）。
	ErrorClass *domain.ErrorClass
	// Retryable 是否值得重试。
	Retryable bool
	// NextRetryAt 下次可重试时刻（0 表示不重试）。
	NextRetryAt int64
	// Detail 人类可读细节（写 `task.error_message`）。
	Detail string
}

// Delay 计算指数退避：`min(BASE·2^(attempt-1), CAP) · U(0.8,1.2)`。
//
// BASE=30s / CAP=1800s 出自 `design/02 §2.1.4`。抖动不是美观问题：
// 没有抖动时，一批同时失败的任务会在同一秒一起回来重投，把节点再打一次。
func Delay(attempt int64, jitter func() float64) time.Duration {
	return DelayWith(attempt, 30, 1800, 1, jitter)
}

// DelayWith 是 [Delay] 的**参数化**版本（B1.5：由策略 `retry_backoff` 驱动）。
//
// 三个参数为 0/负 时各自回落默认值 —— 这样"零值 Policy"与"默认策略"行为一致，
// 任何忘填字段的调用点都不会意外改变退避节奏。
//
// `throttleMul` 只对**限流类**失败生效（由调用方决定是否传 >1 的倍率，
// 见 [failWith]）：被限流时退得更久是合理的，而对 404 退更久只是浪费。
func DelayWith(attempt, baseSec, capSec int64, throttleMul float64, jitter func() float64) time.Duration {
	if baseSec <= 0 {
		baseSec = 30
	}
	if capSec <= 0 {
		capSec = 1800
	}
	if throttleMul <= 0 {
		throttleMul = 1
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 20 { // 防 1<<attempt 溢出
		attempt = 20
	}
	base := time.Duration(baseSec) * time.Second
	capDur := time.Duration(capSec) * time.Second
	d := base * time.Duration(int64(1)<<(attempt-1))
	if d > capDur || d <= 0 {
		d = capDur
	}
	j := 1.0
	if jitter != nil {
		j = jitter()
	}
	return time.Duration(float64(d) * j * throttleMul)
}

// Deliver 执行一次投递：分类端点 → 下发 → (必要时) read-after-write。
//
// **必须在建绑事务提交之后调用**（`design/12 §9` 硬约束④：事务内禁止网络 I/O）。
//
// 返回值永远不是 nil error 才叫失败：网络/节点错误被**分类**后放进
// `DeliverResult.ErrorClass`，由调用方统一写库。这样「失败也是一等公民」，
// 不会因为一处 `return err` 就把任务静默留在 `dispatching`。
func Deliver(ctx context.Context, d *Deps, in DeliverInput) DeliverResult {
	in.jitter = d.ensureJitter()
	cred, err := LoadNodeCredential(ctx, d.Store, in.NodeID)
	if err != nil {
		return failWith(domain.ErrNodeDown, false, 0, err.Error(), in)
	}
	plain, err := d.Secrets.Open(cred.PassEnc)
	if err != nil {
		// 主密钥变过（换了 DISPATCH_SECRET_KEY / 密钥文件丢失）—— 节点级配置错误，
		// 重试不会好，但也不该把任务判死：留给人去修密钥后重新派发。
		return failWith(domain.ErrUnknown, false, 0, "解密节点密码失败: "+err.Error(), in)
	}

	profile := protocol.NewNodeProfile(cred.BaseURL, cred.User, plain)
	if strings.TrimSpace(cred.DeviceName) != "" {
		profile.DeviceName = cred.DeviceName
	}

	newClient := d.NewClient
	if newClient == nil {
		newClient = protocol.NewClient
	}
	cl, err := newClient(profile, 30)
	if err != nil {
		return failWith(domain.ErrNodeDown, true, in.Now, "构造节点客户端失败: "+err.Error(), in)
	}

	kind := protocol.ClassifyAddTarget(in.URL)

	// BitComet 2.22+ 要求显式 save_folder（不传 → FATAL_ERROR: save_folder missing）。
	// 目录因机器而异，只能问节点；查不到就退回"不传"的旧行为，让旧版节点照常工作。
	taskType := "http"
	if kind == protocol.AddKindTorrent {
		taskType = "bt"
	}
	var saveFolder *string
	if folder, ok := resolveSaveFolder(ctx, d, in.NodeID, cl, taskType); ok {
		saveFolder = &folder
	}

	var resp map[string]any
	if kind == protocol.AddKindTorrent {
		resp, err = cl.AddTorrentLinks(ctx, in.URL, saveFolder, in.StartLater)
	} else {
		var fname *string
		if in.Filename != "" {
			f := in.Filename
			fname = &f
		}
		resp, err = cl.AddHTTPTaskNamed(ctx, in.URL, fname, saveFolder, in.StartLater)
	}
	if err != nil {
		// save_folder 相关的失败多半是缓存了过期目录（节点上改了下载路径），
		// 丢掉缓存让下次重新问，避免这个错误被缓存粘住直到 TTL 到期。
		if msg := err.Error(); strings.Contains(msg, "save_folder") {
			d.SaveFolders.invalidate(in.NodeID)
		}
		return classifyError(err, in)
	}

	// 成功响应里可能带 task_id（`torrent_links/add` 是异步接口，某些版本不带）。
	nodeTaskID := respTaskID(resp)

	if nodeTaskID != "" {
		// 拿到了 id 就算建成：`attempt.state` 迁移到 `created`。
		// 不再做 10s 阻塞回查 —— 那是"返回成功却没有 id"时才需要的兜底；
		// 拿 id 还等 10s，会让一个 50 任务的批次白等 8 分钟。真正的核对交给 T06 轮询
		// （它本来就要拉列表，顺手就能发现"id 不在列表里"）。
		return DeliverResult{NodeTaskID: normalizeTaskID(nodeTaskID), AttemptState: "created"}
	}

	// 未拿到 id：按名字在 T_verify 内回查一次。命中 ⇒ 成功；未命中 ⇒ silently_not_created。
	if id, state, ok := verifyByName(ctx, cl, in.Name, d.verifyTimeout(), d.verifyPoll()); ok {
		return DeliverResult{NodeTaskID: id, AttemptState: state}
	}
	return failWith(domain.ErrSilentlyNotCreated, true, in.Now, "节点返回成功但回查不到任务", in)
}

// verifyByName 在 T_verify 内轮询任务列表，按名字找到刚投的任务。
func verifyByName(ctx context.Context, cl *protocol.Client, name string, timeout, poll time.Duration) (string, string, bool) {
	if strings.TrimSpace(name) == "" {
		return "", "", false
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		resp, err := protocol.FetchTaskListParams(ctx, cl, protocol.TaskListParams{StateGroup: "ALL", Limit: 200})
		if err == nil {
			tasks := protocol.ParseTaskList(resp)
			if t, found := protocol.FindByName(tasks, name); found {
				state := "created"
				if strings.EqualFold(t.State, "running") || strings.EqualFold(t.State, "active") {
					state = "running"
				}
				return normalizeTaskID(t.TaskID), state, true
			}
		}
		if time.Now().After(deadline) {
			return "", "", false
		}
		select {
		case <-ctx.Done():
			return "", "", false
		case <-time.After(poll):
		}
	}
}

// classifyError 把客户端错误映射成 `error_class` + 可重试性。
//
// 依据 `design/02 §2.1.4` 的错误分类表：
//   - 4xx（403/404 不可区分）⇒ `server_file_not_found`，**终态**（不重试、不换节点）
//   - `out_of_retry` ⇒ 默认回原节点重试
//   - TCP/连接失败 ⇒ `node_down`，可重试
//   - 其余 ⇒ `unknown`，可重试但要有上限
func classifyError(err error, in DeliverInput) DeliverResult {
	var remote *protocol.RemoteError
	if errors.As(err, &remote) {
		class := domain.MapErrorClass(remote.Code)
		switch class {
		case domain.ErrServerFileNotFound:
			// 终态：源文件不存在，重试一百次也还是不存在。
			return failWith(class, false, 0, remote.Error(), in)
		case domain.ErrOutOfRetry, domain.ErrThrottled:
			return failWith(class, true, in.Now, remote.Error(), in)
		default:
			if class == domain.ErrUnknown {
				// 把节点的原始 error_code 留在 message 里：分类被归一时信息不能一起丢。
				return failWith(domain.ErrUnknown, true, in.Now, remote.Error(), in)
			}
			return failWith(class, true, in.Now, remote.Error(), in)
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return failWith(domain.ErrNodeDown, true, in.Now, err.Error(), in)
	}
	if errors.Is(err, protocol.ErrTokenRejected) || errors.Is(err, protocol.ErrNotLoggedIn) {
		// 认证失败是节点级配置问题：重试无意义，但也别判任务死。
		return failWith(domain.ErrUnknown, false, 0, err.Error(), in)
	}
	return failWith(domain.ErrUnknown, true, in.Now, err.Error(), in)
}

// failWith 组装失败结果，并按"尝试次数是否耗尽"决定是否给出下次重试时刻。
//
// 耗尽 ⇒ `Retryable=false`，调用方写 `failed_terminal`（**不是** `failed`：
// INV-A1 按 `failed_terminal` 排除终态，写错会被误判成违规）。
func failWith(class domain.ErrorClass, retryable bool, now int64, detail string, in DeliverInput) DeliverResult {
	res := DeliverResult{ErrorClass: &class, Retryable: retryable, Detail: detail}
	if !retryable {
		return res
	}
	next := in.AttemptNo + 1
	if in.MaxAttempts > 0 && next > in.MaxAttempts {
		res.Retryable = false
		return res
	}
	if now == 0 {
		now = time.Now().Unix()
	}
	mul := 1.0
	if class == domain.ErrThrottled {
		// 限流类失败才吃额外倍率（`policy.throttle_backoff`）。
		mul = in.ThrottleMultiplier
		if mul <= 0 {
			mul = 1
		}
	}
	res.NextRetryAt = now + int64(DelayWith(next, in.RetryBaseSec, in.RetryCapSec, mul, in.jitter).Seconds())
	return res
}

// respTaskID 从 `add` 响应里取节点任务 id（字符串或数字都接受）。
//
// `torrent_links/add` 是**异步**接口，某些版本不返回 id（`design/02 §2.1.4` 已记录），
// 所以"取不到"是正常分支、不是错误 —— 调用方会走名字回查。
func respTaskID(resp map[string]any) string {
	for _, k := range []string{"task_id", "taskid", "id"} {
		v, ok := resp[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case string:
			if strings.TrimSpace(x) != "" {
				return x
			}
		case float64:
			return fmt.Sprintf("%.0f", x)
		case int64:
			return fmt.Sprintf("%d", x)
		}
	}
	return ""
}

// normalizeTaskID 把节点 id 规范成字符串（`design/02 §2.1.4`「node_task_id 类型统一」）。
//
// 禁止混用 INTEGER/TEXT：`123` 与 `"123"` 在比较、转发、日志里是两种东西，
// 而 DDL 已经是 TEXT —— 这里再统一一次，避免将来有人"顺手"传个 int64。
func normalizeTaskID(s string) string {
	return strings.TrimSpace(s)
}

// Logf 是包级日志 seam（测试可静音）。
var Logf = func(format string, args ...any) {
	slog.Info(fmt.Sprintf(format, args...))
}

// jitterDefault 是默认抖动源（math/rand 的顶层函数在 Go 1.20+ 已自动播种）。
func jitterDefault() float64 { return 0.8 + rand.Float64()*0.4 }

// EnsureJitter 在 Deps 未注入抖动源时兜底。
func (d *Deps) ensureJitter() func() float64 {
	if d.Jitter != nil {
		return d.Jitter
	}
	return jitterDefault
}

// verifyTimeout 返回回查超时（默认 10s）。
func (d *Deps) verifyTimeout() time.Duration {
	if d.VerifyTimeout > 0 {
		return d.VerifyTimeout
	}
	return 10 * time.Second
}

// verifyPoll 返回回查间隔（默认 1s）。
func (d *Deps) verifyPoll() time.Duration {
	if d.VerifyPoll > 0 {
		return d.VerifyPoll
	}
	return time.Second
}
