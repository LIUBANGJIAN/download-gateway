package domain

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 契约金样比对：Go 侧的常量表必须与**冻结的** DDL 逐值一致。
//
// 这是本次 Go 重构的第一条"零漂移"判据 —— 它不检查任何逻辑，
// 只保证「Go 代码认为合法的取值」和「数据库允许写入的取值」是同一个集合。
// 任何一侧单独改动都会让这个文件的用例变红。

// ddlRelPath 相对于本包目录（internal/domain）。
const ddlRelPath = "../../migrations/V1__init.sql"

func readDDL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(ddlRelPath))
	if err != nil {
		t.Fatalf("读取 DDL 失败（%s）：%v", ddlRelPath, err)
	}
	return string(b)
}

// taskTableBlock 切出 `CREATE TABLE task ( ... );` 这一段。
//
// ⚠️ 必须**按表块切分**，不能全文搜 `kind IN (...)`：
// `kind` 这个列名在 `task_group`（DDL 第 67 行）里也出现过一次，
// 其值集是 ('series','batch','single','source')，与 `task` 的
// ('http','bt','magnet','torrent') 完全不同。全文搜会拿错枚举 ——
// 这正是"契约比对"最典型的假绿来源：测试跑绿了，比的是另一张表。
func taskTableBlock(t *testing.T, ddl string) string {
	t.Helper()
	const head = "CREATE TABLE task ("
	start := strings.Index(ddl, head)
	if start < 0 {
		t.Fatalf("DDL 里找不到 %q（表名或括号写法变了？）", head)
	}
	rest := ddl[start:]
	// 表块内部不含 `\n);`（CHECK 子句收尾是 `)),`，外键收尾是 `,`），
	// 所以第一个 `\n);` 就是表的结尾。
	end := strings.Index(rest, "\n);")
	if end < 0 {
		t.Fatalf("DDL 的 task 表块没有找到结束的 \"\\n);\"")
	}
	return rest[:end]
}

var (
	checkInRe = regexp.MustCompile(`(?s)([a-z_]+)\s+IN\s*\(([^)]*)\)`)
	quotedRe  = regexp.MustCompile(`'([^']*)'`)
	columnRe  = regexp.MustCompile(`(?m)^\s{2}([a-z_]+)\s+[A-Z]`)
)

// extractCheckSets 把表块里所有 `col IN ('a','b')` 抽成 col → 排序后的值列表。
func extractCheckSets(t *testing.T, block string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, m := range checkInRe.FindAllStringSubmatch(block, -1) {
		col := m[1]
		var vals []string
		for _, q := range quotedRe.FindAllStringSubmatch(m[2], -1) {
			vals = append(vals, q[1])
		}
		if len(vals) == 0 {
			// `CHECK (x IN (0,1))` 这种数字型约束，本文件的比对表用不到，跳过。
			continue
		}
		if prev, dup := out[col]; dup {
			t.Fatalf("列 %s 在 task 表块里出现多个文本型 CHECK (… IN …)：%v 与 %v —— 比对会二义", col, prev, vals)
		}
		sort.Strings(vals)
		out[col] = vals
	}
	return out
}

// TestEnumSetsMatchFrozenDDL 是本文件的主判据。
func TestEnumSetsMatchFrozenDDL(t *testing.T) {
	sets := extractCheckSets(t, taskTableBlock(t, readDDL(t)))

	cases := []struct {
		column string
		got    []string
	}{
		{"kind", stringsOf(allKinds)},
		{"size_state", stringsOf(allSizeStates)},
		{"error_class", stringsOf(allErrorClasses)},
		{"dispatch_reason", stringsOf(allDispatchReasons)},
	}
	for _, c := range cases {
		want, ok := sets[c.column]
		if !ok {
			t.Fatalf("task 表块里没找到 %s 的文本型 CHECK (… IN (…))。"+
				"若是有意删掉该约束，请连同本用例一起删；否则说明 DDL 被改动了。", c.column)
		}
		got := append([]string(nil), c.got...)
		sort.Strings(got)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("列 %s 的取值集与 DDL 不一致：\n  Go  侧 = %v\n  DDL 侧 = %v", c.column, got, want)
		}
	}
}

// TestTaskRowProjectionMatchesDDLColumns 钉住 TaskRow 与 `task` 表的列对应关系。
//
// 判据分两半：
//
//	① TaskRow 投影的每一列都**真实存在**于 DDL（防止 Go 读一个不存在的列）；
//	② 未被投影的列**恰好**是预期的那 6 个（防止有人悄悄往 SELECT_COLS 里塞列
//	   却忘了同步 TaskRow，或反过来漏读一列）。
func TestTaskRowProjectionMatchesDDLColumns(t *testing.T) {
	block := taskTableBlock(t, readDDL(t))

	actual := map[string]bool{}
	for _, m := range columnRe.FindAllStringSubmatch(block, -1) {
		actual[m[1]] = true
	}

	projected := []string{
		"task_id", "gid", "group_id", "source_key", "kind", "url_raw", "url_norm", "name",
		"size_state", "total_size", "downloaded_size", "permillage", "aria_status", "internal_state",
		"error_class", "error_message", "node_id", "node_task_id", "attempt_no", "max_attempts",
		"next_retry_at", "possible_duplicate", "created_at", "updated_at", "completed_at",
	}
	for _, c := range projected {
		if !actual[c] {
			t.Errorf("TaskRow 投影了列 %q，但 DDL 的 task 表里没有这一列", c)
		}
	}

	// 未被投影的列必须恰好是这 6 个（与 Rust `SELECT_COLS` 的取舍一致）。
	wantUnprojected := []string{
		"dedup_key", "dedup_level", "episode_no", "dispatch_reason", "progress_at", "batch_pending",
	}
	projSet := map[string]bool{}
	for _, c := range projected {
		projSet[c] = true
	}
	var gotUnprojected []string
	for c := range actual {
		if !projSet[c] {
			gotUnprojected = append(gotUnprojected, c)
		}
	}
	sort.Strings(gotUnprojected)
	sort.Strings(wantUnprojected)
	if strings.Join(gotUnprojected, "|") != strings.Join(wantUnprojected, "|") {
		t.Errorf("未被 TaskRow 投影的列变了：\n  实际 = %v\n  预期 = %v\n"+
			"若是有意增减，请同步改本用例与 TaskRow 的文档注释。", gotUnprojected, wantUnprojected)
	}
}

// TestInternalStateAndAriaStatusHaveNoDDLCheck 把"为什么它俩不在上面的比对表里"写死。
//
// 这两列在 DDL 里**故意没有** CHECK 约束（internal_state 允许演进、
// aria_status 是 Aria2 协议词汇，由写路径负责正确性）。
// 一旦有人给它们加上 CHECK，这条用例会红，逼他把值集补进 TestEnumSetsMatchFrozenDDL ——
// 而不是让"数据库约束"和"Go 常量表"两套口径悄悄分叉。
func TestInternalStateAndAriaStatusHaveNoDDLCheck(t *testing.T) {
	block := taskTableBlock(t, readDDL(t))
	for _, col := range []string{"internal_state", "aria_status"} {
		re := regexp.MustCompile(col + `\s+TEXT[^,\n]*CHECK`)
		if re.MatchString(block) {
			t.Fatalf("DDL 的 %s 现在带 CHECK 约束了 —— 请把它的取值集加入 "+
				"TestEnumSetsMatchFrozenDDL 的比对表，并同步更新本用例。", col)
		}
	}
}

// stringsOf 把任意字符串底层类型的切片转成 []string。
func stringsOf[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}
