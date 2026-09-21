package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// runFlags 是 `run` 与 `resume` 共用的运行参数。
//
// 为什么抽出来：`resume` 的语义是「用**同样的**配置接着跑」，而配置的权威副本
// 是快照里的 `RunSpec`（引擎会用 `RunSpec.Digest()` 与快照比对，漂移即 fail
// closed）。所以这里定义的 flag 不是「resume 的配置」，而是「给装配层拼 RunSpec
// 用的那一份」；T14 实现 resume 时会拿它去和快照比对，而不是拿它覆盖快照。
type runFlags struct {
	store      string
	scenario   string
	targets    string
	provider   string
	model      string
	thinking   string
	exts       string
	approve    bool
	homeDir    string
	sessionDir string
	image      string
	workdir    string
	cpus       float64
	memoryMB   int
	pidsLimit  int
	readOnly   bool
	allowHosts string

	budgetRounds int
	budgetWall   time.Duration
	budgetTurns  int
	budgetCost   float64

	hint              string
	submit            bool
	policyMaxAttempts int
	policyDryRounds   int
}

// register 把 flag 挂到一个 FlagSet 上。名字即 CLI 的公开面，改名字是破坏性变更。
func (f *runFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.store, "store", "runs", "运行目录根（<store>/runs/<runID>/）")
	fs.StringVar(&f.scenario, "scenario", "", "场景名（对应装配层 Options.Scenarios 的键）")
	fs.StringVar(&f.targets, "targets", "", "题目 code 列表，逗号分隔；空表示全部未完成")

	fs.StringVar(&f.provider, "provider", "", "pi 的 --provider")
	fs.StringVar(&f.model, "model", "", "pi 的 --model")
	fs.StringVar(&f.thinking, "thinking", "", "pi 的 --thinking（off/minimal/low/medium/high/xhigh/max）")
	fs.StringVar(&f.exts, "extensions", "", "extension 文件的**绝对路径**，逗号分隔（走 -e）")
	fs.BoolVar(&f.approve, "approve", true, "pi 的 --approve；不开则项目本地资源被静默忽略")
	fs.StringVar(&f.homeDir, "home", "", "每题独立的 HOME（空则由引擎按 <runDir>/.pi-home 生成）")
	fs.StringVar(&f.sessionDir, "session-dir", "", "pi 的 --session-dir")

	fs.StringVar(&f.image, "image", "", "runner 镜像")
	fs.StringVar(&f.workdir, "workdir", "", "容器工作目录（唯一允许挂进去的宿主目录）")
	fs.Float64Var(&f.cpus, "cpus", 0, "CPU 上限（0 表示镜像默认）")
	fs.IntVar(&f.memoryMB, "memory-mb", 0, "内存上限 MB（0 表示镜像默认）")
	fs.IntVar(&f.pidsLimit, "pids-limit", 0, "PID 上限（0 表示镜像默认）")
	fs.BoolVar(&f.readOnly, "read-only", true, "只读 rootfs")
	fs.StringVar(&f.allowHosts, "allow-hosts", "", "目标地址白名单（IP:port），逗号分隔；其余出站默认拒绝")

	// 预算的零值在这里是**「没给」**的标记：给 0 会让 Budget 变成「全部不限」，
	// 那等于没有护栏。所以零值一律替换成 harness.DefaultBudget() 的对应项。
	fs.IntVar(&f.budgetRounds, "budget-rounds", 0, "意图轮次上限（0 表示用默认 40）")
	fs.DurationVar(&f.budgetWall, "budget-wall", 0, "墙钟上限（0 表示用默认 30m）")
	fs.IntVar(&f.budgetTurns, "budget-turns", 0, "工具调用总数上限（0 表示用默认 600）")
	fs.Float64Var(&f.budgetCost, "budget-cost", 0, "累计成本上限 USD（0 表示不限）")

	fs.StringVar(&f.hint, "hint", harness.HintAuto, "提示策略：off / auto / always")
	fs.BoolVar(&f.submit, "submit", false, "是否真向平台提交（默认干跑，只记账）")

	fs.IntVar(&f.policyMaxAttempts, "max-attempts", 0, "同一意图的最大重试轮数（0 表示用引擎默认）")
	fs.IntVar(&f.policyDryRounds, "dry-rounds-before-hint", 0, "连续无进展多少轮后允许请求提示（0 表示用引擎默认）")
}

