package grouping

import "testing"

// TestDeriveKeyTable 是分组识别的行为表。
//
// 每条断言都落在**可观测结果**上（Key/Layer/Stem/Season），不查中间变量——
// 否则改实现时测试会跟着改，等于没守。
func TestDeriveKeyTable(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantKey    string
		wantLayer  string
		wantStem   string
		wantSeason string
		wantEp     int64 // -1 表示期望 nil
		wantConf   float64
	}{
		{
			name:       "SxxExx 标准",
			raw:        "Show.Name.S01E02.1080p.WEB-DL.x264-GROUP",
			wantKey:    "name:Show Name|S01",
			wantLayer:  "L2",
			wantStem:   "Show Name",
			wantSeason: "S01",
			wantEp:     2,
			wantConf:   0.95,
		},
		{
			name:       "中文剧名 + 分辨率",
			raw:        "西部世界.S02E05.2160p.WEB-DL",
			wantKey:    "name:西部世界|S02",
			wantLayer:  "L2",
			wantStem:   "西部世界",
			wantSeason: "S02",
			wantEp:     5,
			wantConf:   0.95,
		},
		{
			// L1 优先：tmdb 命中时键形态不同（"tmdb:" 前缀），且置信度更高。
			name:       "tmdbid 优先",
			raw:        "Some.Show.S01E01.{tmdbid-12345}.1080p",
			wantKey:    "tmdb:12345|S01",
			wantLayer:  "L1",
			wantStem:   "Some Show",
			wantSeason: "S01",
			wantEp:     1,
			wantConf:   0.97,
		},
		{
			name:       "只有季（无集号）",
			raw:        "Show.Name.S03.Complete.1080p",
			wantKey:    "name:Show Name|S03",
			wantLayer:  "L2",
			wantStem:   "Show Name",
			wantSeason: "S03",
			wantEp:     -1,
			wantConf:   0.88,
		},
		{
			name:       "第N季中文",
			raw:        "某剧 第2季 第07集",
			wantKey:    "name:某剧|S02",
			wantLayer:  "L2",
			wantStem:   "某剧",
			wantSeason: "S02",
			wantEp:     7,
			wantConf:   0.95,
		},
		{
			// 退化命名：识别不出季 ⇒ 落 L3、不建组（安全侧失败）
			name:      "退化命名 01.ts",
			raw:       "01.ts",
			wantKey:   "",
			wantLayer: "L3",
			wantEp:    -1,
		},
		{
			name:      "电影（无季）不建组",
			raw:       "Some.Movie.2026.1080p.BluRay.x264",
			wantKey:   "",
			wantLayer: "L3",
			wantEp:    -1,
		},
		{
			// stem 为空 ⇒ 过不了 MIN_STEM 闸门
			name:       "裸 S01E01",
			raw:        "S01E01.1080p",
			wantKey:    "",
			wantLayer:  "L3",
			wantSeason: "S01",
			wantEp:     -1,
		},
		{
			// 「DTS5.1」里的 S5 不得被当成第 5 季：错绑比不分组贵得多
			name:      "DTS5.1 不得误判为季",
			raw:       "Some.Movie.DTS5.1.1080p",
			wantKey:   "",
			wantLayer: "L3",
			wantEp:    -1,
		},
		{
			// URL 形态：只取 basename，query/fragment 不参与识别
			name:       "URL 取 basename",
			raw:        "https://example.com/files/Show.Name.S04E08.1080p.mkv?token=abc#frag",
			wantKey:    "name:Show Name|S04",
			wantLayer:  "L2",
			wantStem:   "Show Name",
			wantSeason: "S04",
			wantEp:     8,
			wantConf:   0.95,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveKey(c.raw, "", DefaultKeyOptions())
			if got.Key != c.wantKey {
				t.Errorf("Key = %q，期望 %q", got.Key, c.wantKey)
			}
			if got.Layer != c.wantLayer {
				t.Errorf("Layer = %q，期望 %q", got.Layer, c.wantLayer)
			}
			if got.Stem != c.wantStem {
				t.Errorf("Stem = %q，期望 %q", got.Stem, c.wantStem)
			}
			if got.Season != c.wantSeason {
				t.Errorf("Season = %q，期望 %q", got.Season, c.wantSeason)
			}
			if c.wantEp >= 0 {
				if got.EpisodeNo == nil {
					t.Errorf("EpisodeNo = nil，期望 %d", c.wantEp)
				} else if *got.EpisodeNo != c.wantEp {
					t.Errorf("EpisodeNo = %d，期望 %d", *got.EpisodeNo, c.wantEp)
				}
			}
			if c.wantKey != "" && got.Confidence != c.wantConf {
				t.Errorf("Confidence = %v，期望 %v", got.Confidence, c.wantConf)
			}
		})
	}
}

