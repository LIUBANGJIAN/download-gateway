// Package config 提供运行期配置：从环境变量解析、解析管理面策略、以及管理台「运行环境」
// 面板所需的只读快照组装。
//
// # 1:1 移植来源（逐个文件标注）
//
//   - `src/config.rs`（仓库根，Rust bin 的 config 模块）：`Config` / `Config::from_env` /
//     `resolve_admin_policy` / `env_or` / `env_opt` / `env_bool`。
//   - `crates/dispatch-core/src/admin/config.rs`：`EnvSnapshot::items` + `ConfigItem` +
//     `is_sensitive`（敏感项一律掩码 `****`）。
//   - `crates/dispatch-core/src/state.rs` 的**配置相关部分**：`AdminPolicy` /
//     `CookieSecureMode` / `EnvSnapshot` 三个类型。
//
// # 与 design/12 §2 的一处显式偏离（已在回执「发现但未动的问题」记明）
//
// §2 把 `AdminPolicy` / `CookieSecureMode` / `EnvSnapshot` 归到 `internal/state/state.go`。
// 但 Go 的**方法接收者必须与类型定义同包**：`EnvSnapshot.Items()` 只能落在定义
// `EnvSnapshot` 的包里。若把三类型放 `state`、把 `Items` 放 `config`，则无法编译；
// 而若把 `EnvSnapshot` 放 `config`、`AdminPolicy` 留 `state`，`config`（`resolve_admin_policy`
// 需 `AdminPolicy`）与 `state`（`AdminState` 需 `EnvSnapshot`）会**相互引用形成包循环**。
// 因此本包承载「全部配置相关类型」，`internal/state` **单向**依赖本包。
//
// 这与 Rust 的实际依赖方向一致：`src/config.rs` 顶部即
// `use dispatch_core::state::{AdminPolicy, CookieSecureMode};`（config → state）。
package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/LIUBANGJIAN/download-gateway/internal/ids"
)

// 环境变量名（逐字对照 Rust 侧字面量）。
const (
	EnvPublicAddr        = "DISPATCH_PUBLIC_ADDR"
	EnvAdminAddr         = "DISPATCH_ADMIN_ADDR"
	EnvDB                = "DISPATCH_DB"
	EnvMigrations        = "DISPATCH_MIGRATIONS"
	EnvLog               = "DISPATCH_LOG"
	EnvPublicToken       = "DISPATCH_PUBLIC_TOKEN"
	EnvAdminPassword     = "DISPATCH_ADMIN_PASSWORD"
	EnvAdminCookieSecure = "DISPATCH_ADMIN_COOKIE_SECURE"
	EnvStateDir          = "DISPATCH_STATE_DIR"
	EnvWebDir            = "DISPATCH_WEB_DIR"
	EnvAllowFileDelete   = "DISPATCH_ALLOW_FILE_DELETE"
	EnvPublicCORS        = "DISPATCH_PUBLIC_CORS"
)

// 默认值（对照 Rust `Config::from_env` 的字面量）。
const (
	DefaultPublicAddr = "0.0.0.0:6800"
	DefaultAdminAddr  = "127.0.0.1:8080"
	DefaultDB         = "data/dispatch.db"
	DefaultMigrations = "migrations"
	DefaultLogLevel   = "info"
	DefaultCookieMode = "auto"
)

// Config 是进程级配置（对应 Rust `struct Config`）。
//
// Rust 侧路径类型为 `PathBuf`；Go 用 `string`（`store.Open` / `secret.Resolve` 亦取 string）。
// 选项语义以指针表示：`nil` == Rust 的 `None`。
type Config struct {
	// PublicAddr 对外推送端口监听地址。
	PublicAddr string
	// AdminAddr 管理后台监听地址。
	AdminAddr string
	// DBPath SQLite 文件路径。
	DBPath string
	// MigrationsDir 迁移脚本目录。
	MigrationsDir string
	// LogLevel 日志级别。
	LogLevel string
	// PublicToken `DISPATCH_PUBLIC_TOKEN`；nil = 对外口不校验。
	PublicToken *string
	// AdminPassword `DISPATCH_ADMIN_PASSWORD`；nil = 启动生成随机口令。
	AdminPassword *string
	// AdminCookieSecure 会话 Cookie 的 `Secure` 策略。
	AdminCookieSecure CookieSecureMode
	// WebDir `DISPATCH_WEB_DIR`；nil = 用内嵌页面。
	WebDir *string
	// AllowFileDelete 是否允许删节点文件。
	AllowFileDelete bool
	// PublicCORS 是否开启对外口 CORS。
	PublicCORS bool
}

