package dedup

import (
	"encoding/base32"
	"encoding/hex"
	"strings"
	"testing"
)

// TestNormalizeURLRules 逐条守 `design/02 §2.2.3` 的六条归一化规则。
//
// 每条都用"两个只差一处、结果必须相同"的形式：只要某条规则失效，
// 两者就会归一化成不同串 ⇒ 红。
func TestNormalizeURLRules(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"scheme 大小写", "HTTP://Host/x.ts", "http://Host/x.ts"},
		{"host 大小写", "http://HOST/x.ts", "http://host/x.ts"},
		{"默认端口 http", "http://host:80/x.ts", "http://host/x.ts"},
		{"默认端口 https", "https://host:443/x.ts", "https://host/x.ts"},
		{"fragment", "http://host/x.ts#t=10", "http://host/x.ts"},
		{"query 顺序", "http://host/x.ts?b=2&a=1", "http://host/x.ts?a=1&b=2"},
		{"跟踪参数", "http://host/x.ts?utm_source=a&spm=b", "http://host/x.ts"},
		{"网盘签名", "http://host/x.ts?auth_key=abc&token=t", "http://host/x.ts"},
		{"unreserved 解码", "http://host/%7Euser/x.ts", "http://host/~user/x.ts"},
		{"折叠斜杠", "http://host//a///x.ts", "http://host/a/x.ts"},
		{"末尾斜杠", "http://host/a/", "http://host/a"},
	}
	for _, c := range cases {
		got1, got2 := NormalizeURL(c.a), NormalizeURL(c.b)
		if got1 == "" {
			t.Errorf("%s: 归一化返回空串（输入 %q）", c.name, c.a)
			continue
		}
		if got1 != got2 {
			t.Errorf("%s: %q → %q，%q → %q，期望相同", c.name, c.a, got1, c.b, got2)
		}
	}
}

// TestNormalizeURLRefusesNonHTTP 守住"只接受 http/https/ftp"。
//
// 返回原始串比返回空串更危险：它会与其他层级的键重复，产生一条低置信噪音。
func TestNormalizeURLRefusesNonHTTP(t *testing.T) {
	for _, raw := range []string{
		"magnet:?xt=urn:btih:" + strings.Repeat("a", 40),
		"file:///etc/passwd",
		"not-a-url",
		"",
	} {
		if got := NormalizeURL(raw); got != "" {
			t.Errorf("%q 应返回空串（不支持的 scheme），实得 %q", raw, got)
		}
	}
}

// TestTaskGUIDMatchesNodeAlgorithm 钉住"与节点算法对齐"。
//
// 判据是**具体值**而不是"非空"：节点的 `task_guid` 就是 `md_ + sha1(url)`，
// 这里算出来的必须逐字符相同，否则代理与节点会判重错位（A-10 的全部理由）。
func TestTaskGUIDMatchesNodeAlgorithm(t *testing.T) {
	// echo -n "http://example.com/a.ts" | sha1sum
	// 41d7f2a4b4f2b0a4f0f2b3f9a0d8a1c2e3f4a5b6 之外，这里用可复核的已知值：
	// sha1("abc") = a9993e364706816aba3e25717850c26c9cd0d89d
	got := TaskGUID2("abc")
	if got != "md_a9993e364706816aba3e25717850c26c9cd0d89d" {
		t.Fatalf("TaskGUID(abc) = %q，与节点算法 md_+sha1 不一致", got)
	}
}

// TaskGUID2 只是为了在测试里表达"输入任意串"，避免与生产签名混淆。
func TaskGUID2(s string) string { return TaskGUID(s) }

// TestInfohashBothEncodings 守 L1：40 位 hex 与 32 位 base32 都要认。
func TestInfohashBothEncodings(t *testing.T) {
	hexHash := strings.Repeat("ab", 20) // 40 hex
	magnetHex := "magnet:?xt=urn:btih:" + hexHash + "&dn=x"
	got := Keys(Input{Kind: "magnet", URLRaw: magnetHex})
	if len(got) == 0 || got[0].Rule != "L1" {
		t.Fatalf("磁力链应产出 L1 键，实得 %v", got)
	}
	if got[0].Value != "bt:"+hexHash {
		t.Fatalf("L1 键 = %q，期望 bt:%s", got[0].Value, hexHash)
	}
	if !got[0].Skippable() {
		t.Fatal("L1 是精确级，必须允许跳过")
	}

	// base32 形式（32 位）解码后应与 hex 形式得到**同一个**键。
	// 找一个 hex 对应的 base32：用 go 的标准库反算，避免手写出错。
	b32 := base32Of(hexHash)
	if b32 == "" {
		t.Skip("base32 反算失败，跳过该子用例")
	}
	got2 := Keys(Input{Kind: "magnet", URLRaw: "magnet:?xt=urn:btih:" + b32})
	if len(got2) == 0 || got2[0].Value != "bt:"+hexHash {
		t.Fatalf("base32 磁力链应得到同一 L1 键 %q，实得 %v", "bt:"+hexHash, got2)
	}
}

// TestKeysSkipUnknownSizeLayers 守"缺字段就跳过该层级，不用 0 硬凑"。
//
// 用 0 凑 L3 的后果是把"大小未知的不同文件"全判成疑似重复 —— 那会让整批任务
// 卡在待裁决，比不去重糟得多。
func TestKeysSkipUnknownSizeLayers(t *testing.T) {
	keys := Keys(Input{Kind: "http", URLRaw: "http://h/a.ts", Name: "A.S01E01.ts"})
	for _, k := range keys {
		if k.Rule == "L2c" || k.Rule == "L3" {
			t.Fatalf("大小未知时不应产出 %s（键=%s）", k.Rule, k.Value)
		}
	}
	// 有大小之后才出现
	keys2 := Keys(Input{Kind: "http", URLRaw: "http://h/a.ts", Name: "A.S01E01.ts", TotalSize: 100, Referrer: "pan"})
	var hasL2c, hasL3 bool
	for _, k := range keys2 {
		switch k.Rule {
		case "L2c":
			hasL2c = true
		case "L3":
			hasL3 = true
		}
	}
	if !hasL2c || !hasL3 {
		t.Fatalf("大小与来源齐备时应产出 L2c 与 L3，实得 %v", keys2)
	}
}

// TestMediumConfidenceNeverSkippable 是 B3-AC3 的**纯函数**形态判据。
//
// 反证：把 `Skippable()` 改成恒 true ⇒ 这条红 ⇒ 中置信会被"静默跳过"，
// 用户永远不知道有一个任务被丢弃（漏任务不可挽回）。
func TestMediumConfidenceNeverSkippable(t *testing.T) {
	keys := Keys(Input{
		Kind: "http", URLRaw: "http://h/a.ts?utm_source=x",
		Name: "A.ts", TotalSize: 100, Referrer: "pan",
	})
	sawMedium := false
	for _, k := range keys {
		if k.Conf == ConfMedium {
			sawMedium = true
			if k.Skippable() {
				t.Fatalf("%s（medium）被判成可跳过 —— 中置信只能标记", k.Rule)
			}
		}
	}
	if !sawMedium {
		t.Fatal("这组输入应产出至少一条 medium 键")
	}
}

// base32Of 把 40 位 hex 转成 32 位 base32（磁力链的另一种合法写法）。
func base32Of(hexStr string) string {
	b, err := hex.DecodeString(hexStr)
	if err != nil || len(b) != 20 {
		return ""
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}
