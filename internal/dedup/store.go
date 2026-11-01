package dedup

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/store"
)

// Hit 是一次查重命中的结果。
type Hit struct {
	// Key 命中的那条键（判"能不能跳过"要看它）。
	Key Key
	// TaskID 既有任务的 id（`dedup_index.task_id`）。
	TaskID string
	// NodeID 既有任务所在的节点（可为 nil）。
	NodeID *int64
	// State 既有任务的 `internal_state`（空串表示任务行已不存在）。
	State string
}

// Alive 表示既有任务是否"仍然算数"。
//
// 判据（`design/02 §2.2.3` 的默认策略表末行）：命中但既有任务已 `removed` /
// `failed_terminal` ⇒ **允许重建**。不放行会导致"这个文件再也下不了"——
// 用户删掉一个失败任务后重新推送，系统告诉他"已经有了"。
func (h *Hit) Alive() bool {
	if h == nil || h.TaskID == "" {
		return false
	}
	switch h.State {
	case "removed", "failed_terminal", "failed":
		return false
	}
	return true
}

// Lookup 按强度顺序查 `dedup_index`，返回**第一个存活命中**。
//
// `selfTaskID` 必须传本任务 id：否则重试的任务会命中自己上次写下的键，
// 被自己判成重复 —— 那会让所有可重试失败都变成"假去重"，任务再也不投递。
//
// 返回值：`(nil, nil)` 表示未命中（调用方照常投递并落账）。
func Lookup(ctx context.Context, st *store.Store, keys []Key, selfTaskID string) (*Hit, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	// 键最多 4 条，逐个查比拼 IN 更好读，也避免参数个数随层级变化。
	for _, k := range keys {
		rows, err := st.Query(ctx,
			`SELECT d.task_id, d.node_id, COALESCE(t.internal_state, '')
			   FROM dedup_index d
			   LEFT JOIN task t ON t.task_id = d.task_id
			  WHERE d.dedup_key = ?1`, k.Value)
		if err != nil {
			return nil, fmt.Errorf("dedup: 查重: %w", err)
		}
		if len(rows) == 0 {
			continue
		}
		r := rows[0]
		h := &Hit{Key: k}
		if s, ok := r[0].(string); ok {
			h.TaskID = s
		}
		if v, ok := r[1].(int64); ok {
			h.NodeID = &v
		}
		if s, ok := r[2].(string); ok {
			h.State = s
		}
		if h.TaskID == selfTaskID {
			continue // 命中自己 ⇒ 不算重复
		}
		if h.Alive() {
			return h, nil
		}
		// 死键：继续往下找（可能还有别的层级命中一个活着的任务）。
	}
	return nil, nil
}

// Record 把一条键落账（幂等：`dedup_key` 是主键，冲突即忽略）。
//
// 为什么不覆盖：一条键已经指向任务 A，任务 B 又来写它，说明**判重漏了**。
// 这时保留 A（先到者）比改成 B 更符合"事实"——B 本来就不该被投递。
func Record(ctx context.Context, st *store.Store, k Key, taskID string, nodeID *int64) error {
	_, err := st.Execute(ctx,
		`INSERT INTO dedup_index (dedup_key, level, confidence, task_id, node_id, created_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, strftime('%s','now'))
		 ON CONFLICT(dedup_key) DO NOTHING`,
		k.Value, int(k.Level), string(k.Conf), nullIfEmpty(taskID), optI64(nodeID),
	)
	if err != nil {
		return fmt.Errorf("dedup: 落账 %s: %w", k.Value, err)
	}
	return nil
}

// TxRecord 在调用方的事务里落账（调度事务用，保证"投递意图"与"去重账"原子）。
func TxRecord(ctx context.Context, tx *sql.Tx, k Key, taskID string, nodeID int64, now int64) error {
	if now == 0 {
		now = time.Now().Unix()
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO dedup_index (dedup_key, level, confidence, task_id, node_id, created_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6)
		 ON CONFLICT(dedup_key) DO NOTHING`,
		k.Value, int(k.Level), string(k.Conf), taskID, nodeID, now,
	)
	if err != nil {
		return fmt.Errorf("dedup: 事务内落账 %s: %w", k.Value, err)
	}
	return nil
}

// Summary 把键列表压成一行字符串（写日志 / `event_log.detail` 用）。
func Summary(keys []Key) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k.Rule+"="+k.Value)
	}
	return strings.Join(parts, "; ")
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func optI64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
