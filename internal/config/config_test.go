package config

import (
	"os"
	"testing"
)

// ---------------------------------------------------------------------------
// 与 Rust `src/config.rs::tests` 逐条对照
// ---------------------------------------------------------------------------

// 对照 `env_or_falls_back_when_unset`。
//
// ★ 不再用「若已被设置则 Skip」——**静默跳过＝假绿**。这里显式**移除**该键（并在结束恢复），
// 保证「未设置」这条分支在任何宿主环境下都真被执行。
func TestEnvOrFallsBackWhenUnset(t *testing.T) {
	const key = "DISPATCH_DEFINITELY_UNSET_KEY_XYZ"
	mustUnset(t, key)
	if got := envOr(key, "dflt"); got != "dflt" {
		t.Fatalf("未设置应回落默认值，实际 %q", got)
	}
}

// 对照 `env_or_treats_blank_as_unset`。
func TestEnvOrTreatsBlankAsUnset(t *testing.T) {
	t.Setenv("DISPATCH_BLANK_KEY_XYZ", "   ")
	if got := envOr("DISPATCH_BLANK_KEY_XYZ", "dflt"); got != "dflt" {
		t.Fatalf("全空白应视为未设置、回落默认值，实际 %q", got)
	}
}

// 对照 `env_or_reads_real_value`。
func TestEnvOrReadsRealValue(t *testing.T) {
	t.Setenv("DISPATCH_REAL_KEY_XYZ", "v1")
	if got := envOr("DISPATCH_REAL_KEY_XYZ", "dflt"); got != "v1" {
		t.Fatalf("应读到真实值，实际 %q", got)
	}
}

// 补充：Rust `env_or` 返回的是**未 trim 的原值**。这条钉住「读值后不做规范化」这一契约，
// 否则下游（如地址串、路径）会在无察觉下被改写。
func TestEnvOrReturnsUntrimmedValue(t *testing.T) {
	t.Setenv("DISPATCH_SPACEY_KEY_XYZ", " v2 ")
	if got := envOr("DISPATCH_SPACEY_KEY_XYZ", "dflt"); got != " v2 " {
		t.Fatalf("应原样返回（不 trim），实际 %q", got)
	}
}

// 对照 `defaults_match_design_ports`（四个默认值）。
//
// ★ 取消 Skip：静默跳过＝假绿。这里用 `t.Setenv(键,"")` 把四个键显式**清空** —— `envOr`
// 对空白串本就回落默认值，故本用例在**任何**宿主环境下都真跑且无条件断言。
func TestDefaultsMatchDesignPorts(t *testing.T) {
	t.Setenv(EnvPublicAddr, "")
	t.Setenv(EnvAdminAddr, "")
	t.Setenv(EnvLog, "")
	t.Setenv(EnvMigrations, "")
	cfg := FromEnv()
	if cfg.PublicAddr != "0.0.0.0:6800" {
		t.Fatalf("public_addr 默认值应为 0.0.0.0:6800，实际 %q", cfg.PublicAddr)
	}
	if cfg.AdminAddr != "127.0.0.1:8080" {
		t.Fatalf("admin_addr 默认值应为 127.0.0.1:8080，实际 %q", cfg.AdminAddr)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("log_level 默认值应为 info，实际 %q", cfg.LogLevel)
	}
	if cfg.MigrationsDir != "migrations" {
		t.Fatalf("migrations_dir 默认值应为 migrations，实际 %q", cfg.MigrationsDir)
	}
}

// 对照 `cookie_secure_parse_defaults_to_auto`。
func TestCookieSecureParseDefaultsToAuto(t *testing.T) {
	cases := []struct {
		in   string
		want CookieSecureMode
	}{
		{"", CookieSecureAuto},
		{"ALWAYS", CookieSecureAlways},
		{"never", CookieSecureNever},
		{"garbage", CookieSecureAuto},
		{"  Always  ", CookieSecureAlways}, // trim + 大小写不敏感
	}
	for _, c := range cases {
		if got := ParseCookieSecureMode(c.in); got != c.want {
			t.Fatalf("ParseCookieSecureMode(%q) = %v，应为 %v", c.in, got, c.want)
		}
	}
}

// 补充：`String()` 是 `EnvSnapshot` 上报的稳定字符串，必须与 `parse` 互逆。
func TestCookieSecureStringRoundTrips(t *testing.T) {
	for _, m := range []CookieSecureMode{CookieSecureAuto, CookieSecureAlways, CookieSecureNever} {
		if got := ParseCookieSecureMode(m.String()); got != m {
			t.Fatalf("String/Parse 不互逆：%v → %q → %v", m, m.String(), got)
		}
	}
}

