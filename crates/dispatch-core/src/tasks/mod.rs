//! 任务域对外出口。
//!
//! 本模块聚合「任务」这一领域的所有读写：
//! - [`create`]：**★唯一入库入口** `create_task` —— aria2 / BitComet / admin 三处共用；
//! - [`query`]：读路径（按 GID/task_id 查询、分页列表、状态变更、删除）；
//! - [`row`]：列索引常量 + 值解码（`Vec<Value>` → `TaskRow`）；
//! - [`state`]：`internal_state` ↔ Aria2 `status` 的映射（含 `stopped` 双义）。

pub mod create;
pub mod query;
pub mod row;
pub mod state;

pub use create::{CreateError, IngressOrigin, TaskCreateInput, TaskCreated, TaskKind, create_task};
pub use query::{
    PAGE_DEFAULT, TaskListPage, TaskListQuery, count_all, count_by_internal_state, delete_task,
    get_by_gid, get_by_id, list_by_ids, list_by_states, list_tasks, set_state,
};
pub use row::{N_TASK_COLUMNS, TASK_COLUMNS, TaskRow, decode_task};
pub use state::{AriaView, aria_status, aria_view, classify_node_status};
