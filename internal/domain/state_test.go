package domain

import "testing"

// ---------------------------------------------------------------------------
// 与 Rust 侧 `crates/dispatch-core/src/tasks/state.rs` 的 `mod tests` 逐条对照
// ---------------------------------------------------------------------------

// 对照 `full_internal_state_table_maps_deterministically`。
func TestFullInternalStateTableMapsDeterministically(t *testing.T) {
	cases := []struct {
		name   string
		state  InternalState
		permil int64
		errCls *ErrorClass
		want   AriaStatus
	}{
		{"queued→waiting", StateQueued, 0, nil, AriaWaiting},
		{"dispatching→waiting", StateDispatching, 0, nil, AriaWaiting},
		{"running→active", StateRunning, 100, nil, AriaActive},
		{"paused→paused", StatePaused, 0, errPtr(ErrNotStarted), AriaPaused},
		{"completed→complete", StateCompleted, 1000, nil, AriaComplete},
		{"failed→error", StateFailed, 0, errPtr(ErrNodeDown), AriaError},
		{"removed→removed", StateRemoved, 0, nil, AriaRemoved},
		{"未知状态→waiting", InternalState("unknown-thing"), 0, nil, AriaWaiting},
	}
	for _, c := range cases {
		if got := DeriveAriaStatus(c.state, c.permil, c.errCls); got != c.want {
			t.Errorf("%s：DeriveAriaStatus(%q,%d,%v) = %q，期望 %q",
				c.name, c.state, c.permil, derefErr(c.errCls), got, c.want)
		}
	}
}

// 对照 `stopped_is_double_meaning`：stopped 一个值要能反解出三种对外状态。
func TestStoppedIsDoubleMeaning(t *testing.T) {
	cases := []struct {
		name   string
		permil int64
		errCls *ErrorClass
		want   AriaStatus
	}{
		{"已暂停", 0, nil, AriaPaused},
		{"未点开始", 0, errPtr(ErrNotStarted), AriaPaused},
		{"出错", 0, errPtr(ErrServerFileNotFound), AriaError},
		{"已完成", 1000, nil, AriaComplete},
	}
	for _, c := range cases {
		if got := DeriveAriaStatus(StateStopped, c.permil, c.errCls); got != c.want {
			t.Errorf("%s：stopped(permillage=%d, error_class=%v) = %q，期望 %q",
				c.name, c.permil, derefErr(c.errCls), got, c.want)
		}
	}
}

// 对照 `running_with_full_permillage_is_active`。
//
// 这条看着多余，其实是在钉「permillage 不该影响 running 分支」——
// 防止有人把 stopped 的完成度判据顺手复制到 running 上。
func TestRunningWithFullPermillageIsStillActive(t *testing.T) {
	if got := DeriveAriaStatus(StateRunning, 1000, nil); got != AriaActive {
		t.Fatalf("running + 1000‰ 仍应为 active，实际 %q", got)
	}
}

// 对照 `view_sets_error_code_only_for_real_errors`。
func TestViewSetsErrorCodeOnlyForRealErrors(t *testing.T) {
	row := sampleRow()
	row.InternalState = StateFailed
	row.AriaStatus = string(AriaError)
	row.ErrorClass = errPtr(ErrNodeDown)
	row.ErrorMessage = strPtr("节点离线")

	v := AriaViewFrom(row)
	if v.Status != AriaError {
		t.Errorf("status = %q，期望 error", v.Status)
	}
	if v.ErrorCode != 1 {
		t.Errorf("error_code = %d，期望 1", v.ErrorCode)
	}
	if v.ErrorMessage != "节点离线" {
		t.Errorf("error_message = %q，期望 %q", v.ErrorMessage, "节点离线")
	}

	row.ErrorClass = errPtr(ErrNotStarted)
	if v := AriaViewFrom(row); v.ErrorCode != 0 {
		t.Errorf("not_started 不是错误，error_code 应为 0，实际 %d", v.ErrorCode)
	}
}