// 对照 `admin_policy_generates_when_absent`。
func TestAdminPolicyGeneratesWhenAbsent(t *testing.T) {
	cfg := Config{PublicAddr: "x", AdminAddr: "x", DBPath: "x", MigrationsDir: "x",
		LogLevel: "info", PublicToken: nil, AdminPassword: nil,
		AdminCookieSecure: CookieSecureAuto, WebDir: nil, AllowFileDelete: false, PublicCORS: false}
	p := ResolveAdminPolicy(cfg)
	if !p.Generated {
		t.Fatalf("缺口令应标记为已生成")
	}
	if len(p.Password) < 16 {
		t.Fatalf("生成口令应足够长（≥16），实际 %d", len(p.Password))
	}
}

// 对照 `admin_policy_uses_injected_password`。
func TestAdminPolicyUsesInjectedPassword(t *testing.T) {
	pw := "test123"
	cfg := Config{PublicAddr: "x", AdminAddr: "x", DBPath: "x", MigrationsDir: "x",
		LogLevel: "info", PublicToken: nil, AdminPassword: &pw,
		AdminCookieSecure: CookieSecureAuto, WebDir: nil, AllowFileDelete: false, PublicCORS: false}
	p := ResolveAdminPolicy(cfg)
	if p.Generated {
		t.Fatalf("注入口令时不应标记为已生成")
	}
	if p.Password != "test123" {
		t.Fatalf("口令应取注入值，实际 %q", p.Password)
	}
}

// ---------------------------------------------------------------------------
// env_opt / env_bool 的分支覆盖（Rust 侧只有注释声明、无直接断言）
// ---------------------------------------------------------------------------

func TestEnvOptBranches(t *testing.T) {
	const unsetKey = "DISPATCH_OPT_UNSET_XYZ"
	mustUnset(t, unsetKey)
	if got := envOpt(unsetKey); got != nil {
		t.Fatalf("未设置应返回 nil，实际 %q", *got)
	}

	t.Setenv("DISPATCH_OPT_BLANK_XYZ", "\t ")
	if got := envOpt("DISPATCH_OPT_BLANK_XYZ"); got != nil {
		t.Fatalf("全空白应返回 nil，实际 %q", *got)
	}

	t.Setenv("DISPATCH_OPT_REAL_XYZ", "v")
	if got := envOpt("DISPATCH_OPT_REAL_XYZ"); got == nil || *got != "v" {
		t.Fatalf("应读到真实值，实际 %v", got)
	}
}

