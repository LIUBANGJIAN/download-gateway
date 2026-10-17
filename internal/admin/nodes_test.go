package admin

import (
	"math"
	"strings"
	"testing"
)

// 限速换算必须与 Rust 侧逐字一致（`nodes.rs` 的 bytes_to_kbps / kbps_to_bytes）。
func TestNodeRateConversion(t *testing.T) {
	if got := kbpsToBytes(0); got != 0 {
		t.Fatalf("0 表示不限速，实际 %d", got)
	}
	if got := kbpsToBytes(1024); got != 1024*1024 {
		t.Fatalf("1024 KB/s 应为 1 MiB/s，实际 %d", got)
	}
	if got := bytesToKbps(0); got != 0 {
		t.Fatalf("0 字节应回 0 KB/s，实际 %d", got)
	}
	if got := bytesToKbps(1024 * 1024); got != 1024 {
		t.Fatalf("1 MiB/s 应为 1024 KB/s，实际 %d", got)
	}
	// 非 1024 整数倍向下取整。
	if got := bytesToKbps(1500); got != 1 {
		t.Fatalf("1500 B/s 应向下取整为 1 KB/s，实际 %d", got)
	}
	// 负值一律当 0（防脏数据把界面显示成负数）。
	if got := bytesToKbps(-5); got != 0 {
		t.Fatalf("负字节应回 0，实际 %d", got)
	}
	if got := kbpsToBytes(-5); got != 0 {
		t.Fatalf("负 KB/s 应回 0，实际 %d", got)
	}
}

func TestValidateBaseURL(t *testing.T) {
	if _, err := validateBaseURL("http://203.0.113.10:9085"); err != nil {
		t.Fatalf("合法地址被拒：%v", err)
	}
	got, err := validateBaseURL("https://198.51.100.7:9085/")
	if err != nil {
		t.Fatalf("合法地址被拒：%v", err)
	}
	// 尾斜杠必须被规范掉，否则拼探测 URL 会出现 `//`。
	if got != "https://198.51.100.7:9085" {
		t.Fatalf("尾斜杠未去掉：%q", got)
	}
	for _, bad := range []string{"", "203.0.113.10:9085", "ftp://x", "http://a b"} {
		if _, err := validateBaseURL(bad); err == nil {
			t.Fatalf("%q 应被拒", bad)
		}
	}
	if _, err := validateBaseURL("203.0.113.10:9085"); err == nil {
		t.Fatal("缺方案应被拒")
	}
}

func TestValidateAlias(t *testing.T) {
	got, err := validateAlias("  客厅NAS  ")
	if err != nil {
		t.Fatalf("合法别名被拒：%v", err)
	}
	if got != "客厅NAS" {
		t.Fatalf("应去除首尾空白，实际 %q", got)
	}
	if _, err := validateAlias(""); err == nil {
		t.Fatal("空别名应被拒")
	}
	if _, err := validateAlias("   "); err == nil {
		t.Fatal("纯空白应被拒")
	}
	if _, err := validateAlias(strings.Repeat("x", aliasMax+1)); err == nil {
		t.Fatal("超长别名应被拒")
	}
	if _, err := validateAlias(strings.Repeat("x", aliasMax)); err != nil {
		t.Fatalf("边界长度应允许：%v", err)
	}
	// 长度按**字符**计：64 个汉字应当合法（按字节会是 192）。
	if _, err := validateAlias(strings.Repeat("节", aliasMax)); err != nil {
		t.Fatalf("64 个汉字应合法（按字符计数）：%v", err)
	}
}

func TestValidateRole(t *testing.T) {
	if got, err := validateRole("SERIES"); err != nil || got != "series" {
		t.Fatalf("应大小写不敏感并归一为小写，实际 %q / %v", got, err)
	}
	if got, err := validateRole(" generic "); err != nil || got != "generic" {
		t.Fatalf("应去除首尾空白，实际 %q / %v", got, err)
	}
	for _, bad := range []string{"nas", ""} {
		if _, err := validateRole(bad); err == nil {
			t.Fatalf("%q 应被拒", bad)
		}
	}
}

func TestValidateNodeNumbers(t *testing.T) {
	if err := validateNodeNumbers(1.0, 3, 0); err != nil {
		t.Fatalf("合法数值被拒：%v", err)
	}
	if err := validateNodeNumbers(0.0, 1, 100); err != nil {
		t.Fatalf("边界数值应允许：%v", err)
	}
	cases := []struct {
		name          string
		weight        float64
		maxConcurrent int64
		kbps          int64
	}{
		{"负权重", -1.0, 3, 0},
		{"NaN 权重", math.NaN(), 3, 0},
		{"并发 0", 1.0, 0, 0},
		{"并发超上界", 1.0, maxConcurrentCap + 1, 0},
		{"负限速", 1.0, 3, -1},
	}
	for _, c := range cases {
		if err := validateNodeNumbers(c.weight, c.maxConcurrent, c.kbps); err == nil {
			t.Fatalf("%s 应被拒", c.name)
		}
	}
}

// view 必须**永不**带出密文；探测结果要能覆盖库内状态。
func TestNodeViewNeverLeaksCiphertext(t *testing.T) {
	row := nodeRow{
		nodeID:       1,
		alias:        "客厅NAS",
		baseURL:      "http://203.0.113.10:9085",
		user:         "admin",
		passEnc:      "SUPER-SECRET-CIPHERTEXT",
		weight:       1.0,
		maxRateBytes: 2048,
		role:         "generic",
		state:        "unknown",
	}
	v := row.toView(nil)
	if v.PasswordSet != true {
		t.Fatal("password_set 应为 true")
	}
	if v.MaxRateKbps != 2 {
		t.Fatalf("2048 B/s 应折算成 2 KB/s，实际 %d", v.MaxRateKbps)
	}
	if v.State != "unknown" || v.Online {
		t.Fatalf("无探测结果时应回库内历史状态，实际 %q/%v", v.State, v.Online)
	}
	if v.ProbeError != nil || v.HTTPCode != nil {
		t.Fatal("无探测结果时 probe_error / http_code 应为 null")
	}

	code := 401
	p := probeResult{online: true, httpCode: &code, latencyMs: 12}
	v2 := row.toView(&p)
	if v2.State != "online" || !v2.Online {
		t.Fatalf("有探测结果时以探测为准，实际 %q/%v", v2.State, v2.Online)
	}
	if v2.HTTPCode == nil || *v2.HTTPCode != 401 {
		t.Fatal("BitComet 未认证返 401 也算在线")
	}

	off := probeResult{online: false, latencyMs: 1500, errMsg: "超时（>1500ms）"}
	v3 := row.toView(&off)
	if v3.State != "offline" || v3.Online {
		t.Fatalf("离线应为 offline/false，实际 %q/%v", v3.State, v3.Online)
	}
	if v3.ProbeError == nil {
		t.Fatal("离线时应带 probe_error")
	}
	if v3.HTTPCode != nil {
		t.Fatal("离线时 http_code 应为 null")
	}
}
