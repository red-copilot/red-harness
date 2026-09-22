package legacy_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ── 依赖方向的**可执行**断言 ──
//
// CLAUDE.md 里有三条靠人读 import 块维持的规矩：
//
//  1. 根包零内部依赖（不许 import 任何子包）；
//  2. 实现包两两互不 import（executor 不认识 scenario，scenario 不认识 dag）；
//  3. 同时 import 多个实现包的**恰好只有一个**（今天它是 `local/`；迁移窗口内
//     `internal/wire` 是它的薄转发层，但转发层自己也不 import 实现包）。
//
// 它们此前**只写在文档里**。文档不会因为一次 `import` 而变红，所以漂移的代价是
// 事后有人读注释才发现——而这两条恰恰是「一个子包 = 一个 agent、波次内文件不相交」
// 那套并行开发办法成立的前提：一旦 executor 与 scenario 互相 import，改其中一个
// 就会让另一个编译不过，派工表当场失效。
//
// 本文件把它们变成断言。用 go/parser 扫源码而不是读 `go list` 的输出：后者需要
// 一个可编译的模块状态，而依赖方向恰恰是**编译失败时最需要被检查**的东西。

const modulePath = "github.com/red-copilot/red-harness"

// implPkgs 是「实现包」：它们彼此之间必须零边。
//
// `answer` 与 `legacy` **不在这里**：两者都是共享叶子（像根包一样只被依赖、
// 不依赖别的实现包）。把 answer 算进来会立刻误报——gate / dag / store 都 import
// 它，那是设计如此（它是 Shape 与 Fingerprint 的唯一真源）。区分「实现包」与
// 「共享叶子」正是这份断言要表达的判断，所以它必须写在代码里而不是靠印象。
var implPkgs = map[string]bool{
	"dag": true, "gate": true, "store": true, "executor": true,
	"bridge": true, "piai": true, "scenario": true,
}

var sharedLeaves = map[string]bool{"answer": true, "legacy": true}

// assemblyPkgs 是**允许**「同时 import ≥2 个实现包」的那一个装配点。
//
// 保留成集合而不是一个字面量，是因为它曾经有两个合法取值：N0.1 把装配实现从
// `internal/wire` 搬到了 `local/`（搬家的理由见 `local/wire.go` 的包文档：Go 的
// internal 可见性规则让 `internal/wire` 只能被本模块 import，于是仓库内的示例
// 证明不了「外部可用」），而 `internal/wire` 作为薄转发层留了一轮迁移窗口。
//
// ⚠️ **那个窗口已经关了**（N0.1 的 CLI 侧在 `ed6f7af` 落地：`cli.Main` 收显式的
// 装配函数参数，`cmd/red-harness/main.go` 改 import `local`，包级 `SetWire` 删除）。
// 于是 `internal/wire` 现在**没有任何 import 方**，集合收紧回单个 `local`。
// 这条断言要钉住的从来不是某个路径字符串，而是**「装配点只有一个」**：多一个就
// 意味着「谁把 X 交给 Y」这件事有两个地方会做，而两个地方迟早会不一致。
//
// ⚠️ 它**不**属于 implPkgs，也**不**属于 sharedLeaves：前者会让
// `TestLayeringImplementationPackagesAreDisjoint` 把装配点自己的 import 当成
// 违规（放行它正是本条断言的内容），后者会让「没有实现包 import 它」这件事
// 变成一条没人检查的假设。
var assemblyPkgs = map[string]bool{"local": true}

// pkgImports 扫描 moduleRoot 下所有包的生产代码（跳过 _test.go），返回
// 「包相对路径 → 它 import 的本模块包路径集合」。
func pkgImports(t *testing.T, moduleRoot string) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			// .git / .claude（worktree 副本）/ analysis（Python）都不是本模块的包。
			if base == ".git" || base == ".claude" || base == "analysis" || base == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		rel, err := filepath.Rel(moduleRoot, dir)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		if out[rel] == nil {
			out[rel] = map[string]bool{}
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !strings.HasPrefix(p, modulePath) {
				continue
			}
			// import 根包（`github.com/.../red-harness`）归一成 "."——它与
			// filepath.Rel 给出的根目录键是同一个东西。少了这一步，根包会变成
			// 空串，而空串不在任何白名单里，于是「import 根包」被误报成违规。
			sub := strings.TrimPrefix(strings.TrimPrefix(p, modulePath), "/")
			if sub == "" {
				sub = "."
			}
			out[rel][sub] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 import 图失败: %v", err)
	}
	return out
}

func TestLayeringRootPackageHasNoInternalImports(t *testing.T) {
	graph := pkgImports(t, "..")
	root, ok := graph["."]
	if !ok {
		t.Fatal("没有扫到根包（.）——扫描逻辑坏了，下面的断言都会空转")
	}
	if len(root) != 0 {
		var got []string
		for p := range root {
			got = append(got, p)
		}
		sort.Strings(got)
		t.Fatalf("根包 import 了子包 %v —— 根包必须零内部依赖", got)
	}
}

func TestLayeringLegacyIsALeaf(t *testing.T) {
	graph := pkgImports(t, "..")
	// 只准 import 标准库与根包。任何实现包出现在这里，都会让 legacy 变成传递
	// 依赖：`store → legacy → executor` 就意味着改 executor 会让 store 编译不过，
	// 那正是「实现包两两互不 import」要防的事故。
	for p := range graph["legacy"] {
		// "." 是根包：**允许**，而且是唯一被允许的本模块依赖。
		if p != "." && !sharedLeaves[p] {
			t.Fatalf("legacy import 了 %q —— 它只准 import 标准库与根包", p)
		}
	}
}

func TestLayeringImplementationPackagesAreDisjoint(t *testing.T) {
	graph := pkgImports(t, "..")
	var edges []string
	for from, imps := range graph {
		if !implPkgs[from] {
			continue
		}
		for to := range imps {
			if implPkgs[to] {
				edges = append(edges, from+" → "+to)
			}
		}
	}
	if len(edges) > 0 {
		sort.Strings(edges)
		t.Fatalf("实现包之间出现了边 %v —— 它们必须两两互不 import（否则「一个子包=一个 agent」的派工前提失效）", edges)
	}
}

// 名字里的 `OnlyLocal` 是**订正过的**：它曾经叫 `…OnlyWire…`，而装配点早已不是
// `internal/wire`。一个断言「装配点只有一个」的测试顶着旧装配点的名字，会让读的
// 人以为装配还在那个包——而那正好是这条断言要防的「两处会漂移」的认知形态。
func TestLayeringOnlyLocalAssemblesMultipleImplPackages(t *testing.T) {
	graph := pkgImports(t, "..")
	var multi []string
	for from, imps := range graph {
		n := 0
		for to := range imps {
			if implPkgs[to] {
				n++
			}
		}
		if n >= 2 {
			multi = append(multi, from)
		}
	}
	sort.Strings(multi)
	// **恰好一个**：不是「至少一个」，也不是「都在白名单里就行」。两个装配点
	// 意味着同一份接线有两份实现，而它们只会在某次运行的行为差异里被发现。
	if len(multi) != 1 || !assemblyPkgs[multi[0]] {
		t.Fatalf("同时 import ≥2 个实现包的包 = %v，期望恰好只有 local 一个（装配点只有一个）", multi)
	}
}
