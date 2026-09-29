//! `internal_state` ↔ Aria2 `status` 的映射。
//!
//! 关键点：Aria2 的 `status` 只有 6 个取值（waiting/paused/active/complete/error/removed），
//! 而我们的内部状态更丰富。映射是**单向、确定性**的纯函数，便于单测与跨入口一致。

use super::row::TaskRow;

/// 由 `(internal_state, permillage, error_class)` 得 Aria2 `status`。
///
/// `stopped` 是**双义**（`02 §4.1.6`）：既可能是「已完成」也可能是「已暂停」，还可能带错误。
/// 因此需要 `permillage` 与 `error_class` 辅助反解，而**不能**只看 `internal_state`。
pub fn aria_status(
    internal_state: &str,
    permillage: i64,
    error_class: Option<&str>,
) -> &'static str {
    match internal_state {
        "queued" | "dispatching" => "waiting",
        "running" => "active",
        "paused" => "paused",
        "completed" => "complete",
        "failed" => "error",
        "removed" => "removed",
        "stopped" => {
            if permillage >= 1000 {
                "complete"
            } else if matches!(error_class, Some(c) if c != "none" && c != "not_started") {
                "error"
            } else {
                "paused"
            }
        }
        _ => "waiting",
    }
}

/// 完整状态视图（供 `tellStatus` / `tellActive` / … 组装响应）。
#[derive(Debug, Clone)]
pub struct AriaView {
    /// Aria2 `status` 字符串。
    pub status: String,
    /// Aria2 数字错误码（0 = 无错误，非 0 = 失败）。
    pub error_code: i64,
    /// 人类可读错误信息。
    pub error_message: String,
}

/// 由 `TaskRow` 组装 Aria2 视图。
///
/// `status` **优先取 `task.aria_status` 列**（写路径已落库的权威口径，`NOT NULL DEFAULT 'waiting'`）；
/// 仅当列为空串时，才回退到 [`aria_status`] 的派生映射。这样读路径与写口径一致，避免
/// 「列里写着 `paused` 却因派生出 `waiting`」这类漂移（BUG-4）。
///
/// `error_code` 语义对齐 Aria2：0 表示无错误；`not_started`（用户未点开始）不算错误 ⇒ 0。
pub fn aria_view(t: &TaskRow) -> AriaView {
    let status = if t.aria_status.trim().is_empty() {
        aria_status(&t.internal_state, t.permillage, t.error_class.as_deref()).to_string()
    } else {
        t.aria_status.clone()
    };
    let has_error = matches!(
        t.error_class.as_deref(),
        Some(c) if c != "none" && c != "not_started"
    );
    AriaView {
        status,
        error_code: if has_error { 1 } else { 0 },
        error_message: t.error_message.clone().unwrap_or_default(),
    }
}

/// §4.1.6「stopped 双义」的反解（供未来 T05 使用）。
///
/// 输入是节点上报的 `status`/`errorCode`，输出是我们内部的 `(internal_state, error_class)`。
/// 纯函数，本轮以单测夹具验证，不参与运行期逻辑。
pub fn classify_node_status(
    node_status: &str,
    error_code: Option<&str>,
    permillage: i64,
) -> (&'static str, Option<&'static str>) {
    let ec = error_code.filter(|c| !c.is_empty() && *c != "none");
    match node_status {
        "complete" | "completed" => ("completed", None),
        "error" | "failed" => ("failed", Some(map_error_class(ec.unwrap_or("unknown")))),
        "paused" => (
            "paused",
            if permillage == 0 {
                Some("not_started")
            } else {
                None
            },
        ),
        "active" | "running" => ("running", None),
        "waiting" | "queued" => ("queued", None),
        "removed" => ("removed", None),
        _ => ("queued", None),
    }
}

