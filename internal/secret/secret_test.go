package secret

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
)

// ---------------------------------------------------------------------------
// 与 Rust `crates/dispatch-core/src/secret.rs::tests` 逐条对照
// ---------------------------------------------------------------------------

// 对照 `wrap_then_open_roundtrips`：**核心契约**，含中文、符号、空串、单字符。
func TestWrapThenOpenRoundtrips(t *testing.T) {
	sb := WithKey("a-test-master-key-0123456789")
	for _, plain := range []string{"", "p@ssw0rd", "中文口令·带符号!#$%", "a"} {
		enc, err := sb.Wrap(plain)
		if err != nil {
			t.Fatalf("加密应成功：plain=%q err=%v", plain, err)
		}
		if enc == plain {
			t.Fatalf("密文不应等于明文：%q", plain)
		}
		back, err := sb.Open(enc)
		if err != nil {
			t.Fatalf("解密应成功：plain=%q err=%v", plain, err)
		}
		if back != plain {
			t.Fatalf("roundtrip 必须逐字节一致：入 %q 出 %q", plain, back)
		}
	}
}

// 对照 `ciphertext_is_randomized`：同一明文两次加密结果必须不同（随机 salt/IV）。
func TestCiphertextIsRandomized(t *testing.T) {
	sb := WithKey("a-test-master-key-0123456789")
	a, err := sb.Wrap("same")
	if err != nil {
		t.Fatalf("加密应成功：%v", err)
	}
	b, err := sb.Wrap("same")
	if err != nil {
		t.Fatalf("加密应成功：%v", err)
	}
	if a == b {
		t.Fatalf("同一明文两次加密结果必须不同（salt/IV 随机）")
	}
	ba, _ := sb.Open(a)
	bb, _ := sb.Open(b)
	if ba != bb {
		t.Fatalf("两段密文解出的明文应相同：%q vs %q", ba, bb)
	}
}

// 对照 `wrong_key_fails_to_open`：换错密钥必须**解密失败**。
func TestWrongKeyFailsToOpen(t *testing.T) {
	a := WithKey("key-aaaaaaaaaaaaaaaa")
	b := WithKey("key-bbbbbbbbbbbbbbbb")
	enc, err := a.Wrap("secret")
	if err != nil {
		t.Fatalf("加密应成功：%v", err)
	}
	if _, err := b.Open(enc); err == nil {
		t.Fatalf("错误主密钥必须解密失败")
	}
}

// 对照 `tampered_ciphertext_is_rejected`：篡改密文必须被 HMAC 发现。
func TestTamperedCiphertextIsRejected(t *testing.T) {
	sb := WithKey("a-test-master-key-0123456789")
	enc, err := sb.Wrap("secret")
	if err != nil {
		t.Fatalf("加密应成功：%v", err)
	}
	mid := len(enc) / 2
	repl := byte('A')
	if enc[mid] == 'A' {
		repl = 'B'
	}
	tampered := enc[:mid] + string(repl) + enc[mid+1:]
	if _, err := sb.Open(tampered); err == nil {
		t.Fatalf("篡改后的密文必须被拒绝")
	}
}

// 对照 `key_file_sits_next_to_db`。
func TestKeyFileSitsNextToDB(t *testing.T) {
	// 库文件在某目录内 ⇒ 密钥文件与库同目录。
	dbInDir := filepath.Join("data", "dispatch.db")
	if got, want := KeyFilePath(dbInDir), filepath.Join("data", KeyFileName); got != want {
		t.Fatalf("KeyFilePath(%q) = %q，应为 %q", dbInDir, got, want)
	}
	// 无父目录（裸文件名）⇒ 退化为当前目录下，且不 panic。
	if got := KeyFilePath("dispatch.db"); got != KeyFileName {
		t.Fatalf("KeyFilePath(dispatch.db) = %q，应为 %q", got, KeyFileName)
	}
}