func TestEnvBoolBranches(t *testing.T) {
	cases := []struct {
		set   bool
		value string
		def   bool
		want  bool
	}{
		{false, "", false, false}, // 未设置 → 默认
		{true, "1", false, true},
		{true, "true", false, true},
		{true, "YES", false, true}, // 大小写不敏感
		{true, " on ", false, true},
		{true, "0", true, false},
		{true, "false", true, false},
		{true, "NO", true, false},
		{true, "off", true, false},
		{true, "", true, true},        // 空串 → 默认
		{true, "garbage", true, true}, // 无法识别 → 默认
	}
	for i, c := range cases {
		key := "DISPATCH_BOOL_XYZ"
		if c.set {
			t.Setenv(key, c.value)
		} else {
			mustUnset(t, key)
		}
		if got := envBool(key, c.def); got != c.want {
			t.Fatalf("第 %d 例 envBool(set=%v,val=%q,def=%v) = %v，应为 %v",
				i, c.set, c.value, c.def, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 与 Rust `crates/dispatch-core/src/admin/config.rs::tests` 对照
// ---------------------------------------------------------------------------

func snap() EnvSnapshot {
	return EnvSnapshot{
		PublicAddr:              "0.0.0.0:6800",
		AdminAddr:               "127.0.0.1:8080",
		DBPath:                  "data/x.db",
		MigrationsDir:           "migrations",
		LogLevel:                "info",
		StateDir:                strPtr("/data"),
		WebDir:                  nil,
		AdminPasswordConfigured: true,
		PublicTokenConfigured:   true,
		AllowFileDelete:         false,
		PublicCORS:              false,
		AdminCookieSecure:       CookieSecureAuto,
	}
}

// 对照 `sensitive_items_are_masked`，并把 Rust 注释里「非敏感项保留原值」也断言上。
func TestItemsSensitiveAreMasked(t *testing.T) {
	items := snap().Items()
	for _, it := range items {
		if isSensitive(it.Key) && it.Value != "****" {
			t.Fatalf("敏感项必须掩码为 ****：key=%s value=%q", it.Key, it.Value)
		}
	}
	// 非敏感项保留原值
	var logItem *ConfigItem
	for i := range items {
		if items[i].Key == EnvLog {
			logItem = &items[i]
		}
	}
	if logItem == nil || logItem.Value != "info" {
		t.Fatalf("非敏感项 DISPATCH_LOG 应保留原值 info，实际 %+v", logItem)
	}
}

// ★ 关键契约：`DISPATCH_ADMIN_PASSWORD` / `DISPATCH_PUBLIC_TOKEN` 的**键名本身命中敏感判定**，
// 故即便其展示字面量是「已配置」，最终也**必然**被掩码成 `****`（Rust 既有行为，本移植不"修正"）。
// 这条能杀掉「先掩码再算值」或「漏掉这两个键」的变异。
func TestItemsMasksAdminPasswordAndPublicTokenEvenWhenConfigured(t *testing.T) {
	byKey := indexItems(snap().Items())
	if got := byKey[EnvAdminPassword].Value; got != "****" {
		t.Fatalf("DISPATCH_ADMIN_PASSWORD 必须掩码为 ****，实际 %q", got)
	}
	if got := byKey[EnvPublicToken].Value; got != "****" {
		t.Fatalf("DISPATCH_PUBLIC_TOKEN 必须掩码为 ****，实际 %q", got)
	}
}

// 断言顺序与若干非敏感项的**实际展示值**（可观测终态），钉住 1:1 移植的字面量与默认占位符。
func TestItemsContentAndOrder(t *testing.T) {
	items := snap().Items()
	want := []struct {
		key, value string
	}{
		{EnvPublicAddr, "0.0.0.0:6800"},
		{EnvAdminAddr, "127.0.0.1:8080"},
		{EnvDB, "data/x.db"},
		{EnvMigrations, "migrations"},
		{EnvLog, "info"},
		{EnvStateDir, "/data"},
		{EnvWebDir, "(内嵌)"}, // WebDir=nil ⇒ 内嵌
		{EnvAdminPassword, "****"},
		{EnvPublicToken, "****"},
		{EnvAllowFileDelete, "false"},
		{EnvPublicCORS, "false"},
		{EnvAdminCookieSecure, "auto"},
	}
	if len(items) != len(want) {
		t.Fatalf("配置项数量应为 %d，实际 %d", len(want), len(items))
	}
	for i, w := range want {
		if items[i].Key != w.key {
			t.Fatalf("第 %d 项键应为 %s，实际 %s", i, w.key, items[i].Key)
		}
		if items[i].Value != w.value {
			t.Fatalf("第 %d 项(%s)值应为 %q，实际 %q", i, w.key, w.value, items[i].Value)
		}
		if items[i].Mutable {
			t.Fatalf("第 %d 项(%s) mutable 应恒为 false", i, w.key)
		}
		if !items[i].Effective {
			t.Fatalf("第 %d 项(%s) effective 应恒为 true", i, w.key)
		}
	}
}

// StateDir=nil 时的默认占位符必须是 `(默认)`（半角括号）—— 与 WebDir 的 `(内嵌)` 同风格，
// 但**不同于** AdminPassword/PublicToken 用的全角括号；这条钉住它们别被写串。
func TestItemsStateDirDefaultPlaceholder(t *testing.T) {
	e := snap()
	e.StateDir = nil
	byKey := indexItems(e.Items())
	if got := byKey[EnvStateDir].Value; got != "(默认)" {
		t.Fatalf("StateDir=nil 应展示 (默认)，实际 %q", got)
	}
}

func indexItems(items []ConfigItem) map[string]ConfigItem {
	m := make(map[string]ConfigItem, len(items))
	for _, it := range items {
		m[it.Key] = it
	}
	return m
}

func strPtr(s string) *string { return &s }

// mustUnset 显式移除环境变量，并在测试结束恢复其原值（原本存在则恢复、不存在则保持移除）。
//
// 用途：把「未设置」这条分支变成**无条件执行**的判据 —— 避免「若宿主已设置则 Skip」这种
// **静默跳过（＝假绿）**。`t.Setenv` 只能设置、无法表示「未设置」，故需要本辅助。
func mustUnset(t *testing.T, key string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv(%s) 失败：%v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// ---------------------------------------------------------------------------
// 金样声明（如实记录：以下判据在 Rust 侧**无**对应断言，其期望值是本包新定的）
// ---------------------------------------------------------------------------
//
//   - `env_bool` 的各分支：Rust `src/config.rs` 只声明、**无单测**；
//   - `EnvSnapshot::items` 的**顺序与字面量**（描述/风险级/默认占位符）：Rust
//     `admin/config.rs` 只断言了 `sensitive_items_are_masked`，未断言这些；
//   - 故 `TestEnvBoolBranches` / `TestItemsContentAndOrder` 的期望值是**据实现与设计字面量
//     新定**的，**不是**从 Rust 跑出来的金样（本机无 Rust 工具链，详见 T01 回执 §6）。
