//! 管理台「设置」页的数据源：派发 / 负载策略的**清单 + 开关 + 优先级**。
//!
//! # 本轮它只是「可配置」，**不会真的生效**
//!
//! 真正按这些策略挑机器的那段代码（内部代号 T03–T07「调度内核」）还没写。
//! 因此每个策略项都带 `effective: false` 与一句 `effective_note`，
//! 界面必须**如实标注**。这不是保守，而是诚实：让用户以为「改完行为立刻变化」
//! 比不做这个页面更糟。
//!
//! # 存储：复用 `app_config` KV，**不新建表**
//!
//! * key = `policy.<策略键>`（如 `policy.least_tasks`）
//! * value = JSON `{"enabled":bool,"priority":int}`
//!
//! 代码里内置**完整默认表**（[`CATALOG`]），库里**只存用户改过的差异**。
//! 这个取舍的理由：将来调度内核落地、策略清单增删时，老库**不需要任何迁移** ——
//! 新增的策略会自动以默认值出现，删掉的策略残留行会被忽略。
//! 若改成「全量落库」，升级时就必须写一段「补齐缺失项」的迁移逻辑，
//! 而那段逻辑一旦漏了某个键，症状是「界面少了一条策略」这种安静的错。

use std::collections::HashMap;
use std::sync::Arc;

use axum::Json;
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::Response;
use rusqlite::types::Value;
use serde::{Deserialize, Serialize};
use serde_json::json;

use super::{C_INTERNAL, C_VALIDATION_ERROR, err, ok};
use crate::state::AdminState;
use crate::store::{Store, StoreError};

/// 优先级下界。
pub const PRIORITY_MIN: i64 = 1;
/// 优先级上界。
pub const PRIORITY_MAX: i64 = 999;
/// 前端拖拽排序时的建议步进（仅为界面默认，**不做校验**：
/// 强校验步进会让「手填 25」这种无害输入被拒，收益为负）。
pub const PRIORITY_STEP: i64 = 10;

/// 所有策略统一的生效说明（内核未落地）。
pub const EFFECTIVE_NOTE: &str =
    "已保存，当前尚未生效 —— 策略由调度内核应用，而调度内核（T03–T07）尚未落地";

/// 单条策略的**静态定义**。全部 `&'static str`，可放进 `const`。
pub struct PolicyDef {
    /// 稳定键（落库用，**不可改**）。
    pub key: &'static str,
    /// 界面显示名。
    pub name: &'static str,
    /// 分组（界面上的小标题）。
    pub category: &'static str,
    /// 给非程序员看的一句话说明。
    pub description: &'static str,
    /// 出厂默认是否开启。
    pub default_enabled: bool,
    /// 是否允许关闭。`false` 的项在界面置灰、后端拒绝关闭请求。
    pub can_disable: bool,
    /// 出厂默认优先级（**越小越优先**）。
    pub default_priority: i64,
    /// 是否标记为「危险」（关闭会伤到系统，界面需二次确认）。
    pub dangerous: bool,
}

