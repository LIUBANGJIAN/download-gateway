package engine

// 本文件是 B3「跨来源去重」的**执行层**：判重 → 分流（跳过 / 标记待裁决 / 正常投递）。
//
// 三个出口对应 `design/02 §2.2.3` 的默认策略表，一条都不能合并：
//
//	exact/high 命中且既有任务存活 ⇒ `deduplicated`（终态，返回既有 task_id）
//	medium 命中                 ⇒ 不投递 + `possible_duplicate=1`（等人工裁决）
//	未命中                      ⇒ 正常投递，并在建绑事务里把键落账
//
// 「中置信绝不跳过」是最容易被"优化"掉的一条：跳过看起来更省事，
// 但漏掉一个真实任务不可挽回，重复下载只是浪费带宽（Q4 的原始裁定）。

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/LIUBANGJIAN/download-gateway/internal/dedup"
	"github.com/LIUBANGJIAN/download-gateway/internal/dispatch"
	"github.com/LIUBANGJIAN/download-gateway/internal/grouping"
)

// dedupKeys 计算一个待派发任务的全部去重键。
func dedupKeys(t dispatch.QueuedTask) []dedup.Key {
	return dedup.Keys(dedup.Input{
		Kind:      t.Kind,
		URLRaw:    t.URLRaw,
		Name:      deref(t.Name),
		Referrer:  deref(t.SourceKey),
		TotalSize: t.TotalSize,
	})
}

// decideDedup 做判重并（命中时）落库。返回 true 表示"本任务不要再投递了"。
//
// 注意 `Lookup` 传了 `t.TaskID`：重试的任务会命中自己上次写的键，
// 不排除自己会让所有可重试失败都变成"假去重"。
func (e *Engine) decideDedup(ctx context.Context, t dispatch.QueuedTask, keys []dedup.Key, now int64) (bool, error) {
	hit, err := dedup.Lookup(ctx, e.st, keys, t.TaskID)
	if err != nil {
		return false, err
	}
	if hit == nil {
		return false, nil
	}

	if hit.Key.Skippable() {
		return true, e.markDeduplicated(ctx, t.TaskID, hit, now)
	}
	return true, e.markPossibleDuplicate(ctx, t.TaskID, hit, now)
}

// markDeduplicated 把命中 exact/high 的任务置为 `deduplicated`（终态，不投递）。
//
// 三件事必须在**同一事务**里发生：任务状态、`dispatch_log(decision='deduped')`、
// `event_log`。少任何一件，"为什么这集没下载"就答不上来。
func (e *Engine) markDeduplicated(ctx context.Context, taskID string, hit *dedup.Hit, now int64) error {
	reason := fmt.Sprintf("dedup_hit_%s", hit.Key.Rule)
	err := e.st.InTx(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(txCtx,
			`UPDATE task
			    SET internal_state = 'deduplicated',
			        aria_status = 'removed',
			        dispatch_reason = NULL,
			        updated_at = ?1
			  WHERE task_id = ?2 AND internal_state = 'queued'`,
			now, taskID,
		); err != nil {
			return fmt.Errorf("engine: 标记去重: %w", err)
		}
		if _, err := tx.ExecContext(txCtx,
			`INSERT INTO dispatch_log
			   (task_id, group_id, chosen_node, candidates, score_json, decision, reason, created_at)
			 VALUES (?1, NULL, ?2, 0, ?3, 'deduped', ?4, ?5)`,
			taskID, optI64(hit.NodeID),
			mustJSON(map[string]any{
				"dedup_key": hit.Key.Value, "rule": hit.Key.Rule,
				"confidence": string(hit.Key.Conf), "existing_task": hit.TaskID,
			}),
			reason, now,
		); err != nil {
			return fmt.Errorf("engine: 写去重决策: %w", err)
		}
		if _, err := tx.ExecContext(txCtx,
			`INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at)
			 VALUES ('info', 'dedup', ?1, ?2, ?3, ?4, ?5)`,
			optI64(hit.NodeID), taskID,
			fmt.Sprintf("去重命中（%s/%s），跳过投递；已有任务 %s 覆盖同一内容",
				hit.Key.Rule, hit.Key.Conf, hit.TaskID),
			hit.Key.Value, now,
		); err != nil {
			return fmt.Errorf("engine: 写去重事件: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	e.log.Info("去重命中，跳过投递",
		"task", taskID, "rule", hit.Key.Rule, "existing", hit.TaskID)
	return nil
}

// markPossibleDuplicate 把中置信命中的任务标为"待裁决"并**不投递**。
//
// 为什么用 `possible_duplicate=1` 而不是直接丢弃：
//   - 丢弃 = 用户永远不知道有这么个任务（漏任务不可挽回）；
//   - 直接下载 = 静默重复（浪费带宽，且可能与既有任务抢同一落盘路径）。
//
// 标记后 `LoadQueuedTasks` 会跳过它（见那里的 `possible_duplicate = 0`），
// 直到用户在管理台裁决；裁决把标记清 0，它就重新进入调度。
func (e *Engine) markPossibleDuplicate(ctx context.Context, taskID string, hit *dedup.Hit, now int64) error {
	reason := fmt.Sprintf("possible_duplicate_pending_review_%s", hit.Key.Rule)
	err := e.st.InTx(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(txCtx,
			`UPDATE task
			    SET possible_duplicate = 1,
			        dispatch_reason = 'affinity_wait',
			        updated_at = ?1
			  WHERE task_id = ?2 AND internal_state = 'queued'`,
			now, taskID,
		); err != nil {
			return fmt.Errorf("engine: 标记疑似重复: %w", err)
		}
		if _, err := tx.ExecContext(txCtx,
			`INSERT INTO dispatch_log
			   (task_id, group_id, chosen_node, candidates, score_json, decision, reason, created_at)
			 VALUES (?1, NULL, NULL, 0, ?2, 'queued', ?3, ?4)`,
			taskID,
			mustJSON(map[string]any{
				"dedup_key": hit.Key.Value, "rule": hit.Key.Rule,
				"confidence": string(hit.Key.Conf), "existing_task": hit.TaskID,
			}),
			reason, now,
		); err != nil {
			return fmt.Errorf("engine: 写疑似重复决策: %w", err)
		}
		if _, err := tx.ExecContext(txCtx,
			`INSERT INTO event_log (level, category, node_id, task_id, message, detail, created_at)
			 VALUES ('warn', 'dedup', NULL, ?1, ?2, ?3, ?4)`,
			taskID,
			fmt.Sprintf("疑似重复（%s/%s）：暂不投递，请在管理台裁决（既有任务 %s）",
				hit.Key.Rule, hit.Key.Conf, hit.TaskID),
			hit.Key.Value, now,
		); err != nil {
			return fmt.Errorf("engine: 写疑似重复事件: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	e.log.Warn("疑似重复，转人工裁决",
		"task", taskID, "rule", hit.Key.Rule, "existing", hit.TaskID)
	return nil
}

// toGroupingDedupKeys 把 dedup.Key 转成建绑事务要的形状。
func toGroupingDedupKeys(keys []dedup.Key) []grouping.DedupKey {
	if len(keys) == 0 {
		return nil
	}
	out := make([]grouping.DedupKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, grouping.DedupKey{
			Value: k.Value, Level: int(k.Level), Conf: string(k.Conf),
		})
	}
	return out
}
