// 一次性临时口令的测试。
//
// 这里守的是**四件必须成立的事**，任何一条塌了都会让「自助找回」变成安全洞或坏体验：
//  1. 临时口令可登录，且**原口令同时仍可用**（不采用「覆盖原口令」，避免点一下按钮就把人锁死）；
//  2. 过期、改口令、重新生成，三者任一发生 ⇒ 旧临时口令立即失效；
//  3. 频率限制真的拦得住（同 IP 冷却 + 全局窗口），否则谁能刷屏把真正的日志冲走；
//  4. 生成的串落在受限字母表里且不重复（用户要从日志里**手抄**，看错字符等于白折腾）。
package adminpass

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// newTestCredential 造一个时钟可注入的凭据（否则 TTL 用例只能靠 sleep）。
func newTestCredential(now *int64) *Credential {
	c := NewCredential("deploy-init-pass")
	c.now = func() int64 { return *now }
	return c
}

func TestGenerateTempCodeShape(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		code, err := generateTempCode()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if len(code) != TempCodeLen {
			t.Fatalf("长度应为 %d，实际 %d（%q）", TempCodeLen, len(code), code)
		}
		for _, r := range code {
			if !strings.ContainsRune(tempAlphabet, r) {
				t.Fatalf("出现字母表之外的字符 %q（口令 %q）", r, code)
			}
		}
		// 容易看错的字符必须不在字母表里：用户是照着日志手抄的。
		for _, bad := range []rune{'I', 'O', '0', '1'} {
			if strings.ContainsRune(code, bad) {
				t.Fatalf("口令里出现了易混字符 %q：%q", bad, code)
			}
		}
		if seen[code] {
			t.Fatalf("200 次生成出现重复：%q", code)
		}
		seen[code] = true
	}
}

// 临时口令是**额外**凭据：生成它不影响原口令。
func TestTempCodeCoexistsWithRealPassword(t *testing.T) {
	now := int64(1_000)
	c := newTestCredential(&now)

	code, ttl, err := c.IssueTemp(now, "192.168.1.10", DefaultTempTTL)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if ttl != DefaultTempTTL {
		t.Fatalf("有效期应为 %d，实际 %d", DefaultTempTTL, ttl)
	}
	if !c.Verify(code) {
		t.Fatal("临时口令应能通过校验")
	}
	if !c.Verify("deploy-init-pass") {
		t.Fatal("生成临时口令后，原口令必须仍然可用（否则谁点按钮都能把人锁死）")
	}
	if c.Verify("wrong-pass") {
		t.Fatal("错误口令不该通过")
	}

	// 再生成一串（换 IP 绕开冷却）⇒ 旧串立即失效，任何时刻只允许一串有效。
	second, _, err := c.IssueTemp(now+5, "192.168.1.11", DefaultTempTTL)
	if err != nil {
		t.Fatalf("第二次生成失败: %v", err)
	}
	if c.Verify(code) {
		t.Fatal("旧临时口令在新口令生成后必须失效")
	}
	if !c.Verify(second) {
		t.Fatal("新临时口令应可用")
	}
}

func TestTempCodeExpires(t *testing.T) {
	now := int64(1_000)
	c := newTestCredential(&now)
	code, _, err := c.IssueTemp(now, "192.168.1.10", 600)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if !c.TempActive() {
		t.Fatal("刚生成时应为生效状态")
	}

	now += 601
	if c.Verify(code) {
		t.Fatal("过期后必须失效")
	}
	if c.TempActive() {
		t.Fatal("过期后 TempActive 应为 false")
	}
}

// 边界：恰好到期的那一秒仍算过期（`>` 而非 `>=` 的实现差异不该留给用户猜）。
func TestTempCodeExpiryBoundary(t *testing.T) {
	now := int64(1_000)
	c := newTestCredential(&now)
	code, _, err := c.IssueTemp(now, "192.168.1.10", 60)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	now = 1_060
	if !c.Verify(code) {
		t.Fatal("到期那一刻（now == expiresAt）仍应有效")
	}
	now = 1_061
	if c.Verify(code) {
		t.Fatal("过了一秒必须失效")
	}
}

func TestTempCodeRateLimit(t *testing.T) {
	now := int64(1_000)
	c := newTestCredential(&now)

	if _, _, err := c.IssueTemp(now, "10.0.0.1", DefaultTempTTL); err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}

	// 同一 IP 的冷却：60 秒。
	_, wait, err := c.IssueTemp(now+10, "10.0.0.1", DefaultTempTTL)
	if !errors.Is(err, ErrTempTooFrequent) {
		t.Fatalf("同 IP 10 秒内应被限流，实际 err=%v", err)
	}
	if wait != TempPerIPCooldown-10 {
		t.Fatalf("建议重试间隔应为 %d，实际 %d", TempPerIPCooldown-10, wait)
	}

	// 换 IP 可以，但全局窗口内最多 5 次（上面已用掉 1 次）。
	for i := 0; i < 4; i++ {
		now += TempPerIPCooldown
		if _, _, err := c.IssueTemp(now, fmt.Sprintf("10.0.1.%d", i), DefaultTempTTL); err != nil {
			t.Fatalf("第 %d 次（全局额度内）不该被限流: %v", i+2, err)
		}
	}
	now += TempPerIPCooldown
	if _, _, err := c.IssueTemp(now, "10.0.2.1", DefaultTempTTL); !errors.Is(err, ErrTempTooFrequent) {
		t.Fatalf("全局 10 分钟内第 6 次应被限流，实际 err=%v", err)
	}

	// 窗口滑过去之后恢复。
	now += TempGlobalWindow
	if _, _, err := c.IssueTemp(now, "10.0.3.1", DefaultTempTTL); err != nil {
		t.Fatalf("窗口滑过后应恢复，实际: %v", err)
	}
}

// 改口令 = 临时口令立刻作废（否则改完的 10 分钟里日志那串还能登进来）。
func TestSetHashClearsTempCode(t *testing.T) {
	now := int64(1_000)
	c := newTestCredential(&now)
	code, _, err := c.IssueTemp(now, "10.0.0.1", DefaultTempTTL)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	phc, err := Hash("brand-new-password")
	if err != nil {
		t.Fatalf("计算哈希失败: %v", err)
	}
	c.SetHash(phc)

	if c.Verify(code) {
		t.Fatal("改口令后临时口令必须立即失效")
	}
	if !c.Verify("brand-new-password") {
		t.Fatal("新口令应可用")
	}
	if c.TempActive() {
		t.Fatal("改口令后不应还有生效的临时口令")
	}
}

// nil 凭据（状态未装配）时不能 panic。
func TestNilCredentialTempIsSafe(t *testing.T) {
	var c *Credential
	if _, _, err := c.IssueTemp(1, "ip", 600); err == nil {
		t.Fatal("nil 凭据生成应报错")
	}
	if c.VerifyTemp("x") || c.TempActive() {
		t.Fatal("nil 凭据不应报告生效")
	}
	c.ClearTemp() // 不 panic 即通过
}
