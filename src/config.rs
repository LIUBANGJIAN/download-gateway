//! 运行期配置。
//!
//! 全部来自环境变量（Docker 友好），无需配置文件即可启动。
//!
//! | 变量 | 默认值 | 说明 |
//! |---|---|---|
//! | `DISPATCH_PUBLIC_ADDR` | `0.0.0.0:6800` | 对外推送端口（`02 §0 Q6`） |
//! | `DISPATCH_ADMIN_ADDR` | `127.0.0.1:8080` | 管理后台端口（`02 §0 Q7`：默认仅内网） |
//! | `DISPATCH_DB` | `data/gateway.db` | SQLite 路径（Docker 里挂 `/data`） |
//! | `DISPATCH_MIGRATIONS` | `migrations` | 迁移目录 |
//! | `DISPATCH_LOG` | `info` | 日志级别（可被 `RUST_LOG` 覆盖） |

use std::path::PathBuf;

use anyhow::Result;

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
        })
    }
}

fn env_or(key: &str, default: &str) -> String {
    match std::env::var(key) {
        Ok(v) if !v.trim().is_empty() => v,
        _ => default.to_string(),
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
}
