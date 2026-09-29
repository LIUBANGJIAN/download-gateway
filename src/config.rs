//! 运行期配置。
//!
//! 全部来自环境变量（Docker 友好），无需配置文件即可启动。
//!
//! | 变量 | 默认值 | 说明 |
//! |---|---|---|
//! | `DISPATCH_PUBLIC_ADDR` | `0.0.0.0:6800` | 对外推送端口（`02 §0 Q6`） |
//! | `DISPATCH_ADMIN_ADDR` | `127.0.0.1:8080` | 管理后台端口（`02 §0 Q7`：默认仅内网） |
//! | `DISPATCH_DB` | `data/dispatch.db` | SQLite 路径（Docker 里挂 `/data`） |
//! | `DISPATCH_MIGRATIONS` | `migrations` | 迁移目录 |
//! | `DISPATCH_LOG` | `info` | 日志级别（可被 `RUST_LOG` 覆盖） |
//! | `DISPATCH_PUBLIC_TOKEN` | 空=不校验 | 对外口可选 token（敏感） |
//! | `DISPATCH_ADMIN_PASSWORD` | 空⇒启动生成随机口令并打印 | 管理口令（敏感） |
//! | `DISPATCH_ADMIN_COOKIE_SECURE` | `auto` | `auto`/`always`/`never` |
//! | `DISPATCH_WEB_DIR` | 无 | 覆盖内嵌页面（热改/排障） |
//! | `DISPATCH_ALLOW_FILE_DELETE` | `false` | 是否允许删节点文件 |
//! | `DISPATCH_PUBLIC_CORS` | `false` | 是否对 6800 回 `Access-Control-Allow-Origin` |

use std::path::PathBuf;

use anyhow::Result;
use dispatch_core::state::{AdminPolicy, CookieSecureMode};

/// 进程级配置。
#[derive(Debug, Clone)]
pub struct Config {
    /// 对外推送端口监听地址。
    pub public_addr: String,
    /// 管理后台监听地址。
    pub admin_addr: String,
    /// SQLite 文件路径。
    pub db_path: PathBuf,
    /// 迁移脚本目录。
    pub migrations_dir: PathBuf,
    /// 日志级别（EnvFilter 语法）。
    pub log_level: String,
    /// `DISPATCH_PUBLIC_TOKEN`；`None` = 对外口不校验。
    pub public_token: Option<String>,
    /// `DISPATCH_ADMIN_PASSWORD`；`None` = 启动生成随机口令。
    pub admin_password: Option<String>,
    /// 会话 Cookie 的 `Secure` 策略。
    pub admin_cookie_secure: CookieSecureMode,
    /// `DISPATCH_WEB_DIR`。
    pub web_dir: Option<PathBuf>,
    /// 是否允许删节点文件。
    pub allow_file_delete: bool,
    /// 是否开启对外口 CORS。
    pub public_cors: bool,
}

impl Config {
    /// 从环境变量构造；缺失或空白一律回落默认值。
    pub fn from_env() -> Result<Self> {
        Ok(Self {
            public_addr: env_or("DISPATCH_PUBLIC_ADDR", "0.0.0.0:6800"),
            admin_addr: env_or("DISPATCH_ADMIN_ADDR", "127.0.0.1:8080"),
            db_path: PathBuf::from(env_or("DISPATCH_DB", "data/dispatch.db")),
            migrations_dir: PathBuf::from(env_or("DISPATCH_MIGRATIONS", "migrations")),
            log_level: env_or("DISPATCH_LOG", "info"),
            public_token: env_opt("DISPATCH_PUBLIC_TOKEN"),
            admin_password: env_opt("DISPATCH_ADMIN_PASSWORD"),
            admin_cookie_secure: CookieSecureMode::parse(&env_or(
                "DISPATCH_ADMIN_COOKIE_SECURE",
                "auto",
            )),
            web_dir: env_opt("DISPATCH_WEB_DIR").map(PathBuf::from),
            allow_file_delete: env_bool("DISPATCH_ALLOW_FILE_DELETE", false),
            public_cors: env_bool("DISPATCH_PUBLIC_CORS", false),
        })
    }
}