/// 把节点错误串归一到 `task.error_class` 的受约束取值集（DDL CHECK）。
fn map_error_class(e: &str) -> &'static str {
    match e {
        "not_started" => "not_started",
        "server_file_not_found" => "server_file_not_found",
        "out_of_retry" => "out_of_retry",
        "node_down" => "node_down",
        "silently_not_created" => "silently_not_created",
        "throttled" => "throttled",
        "agent_timeout" => "agent_timeout",
        _ => "unknown",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn full_internal_state_table_maps_deterministically() {
        assert_eq!(aria_status("queued", 0, None), "waiting");
        assert_eq!(aria_status("dispatching", 0, None), "waiting");
        assert_eq!(aria_status("running", 100, None), "active");
        assert_eq!(aria_status("paused", 0, Some("not_started")), "paused");
        assert_eq!(aria_status("completed", 1000, None), "complete");
        assert_eq!(aria_status("failed", 0, Some("node_down")), "error");
        assert_eq!(aria_status("removed", 0, None), "removed");
        assert_eq!(aria_status("unknown-thing", 0, None), "waiting");
    }

    #[test]
    fn stopped_is_double_meaning() {
        // 已暂停（用户未点开始）
        assert_eq!(aria_status("stopped", 0, None), "paused");
        assert_eq!(aria_status("stopped", 0, Some("not_started")), "paused");
        // 出错
        assert_eq!(
            aria_status("stopped", 0, Some("server_file_not_found")),
            "error"
        );
        // 已完成
        assert_eq!(aria_status("stopped", 1000, None), "complete");
    }

    #[test]
    fn running_with_full_permillage_is_active() {
        assert_eq!(aria_status("running", 1000, None), "active");
    }

    #[test]
    fn view_sets_error_code_only_for_real_errors() {
        let mut t = sample_row();
        t.internal_state = "failed".into();
        t.aria_status = "error".into();
        t.error_class = Some("node_down".into());
        t.error_message = Some("节点离线".into());
        let v = aria_view(&t);
        assert_eq!(v.status, "error");
        assert_eq!(v.error_code, 1);
        assert_eq!(v.error_message, "节点离线");

        t.error_class = Some("not_started".into());
        let v = aria_view(&t);
        assert_eq!(v.error_code, 0, "not_started 不是错误");
    }

    /// BUG-4 回归：`status` 以 `aria_status` 列为权威；列空串才回退派生。
    #[test]
    fn view_prefers_aria_status_column_then_falls_back() {
        // 列非空 ⇒ 取列值（即使派生会得出别的结论）
        let mut t = sample_row();
        t.internal_state = "queued".into(); // 派生本会得 "waiting"
        t.aria_status = "paused".into();
        assert_eq!(aria_view(&t).status, "paused", "非空列优先");

        // 列空串 ⇒ 回退到派生（failed + node_down → error）
        t.internal_state = "failed".into();
        t.error_class = Some("node_down".into());
        t.aria_status = String::new();
        assert_eq!(aria_view(&t).status, "error", "空列应回退派生");
    }

    #[test]
    fn classify_reverse_mapping() {
        assert_eq!(
            classify_node_status("complete", None, 1000),
            ("completed", None)
        );
        assert_eq!(
            classify_node_status("error", Some("node_down"), 0),
            ("failed", Some("node_down"))
        );
        assert_eq!(
            classify_node_status("paused", None, 0),
            ("paused", Some("not_started"))
        );
        assert_eq!(classify_node_status("running", None, 10), ("running", None));
        assert_eq!(classify_node_status("waiting", None, 0), ("queued", None));
    }

    fn sample_row() -> TaskRow {
        TaskRow {
            task_id: "01".into(),
            gid: Some("0".into()),
            group_id: None,
            source_key: None,
            kind: "http".into(),
            url_raw: "http://e/x".into(),
            url_norm: None,
            name: None,
            size_state: "unknown".into(),
            total_size: None,
            downloaded_size: 0,
            permillage: 0,
            aria_status: "waiting".into(),
            internal_state: "queued".into(),
            error_class: None,
            error_message: None,
            node_id: None,
            node_task_id: None,
            attempt_no: 0,
            max_attempts: 3,
            next_retry_at: 0,
            possible_duplicate: false,
            created_at: 0,
            updated_at: 0,
            completed_at: None,
        }
    }
}
