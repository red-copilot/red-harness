package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// runFlags 是 `run` 的运行参数。
//
// 它是**用户意图 → RunSpec** 的唯一翻译层：装配层（`local`）只补部署级
// 字段（镜像、profile、结果目录），不再碰这些值。错一个字段就会让预算护栏或
// 摘要校验失真。
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

	budgetRounds int
	budgetWall   time.Duration
	budgetTurns  int
	budgetCost   float64

	hint   string
	submit bool

	policyMaxAttempts    int
	policyDryRounds      int
	policyMaxSubmissions int

	profileFile string
	bundleDir   string
	lockPath    string
}

// register 把 flag 挂到一个 FlagSet 上。名字即 CLI 的公开面，改名字是破坏性变更。
func (f *runFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.store, "store", "runs", "运行目录根（<store>/runs/<runID>/）")
	fs.StringVar(&f.scenario, "scenario", "", "场景名：fake / tsecbench（空则用装配层缺省）")
	fs.StringVar(&f.targets, "targets", "", "题目 code 列表，逗号分隔；空表示全部未完成")

	fs.StringVar(&f.provider, "provider", "", "pi 的 --provider")
	fs.StringVar(&f.model, "model", "", "pi 的 --model")
	fs.StringVar(&f.thinking, "thinking", "", "pi 的 --thinking（off/minimal/low/medium/high/xhigh/max）")
	fs.StringVar(&f.exts, "extensions", "", "extension 文件的**绝对路径**，逗号分隔（走 -e）")
	fs.BoolVar(&f.approve, "approve", true, "pi 的 --approve；不开则项目本地资源被静默忽略")
	fs.StringVar(&f.homeDir, "home", "", "每题独立的 HOME（空则由装配层按容器工作目录生成）")
	fs.StringVar(&f.sessionDir, "session-dir", "", "pi 的 --session-dir")

	fs.StringVar(&f.image, "image", "", "runner 镜像（空则用装配层缺省）")

	// 预算的零值在这里是**「没给」**的标记：给 0 会让 Budget 变成「全部不限」，
	// 那等于没有护栏。所以零值一律替换成 harness.DefaultBudget() 的对应项。
	fs.IntVar(&f.budgetRounds, "budget-rounds", 0, "意图轮次上限（0 表示用默认 40）")
	fs.DurationVar(&f.budgetWall, "budget-wall", 0, "墙钟上限（0 表示用默认 30m）")
	fs.IntVar(&f.budgetTurns, "budget-turns", 0, "工具调用总数上限（0 表示用默认 600）")
	fs.Float64Var(&f.budgetCost, "budget-cost", 0, "累计成本上限 USD（0 表示不限）")

	fs.StringVar(&f.hint, "hint", harness.HintAuto, "提示策略：off / auto / always")
	fs.BoolVar(&f.submit, "submit", false, "是否真向平台提交（默认干跑，只记账）")

	fs.IntVar(&f.policyMaxAttempts, "max-attempts", 0, "同一意图的最大重试轮数（0 表示用默认）")
	fs.IntVar(&f.policyDryRounds, "dry-rounds-before-hint", 0, "连续无进展多少轮后允许请求提示（0 表示用默认）")
	fs.IntVar(&f.policyMaxSubmissions, "max-submissions", 0, "每题向平台提交候选的次数上限（0 表示用默认 50）")

	fs.StringVar(&f.profileFile, "profile", "", "solver profile 的 JSON 文件（未知键/非法值在起跑前拒绝）")
	fs.StringVar(&f.bundleDir, "bundle", "", "只读挂进容器的 extension bundle 目录（同时决定 BundleDigest）")

	// ⚠️ **默认空**：锁文件的默认位置（`<StoreDir>/run.lock`）只能由装配层给出。
	// CLI 自己补一个默认路径就等于把「哪两个进程算同一个部署」复制一份到这一层，
	// 于是同一个 store 经 CLI 与经 SDK 会拿到两把不同的锁——互斥看起来在、实际不在。
	//
	// 它的正当用途只有两种：多 store 分治时让几个 store 共用一把锁；以及集成测试
	// 里**显式**让两个不同 `--store` 的进程争同一把锁（那条测试验的正是锁机制本身）。
	// 指向不同路径的两个进程**互不互斥**，这一点写在帮助文本里，因为它是这个 flag
	// 唯一的危险面。
	fs.StringVar(&f.lockPath, "lock", "", "单运行锁文件路径（空则用装配层缺省 <store>/run.lock；⚠️ 不同路径之间不互斥）")
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
// ⚠️ **这里只放「运行意图与资源上限」**：跑哪几道题、用哪个模型、花多少预算、
// 提不提交。运行目录与凭据来源属于**部署配置**，由装配层持有（见 storeDir 的
// 注释）——它们曾经在 RunSpec 里，代价是每次都被装配层盖掉，调用方以为自己
// 填的值生效了。
func (f *runFlags) spec() (harness.RunSpec, error) {
	profile, err := f.profile()
	if err != nil {
		return harness.RunSpec{}, err
	}
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
		Budget:     f.budget(),
		Sandbox:    harness.SandboxSpec{Image: f.image},
		HintPolicy: f.hint,
		Submit:     f.submit,
		Policy: harness.PolicySpec{
			MaxAttemptsPerIntent:       f.policyMaxAttempts,
			DryRoundsBeforeHint:        f.policyDryRounds,
			MaxSubmissionsPerChallenge: f.policyMaxSubmissions,
		},
		Profile: profile,
	}, nil
}

