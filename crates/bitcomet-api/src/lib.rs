//! BitComet WebUI API 客户端。
//!
//! 本 crate 的代码在 **T02** 从 `G:/workbuddy/bitcomet监控/bitcomet-monitor-gui`
//! 的 `bitcomet_core` 抽取而来，并做了两处必要改造。
//!
//! # 模块
//!
//! | 模块 | 来源 | 改造程度 |
//! |---|---|---|
//! | [`rncryptor`] | 参考实现 `rncryptor.rs` | **原样复用**（纯算法，与平台无关） |
//! | [`addtask`] | 参考实现 `addtask.rs` | **原样复用**（纯分类逻辑） |
//! | [`clientid`] | 参考实现 `clientid.rs` | 改造：状态目录解析加 `DISPATCH_STATE_DIR` / `XDG_CONFIG_HOME` / `HOME`（容器里 `%APPDATA%` 不存在且 `/usr/local/bin` 不可写） |
//! | [`profile`] | 参考实现 `config.rs` 的 `ServerProfile` | 收敛为最小字段集 |
//! | [`client`] | 参考实现 `client.rs` | **重写为异步**（`reqwest::blocking` → `reqwest`），
//!   三段式认证按官方 WebUI API 文档实现 |
//!
//! # 两条硬约束（违反会导致 Docker 内直接不可用）
//!
//! ## 一、TLS 必须走 rustls
//!
//! `Cargo.toml` 已设 `default-features = false`，只启用 `json` / `rustls-tls` / `http2`。
//! 参考实现原用 `native-tls`（Windows 走 schannel），而 Linux / Docker 下 schannel
//! 根本不存在 —— 切到 rustls 是 T02 的必做项（`00 §50-51`、`02 §0 A-1`）。
//! CI 中另有一道闸门：`Cargo.lock` 中**不得出现 `native-tls`**。
//!
//! 注意 feature 名的版本陷阱（实测，缺陷 D-4）：`rustls-tls` 这个名字**只在
//! `reqwest` ≤ 0.12 存在**，0.13 已改名为 `rustls`。因此本 crate 锁定 `reqwest = "0.12"`。
//!
//! ## 二、HTTP 客户端必须 no_proxy
//!
//! BitComet 节点在内网，发往节点的请求**不能**被系统代理变量劫持（F11）。
//! 本机实测环境会注入 `http_proxy`，故 [`client::Client::new`] 与 [`build_client`]
//! 都固定使用 `.no_proxy()`。

pub mod addtask;
pub mod client;
pub mod clientid;
pub mod profile;
pub mod rncryptor;

pub use client::{Client, ClientError, is_ok_code};
pub use profile::NodeProfile;

/// 默认上报的客户端设备名（BitComet WebUI 登录参数之一）。
pub const DEFAULT_DEVICE_NAME: &str = "dispatch-proxy";

/// 默认 User-Agent。
pub fn user_agent() -> String {
    format!("download-gateway/{}", env!("CARGO_PKG_VERSION"))
}

/// 构造一个「不走系统代理」的通用 `reqwest::Client`。
///
/// 业务请求请优先用 [`Client`]（它已内含 `.no_proxy()`）；
/// 本函数供需要裸 HTTP 客户端的场景（如网盘反查适配器）使用。
pub fn build_client() -> Result<reqwest::Client, reqwest::Error> {
    reqwest::Client::builder()
        .no_proxy()
        .user_agent(user_agent())
        .build()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn client_builds() {
        let c = build_client();
        assert!(c.is_ok(), "reqwest client 构造失败: {:?}", c.err());
    }

    #[test]
    fn user_agent_carries_crate_version() {
        assert!(user_agent().starts_with("download-gateway/"));
    }
}
