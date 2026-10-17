// Package grouping 把「一次推送」识别成「属于哪一季」，为组级硬绑定提供键。
//
// # 为什么必须在代理侧识别，而不是事后去节点反推
//
// `design/02 §2.2.1`（F24 逼出）：分组判定必须发生在**代理收到推送请求**时。
// 节点侧的任务可能只剩一个 `01.ts` —— 事后反推是信息缺失，不是"晚一点也能做"。
// 所以本包只吃 `task.name`（推送时用户给的名字），**从不**读取节点侧任务名。
//
// # 为什么识别失败要"不建组"而不是猜
//
// `design/04 §5.3`：**宁可退化为普通调度，也不要把两部剧错绑到一台**。
// 错绑的代价不是一次调度失误，而是一整季落到错误的机器上、且（B1 没有失效路径）
// 无法自动纠正。所以闸门（置信度 + 主干长度）不达标一律 `Key==""`。
package grouping

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// KeyOptions 是识别开关（来自 app_config / 代码默认）。
type KeyOptions struct {
	// ExplicitKey 上游显式组键（L0，conf 1.00）。本链路取不到，恒空；
	// 预留给将来 Aria2 `dir` / `X-Group-Name` 可用时接入。
	ExplicitKey string
	// IncludeSource `group.key_include_source`（默认 off）⇒ 键里带 `src:` 前缀。
	IncludeSource bool
	// TMDBPriority `group.tmdb_priority`（默认 on）⇒ 名字里的 `{tmdbid-123}` 优先分组。
	TMDBPriority bool
	// MinStem 主干最短长度（默认 3）。
	MinStem int
	// ConfMin 置信度闸门（默认 0.8）。
	ConfMin float64
}

// DefaultKeyOptions 返回设计文档给出的默认开关。
//
// `group.key_include_source=off`、`group.tmdb_priority=on` 见 `design/02 §2.2.1`；
// `CONF_MIN=0.8` 见 `design/04 §5.3`；`MIN_STEM=3`（含 CJK 可放宽到 2）见 `design/04 §17` 项 19。
func DefaultKeyOptions() KeyOptions {
	return KeyOptions{
		IncludeSource: false,
		TMDBPriority:  true,
		MinStem:       3,
		ConfMin:       0.8,
	}
}

// KeyResult 是分组识别结果。`Key==""` 表示"不建组"（走普通负载调度）。
type KeyResult struct {
	// Key affinity_key；空串 ⇒ 不建组。
	Key string
	// Layer 命中的判据层："L0"|"L1"|"L2"|"L3"（L0b 只提示、不绑定）。
	Layer string
	// Confidence 置信度（L3 为 0）。
	Confidence float64
	// Stem 剧名主干（`norm[:marker.start()]`）。
	Stem string
	// Season 规范化的季（"S01"），无季为空串。
	Season string
	// EpisodeNo 集号（可空；用于 `task.episode_no`）。
	EpisodeNo *int64
	// Name 建议的 `task_group.name`（= Stem）。
	Name string
}

