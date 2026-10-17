package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/config"
)

// 对照 Rust `crates/dispatch-core/src/lib.rs::version_is_three_numeric_parts`。
//
// ⚠️ 与 Rust 相同的诚实边界：注入链（`APP_VERSION` → `-ldflags -X`）**无法**在单测层证伪，
// 单测只能断言「值的形态」。真正的守卫在端到端（容器 `/healthz` 自报版本）。
func TestVersionIsThreeNumericParts(t *testing.T) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		t.Fatalf("版本号应为三段 X.Y.Z，实际 = %s", version)
	}
	for _, p := range parts {
		if p == "" {
			t.Fatalf("版本号每一段都应为纯数字，实际 = %s", version)
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				t.Fatalf("版本号每一段都应为纯数字，实际 = %s", version)
			}
		}
	}
}

// effectiveLogLevel 的分支：DISPATCH_LOG（显式）优先 → RUST_LOG 回退 → 配置默认。
//
// ⚠️ 这是**有意偏离 Rust** 的一条（Rust `try_from_default_env` 是 RUST_LOG 优先），
// 按 team-lead 裁定实现，见 `effectiveLogLevel` 的文档注释。
func TestEffectiveLogLevel(t *testing.T) {
	t.Setenv(config.EnvLog, "")
	t.Setenv(EnvRustLog, "")
	if got := effectiveLogLevel("info"); got != "info" {
		t.Fatalf("两者都未设置应回落 cfgLevel，实际 %q", got)
	}
	t.Setenv(EnvRustLog, "debug")
	if got := effectiveLogLevel("info"); got != "debug" {
		t.Fatalf("DISPATCH_LOG 未设置时应取 RUST_LOG，实际 %q", got)
	}
	t.Setenv(config.EnvLog, "warn")
	if got := effectiveLogLevel("info"); got != "warn" {
		t.Fatalf("DISPATCH_LOG 显式设置时应优先，实际 %q", got)
	}
	// 纯级别经 parseLevel 落到正确级别
	if got := parseLevel(effectiveLogLevel("info")); got != slog.LevelWarn {
		t.Fatalf("DISPATCH_LOG=warn 应生效为 Warn，实际 %v", got)
	}
}

// parseLevel 的全部分支。
//
// ⚠️ 金样声明：`parseLevel` 在 Rust 侧无对应单测（`init_tracing` 直接用 EnvFilter），
// 故本条期望值是按 Rust `EnvFilter::new` 对纯级别的语义**新定**的，不是从 Rust 抄来的金样。
func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo}, // 大小写不敏感
		{"  Debug ", slog.LevelDebug},
		{"garbage", slog.LevelInfo}, // 未知 → 回落 Info
		{"", slog.LevelInfo},        // 空 → 回落 Info
	}
	for _, c := range cases {
		if got := parseLevel(c.in); got != c.want {
			t.Fatalf("parseLevel(%q) = %v，应为 %v", c.in, got, c.want)
		}
	}
}

// isLoopback 的分支（对照 Rust `is_loopback`）。
func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"127.0.0.1", true}, // 无端口
		{"0.0.0.0:8080", false},
		{"192.168.1.1:8080", false},
		{"example.com:80", false},
		{":8080", false}, // 空 host
	}
	for _, c := range cases {
		if got := isLoopback(c.addr); got != c.want {
			t.Fatalf("isLoopback(%q) = %v，应为 %v", c.addr, got, c.want)
		}
	}
}

// stateDirFromEnv 的分支：未设置 ⇒ nil；已设置（含空串）⇒ 非 nil。
func TestStateDirFromEnv(t *testing.T) {
	const key = "DISPATCH_STATE_DIR"

	if _, ok := os.LookupEnv(key); ok {
		t.Skip("环境已设置 DISPATCH_STATE_DIR，跳过未设置分支")
	}

	// 分支一：未设置 ⇒ nil。
	if got := stateDirFromEnv(); got != nil {
		t.Fatalf("未设置应为 nil，实际 %q", *got)
	}

	// 分支二：已设置（非空）⇒ 原值。
	t.Setenv(key, "/data")
	if got := stateDirFromEnv(); got == nil || *got != "/data" {
		t.Fatalf("已设置应返回原值，实际 %v", got)
	}
}

// 分支三：已设置但为空串 ⇒ **非 nil**（刻意照搬 Rust `std::env::var(..).ok()` 的行为，
// 与 config.envOpt 的「空白视为未设置」不同）。
func TestStateDirFromEnvTreatsEmptyAsSet(t *testing.T) {
	t.Setenv("DISPATCH_STATE_DIR", "")
	got := stateDirFromEnv()
	if got == nil {
		t.Fatalf("已设置（空串）时不应返回 nil")
	}
	if *got != "" {
		t.Fatalf("已设置（空串）时应返回空串，实际 %q", *got)
	}
}
