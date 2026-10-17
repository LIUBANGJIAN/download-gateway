package ids

import (
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 与 Rust 侧 `crates/dispatch-core/src/ids.rs` 的 `mod tests` 逐条对照
// ---------------------------------------------------------------------------

// 对照 `gid_matches_16_lower_hex`：1000 次抽取，每次都是 16 位纯小写十六进制。
func TestNewGIDMatches16LowerHex(t *testing.T) {
	for i := 0; i < 1000; i++ {
		g := NewGID()
		if len(g) != 16 {
			t.Fatalf("GID 长度应为 16，实际 %q（第 %d 次）", g, i)
		}
		for _, c := range g {
			isLowerHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isLowerHex {
				t.Fatalf("GID 必须是纯小写十六进制，实际 %q（第 %d 次）", g, i)
			}
		}
	}
}

// 对照 `gid_is_unique_over_1000_draws`：1000 次抽取零重复。
func TestNewGIDUniqueOver1000Draws(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		g := NewGID()
		if _, dup := seen[g]; dup {
			t.Fatalf("1000 次抽取出现重复 GID：%s", g)
		}
		seen[g] = struct{}{}
	}
	if len(seen) != 1000 {
		t.Fatalf("应得 1000 个不同 GID，实际 %d", len(seen))
	}
}

// 对照 `task_id_is_26_chars`。
func TestNewTaskIDIs26Chars(t *testing.T) {
	id := NewTaskID(1_700_000_000_000)
	if len(id) != 26 {
		t.Fatalf("ULID 应为 26 字符，实际 %d：%q", len(id), id)
	}
	for _, c := range id {
		if !strings.ContainsRune(crockford, c) {
			t.Fatalf("字符必须来自 Crockford 字母表，实际 %q：%q", c, id)
		}
	}
}

// 对照 `task_id_time_prefix_is_monotonic`。
func TestNewTaskIDTimePrefixIsMonotonic(t *testing.T) {
	a := NewTaskID(1_000_000_000_000)
	b := NewTaskID(1_000_000_001_000)
	c := NewTaskID(2_000_000_000_000)
	if !(a[:10] <= b[:10]) {
		t.Fatalf("时间前缀应不减：%q vs %q", a, b)
	}
	if !(b[:10] <= c[:10]) {
		t.Fatalf("时间前缀应不减：%q vs %q", b, c)
	}
}

// 对照 `hex_128_is_32_lower_hex`。
func TestNewHex128Is32LowerHex(t *testing.T) {
	h := NewHex128()
	if len(h) != 32 {
		t.Fatalf("应为 32 字符，实际 %d：%q", len(h), h)
	}
	for _, c := range h {
		isLowerHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isLowerHex {
			t.Fatalf("必须是小写十六进制，实际 %q", h)
		}
	}
}

// ---------------------------------------------------------------------------
// 本包新增：Rust 侧只靠文档声明、没有断言的三条性质
// ---------------------------------------------------------------------------

// 「时间前缀单调」只是必要条件 —— 它无法证伪「前缀里混进了随机位」。
// 这里做**真正的往返解码**：把前 10 个字符解回 48bit，必须逐位等于入参时间戳。
func TestNewTaskIDTimePrefixDecodesBackToInputTimestamp(t *testing.T) {
	stamps := []int64{
		1,                 // 下界附近
		1_000_000_000_000, // 2001-09-09
		1_700_000_000_000, // 2023-11-14
		2_814_748_706_657, // 48bit 时间戳上界（公元 10889 年）
	}
	for _, ts := range stamps {
		id := NewTaskID(ts)
		got := decodeULIDTimePrefix(t, id[:10])
		if got != uint64(ts) {
			t.Fatalf("时间前缀往返失败：入参 %d，解回 %d（id=%q）", ts, got, id)
		}
	}
}

// 负时间戳按 0 处理（Rust `now_ms.max(0)` 的显式移植）。
func TestNewTaskIDNegativeTimestampClampsToZero(t *testing.T) {
	id := NewTaskID(-1)
	if got := decodeULIDTimePrefix(t, id[:10]); got != 0 {
		t.Fatalf("负时间戳应钳到 0，实际解回 %d（id=%q）", got, id)
	}
}

// 26 字符 × 5bit = 130bit > 128bit ⇒ 首字符只能取 0..=7。
// 这条不是形式主义：它同时钉住了「编码没有把高位写反」这一整类错误。
func TestNewTaskIDFirstCharIsWithinHeaderRange(t *testing.T) {
	for i := 0; i < 500; i++ {
		id := NewTaskID(1_700_000_000_000)
		if id[0] < '0' || id[0] > '7' {
			t.Fatalf("首字符必须落在 0..=7（最高的 2bit 恒为零），实际 %q：%q", id[0], id)
		}
	}
}

// 随机源故障 ⇒ 回落全零（**不 panic**）。
//
// Rust 侧这条分支是**不可测的**（它直接调 `getrandom::fill`，没有注入口），
// 只能靠注释声明；Go 版把随机源提成包级变量，于是这条降级语义第一次有了可执行断言。
func TestRandomBytesFallsBackToZeroWhenSourceFails(t *testing.T) {
	orig := randReader
	t.Cleanup(func() { randReader = orig })
	randReader = failingReader{}

	for _, n := range []int{1, 8, 10, 16} {
		buf := randomBytes(n)
		if len(buf) != n {
			t.Fatalf("长度应为 %d，实际 %d", n, len(buf))
		}
		for i, b := range buf {
			if b != 0 {
				t.Fatalf("降级路径应回落全零，但 buf[%d]=%d（n=%d）", i, b, n)
			}
		}
	}
}

// 部分读取失败（读走 k 字节后报错）也必须整体归零 ——
// 否则会留下「一半真随机、一半零」的脏缓冲，这类残留在 Rust 侧完全没被覆盖。
func TestRandomBytesZeroesPartialReadOnFailure(t *testing.T) {
	orig := randReader
	t.Cleanup(func() { randReader = orig })
	randReader = partialThenFailReader{fill: 3}

	buf := randomBytes(8)
	for i, b := range buf {
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

// decodeULIDTimePrefix 把 ULID 的前 10 个字符解码回 48bit 毫秒时间戳。
// 仅测试使用 —— 生产代码只编码不编码，避免无谓的解析面。
func decodeULIDTimePrefix(t *testing.T, prefix10 string) uint64 {
	t.Helper()
	if len(prefix10) != 10 {
		t.Fatalf("时间前缀应为 10 字符，实际 %q", prefix10)
	}
	var v uint64
	for _, c := range prefix10 {
		idx := strings.IndexRune(crockford, c)
		if idx < 0 {
			t.Fatalf("非法 Crockford 字符 %q（前缀 %q）", c, prefix10)
		}
		v = v<<5 | uint64(idx)
	}
	// 10 字符 × 5bit = 50bit，其中高 2bit 必须为零，低 48bit 才是时间戳。
	if v>>48 != 0 {
		t.Fatalf("时间前缀的高 2bit 应为零，实际解出 %b（前缀 %q）", v>>48, prefix10)
	}
	return v & 0xffff_ffff_ffff
}