// FromEnv 从环境变量构造配置；缺失或空白一律回落默认值。
//
// Rust 的 `from_env` 签名是 `-> Result<Self>`，但**实现上恒为 `Ok`**（无任何失败出口）。
// Go 版据此直接返回 `Config`（不引入永不触发 error 的分支）。
func FromEnv() Config {
	return Config{
		PublicAddr:        envOr(EnvPublicAddr, DefaultPublicAddr),
		AdminAddr:         envOr(EnvAdminAddr, DefaultAdminAddr),
		DBPath:            envOr(EnvDB, DefaultDB),
		MigrationsDir:     envOr(EnvMigrations, DefaultMigrations),
		LogLevel:          envOr(EnvLog, DefaultLogLevel),
		PublicToken:       envOpt(EnvPublicToken),
		AdminPassword:     envOpt(EnvAdminPassword),
		AdminCookieSecure: ParseCookieSecureMode(envOr(EnvAdminCookieSecure, DefaultCookieMode)),
		WebDir:            envOpt(EnvWebDir),
		AllowFileDelete:   envBool(EnvAllowFileDelete, false),
		PublicCORS:        envBool(EnvPublicCORS, false),
	}
}

// ResolveAdminPolicy 解析管理面策略；口令未注入时**生成随机口令**并置 `Generated=true`。
//
// 对照 Rust `resolve_admin_policy`：`None` 分支用 `ids::new_hex_128()`（32 字符十六进制）。
func ResolveAdminPolicy(cfg Config) AdminPolicy {
	if cfg.AdminPassword != nil {
		return AdminPolicy{
			Password:        *cfg.AdminPassword,
			Generated:       false,
			CookieSecure:    cfg.AdminCookieSecure,
			AllowFileDelete: cfg.AllowFileDelete,
		}
	}
	return AdminPolicy{
		Password:        ids.NewHex128(),
		Generated:       true,
		CookieSecure:    cfg.AdminCookieSecure,
		AllowFileDelete: cfg.AllowFileDelete,
	}
}

// envOr 读环境变量：未设置或**全空白**时回落默认值；否则**原样返回**（不 trim）。
//
// 对照 Rust `env_or`：`Ok(v) if !v.trim().is_empty() => v`（返回未 trim 的 v）。
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// envOpt 读环境变量：未设置或全空白时返回 nil。
//
// 对照 Rust `env_opt`。
func envOpt(key string) *string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return &v
	}
	return nil
}

// envBool 读布尔环境变量：`1/true/yes/on` ⇒ true；`0/false/no/off` ⇒ false；
// 其余取值（含空串、无法识别）与「未设置」一律回落默认值。
//
// 对照 Rust `env_bool`（大小写不敏感，先 trim）。
func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// CookieSecureMode 是 Cookie `Secure` 属性策略（对照 Rust `enum CookieSecureMode`）。
type CookieSecureMode int

const (
	// CookieSecureAuto 仅当 `X-Forwarded-Proto: https` 时加 `Secure`（默认）。
	CookieSecureAuto CookieSecureMode = iota
	// CookieSecureAlways 恒加 `Secure`。
	CookieSecureAlways
	// CookieSecureNever 恒不加 `Secure`（排障）。
	CookieSecureNever
)

// ParseCookieSecureMode 解析环境变量取值；未知/空白回落 `Auto`（大小写不敏感，先 trim）。
//
// 对照 Rust `CookieSecureMode::parse`。
func ParseCookieSecureMode(s string) CookieSecureMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "always":
		return CookieSecureAlways
	case "never":
		return CookieSecureNever
	default:
		return CookieSecureAuto
	}
}

// String 返回供 `EnvSnapshot` 上报的稳定字符串（`auto`/`always`/`never`）。
//
// 对照 Rust `CookieSecureMode::as_str`。
func (m CookieSecureMode) String() string {
	switch m {
	case CookieSecureAlways:
		return "always"
	case CookieSecureNever:
		return "never"
	default:
		return "auto"
	}
}

// AdminPolicy 是管理面策略（对照 Rust `struct AdminPolicy`）。
type AdminPolicy struct {
	// Password 管理口令（明文，仅存内存）。
	Password string
	// Generated true = 口令由启动时生成（非环境注入）。
	Generated bool
	// CookieSecure Cookie `Secure` 属性策略。
	CookieSecure CookieSecureMode
	// AllowFileDelete 是否允许删节点文件。
	AllowFileDelete bool
}

