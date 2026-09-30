//! 调度核心。
//!
//! T01 阶段只含**存储层**（`store`）：SQLite 单写者 actor + 迁移框架。
//! 本轮增量（T08α/T09α/T10α）在此追加：
//!
//! | 模块 | 职责 |
//! |---|---|
//! | [`state`] | 两端口各自的共享状态（`PublicState` / `AdminState` / `AdminPolicy` / `EnvSnapshot`） |
//! | [`ids`] | GID / task_id（ULID）/ 128 位随机十六进制，纯函数可单测 |
//! | [`health`] | `/healthz` 响应体（契约与旧 `main.rs::healthz` 逐字节一致，含 503） |
//! | [`tasks`] | 任务域：**★唯一入库入口 `create_task`** + 读路径 + 行解码 + 状态映射 |
//! | [`ingress`] | 对外口（6800）：Aria2 兼容面 + BitComet 兼容面 + 自签三段式握手 |
//! | [`admin`] | 管理口（8080）：会话 + REST + 内嵌零构建 Web 管理台 |
//! | [`secret`] | 节点密码的可逆封装（管理台「眼睛」回看原文；复用 `bitcomet-api::rncryptor`） |
//!
//! 后续任务会继续追加：`scheduler`（T04）、`sync`（T06）、`dedup`（T07）、`naming`（T13）。

pub mod admin;
pub mod health;
pub mod ids;
pub mod ingress;
pub mod secret;
pub mod state;
pub mod store;
pub mod tasks;

/// 本版本。CI 构建时由 `APP_VERSION` **在编译期**注入（形如 `0.1.7`）；
/// 本地开发 / 未注入时退回 Cargo 清单里的版本（`0.1.0`）。
/// 注意 `option_env!` 是编译期求值 —— 这不是运行时读环境变量。
pub const VERSION: &str = match option_env!("APP_VERSION") {
    Some(v) => v,
    None => env!("CARGO_PKG_VERSION"),
};

#[cfg(test)]
mod tests {
    use super::VERSION;

    /// 「版本号一定长成 X.Y.Z」的不变量（不依赖是否注入）。
    #[test]
    fn version_is_three_numeric_parts() {
        let parts: Vec<&str> = VERSION.split('.').collect();
        assert_eq!(parts.len(), 3, "版本号应为三段 X.Y.Z，实际 = {VERSION}");
        for p in &parts {
            assert!(
                !p.is_empty() && p.bytes().all(|b| b.is_ascii_digit()),
                "版本号每一段都应为纯数字，实际 = {VERSION}"
            );
        }
    }

    /// 注入链取证：CI 注入 `APP_VERSION` 时必须逐字生效；未注入时退回清单版本。
    /// 配合「`APP_VERSION=0.1.7` 下重编」的两次运行，证明注入真的进了二进制。
    #[test]
    fn version_follows_injected_env() {
        match option_env!("APP_VERSION") {
            Some(v) => assert_eq!(VERSION, v, "注入 APP_VERSION={v} 后 VERSION 应逐字相等"),
            None => assert_eq!(
                VERSION,
                env!("CARGO_PKG_VERSION"),
                "未注入时应退回 CARGO_PKG_VERSION"
            ),
        }
    }
}
