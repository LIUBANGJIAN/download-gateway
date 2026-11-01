// Command apicheck 逐字段试探 BitComet 的 `task_list/get` 参数兼容性。
//
// 背景：`internal/protocol.FetchTaskListParams` 发的那套参数在实测节点上返回
// `FATAL_ERROR: invalid task query`。参数是照旧版文档写的，新版明显收紧了校验。
// 与其猜哪个字段不被接受，不如**逐个做减法**：从完整参数集开始，一次去掉一个，
// 看哪一个去掉之后就好了。
//
// 用法：
//
//	go run ./cmd/apicheck <base_url> <user> <password>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/LIUBANGJIAN/download-gateway/internal/protocol"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "用法: apicheck <base_url> <user> <password>")
		os.Exit(2)
	}
	baseURL, user, pass := os.Args[1], os.Args[2], os.Args[3]

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cl, err := protocol.NewClient(protocol.NewNodeProfile(baseURL, user, pass), 15)
	if err != nil {
		fmt.Println("构造客户端失败:", err)
		os.Exit(1)
	}
	if err := cl.Login(ctx); err != nil {
		fmt.Println("登录失败:", err)
		os.Exit(1)
	}
	fmt.Println("登录 OK:", baseURL)

	// 每个用例给出完整参数集，逐个做减法找出「多出来的那一个」。
	cases := []struct {
		name string
		body map[string]any
	}{
		{"完整（当前实现）", map[string]any{
			"state_group": "ALL", "task_type": "ALL", "tag_filter": "ALL",
			"sort_key": "Name", "sort_order": "ascend", "keyword": "",
		}},
		{"去掉 tag_filter", map[string]any{
			"state_group": "ALL", "task_type": "ALL",
			"sort_key": "Name", "sort_order": "ascend", "keyword": "",
		}},
		{"去掉 sort_key/sort_order", map[string]any{
			"state_group": "ALL", "task_type": "ALL", "tag_filter": "ALL", "keyword": "",
		}},
		{"去掉 keyword", map[string]any{
			"state_group": "ALL", "task_type": "ALL", "tag_filter": "ALL",
			"sort_key": "Name", "sort_order": "ascend",
		}},
		{"只留 state_group", map[string]any{"state_group": "ALL"}},
		{"空对象", map[string]any{}},
		{"state_group=ACTIVE", map[string]any{"state_group": "ACTIVE"}},
		{"带 limit:50", map[string]any{"state_group": "ALL", "limit": 50}},
		{"带 limit:10", map[string]any{"state_group": "ALL", "limit": 10}},
		{"带 start:0", map[string]any{"state_group": "ALL", "start": 0}},
		{"带 max:50", map[string]any{"state_group": "ALL", "max": 50}},
		{"带 page_size:50", map[string]any{"state_group": "ALL", "page_size": 50}},
		{"带 count:50", map[string]any{"state_group": "ALL", "count": 50}},
		{"state_group 小写 all", map[string]any{"state_group": "all"}},
		{"task_type 小写", map[string]any{"state_group": "ALL", "task_type": "all"}},
		{"sort_key 小写 name", map[string]any{
			"state_group": "ALL", "task_type": "ALL", "tag_filter": "ALL",
			"sort_key": "name", "sort_order": "ascend",
		}},
	}

	checkParse(ctx, cl)

	for _, c := range cases {
		resp, err := cl.Call(ctx, "/api_v2/task_list/get", c.body)
		if err != nil {
			fmt.Printf("  [FAIL] %-26s %v\n", c.name, trim(err.Error()))
			continue
		}
		n := 0
		if arr, ok := resp["tasks"].([]any); ok {
			n = len(arr)
		}
		fmt.Printf("  [OK]   %-26s 返回 %d 个任务\n", c.name, n)
		if n > 0 {
			b, _ := json.MarshalIndent(resp["tasks"].([]any)[0], "         ", "  ")
			fmt.Printf("         首条样本:\n%s\n", string(b))
		}
	}
}

// checkParse 复现回读链路：FetchTaskListParams -> ParseTaskList，打印解析结果。
//
// 单独一段是因为"节点返回了任务"和"我们解析出了任务"是两件事：
// 前者看 JSON 就知道，后者要跑过解析层才看得出来。实测就是在这里丢的。
func checkParse(ctx context.Context, cl *protocol.Client) {
	fmt.Println()
	fmt.Println("=== 回读链路（FetchTaskListParams -> ParseTaskList）===")
	resp, err := protocol.FetchTaskListParams(ctx, cl, protocol.TaskListParams{StateGroup: "ALL", Limit: 50})
	if err != nil {
		fmt.Println("  拉取失败:", err)
		return
	}
	raw, _ := resp["tasks"].([]any)
	fmt.Printf("  原始 tasks 条数: %d\n", len(raw))
	tasks := protocol.ParseTaskList(resp)
	fmt.Printf("  解析后条数: %d\n", len(tasks))
	for _, t := range tasks {
		fmt.Printf("    id=%-8s name=%-28s state=%-9s permil=%-4d dl=%-12d total=%-12d err=%s\n",
			t.TaskID, trunc(t.Name, 28), t.State, t.Permillage, t.Downloaded, t.TotalSize, t.ErrorCode)
	}
	if len(raw) > 0 && len(tasks) == 0 {
		fmt.Println("  !! 原始有数据但解析为空 —— 解析层丢行")
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func trim(s string) string {
	if len(s) > 140 {
		return s[:140] + "..."
	}
	return s
}
