// Command red-harness 是 red-harness 的 CLI 入口。
//
// 这里只做两件事：**构造装配层**（`local.New`）并把它作为 `cli.Main` 的第四个
// 参数交进去，再用 `Main` 的返回值作为退出码。全部逻辑（子命令解析、输出归属、
// 退出码映射）都在 `internal/cli` 里，这样 CLI 才能被单元测试覆盖——把逻辑写在
// 这个文件里意味着它只能在真起进程时才能被测到。
//
// ⚠️ **接线是 `main` 里的一行，不是包级状态**。改前它是 `init()` 里的
// `cli.SetWire(newPorts)`：接线发生在一个没人调用的函数里，于是「CLI 连的到底
// 是谁、默认值从哪来」只能靠读 `init()` 才知道，而测试要换掉它就得改包级变量、
// 并行用例互相污染。现在读 `main` 函数就能回答那两个问题。
package main

import (
	"context"
	"os"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/internal/cli"
	"github.com/red-copilot/red-harness/local"
)

// newPorts 是 CLI 与装配层之间唯一的接缝。
//
// 它的全部工作就是「把 CLI 的输入翻译成 local.Options，再取回 CLI 要的那几个
// 动作」。翻译刻意保持**薄**：任何「顺手做点什么」的念头都会让 CLI 的行为
// 散落在两处，而这条接缝存在的意义正是让 CLI 的可测面保持完整。
//
// ⚠️ **这里不补任何缺省**。`local.Runner.resolve()` 是默认值的**唯一**真源
// （场景、镜像、profile、预算回落、提示策略、结果目录、锁路径），CLI 与直接调
// SDK 的调用方都从它那里拿默认值。以前这里曾经替 `StoreDir` 补过一个 "."、替
// ResultDir 补过一次 `--store`，两次都让「同一次运行」在两条路径上变成两个不同
// 的落点。唯一的例外是 doctor 的 storeDir 补缺，见下。
func newPorts(storeDir string, spec harness.RunSpec, deploy cli.DeployOptions) (cli.Ports, error) {
	// doctor 传的是空 storeDir（体检的是宿主环境，与某个 store 无关，见
	// cli.doctor 的注释）。装配层需要一个可用的根来放 bridge 的 private/
	// 与结果目录，所以这里补上 cwd——**只补 doctor 这一条路径**：run/list/stats
	// 的 storeDir 由 CLI 绝对化过，永远非空。
	//
	// ⚠️ 不要把这条补缺省逻辑搬进 `local.New`：那会让「StoreDir 必须由调用方
	// 明确给出」这条纪律消失，而 StoreDir 是 RunSpec 摘要的一部分——凭空补一个
	// 缺省等于让同一次运行在两条路径上的落点取决于 cwd。
	if storeDir == "" {
		storeDir = "."
	}
	opts := local.Options{
		StoreDir: storeDir,
		// 场景名由 CLI 的 flag 决定（`--scenario`），装配层为空时按 fake 处理。
		Scenario:       spec.Scenario,
		FakeChallenges: deploy.FakeChallenges,
		Agent: local.AgentOptions{
			Provider:   spec.Agent.Provider,
			Model:      spec.Agent.Model,
			Thinking:   spec.Agent.Thinking,
			Extensions: spec.Agent.Extensions,
			Approve:    spec.Agent.Approve,
			SessionDir: spec.Agent.SessionDir,
			HomeDir:    spec.Agent.HomeDir,
		},
		// ProfileDir 来自部署选项（`--bundle`）：它是「这台机器上那份扩展包在哪」，
		// 不是运行意图。装配层会用它补上运行级 profile 的 ExtensionBundle，
		// 从而让 CLI 与直接调 SDK 得到同一个 ProfileDigest / BundleDigest。
		Sandbox: local.SandboxOptions{Image: spec.Sandbox.Image, ProfileDir: deploy.BundleDir},
		Run: local.RunOptions{
			Targets:    spec.Targets,
			Budget:     spec.Budget,
			HintPolicy: spec.HintPolicy,
			Submit:     spec.Submit,
			Policy:     spec.Policy,
		},
	}
	// 锁的显式覆盖（`--lock`）。**为空就不设**：`local.New` 会用它自己的默认值
	// （`<StoreDir>/run.lock`），而那个默认值是唯一真源——在这里替它算一遍
	// 等于把「哪两个进程算同一个部署」这个判据复制到 CLI，两处会漂移。
	if deploy.LockPath != "" {
		opts.Lock = local.NewFileLock(deploy.LockPath)
	}
	r, err := local.New(opts)
	if err != nil {
		// 原样上抛：装配失败的**类别**由 `local` 给出（`wire.new`/`wire.run`/...），
		// CLI 的 `app.ports` 只按 Kind 决定退出码，不改写消息。
		return cli.Ports{}, err
	}
	// ⚠️ **这里不 defer r.Close()**：Runner 持有 bridge 常驻子进程与单运行锁，
	// 它的生命周期必须覆盖整次 `Harness.Run`（而 Run 是在 cli 里调的）。进程
	// 退出时子进程会被内核回收，而锁由 flock 在进程死亡时自动释放——这正是
	// 选 flock 的理由之一（见 local/lock.go）。
	//
	// ⚠️ **不要在这里把 `--store` 落到 ResultDir**：结果目录的缺省是「与 store
	// 同根」，由装配层决定；CLI 再传一次会让同一个根出现两种表示。
	return cli.Ports{Harness: r, Doctor: r, Results: resultStore{r}}, nil
}

// resultStore 把装配层的存储适配成 `harness.ResultStore`。
//
// 为什么不直接在 `cli.Ports` 里放 `r.Results()`：CLI 的 `Ports.Results` 类型
// 就是 `harness.ResultStore`（四个方法），所以这层适配看起来像空转——但它把
// 「CLI 用到的存储」与「装配层交出的存储」这两个概念分开写了一次，将来若要收窄
// CLI 的读取面（例如只给 List/Stats），改的是这里而不是装配层的公开面。
//
// ⚠️ 它**只转发**，不做任何过滤/裁剪：过滤语义的真源在 `store.ResultFileStore`
// （公开 schema 只有指标与指纹），在转发层再筛一遍会让两处口径漂移。
type resultStore struct{ r *local.Runner }

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

// main 是进程入口：装配 + 交给 CLI。装配函数在这里**显式**传入。
//
// 注意顺序：`newPorts` 只是在构造 Options 时被传进去，真正的 `local.New` 是在
// CLI 需要端口的子命令里才被调用的（`--help` 与用法错根本不会装配）——所以
// 「一份写错的配置不该先起 bridge 子进程」这条保证仍然成立。
func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr, newPorts))
}