// 对照 `view_prefers_aria_status_column_then_falls_back`（BUG-4 回归）。
func TestViewPrefersAriaStatusColumnThenFallsBack(t *testing.T) {
	row := sampleRow()

	// 列非空 ⇒ 取列值（即使派生会得出别的结论）
	row.InternalState = StateQueued // 派生本会得 waiting
	row.AriaStatus = string(AriaPaused)
	if v := AriaViewFrom(row); v.Status != AriaPaused {
		t.Fatalf("非空列应优先，实际 %q", v.Status)
	}

	// 列空串 ⇒ 回退到派生（failed + node_down → error）
	row.InternalState = StateFailed
	row.ErrorClass = errPtr(ErrNodeDown)
	row.AriaStatus = ""
	if v := AriaViewFrom(row); v.Status != AriaError {
		t.Fatalf("空列应回退派生得 error，实际 %q", v.Status)
	}

	// 列**全空白**（不是空串）同样应回退 —— Rust 用的是 trim().is_empty()
	row.AriaStatus = " \t\n "
	if v := AriaViewFrom(row); v.Status != AriaError {
		t.Fatalf("全空白列应回退派生得 error，实际 %q", v.Status)
	}
}

// 对照 `classify_reverse_mapping`。
func TestClassifyReverseMapping(t *testing.T) {
	type want struct {
		state InternalState
		err   *ErrorClass
	}
	cases := []struct {
		name       string
		nodeStatus string
		errorCode  *string
		permil     int64
		want       want
	}{
		{"complete", "complete", nil, 1000, want{StateCompleted, nil}},
		// ★B1 起节点自报失败落 `failed_terminal`（不再用 Rust 遗留值 `failed`），
		// 理由见 task.go 的 StateFailedTerminal 注释（INV-A1 按前者排除终态）。
		{"error+node_down", "error", strPtr("node_down"), 0, want{StateFailedTerminal, errPtr(ErrNodeDown)}},
		{"error 无错误码", "error", nil, 0, want{StateFailedTerminal, errPtr(ErrUnknown)}},
		{"paused 未开始", "paused", nil, 0, want{StatePaused, errPtr(ErrNotStarted)}},
		{"paused 有进度", "paused", nil, 10, want{StatePaused, nil}},
		{"running", "running", nil, 10, want{StateRunning, nil}},
		{"waiting", "waiting", nil, 0, want{StateQueued, nil}},
		{"removed", "removed", nil, 0, want{StateRemoved, nil}},
		// BitComet 的成功码是 "OK"；漏掉它会把每次成功都判成 unknown 错误。
		{"OK 不算错误", "running", strPtr("OK"), 10, want{StateRunning, nil}},
	}
	for _, c := range cases {
		gotState, gotErr := ClassifyNodeStatus(c.nodeStatus, c.errorCode, c.permil)
		if gotState != c.want.state {
			t.Errorf("%s：state = %q，期望 %q", c.name, gotState, c.want.state)
		}
		if !errEq(gotErr, c.want.err) {
			t.Errorf("%s：error_class = %v，期望 %v", c.name, derefErr(gotErr), derefErr(c.want.err))
		}
	}
}

// ---------------------------------------------------------------------------
// 本包新增：Rust 侧没覆盖到、但属于「一旦错就是静默错误」的性质
// ---------------------------------------------------------------------------

// MapErrorClass 的输出**必须**始终落在 DDL 的 CHECK 取值集内。
//
// 这是硬约束而非风格问题：若把节点上报的陌生错误串原样透传，
// `INSERT/UPDATE` 会直接违反 `error_class` 的 CHECK 而失败。
// 下面用一批"像真错误又不是真错误"的字符串去撞这个性质。
func TestMapErrorClassAlwaysLandsInDDLSet(t *testing.T) {
	inputs := []string{
		"", "none", "NONE", "None", " not_started", "not_started ", // 大小写与空白变体
		"server_file_not_found", "out_of_retry", "node_down",
		"silently_not_created", "throttled", "agent_timeout", "unknown",
		"disk_full", "permission_denied", "ENOSPC", "0", "-1",
		"无法连接节点", "error: timeout", "null", "nil", "\x00",
	}
	for _, in := range inputs {
		got := MapErrorClass(in)
		if !got.Valid() {
			t.Errorf("MapErrorClass(%q) = %q，不在 DDL 的 CHECK 取值集内", in, got)
		}
	}
	// 且必须覆盖「归一」语义：陌生串 ⇒ unknown，而不是原样返回。
	if got := MapErrorClass("disk_full"); got != ErrUnknown {
		t.Errorf("陌生错误串应归一到 unknown，实际 %q", got)
	}
	// ⚠️ 陷阱：`none` 也会被归一到 unknown。
	// Rust 的 `map_error_class` 就是没有 `"none"` 分支 —— 这不是 bug，
	// 因为调用方 ClassifyNodeStatus 会**先**把 "none" 过滤掉。此处把它显式钉住，
	// 免得将来有人"顺手修一下"却在别处引入了不一致。
	if got := MapErrorClass("none"); got != ErrUnknown {
		t.Errorf("按 Rust 口径 MapErrorClass(\"none\") = unknown，实际 %q", got)
	}
}

