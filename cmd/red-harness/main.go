// Command red-harness 是 red-harness 的 CLI 入口。
//
// 这里**只做两件事**：把进程参数与进程 stdio 交给 `cli.Main`，用它的返回值
// 作为退出码；以及**安装装配函数**（`cli.SetWire`）。全部逻辑（子命令解析、
// 输出归属、退出码映射）都在 `internal/cli` 里，这样 CLI 才能被单元测试覆盖
// ——把逻辑写在这个文件里意味着它只能在真起进程时才能被测到。
package main

import (
	"context"
	"os"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/internal/cli"
	"github.com/red-copilot/red-harness/internal/wire"
)

func init() {
	// 装配函数在 **init** 里装，而不是在 main 里：`cli.Main` 可能在解析
	// `--help` 之前就被调用（测试、脚本探测），而一个还没装配的 CLI 会回答
	// 「未实现」——那是一条假的结论。装在这里，进程内的任何一次调用看到的都是
	// 同一套端口。
	cli.SetWire(newPorts)
}

// newPorts 是 CLI 与装配层之间唯一的接缝。
//
// 它的全部工作就是「把 CLI 的输入翻译成 wire.Options，再取回 CLI 要的那两个
// 动作」。翻译刻意保持**薄**：任何「顺手做点什么」的念头都会让 CLI 的行为
// 散落在两处，而这条接缝存在的意义正是让 CLI 的可测面保持完整。
func newPorts(storeDir string, spec harness.RunSpec, deploy cli.DeployOptions) (cli.Ports, error) {
	// doctor 传的是空 storeDir（体检的是宿主环境，与某个 store 无关，见
	// cli.doctor 的注释）。装配层需要一个可用的根来放 bridge 的 private/
	// 与结果目录，所以这里补上 cwd——**只补 doctor 这一条路径**：run/list/stats
	// 的 storeDir 由 CLI 绝对化过，永远非空。
	//
	// ⚠️ 不要把这条补缺省逻辑搬进 `wire.New`：那会让「StoreDir 必须由调用方
	// 明确给出」这条纪律消失，而 StoreDir 是 RunSpec 摘要的一部分——凭空补一个
	// 缺省等于让同一次运行的落点取决于 cwd。
	if storeDir == "" {
		storeDir = "."
	}
	r, err := wire.New(wire.Options{
		StoreDir: storeDir,
		// 场景名由 CLI 的 flag 决定（`--scenario`），装配层为空时按 fake 处理。
		Scenario:       spec.Scenario,
		FakeChallenges: deploy.FakeChallenges,
		Agent: wire.AgentOptions{
			Provider:   spec.Agent.Provider,
			Model:      spec.Agent.Model,
			Thinking:   spec.Agent.Thinking,
			Extensions: spec.Agent.Extensions,
			Approve:    spec.Agent.Approve,
			SessionDir: spec.Agent.SessionDir,
			HomeDir:    spec.Agent.HomeDir,
		},
		Run: wire.RunOptions{
			Targets:    spec.Targets,
			Budget:     spec.Budget,
			HintPolicy: spec.HintPolicy,
			Submit:     spec.Submit,
			Policy:     spec.Policy,
		},
	})
	if err != nil {
		return cli.Ports{}, err
	}
	// ⚠️ **这里不 defer r.Close()**：Runner 持有 bridge 常驻子进程与单运行锁，
	// 它的生命周期必须覆盖整次 `Harness.Run`（而 Run 是在 cli 里调的）。进程
	// 退出时子进程会被内核回收，而锁由 flock 在进程死亡时自动释放——这正是
	// 选 flock 的理由之一（见 internal/wire/lock.go）。
	//
	// ⚠️ **不要在这里把 `--store` 落到 ResultDir**：结果目录的缺省是「与 store
	// 同根」，由装配层决定；CLI 再传一次会让同一个根出现两种表示。
	return cli.Ports{Harness: r, Doctor: r, Results: resultStore{r}}, nil
}

// resultStore 把装配层的存储适配成 `harness.ResultStore`。
//
// 为什么不直接在 `wire.Ports` 里放 `r.Results()`：CLI 的 `Ports.Results` 类型
// 就是 `harness.ResultStore`（四个方法），所以这层适配看起来像空转——但它把
// 「CLI 用到的存储」与「装配层交出的存储」这两个概念分开写了一次，将来若要收窄
// CLI 的读取面（例如只给 List/Stats），改的是这里而不是 wire 的公开面。
//
// ⚠️ 它**只转发**，不做任何过滤/裁剪：过滤语义的真源在 `store.ResultFileStore`
// （公开 schema 只有指标与指纹），在转发层再筛一遍会让两处口径漂移。
type resultStore struct{ r *wire.Runner }

func (s resultStore) Save(ctx context.Context, res harness.RunResult) error {
	return s.r.Results().Save(ctx, res)
}
func (s resultStore) Get(ctx context.Context, id harness.RunID) (harness.RunResult, error) {
	return s.r.Results().Get(ctx, id)
}
func (s resultStore) List(ctx context.Context) ([]harness.RunResult, error) {
	return s.r.Results().List(ctx)
}
func (s resultStore) Stats(ctx context.Context, q harness.StatsQuery) (harness.StatsReport, error) {
	return s.r.Results().Stats(ctx, q)
}

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
