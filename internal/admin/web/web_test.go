package web

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
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

// independentFromRust 登记「已独立演化、不再以 Rust 原件为参照」的静态文件。
//
// 历史沿革（为什么判据换过两次）：
//   - B1 之前两边是「原样搬」，逐字节一致是合理判据；
//   - B1 之后 Go 侧长出 Rust 侧没有的东西（调度内核状态、effective 列、
//     event_log 读入口、三态可用性），逐字节一致失效，改成「变更行数上限」；
//   - 本版把这套 UI 整体重做过（列合并、进度条、toast、可访问性、响应式），
//     与 Rust 原件的关系从「几乎相同」变成「同源但已分叉」——此时"变更行数上限"
//     不再携带任何信息：一份重写到 90% 的界面，把上限调成 300 行也只是走形式，
//     而它守护的真正目的（别乱改）与当前意图（就是要改）直接冲突。
//
// 所以判据收成两条，两条都还能被单独证伪：
//  1. 登记过的文件必须**确实**与 Rust 原件不同 —— 若哪天被同步回去、变得逐字节
//     相同，说明这份登记已陈旧，必须删掉，否则名单长期挂着一条假豁免；
//  2. 未登记的文件必须逐字节一致 —— 新增静态文件不许悄悄分叉。
//
// 「差异有多大」这条被有意放弃，由 TestEveryReferencedElementIDExists 接手：
// 重构中最常见的真 bug 是「id 改名、引用漏改」，那份测试直接卡这个，比数行数有用。
var independentFromRust = map[string]string{
	"index.html": "Go 侧有调度内核状态横幅（写死「已启用」会在 DISPATCH_SCHEDULER=off 时撒谎）、" +
		"告警页（event_log 的读入口）、登录用户名框；本版另做布局与语义重做" +
		"（scope=col / aria-current / 可关闭横幅 / 节点搜索 / 合并「调度参数」列）。",
	"app.js": "Go 侧独有：① 策略行的 effective 逐条渲染；② 告警页 loadEvents/renderEvents；" +
		"③ 节点三态可用性（state 由内核给、reachable 只表示端口通）；④ 登录带 username；" +
		"⑤ 本版新增进度条、toast、自动刷新、横幅状态文案。另：Rust 侧存在 POST /api/admin/tasks，" +
		"Go 侧已删除，前端也不得有任何创建任务的路径。",
	"app.css": "为上述新增元素供给样式：.prog/.toast/.stack/.dot-warn/.eff-on/.lvl-*/" +
		":focus-visible/响应式断点/prefers-reduced-motion。",
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
		if reason, registered := independentFromRust[c.name]; registered {
			if strings.TrimSpace(reason) == "" {
				t.Fatalf("%s 登记为「已独立演化」但没写理由 —— 写不出理由说明它不该分叉", c.name)
			}
			if bytes.Equal(c.got, want) {
				t.Fatalf("%s 登记为「已独立演化」，但实际与 Rust 原件逐字节相同 —— "+
					"登记已陈旧（差异被同步回去了），请从 independentFromRust 中删除该项", c.name)
			}
			continue
		}
		if !bytes.Equal(c.got, want) {
			t.Fatalf("内嵌 %s 与 Rust 源文件不一致（len got=%d want=%d），"+
				"且未登记在 independentFromRust 里 —— 要么同步，要么登记并写明理由",
				c.name, len(c.got), len(want))
		}
	}
}

var (
	htmlIDRe   = regexp.MustCompile(`id="([A-Za-z][\w-]*)"`)
	jsByIDRe   = regexp.MustCompile(`\$\('([A-Za-z][\w-]*)'\)`)
	jsHashIDRe = regexp.MustCompile(`#([a-z][a-z0-9-]*)`)
)

// TestEveryReferencedElementIDExists 卡「id 改名、引用漏改」。
//
// 为什么值得单独一道锁：三件套之间唯一的耦合面就是元素 id，而它是**字符串**——
// 编译器看不见。任何一个 $('x') 的 x 写错，运行时只会得到 null，
// 表现为"点了没反应"或"整页初始化时中断"（init 里第一个 null.addEventListener 就炸），
// 而所有 Go 测试照样全绿。本版重做了表格与新增了十来个元素，正是这类错误的高发场景。
//
// 只覆盖两种取元素写法：$('x') 与 querySelector('#x ...')。
// 动态拼接（`view-${name}`）模板串匹配不到，由下面的显式清单兜住。
func TestEveryReferencedElementIDExists(t *testing.T) {
	have := make(map[string]bool, 64)
	for _, m := range htmlIDRe.FindAllStringSubmatch(string(indexHTML), -1) {
		have[m[1]] = true
	}

	var missing []string
	seen := make(map[string]bool)
	addRef := func(id string) {
		if have[id] || seen[id] {
			return
		}
		seen[id] = true
		missing = append(missing, id)
	}

	for _, line := range strings.Split(string(appJS), "\n") {
		for _, m := range jsByIDRe.FindAllStringSubmatch(line, -1) {
			addRef(m[1])
		}
		if strings.Contains(line, "querySelector") {
			for _, m := range jsHashIDRe.FindAllStringSubmatch(line, -1) {
				addRef(m[1])
			}
		}
	}

	// show() 用模板串拼 view-<name>，六个视图容器必须都在，否则切页时 null.hidden 抛错。
	for _, name := range []string{"login", "overview", "nodes", "tasks", "events", "settings"} {
		addRef("view-" + name)
	}

	if len(missing) > 0 {
		t.Fatalf("app.js 引用了 index.html 里不存在的元素 id：%v\n"+
			"（这类错误编译期和 Go 侧测试都发现不了，运行时表现为「点了没反应」或 init 中断）", missing)
	}
}

// testdataDir 返回临时目录，供下面几个覆盖/回落用例复用。
func testdataDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// DISPATCH_WEB_DIR 生效时，必须返回磁盘文件内容（覆盖内嵌）。
func TestWebDirOverrideWins(t *testing.T) {
	dir := testdataDir(t)
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
	dir := testdataDir(t)
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
	dir := testdataDir(t) // 目录为空
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