// 季/集正则（`design/04 §5.2.2`）。
//
// 只用捕获组，不用前瞻/反向引用 —— 与 Rust `regex` 的能力集对齐，便于两版共享同一套判据。
var (
	// R1：SxxExx / Sxx.Eyy / Sxx-Exx / SxxEPxx
	reSxxExx = regexp.MustCompile(`(?i)\bs(\d{1,2})[\s._-]*e(?:p)?(\d{1,3})\b`)
	// R2：只有季 Sxx
	reSxx = regexp.MustCompile(`(?i)\bs(\d{1,2})\b`)
	// R2c：第 N 季
	reSeasonCN = regexp.MustCompile(`第\s*(\d{1,3})\s*季`)
	// R3：Ep/E 前缀集号
	reEpPrefix = regexp.MustCompile(`(?i)\bep?(\d{1,3})\b`)
	// R4：第 N 集/话/話
	reEpisodeCN = regexp.MustCompile(`第\s*(\d{1,3})\s*[集话話]`)
	// R5：裸集号（前后必须是分隔符或边界）
	reBareEpisode = regexp.MustCompile(`(?:^|[\s._\-\[\(])(\d{1,3})(?:[\s._\-\]\)]|$)`)
	// L1：{tmdbid-12345}
	reTMDBID = regexp.MustCompile(`(?i)\{tmdbid[-_ ]?(\d{1,8})\}`)

	// 归一化词表（`design/02 §2.2.2(4)`）。
	reResolution = regexp.MustCompile(`(?i)\b\d{3,4}p\b`)
	reFrameRate  = regexp.MustCompile(`(?i)\b\d{1,3}(?:\.\d{1,3})?\s?fps\b`)
	// 语言标签：只删「纯语言词」的方括号，不删 `(2026)`、不删 `{tmdbid-...}`
	reLangTag = regexp.MustCompile(`\[[粤国日英韩中双简繁多语音字幕语]{1,6}\]`)
	reCodec   = regexp.MustCompile(`(?i)\bh[.\- ]?26[45]\b|\b\d{1,4}(?:\.\d{1,3})?\s?Mbps\b|\bRealVideo\b|\bCooker\b|\bDTSHD-MA\b`)
	reSpaces  = regexp.MustCompile(`\s+`)
	// 内部分隔符一律归一成单空格。
	//
	// 为什么必须做：同一部剧的不同集可能写成 `Show.Name.S01E01` 与 `Show Name S01E02`。
	// 若保留点号，两者会得到**不同**的 affinity_key，同组同节点立刻失效 ——
	// 而这不是"命名不规范"，是真实存在的混排（不同发布组/不同网盘来源）。
	reRepeat = regexp.MustCompile(`[\s._\-]+`)
)

// NormalizeName 是 `stem` 提取前必须做的归一化。
//
// ⚠️ 仍**不得**删任意 `(...)` / `{...}`：会毁掉 `(2026)` 与 `{tmdbid-…}`。
// 这是设计文档明确点名的坑（`design/02 §2.2.2(4)` 末）。
func NormalizeName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// 1) URL 形态：只取 basename。scheme/host 小写与去默认端口对 stem 无影响，
	//    因为 stem 只来自文件名部分；这里顺手把 query/fragment 也剥掉。
	if looksLikeURL(s) {
		if u, err := url.Parse(s); err == nil {
			s = u.Path
		}
		s = path.Base(strings.TrimRight(s, "/"))
		if decoded, err := url.PathUnescape(s); err == nil {
			s = decoded
		}
	}

	// 2)~4) 分辨率 / 帧率 / 语言标签 / 编码
	s = reResolution.ReplaceAllString(s, " ")
	s = reFrameRate.ReplaceAllString(s, " ")
	s = reLangTag.ReplaceAllString(s, " ")
	s = reCodec.ReplaceAllString(s, " ")

	// 5) 折叠空白
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// looksLikeURL 判断输入是否是一个带 scheme 的地址。
func looksLikeURL(s string) bool {
	for _, p := range []string{"http://", "https://", "ftp://", "ftps://"} {
		if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
			return true
		}
	}
	return false
}

// ExtractTMDBID 从**归一化之前的原始串**抓 `{tmdbid-12345}`。
//
// 必须先于归一化：归一化会折叠分隔符，`{tmdbid 123}` 这类变体会被改写。
func ExtractTMDBID(rawName string) (int64, bool) {
	m := reTMDBID.FindStringSubmatch(rawName)
	if m == nil {
		return 0, false
	}
	var id int64
	for _, c := range m[1] {
		if c < '0' || c > '9' {
			return 0, false
		}
		id = id*10 + int64(c-'0')
	}
	if id <= 0 {
		return 0, false
	}
	return id, true
}

// ExtractSeasonEpisode 在归一化串里定位季集标记。
//
// 返回值：
//   - season：规范化后的 "S01"（无季标记为空串）
//   - markerStart：**季集标记的起始字节位置**，`stem = norm[:markerStart]`
//   - ok：是否找到标记
//
// 判据顺序不可颠倒（`design/04 §5.7`）：SxxExx → Sxx → 第N季 → 集号。
// 先判 SxxExx 是因为 `S01E02` 同时满足 R1 与 R2，必须取信息更全的那个。
func ExtractSeasonEpisode(normName string) (season string, markerStart int, ok bool) {
	if m := reSxxExx.FindStringSubmatchIndex(normName); m != nil {
		if precedesWord(normName, m[0]) {
			n := atoi(m[2], m[3], normName)
			return fmt.Sprintf("S%02d", n), m[0], true
		}
	}
	if m := reSxx.FindStringSubmatchIndex(normName); m != nil {
		if precedesWord(normName, m[0]) {
			n := atoi(m[2], m[3], normName)
			return fmt.Sprintf("S%02d", n), m[0], true
		}
	}
	if m := reSeasonCN.FindStringSubmatchIndex(normName); m != nil {
		n := atoi(m[2], m[3], normName)
		return fmt.Sprintf("S%02d", n), m[0], true
	}
	// 只有集号、没有季 ⇒ 仍返回 ok，但 season 为空；调用方据此落到 L3（不建组）。
	for _, re := range []*regexp.Regexp{reEpPrefix, reEpisodeCN, reBareEpisode} {
		if m := re.FindStringSubmatchIndex(normName); m != nil {
			if re == reEpPrefix && !precedesWord(normName, m[0]) {
				continue
			}
			return "", m[0], true
		}
	}
	return "", 0, false
}

