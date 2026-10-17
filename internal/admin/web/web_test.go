package web

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 内嵌三文件必须在场且内容可辨（能杀掉「embed 路径写错 ⇒ 空资源」的变异）。
func TestEmbeddedAssetsArePresent(t *testing.T) {
	idx := Index(nil)
	if len(idx) == 0 {
		t.Fatalf("内嵌 index.html 不得为空")
	}
	if !bytes.Contains(idx, []byte("<!DOCTYPE html>")) || !bytes.Contains(idx, []byte("下载网关")) {
		t.Fatalf("内嵌 index.html 内容不像管理台壳页面")
	}
	css := CSS(nil)
	if len(css) == 0 || !bytes.Contains(css, []byte(":root")) {
		t.Fatalf("内嵌 app.css 应非空且含 :root")
	}
	js := JS(nil)
	if len(js) == 0 || !bytes.Contains(js, []byte("VIEWS")) {
		t.Fatalf("内嵌 app.js 应非空且含 VIEWS")
	}
}

// 内嵌字节必须与包目录下的静态文件**逐字节一致**（否则说明 embed 与磁盘文件脱钩）。
func TestEmbeddedMatchesLocalStaticFiles(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
	}{
		{"index.html", Index(nil)},
		{"app.css", CSS(nil)},
		{"app.js", JS(nil)},
	}
	for _, c := range cases {
		want, err := os.ReadFile(c.name)
		if err != nil {
			t.Fatalf("读本地静态文件 %s 失败：%v", c.name, err)
		}
		if !bytes.Equal(c.got, want) {
			t.Fatalf("内嵌 %s 与本地文件不一致（len got=%d want=%d）", c.name, len(c.got), len(want))
		}
	}
}

// rustOriginDiff 登记「Go 内嵌页与 Rust 原件之间**已知且有理由**的差异。
//
// 为什么需要这份白名单：B1 之前两边是"原样搬"，逐字节一致是合理判据。
// B1 之后 Go 侧有了调度内核（真的选点 / 下发 / 回读），Rust 侧没有 ——
// 于是**两边的正确文案本来就不一样**：Go 版写"未启用"是自贬，Rust 版写"已启用"是撒谎。
// 逐字节一致这条判据在这里已经失效，但"随便改"也不行。
//
// 所以判据换成三条，每条都能单独证伪：
//  1. 未登记的文件（`app.css` / `app.js`）仍必须逐字节一致 —— 它们没有理由分叉；
//  2. 已登记的文件必须**确实**存在差异 —— 否则白名单变陈旧（差异被同步回去了，
//     登记项就该删掉），否则这份名单会长期挂着一条假豁免；
//  3. 差异行数不得超过登记值 —— 扩大差异必须回来改这个数字，也就必然读到这段说明。
type rustOriginDiff struct {
	// maxChangedLines 是允许的最大变更行数（新增行 + 删除行）。
	maxChangedLines int
	// reason 说明为什么两边**应该**不同。写不出理由，就说明那处不该改。
	reason string
}

var rustOriginDiffs = map[string]rustOriginDiff{
	"index.html": {
		maxChangedLines: 8,
		reason: "Go 侧已有调度内核、Rust 侧没有。启动横幅与「删节点文件」提示必须不同 —— " +
			"写成一样，必然有一边在撒谎（Go 自贬为「不会真正下载」，或 Rust 假称「已启用」）。",
	},
}

// changedLines 统计两份文本按行比较的变更行数（多重集对称差）。
//
// 够用即可：两份文件都由同一套模板演化而来、行序基本一致，
// 引入 LCS 只会让这段测试代码本身变成需要维护的东西。
func changedLines(a, b []byte) int {
	norm := func(s []byte) []string {
		return strings.Split(strings.ReplaceAll(string(s), "\r\n", "\n"), "\n")
	}
	al, bl := norm(a), norm(b)
	count := make(map[string]int, len(al))
	for _, l := range al {
		count[l]++
	}
	n := 0
	for _, l := range bl {
		if count[l] > 0 {
			count[l]--
		} else {
			n++
		}
	}
	for _, c := range count {
		n += c
	}
	return n
}

// TestEmbeddedMatchesRustOriginals 校验内嵌三文件与 Rust 侧源文件的关系。
// Rust 源目录不可达时跳过（过渡态后 Rust 树可能被移除）。
func TestEmbeddedMatchesRustOriginals(t *testing.T) {
	originDir := filepath.Join("..", "..", "..", "crates", "dispatch-core", "src", "admin", "web")
	cases := []struct {
		name string
		got  []byte
	}{
		{"index.html", Index(nil)},
		{"app.css", CSS(nil)},
		{"app.js", JS(nil)},
	}
	for _, c := range cases {
		want, err := os.ReadFile(filepath.Join(originDir, c.name))
		if err != nil {
			t.Skipf("Rust 源目录不可达，跳过原样性比对：%v", err)
		}
		if d, registered := rustOriginDiffs[c.name]; registered {
			n := changedLines(want, c.got)
			if n == 0 {
				t.Fatalf("%s 已登记为「允许有差异」，但实际与 Rust 原件逐字节相同 —— "+
					"白名单已陈旧，请删除该登记项", c.name)
			}
			if n > d.maxChangedLines {
				t.Fatalf("%s 与 Rust 原件的差异扩大到 %d 行（登记上限 %d 行）。\n"+
					"若这处改动是有意的，请同步上调 rustOriginDiffs 里的上限并确认理由仍成立：\n  %s",
					c.name, n, d.maxChangedLines, d.reason)
			}
			continue
		}
		if !bytes.Equal(c.got, want) {
			t.Fatalf("内嵌 %s 与 Rust 源文件不一致（len got=%d want=%d），"+
				"且未登记在 rustOriginDiffs 里 —— 要么同步，要么登记并写明理由",
				c.name, len(c.got), len(want))
		}
	}
}

// DISPATCH_WEB_DIR 生效时，必须返回磁盘文件内容（覆盖内嵌）。
func TestWebDirOverrideWins(t *testing.T) {
	dir := t.TempDir()
	custom := []byte("<html>custom</html>\n")
	if err := os.WriteFile(filepath.Join(dir, "index.html"), custom, 0o600); err != nil {
		t.Fatalf("写覆盖文件失败：%v", err)
	}
	got := Index(&dir)
	if !bytes.Equal(got, custom) {
		t.Fatalf("应返回磁盘覆盖内容，实际 %q", got)
	}
}

// 磁盘文件**为空**时必须回落内嵌（不能返回空页面）。
func TestWebDirEmptyFileFallsBackToEmbedded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.css"), []byte{}, 0o600); err != nil {
		t.Fatalf("写空文件失败：%v", err)
	}
	got := CSS(&dir)
	if !bytes.Equal(got, appCSS) {
		t.Fatalf("空文件应回落内嵌，实际 len=%d", len(got))
	}
	if len(got) == 0 {
		t.Fatalf("回落内容不应为空")
	}
}

// 磁盘文件**不存在**时必须回落内嵌。
func TestWebDirMissingFileFallsBackToEmbedded(t *testing.T) {
	dir := t.TempDir() // 目录为空
	got := JS(&dir)
	if !bytes.Equal(got, appJS) {
		t.Fatalf("缺文件应回落内嵌，实际 len=%d", len(got))
	}
}

// webDir 指针非 nil 但指向不存在的目录，也必须回落内嵌（不 panic）。
func TestWebDirMissingDirectoryFallsBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if got := Index(&dir); !bytes.Equal(got, indexHTML) {
		t.Fatalf("目录不存在应回落内嵌，实际 len=%d", len(got))
	}
}