/// 策略总表（12 条）。**这是唯一事实源**：界面、默认值、校验都读它。
pub const CATALOG: &[PolicyDef] = &[
    PolicyDef {
        key: "max_concurrent",
        name: "节点并发上限",
        category: "容量安全阀",
        description: "一台机器同时在跑的任务数达到上限后，不再往它派新任务。这是防压垮机器的最后一道闸。",
        default_enabled: true,
        can_disable: false,
        default_priority: 10,
        dangerous: true,
    },
    PolicyDef {
        key: "group_affinity",
        name: "同组同节点",
        category: "亲和",
        description: "同一部剧集的各集尽量落在同一台机器上，避免把一部剧拆得七零八落。",
        default_enabled: true,
        can_disable: true,
        default_priority: 20,
        dangerous: false,
    },
    PolicyDef {
        key: "least_tasks",
        name: "最少活跃任务优先",
        category: "负载均衡",
        description: "新任务优先派给当前手上活最少的那台机器。",
        default_enabled: true,
        can_disable: true,
        default_priority: 30,
        dangerous: false,
    },
    PolicyDef {
        key: "retry_backoff",
        name: "失败重试退避",
        category: "容错",
        description: "任务失败后按递增间隔重试，而不是死循环硬打。",
        default_enabled: true,
        can_disable: true,
        default_priority: 40,
        dangerous: false,
    },
    PolicyDef {
        key: "throttle_backoff",
        name: "限流退避",
        category: "容错",
        description: "某台机器被限流时，暂时降低对它的调用频率，等它缓过来。",
        default_enabled: true,
        can_disable: true,
        default_priority: 50,
        dangerous: false,
    },
    PolicyDef {
        key: "node_weight",
        name: "节点权重",
        category: "负载均衡",
        description: "按每台机器配置的权重加权分配，性能强的机器多分一些活。",
        default_enabled: true,
        can_disable: true,
        default_priority: 60,
        dangerous: false,
    },
    PolicyDef {
        key: "max_concurrent_in_group",
        name: "组内并发上限",
        category: "容量安全阀",
        description: "同一部剧集在单台机器上同时下载的集数上限。",
        default_enabled: true,
        can_disable: true,
        default_priority: 70,
        dangerous: false,
    },
    PolicyDef {
        key: "weighted_least_tasks",
        name: "加权最少任务",
        category: "负载均衡",
        description: "在「最少活跃任务」的基础上按权重折算，避免弱机器被塞满。",
        default_enabled: false,
        can_disable: true,
        default_priority: 80,
        dangerous: false,
    },
    PolicyDef {
        key: "source_affinity",
        name: "同来源同节点",
        category: "亲和",
        description: "来自同一个网盘的任务尽量落在同一台机器上。",
        default_enabled: false,
        can_disable: true,
        default_priority: 90,
        dangerous: false,
    },
    PolicyDef {
        key: "source_pinned",
        name: "来源固定节点",
        category: "约束",
        description: "指定某个来源的任务**只**派到某一台机器，其余机器一概不用。",
        default_enabled: false,
        can_disable: true,
        default_priority: 100,
        dangerous: false,
    },
    PolicyDef {
        key: "role_routing",
        name: "按节点角色路由",
        category: "路由",
        description: "按机器上标注的角色分派：剧集任务给「剧集机」、网盘任务给「网盘机」。",
        default_enabled: false,
        can_disable: true,
        default_priority: 110,
        dangerous: false,
    },
    PolicyDef {
        key: "node_rate_limit",
        name: "节点限速",
        category: "限速",
        description: "按每台机器配置的限速值限制下发速率，避免打满带宽。",
        default_enabled: false,
        can_disable: true,
        default_priority: 120,
        dangerous: false,
    },
];

/// 按 key 查静态定义。
pub fn find(key: &str) -> Option<&'static PolicyDef> {
    CATALOG.iter().find(|d| d.key == key)
}

/// 界面/接口用的策略视图（静态定义 + 当前值）。
#[derive(Debug, Clone, Serialize)]
pub struct PolicyView {
    /// 稳定键。
    pub key: &'static str,
    /// 显示名。
    pub name: &'static str,
    /// 分组。
    pub category: &'static str,
    /// 说明。
    pub description: &'static str,
    /// 当前是否开启。
    pub enabled: bool,
    /// 当前优先级（越小越优先）。
    pub priority: i64,
    /// 是否允许关闭。
    pub can_disable: bool,
    /// 是否危险。
    pub dangerous: bool,
    /// 是否被用户改过（`app_config` 里存在差异行）。
    pub customized: bool,
    /// 本轮恒为 `false` —— 见模块文档。
    pub effective: bool,
    /// 生效说明。
    pub effective_note: &'static str,
}

/// 库里存的一行覆盖值。
#[derive(Debug, Clone, Deserialize, Serialize)]
struct Stored {
    enabled: bool,
    priority: i64,
}

/// 读全部策略（默认值 + 库内差异合并）。顺序恒为 [`CATALOG`] 顺序。
pub async fn load(store: &Store) -> Result<Vec<PolicyView>, StoreError> {
    let rows = store
        .query(
            "SELECT key, value FROM app_config WHERE key LIKE 'policy.%'",
            vec![],
        )
        .await?;

    let mut stored: HashMap<String, Stored> = HashMap::new();
    for row in rows {
        let (Value::Text(k), Value::Text(v)) = (&row[0], &row[1]) else {
            continue;
        };
        let Some(name) = k.strip_prefix("policy.") else {
            continue;
        };
        // 解析失败**不能**让整个页面挂掉：忽略这一行，退回默认值即可。
        if let Ok(s) = serde_json::from_str::<Stored>(v) {
            stored.insert(name.to_string(), s);
        } else {
            tracing::warn!("app_config 里的 {k} 不是合法 JSON，已忽略并退回默认值");
        }
    }

    Ok(CATALOG
        .iter()
        .map(|d| {
            let hit = stored.get(d.key);
            PolicyView {
                key: d.key,
                name: d.name,
                category: d.category,
                description: d.description,
                enabled: hit.map_or(d.default_enabled, |s| s.enabled),
                priority: hit.map_or(d.default_priority, |s| s.priority),
                can_disable: d.can_disable,
                dangerous: d.dangerous,
                customized: hit.is_some(),
                effective: false,
                effective_note: EFFECTIVE_NOTE,
            }
        })
        .collect())
}