// ExtractEpisodeNo 从归一化串里取集号（无则 nil）。
//
// 与 [ExtractSeasonEpisode] 分开是刻意的：季决定"是否建组"，集只影响 INV-A5
// （组内集号唯一），后者允许为空。把两者揉成一个返回值会诱导调用方在"无季"时
// 也去建组。
func ExtractEpisodeNo(normName string) *int64 {
	if m := reSxxExx.FindStringSubmatchIndex(normName); m != nil {
		n := atoi(m[4], m[5], normName)
		return &n
	}
	for _, re := range []*regexp.Regexp{reEpPrefix, reEpisodeCN} {
		if m := re.FindStringSubmatchIndex(normName); m != nil {
			if re == reEpPrefix && !precedesWord(normName, m[0]) {
				continue
			}
			n := atoi(m[2], m[3], normName)
			return &n
		}
	}
	if m := reBareEpisode.FindStringSubmatchIndex(normName); m != nil {
		// 裸集号可能落在季标记**内部**（`S03` 的 `03`）——那是季不是集，必须跳过。
		// 不做这一步，`Show.S03.Complete` 会得到 episode_no=3。
		if seasonStart, seasonEnd := seasonSpan(normName); seasonStart >= 0 &&
			seasonStart <= m[0] && m[1] <= seasonEnd {
			return nil
		}
		n := atoi(m[2], m[3], normName)
		return &n
	}
	return nil
}

// seasonSpan 返回季标记的字节区间（无季标记时 start<0）。
func seasonSpan(normName string) (start, end int) {
	for _, re := range []*regexp.Regexp{reSxx, reSeasonCN} {
		if m := re.FindStringSubmatchIndex(normName); m != nil {
			if re == reSxx && !precedesWord(normName, m[0]) {
				continue
			}
			return m[0], m[1]
		}
	}
	return -1, -1
}

// precedesWord 拒绝"字母后紧跟的 sN"（如 `DTS5.1` 里的 `S5`）。
//
// 这是 `\bsN\b` 的**必要补丁**：`\b` 只保证"不是字母数字边界"，而 `DTS5`
// 的 `S` 前面是字母 `T`——正则的 `\bs` 在这里**仍然匹配**（因为 `\b` 判定的是
// T→S 之间没有边界… 实际上 `\b` 要求前一个字符是非单词字符，故 T-S 之间无边界，
// 不会匹配）。真正会误伤的是 `x264` 之后的 `s`、以及 `H264.S01` 这类正常情形。
// 这里保留显式检查，是因为**放宽正则时**（比如允许 `s` 前有 `[`）最容易退化，
// 让"名字里带 S5 的片子"被当成第 5 季，而错绑比不分组贵得多。
func precedesWord(s string, idx int) bool {
	if idx <= 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:idx])
	return !unicode.IsLetter(r)
}

