package engine

// 本文件是 B2「绑定失效与重绑」的**执行层**：把 [scheduler.BindingResolution] 落成库里的
// `task_group` 状态迁移 + `group_binding_log` 留痕 + `event_log` 告警。
//
// # 为什么 split 必须独立成一个事务、且在选点之前
//
// 拆组的意义是"让本组后续成员改走评分"，而评分是否发生取决于 `group_state`。
// 如果把它推迟到建绑事务里（和投递意图一起提交），那么**本次**的决策仍然基于
// 旧的 `active` 状态 —— 于是要么这集白排一次队，要么选点和落库的语义对不上。
//
// # 为什么不用 UPDATE ... WHERE bound_node_id = ? 之类的乐观条件就完事
//
// 条件更新能防重复写日志，但防不住"两个 tick 同时判定 split 并各自写一条"。
// 这里用 `group_state <> 'split'` 做条件：只有真正把状态翻过去的那一个赢家写日志，
// 与 B1 建绑用 `bound_node_id IS NULL` 做赢家判定的做法一致。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/LIUBANGJIAN/download-gateway/internal/dispatch"
	"github.com/LIUBANGJIAN/download-gateway/internal/grouping"
	"github.com/LIUBANGJIAN/download-gateway/internal/scheduler"
)

// applySplit 执行一次拆组：`group_state→split`、`binding_epoch+1`、写 `group_binding_log`。
//
// 拆组**不动** `bound_node_id`：它保留"原节点"这个事实，供 [scheduler.ResolveBinding]
// 在节点恢复后判定 rejoin。把它清空会让 rejoin 失去目标。
//
// 拆组**不动**任何 `task` 行：已落点的成员留在原节点、继续下载。
// 这条是 B2-AC2 的硬要求（"已下载内容不被删除"）—— 迁移等于重新下载，
// 而 BitComet 不支持下载中热迁移（设计 §6 第 ⑦ 条）。
func (e *Engine) applySplit(ctx context.Context, res scheduler.BindingResolution, g *scheduler.GroupRef, now int64) error {
	if g == nil || g.GroupID <= 0 {
		return nil
	}
	var epoch int64
	err := e.st.InTx(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		up, err := tx.ExecContext(txCtx,
			`UPDATE task_group
			    SET group_state = 'split',
			        split_at = ?1,
			        split_reason = ?2,
			        binding_epoch = binding_epoch + 1,
			        wait_deadline = NULL,
			        updated_at = ?1
			  WHERE group_id = ?3 AND group_state <> 'split'`,
			now, res.Reason, g.GroupID,
		)
		if err != nil {
			return fmt.Errorf("engine: 拆组: %w", err)
		}
		if n, _ := up.RowsAffected(); n == 0 {
			// 已经被别的 tick 拆过：读回当前 epoch，不再写第二条日志。
			return tx.QueryRowContext(txCtx,
				`SELECT binding_epoch FROM task_group WHERE group_id = ?1`, g.GroupID).Scan(&epoch)
		}
		if err := tx.QueryRowContext(txCtx,
			`SELECT binding_epoch FROM task_group WHERE group_id = ?1`, g.GroupID).Scan(&epoch); err != nil {
			return fmt.Errorf("engine: 读绑定代数: %w", err)
		}
		detail := mustJSON(map[string]any{
			"reason":         res.Reason,
			"kept_node":      g.BoundNodeID,
			"binding_policy": g.BindingPolicy,
			"split_policy":   g.SplitPolicy,
		})
		if _, err := tx.ExecContext(txCtx,
			`INSERT INTO group_binding_log
			   (group_id, binding_epoch, from_node_id, to_node_id, action, reason, detail, at)
			 VALUES (?1, ?2, ?3, NULL, 'split', ?4, ?5, ?6)`,
			g.GroupID, epoch, optI64(g.BoundNodeID), res.Reason, detail, now,
		); err != nil {
			return fmt.Errorf("engine: 写拆组日志: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 内存视图同步：本次调用后续的选点要立刻看到 split，不能等下一轮读库。
	g.GroupState = scheduler.GroupStateSplit
	g.BindingEpoch = epoch
	g.WaitDeadline = 0

	level := "warn"
	if res.Action != scheduler.BindSplit {
		level = "info"
	}
	dispatch.LogEvent(ctx, e.st, level, "group", 0, "",
		fmt.Sprintf("组 #%d 拆组（%s）：%s", g.GroupID, res.Reason, res.Note))
	e.log.Info("组已拆组", "group", g.GroupID, "reason", res.Reason, "epoch", epoch)
	return nil
}

// rejoinAction 把"追平"结论转成 [grouping.GroupAction]。
//
// 只在组已 split、原绑定节点恢复时调用；首次建绑（`GroupAction==nil`）不会走到这里。
func rejoinAction(res scheduler.BindingResolution, g *scheduler.GroupRef, nodeID int64) *grouping.GroupAction {
	if g == nil || g.BoundNodeID == nil {
		return nil
	}
	return &grouping.GroupAction{
		Action:     "rejoin",
		Reason:     res.Reason,
		FromNodeID: g.BoundNodeID,
		ToNodeID:   nodeID,
	}
}

// mustJSON 序列化 detail（失败返回 "{}"：留痕字段坏掉不该让业务事务回滚）。
func mustJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// optI64 把 `*int64` 映射为 SQL 参数（nil ⇒ NULL）。
func optI64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
