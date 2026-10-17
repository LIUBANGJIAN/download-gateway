package domain

import (
	"bufio"
	"os"
	"sort"
	"strings"
	"testing"
)

// notYetLanded 是「冻结表里有、Go 枚举里还没有」的**显式白名单**。
//
// 为什么需要白名单而不是直接要求两边完全相等：B1 只落地 `failed_terminal`，
// 其余 4 个值分别归 B2（`orphaned`）、B3（`deduplicated`）与后续（`missing_on_node`/`external`）。
// 但**白名单必须是显式的**：否则「扩值域时随手造一个值」会让这条断言依然全绿，
// 那它就白写了（R-6 的原始缺口正是这个形态）。
var notYetLanded = map[InternalState]bool{
	"orphaned":        true, // B2：绑定失效后的孤儿任务
	"deduplicated":    true, // B3：跨来源去重命中
	"missing_on_node": true, // 后续：节点可达但任务已不在
	"external":        true, // 后续：节点上外部创建的任务
}

// legacyStates 是 Rust 时代遗留、且**不在** design/02 11 值表里的值。
//
//   - `failed`：`crates/dispatch-core/src/tasks/create.rs` 与 `admin/rest.rs` 都在用；
//     B1 的写路径不再产生它，但读取侧必须继续认识（否则存量库里的失败任务会变空白）。
//   - `stopped`：Rust 的 `aria_status` match 里有这个分支，而 design/02 的 v2 表
//     已把它**拆解掉**（`stopped` 双义在反解阶段就分成了 completed/paused/failed_terminal，
//     内部状态不再有 `stopped`）。保留它是因为 `DeriveAriaStatus` 的既有分支仍被
//     存量数据引用。
var legacyStates = map[InternalState]bool{
	StateFailed:  true,
	StateStopped: true,
}

// TestInternalStateMatchesFrozenSet 把 `design/02 §3.4` 的权威值集与 Go 枚举逐值比对。
//
// 它守的是 R-6 指出的缺口：`ddl_contract_test.go` **主动放弃**对 `internal_state`
// 的比对（该列故意没有 CHECK），而 `allInternalStates` 的旧注释把 **Rust 实现**当权威
// ⇒ 权威需求源的值集**没有任何断言在守**，扩值域时可以随手造值而全套测试全绿。
//
// 判据（三边，缺一不可）：
//  1. Go 枚举 ⊆ 冻结表 ∪ {`failed` 这一个 Rust 遗留值}；
//  2. 冻结表 − Go 枚举 ⊆ [notYetLanded]（未落地的值必须登记，不能默默少）；
//  3. 冻结表本身没有重复行（防止"用重复行凑数"）。
func TestInternalStateMatchesFrozenSet(t *testing.T) {
	frozen, err := readFrozenStates("testdata/internal_state.txt")
	if err != nil {
		t.Fatalf("读取冻结值集失败: %v", err)
	}
	if len(frozen) != 11 {
		t.Fatalf("冻结值集应为 design/02 §3.4 的 11 值，实得 %d：%v", len(frozen), frozen)
	}

	inFrozen := make(map[InternalState]bool, len(frozen))
	for _, s := range frozen {
		if inFrozen[s] {
			t.Errorf("冻结值集有重复行: %q", s)
		}
		inFrozen[s] = true
	}

	// 方向 1：Go 枚举里不许出现"既不在冻结表、也不是已登记的 Rust 遗留值"的成员。
	inGo := make(map[InternalState]bool, len(allInternalStates))
	for _, s := range allInternalStates {
		if inGo[s] {
			t.Errorf("allInternalStates 有重复成员: %q", s)
		}
		inGo[s] = true
		if inFrozen[s] || legacyStates[s] {
			continue
		}
		t.Errorf("Go 枚举出现冻结表之外的值 %q —— 新增状态必须先在 design/02 §3.4 登记并写进 testdata", s)
	}

	// 方向 2：冻结表里尚未落地的值，必须落在显式白名单内。
	for _, s := range frozen {
		if !inGo[s] && !notYetLanded[s] {
			t.Errorf("冻结表里的 %q 既未落地、也不在 notYetLanded 白名单 —— 要么实现它，要么显式登记欠账", s)
		}
	}

	// 方向 3：B1 声称落地的值必须真在枚举里（防止"改了 testdata 忘了改代码"）。
	if !inGo[StateFailedTerminal] {
		t.Error("failed_terminal 未进 allInternalStates —— B1 的终态失败会写不进库")
	}
}

// TestNotYetLandedIsNotVacuouslyStale 防止白名单变成"永远为真"的死表：
// 某个值一旦落地，它必须从白名单里消失。否则白名单会掩盖「以为没做、其实做了」的漂移。
func TestNotYetLandedIsNotVacuouslyStale(t *testing.T) {
	for s := range notYetLanded {
		for _, got := range allInternalStates {
			if got == s {
				t.Errorf("%q 已进 Go 枚举，应从 notYetLanded 白名单移除（否则断言 2 对它失效）", s)
			}
		}
	}
}

// TestFrozenStatesAreSortedAndUnique 是一条廉价的一致性检查：文件本身要可读、可 diff。
func TestFrozenStatesAreSortedAndUnique(t *testing.T) {
	frozen, err := readFrozenStates("testdata/internal_state.txt")
	if err != nil {
		t.Fatalf("读取冻结值集失败: %v", err)
	}
	sorted := make([]string, len(frozen))
	for i, s := range frozen {
		sorted[i] = string(s)
	}
	if !sort.StringsAreSorted(sorted) {
		t.Errorf("testdata/internal_state.txt 未按字典序排列，diff 会变噪音：%v", sorted)
	}
}

// readFrozenStates 读冻结值集（一行一值；允许空行与 `#` 注释）。
func readFrozenStates(path string) ([]InternalState, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []InternalState
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, InternalState(line))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
