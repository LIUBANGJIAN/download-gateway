//! `GET /api/admin/config` 的数据组装：把 [`EnvSnapshot`] 拍平成配置项列表。
//!
//! **敏感项一律掩码 `****`**（含 `token` / `password` 的键）。已配置/未配置只反映布尔，
//! 不回传任何密钥明文。

use serde::Serialize;

use crate::state::EnvSnapshot;

/// 单条配置项。
#[derive(Debug, Clone, Serialize)]
pub struct ConfigItem {
    /// 变量名。
    pub key: String,
    /// 生效值（敏感项为 `****`）。
    pub value: String,
    /// 是否可热改（本轮全部需重启生效 ⇒ `false`）。
    pub mutable: bool,
    /// 是否当前生效。
    pub effective: bool,
    /// 风险级别（`low`/`medium`/`high`）。
    pub risk: &'static str,
    /// 描述。
    pub description: &'static str,
}

/// 敏感键判定。
fn is_sensitive(key: &str) -> bool {
    let k = key.to_ascii_lowercase();
    k.contains("token") || k.contains("password")
}

impl EnvSnapshot {
    /// 生成配置项列表（顺序稳定）。
    pub fn items(&self) -> Vec<ConfigItem> {
        let mut out = Vec::new();
        let mut push = |key: &str, value: String, risk: &'static str, desc: &'static str| {
            let shown = if is_sensitive(key) {
                "****".to_string()
            } else {
                value
            };
            out.push(ConfigItem {
                key: key.to_string(),
                value: shown,
                mutable: false,
                effective: true,
                risk,
                description: desc,
            });
        };

        push(
            "DISPATCH_PUBLIC_ADDR",
            self.public_addr.clone(),
            "medium",
            "对外推送端口监听地址（可能公网可达）",
        );
        push(
            "DISPATCH_ADMIN_ADDR",
            self.admin_addr.clone(),
            "high",
            "管理后台监听地址（应仅内网）",
        );
        push("DISPATCH_DB", self.db_path.clone(), "low", "SQLite 路径");
        push(
            "DISPATCH_MIGRATIONS",
            self.migrations_dir.clone(),
            "low",
            "迁移目录",
        );
        push("DISPATCH_LOG", self.log_level.clone(), "low", "日志级别");
        push(
            "DISPATCH_STATE_DIR",
            self.state_dir.clone().unwrap_or_else(|| "(默认)".into()),
            "low",
            "client_id 文件目录来源",
        );
        push(
            "DISPATCH_WEB_DIR",
            self.web_dir.clone().unwrap_or_else(|| "(内嵌)".into()),
            "low",
            "管理台页面覆盖目录",
        );
        push(
            "DISPATCH_ADMIN_PASSWORD",
            if self.admin_password_configured {
                "已配置".into()
            } else {
                "（启动生成·仅本次有效）".into()
            },
            "high",
            "管理口令是否由环境注入",
        );
        push(
            "DISPATCH_PUBLIC_TOKEN",
            if self.public_token_configured {
                "已配置".into()
            } else {
                "未配置（不校验）".into()
            },
            "high",
            "对外口可选 token",
        );
        push(
            "DISPATCH_ALLOW_FILE_DELETE",
            self.allow_file_delete.to_string(),
            "high",
            "是否允许删节点文件",
        );
        push(
            "DISPATCH_PUBLIC_CORS",
            self.public_cors.to_string(),
            "medium",
            "是否对 6800 回 ACAO",
        );
        push(
            "DISPATCH_ADMIN_COOKIE_SECURE",
            self.admin_cookie_secure.to_string(),
            "medium",
            "会话 Cookie 的 Secure 策略",
        );
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn snap() -> EnvSnapshot {
        EnvSnapshot {
            public_addr: "0.0.0.0:6800".into(),
            admin_addr: "127.0.0.1:8080".into(),
            db_path: "data/x.db".into(),
            migrations_dir: "migrations".into(),
            log_level: "info".into(),
            state_dir: Some("/data".into()),
            web_dir: None,
            admin_password_configured: true,
            public_token_configured: true,
            allow_file_delete: false,
            public_cors: false,
            admin_cookie_secure: "auto",
        }
    }

    #[test]
    fn sensitive_items_are_masked() {
        let items = snap().items();
        for it in &items {
            if is_sensitive(&it.key) {
                assert_eq!(it.value, "****", "敏感项必须掩码: {}", it.key);
            }
        }
        // 非敏感项保留原值
        let log = items.iter().find(|i| i.key == "DISPATCH_LOG").unwrap();
        assert_eq!(log.value, "info");
    }
}