// profile 把 `--profile` 与 `--bundle` 折成 harness.SolverProfile。
//
// 为什么要从文件读，而不是给 profile 的每个键各开一个 flag：profile 的键是
// **带 schema 的配置面**（PlannerConfig / PromptConfig），一个键一个 flag 会让
// 每加一个键就要动三处（flag、spec 组装、帮助文本），而 schema 校验在文件入口
// 上是一处——`harness.LoadProfile` 就是那个入口。
//
// ⚠️ 解析失败必须在**任何副作用之前**返回：`--profile` 指向一份写错的配置时，
// 不该先起容器、起题、花掉平台额度再报错。这里的调用点在 `a.ports(...)` 之前，
// 而 ports 是装配（会建目录）的那一步。
func (f *runFlags) profile() (harness.SolverProfile, error) {
	var p harness.SolverProfile
	if f.profileFile != "" {
		fh, err := os.Open(f.profileFile)
		if err != nil {
			return p, harness.Ef(harness.KindConfig, "cli.run", "打开 profile 文件失败", err)
		}
		defer fh.Close()
		if p, err = harness.LoadProfile(fh); err != nil {
			return harness.SolverProfile{}, err
		}
	}
	// `--bundle` **不在这里生效**：它是部署级选项（见 cli.DeployOptions.BundleDir），
	// 由装配层经 `local.Options.Sandbox.ProfileDir` 接管。
	//
	// 为什么不让它写进 spec.Profile.ExtensionBundle（改前的做法）：那会让 CLI 造出
	// 一份**非空**的 profile，从而顶掉装配层的默认 profile（`{Name:"default"}`），
	// 于是同一个 bundle 经 CLI 与经 SDK 跑出**两个不同的 ProfileDigest**——
	// 「同一次实验」在报告里被拆成两组。实测发现的：两条路径的 bundleDigest 逐字
	// 相同，而 profileDigest 一个是 d96751574f823ad8 一个是 445d6dc940f6960e。
	//
	// 若同时给了 `--profile` 文件且文件里带了 extensionBundle，**flag 仍然胜出**：
	// 把文件里那个清掉，让部署级的值去填（Runner.resolve 只在运行级为空时补）。
	if f.bundleDir != "" {
		p.ExtensionBundle = ""
	}
	if err := p.Validate(); err != nil {
		return harness.SolverProfile{}, err
	}
	return p, nil
}

// storeDir 把 `--store` 折成绝对路径，是**唯一**该用来喂装配层（`a.ports`）的
// store 值。
//
// ⚠️ **不要在这里改回 f.store**：`--store` 的默认值是一个相对路径（"runs"），
// 而装配层按它建目录、`list`/`stats` 按它找结果目录。同一个 store 若有两种表示，
// 换个 cwd 再跑就会分叉——而分叉的形态不是报错，是「list 说没有运行」。
// 所以「绝对化」必须发生在**所有**出口上。
//
// v0.4 起 store 根**不再进 RunSpec**：它是部署级配置，不是运行意图。它因此也不再
// 是摘要的输入——但绝对化这条要求与摘要无关，它来自「三条路径必须指向同一个目录」。
func (f *runFlags) storeDir() string { return absStoreDir(f.store) }

