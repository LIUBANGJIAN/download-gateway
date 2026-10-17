// Package web 提供内嵌的零构建管理台（3 屏 + 原生 ES 模块）。
//
// # 1:1 移植来源
//
// `crates/dispatch-core/src/admin/web/mod.rs`：三个静态文件用 `include_str!` 编进二进制
// （零依赖、可复现）；若设置了 `DISPATCH_WEB_DIR`，则改从磁盘读同名文件（便于热改/排障）。
// Go 用 `//go:embed` 等价实现。三个静态文件（`index.html`/`app.css`/`app.js`）从 Rust 侧
// **原样搬**，内容与格式均未改动。
//
// # Go 差异（已在回执记明）
//
// Rust 的 axum handler 直接收 `State<Arc<AdminState>>`（从而读到 `st.web_dir`）；
// Go 侧把 `webDir` 作为**显式参数**，使本包**只依赖标准库**，避免 `state ↔ web` 的包耦合，
// 也让 T04 的 admin router 自由决定 web_dir 的来源。
package web

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
)

// 三个内嵌静态文件（`//go:embed` 于编译期编进二进制）。
//
//go:embed index.html
var indexHTML []byte

//go:embed app.css
var appCSS []byte

//go:embed app.js
var appJS []byte

// 内容类型常量（逐字对照 Rust `mod.rs`）。
const (
	// ContentTypeHTML `GET /`、`/login`、`/tasks`、`/tasks/new` 壳页面的 Content-Type。
	ContentTypeHTML = "text/html; charset=utf-8"
	// ContentTypeCSS `GET /app.css` 的 Content-Type。
	ContentTypeCSS = "text/css; charset=utf-8"
	// ContentTypeJS `GET /app.js` 的 Content-Type。
	ContentTypeJS = "text/javascript; charset=utf-8"
)

// Index 返回壳页面字节：优先磁盘 `webDir/index.html`，回落内嵌。
func Index(webDir *string) []byte { return readAsset(webDir, "index.html", indexHTML) }

// CSS 返回样式字节：优先磁盘 `webDir/app.css`，回落内嵌。
func CSS(webDir *string) []byte { return readAsset(webDir, "app.css", appCSS) }

// JS 返回脚本字节：优先磁盘 `webDir/app.js`，回落内嵌。
func JS(webDir *string) []byte { return readAsset(webDir, "app.js", appJS) }

// readAsset 读取资源：优先磁盘目录，回落内嵌。
//
// 对照 Rust `read_asset` 的三条出口：
//   - `webDir == nil`（未设置 DISPATCH_WEB_DIR）⇒ 内嵌；
//   - 磁盘文件**为空** ⇒ 记 warn、内嵌；
//   - 磁盘读取**失败** ⇒ 记 warn、内嵌；
//   - 否则返回磁盘内容。
func readAsset(webDir *string, name string, embedded []byte) []byte {
	if webDir != nil {
		path := filepath.Join(*webDir, name)
		b, err := os.ReadFile(path)
		switch {
		case err != nil:
			log.Printf("读 DISPATCH_WEB_DIR 失败，回落内嵌：path=%s err=%v", path, err)
		case len(b) == 0:
			log.Printf("DISPATCH_WEB_DIR 下文件为空，回落内嵌：path=%s", path)
		default:
			return b
		}
	}
	return embedded
}
