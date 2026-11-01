// Package dedup 是 B3「跨来源去重」的**纯计算**部分：把一次提交换算成一组
// 分级去重键，并给出"命中该怎么办"。
//
// # 为什么是五级键而不是一个哈希
//
// 设计（`design/02 §2.2.3`）实测出两件事：
//   - 节点自身的 `task_guid = "md_" + sha1(url_raw)` ⇒ 自创归一化哈希会与**节点的判重错位**；
//   - 网盘直链每次重签 ⇒ URL 一变，"同文件重推"就漏判。
//
// 所以层级不是"越严格越好"，而是**每条键各自负责一类漏判**：
//
//	L1  infohash      BT 场景 100%，重复提交必被抓
//	L2a task_guid     与节点算法对齐（文本完全相同的 URL）
//	L2c 稳定键        referrer+文件名+大小，跨重签仍然稳
//	L2b 归一化 URL    中置信：只能"标记待裁决"，不能跳过
//	L3  名+大小      中置信：同上
//
// # 一个硬纪律：中置信绝不跳过
//
// 「误判跳过」比「重复下载」危害大得多 —— 漏任务不可挽回，重复下载只是浪费带宽
// （`design/02` Q4 的原话）。所以 exact/high 才允许跳过，medium 一律"建任务 + 标记"。
package dedup