// absStoreDir 把 store 根折成绝对路径（解析失败时退回原值，让 store.New 给出
// 更准确的错误）。
//
// 它同时被 run 与 list/stats 使用：这几条路径必须对同一个 `--store` 得到同一个
// 字符串，否则「run 在哪儿写结果」与「list 去哪儿读结果」会分叉。`--lock` 与
// `--bundle` 走同一个 absPath，理由同族（见各自的调用点）。
func absStoreDir(store string) string { return absPath(store) }

// absPath 把用户给的路径折成绝对路径（空串原样返回，解析失败退回原值）。
//
// 「解析失败退回原值」是刻意的：本函数的职责是**统一表示**，不是校验。真正
// 打不开那个路径时，报错的应该是打开它的那一层（它能说出为什么），而不是这里。
func absPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// run 执行 `run` 子命令：新建一次运行并等它终局。
//
// **SIGINT 经 context 触发清理**（v0.4 明确要求）：`signal.NotifyContext` 在收到
// Ctrl-C / SIGTERM 时取消 ctx，取消沿着 `Harness.Run` 传到轮循环与 sandbox，
// 由引擎负责回收容器、网络与规则。CLI 这一层**不做任何自己的清理**——它拿不到
// sandbox 句柄，而且「谁创建谁回收」是引擎的契约。
//
// 三条终止路径在退出码上必须可区分：
//
//   - 跑完了且有题解出来  ⇒ 0
//   - 跑完了但没题解出来  ⇒ exitUnsolved（3）
//   - 出错（含被取消）    ⇒ exitFailure（1）
//
// 第三条**包含取消**：Ctrl-C 是用户主动中断，脚本不该把它读成「跑完了」。
func (a *app) run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	f := &runFlags{}
	f.register(fs)
	help, err := a.parseFlags("run", fs, args)
	if help || err != nil {
		return err
	}

	spec, err := f.spec()
	if err != nil {
		return err
	}
	if f.bundleDir != "" {
		// 绝对化在这里做：bundle 路径会经 SandboxSpec.ProfileDir 走到 executor 的
		// 只读挂载校验，而那里**要求绝对路径**——相对路径会在起容器时才被拒，
		// 报错点离用户输入太远。
		a.Deploy.BundleDir = absStoreDir(f.bundleDir)
	}
	if f.lockPath != "" {
		// 绝对化：锁路径决定「哪两个进程算同一个部署」，而相对路径会随 cwd
		// 变化——同一台机器上两个 cwd 不同的进程会静默拿到两把不同的锁。
		a.Deploy.LockPath = absPath(f.lockPath)
	}
	ports, err := a.ports(f.storeDir(), spec)
	if err != nil {
		return err
	}
	if ports.Harness == nil {
		// 装配层还没接线（`cmd/red-harness` 未注入 WireFunc）。
		return notImplemented("run")
	}

	// NotifyContext 在收到第一个信号时取消 ctx；stop 必须调（defer），否则
	// 信号处理器会一直挂着，第二次 Ctrl-C 不会回到默认处置（用户按下去没有反应）。
	//
	// ⚠️ **外面还要再套一层 WithCancel**：`stop()` 只做「注销信号处理器」，
	// **它并不取消 ctx**。少了这层 cancel，一次正常返回之后 ctx 仍然是活的，
	// 引擎里任何按 ctx 退出但还挂着的东西（reader 协程、看门狗）会一直等到
	// 进程结束——表现为「run 已经打完了摘要，进程却迟迟不退」。defer 顺序是
	// 「先 stop 再 cancel」，两者都跑。
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	res, runErr := ports.Harness.Run(ctx, spec)
	// 先打结果再判退出码：**失败路径也要打**，否则一次「跑了 30 轮然后超时」的
	// 运行在终端上只剩一行错误，用户拿不到任何进度信息。
	printRunResult(a.out(), res)
	if runErr != nil {
		if ctx.Err() != nil {
			// 取消是**用户主动**的终止：消息要说明「清理已交给引擎」，而不是
			// 让用户猜容器还在不在。
			return harness.Ef(harness.KindCancelled, "cli.run",
				"运行被取消（SIGINT），清理已交给引擎", runErr)
		}
		return runErr
	}
	if !runSolved(res) {
		// 「跑完了但没解出来」不是错误，但也不是成功——见 exitUnsolved 的注释。
		return &unsolvedError{err: harness.Ef(harness.KindConfig, "cli.run",
			fmt.Sprintf("运行结束但没有任何题目达成目标（reason=%s）", res.Reason), nil)}
	}
	return nil
}