// ClassifyNodeStatus 的 error_class 输出**只能是 nil 或 DDL 合法值**。
func TestClassifyNodeStatusNeverEmitsIllegalErrorClass(t *testing.T) {
	statuses := []string{
		"complete", "completed", "error", "failed", "paused",
		"active", "running", "waiting", "queued", "removed", "", "wat", "COMPLETE",
	}
	codes := []*string{nil, strPtr(""), strPtr("none"), strPtr("disk_full"), strPtr("node_down")}
	for _, s := range statuses {
		for _, c := range codes {
			for _, p := range []int64{0, 1, 999, 1000} {
				state, ec := ClassifyNodeStatus(s, c, p)
				if !contains(allInternalStates, state) {
					t.Errorf("ClassifyNodeStatus(%q,%v,%d) 产出非法 internal_state %q", s, derefStr(c), p, state)
				}
				if ec != nil && !ec.Valid() {
					t.Errorf("ClassifyNodeStatus(%q,%v,%d) 产出非法 error_class %q", s, derefStr(c), p, derefErr(ec))
				}
			}
		}
	}
}

// DeriveAriaStatus 的返回值**永远是 6 个合法 Aria2 状态之一**（全域穷举）。
//
// 这条判据的价值在于它同时排除了两类错：把 stoped 拼错、或在 match 里漏分支后
// 让零值（""）漏出去。跨入口一致性全靠这个函数的"全函数"性质。
func TestDeriveAriaStatusIsTotal(t *testing.T) {
	permils := []int64{-1, 0, 1, 999, 1000, 1001}
	errs := []*ErrorClass{nil, errPtr(ErrNone), errPtr(ErrNotStarted), errPtr(ErrNodeDown), errPtr(ErrUnknown)}
	seen := map[AriaStatus]bool{}

	for _, st := range allInternalStates {
		for _, p := range permils {
			for _, e := range errs {
				got := DeriveAriaStatus(st, p, e)
				if !contains(allAriaStatuses, got) {
					t.Fatalf("DeriveAriaStatus(%q,%d,%v) = %q，不是合法 Aria2 状态", st, p, derefErr(e), got)
				}
				seen[got] = true
			}
		}
	}
	// 并集必须恰好覆盖全部 6 个状态 —— 证明没有"永远走不到"的分支。
	for _, want := range allAriaStatuses {
		if !seen[want] {
			t.Errorf("穷举后仍取不到状态 %q：说明映射表里有一条死分支", want)
		}
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func sampleRow() *TaskRow {
	return &TaskRow{
		TaskID:        "01",
		GID:           strPtr("0"),
		Kind:          KindHTTP,
		URLRaw:        "http://e/x",
		SizeState:     SizeUnknown,
		AriaStatus:    string(AriaWaiting),
		InternalState: StateQueued,
		MaxAttempts:   3,
	}
}

func strPtr(s string) *string { return &s }

func errPtr(e ErrorClass) *ErrorClass { return &e }

func derefErr(e *ErrorClass) string {
	if e == nil {
		return "<nil>"
	}
	return string(*e)
}

func derefStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func errEq(a, b *ErrorClass) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// TestClassifyNodeStatus_StoppedDualMeaning 守 B1 规格 §7.6 / §9 的 R-2 行。
//
// 这段分支此前**完全不存在**：BitComet 实测只上报 `running`/`stopped`（F4），
// 而旧实现把 `stopped` 落进 `default: queued`。后果不是"显示不好看"：
//   - 失败任务被分类成 `queued` ⇒ 调度循环反复扫到、反复重投；
//   - 对外永远不显 `error` ⇒ 用户以为还在排队。
//
// 因此这四条断言全部是**行为断言**，删掉 stopped 分支就会 4 红。
func TestClassifyNodeStatus_StoppedDualMeaning(t *testing.T) {
	cases := []struct {
		name      string
		nodeStat  string
		errorCode *string
		permil    int64
		wantState InternalState
		wantErr   *ErrorClass
	}{
		// ① 带错误码 ⇒ 真的失败了，不得报 paused
		{"stopped+server_file_not_found", "stopped", strPtr("server_file_not_found"), 0, StateFailedTerminal, errPtr(ErrServerFileNotFound)},
		{"stopped+out_of_retry", "stopped", strPtr("out_of_retry"), 0, StateFailedTerminal, errPtr(ErrOutOfRetry)},
		// ② 无错误 + 满进度 ⇒ 做种完成
		{"stopped+1000", "stopped", strPtr("OK"), 1000, StateCompleted, nil},
		{"stopped+nil", "stopped", nil, 1000, StateCompleted, nil},
		// ③ 无错误 + 零进度 ⇒ start_later 的正常未启动（非错误）
		{"stopped+0", "stopped", nil, 0, StatePaused, errPtr(ErrNotStarted)},
		// ④ 无错误 + 有进度 ⇒ 暂停
		{"stopped+500", "stopped", strPtr(""), 500, StatePaused, nil},
		// 大小写/空白容忍（节点版本之间可能不一致）
		{" STOPPED 大写", " STOPPED ", nil, 0, StatePaused, errPtr(ErrNotStarted)},
	}
	for _, c := range cases {
		gotState, gotErr := ClassifyNodeStatus(c.nodeStat, c.errorCode, c.permil)
		if gotState != c.wantState {
			t.Errorf("%s：state = %q，期望 %q（旧实现会得到 queued）", c.name, gotState, c.wantState)
		}
		if !errEq(gotErr, c.wantErr) {
			t.Errorf("%s：error_class = %v，期望 %v", c.name, derefErr(gotErr), derefErr(c.wantErr))
		}
	}
}

// TestFailedTerminalIsNeverReportedAsWaiting 守 R-1 派生的「别把失败藏起来」：
// 新增的 failed_terminal 必须在 Aria 面显示成 error。
// 若不补 DeriveAriaStatus 分支，它会落 `default: waiting` ⇒ 用户看到"排队中"。
func TestFailedTerminalIsNeverReportedAsWaiting(t *testing.T) {
	ec := ErrNodeDown
	if got := DeriveAriaStatus(StateFailedTerminal, 0, &ec); got != AriaError {
		t.Fatalf("failed_terminal 应映射为 error，实得 %q", got)
	}
}

// TestDispatchViewReflectsRealState 守一条**曾经的谎报**：
// `dispatched` / `dispatch_state` 在调度内核落地前是硬编码的
// （`false` / `queued_no_node`），内核落地后若不改成派生值，
// 任务明明已经在节点上下载，管理台与 aria2 客户端仍会看到"未派发"。
//
// 这条用例的可证伪性很直接：把实现改回常量 false/queued_no_node ⇒ 前三个断言全红。
func TestDispatchViewReflectsRealState(t *testing.T) {
	node := int64(7)

	cases := []struct {
		name       string
		nodeID     *int64
		state      InternalState
		wantDisp   bool
		wantPhrase string
	}{
		{"排队未落点", nil, StateQueued, false, "queued_no_node"},
		{"已落点但节点侧排队", &node, StateQueued, true, "queued_on_node"},
		{"已下发待确认", &node, StateDispatching, true, "dispatching"},
		{"下载中", &node, StateRunning, true, "running"},
		{"完成", &node, StateCompleted, true, "completed"},
		{"终态失败", &node, StateFailedTerminal, true, "failed"},
		{"已移除", &node, StateRemoved, true, "removed"},
	}
	for _, c := range cases {
		disp, phrase := DispatchView(c.nodeID, c.state)
		if disp != c.wantDisp {
			t.Errorf("%s：dispatched = %v，期望 %v", c.name, disp, c.wantDisp)
		}
		if phrase != c.wantPhrase {
			t.Errorf("%s：dispatchState = %q，期望 %q", c.name, phrase, c.wantPhrase)
		}
	}
}

// TestDispatchViewNeverSaysNoNodeWhenNodeIsSet 是上面那条的核心不变量：
// **只要有 node_id，就绝不能报 queued_no_node**。
//
// 单独提出来是因为它是用户最容易撞见的矛盾形态：
// 列表里 node_id=1、却写着"没有可用节点"。
func TestDispatchViewNeverSaysNoNodeWhenNodeIsSet(t *testing.T) {
	node := int64(1)
	for _, st := range AllInternalStates() {
		if _, phrase := DispatchView(&node, st); phrase == "queued_no_node" {
			t.Errorf("state=%q 且有 node_id 时仍报 queued_no_node", st)
		}
	}
}
