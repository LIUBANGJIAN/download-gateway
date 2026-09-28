//! 调度核心。
//!
//! T01 阶段只含**存储层**（`store`）：SQLite 单写者 actor + 迁移框架。
//! 后续任务会在这里追加：`health`（T03）、`scheduler`（T04）、`task`（T05）、
//! `sync`（T06）、`dedup`（T07）、`ingress`（T08/T09）、`admin`（T10）、`naming`（T13）。

pub mod store;

/// 本 crate 版本（供 `/healthz` 与日志上报）。
pub const VERSION: &str = env!("CARGO_PKG_VERSION");
