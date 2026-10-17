// Package layerguard 是 `internal/**` 依赖分层的**可执行守卫**。
//
// # 为什么需要它
//
// team-lead 裁定用「叶子包」破掉 Rust 单 crate 允许、Go 禁止的包循环
// （详见 `internal/state`、`internal/handshake`、`internal/session` 的包文档）。
// 但**光有分层约定会被下一个人破坏**（一句 `import` 就成环）。本测试静态解析
// `internal/**` 的 import 图，把约定变成**能红**的判据。
//
// # 分层（高 → 低）
//
//	main(根包) → internal/{ingress,admin} → internal/state
//	          → internal/{handshake,session} → internal/{config,secret,admin/web,tasks,ids,protocol,store,domain}
//
// # 规则
//
// 对任意边 `P → Q`（Q 为 internal 包）：必须 `rank(Q) < rank(P)`。
// 该规则同时保证「无环」与「低层不被高层反向依赖」。
// 任何**未登记**的 internal 包都判失败 —— 强制后来人维护此表。
//
// # ⚠️ 本守卫证明了什么、**没有**证明什么（诚实边界）
//
//   - 它证明的：**没有任何人偷偷新增一条违反分层的 import 边**（含新出现一个未登记的包）。
//   - 它**没有**证明的：**分层设计本身正确**。下表的 `rank` 是**手抄的意图**，不是从真实
//     依赖图推出来的 ⇒ 若有人把 `rank` **写反**（例如把 `state` 与 `session` 的层级对调），
//     那么一条**真正的**反向依赖反而会被判成「合法」。同理，**同一层内**两个包之间的错误
//     依赖（设计缺陷）它也无从发现。
//   - ⇒ **凡改动 `rank` 表的提交，必须附一句理由**（为什么该包属于该层），勿让后人误以为
//     「测试绿了 == 分层设计正确」。
//
// # 本守卫的判据（三件套）
//
//	① 输入：`internal/**`（跳过 `.`/`_`/`testdata`）每个**非测试**包文件的 import 列表，
//	   外加根包（模块根目录下非 `_test.go`）的 import 列表。
//	② 主断言：`layeringViolations` 返回**空**（可观测终态 = 违反分层的文案一条都没有）。
//	③ 阳性对照：`TestUnregisteredPackageIsRejected` 用**注入夹具**证明「合法边不报违规、
//	   未登记包必报违规」——排除「永远红」与「什么都没查」两种假通过。
//
// 本目录只有 `_test.go`（无生产文件），故自身不在图内。
package layerguard

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// modulePath 是本模块的 import 前缀（与 go.mod 一致）。
const modulePath = "github.com/LIUBANGJIAN/download-gateway"

