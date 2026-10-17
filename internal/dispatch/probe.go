package dispatch

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
	"github.com/LIUBANGJIAN/download-gateway/internal/secret"
	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// ProbeResult 是一次节点探测的结果。
type ProbeResult struct {
	NodeID    int64
	Alias     string
	State     string // online / degraded / offline
	TCPOK     bool
	APIOK     bool
	LatencyMs int64
	Detail    string
}

// ProbeAndPersist 探测一个节点并把结论写回 `node` / `node_health`。
//
// 探测分两层，缺一不可：
//
//	L1 TCP：端口通不通（离线的最廉价判据）
//	L2 API：登录 + `/api/config/about/get` 能不能拿到业务响应
//
// **只做 L1 会把"端口开着但 App 访问关闭"判成健康** —— 那种节点能连上、
// 但每一个 `add` 都会被拒绝，任务会在"看起来有机器可用"和"投不进去"之间反复。
// 这正是 `design/02 §2.1.2` 要求把 `APP_ACCESS_DISABLED` 判成 `degraded` 的原因。
func ProbeAndPersist(ctx context.Context, st *store.Store, box *secret.SecretBox, nodeID int64) (ProbeResult, error) {
	cred, err := LoadNodeCredential(ctx, st, nodeID)
	if err != nil {
		return ProbeResult{}, err
	}
	res := ProbeResult{NodeID: nodeID, Alias: cred.Alias}

	host, port := splitHostPort(cred.BaseURL)
	if host == "" {
		res.State = "offline"
		res.Detail = "base_url 无法解析"
	} else {
		start := time.Now()
		conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
		res.LatencyMs = time.Since(start).Milliseconds()
		if dialErr != nil {
			res.State = "offline"
			res.Detail = "TCP 不通: " + dialErr.Error()
		} else {
			_ = conn.Close()
			res.TCPOK = true
		}
	}

	// L2：TCP 通了才有必要登录（否则每次探测都要白等一次登录超时）。
	if res.TCPOK {
		plain, decErr := box.Open(cred.PassEnc)
		if decErr != nil {
			res.State = "degraded"
			res.Detail = "解密节点密码失败: " + decErr.Error()
		} else {
			profile := protocol.NewNodeProfile(cred.BaseURL, cred.User, plain)
			if cred.DeviceName != "" {
				profile.DeviceName = cred.DeviceName
			}
			cl, cErr := protocol.NewClient(profile, 10)
			if cErr != nil {
				res.State = "degraded"
				res.Detail = "构造客户端失败: " + cErr.Error()
			} else if _, aErr := cl.About(ctx); aErr != nil {
				// 认证失败 / App 访问关闭 / 业务错误码，都在这里落 degraded。
				res.State = "degraded"
				res.Detail = "API 探测失败: " + aErr.Error()
			} else {
				res.APIOK = true
				res.State = "online"
			}
		}
	}

	if err := persistProbe(ctx, st, res); err != nil {
		return res, err
	}
	return res, nil
}

// ProbeAll 探测全部启用节点，返回每个节点的结论。
//
// 禁用（`enabled=0`）的节点也探：用户可能只是暂时不想用它，
// 但管理台仍应看到它现在通不通。这不影响调度 —— C1 会把它挡在候选之外。
func ProbeAll(ctx context.Context, st *store.Store, box *secret.SecretBox) ([]ProbeResult, error) {
	rows, err := st.Query(ctx, `SELECT node_id FROM node ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("dispatch: 读节点清单: %w", err)
	}
	out := make([]ProbeResult, 0, len(rows))
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		id := asInt64(r[0])
		res, err := ProbeAndPersist(ctx, st, box, id)
		if err != nil {
			out = append(out, ProbeResult{NodeID: id, State: "unknown", Detail: err.Error()})
			continue
		}
		out = append(out, res)
	}
	return out, nil
}

// persistProbe 写回节点健康度。
//
// `state_since` 只在**状态真的变了**时才更新：它表达的是"这个状态从什么时候开始"，
// 每次探测都刷一遍会让"离线多久了"这个最有用的问题永远答不出来。
func persistProbe(ctx context.Context, st *store.Store, r ProbeResult) error {
	now := time.Now().Unix()
	_, err := st.Execute(ctx,
		`UPDATE node
		    SET state = ?1,
		        state_since = CASE WHEN state = ?1 THEN state_since ELSE ?2 END,
		        ok_streak = CASE WHEN ?1 = 'online' THEN ok_streak + 1 ELSE 0 END,
		        fail_streak = CASE WHEN ?1 = 'online' THEN 0 ELSE fail_streak + 1 END,
		        last_probe_at = ?2,
		        last_seen_at = CASE WHEN ?1 = 'online' THEN ?2 ELSE last_seen_at END,
		        updated_at = ?2
		  WHERE node_id = ?3`,
		r.State, now, r.NodeID,
	)
	if err != nil {
		return fmt.Errorf("dispatch: 写节点健康度: %w", err)
	}
	// 历史表：`node_health.state_after` 的 CHECK 只允许三值，不含 unknown。
	if r.State == "online" || r.State == "degraded" || r.State == "offline" {
		_, _ = st.Execute(ctx,
			`INSERT INTO node_health (node_id, tcp_ok, api_ok, http_code, error_code,
			                          latency_ms, state_after, observed_at)
			 VALUES (?1, ?2, ?3, NULL, ?4, ?5, ?6, ?7)`,
			r.NodeID, boolToInt(r.TCPOK), boolToInt(r.APIOK), nullIfEmpty(r.Detail),
			r.LatencyMs, r.State, now,
		)
	}
	return nil
}

// splitHostPort 从 base_url 里取主机与端口（缺端口时按 scheme 补默认值）。
func splitHostPort(baseURL string) (string, string) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return "", ""
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Hostname(), port
}

// boolToInt 把 bool 转成 DDL 需要的 0/1。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullIfEmpty 把空串转成 SQL NULL。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