// budget 把 flag 折成 harness.Budget。
//
// **零值必须是「用默认值」而不是「不限」**：`Budget{MaxRounds: 0}` 在
// `Budget.Exhausted` 里表示该维度不设限，如果用户什么都没填就落成零值，
// 预算护栏会整条消失——而「护栏看起来在、实际不在」比没有护栏更危险。
//
// **负数同样必须归到默认值**：`Exhausted` 的判据是 `b.MaxRounds > 0 && …`，
// 所以 `--budget-rounds=-1` 落进 Budget 等于**关掉**该维度（而不是「用默认」
// 或报错）——用户以为收紧了，实际把护栏拆了。
func (f *runFlags) budget() harness.Budget {
	def := harness.DefaultBudget()
	b := harness.Budget{
		MaxRounds:  f.budgetRounds,
		MaxWall:    f.budgetWall,
		MaxTurns:   f.budgetTurns,
		MaxCostUSD: f.budgetCost, // 0 = 不限，与 harness 的语义一致
	}
	if b.MaxRounds <= 0 {
		b.MaxRounds = def.MaxRounds
	}
	if b.MaxWall <= 0 {
		b.MaxWall = def.MaxWall
	}
	if b.MaxTurns <= 0 {
		b.MaxTurns = def.MaxTurns
	}
	if b.MaxCostUSD < 0 {
		// 负成本上限在 Exhausted 里同样等价于「不限」，是纯粹的输入错误。
		b.MaxCostUSD = 0
	}
	return b
}

// spec 把 flag 折成 RunSpec。
//
// ⚠️ **RunSpec 会整份写进 run.json（公开文件），所以这里绝不能放凭据。**
// provider 的 API key 走进程环境变量，平台 token 走 bridge 子进程环境变量。
//
// ⚠️ **StoreDir 先绝对化再入 spec。** 它是 `RunSpec.Digest()` 的一部分，而
// `--store` 的默认值是一个相对路径（"runs"）：原样写进去等于把 **cwd 变成配置
// 的一部分**——同一个 store 在 /a 与 /b 下起两次会得到两个不同的摘要，恢复时
// 报「配置漂移」，而两次其实指向不同的目录（更糟的是不报错的那种）。
func (f *runFlags) spec() harness.RunSpec {
	return harness.RunSpec{
		Scenario: f.scenario,
		Targets:  splitList(f.targets),
		Agent: harness.AgentSpec{
			Provider:   f.provider,
			Model:      f.model,
			Thinking:   f.thinking,
			Extensions: splitList(f.exts),
			Approve:    f.approve,
			SessionDir: f.sessionDir,
			HomeDir:    f.homeDir,
		},
		Executor: harness.ExecutorSpec{
			Image:      f.image,
			Workdir:    f.workdir,
			CPUs:       f.cpus,
			MemoryMB:   f.memoryMB,
			PidsLimit:  f.pidsLimit,
			ReadOnly:   f.readOnly,
			AllowHosts: splitList(f.allowHosts),
		},
		Budget:     f.budget(),
		HintPolicy: f.hint,
		Submit:     f.submit,
		StoreDir:   f.storeDir(),
		Policy: harness.PolicySpec{
			MaxAttemptsPerIntent: f.policyMaxAttempts,
			DryRoundsBeforeHint:  f.policyDryRounds,
		},
	}
}

// storeDir 把 `--store` 折成绝对路径，是**唯一**该用来喂装配层（`a.ports`）、
// 拼 socket 路径、以及写进 RunSpec.StoreDir 的 store 值。
//
// ⚠️ **不要在这里改回 f.store**：`spec()` 把绝对化后的值写进 RunSpec.StoreDir，
// 而 StoreDir 是 Digest 的一部分、也是恢复时的漂移判据。如果装配层拿到的是
// 相对路径（"runs"）而摘要里是 "/abs/cwd/runs"，同一个 store 就有了两种表示：
// 装配层按 cwd 建 store、摘要按另一个根比对，换个 cwd 恢复就报「配置漂移」；
// pause/cancel 更直接——它们会去 cwd 下找一个并不存在的 control.sock。
// 所以「绝对化」必须发生在**所有**出口上，而不是只发生在 spec 里。
func (f *runFlags) storeDir() string { return absStoreDir(f.store) }