// rank 为每个 internal 包登记层级（越小越底层）。**新增 internal 包必须在此登记**。
//
// ⚠️ 这是**手抄的意图**，不是推导值 —— 改它请附理由（见包文档「诚实边界」）。
var rank = map[string]int{
	// 层 0：纯叶子（只依赖标准库）。
	"internal/ids":      0,
	"internal/protocol": 0,
	"internal/store":    0,
	"internal/domain":   0,

	// 层 1：配置与密钥。
	"internal/config": 1,
	"internal/secret": 1,

	// 层 1：管理口令的哈希 / 校验 / 持久化（`adminpass`）。
	//
	// 登记理由：`internal/state`（层 3，持凭据并在登录时校验）与
	// `internal/admin`（层 4，提供改口令接口）都要用它，而按现有分层
	// `admin → state` 已经成立；若把它放进 `admin`，`state` 反向依赖 `admin` 就成环。
	// 下沉为叶子包后两边都只单向依赖它。实测依赖集仅 标准库 + `internal/store`（层 0），
	// 不含任何 HTTP / 会话 / 状态容器逻辑，故定层 1（与 config/secret 同级）。
	"internal/adminpass": 1,

	// 层 1：管理台的**内嵌静态资源**（`//go:embed` 三件套 + 磁盘覆盖读取）。
	//
	// 登记理由：它原本与 `internal/admin` 同列第 4 层，但接线后 `internal/admin`
	// 必须 import 它（壳页面由它提供），而守卫要求**严格更低层** ⇒ 同层成环。
	// 实测它的依赖只有标准库（`embed`/`os`/`path/filepath`/`log`），是纯资源包、
	// 不含任何业务判定，故下沉到层 1：既满足 `rank(web)=1 < rank(admin)=4`，
	// 又不给它反向依赖业务包的机会。
	"internal/admin/web": 1,

	// 层 1：任务编排（create / query / row）。
	//
	// 登记理由：`internal/tasks` 是 Go 版后补的包，此前漏登记，守卫因此长期红着
	// （`internal 包 "internal/tasks" 未登记在 layerguard.rank`）。
	// 选层 1 的依据是**实测依赖集**：它只 import `internal/{domain,ids,store}`，
	// 三者都是层 0 叶子 —— 于是 `rank(被依赖)=0 < rank(自己)=1` 成立，且层号取到
	// **最紧**（更高层无法反向依赖它，留给将来的 ingress/admin 直接引用）。
	// 若日后它需要 `config`/`secret` 之外的层，请连同此表一起改并说明理由。
	"internal/tasks": 1,

	// 层 2：T04 破环用的两个叶子包。
	"internal/handshake": 2,
	"internal/session":   2,

	// 层 0：选点内核（`scheduler`）。
	//
	// 登记理由：**纯决策**包，实测依赖只有标准库（`fmt`/`math`）。
	// 它刻意不 import `domain`/`store`/`protocol` —— 一旦它开始读库或发网络，
	// B1 的"同一输入必得同一选择"就无法单测。定层 0 让它无法反向依赖任何业务包。
	"internal/scheduler": 0,

	// 层 1：分组识别与建绑（`grouping`）。
	//
	// 登记理由：`key.go` 是纯函数（只用标准库），`bind.go` 依赖 `internal/domain`（层 0）
	// 与 `database/sql`；**不**依赖 `store`（事务由调用方开好后传 `*sql.Tx` 进来）。
	// 因此层 1 是"最紧且成立"的取值：`rank(domain)=0 < rank(grouping)=1`。
	"internal/grouping": 1,

	// 层 2：投递编排（`dispatch`）与回读同步（`sync`）。
	//
	// 登记理由（实测依赖集）：
	//   - `dispatch`：`scheduler`(0) / `store`(0) / `domain`(0) / `protocol`(0) / `secret`(1)
	//   - `sync`：`store`(0) / `domain`(0) / `protocol`(0) / `secret`(1)
	// 两者最大被依赖层是 1（secret），故取 2。二者**互不依赖**：投递只管"发出去"，
	// 同步只管"读回来"，谁也不该等谁。
	"internal/dispatch": 2,
	"internal/sync":     2,

	// 层 3：驱动循环（`engine`）。
	//
	// 登记理由：它是唯一同时依赖 `dispatch`(2) 与 `sync`(2) 的包，故必须**严格更高**（3）。
	// 它不 import `ingress`/`admin`：调度内核不认识 HTTP，只认识库与节点客户端 ——
	// 这条边界让"把调度器拔掉"（`DISPATCH_SCHEDULER=off`）不会牵连协议面。
	"internal/engine": 3,

	// 层 3：两端口共享状态（只持有叶子包类型）。
	"internal/state": 3,

	// 层 4：接入层（HTTP handler 与其装配）。
	"internal/ingress": 4,
	"internal/admin":   4,
}

// rootRank 是根包（main）的层级上界：允许依赖任何 internal 包。
const rootRank = 5

// TestInternalLayeringIsRespected 断言 internal/** 的真实 import 图满足分层。
//
// 这是「真盘面」校验；分支级证伪见 TestUnregisteredPackageIsRejected。
func TestInternalLayeringIsRespected(t *testing.T) {
	root := moduleRoot(t)
	pkgs := internalImports(t, root)

	violations := layeringViolations(pkgs, rootImports(t, root))
	if len(violations) > 0 {
		t.Fatalf("依赖分层被破坏：\n  %s", strings.Join(violations, "\n  "))
	}
}