// runSolved 报告这次运行是否**真的解出了题**。
//
// 判据是平台权威的 `ReasonSolved`（Objective.Completed 的映射），不是
// `Completed` 字段——根包的 `runCompleted` 已经把两者区分开了，但 CLI 不该
// 依赖那个内部判据的形状：它只看「有没有一道题的终止原因是 solved」。
func runSolved(res harness.RunResult) bool {
	for _, c := range res.Challenges {
		if c.Outcome.Reason == harness.ReasonSolved {
			return true
		}
	}
	return false
}

// printRunResult 打印一次运行的收尾摘要。
//
// ⚠️ **只打印公开字段**：`OutcomeView.Flags` 是候选明文，它虽然在返回值里
// （契约允许），但打印出来会进终端 scrollback、工单与 CI 日志。所以这里刻意
// 不碰它，只打计数与指纹级别的信息。
//
// 摘要行同时带 `Reason` 与 `State`，因为两者回答的是**不同的问题**：
// `Reason` 是「为什么停的」（超时 / 预算耗尽 / 无进展……），`State` 是
// 「运行怎么结束的」（finished / failed / cancelled）。少了 State，操作员在
// 终端上分不出「正常跑完但一道题没解出来」与「跑到一半被 Ctrl-C 掉」——
// 而这两者的下一步动作完全相反（前者要调题目/阈值，后者只要重跑）。
func printRunResult(w io.Writer, res harness.RunResult) {
	if res.RunID == "" {
		// 装配/取锁阶段就失败了：没有任何可打印的运行信息。
		return
	}
	fmt.Fprintf(w, "run %s：%s（场景 %s，终态 %s）\n",
		res.RunID, reasonText(res), res.Scenario, stateText(res.State))
	for _, c := range res.Challenges {
		// ⚠️ 这里的字段全部是**计数或枚举**：`Submitted` 是去重后的确认数，
		// `Attempts` 是实际调用 Evaluate 的次数，`ProgressConfirmed/Total` 是平台
		// 权威进度，`GraphState` 是四个公开枚举之一。**绝不**打印
		// `Outcome.Flags` / `Outcome.Candidates`——它们是候选明文，会进
		// 终端 scrollback、工单与 CI 日志。
		//
		// `尝试` 与 `确认` **必须都在**：前者是「我们试了多少次」，后者是「平台认了
		// 多少条」。只有确认数时，「跑了 3 次才对」与「一次就中」在终端上完全同形，
		// 而这两件事的下一步动作不同（前者要收紧候选集合/额度，后者是答案质量好）。
		fmt.Fprintf(w, "  %s\t%s\t进度 %d/%d\t确认 %d\t尝试 %d\t重复 %d\t判错 %d\t轮次 %d\t耗时 %s",
			c.Challenge.Code, outcomeReasonText(c.Outcome.Reason),
			c.Outcome.ProgressConfirmed, c.Outcome.ProgressTotal,
			c.Outcome.Submitted, c.Outcome.Attempts, c.Outcome.Duplicates, c.Outcome.Rejected,
			c.Outcome.Rounds, c.Outcome.Duration().Round(time.Second))
		if c.Outcome.BranchesAbandoned > 0 {
			// **只在非 0 时加这一列**：常规运行里它恒为 0，无条件打出来只会让
			// 摘要变宽；而一旦非 0，它就是解释「为什么停」的关键——题目做不动
			// 到底是方向都做完了，还是编排层把几个方向判成停滞扔掉了，改法
			// 完全不同（见 model.go 的 BranchesAbandoned 注释）。
			fmt.Fprintf(w, "\t换支 %d", c.Outcome.BranchesAbandoned)
		}
		// 图产物这一栏**无条件打印**，与 `换支` 的「非 0 才打」刻意不同：
		// 四态要能互相区分，而「没图」有三种、处置完全不同——disabled 是这次部署
		// 压根没开图（改装配层），absent 是开了但这一题没登记过图（题没走到那一步），
		// failed 是登记了却没写出去（查落盘面）。把它们压成一个「没图」等于让操作员
		// 去猜该动哪一层；而只在失败时打印，`absent` 与 `saved` 又会长得一模一样。
		//
		// 卡在哪一步（阶段枚举）只在失败时附上：那是可执行的下一步，而成功时
		// 它只是一个恒为空的东西。
		fmt.Fprintf(w, "\t图 %s", graphStateText(c.Outcome.GraphState))
		if len(c.Outcome.GraphSaveFailures) > 0 {
			fmt.Fprintf(w, "（阶段 %s）", strings.Join(c.Outcome.GraphSaveFailures, ","))
		}
		fmt.Fprintln(w)
		if c.Outcome.AuditIncomplete {
			// ⚠️ **单独一行、单独一个前缀**，绝不挤进上面那条计数行。
			//
			// 候选审计是「提交了什么、平台怎么判的」这件事的**唯一**记录，没落全
			// 就没有任何别的东西能回答它（图写不出去只是少一份研究材料，审计写不
			// 出去是结论本身不可核对）。所以它必须被读见：此前它只进结果文件的
			// 一个布尔位，命令行用户完全看不到，而这正是「失败要报失败」在输出面上
			// 的落实。它与根包的 ErrAuditIncomplete 配对，Run 会因此停下来。
			fmt.Fprintf(w, "  ！！%s 的候选审计不完整：本次提交记录没有全部落盘，"+
				"「提交了什么、平台怎么判的」已不可回答（先修落盘面再重跑）\n", c.Challenge.Code)
		}
	}
	if res.Err != "" {
		// res.Err 是根包折叠过的分类串（`%T`），不含响应体/路径/凭据。
		fmt.Fprintf(w, "错误分类：%s\n", res.Err)
	}
}