// absStoreDir 把 store 根折成绝对路径（解析失败时退回原值，让 store.New 给出
// 更准确的错误）。
//
// 它同时被 runFlags 与 pause/cancel 使用：这两条路径必须对同一个 `--store`
// 得到同一个字符串，否则「run 在哪儿建的 socket」与「pause 去哪儿拨号」会分叉。
func absStoreDir(store string) string {
	if abs, err := filepath.Abs(store); err == nil {
		return abs
	}
	return store
}

// run 执行 `run` 子命令：新建一次运行并等它终局。
func (a *app) run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	f := &runFlags{}
	f.register(fs)
	help, err := a.parseFlags("run", fs, args)
	if help || err != nil {
		return err
	}

	ports, err := a.ports(f.storeDir())
	if err != nil {
		return err
	}
	if ports.Engine == nil {
		// 本波次的正常路径：装配层还没接线（T14 的 wire.go）。
		return notImplemented("run")
	}

	// 信号 → Cancel 的接线在 T14（要先拿到 handle 才能 Cancel）。
	handle, err := ports.Engine.Start(context.Background(), f.spec())
	if err != nil {
		return err
	}
	// 等终局再返回：CLI 的退出码要反映这次运行的结果，而不是「进程起来了」。
	// T14 会在这里接上事件流打印与 Ctrl-C → Cancel 的处理。
	snap, err := handle.Wait(context.Background())
	if err != nil {
		return err
	}
	printSnapshot(a.out(), snap)
	return nil
}

// resume 执行 `resume` 子命令：恢复一次已有运行。
//
// 恢复**不做**任何本地猜测：终态 run、配置摘要漂移、private/ 账本缺失三种情形
// 都由引擎 fail closed（见设计文档 §5）。CLI 这一层只负责把 RunID 交出去，
// 绝不「因为恢复失败就重跑一遍」——那会重复提交 flag、重复消耗预算。
func (a *app) resume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	var (
		store = fs.String("store", "runs", "运行目录根")
		runID = fs.String("run", "", "要恢复的运行 ID")
	)
	help, err := a.parseFlags("resume", fs, args)
	if help || err != nil {
		return err
	}
	if err := requireRunID(*runID); err != nil {
		return err
	}

	ports, err := a.ports(absStoreDir(*store))
	if err != nil {
		return err
	}
	if ports.Engine == nil {
		return notImplemented("resume")
	}
	handle, err := ports.Engine.Resume(context.Background(), harness.RunID(*runID))
	if err != nil {
		return err
	}
	snap, err := handle.Wait(context.Background())
	if err != nil {
		return err
	}
	printSnapshot(a.out(), snap)
	return nil
}

// list 执行 `list` 子命令：列出运行。
//
// 只打印 RunSummary 里的公开字段——它按契约**不含任何明文**（model.go），
// 所以列表输出可以直接贴进工单。
func (a *app) list(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var (
		store = fs.String("store", "runs", "运行目录根")
		all   = fs.Bool("all", false, "包含终局（completed/failed/cancelled）的运行")
	)
	help, err := a.parseFlags("list", fs, args)
	if help || err != nil {
		return err
	}

	ports, err := a.ports(absStoreDir(*store))
	if err != nil {
		return err
	}
	if ports.Runs == nil {
		return notImplemented("list")
	}
	sums, err := ports.Runs.List(context.Background())
	if err != nil {
		return err
	}
	for _, s := range sums {
		if !*all && s.State.Terminal() {
			continue
		}
		fmt.Fprintf(a.out(), "%s\t%s\t%s\t%d/%d\t%s\n",
			s.RunID, s.State, s.Scenario, s.Objective.Got, s.Objective.Want, s.Reason)
	}
	return nil
}

// printSnapshot 打印一次运行的收尾摘要。
//
// **只打印快照里的公开字段**：Snapshot 刻意没有 Flags 字段（明文只在 private/
// 与返回值里），所以这里不可能漏出候选明文。
func printSnapshot(w io.Writer, s harness.Snapshot) {
	fmt.Fprintf(w, "run %s: %s\n", s.RunID, s.State)
	if s.Reason != "" {
		fmt.Fprintf(w, "终止原因: %s\n", s.Reason)
	}
	fmt.Fprintf(w, "进度: %d/%d 已确认=%d 重复=%d 判错=%d 轮次=%d\n",
		s.Objective.Got, s.Objective.Want,
		s.Public.SubmittedConfirmed, s.Public.Duplicates, s.Public.Rejected, s.Public.Rounds)
	if s.Err != "" {
		fmt.Fprintf(w, "错误: %s\n", s.Err)
	}
}
