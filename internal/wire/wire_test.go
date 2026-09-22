package wire

import (
	"testing"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/local"
)

// 本文件只测**转发本身**：实现的行为测试全在 `local/` 里。这里若去测一遍装配
// 逻辑，就等于把同一批断言写两遍，而第二遍只在转发层断掉时才可能变红——那时
// 别名不成立，编译阶段就已经拦住了。
//
// 三条断言各对一个真实风险：
//
//  1. `Runner` 必须是**别名**而不是「长得像的另一个结构体」。手抄一份的代价：
//     两边字段都在，谁也不会发现，直到某天只改了其中一边。
//  2. `New` 必须真的转发（返回的 Runner 要能给出装配层解析过的 DefaultSpec）。
//  3. 失败路径的错误必须原样穿过——`Kind` 分类是调用方分支的判据（errors.go
//     禁止解析错误消息）。
func TestShimRunnerIsAliasNotCopy(t *testing.T) {
	var r *Runner
	// 两个方向都要能赋值：任一侧是独立类型，这里就编译不过。
	var toLocal *local.Runner = r
	var back *Runner = toLocal
	if back != nil {
		t.Fatal("nil 穿过别名之后不该变成非 nil")
	}
}

func TestShimNewForwards(t *testing.T) {
	r, err := New(Options{StoreDir: t.TempDir(), Scenario: ScenarioFake})
	if err != nil {
		t.Fatalf("转发后的 New 应当装配成功: %v", err)
	}
	defer func() { _ = r.Close() }()

	// 缺省镜像是**装配层解析出来**的值（不是 Options 里的原样回显）：它非空就
	// 说明请求真的走到了 local 的 resolve 逻辑，而不是被转发层截住。
	if spec := r.DefaultSpec(); spec.Sandbox.Image == "" || spec.Executor.Image == "" {
		t.Fatalf("DefaultSpec 没有带上缺省镜像，转发层像是截住了请求: %+v", spec.Sandbox)
	}
	if r.Results() == nil {
		t.Fatal("Results() 转发为空")
	}
}

func TestShimPropagatesConfigError(t *testing.T) {
	_, err := New(Options{StoreDir: "  "})
	if err == nil {
		t.Fatal("StoreDir 为空白时应当失败")
	}
	if !harness.IsKind(err, harness.KindConfig) {
		t.Fatalf("错误分类没有原样穿过转发层: %v", err)
	}
}