// layeringViolations 是守卫的**纯核心**：给定一张 import 图（包 → 其 import 列表）
// 与根包的 import 列表，返回**违规清单**（空切片 = 合规）。
//
// 之所以把它抽成**不读盘的纯函数**：为了让「包未登记」这类分支能被**注入夹具**单测
// 驱动 —— 否则只能真建一个 `internal/` 包来造红，那会污染真实结构。
//
// 三类违规（与包文档「规则」一一对应）：
//  1. 某个 internal 包未登记在 rank 表；
//  2. 某个已登记包 import 了「层级不低于自己」的包，或 import 了一个未登记的目标包；
//  3. 根包 import 了未登记的 internal 包（根包自身可依赖任何已登记 internal 包）。
func layeringViolations(pkgs map[string][]string, rootImports []string) []string {
	var violations []string

	// 分支 1：发现的每个包都必须登记。
	for path := range pkgs {
		if _, ok := rank[path]; !ok {
			violations = append(violations,
				fmt.Sprintf("internal 包 %q 未登记在 layerguard.rank：请为它指定层级", path))
		}
	}

	// 分支 2：逐边检查 rank(imported) < rank(importer)。
	for path, imports := range pkgs {
		imp, registered := rank[path]
		if !registered {
			// 已在分支 1 报过；未登记包没有可信层级，跳过其出边以免制造噪声违规。
			continue
		}
		for _, raw := range imports {
			target, isInternal := internalPathOf(raw)
			if !isInternal {
				continue // 标准库 / 第三方，不受约束
			}
			ir, known := rank[target]
			if !known {
				violations = append(violations,
					fmt.Sprintf("%s → %s：目标 internal 包未登记层级", path, target))
				continue
			}
			if ir >= imp {
				violations = append(violations,
					fmt.Sprintf("%s(层%d) → %s(层%d)：只可依赖严格更低层", path, imp, target, ir))
			}
		}
	}

	// 分支 3：根包可依赖任何已登记的 internal 包；不得依赖未登记的 internal 包。
	for _, raw := range rootImports {
		target, isInternal := internalPathOf(raw)
		if !isInternal {
			continue
		}
		if _, known := rank[target]; !known {
			violations = append(violations,
				fmt.Sprintf("根包 import 了未登记的 internal 包 %q", target))
			continue
		}
		if rank[target] >= rootRank {
			violations = append(violations,
				fmt.Sprintf("根包(层%d) → %s：根包应能依赖任何 internal 包", rootRank, target))
		}
	}

	sort.Strings(violations)
	return violations
}

