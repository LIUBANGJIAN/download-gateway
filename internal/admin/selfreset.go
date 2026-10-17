// 自助找回：`POST /api/admin/password/reset-request` —— 生成一次性临时口令并**只写日志**。
//
// # 为什么这个接口不需要登录
//
// 它存在的理由就是「用户进不去」。所以它天然是匿名口，安全性不能建立在会话上，
// 只能建立在**口令交付通道**上：口令只出现在服务端日志里，
// 而能读到日志的人（`docker logs` / `journalctl` / 容器平台控制台）
// 本来就能拿到这个容器的完全控制权。攻击者能点按钮，但看不到结果。
//
// # 这个接口能被人滥用成什么样（如实写清）
//
//   - **刷日志**：每次生成写一行带口令的日志。限制为同一 IP 60 秒 1 次、
//     全局 10 分钟 5 次（见 `adminpass.IssueTemp`），把刷屏成本抬到「会留下明显痕迹」。
//   - **把人锁在外面？** 不会。临时口令与真实口令**并存**，原口令不失效
//     （这是刻意不采用「覆盖原口令」实现的原因 —— 那种做法下谁点一下按钮都能造成拒绝服务）。
//   - **拿到临时口令？** 需要日志读权限 = 宿主机控制权，与 `reset-password` 子命令同级。
//
// # 为什么不回传口令
//
// 一旦回传，这个接口就变成「任何人点一下即可拿到管理口令」，等于没有认证。
// 所以响应里只有有效期；口令只进日志，且日志那一行必须自带 `temp_password=` 前缀，
// 方便用户 `grep`，也方便日志系统做敏感字段脱敏（知道该脱哪个键）。
//
// # 与 Rust 侧的对齐
//
// 路径、请求体（无）、响应字段、错误码、日志键名、环境变量与
// `crates/dispatch-core/src/admin/selfreset.rs` 保持一致。
package admin

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/adminpass"
	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
)

const (
	// EnvAllowSelfReset 控制登录页的「忘记口令」入口是否可用（`0/false/off/no` 关闭）。
	EnvAllowSelfReset = "DISPATCH_ALLOW_SELF_RESET"
	// EnvTempPasswordTTL 是临时口令有效期（秒），允许 30–86400。
	EnvTempPasswordTTL = "DISPATCH_TEMP_PASSWORD_TTL"
)

// selfResetPolicy 读两个只在管理口用到的环境变量。
//
// 为什么不塞进 `config.Config`：那两个字段会进 `EnvSnapshot`（管理台里的「环境快照」
// 面板），而快照的键集合是与 Rust 侧逐字对齐的契约，加键要两边同时改并补断言。
// 这两个值只影响一个 handler 的行为、不需要展示，就地读取更小的改动面。
func selfResetPolicy() (enabled bool, ttl int64) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvAllowSelfReset))) {
	case "0", "false", "off", "no":
		return false, 0
	}
	ttl = adminpass.DefaultTempTTL
	if s := strings.TrimSpace(os.Getenv(EnvTempPasswordTTL)); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 30 && n <= 86400 {
			ttl = n
		}
		// 解析失败/越界**静默回落默认值**：这类部署参数写错不该让「找回口令」失效，
		// 那是用户唯一能自救的路径，宁可放宽也不能因为一个手误把它关掉。
	}
	return true, ttl
}

// resetClientIP 取来源 IP（不含端口）。
//
// 刻意**不读** `X-Forwarded-For`：那是个可以随便伪造的头，用它做限流等于没限流。
// 代价是放在反代后面时所有人共享反代的 IP（频率限制退化为全局限制）—— 可接受，
// 因为全局限制才是防刷日志的主力，按 IP 只是让正常用户不至于互相影响。
func resetClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requestTempPassword 实现 `POST /api/admin/password/reset-request`。
func (s *adminServer) requestTempPassword(w http.ResponseWriter, r *http.Request) {
	enabled, ttl := selfResetPolicy()
	if !enabled {
		// 关闭时用 404 而不是 403：这条路径对外**不存在**，不给探测者
		// 「这里有功能但被禁用了」的信息。
		fail(w, http.StatusNotFound, CodeNotFound, "自助重置未启用")
		return
	}
	if s.st.Credential == nil {
		fail(w, http.StatusInternalServerError, CodeInternal, "管理口令未初始化")
		return
	}

	now := ids.NowSecs()
	ip := resetClientIP(r)
	code, expiresIn, err := s.st.Credential.IssueTemp(now, ip, ttl)
	if errors.Is(err, adminpass.ErrTempTooFrequent) {
		if expiresIn > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(expiresIn, 10))
		}
		fail(w, http.StatusTooManyRequests, CodeTooManyRequests,
			"生成过于频繁，请稍后再试")
		return
	}
	if err != nil {
		slog.Error("生成临时管理口令失败", "err", err)
		fail(w, http.StatusInternalServerError, CodeInternal, "生成临时口令失败")
		return
	}

	// ── 唯一的交付通道：日志 ───────────────────────────────────────────────
	// 键名固定为 `temp_password`：用户的排查动作是 `docker logs <c> | grep temp_password`，
	// 日志平台做敏感字段脱敏时也认这个键。
	// 级别用 WARN：让它在一片 INFO 里显眼，且默认日志级别下不会被过滤掉。
	slog.Warn("已生成一次性临时管理口令（只打印在本日志里，登录后请立即修改口令）",
		"temp_password", code,
		"expires_in_seconds", expiresIn,
		"client_ip", ip,
	)

	// 审计**不记口令**：event_log 在管理台上是可见的，记了就等于把口令贴到界面上。
	s.auditPassword(r, now, "已生成一次性临时管理口令（来源 IP "+ip+"）", "warn")

	w.Header().Set("Cache-Control", "no-store")
	ok(w, map[string]any{
		"expires_in": expiresIn,
		"log_hint":   "docker logs <容器名> | grep temp_password",
	})
}