// reasonText 把 run 级 Reason 翻成中文。
//
// 未识别的值原样打印：Reason 是**公开 API**（新增不允许改旧值），打印原始串
// 比打印「未知」更有用——后者会让一次新增 Reason 的升级看起来像 bug。
func reasonText(res harness.RunResult) string {
	switch res.Reason {
	case harness.ReasonSolved:
		return "已解出"
	case harness.ReasonCompleted:
		return "跑完了"
	case harness.ReasonNoProgress:
		return "跑完了但无进展"
	case harness.ReasonError:
		return "以错误收场"
	case harness.ReasonStopped:
		return "被取消"
	case harness.ReasonTimeout:
		return "超时"
	case harness.ReasonMaxRounds:
		return "轮次预算耗尽"
	case harness.ReasonMaxTurns:
		return "工具调用预算耗尽"
	case harness.ReasonMaxCost:
		return "成本预算耗尽"
	case harness.ReasonNoIntent:
		return "意图前沿耗尽"
	case harness.ReasonProviderFailure:
		return "provider 故障"
	}
	if res.Reason == "" {
		return "未知（无 Reason）"
	}
	return res.Reason
}

// stateText 把运行终态翻成中文。
//
// 未识别的值**原样打印**，与 reasonText 同一条理由：`RunState` 是公开 API
// （旧值永不改动、新值可能增加），把原始串打出来比打「未知」更有用——后者会
// 让一次「根包新增终态」的升级看起来像 CLI 出了 bug。
//
// 空串单独处理：它表示**未记录**（例如由装配层直接构造、或从 v0.3 的旧数据
// 读回来的结果），与「记录了一个我们不认识的值」是两回事。
//
// 这里把根包 `RunState` 的**全部**常量都映射了，尽管 `RunResult.State` 只会
// 是终态那三个（finished / failed / cancelled）：映射表是这个类型的完整翻译，
// 留几个洞反而会让下一次改动分不清「故意不映射」与「忘了」。
func stateText(s harness.RunState) string {
	switch s {
	case harness.RunFinished:
		return "正常结束"
	case harness.RunFailed:
		return "失败"
	case harness.RunCancelled:
		return "被取消"
	case harness.RunCompleted:
		// v0.3 的旧终态值：新运行不再产生它，但旧快照的读取路径仍在。
		return "已完成"
	case harness.RunCreated:
		return "已创建"
	case harness.RunPreparing:
		return "准备中"
	case harness.RunRunning:
		return "运行中"
	case harness.RunPaused:
		return "已暂停"
	case "":
		return "未记录"
	}
	return string(s)
}