import (
	"crypto/sha1" //nolint:gosec // 协议兼容：节点的 task_guid 就是 md_+sha1(url)，不是安全用途
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// Level 是 `dedup_index.level`（DDL 只允许 1/2/3）。
type Level int

const (
	// LevelContent 内容级（BT infohash）。
	LevelContent Level = 1
	// LevelExact 精确级（task_guid / 稳定键）。
	LevelExact Level = 2
	// LevelSuspect 疑似级（归一化 URL / 名+大小）。
	LevelSuspect Level = 3
)

// Confidence 是 `dedup_index.confidence`（DDL 只允许 exact/high/medium）。
type Confidence string

const (
	// ConfExact 精确：命中即可跳过。
	ConfExact Confidence = "exact"
	// ConfHigh 高置信：命中即可跳过（跨重签稳定）。
	ConfHigh Confidence = "high"
	// ConfMedium 中置信：只标记，不跳过。
	ConfMedium Confidence = "medium"
)

// Key 是一条去重键。
type Key struct {
	// Value 键本体（写 `dedup_index.dedup_key`）。
	Value string
	Level Level
	Conf  Confidence
	// Rule 命中的规则名（L1/L2a/L2c/L2b/L3），写进日志便于解释"凭什么判重"。
	Rule string
}

// Skippable 表示命中这条键时是否允许**跳过投递**。
//
// 只有 exact/high 允许。这个判断单独成函数，是为了让"中置信被跳过"这类
// 事故在代码层面只有一处可能出错，且能被单测直接钉住。
func (k Key) Skippable() bool {
	return k.Conf == ConfExact || k.Conf == ConfHigh
}

// Input 是算键所需的字段。
type Input struct {
	// Kind ∈ http/bt/magnet/torrent。
	Kind string
	// URLRaw 原始 URL（未归一化）。
	URLRaw string
	// Name 任务名（可能为空）。
	Name string
	// Referrer 来源（网盘来源键，可能为空）。
	Referrer string
	// TotalSize 已解析的文件大小（0 表示未知）。
	TotalSize int64
}

// Keys 计算一个任务的**全部**去重键，按"强度降序"返回（第一个命中就用它）。
//
// 缺字段的层级会被跳过（例如 `total_size` 未知时不产 L2c / L3），
// 而不是用 0 硬凑一个键 —— 用 0 凑出来的键会把"大小未知的不同文件"判成重复。
func Keys(in Input) []Key {
	out := make([]Key, 0, 4)

	// ── L1 infohash（BT / 磁力 / torrent）──────────────────────────────
	if h := infohash(in.URLRaw); h != "" {
		out = append(out, Key{
			Value: "bt:" + h, Level: LevelContent, Conf: ConfExact, Rule: "L1",
		})
	}

	// ── L2a task_guid（与节点算法对齐）─────────────────────────────────
	if in.URLRaw != "" {
		out = append(out, Key{
			Value: "guid:" + TaskGUID(in.URLRaw), Level: LevelExact, Conf: ConfExact, Rule: "L2a",
		})
	}

	// ── L2c 稳定键（跨重签）────────────────────────────────────────────
	//
	// 三段缺一不可：`referrer` 缺时该段为空会让"不同来源的同名文件"撞在一起，
	// 所以要求 referrer 与 name 同时非空才产这条键。
	if in.Referrer != "" && in.Name != "" && in.TotalSize > 0 {
		out = append(out, Key{
			Value: fmt.Sprintf("src:%s|file:%s|size:%d", in.Referrer, nameNorm(in.Name), in.TotalSize),
			Level: LevelExact, Conf: ConfHigh, Rule: "L2c",
		})
	}

	// ── L2b 归一化 URL（中置信）────────────────────────────────────────
	if norm := NormalizeURL(in.URLRaw); norm != "" {
		sum := sha256.Sum256([]byte(norm))
		out = append(out, Key{
			Value: "http:" + hex.EncodeToString(sum[:]), Level: LevelSuspect, Conf: ConfMedium, Rule: "L2b",
		})
	}

	// ── L3 名 + 大小（中置信）──────────────────────────────────────────
	//
	// ⚠️ 绝不"仅凭文件名相同"判重（`design/02 §2.2.3` 的 F33）：必须带大小。
	if in.Name != "" && in.TotalSize > 0 {
		sum := sha256.Sum256([]byte(nameNorm(in.Name) + "\x00" + itoa(in.TotalSize)))
		out = append(out, Key{
			Value: "ns:" + hex.EncodeToString(sum[:]), Level: LevelSuspect, Conf: ConfMedium, Rule: "L3",
		})
	}
	return out
}

// TaskGUID 复刻节点的 `task_guid = "md_" + sha1(url_raw)`（`design/02` A-10 / F13）。
//
// ⚠️ 用的是**原始** URL，不是归一化 URL：节点就是这么算的，我们要和它判重**对齐**，
// 而不是比它更聪明。更聪明的结果是"节点认为重复、代理认为不重复"，然后重复下载。
func TaskGUID(rawURL string) string {
	//nolint:gosec // 同上：协议兼容用途，不用于任何安全判定
	sum := sha1.Sum([]byte(rawURL))
	return "md_" + hex.EncodeToString(sum[:])
}

// queryDeny 是要丢弃的查询参数（跟踪类 + 网盘时效签名）。
//
// 少了后半段（`auth_key`/`token`/`abt`…）L2b 会完全失效：
// 同一文件每次重推都带新的签名 ⇒ 每次都算成"不同 URL"。
var queryDeny = map[string]bool{
	// 跟踪参数
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "spm": true, "from": true,
	"referrer": true, "tk": true, "share_id": true, "_t": true,
	"timestamp": true, "sign": true,
	// 网盘时效签名（F26）
	"auth_key": true, "token": true, "abt": true, "ud": true,
	"ork": true, "sp": true, "dfi": true,
}

// NormalizeURL 按 `design/02 §2.2.3` 的六条规则归一化 URL。
//
// 任何一步失败都返回空串（调用方据此跳过 L2b），**不返回原始串**：
// 拿原始串当"归一化结果"会产生一条与 L2a 重复但置信度更低的键，纯属噪音。
func NormalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	// 1) scheme 小写；仅允许 http/https/ftp
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "ftp":
	default:
		return ""
	}
	// 2) host 小写 + 去默认端口
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") || (scheme == "ftp" && port == "21") {
		port = ""
	}
	// 3) 去 fragment（u.Fragment 天然不含 #，这里只要不拼回去即可）
	// 4) query 排序 + 丢 denylist
	q := u.Query()
	for k := range q {
		if queryDeny[strings.ToLower(k)] {
			q.Del(k)
		}
	}
	// 5) path：解码 unreserved、折叠重复 '/'、去末尾 '/'
	p := foldSlashes(u.EscapedPath())
	p = decodeUnreserved(p)
	p = strings.TrimRight(p, "/")

	var b strings.Builder
	b.WriteString(scheme)
	b.WriteString("://")
	b.WriteString(host)
	if port != "" {
		b.WriteString(":")
		b.WriteString(port)
	}
	b.WriteString(p)
	// url.Values.Encode 已经按 key 字典序排序，正好满足规则 4 的"按 key 排序"。
	if enc := q.Encode(); enc != "" {
		b.WriteString("?")
		b.WriteString(enc)
	}
	return b.String()
}

