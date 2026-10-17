package protocol

import (
	"context"
	"fmt"
	"strings"
)

// RemoteTask 是节点任务列表里的一行（**只取调度需要的字段**）。
//
// 设计依据：`design/02 §4.1.6` 的实测结论——BitComet 只上报
// `running`/`stopped` 两态 + `permillage`，所以不要指望有更细的状态机。
//
// 字段名做**兼容读取**（`state`/`status`、`download_rate`/`download_speed`）：
// 节点版本异构在 `design/00B` 里已被实测证实（跨两个小版本行为不同），
// 硬编码单一键名会在某个版本上静默取到零值 —— 而"进度永远是 0"极难被发现。
type RemoteTask struct {
	TaskID     string
	GUID       string
	Name       string
	State      string
	Permillage int64
	TotalSize  int64
	Downloaded int64
	Rate       float64
	ErrorCode  string
}

// TaskListParams 是 `task_list/get` 的请求参数。
type TaskListParams struct {
	StateGroup string // ALL / ACTIVE / UNCOMPLETED / ...
	Limit      int
	Start      int
}

// FetchTaskListParams 按给定参数拉一页任务列表（**扁平信封**：`tasks` 在顶层）。
//
// 为什么不用 [Client.FetchTaskList]：那个方法固定发 `{}`，拿到的是节点默认
// 分页（可能只有 50 条）。回读同步必须能指定 `state_group` 与 `limit`，
// 否则节点上任务一多就会**静默漏掉**后面的行。
func FetchTaskListParams(ctx context.Context, c *Client, p TaskListParams) (map[string]any, error) {
	if err := c.EnsureLogin(ctx); err != nil {
		return nil, err
	}
	body := map[string]any{
		"state_group": p.StateGroup,
		"task_type":   "ALL",
		"tag_filter":  "ALL",
		"sort_key":    "Name",
		"sort_order":  "ascend",
		"keyword":     "",
	}
	if p.StateGroup == "" {
		body["state_group"] = "ALL"
	}
	if p.Start > 0 {
		body["start"] = p.Start
	}
	if p.Limit > 0 {
		body["limit"] = p.Limit
	}
	return c.Call(ctx, "/api_v2/task_list/get", body)
}

// ParseTaskList 把响应解析成 [RemoteTask] 切片。
//
// 容错原则：**宁可少一行，也不要因为一行畸形而整批失败**——
// 一个任务的 `permillage` 是字符串不该让另外 199 个任务的进度都不同步。
// 但"少的那一行"不算成功：调用方会拿它的 node_task_id 去比对，
// 找不到就按 `silently_not_created` 处理。
func ParseTaskList(resp map[string]any) []RemoteTask {
	raw, ok := resp["tasks"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]RemoteTask, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := firstString(m, "task_id", "taskid", "id")
		if id == "" {
			continue
		}
		out = append(out, RemoteTask{
			TaskID:     id,
			GUID:       firstString(m, "task_guid", "guid"),
			Name:       firstString(m, "name", "task_name"),
			State:      firstString(m, "state", "status"),
			Permillage: clampPermillage(firstInt(m, "permillage", "progress", "permil")),
			TotalSize:  firstInt(m, "total_size", "totalSize", "selected_size"),
			Downloaded: firstInt(m, "download_size", "downloaded_size", "downloaded"),
			Rate:       float64(firstInt(m, "download_rate", "download_speed", "speed")),
			ErrorCode:  firstString(m, "error_code", "errorCode"),
		})
	}
	return out
}

// FindByName 在列表里按任务名找（read-after-write 的兜底匹配）。
//
// 为什么要名字兜底：`torrent_links/add` 是**异步**接口，某些版本不返回
// `task_id`（`design/02 §2.1.4` 的"成功但未见 id"分支正是为此而写）。
// 只按 id 匹配会把这种正常情况误判成 `silently_not_created` 并触发重投，
// 结果是同一部剧被下两遍。
func FindByName(tasks []RemoteTask, name string) (RemoteTask, bool) {
	want := strings.TrimSpace(name)
	if want == "" {
		return RemoteTask{}, false
	}
	for _, t := range tasks {
		if strings.TrimSpace(t.Name) == want {
			return t, true
		}
	}
	return RemoteTask{}, false
}

// FindByID 按节点任务 id 精确匹配。
func FindByID(tasks []RemoteTask, id string) (RemoteTask, bool) {
	want := strings.TrimSpace(id)
	if want == "" {
		return RemoteTask{}, false
	}
	for _, t := range tasks {
		if strings.TrimSpace(t.TaskID) == want {
			return t, true
		}
	}
	return RemoteTask{}, false
}

// firstString 取第一个存在且非空白的字符串键（兼容数字型 id）。
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case string:
			if strings.TrimSpace(x) != "" {
				return x
			}
		case float64:
			return formatInt(int64(x))
		case int64:
			return formatInt(x)
		case int:
			return formatInt(int64(x))
		}
	}
	return ""
}

// firstInt 取第一个存在且可解析的整数键（兼容数字型字符串）。
func firstInt(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case float64:
			return int64(x)
		case int64:
			return x
		case int:
			return int64(x)
		case string:
			var n int64
			if _, err := fmt.Sscan(strings.TrimSpace(x), &n); err == nil {
				return n
			}
		}
	}
	return 0
}

// formatInt 无依赖地把 int64 转十进制（避免为一行引入 strconv 的导入噪音）。
func formatInt(n int64) string {
	return fmt.Sprintf("%d", n)
}

// clampPermillage 把进度夹到 DDL 的 [0,1000] 区间。
//
// 必须夹：`task.permillage` 有 `CHECK (permillage BETWEEN 0 AND 1000)`，
// 节点若报 100（百分制）或 1001（浮点误差），原样写库会**直接违反约束**，
// 让一次正常的进度同步变成一条 SQL 错误。
func clampPermillage(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > 1000 {
		return 1000
	}
	return v
}