// 对照 `resolve_is_stable_across_calls`：落盘的密钥必须**再次 resolve 时复用**。
func TestResolveIsStableAcrossCalls(t *testing.T) {
	t.Setenv(KeyEnv, "") // 中和环境变量（设空 ⇒ 视为未设置）
	db := filepath.Join(t.TempDir(), "dispatch.db")

	a, gen1, err := Resolve(db)
	if err != nil {
		t.Fatalf("首次 resolve 应成功：%v", err)
	}
	b, gen2, err := Resolve(db)
	if err != nil {
		t.Fatalf("二次 resolve 应成功：%v", err)
	}
	if !gen1 {
		t.Fatalf("首次应生成密钥文件")
	}
	if gen2 {
		t.Fatalf("第二次应复用既有密钥文件")
	}
	enc, err := a.Wrap("p")
	if err != nil {
		t.Fatalf("加密应成功：%v", err)
	}
	back, err := b.Open(enc)
	if err != nil || back != "p" {
		t.Fatalf("两次 resolve 必须得到同一主密钥：back=%q err=%v", back, err)
	}
}

// ---------------------------------------------------------------------------
// 本包新增：Rust 侧未直接断言、但属错误分支 / 契约一部分的出口
//
// ⚠️ 金样声明（如实记录，勿据此判"弱"当缺陷修）：
//   - `加密失败：<detail>` / `解密失败：<detail>` 里的 `<detail>`：Rust 来自 Rust 版
//     rncryptor 的报错文本、Go 来自 `internal/protocol`，**跨语言必然不同 ⇒ 无法逐字取金样**。
//     故这里只逐字钉住**固定前缀**与**完全固定文案**（过短密钥全文、`解密结果不是合法 UTF-8`）。
//   - 本机**无 Rust 工具链**，无法在真机录金样；全部金样取自 `crates/**` 的断言字面量。
// ---------------------------------------------------------------------------

// ★ 错误文案契约：环境变量过短 ⇒ `加密失败：DISPATCH_SECRET_KEY 过短（N 字符）：至少 16 字符，否则派生强度不足`。
// 下游判据按此文案断言，故必须**逐字**匹配，且 errors.Is(err, ErrEncrypt) 成立。
func TestResolveRejectsShortEnvKeyWithExactMessage(t *testing.T) {
	t.Setenv(KeyEnv, "short") // 5 字符 < 16
	_, _, err := Resolve(filepath.Join(t.TempDir(), "dispatch.db"))
	want := "加密失败：DISPATCH_SECRET_KEY 过短（5 字符）：至少 16 字符，否则派生强度不足"
	if err == nil {
		t.Fatalf("过短的注入密钥必须报错")
	}
	if err.Error() != want {
		t.Fatalf("错误文案必须逐字一致：\n got=%q\nwant=%q", err.Error(), want)
	}
	if !errors.Is(err, ErrEncrypt) {
		t.Fatalf("应可用 errors.Is(err, ErrEncrypt) 判定，实际 %v", err)
	}
}

// 边界：正好 16 字符必须**通过**（不小于最小值）。
// ★ 键长用**字面量** 16（而非 `keyMinLen`），否则本用例会随阈值常量一起漂移、失去证伪力。
func TestResolveAcceptsExactlyMinLenEnvKey(t *testing.T) {
	key := "0123456789abcdef" // 恰好 16 字符
	if len(key) != 16 {
		t.Fatalf("测试前置：键长应为 16，实际 %d", len(key))
	}
	t.Setenv(KeyEnv, key)
	sb, gen, err := Resolve(filepath.Join(t.TempDir(), "dispatch.db"))
	if err != nil {
		t.Fatalf("16 字符应被接受：%v", err)
	}
	if gen {
		t.Fatalf("注入密钥时不应生成文件")
	}
	// 行为断言：用的是注入的那把密钥。
	enc, err := sb.Wrap("x")
	if err != nil {
		t.Fatalf("加密应成功：%v", err)
	}
	if _, err := WithKey("some-other-key-0123456789").Open(enc); err == nil {
		t.Fatalf("密文应由注入密钥解出，别的密钥必须失败")
	}
	if back, err := WithKey(key).Open(enc); err != nil || back != "x" {
		t.Fatalf("注入密钥应能解出，实际 back=%q err=%v", back, err)
	}
}