// foldSlashes 把连续 '/' 折叠成一个（保留开头的 '/'）。
func foldSlashes(p string) string {
	if !strings.Contains(p, "//") {
		return p
	}
	var b strings.Builder
	prevSlash := false
	for _, r := range p {
		if r == '/' {
			if prevSlash {
				continue
			}
			prevSlash = true
		} else {
			prevSlash = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// decodeUnreserved 把 unreserved 字符的百分号编码还原（%7E→~ 等）。
//
// 只处理 unreserved 集合（`A-Za-z0-9-._~`）：把 `%2F` 也还原会把路径分隔符
// 解出来，等于把 "/a%2Fb" 和 "/a/b" 判成同一个 URL —— 那是错的。
func decodeUnreserved(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '%' && i+2 < len(p) {
			if v, err := hex.DecodeString(p[i+1 : i+3]); err == nil && len(v) == 1 {
				c := v[0]
				if isUnreserved(c) {
					b.WriteByte(c)
					i += 2
					continue
				}
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// isUnreserved 判断是否是 RFC 3986 的 unreserved 字符。
func isUnreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '.', c == '_', c == '~':
		return true
	}
	return false
}

// infohash 从磁力链接里取 infohash（40 位 hex 或 32 位 base32），统一输出小写 hex。
//
// `.torrent` 文件的元信息要解析 bencode，属 B3 之外（当前入口只收 URL/磁力）——
// 这里**不猜**：拿不到就返回空，让 L2a/L2c 接手。
func infohash(raw string) string {
	lower := strings.ToLower(raw)
	i := strings.Index(lower, "urn:btih:")
	if i < 0 {
		return ""
	}
	rest := lower[i+len("urn:btih:"):]
	// 到 '&' 或 '?' 为止
	if j := strings.IndexAny(rest, "&?#"); j >= 0 {
		rest = rest[:j]
	}
	switch len(rest) {
	case 40:
		if _, err := hex.DecodeString(rest); err == nil {
			return rest
		}
	case 32:
		if b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(rest)); err == nil && len(b) == 20 {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

// nameNorm 是名称归一化：小写、去扩展名、把非字母数字折成单个 '.'。
//
// 与分组识别（`internal/grouping`）的 stem 归一化保持同向，但**更宽松**：
// 去重宁可多算几个键（多一条待裁决），也不能因为大小写/下划线的差异漏判。
func nameNorm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// 去扩展名（只看最后一段的最后一个点）
	if i := strings.LastIndex(s, "."); i > 0 {
		switch s[i+1:] {
		case "ts", "mp4", "mkv", "avi", "rmvb", "flv", "mov", "wmv", "m2ts", "iso":
			s = s[:i]
		}
	}
	var b strings.Builder
	lastDot := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127 {
			b.WriteRune(r)
			lastDot = false
			continue
		}
		if !lastDot {
			b.WriteByte('.')
			lastDot = true
		}
	}
	return strings.Trim(b.String(), ".")
}

// itoa 避免为一行格式化引入 strconv。
func itoa(v int64) string { return fmt.Sprintf("%d", v) }
