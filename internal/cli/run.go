package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// runFlags 是 `run` 的运行参数。
//
// 它是**用户意图 → RunSpec** 的唯一翻译层：装配层（`internal/wire`）只补部署级
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

	policyMaxAttempts int
	policyDryRounds   int
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
// 的一部分**——同一个 store 在 /a 与 /b 下起两次会得到两个不同的摘要，比对时报
// 「配置漂移」，而两次其实指向不同的目录（更糟的是不报错的那种）。
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

// storeDir 把 `--store` 折成绝对路径，是**唯一**该用来喂装配层（`a.ports`）
// 与写进 RunSpec.StoreDir 的 store 值。
//
// ⚠️ **不要在这里改回 f.store**：`spec()` 把绝对化后的值写进 RunSpec.StoreDir，
// 而 StoreDir 是 Digest 的一部分、也是比对时的漂移判据。如果装配层拿到的是
// 相对路径（"runs"）而摘要里是 "/abs/cwd/runs"，同一个 store 就有了两种表示：
// 装配层按 cwd 建目录、摘要按另一个根比对，换个 cwd 再跑就报「配置漂移」；
// `list`/`stats` 更直接——它们会去 cwd 下找一个并不存在的结果目录。
// 所以「绝对化」必须发生在**所有**出口上，而不是只发生在 spec 里。
func (f *runFlags) storeDir() string { return absStoreDir(f.store) }

// absStoreDir 把 store 根折成绝对路径（解析失败时退回原值，让 store.New 给出
// 更准确的错误）。
//
// 它同时被 run 与 list/stats 使用：这几条路径必须对同一个 `--store` 得到同一个
// 字符串，否则「run 在哪儿写结果」与「list 去哪儿读结果」会分叉。
func absStoreDir(store string) string {
	if abs, err := filepath.Abs(store); err == nil {
		return abs
	}
	return store
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

	spec := f.spec()
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
func printRunResult(w io.Writer, res harness.RunResult) {
	if res.RunID == "" {
		// 装配/取锁阶段就失败了：没有任何可打印的运行信息。
		return
	}
	fmt.Fprintf(w, "run %s：%s（场景 %s）\n", res.RunID, reasonText(res), res.Scenario)
	for _, c := range res.Challenges {
		// ⚠️ 这里的字段全部是**计数**：`Submitted` 是去重后的确认数，
		// `ProgressConfirmed/Total` 是平台权威进度。**绝不**打印
		// `Outcome.Flags` / `Outcome.Candidates`——它们是候选明文，会进
		// 终端 scrollback、工单与 CI 日志。
		fmt.Fprintf(w, "  %s\t%s\t进度 %d/%d\t确认 %d\t重复 %d\t判错 %d\t轮次 %d\t耗时 %s\n",
			c.Challenge.Code, outcomeReasonText(c.Outcome.Reason),
			c.Outcome.ProgressConfirmed, c.Outcome.ProgressTotal,
			c.Outcome.Submitted, c.Outcome.Duplicates, c.Outcome.Rejected,
			c.Outcome.Rounds, c.Outcome.Duration().Round(time.Second))
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