// EnvSnapshot 是 `GET /api/admin/config` 的数据源（对照 Rust `struct EnvSnapshot`）。
//
// 只读快照：进程启动时的有效环境，不含密钥明文（密钥类字段只上报「是否已配置」）。
type EnvSnapshot struct {
	// PublicAddr 对外监听地址。
	PublicAddr string
	// AdminAddr 管理监听地址。
	AdminAddr string
	// DBPath SQLite 路径。
	DBPath string
	// MigrationsDir 迁移目录。
	MigrationsDir string
	// LogLevel 日志级别。
	LogLevel string
	// StateDir `DISPATCH_STATE_DIR`（client_id 文件目录来源）；nil = 未设置。
	StateDir *string
	// WebDir `DISPATCH_WEB_DIR`；nil = 内嵌。
	WebDir *string
	// AdminPasswordConfigured 管理口令是否由环境注入。
	AdminPasswordConfigured bool
	// PublicTokenConfigured `DISPATCH_PUBLIC_TOKEN` 是否已配置。
	PublicTokenConfigured bool
	// AllowFileDelete 是否允许删节点文件。
	AllowFileDelete bool
	// PublicCORS 是否开启对外口 CORS。
	PublicCORS bool
	// AdminCookieSecure `DISPATCH_ADMIN_COOKIE_SECURE` 的有效取值。
	AdminCookieSecure CookieSecureMode
}

// ConfigItem 是单条配置项（对照 Rust `struct ConfigItem`）。
type ConfigItem struct {
	// Key 变量名。
	Key string
	// Value 生效值（敏感项为 `****`）。
	Value string
	// Mutable 是否可热改（本轮全部需重启生效 ⇒ 恒 false）。
	Mutable bool
	// Effective 是否当前生效（恒 true）。
	Effective bool
	// Risk 风险级别（`low`/`medium`/`high`）。
	Risk string
	// Description 描述。
	Description string
}

// isSensitive 判定敏感键：键名（小写后）含 `token` 或 `password`。
//
// 对照 Rust `is_sensitive`。
func isSensitive(key string) bool {
	k := strings.ToLower(key)
	return strings.Contains(k, "token") || strings.Contains(k, "password")
}

// Items 生成配置项列表（顺序稳定）。
//
// 对照 Rust `EnvSnapshot::items`：**先算值、再掩码** —— 注意 `DISPATCH_ADMIN_PASSWORD`
// 与 `DISPATCH_PUBLIC_TOKEN` 的键名本身命中敏感判定，故它们展示出的 `已配置 / 未配置…`
// 字面量会被掩码为 `****`（这是 Rust 侧的既有行为，本移植**不"修正"**它）。
func (e EnvSnapshot) Items() []ConfigItem {
	var out []ConfigItem
	push := func(key, value, risk, desc string) {
		shown := value
		if isSensitive(key) {
			shown = "****"
		}
		out = append(out, ConfigItem{
			Key:         key,
			Value:       shown,
			Mutable:     false,
			Effective:   true,
			Risk:        risk,
			Description: desc,
		})
	}

	push(EnvPublicAddr, e.PublicAddr, "medium", "对外推送端口监听地址（可能公网可达）")
	push(EnvAdminAddr, e.AdminAddr, "high", "管理后台监听地址（应仅内网）")
	push(EnvDB, e.DBPath, "low", "SQLite 路径")
	push(EnvMigrations, e.MigrationsDir, "low", "迁移目录")
	push(EnvLog, e.LogLevel, "low", "日志级别")
	push(EnvStateDir, valueOr(e.StateDir, "(默认)"), "low", "client_id 文件目录来源")
	push(EnvWebDir, valueOr(e.WebDir, "(内嵌)"), "low", "管理台页面覆盖目录")
	push(EnvAdminPassword,
		configuredText(e.AdminPasswordConfigured, "已配置", "（启动生成·仅本次有效）"),
		"high", "管理口令是否由环境注入")
	push(EnvPublicToken,
		configuredText(e.PublicTokenConfigured, "已配置", "未配置（不校验）"),
		"high", "对外口可选 token")
	push(EnvAllowFileDelete, strconv.FormatBool(e.AllowFileDelete), "high", "是否允许删节点文件")
	push(EnvPublicCORS, strconv.FormatBool(e.PublicCORS), "medium", "是否对 6800 回 ACAO")
	push(EnvAdminCookieSecure, e.AdminCookieSecure.String(), "medium", "会话 Cookie 的 Secure 策略")
	return out
}

// valueOr 等价 Rust `opt.clone().unwrap_or_else(|| fallback)`。
func valueOr(opt *string, fallback string) string {
	if opt != nil {
		return *opt
	}
	return fallback
}

// configuredText 等价 Rust 的 `if flag { a } else { b }` 二选一。
func configuredText(flag bool, whenTrue, whenFalse string) string {
	if flag {
		return whenTrue
	}
	return whenFalse
}
