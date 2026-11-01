package adminpass

import (
	"testing"
)

// 口令只来自部署配置：这里钉住「配了什么就认什么」，防止有人日后又加回
// 「库里哈希优先」的分支 —— 那个分支的代价见包注释。
func TestCredentialVerifyPlain(t *testing.T) {
	c := NewCredential("correct-horse-battery")
	if !c.Verify("correct-horse-battery") {
		t.Fatal("正确口令应通过")
	}
	for _, bad := range []string{"", "correct-horse", "correct-horse-battery ", "Correct-Horse-Battery"} {
		if c.Verify(bad) {
			t.Fatalf("错误口令 %q 不得通过", bad)
		}
	}
}

// nil 凭据必须拒绝一切输入。
//
// 这条不是形式主义：`User()` 对 nil 会回落到 DefaultUser，若 `VerifyUser` 忘了
// 先挡 nil，「没配置凭据」反而会放行 admin。
func TestNilCredentialRejects(t *testing.T) {
	var c *Credential
	if c.Verify("anything") {
		t.Fatal("nil 凭据不得通过任何口令")
	}
	if c.VerifyUser("admin") {
		t.Fatal("nil 凭据不得通过任何用户名")
	}
	if got := c.User(); got != DefaultUser {
		t.Fatalf("nil 凭据的 User() 应回落 %q，实际 %q", DefaultUser, got)
	}
}

func TestVerifyUser(t *testing.T) {
	c := NewCredential("pw").WithUser("  ops  ")
	if got := c.User(); got != "ops" {
		t.Fatalf("WithUser 应 trim，实际 %q", got)
	}
	if !c.VerifyUser("ops") {
		t.Fatal("正确用户名应通过")
	}
	if !c.VerifyUser("  ops ") {
		t.Fatal("用户名两端空白应被忽略")
	}
	if c.VerifyUser("admin") {
		t.Fatal("配置了 ops 之后 admin 不得通过")
	}
	if c.VerifyUser("OPS") {
		t.Fatal("用户名大小写敏感")
	}
}

// 空白用户名按默认值处理：老前端/裸 curl 不带 username 时，
// 只要部署用的就是默认用户名，照样能登录。
func TestVerifyUserBlankFallsBackToDefault(t *testing.T) {
	c := NewCredential("pw")
	if !c.VerifyUser("") {
		t.Fatal("未配置用户名时，空白输入应按默认用户名处理")
	}
	if !c.VerifyUser("admin") {
		t.Fatal("未配置用户名时应认 admin")
	}
	c2 := NewCredential("pw").WithUser("ops")
	if c2.VerifyUser("") {
		t.Fatal("配置了 ops 之后，空白输入不得按默认用户名放行")
	}
}

// 用户名与口令是**两个独立维度**：任一对就能过，说明有一个没在起作用。
func TestVerifyUserIndependentOfPassword(t *testing.T) {
	c := NewCredential("pw").WithUser("ops")
	if c.VerifyUser("ops") && !c.Verify("pw") {
		t.Fatal("用户名对了但口令错，不应被当成整体通过")
	}
	if !c.VerifyUser("ops") || !c.Verify("pw") {
		t.Fatal("两个维度都对应通过")
	}
}

// WithUser 传空白不应把用户名清空（否则会静默退回 admin）。
func TestWithUserBlankKeepsExisting(t *testing.T) {
	c := NewCredential("pw").WithUser("ops").WithUser("   ")
	if c.User() != "ops" {
		t.Fatalf("空白 WithUser 不应覆盖已有用户名，实际 %q", c.User())
	}
}