// graphStateText 把「本题图产物的实际结果」翻成中文。
//
// 四个枚举值**逐个映射**，一个都不省：「没图」在公开面里有三种不同的成因，
// 把它们合起来就等于把这个字段存在的理由（见 ports.go 的 GraphState 注释）
// 从输出面上抹掉。
//
// 空串与未识别的值分开处理，与 stateText 同一条约定：空串是**未记录**（结果由
// 装配层直接构造、或从旧数据读回），未识别的值是「根包加了新枚举而 CLI 还不认识」
// ——前者该去查谁没填，后者回显原始串才看得出发生了什么。
func graphStateText(s harness.GraphState) string {
	switch s {
	case harness.GraphDisabled:
		return "未启用"
	case harness.GraphAbsent:
		return "无产物"
	case harness.GraphSaved:
		return "已保存"
	case harness.GraphFailed:
		return "失败"
	}
	if s == "" {
		return "未记录"
	}
	return string(s)
}

// outcomeReasonText 把题级 Reason 翻成中文（只覆盖题级会出现的几个）。
func outcomeReasonText(reason string) string {
	switch reason {
	case harness.ReasonSolved:
		return "已解出"
	case harness.ReasonNoIntent:
		return "意图耗尽"
	case harness.ReasonStopped:
		return "被取消"
	case harness.ReasonError:
		return "出错"
	case harness.ReasonMaxRounds:
		return "轮次耗尽"
	case harness.ReasonMaxTurns:
		return "turns 耗尽"
	case harness.ReasonMaxCost:
		return "成本耗尽"
	case harness.ReasonTimeout:
		return "超时"
	case harness.ReasonProviderFailure:
		return "provider 故障"
	case harness.ReasonCompleted:
		return "跑完了"
	case "":
		return "未记录"
	}
	return reason
}

// list 执行 `list` 子命令：列出历史运行。
//
// 数据源是 `harness.ResultStore`（公开结果），**不是** v0.3 的 `Engine.List`：
// v0.4 的 Harness 没有「列出运行中状态」这个能力（没有暂停恢复、没有事件重放），
// 运行结束后才有落盘的结果。所以列表反映的是「跑过哪些 run」，而不是「现在有
// 哪些在跑」——想看正在跑的，看那个进程的终端。
//
// **输出里绝不含明文**：ResultStore 的公开 schema 只有指标、指纹与配置摘要
// （`store/results.go` 的白名单式构造），`Flags`/`Candidates` 从不落盘。
func (a *app) list(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var (
		store = fs.String("store", "runs", "运行目录根")
		all   = fs.Bool("all", false, "包含未完成的运行（默认只列已完成的）")
	)
	help, err := a.parseFlags("list", fs, args)
	if help || err != nil {
		return err
	}

	ports, err := a.ports(absStoreDir(*store), harness.RunSpec{})
	if err != nil {
		return err
	}
	if ports.Results == nil {
		return notImplemented("list")
	}
	runs, err := ports.Results.List(context.Background())
	if err != nil {
		return err
	}
	printRunList(a.out(), runs, *all)
	return nil
}

// printRunList 打印运行列表。
//
// 表头只在有数据时打印：空列表打一行表头看起来像「有一条空记录」。
func printRunList(w io.Writer, runs []harness.RunResult, all bool) {
	shown := 0
	for _, r := range runs {
		// `Completed` 的语义是「有一道题达成目标」。默认只列它们，是因为
		// 「跑过但没解出来」的历史会迅速淹没列表；要看全部得显式 --all。
		if !all && !r.Completed {
			continue
		}
		if shown == 0 {
			fmt.Fprintln(w, "RUN_ID\t场景\t完成\t开始时间\t耗时\t确认/剩余")
		}
		shown++
		confirmed, remaining := 0, 0
		for _, c := range r.Challenges {
			confirmed += c.Outcome.ProgressConfirmed
			remaining += c.Outcome.RemainingAtStart
		}
		fmt.Fprintf(w, "%s\t%s\t%v\t%s\t%s\t%d/%d\n",
			r.RunID, r.Scenario, r.Completed, r.StartedAt.Format(time.RFC3339),
			r.EndedAt.Sub(r.StartedAt).Round(time.Second), confirmed, remaining)
	}
	if shown == 0 {
		// 空列表要说清「为什么空」：默认过滤掉了未完成的运行是最常见的原因，
		// 而一个什么都不打印的命令会让人以为结果目录是空的。
		fmt.Fprintln(w, "没有可列出的运行（未完成的运行要用 --all 才列出）")
	}
}
