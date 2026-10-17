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
