// Package protocol implements the on-the-wire primitives the download-gateway
// uses to talk to BitComet WebUI nodes.
//
// This file ports crates/bitcomet-api/src/addtask.rs 1:1.
//
// 添加任务的输入分类与拆分（契约 §12.1）。判定顺序固定为两步：先看是否以
// `magnet:` 开头，再看（剥掉 query/fragment 后）是否以 `.torrent` 结尾。两类都走
// `POST /api/task/torrent_links/add`；其余一律走 `POST /api/task/http/add`。
//
// 强调顺序的原因：`magnet:?xt=...&dn=xxx.torrent` 这种链接也以 `.torrent` 结尾，
// 若先判后缀就会把它当直链发去 `http/add`，服务端必然拒绝。
package protocol

import "strings"

// AddKind is the endpoint class a single input link should be sent to.
type AddKind int

const (
	// AddKindTorrent is for `magnet:` or `.torrent` inputs -> torrent_links/add.
	AddKindTorrent AddKind = iota
	// AddKindHTTP is for everything else (http(s)://, ftp://, ...) -> http/add.
	AddKindHTTP
)

// Endpoint returns the server endpoint for this kind.
func (k AddKind) Endpoint() string {
	if k == AddKindTorrent {
		return "/api/task/torrent_links/add"
	}
	return "/api/task/http/add"
}

// asciiLower lowercases only ASCII A-Z, matching Rust's `to_ascii_lowercase`
// (Go's strings.ToLower is Unicode-aware and could change byte length).
func asciiLower(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// ClassifyAddTarget decides the kind of a single raw link. Empty input
// classifies as HTTP (the caller filters blanks during splitting).
func ClassifyAddTarget(raw string) AddKind {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(asciiLower(s), "magnet:") {
		return AddKindTorrent
	}
	// Strip ?query / #fragment before the suffix test: `.torrent?token=...` is
	// still a torrent link.
	head := s
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		head = s[:i]
	}
	if strings.HasSuffix(asciiLower(head), ".torrent") {
		return AddKindTorrent
	}
	return AddKindHTTP
}

// SplitAddLinks splits a (possibly multi-line) input into individual links.
//
// Delimiters: newline, carriage return, comma, semicolon, and the Chinese
// comma/semicolon. It deliberately does NOT split on spaces — unescaped URLs
// containing spaces are common and would otherwise be torn into junk links.
func SplitAddLinks(input string) []string {
	parts := strings.FieldsFunc(input, func(r rune) bool {
		switch r {
		case '\n', '\r', ',', ';', '，', '；':
			return true
		}
		return false
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// CollectAddLinks aggregates multiple inputs (e.g. several CLI --add-task
// flags) into one de-duplicated link list, preserving first-seen order.
func CollectAddLinks(inputs []string) []string {
	out := []string{}
	for _, raw := range inputs {
		for _, link := range SplitAddLinks(raw) {
			if !containsString(out, link) {
				out = append(out, link)
			}
		}
	}
	return out
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