/// 解析管理面策略；口令未注入时**生成随机口令**并标记 `generated=true`。
pub fn resolve_admin_policy(cfg: &Config) -> AdminPolicy {
    match &cfg.admin_password {
        Some(pw) => AdminPolicy {
            password: pw.clone(),
            generated: false,
            cookie_secure: cfg.admin_cookie_secure,
            allow_file_delete: cfg.allow_file_delete,
        },
        None => AdminPolicy {
            password: dispatch_core::ids::new_hex_128(),
            generated: true,
            cookie_secure: cfg.admin_cookie_secure,
            allow_file_delete: cfg.allow_file_delete,
        },
    }
}

fn env_or(key: &str, default: &str) -> String {
    match std::env::var(key) {
        Ok(v) if !v.trim().is_empty() => v,
        _ => default.to_string(),
    }
}

fn env_opt(key: &str) -> Option<String> {
    match std::env::var(key) {
        Ok(v) if !v.trim().is_empty() => Some(v),
        _ => None,
    }
}

fn env_bool(key: &str, default: bool) -> bool {
    match std::env::var(key) {
        Ok(v) => match v.trim().to_ascii_lowercase().as_str() {
            "1" | "true" | "yes" | "on" => true,
            "0" | "false" | "no" | "off" => false,
            "" => default,
            _ => default,
        },
        Err(_) => default,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn env_or_falls_back_when_unset() {
        assert_eq!(env_or("DISPATCH_DEFINITELY_UNSET_KEY_XYZ", "dflt"), "dflt");
    }

    #[test]
    fn env_or_treats_blank_as_unset() {
        // 专用 key，避免与其它测试竞争
        unsafe { std::env::set_var("DISPATCH_BLANK_KEY_XYZ", "   ") };
        assert_eq!(env_or("DISPATCH_BLANK_KEY_XYZ", "dflt"), "dflt");
        unsafe { std::env::remove_var("DISPATCH_BLANK_KEY_XYZ") };
    }

    #[test]
    fn env_or_reads_real_value() {
        unsafe { std::env::set_var("DISPATCH_REAL_KEY_XYZ", "v1") };
        assert_eq!(env_or("DISPATCH_REAL_KEY_XYZ", "dflt"), "v1");
        unsafe { std::env::remove_var("DISPATCH_REAL_KEY_XYZ") };
    }

    #[test]
    fn defaults_match_design_ports() {
        // 干净环境（CI 不注入 DISPATCH_*）下应等于设计端口
        if std::env::var("DISPATCH_PUBLIC_ADDR").is_ok() {
            return;
        }
        let cfg = Config::from_env().unwrap();
        assert_eq!(cfg.public_addr, "0.0.0.0:6800");
        assert_eq!(cfg.admin_addr, "127.0.0.1:8080");
        assert_eq!(cfg.log_level, "info");
        assert_eq!(cfg.migrations_dir, PathBuf::from("migrations"));
    }

    #[test]
    fn cookie_secure_parse_defaults_to_auto() {
        assert_eq!(CookieSecureMode::parse(""), CookieSecureMode::Auto);
        assert_eq!(CookieSecureMode::parse("ALWAYS"), CookieSecureMode::Always);
        assert_eq!(CookieSecureMode::parse("never"), CookieSecureMode::Never);
        assert_eq!(CookieSecureMode::parse("garbage"), CookieSecureMode::Auto);
    }

    #[test]
    fn admin_policy_generates_when_absent() {
        let cfg = Config {
            public_addr: "x".into(),
            admin_addr: "x".into(),
            db_path: PathBuf::from("x"),
            migrations_dir: PathBuf::from("x"),
            log_level: "info".into(),
            public_token: None,
            admin_password: None,
            admin_cookie_secure: CookieSecureMode::Auto,
            web_dir: None,
            allow_file_delete: false,
            public_cors: false,
        };
        let p = resolve_admin_policy(&cfg);
        assert!(p.generated, "缺口令应标记为已生成");
        assert!(p.password.len() >= 16, "生成口令应足够长");
    }

    #[test]
    fn admin_policy_uses_injected_password() {
        let cfg = Config {
            public_addr: "x".into(),
            admin_addr: "x".into(),
            db_path: PathBuf::from("x"),
            migrations_dir: PathBuf::from("x"),
            log_level: "info".into(),
            public_token: None,
            admin_password: Some("test123".into()),
            admin_cookie_secure: CookieSecureMode::Auto,
            web_dir: None,
            allow_file_delete: false,
            public_cors: false,
        };
        let p = resolve_admin_policy(&cfg);
        assert!(!p.generated);
        assert_eq!(p.password, "test123");
    }
}