// TestSameSeasonDifferentEpisodesShareKey 是「同组同节点」的真正前提：
// 同一季的不同集必须得到**逐字节相同**的 affinity_key。
//
// 如果这里漏掉（例如把集号写进键），B1-AC4 会变成"永远为真"的空断言 ——
// 因为每个任务都会自成一组的"唯一成员"。
func TestSameSeasonDifferentEpisodesShareKey(t *testing.T) {
	names := []string{
		"Show.Name.S01E01.1080p.WEB-DL",
		"Show.Name.S01E02.1080p.WEB-DL",
		"Show.Name.S01.E03.2160p",
		"Show.Name.S01EP04.720p",
	}
	opts := DefaultKeyOptions()
	first := DeriveKey(names[0], "", opts).Key
	if first == "" {
		t.Fatal("首条样本未建组，后续比较无意义")
	}
	for _, n := range names[1:] {
		if got := DeriveKey(n, "", opts).Key; got != first {
			t.Errorf("%q 的键 = %q，期望与首条一致 %q", n, got, first)
		}
	}
}

// TestDifferentSeasonsGetDifferentKeys 守反方向：
// 不同季**必须**分开 —— 否则 S01 和 S02 会被硬绑到同一台机器上。
func TestDifferentSeasonsGetDifferentKeys(t *testing.T) {
	opts := DefaultKeyOptions()
	a := DeriveKey("Show.Name.S01E01.1080p", "", opts).Key
	b := DeriveKey("Show.Name.S02E01.1080p", "", opts).Key
	if a == "" || b == "" {
		t.Fatalf("样本未建组：a=%q b=%q", a, b)
	}
	if a == b {
		t.Fatalf("不同季得到了同一个键：%q", a)
	}
}

// TestNormalizeNameKeepsCriticalTokens 守归一化的两个"不许删"：
// `(2026)` 年份与 `{tmdbid-...}` 必须留下，否则 L1 永远命中不了。
func TestNormalizeNameKeepsCriticalTokens(t *testing.T) {
	got := NormalizeName("Show.(2026).{tmdbid-99}.S01E01.1080p.H.264.[国语]")
	for _, want := range []string{"(2026)", "{tmdbid-99}", "S01E01"} {
		if !contains(got, want) {
			t.Errorf("归一化后丢了 %q：%q", want, got)
		}
	}
	for _, unwanted := range []string{"1080p", "H.264", "国语"} {
		if contains(got, unwanted) {
			t.Errorf("归一化后仍含 %q：%q", unwanted, got)
		}
	}
}

// TestIncludeSourcePrefix 覆盖 `group.key_include_source=on` 的形态。
func TestIncludeSourcePrefix(t *testing.T) {
	opts := DefaultKeyOptions()
	opts.IncludeSource = true
	got := DeriveKey("Show.Name.S01E01.1080p", "115", opts)
	if want := "src:115|name:Show Name|S01"; got.Key != want {
		t.Fatalf("Key = %q，期望 %q", got.Key, want)
	}
	// 默认 off 时不得带 src: 前缀（种子配置就是 off）
	off := DeriveKey("Show.Name.S01E01.1080p", "115", DefaultKeyOptions())
	if want := "name:Show Name|S01"; off.Key != want {
		t.Fatalf("默认配置 Key = %q，期望 %q", off.Key, want)
	}
}

// TestConfidenceGate 守闸门：置信度低于 CONF_MIN 一律不建组。
func TestConfidenceGate(t *testing.T) {
	opts := DefaultKeyOptions()
	opts.ConfMin = 0.90 // 高于 0.88（只有季）但不超过 0.95
	if got := DeriveKey("Show.Name.S03.Complete", "", opts); got.Key != "" {
		t.Fatalf("0.88 < CONF_MIN(0.90) 时不应建组，实得 %q", got.Key)
	}
	if got := DeriveKey("Show.Name.S03E01", "", opts); got.Key == "" {
		t.Fatal("0.95 ≥ CONF_MIN(0.90) 应建组，实得空键")
	}
}

// TestMinStemGate 守主干长度闸门（含 CJK 放宽到 2）。
func TestMinStemGate(t *testing.T) {
	opts := DefaultKeyOptions()
	if got := DeriveKey("AB.S01E01", "", opts); got.Key != "" {
		t.Errorf("两个 ASCII 字符的 stem 应被闸门拒绝，实得 %q", got.Key)
	}
	if got := DeriveKey("风骚.S01E01", "", opts); got.Key == "" {
		t.Error("两个 CJK 字符应通过闸门（design/04 §17 项 19）")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
