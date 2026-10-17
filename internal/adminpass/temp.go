// 一次性临时口令：**用户在登录页自己点按钮找回**，口令只出现在服务端日志里。
//
// # 它解决的是哪一步
//
// 改过口令之后，库里只有不可逆哈希，忘了就锁死。离线补救（`reset-password` 子命令）
// 要求用户会 `docker exec`、知道容器名 —— 对「只是忘了自己设的啥」的人来说仍然偏重。
//
// 这里换一条更轻的证明链：**能读到服务端日志的人 = 能控制这个容器的人**。
// 于是流程变成：
//
//	登录页点「生成临时口令」→ 服务端生成一次性口令并**只写日志**
//	→ 用户在宿主机 `docker logs <容器> | grep temp_password` 拿到
//	→ 用它登录 → 改自己的口令
//
// 口令绝不进 HTTP 响应、绝不进 `event_log`（审计表可能在界面上展示），
// 只进应用日志 —— 这是本设计唯一的口令交付通道，也是它的安全边界。
//
// # 为什么是「额外凭据」而不是「覆盖掉原口令」
//
// 覆盖原口令的做法实现更省事（登录路径一行都不用改），但任何能访问管理口的人
// 只要点一次按钮，就能把真正的管理员挡在门外（而且临时口令 10 分钟后就过期，
// 用户若没去看日志，会被永久锁死）。所以这里让临时口令与真实口令**并存**：
// 原口令照常可用，临时口令单独有效、单独受 TTL 与频率限制约束。
//
// # 生命周期（任何一个成立即失效）
//
//   - 过期（默认 600 秒）；
//   - 用户在管理台改了口令（[Credential.SetHash] 内清空）；
//   - 又生成了一串新的（同一时刻只允许存在一串，避免日志里躺着多串有效口令）；
//   - 进程重启（只活在内存里，不落库）。
package adminpass

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
)

// TempCodeLen 是临时口令长度（字符数）。
//
// 12 位、32 个字符的字母表 ≈ 60 bit 熵：从日志里抄写不会出错（去掉了 I/O/0/1），
// 在线爆破在「10 分钟 + 每分钟 1 次 + 全局 10 分钟 5 次」的频率限制下不可行。
const TempCodeLen = 12

// tempAlphabet 去掉了容易看错的 I、O、0、1。长度 32 ⇒ 256 % 32 == 0，
// 用随机字节直接取模没有偏置，不需要拒绝采样。
const tempAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// 频率限制与默认有效期。
const (
	// DefaultTempTTL 是临时口令默认有效期（秒）。
	DefaultTempTTL int64 = 600
	// TempPerIPCooldown 是同一来源 IP 两次生成的间隔（秒）。
	TempPerIPCooldown int64 = 60
	// TempGlobalWindow 是全局计数的滑动窗口（秒）。
	TempGlobalWindow int64 = 600
	// TempGlobalMax 是窗口内允许生成的次数上限。
	//
	// 这个上限防的不是爆破（口令熵够），而是**日志被刷屏**：每次生成都会往日志里
	// 写一行带口令的记录，如果谁都能无限点，真正的记录就会被冲走，
	// 用户翻日志时看到的是一屏陌生口令。
	TempGlobalMax = 5
)

// ErrTempTooFrequent 表示生成过于频繁；返回值里的秒数是建议的重试间隔。
var ErrTempTooFrequent = errors.New("临时口令生成过于频繁")

// tempState 是临时口令的进程内状态（不带锁，由 [Credential.mu] 保护）。
type tempState struct {
	code      string
	expiresAt int64
	byIP      map[string]int64
	recent    []int64
}

// clearLocked 清空临时口令本身（不动频率限制的计数）。
func (t *tempState) clearLocked() {
	t.code = ""
	t.expiresAt = 0
}

// generateTempCode 生成一串临时口令。
func generateTempCode() (string, error) {
	buf := make([]byte, TempCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成临时口令失败: %w", err)
	}
	out := make([]byte, TempCodeLen)
	for i, b := range buf {
		out[i] = tempAlphabet[int(b)%len(tempAlphabet)]
	}
	return string(out), nil
}

// IssueTemp 生成一串新的临时口令并在进程内登记，返回口令、有效期与错误。
//
// 频率限制同时看两个维度：
//   - 同一 IP 至少间隔 [TempPerIPCooldown]；
//   - 全局窗口 [TempGlobalWindow] 内最多 [TempGlobalMax] 次。
//
// 触发限制时返回 [ErrTempTooFrequent] 与建议的重试秒数（供 `Retry-After` 用）。
// 生成失败（随机源异常）时不改动任何已有状态 —— 老口令继续有效，不至于把人锁死。
func (c *Credential) IssueTemp(now int64, ip string, ttl int64) (string, int64, error) {
	if c == nil {
		return "", 0, errors.New("凭据未初始化")
	}
	if ttl <= 0 {
		ttl = DefaultTempTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.temp.byIP == nil {
		c.temp.byIP = make(map[string]int64)
	}
	// 滑动窗口：把窗口外的时间戳扔掉（切片原地复用，不额外分配）。
	kept := c.temp.recent[:0]
	for _, t := range c.temp.recent {
		if now-t < TempGlobalWindow {
			kept = append(kept, t)
		}
	}
	c.temp.recent = kept

	if last, ok := c.temp.byIP[ip]; ok && now-last < TempPerIPCooldown {
		return "", TempPerIPCooldown - (now - last), ErrTempTooFrequent
	}
	if len(c.temp.recent) >= TempGlobalMax {
		return "", TempGlobalWindow - (now - c.temp.recent[0]), ErrTempTooFrequent
	}

	// 顺手清掉太久没来的 IP，避免 map 随着扫描/爬虫无限增长。
	if len(c.temp.byIP) > 128 {
		for k, t := range c.temp.byIP {
			if now-t >= TempGlobalWindow {
				delete(c.temp.byIP, k)
			}
		}
	}

	code, err := generateTempCode()
	if err != nil {
		return "", 0, err
	}
	// 只保留最新一串：日志里躺着多串有效口令，等于把有效期拉长到「最早那串的 TTL」。
	c.temp.code = code
	c.temp.expiresAt = now + ttl
	c.temp.byIP[ip] = now
	c.temp.recent = append(c.temp.recent, now)
	return code, ttl, nil
}

// VerifyTemp 判断输入是否是**当前有效**的临时口令。
//
// 过期时顺手清掉（免得每次登录都白算一次常量时间比较），并返回 false。
func (c *Credential) VerifyTemp(input string) bool {
	if c == nil || input == "" {
		return false
	}
	c.mu.RLock()
	code, expiresAt := c.temp.code, c.temp.expiresAt
	c.mu.RUnlock()
	if code == "" {
		return false
	}
	if c.clock() > expiresAt {
		c.mu.Lock()
		// 双检：只在「还是同一串」时清，避免把刚生成的新串误清掉。
		if c.temp.code == code && c.temp.expiresAt == expiresAt {
			c.temp.clearLocked()
		}
		c.mu.Unlock()
		return false
	}
	return subtle.ConstantTimeCompare([]byte(input), []byte(code)) == 1
}

// TempActive 报告当前是否有未过期的临时口令（供状态接口/日志判断，不回传口令本身）。
func (c *Credential) TempActive() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	code, expiresAt := c.temp.code, c.temp.expiresAt
	c.mu.RUnlock()
	return code != "" && c.clock() <= expiresAt
}

// ClearTemp 主动清掉临时口令（改口令成功后由 [Credential.SetHash] 调用）。
func (c *Credential) ClearTemp() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.temp.clearLocked()
	c.mu.Unlock()
}
