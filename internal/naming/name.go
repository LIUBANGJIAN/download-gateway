// Package naming 是「代理前置接收」的补名器（`design/02 §4.5`，A-12 拍板）。
//
// # 它解决的是什么问题
//
// 上游推送插件**不能自定义文件名**（F34），只能发一个 URL。于是节点上所有任务都叫
// `01.ts`、`02.ts` —— 剧集信息在落盘那一刻就永久丢失了，同一个根目录下还会
// 互相覆盖（F33：同名覆盖，可能静默丢数据）。
//
// 代理是唯一能补上名字的一方：它手里可能有剧名（分组识别的 stem）。
// 所以下发前把 `filename` 一起发过去，**只改文件名、不建目录、不改路径**
// （`save_folder` 一律不下发 —— Q3 的拍板，任意路径会被节点拒绝，`save_folder invalid`）。
//
// # 四条纪律（`design/02 §4.5(5)`）
//
//  1. 不得把 `filename` 当必填：拿不到剧名就**静默不补**，不报错、不降级功能；
//  2. 只在能确定剧名时补：不确定就不补（补错会污染分组，比名字难看严重得多）；
//  3. 补出的名字**必须可逆**：格式 `<剧名>.S01E01.<原名>`，排障时能反推原始名；
//  4. 不得借 `filename` 改变落盘位置：`/` 会被规范成 `_`。
//
// 第 5 条纪律（审计）在调用方 `engine` 里执行：补名前先落 `event_log`。
package naming

import (
	"strings"
)

// Input 是补名所需的信息。
type Input struct {
	// Stem 剧名（分组识别得到，已过"像不像剧名"的校验）。
	Stem string
	// Season "S01" / 空。
	Season string
	// Episode 集号（0 表示未知）。
	Episode int64
	// OriginName 上游给的原始名（可能为空）。
	OriginName string
	// URL 最后兜底的 basename 来源（`OriginName` 为空时用它）。
	URL string
}

// Result 是补名结果。
type Result struct {
	// Filename 建议下发到节点的文件名（空 ⇒ 不补）。
	Filename string
	// Origin 原始名（写审计日志，用于"事后反推与批量纠正"）。
	Origin string
}

// Build 生成可逆的补名文件名：`<剧名>.S01E01.<原名>`。
//
// 返回空 `Filename` 的全部情形都是**有意的静默退回**（纪律 1、2）：
//   - 没有剧名（stem 为空）；
//   - 剧名不像剧名（纯数字 / 单字符 / 疑似哈希）；
//   - 原始名已经包含了剧名与集号（再补就重复了，例如上游自己传了规范名）。
func Build(in Input) Result {
	origin := strings.TrimSpace(in.OriginName)
	if origin == "" {
		origin = basename(in.URL)
	}
	stem := strings.TrimSpace(in.Stem)
	if !looksLikeTitle(stem) {
		return Result{Origin: origin}
	}

	// 原始名已经规范（含 stem 与集号）⇒ 不补，保持上游意图（纪律 2）。
	if alreadyCanonical(origin, stem) {
		return Result{Origin: origin}
	}

	tag := episodeTag(in.Season, in.Episode)
	var name string
	switch {
	case tag != "" && origin != "":
		name = sanitize(stem + "." + tag + "." + origin)
	case tag != "":
		name = sanitize(stem + "." + tag)
	case origin != "":
		name = sanitize(stem + "." + origin)
	default:
		name = sanitize(stem)
	}
	if name == "" {
		return Result{Origin: origin}
	}
	return Result{Filename: name, Origin: origin}
}

// episodeTag 生成 `S01E01` 形式的集标签（缺季或集则退化为可用的一半）。
func episodeTag(season string, episode int64) string {
	season = strings.ToUpper(strings.TrimSpace(season))
	hasSeason := len(season) == 3 && season[0] == 'S' && isDigits(season[1:])
	if episode <= 0 {
		if hasSeason {
			return season
		}
		return ""
	}
	ep := "E" + pad2(episode)
	if hasSeason {
		return season + ep
	}
	return ep
}

// looksLikeTitle 判断一个 stem "像不像剧名"。
//
// 判据刻意保守：这是**唯一**决定"补不补"的闸门，放宽的代价是把哈希、纯数字、
// 单字母当成剧名补到文件名前面 —— 那会污染后续的分组识别（用的是文件名）。
func looksLikeTitle(s string) bool {
	if len(s) < 2 || len([]rune(s)) > 80 {
		return false
	}
	// 纯数字 / 纯十六进制长串（疑似哈希）都不算
	digits := 0
	letters := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127:
			letters++
		}
	}
	if letters == 0 {
		return false
	}
	if len(s) >= 16 && digits > len(s)*3/4 {
		return false
	}
	// 纯十六进制且够长 ⇒ 疑似哈希（`9f86d081884c7d65` 这种绝不会是剧名）
	if len(s) >= 16 && isHex(s) {
		return false
	}
	return true
}

// isHex 判断是否全为十六进制字符。
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// alreadyCanonical 判断原始名是否已经含 stem 与集标签（再补就重复）。
func alreadyCanonical(origin, stem string) bool {
	if origin == "" {
		return false
	}
	lo := strings.ToLower(origin)
	if !strings.Contains(lo, strings.ToLower(stem)) {
		return false
	}
	// 含 SxxExx 或 Exx 才认为"已经规范"
	for i := 0; i+2 < len(lo); i++ {
		if lo[i] == 'e' && isDigit(lo[i+1]) && isDigit(lo[i+2]) {
			return true
		}
	}
	return false
}

// sanitize 把文件名里节点会拒绝或会改变路径的字符规范化。
//
// 纪律 4：`/` 与 `\` 一律换成 `_`。它们被节点解释成目录分隔符，
// 而"建目录"是明确不做的（Q3）—— 放过去等于偷偷改了落盘位置。
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			b.WriteByte('_')
		case '\n', '\r', '\t':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	// 连续空格折成一个（上游名字里常有 "  “）
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	// Windows 风格的前后点会丢字符
	out = strings.Trim(out, ".")
	return out
}

// basename 从 URL 里取最后一段（去 query/fragment）。
func basename(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimRight(raw, "/")
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	return raw
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// pad2 把集号补成两位（1 → "01"）。三位以上原样（有些番剧集号是 100+）。
func pad2(n int64) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	if n < 100 {
		return string(rune('0'+n/10)) + string(rune('0'+n%10))
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