// TestUnregisteredPackageIsRejected 是「包未登记」三个分支的**证伪性**用例。
//
// # 为什么需要它
//
// `TestInternalLayeringIsRespected` 只钉住了「层边违规」这一分支能红；而
// 「未登记」的三个分支（`internal 包 %q 未登记…` / `目标 internal 包未登记层级` /
// `根包 import 了未登记的 internal 包 %q`）**此前从未被证明能红** —— 可它正是
// 「以后新加 internal 包时，守卫会逼你登记」的唯一保障。**一条没人见过它红的兜底
// 分支，就只是一段看起来对的代码。**
//
// # 做法与理由（为什么用「注入夹具」而不是真建包 / 变异 rank 表）
//
// 守卫核心 `layeringViolations` 接受**可注入的 import 图**，故本用例直接**手写一张
// import 图**喂进去：
//   - **不真建 `internal/xxx` 包** —— 那会污染真实结构（且会被 layerguard 主用例扫到）；
//   - **不依赖磁盘夹具目录** —— 守卫实现跳过 `.`/`_`/`testdata`，把夹具放进去只会被跳过，
//     读不到；注入内存对象绕开这一限制，且**不挑测试运行目录**。
//
// # 三件套
//
//	① 输入：下方 pkgs / rootImp 两张手写夹具（含未登记包 orphan / 未登记目标 nonesuch /
//	   根包未登记 ghost，另含**已登记**的合法边 config→ids 与根包→config）。
//	② 主断言：layeringViolations 必须返回**点名**上述未登记实体的违规文案（可观测终态）。
//	③ 阳性对照：夹具里的合法边（已登记 config→已登记 ids、已登记根包→已登记 config）
//	   **不得**出现在违规里 —— 否则「永远红」也能通过本用例。
func TestUnregisteredPackageIsRejected(t *testing.T) {
	const (
		orphan   = "internal/orphan"   // 未登记的**包**
		nonesuch = "internal/nonesuch" // 未登记的**目标**
		ghost    = "internal/ghost"    // 根包引用的未登记包
	)

	// ① 输入：手写 import 图夹具。
	pkgs := map[string][]string{
		orphan:            {modulePath + "/internal/config"}, // 触发分支 1（包未登记）
		"internal/state":  {modulePath + "/" + nonesuch},     // 触发分支 2（目标未登记）
		"internal/config": {modulePath + "/internal/ids"},    // 阳性对照：已登记 → 更低层，合法
	}
	rootImp := []string{
		modulePath + "/internal/config", // 阳性对照：根包 → 已登记包，合法
		modulePath + "/" + ghost,        // 触发分支 3（根包 → 未登记包）
	}

	got := layeringViolations(pkgs, rootImp)
	joined := strings.Join(got, "\n")

	// ② 主断言：三个分支各自的文案都必须命中。
	mustContain := []string{
		fmt.Sprintf("internal 包 %q 未登记在 layerguard.rank", orphan),             // 分支 1
		fmt.Sprintf("%s → %s：目标 internal 包未登记层级", "internal/state", nonesuch), // 分支 2
		fmt.Sprintf("根包 import 了未登记的 internal 包 %q", ghost),                   // 分支 3
	}
	for _, want := range mustContain {
		if !strings.Contains(joined, want) {
			t.Fatalf("「未登记」分支未命中：\n  期望包含：%s\n  实际违规：\n%s", want, joined)
		}
	}

	// ③ 阳性对照：合法边（config 及根包→config）不得产生任何违规。
	for _, forbidden := range []string{"internal/ids", "internal/config"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("阳性对照失败：合法边 %q 不应出现在违规里\n  实际违规：\n%s", forbidden, joined)
		}
	}
}

// moduleRoot 从当前工作目录向上找到含 go.mod 的目录。
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd 失败：%v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("向上未找到 go.mod")
		}
		dir = parent
	}
}

// internalImports 返回 `internal/**` 每个包（有 ≥1 个非 `_test.go` 文件的目录）
// 的 import 列表。跳过 `.`/`_` 前缀目录与 `testdata`。
func internalImports(t *testing.T, root string) map[string][]string {
	t.Helper()
	internalDir := filepath.Join(root, "internal")
	filesByPkg := map[string][]string{}
	err := filepath.WalkDir(internalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != internalDir {
				n := d.Name()
				if strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") || n == "testdata" {
					return fs.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		pkg := filepath.ToSlash(rel) // e.g. internal/secret
		filesByPkg[pkg] = append(filesByPkg[pkg], path)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 internal 失败：%v", err)
	}

	out := make(map[string][]string, len(filesByPkg))
	for pkg, files := range filesByPkg {
		out[pkg] = parseImports(t, files)
	}
	return out
}

// rootImports 返回根包（模块根目录下）的非测试 `.go` 文件的 import 列表。
func rootImports(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读模块根失败：%v", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			files = append(files, filepath.Join(root, n))
		}
	}
	return parseImports(t, files)
}

// parseImports 解析一批 `.go` 文件的 import 路径。
func parseImports(t *testing.T, files []string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var out []string
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", f, err)
		}
		for _, spec := range af.Imports {
			out = append(out, strings.Trim(spec.Path.Value, `"`))
		}
	}
	return out
}

// internalPathOf 把 import 路径映射为 `internal/...` 相对路径；非本模块 internal 包返回 false。
func internalPathOf(imp string) (string, bool) {
	const prefix = modulePath + "/internal/"
	if strings.HasPrefix(imp, prefix) {
		return "internal/" + strings.TrimPrefix(imp, prefix), true
	}
	return "", false
}