// 环境变量**非空白**时优先于密钥文件（即便文件已存在）。
func TestResolveEnvKeyTakesPrecedenceOverFile(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "dispatch.db")
	// 预置一个 64 位十六进制密钥文件。
	fileKey := strings.Repeat("b", 64)
	if err := os.WriteFile(KeyFilePath(db), []byte(fileKey+"\n"), 0o600); err != nil {
		t.Fatalf("预置密钥文件失败：%v", err)
	}
	envKey := strings.Repeat("c", 32)
	t.Setenv(KeyEnv, envKey)

	sb, gen, err := Resolve(db)
	if err != nil {
		t.Fatalf("resolve 应成功：%v", err)
	}
	if gen {
		t.Fatalf("环境变量在时应直接使用、不生成文件")
	}
	enc, _ := sb.Wrap("y")
	if _, err := WithKey(fileKey).Open(enc); err == nil {
		t.Fatalf("应使用环境变量密钥，而非文件密钥")
	}
	if back, err := WithKey(envKey).Open(enc); err != nil || back != "y" {
		t.Fatalf("应能用环境变量密钥解出，实际 back=%q err=%v", back, err)
	}
}

// 环境变量为空白 ⇒ 回落密钥文件（而非报错或生成）。
func TestResolveBlankEnvFallsBackToKeyFile(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "dispatch.db")
	fileKey := strings.Repeat("d", 40)
	if err := os.WriteFile(KeyFilePath(db), []byte("  "+fileKey+"  \n"), 0o600); err != nil {
		t.Fatalf("预置密钥文件失败：%v", err)
	}
	t.Setenv(KeyEnv, "   ")

	sb, gen, err := Resolve(db)
	if err != nil {
		t.Fatalf("resolve 应成功：%v", err)
	}
	if gen {
		t.Fatalf("有可用密钥文件时不应生成")
	}
	enc, _ := sb.Wrap("z")
	if back, err := WithKey(fileKey).Open(enc); err != nil || back != "z" {
		t.Fatalf("应用文件密钥（trim 后），实际 back=%q err=%v", back, err)
	}
}

// 生成的密钥文件内容必须是 64 位小写十六进制 + 一个换行（可被下次 resolve 复用）。
func TestResolveWritesHexKeyFile(t *testing.T) {
	t.Setenv(KeyEnv, "")
	db := filepath.Join(t.TempDir(), "dispatch.db")
	if _, gen, err := Resolve(db); err != nil || !gen {
		t.Fatalf("首次应生成：gen=%v err=%v", gen, err)
	}
	b, err := os.ReadFile(KeyFilePath(db))
	if err != nil {
		t.Fatalf("读密钥文件失败：%v", err)
	}
	content := string(b)
	if !strings.HasSuffix(content, "\n") {
		t.Fatalf("密钥文件应以换行结尾，实际 %q", content)
	}
	trimmed := strings.TrimRight(content, "\n")
	if len(trimmed) != keyBytes*2 {
		t.Fatalf("密钥应为 %d 个十六进制字符，实际 %d（%q）", keyBytes*2, len(trimmed), trimmed)
	}
	for _, c := range trimmed {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("密钥必须是小写十六进制，实际 %q", trimmed)
		}
	}
}

// ★ `Open` 的 UTF-8 出口：解密成功但结果不是合法 UTF-8 ⇒ ErrNotUTF8。
// 用 protocol 直接造一段「合法 RNCryptor 密文承载非法 UTF-8 明文」来触发。
func TestOpenRejectsNonUTF8Plaintext(t *testing.T) {
	key := "a-test-master-key-0123456789"
	raw := []byte{0xff, 0xfe, 0xfd, 0xfc} // 非法 UTF-8
	enc, err := protocol.EncryptToBase64(raw, key)
	if err != nil {
		t.Fatalf("构造密文失败：%v", err)
	}
	_, err = WithKey(key).Open(enc)
	if !errors.Is(err, ErrNotUTF8) {
		t.Fatalf("非法 UTF-8 应返回 ErrNotUTF8，实际 %v", err)
	}
	if err == nil || err.Error() != "解密结果不是合法 UTF-8" {
		t.Fatalf("ErrNotUTF8 文案应为「解密结果不是合法 UTF-8」，实际 %v", err)
	}
}

