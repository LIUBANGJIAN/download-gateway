//! 节点连接配置。

use std::path::PathBuf;

/// 一个 BitComet 节点的连接参数。
///
/// 对应 `node` 表中的 `base_url` / `user` / `pass_enc` / `device_name`。
/// 注意 `password` 在库里是加密存储（`pass_enc`），**解密属后续任务**；
/// 本 crate 只接受明文，由调用方负责解密边界。
#[derive(Debug, Clone)]
pub struct NodeProfile {
    /// 基础 URL，形如 `http://node.example:9085`（**不带尾斜杠**，构造时会去掉）。
    pub base_url: String,
    /// WebUI 用户名。
    pub username: String,
    /// WebUI 密码（明文）。
    pub password: String,
    /// 上报给节点的设备名（出现在节点的已绑定设备列表里）。
    pub device_name: String,
    /// `client_id` 持久化路径；`None` 时用 [`crate::clientid::default_client_id_path`]。
    pub client_id_path: Option<PathBuf>,
}

impl NodeProfile {
    /// 用默认设备名构造。
    pub fn new(
        base_url: impl Into<String>,
        username: impl Into<String>,
        password: impl Into<String>,
    ) -> Self {
        Self {
            base_url: base_url.into(),
            username: username.into(),
            password: password.into(),
            device_name: crate::DEFAULT_DEVICE_NAME.to_string(),
            client_id_path: None,
        }
    }

    /// 规范化后的 base_url（去尾斜杠）。
    pub fn normalized_base(&self) -> String {
        self.base_url.trim_end_matches('/').to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn trailing_slashes_are_stripped() {
        let p = NodeProfile::new("http://example:9085///", "u", "p");
        assert_eq!(p.normalized_base(), "http://example:9085");
    }

    #[test]
    fn default_device_name_is_used() {
        let p = NodeProfile::new("http://example:9085", "u", "p");
        assert_eq!(
            p.device_name,
            crate::DEFAULT_DEVICE_NAME,
            "默认设备名应可预测，否则节点侧设备列表会出现随机条目"
        );
    }
}
