package adminpass

import (
	"strings"
	"testing"
)

func TestHashRoundTripAndRejectWrong(t *testing.T) {
	phc, err := Hash("correct horse battery")
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("PHC 前缀不对: %s", phc)
	}
	if !VerifyHash(phc, "correct horse battery") {
		t.Fatal("正确口令应通过")
	}
	if VerifyHash(phc, "correct horse batterY") {
		t.Fatal("大小写不同不应通过")
	}
	if VerifyHash(phc, "") {
		t.Fatal("空口令不应通过")
	}
}

func TestSaltIsRandom(t *testing.T) {
	a, err := Hash("same-password-123")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Hash("same-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("同一口令两次哈希必须不同（盐未随机化）")
	}
	if !VerifyHash(a, "same-password-123") || !VerifyHash(b, "same-password-123") {
		t.Fatal("两个哈希都应能校验通过")
	}
}

// 坏串必须判失败：不 panic、也不放行。
func TestBrokenHashNeverPasses(t *testing.T) {
	cases := []string{
		"",
		"not-a-phc-string",
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",     // 算法不对
		"$argon2id$v=16$m=19456,t=2,p=1$c2FsdA$aGFzaA",    // 版本不对
		"$argon2id$v=19$m=0,t=2,p=1$c2FsdA$aGFzaA",        // 内存为 0
		"$argon2id$v=19$m=19456,t=2$c2FsdA$aGFzaA",        // 少 p
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$aGFzaA",       // base64 非法
		"$argon2id$v=19$m=4294967295,t=2,p=1$c2FsdA$aGFs", // 内存越界（防 DoS）
	}
	for _, c := range cases {
		if VerifyHash(c, "anything") {
			t.Fatalf("坏串不应通过: %q", c)
		}
	}
}

func TestValidateNew(t *testing.T) {
	if err := ValidateNew("1234567890", "old-password"); err != nil {
		t.Fatalf("10 字符应通过: %v", err)
	}
	if err := ValidateNew("123456789", "old-password"); err == nil {
		t.Fatal("9 字符应被拒")
	}
	if err := ValidateNew("", "old"); err == nil {
		t.Fatal("空口令应被拒")
	}
	if err := ValidateNew(strings.Repeat("x", MaxLen+1), "old"); err == nil {
		t.Fatal("超长应被拒")
	}
	if err := ValidateNew("1234567890", "1234567890"); err == nil {
		t.Fatal("与当前相同应被拒")
	}
	if err := ValidateNew("abc\ndefghij", "old"); err == nil {
		t.Fatal("控制字符应被拒")
	}
	// 中文按**字符**计数，不是字节。
	if err := ValidateNew("中文口令十个字符测试", "old"); err != nil {
		t.Fatalf("中文 10 字符应通过: %v", err)
	}
}

// Credential 的三种状态：明文 / 切到哈希 / 来源标识。
func TestCredentialVerifyAndSource(t *testing.T) {
	c := NewCredential("env-init-pass")
	if c.Source() != "environment" {
		t.Fatalf("初始来源应为 environment，实际 %s", c.Source())
	}
	if !c.Verify("env-init-pass") {
		t.Fatal("初始口令应通过")
	}
	if c.Verify("env-init-passX") {
		t.Fatal("错误口令不应通过")
	}

	phc, err := Hash("user-chosen-pass")
	if err != nil {
		t.Fatal(err)
	}
	c.SetHash(phc)
	if c.Source() != "database" {
		t.Fatalf("切到哈希后来源应为 database，实际 %s", c.Source())
	}
	if c.Verify("env-init-pass") {
		t.Fatal("切到哈希后，旧明文口令必须失效")
	}
	if !c.Verify("user-chosen-pass") {
		t.Fatal("新口令应通过")
	}
}

// 空指针不得 panic（handler 装配出错时宁可拒绝，也不要崩进程）。
func TestNilCredentialRejects(t *testing.T) {
	var c *Credential
	if c.Verify("anything") {
		t.Fatal("nil 凭据不应放行")
	}
	if c.Source() != "environment" {
		t.Fatalf("nil 凭据来源应为 environment，实际 %s", c.Source())
	}
}

// ---------------------------------------------------------------------------
// 用户名（`DISPATCH_ADMIN_USER`）
// ---------------------------------------------------------------------------

// TestVerifyUser 覆盖默认值、自定义值与空白输入的回退。
//
// 空白输入必须回退到 admin：加入用户名之前，所有客户端（含直接 curl）都
// 不带这个字段。不回退等于升级即失联。
func TestVerifyUser(t *testing.T) {
	c := NewCredential("pw")
	if c.User() != DefaultUser {
		t.Fatalf("默认用户名应为 %q，实际 %q", DefaultUser, c.User())
	}
	ok := []string{"admin", " admin ", "", "   "}
	for _, in := range ok {
		if !c.VerifyUser(in) {
			t.Fatalf("默认用户下 %q 应通过", in)
		}
	}
	for _, in := range []string{"root", "Admin", "administrator"} {
		if c.VerifyUser(in) {
			t.Fatalf("默认用户下 %q 不应通过（区分大小写）", in)
		}
	}

	c2 := NewCredential("pw").WithUser("ops")
	if c2.User() != "ops" {
		t.Fatalf("自定义用户名应为 ops，实际 %q", c2.User())
	}
	if !c2.VerifyUser("ops") || !c2.VerifyUser(" ops ") {
		t.Fatalf("自定义用户名应通过（允许首尾空白）")
	}
	if c2.VerifyUser("admin") || c2.VerifyUser("") {
		t.Fatalf("自定义用户名下，admin 与空输入都不应通过 —— 否则等于留了个后门")
	}

	// 空白用户名不覆盖默认值：`WithUser("")` 不该把用户名清成空串。
	c3 := NewCredential("pw").WithUser("   ")
	if c3.User() != DefaultUser {
		t.Fatalf("空白用户名应保持默认，实际 %q", c3.User())
	}

	var nilCred *Credential
	if nilCred.VerifyUser("admin") {
		t.Fatal("nil 凭据不得通过任何用户名")
	}
	if nilCred.User() != DefaultUser {
		t.Fatalf("nil 凭据的 User() 应回默认值")
	}
}

// TestVerifyUserIndependentOfPassword 锁住"两个维度分别校验"这一点。
//
// 用户名对但口令错、口令对但用户名错，都必须失败 —— 任意一个短路通过
// 都意味着另一半形同虚设。
func TestVerifyUserIndependentOfPassword(t *testing.T) {
	c := NewCredential("right-pw").WithUser("ops")
	cases := []struct {
		user, pass string
		want       bool
	}{
		{"ops", "right-pw", true},
		{"ops", "wrong-pw", false},
		{"admin", "right-pw", false},
		{"admin", "wrong-pw", false},
	}
	for _, cs := range cases {
		userOK := c.VerifyUser(cs.user)
		passOK := c.Verify(cs.pass)
		if (userOK && passOK) != cs.want {
			t.Fatalf("(%q,%q) 结果应为 %v，实际 user=%v pass=%v", cs.user, cs.pass, cs.want, userOK, passOK)
		}
	}
}