// ★ `Open` 的解密失败文案前缀：非 base64 / 篡改 ⇒ `解密失败：...`，且 errors.Is(err, ErrDecrypt) 成立。
func TestOpenFailureMessagePrefix(t *testing.T) {
	sb := WithKey("a-test-master-key-0123456789")
	_, err := sb.Open("!!! not base64 !!!")
	if err == nil {
		t.Fatalf("非法输入必须报错")
	}
	if !strings.HasPrefix(err.Error(), "解密失败：") {
		t.Fatalf("解密失败文案应以「解密失败：」开头，实际 %q", err.Error())
	}
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("应可用 errors.Is(err, ErrDecrypt) 判定，实际 %v", err)
	}
}

// ★ 生成路径的 IO 失败文案前缀：父目录无法创建 ⇒ `io: ...`，且 errors.Is(err, ErrIO) 成立。
func TestResolveIOFailureMessagePrefix(t *testing.T) {
	t.Setenv(KeyEnv, "")
	// 用一个「父路径是已存在文件」的库路径，使 MkdirAll 必然失败。
	base := t.TempDir()
	blocker := filepath.Join(base, "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("造阻塞文件失败：%v", err)
	}
	db := filepath.Join(blocker, "sub", "dispatch.db") // 父目录跨越一个普通文件

	_, _, err := Resolve(db)
	if err == nil {
		t.Fatalf("父目录不可创建时必须报错")
	}
	if !strings.HasPrefix(err.Error(), "io: ") {
		t.Fatalf("IO 失败文案应以「io: 」开头，实际 %q", err.Error())
	}
	if !errors.Is(err, ErrIO) {
		t.Fatalf("应可用 errors.Is(err, ErrIO) 判定，实际 %v", err)
	}
}

// ★ 随机源故障 ⇒ 回落全零，并**大声报错**（对应 Rust `random_key_bytes` 的 tracing::error）。
func TestRandomKeyBytesFallsBackToZero(t *testing.T) {
	origReader := randReader
	origLogf := logf
	t.Cleanup(func() { randReader = origReader; logf = origLogf })

	randReader = failingReader{}
	var logged []string
	logf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	got := randomKeyBytes()
	if len(got) != keyBytes {
		t.Fatalf("长度应为 %d，实际 %d", keyBytes, len(got))
	}
	for i, b := range got {
		if b != 0 {
			t.Fatalf("降级路径应回落全零，但 buf[%d]=%d", i, b)
		}
	}
	if len(logged) == 0 {
		t.Fatalf("随机源故障必须记一条告警（不静默）")
	}
	// 文案里必须点名要用的环境变量，否则用户不知道该注入什么。
	if !strings.Contains(logged[0], KeyEnv) {
		t.Fatalf("告警文案应点名 %s，实际 %q", KeyEnv, logged[0])
	}
}

// 部分读取失败（读走 k 字节后报错）也必须整体归零 —— 否则会留下「半随机」脏缓冲。
func TestRandomKeyBytesZeroesPartialRead(t *testing.T) {
	origReader := randReader
	origLogf := logf
	t.Cleanup(func() { randReader = origReader; logf = origLogf })

	randReader = partialThenFailReader{fill: 5}
	logf = func(string, ...any) {}

	got := randomKeyBytes()
	for i, b := range got {
		if b != 0 {
			t.Fatalf("部分读取失败后必须整体归零，但 buf[%d]=%d", i, b)
		}
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("随机源不可用（测试注入）")
}

type partialThenFailReader struct{ fill int }

func (p partialThenFailReader) Read(b []byte) (int, error) {
	n := p.fill
	if n > len(b) {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		b[i] = 0xab // 可辨识的非零内容，用来证明"确实被读进来了、随后被清掉"
	}
	return n, errors.New("读取中途失败（测试注入）")
}

var _ io.Reader = failingReader{}
