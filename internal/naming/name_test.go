package naming

import (
	"strings"
	"testing"
)

// TestBuildCanonicalForm 守补名格式 `<剧名>.S01E01.<原名>`（可逆，纪律 3）。
func TestBuildCanonicalForm(t *testing.T) {
	got := Build(Input{
		Stem: "某剧", Season: "S01", Episode: 1, OriginName: "01.ts",
	})
	if got.Filename != "某剧.S01E01.01.ts" {
		t.Fatalf("补名 = %q，期望 某剧.S01E01.01.ts", got.Filename)
	}
	if got.Origin != "01.ts" {
		t.Fatalf("Origin = %q，审计需要原始名", got.Origin)
	}
	// 可逆：从补后的名字里能反推原始名
	if !strings.HasSuffix(got.Filename, "."+got.Origin) {
		t.Fatalf("%q 不含原始名 %q —— 补名不可逆，排障时无法反推", got.Filename, got.Origin)
	}
}

// TestBuildSilentFallbacks 守"拿不到剧名就不补"（纪律 1、2）。
//
// 反证：把这些情形改成一律硬拼 ⇒ 文件名会被垃圾前缀污染，
// 而后续的分组识别读的正是文件名 ⇒ 批量错绑。
func TestBuildSilentFallbacks(t *testing.T) {
	cases := []struct {
		name string
		in   Input
	}{
		{"无剧名", Input{Season: "S01", Episode: 1, OriginName: "01.ts"}},
		{"剧名纯数字", Input{Stem: "12345", OriginName: "01.ts", Episode: 1}},
		{"剧名单字符", Input{Stem: "A", OriginName: "01.ts", Episode: 1}},
		{"剧名像哈希", Input{Stem: "9f86d081884c7d65", OriginName: "01.ts", Episode: 1}},
		{"原名已规范", Input{Stem: "某剧", Season: "S01", Episode: 1, OriginName: "某剧.S01E01.01.ts"}},
	}
	for _, c := range cases {
		if got := Build(c.in); got.Filename != "" {
			t.Errorf("%s：应静默不补，实际补成 %q", c.name, got.Filename)
		}
	}
}

// TestBuildNeverCarriesPathSeparator 守纪律 4：不得借 filename 改落盘位置。
//
// 反证：去掉 sanitize 里的 '/' 替换 ⇒ 节点会把 filename 当相对路径 ⇒
// 等于偷偷建目录（Q3 明确不做，且任意路径会被节点拒绝 save_folder invalid）。
func TestBuildNeverCarriesPathSeparator(t *testing.T) {
	got := Build(Input{
		Stem: "某/剧", Season: "S01", Episode: 3, OriginName: "../../etc/passwd.ts",
	})
	if got.Filename == "" {
		t.Fatal("这组输入应补名（剧名可辨识）")
	}
	if strings.ContainsAny(got.Filename, `/\`) {
		t.Fatalf("补名 %q 含路径分隔符 —— 会改变落盘位置", got.Filename)
	}
	// `..` 只有在**作为路径段**时才危险；没有分隔符时它只是一个普通字符序列。
	// 这里断言"任何一段都不是 .."，即使用户真的去拼路径也穿不出去。
	for _, seg := range strings.Split(got.Filename, "/") {
		if strings.TrimSpace(seg) == ".." {
			t.Fatalf("补名 %q 含 `..` 路径段", got.Filename)
		}
	}
}

// TestBuildNoSeasonOrEpisode 守"缺季/集时退化成可用的一半"。
func TestBuildNoSeasonOrEpisode(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"有季无集", Input{Stem: "某剧", Season: "S02", OriginName: "x.ts"}, "某剧.S02.x.ts"},
		{"有集无季", Input{Stem: "某剧", Episode: 7, OriginName: "x.ts"}, "某剧.E07.x.ts"},
		{"都无", Input{Stem: "某剧", OriginName: "x.ts"}, "某剧.x.ts"},
		{"三位集号", Input{Stem: "某剧", Season: "S01", Episode: 123, OriginName: "x.ts"}, "某剧.S01E123.x.ts"},
	}
	for _, c := range cases {
		if got := Build(c.in); got.Filename != c.want {
			t.Errorf("%s：补名 = %q，期望 %q", c.name, got.Filename, c.want)
		}
	}
}

// TestBuildOriginFromURL 守"上游没给名字时用 URL basename 兜底"。
func TestBuildOriginFromURL(t *testing.T) {
	got := Build(Input{
		Stem: "某剧", Season: "S01", Episode: 2,
		URL: "https://pan.example.com/d/abc/02.ts?sign=xyz",
	})
	if got.Filename != "某剧.S01E02.02.ts" {
		t.Fatalf("补名 = %q，期望用 URL basename 得 某剧.S01E02.02.ts", got.Filename)
	}
}

// TestSameStemSameStructureAcrossNodes 是 B3-AC4 的纯函数形态判据。
//
// 判据：**同一组**的两集，补出来的文件名结构必须一致（只差集号与原名）。
// 这条替代了"save_folder 相对结构一致"——v2 起 `save_folder` 一律不下发（Q3），
// 所以"跨节点结构一致"只能靠 filename 来体现。
func TestSameStemSameStructureAcrossNodes(t *testing.T) {
	a := Build(Input{Stem: "某剧", Season: "S01", Episode: 1, OriginName: "01.ts"})
	b := Build(Input{Stem: "某剧", Season: "S01", Episode: 2, OriginName: "02.ts"})
	if a.Filename == "" || b.Filename == "" {
		t.Fatalf("同组两集都应补名：%q / %q", a.Filename, b.Filename)
	}
	pa := strings.Split(a.Filename, ".")
	pb := strings.Split(b.Filename, ".")
	if len(pa) < 3 || len(pb) < 3 {
		t.Fatalf("结构不足三段：%q / %q", a.Filename, b.Filename)
	}
	if pa[0] != pb[0] {
		t.Fatalf("同组剧名段不一致：%q vs %q", a.Filename, b.Filename)
	}
	// 季段必须相同（`S01E01` 与 `S01E02` 的前三位）
	if len(pa[1]) < 3 || len(pb[1]) < 3 || pa[1][:3] != pb[1][:3] {
		t.Fatalf("同组季段不一致：%q vs %q", a.Filename, b.Filename)
	}
	if pa[1] == pb[1] {
		t.Fatalf("两集集号相同，测不出结构：%q vs %q", a.Filename, b.Filename)
	}
}