// atoi 用字节区间解析十进制（避免 strconv 的额外错误分支）。
func atoi(start, end int, s string) int64 {
	var n int64
	for _, c := range s[start:end] {
		if c < '0' || c > '9' {
			continue
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// DeriveKey 是 §1.3 判据链的唯一入口。
//
// 顺序（不得颠倒）：
//
//  1. 上游显式组键？        → L0（1.00），跳过全部推断
//  2. 原始名命中 {tmdbid}？  → L1（0.97）
//  3. head-before-season 得 stem 且 season 非空且过闸门 → L2（0.95 / 0.88）
//  4. 否则                  → L3：不建组（安全侧失败）
//
// 第 4 条是**刻意的**：`design/04 §5.3` 宁可退化为普通调度，也不要把两部剧错绑到一台。
func DeriveKey(rawName, sourceKey string, opts KeyOptions) KeyResult {
	opts = withDefaults(opts)

	// ── L0：上游显式组键 ────────────────────────────────────────────────
	if k := strings.TrimSpace(opts.ExplicitKey); k != "" {
		return KeyResult{Key: k, Layer: "L0", Confidence: 1.0, Name: k}
	}

	norm := NormalizeName(rawName)
	if norm == "" {
		return KeyResult{Layer: "L3"}
	}

	// ── L1：{tmdbid-XXXXX} ─────────────────────────────────────────────
	if opts.TMDBPriority {
		if id, ok := ExtractTMDBID(rawName); ok {
			if season, markerStart, found := ExtractSeasonEpisode(norm); found && season != "" {
				stem := trimStem(norm[:markerStart])
				if gateOK(stem, 0.97, opts) {
					return KeyResult{
						Key:        fmt.Sprintf("tmdb:%d|%s", id, season),
						Layer:      "L1",
						Confidence: 0.97,
						Stem:       stem,
						Season:     season,
						EpisodeNo:  ExtractEpisodeNo(norm),
						Name:       stem,
					}
				}
			}
		}
	}

	// ── L2：head-before-season ─────────────────────────────────────────
	season, markerStart, found := ExtractSeasonEpisode(norm)
	if !found || season == "" {
		// 退化命名：不建组。
		//
		// ⚠️ **不带** `EpisodeNo`：没有季的"集号"极不可靠（`DTS5.1` 的 `1` 会被
		// bare-episode 正则吃掉），而它对不建组的任务没有任何用途
		// （`INV-A5` 的组内集号唯一只在 group_id 非空时生效）。
		// 宁可空着，也不要往库里塞一个假的集号。
		return KeyResult{Layer: "L3"}
	}

	// ★stem 口径：`norm[:marker.start()]`，**不是**"去掉标记后的剩余全部文本"。
	// 后者会把 "S01E02.2160p.WEB-DL" 的尾巴也当成剧名（F19 实测逼出的坑）。
	stem := trimStem(norm[:markerStart])
	conf := 0.88 // 只有季、没有集：仍可建组，但置信度低一档
	if ExtractEpisodeNo(norm) != nil {
		conf = 0.95
	}
	if !gateOK(stem, conf, opts) {
		return KeyResult{Layer: "L3", Confidence: conf, Stem: stem, Season: season}
	}

	key := "name:" + stem + "|" + season
	if opts.IncludeSource && strings.TrimSpace(sourceKey) != "" {
		key = "src:" + strings.TrimSpace(sourceKey) + "|" + key
	}
	return KeyResult{
		Key:        key,
		Layer:      "L2",
		Confidence: conf,
		Stem:       stem,
		Season:     season,
		EpisodeNo:  ExtractEpisodeNo(norm),
		Name:       stem,
	}
}

// trimStem 去掉首尾分隔符并折叠空白。
func trimStem(s string) string {
	s = reRepeat.ReplaceAllString(s, " ")
	s = strings.Trim(s, " ._-[](){}【】")
	return strings.TrimSpace(s)
}

// gateOK 是闸门：`conf ≥ CONF_MIN` 且 `stem` 够长（含 CJK 可放宽到 2）。
func gateOK(stem string, conf float64, opts KeyOptions) bool {
	if conf < opts.ConfMin {
		return false
	}
	n := utf8.RuneCountInString(stem)
	if n >= opts.MinStem {
		return true
	}
	return n >= 2 && containsCJK(stem)
}

// containsCJK 报告字符串里是否含中日韩统一表意文字。
func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// withDefaults 把零值字段补成设计默认值（调用方只关心想改的那几项）。
func withDefaults(o KeyOptions) KeyOptions {
	d := DefaultKeyOptions()
	if o.MinStem <= 0 {
		o.MinStem = d.MinStem
	}
	if o.ConfMin <= 0 {
		o.ConfMin = d.ConfMin
	}
	// TMDBPriority 的零值是 false，无法区分"没设"与"显式关"。
	// 这是刻意的取舍：调用方必须显式用 DefaultKeyOptions() 再改，
	// 而不是传一个空结构体然后指望 tmdb 生效。
	return o
}
