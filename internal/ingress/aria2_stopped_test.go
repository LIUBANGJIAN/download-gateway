package ingress

import (
	"testing"

	"github.com/LIUBANGJIAN/download-gateway/internal/domain"
)

// nonStoppedStates 登记**不进** `tellStopped` 的内部状态，每条必须写清理由。
//
// 这份白名单配合 `TestStoppedStatesCoversEveryTerminalState` 使用：新增一个
// internal_state 后，要么进 `stoppedStates`、要么进这里并解释 —— 两者都不做就红。
// 这不是形式主义：漏登记的失败模式是**对客户端隐身**，而不是报错。
var nonStoppedStates = map[string]string{
	"queued":      "还在排队，客户端应在 tellWaiting 里看到",
	"dispatching": "正在下发，属进行中",
	"running":     "下载中，属进行中",
	"paused":      "aria2 的 tellStopped 不含 paused —— `paused` 在 tellWaiting 里",
	"stopped": "Rust 遗留值（裸 TEXT 时期写入）。v2 已把它拆成 paused / failed_terminal，" +
		"新代码不再写入；存量行会被回读改写，因此不必进 tellStopped",
}

// TestStoppedStatesCoversEveryTerminalState 是「终态对客户端可见」这条的守卫。
//
// 这一条守的是一次真实的静默漏单：`stoppedStates` 曾只有
// `{completed, failed, removed}`，而 B1 写的是 `failed_terminal` ——
// 每个重试耗尽的任务都用 `tellStatus` 查得到、用 `tellStopped` 查不到。
// 断言方向取「穷举 domain 的全部取值」，而不是「检查已知的 6 个」：
// 后者只能证明"我列出的都在"，永远抓不到"我漏了一个"。
func TestStoppedStatesCoversEveryTerminalState(t *testing.T) {
	inStopped := make(map[string]bool, len(stoppedStates))
	for _, s := range stoppedStates {
		if inStopped[s] {
			t.Fatalf("stoppedStates 里 %q 重复登记", s)
		}
		inStopped[s] = true
	}

	known := make(map[string]bool)
	for _, st := range domain.AllInternalStates() {
		name := string(st)
		known[name] = true
		switch {
		case inStopped[name]:
			// 已登记为终态：反向要求它对外看起来必须真的"停了"。
			if got := domain.DeriveAriaStatus(st, 1000, nil); got == domain.AriaActive ||
				got == domain.AriaWaiting {
				t.Fatalf("%q 登记在 stoppedStates，但对外派生为 %q —— "+
					"把进行中的状态塞进 tellStopped 会让客户端误以为任务已经结束",
					name, got)
			}
		default:
			if _, ok := nonStoppedStates[name]; !ok {
				t.Fatalf("内部状态 %q 既不在 stoppedStates、也没在 nonStoppedStates 登记理由。\n"+
					"漏登记的后果是这条状态的任务用 tellStopped / numStopped **查不到**（静默漏单），"+
					"请决定它是否终态并登记。", name)
			}
		}
	}

	// 反向：stoppedStates 里不许出现 domain 不认识的值（拼写错误的死条目）。
	for s := range inStopped {
		if !known[s] {
			t.Fatalf("stoppedStates 含 %q，它不是 domain 定义的内部状态（拼写错误？）", s)
		}
	}
	// 反向：白名单也不许陈旧。
	for s := range nonStoppedStates {
		if !known[s] {
			t.Fatalf("nonStoppedStates 含 %q，它不是 domain 定义的内部状态（白名单已陈旧）", s)
		}
	}
}

// TestTellStoppedReturnsRetryExhaustedTask 走一遍真实 HTTP：重试耗尽的任务
// 必须出现在 tellStopped 里，且 numStopped 计数 +1。
//
// 单测表格证明"表写对了"，这条证明"表真的被那条查询用到"——两者缺一都不够。
func TestTellStoppedReturnsRetryExhaustedTask(t *testing.T) {
	srv, st := newAriaEnv(t)
	taskID := seedTaskWithState(t, st, domain.StateFailedTerminal)

	got := jsonRPC(t, srv, `{"jsonrpc":"2.0","id":"1","method":"aria2.tellStopped","params":["token:`+testToken+`",0,10]}`)
	list, ok := got["result"].([]any)
	if !ok {
		t.Fatalf("tellStopped 应返回数组，实际 %v", got)
	}
	if len(list) != 1 {
		t.Fatalf("tellStopped 应返回 1 条（failed_terminal 是终态），实际 %d 条：%v", len(list), list)
	}
	first, _ := list[0].(map[string]any)
	if first["status"] != "error" {
		t.Fatalf("failed_terminal 对外的 status 应为 error，实际 %v", first["status"])
	}
	if gid, _ := first["gid"].(string); gid == "" {
		t.Fatalf("tellStopped 的每一行都必须带 gid（客户端靠它回查），实际 %v", first)
	}

	// numStopped 也要算进去 —— 客户端常用它判断"全部下完了没有"。
	got = jsonRPC(t, srv, `{"jsonrpc":"2.0","id":"2","method":"aria2.getGlobalStat","params":["token:`+testToken+`"]}`)
	stat, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("getGlobalStat 应返回对象，实际 %v", got)
	}
	if stat["numStopped"] != "1" {
		t.Fatalf("numStopped 应为 \"1\"（failed_terminal 已停），实际 %v（任务 %s）", stat["numStopped"], taskID)
	}
}
