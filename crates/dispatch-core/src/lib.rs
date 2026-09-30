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

/// 本 crate 版本（供 `/healthz` 与日志上报）。
pub const VERSION: &str = env!("CARGO_PKG_VERSION");
