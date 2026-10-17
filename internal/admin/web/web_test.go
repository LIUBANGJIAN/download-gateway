package web

import (
	"bytes"
	"os"
	"path/filepath"
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

// ★「原样搬」判据：内嵌三文件必须与 Rust 侧源文件逐字节一致。
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
		if !bytes.Equal(c.got, want) {
			t.Fatalf("内嵌 %s 与 Rust 源文件不一致（len got=%d want=%d）", c.name, len(c.got), len(want))
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