/// 单条提交项。
#[derive(Debug, Clone, Deserialize)]
pub struct PolicyUpdate {
    /// 策略键。
    pub key: String,
    /// 新开关值；`None` = 不改。
    pub enabled: Option<bool>,
    /// 新优先级；`None` = 不改。
    pub priority: Option<i64>,
}

/// 保存校验失败的原因（会被渲染成 `422` 的 message）。
#[derive(Debug, thiserror::Error)]
pub enum PolicyError {
    /// 未知策略键。
    #[error("未知策略：{0}（不在当前策略表内）")]
    UnknownKey(String),
    /// 优先级越界。
    #[error("策略 {key} 的优先级 {value} 越界：允许范围 {PRIORITY_MIN}–{PRIORITY_MAX}")]
    PriorityOutOfRange {
        /// 策略键。
        key: String,
        /// 提交值。
        value: i64,
    },
    /// 试图关闭不可关闭的策略。
    #[error("策略 {0} 不允许关闭：它是容量安全阀，关掉会把节点压垮")]
    NotDisableable(String),
    /// 存储层错误。
    #[error("{0}")]
    Store(#[from] StoreError),
}

impl PolicyError {
    /// 该错误对应的 HTTP 状态码（校验类一律 422，存储类 500）。
    pub fn status(&self) -> StatusCode {
        match self {
            Self::Store(_) => StatusCode::INTERNAL_SERVER_ERROR,
            _ => StatusCode::UNPROCESSABLE_ENTITY,
        }
    }
}

/// 校验 + 落库 + 回读。
///
/// **先全量校验再写**：半途失败不许留下「改了一半」的配置 —— 那会让用户
/// 看到「保存失败，但有一部分确实生效了」这种最难排查的状态。
pub async fn save(store: &Store, updates: &[PolicyUpdate]) -> Result<Vec<PolicyView>, PolicyError> {
    // ---- 1) 全量校验 ----
    for u in updates {
        let Some(def) = find(&u.key) else {
            return Err(PolicyError::UnknownKey(u.key.clone()));
        };
        if let Some(p) = u.priority
            && !(PRIORITY_MIN..=PRIORITY_MAX).contains(&p)
        {
            return Err(PolicyError::PriorityOutOfRange {
                key: u.key.clone(),
                value: p,
            });
        }
        if u.enabled == Some(false) && !def.can_disable {
            return Err(PolicyError::NotDisableable(u.key.clone()));
        }
    }

    // ---- 2) 落库 ----
    let now = crate::ids::now_secs();
    for u in updates {
        let Some(def) = find(&u.key) else {
            continue; // 已在上面被拒，走不到这里
        };
        // 与库内现值合并后再写：允许「只改开关」或「只改优先级」。
        let current = load_one(store, def).await?;
        let merged = Stored {
            enabled: u.enabled.unwrap_or(current.enabled),
            priority: u.priority.unwrap_or(current.priority),
        };
        let value = serde_json::to_string(&merged).unwrap_or_else(|_| "{}".into());
        store
            .execute(
                "INSERT INTO app_config (key, value, updated_at) VALUES (?1, ?2, ?3)
                 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
                vec![
                    Value::Text(format!("policy.{}", def.key)),
                    Value::Text(value),
                    Value::Integer(now),
                ],
            )
            .await?;
    }

    load(store).await.map_err(PolicyError::from)
}

/// 读单条策略的当前值（默认值或库内覆盖）。
async fn load_one(store: &Store, def: &PolicyDef) -> Result<Stored, StoreError> {
    let rows = store
        .query(
            "SELECT value FROM app_config WHERE key = ?1",
            vec![Value::Text(format!("policy.{}", def.key))],
        )
        .await?;
    if let Some(row) = rows.first()
        && let Value::Text(v) = &row[0]
        && let Ok(s) = serde_json::from_str::<Stored>(v)
    {
        return Ok(s);
    }
    Ok(Stored {
        enabled: def.default_enabled,
        priority: def.default_priority,
    })
}

// ---------------------------------------------------------------------------
// HTTP 处理器
// ---------------------------------------------------------------------------

/// `GET /api/admin/config` —— 策略列表 + 只读的运行环境快照。
///
/// 响应形状（**相对旧版是破坏性变更**，这是刻意的）：
///
/// ```json
/// { "policies": [ … ], "env": [ … ], "effective_note": "…" }
/// ```
///
/// 旧版返回的是一个**裸数组**（环境变量清单）。改成对象的原因是：
/// 策略是主体、环境是附录，两者不能共用一个匿名数组 —— 那样前端只能靠字段名猜。
pub async fn config_get(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
) -> Response {
    let expiry = match super::rest::require_read(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let policies = match load(&st.store).await {
        Ok(v) => v,
        Err(e) => {
            return err(
                StatusCode::INTERNAL_SERVER_ERROR,
                C_INTERNAL,
                format!("{e}"),
            );
        }
    };
    let mut resp = ok(json!({
        "policies": policies,
        "env": st.env.items(),
        "effective_note": EFFECTIVE_NOTE,
        "priority": { "min": PRIORITY_MIN, "max": PRIORITY_MAX, "step": PRIORITY_STEP },
    }));
    super::rest::renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

/// `PUT /api/admin/config` 的请求体。
#[derive(Debug, Deserialize)]
pub struct ConfigPutReq {
    /// 要保存的策略项（允许只提交改动过的几条）。
    pub policies: Vec<PolicyUpdate>,
}

/// `PUT /api/admin/config` —— 保存开关与优先级。
pub async fn config_put(
    State(st): State<Arc<AdminState>>,
    headers: axum::http::HeaderMap,
    Json(req): Json<ConfigPutReq>,
) -> Response {
    let expiry = match super::rest::require_write(&st, &headers) {
        Ok(e) => e,
        Err(r) => return r,
    };
    let mut resp = match save(&st.store, &req.policies).await {
        Ok(policies) => ok(json!({
            "policies": policies,
            "env": st.env.items(),
            "effective_note": EFFECTIVE_NOTE,
            "priority": { "min": PRIORITY_MIN, "max": PRIORITY_MAX, "step": PRIORITY_STEP },
        })),
        Err(e) => err(e.status(), C_VALIDATION_ERROR, format!("{e}")),
    };
    super::rest::renew_cookie(&mut resp, &st, &headers, expiry);
    resp
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn store() -> Store {
        let s = Store::open_in_memory().unwrap();
        s.execute(
            "CREATE TABLE app_config (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL)",
            vec![],
        )
        .await
        .unwrap();
        s
    }

    #[test]
    fn catalog_keys_are_unique_and_have_sane_priorities() {
        let mut seen = std::collections::HashSet::new();
        for d in CATALOG {
            assert!(seen.insert(d.key), "策略键重复：{}", d.key);
            assert!(
                (PRIORITY_MIN..=PRIORITY_MAX).contains(&d.default_priority),
                "默认优先级越界：{}",
                d.key
            );
            assert!(!d.name.is_empty() && !d.description.is_empty());
        }
        assert_eq!(CATALOG.len(), 12, "本轮策略表固定 12 条");
    }

    /// 出厂状态下，界面拿到的就是默认表（顺序、开关、优先级全部一致）。
    #[tokio::test]
    async fn load_returns_defaults_on_empty_db() {
        let s = store().await;
        let v = load(&s).await.unwrap();
        assert_eq!(v.len(), CATALOG.len());
        for (got, def) in v.iter().zip(CATALOG) {
            assert_eq!(got.key, def.key);
            assert_eq!(got.enabled, def.default_enabled);
            assert_eq!(got.priority, def.default_priority);
            assert!(!got.customized);
            assert!(!got.effective, "内核未落地，effective 必须恒为 false");
        }
        s.shutdown().await;
    }

    /// 保存后再读，改动必须**持久化**（这是 C-02/C-03/C-04 的核心）。
    #[tokio::test]
    async fn save_then_load_persists() {
        let s = store().await;
        let out = save(
            &s,
            &[
                PolicyUpdate {
                    key: "least_tasks".into(),
                    enabled: Some(false),
                    priority: Some(300),
                },
                PolicyUpdate {
                    key: "node_weight".into(),
                    enabled: None,
                    priority: Some(15),
                },
            ],
        )
        .await
        .unwrap();
        assert_eq!(out.len(), 12, "返回值必须是完整策略表");

        let lt = out.iter().find(|p| p.key == "least_tasks").unwrap();
        assert!(!lt.enabled);
        assert_eq!(lt.priority, 300);
        assert!(lt.customized);

        // 只改优先级的那条，开关应保持默认（不被误置为 false）。
        let nw = out.iter().find(|p| p.key == "node_weight").unwrap();
        assert_eq!(nw.priority, 15);
        assert_eq!(
            nw.enabled,
            CATALOG
                .iter()
                .find(|d| d.key == "node_weight")
                .unwrap()
                .default_enabled
        );

        s.shutdown().await;
    }

    /// 未知键必须被拒（而不是静默忽略 —— 静默会让前端拼错 key 时"保存成功但没生效"）。
    #[tokio::test]
    async fn unknown_key_is_rejected() {
        let s = store().await;
        let e = save(
            &s,
            &[PolicyUpdate {
                key: "no_such_policy".into(),
                enabled: Some(true),
                priority: None,
            }],
        )
        .await
        .unwrap_err();
        assert!(matches!(e, PolicyError::UnknownKey(_)), "实际：{e:?}");
        s.shutdown().await;
    }

    /// 优先级越界必须 422。
    #[tokio::test]
    async fn priority_out_of_range_is_rejected() {
        let s = store().await;
        for bad in [0i64, -1, 1000, 99999] {
            let e = save(
                &s,
                &[PolicyUpdate {
                    key: "least_tasks".into(),
                    enabled: None,
                    priority: Some(bad),
                }],
            )
            .await
            .unwrap_err();
            assert!(
                matches!(e, PolicyError::PriorityOutOfRange { .. }),
                "值 {bad} 应被拒，实际：{e:?}"
            );
            assert_eq!(e.status(), StatusCode::UNPROCESSABLE_ENTITY);
        }
        s.shutdown().await;
    }

    /// 容量安全阀不允许关闭（C-05 的第三条）。
    #[tokio::test]
    async fn non_disableable_policy_cannot_be_disabled() {
        let s = store().await;
        let e = save(
            &s,
            &[PolicyUpdate {
                key: "max_concurrent".into(),
                enabled: Some(false),
                priority: None,
            }],
        )
        .await
        .unwrap_err();
        assert!(matches!(e, PolicyError::NotDisableable(_)), "实际：{e:?}");
        s.shutdown().await;
    }

    /// **半途失败不许留下一半改动**：一条合法 + 一条非法，整体必须不落库。
    #[tokio::test]
    async fn partial_batch_does_not_half_apply() {
        let s = store().await;
        let before = load(&s).await.unwrap();
        let e = save(
            &s,
            &[
                PolicyUpdate {
                    key: "least_tasks".into(),
                    enabled: Some(false),
                    priority: None,
                },
                PolicyUpdate {
                    key: "bogus".into(),
                    enabled: Some(true),
                    priority: None,
                },
            ],
        )
        .await
        .unwrap_err();
        assert!(matches!(e, PolicyError::UnknownKey(_)));
        let after = load(&s).await.unwrap();
        for (a, b) in after.iter().zip(&before) {
            assert_eq!(a.enabled, b.enabled, "{} 不应被改动", a.key);
            assert_eq!(a.priority, b.priority, "{} 不应被改动", a.key);
        }
        s.shutdown().await;
    }

    /// 库里存了坏 JSON 时，页面**不能挂**，应退回默认值。
    #[tokio::test]
    async fn corrupt_row_falls_back_to_default() {
        let s = store().await;
        s.execute(
            "INSERT INTO app_config (key, value, updated_at) VALUES ('policy.least_tasks', 'not json', 0)",
            vec![],
        )
        .await
        .unwrap();
        let v = load(&s).await.unwrap();
        let lt = v.iter().find(|p| p.key == "least_tasks").unwrap();
        assert_eq!(lt.enabled, true, "坏行应退回默认值");
        assert!(!lt.customized);
        s.shutdown().await;
    }
}
