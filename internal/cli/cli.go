// Package cli 是 red-harness 的命令行入口。
//
// 三个设计要点（都是为什么长这样，而不是随手写成这样）：
//
//   - **`Main(args, stdout, stderr) int`** 而不是直接读 `os.Args`、写 `os.Stdout`：
//     CLI 的输出必须能被测试捕获。把输出散在 `fmt.Println` 里的话，「--help 是否
//     列全了 4 个子命令」「stats 在分母未知时有没有明确标注」这类断言根本写不出来。
//   - **每个子命令一个 `flag.FlagSet`**：`flag` 包的全局 `CommandLine` 会让子命令
//     之间的 flag 互相污染，而且 `go test` 里 testing 包自己也用它。
//   - **端口通过本包内定义的窄接口注入**（见 `Ports` 与 `WireFunc`），而不是
//     `import` `internal/wire` 或根包的 `*harness.Harness`。原因有两条：CLI 真正
//     用到的只是「跑一次」与「体检一次」两个动作，写成一个窄接口后测试可以注入
//     记账型 fake；而且它把「CLI 到底用了引擎的哪几个方法」变成可读的事实。
//     装配层（`internal/wire`）在 `cmd/red-harness` 里通过 `WireFunc` 接进来。
//
// v0.4 的子命令面是 **doctor / list / run / stats** 四个。v0.3 骨架里的
// `resume`/`pause`/`cancel`/`serve`/`report` 全部删除：v0.4 明确不做暂停恢复、
// control socket、Web 与报告接口（见 docs/PLAN v0.4.md 的「公共接口与行为变更」），
// 运行中取消改用 SIGINT/context。
package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	harness "github.com/red-copilot/red-harness"
)

// subcommands 是全部子命令，顺序即 --help 的输出顺序。
//
// 为什么是包级变量而不是散在 switch 里：`--help` 必须列全它们，而「列全」这条
// 保证只能靠一份清单来钉（测试逐名断言）。
var subcommands = []string{"doctor", "list", "run", "stats"}

// 退出码。脚本靠它判断「这次跑成功了吗」。
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	// exitUnsolved 表示「跑完了，但有题目没解出来」。
	//
	// 为什么必须与 exitFailure 分开：v0.4 的第一条纪律是「运行无错误」不等于
	// 「解题成功」。合成一个码会让「模型没解出来」与「跑的过程中坏了」在脚本
	// 与 CI 里完全同形——而这两件事的处置完全不同（前者是研究结论，后者要查日志）。
	// 与 grep/diff 的 0/1/2 约定同族。
	exitUnsolved = 3
)

// ── 注入点 ──

// runner 是 CLI 需要的一次运行动作。
//
// ⚠️ **不要**改成直接依赖 `*harness.Harness`：接口越小，测试的 fake 越小，而且
// 它把「CLI 到底用了引擎的哪几个方法」变成可读的事实。
type runner interface {
	Run(ctx context.Context, spec harness.RunSpec) (harness.RunResult, error)
}

// doctorReporter 覆盖 `Harness.Doctor`。
type doctorReporter interface {
	Doctor(ctx context.Context) harness.DoctorReport
}

// Ports 是 CLI 需要的外部能力。
//
// **零值表示「装配层还没接线」**：此时需要它的子命令返回「未实现」（退出码 3
// 的历史含义已被 exitUnsolved 复用，所以装配缺失改走 exitFailure）。
type Ports struct {
	Harness runner
	Doctor  doctorReporter
	Results harness.ResultStore
}

// DeployOptions 是装配层需要、但**不属于某一次运行**的部署级选项。
//
// 为什么与 RunSpec 分开：RunSpec 会整份写进 run.json（公开面），而部署选项描述
// 的是「这台机器怎么装」。把题目夹具路径塞进 RunSpec 会让一台机器的目录布局变成
// 运行配置的一部分（换个路径就报「配置漂移」），而且夹具里**含答案明文**，它会
// 顺着 run.json 扩散出去——那正是明文纪律要防的。
//
// ⚠️ **这里不许出现凭据值**：将来若要加 `.env` 路径，也只能是**路径**，不是内容。
type DeployOptions struct {
	// FakeChallenges 是离线场景（`--scenario fake`）的题目夹具 JSON 路径。
	// 为空时装配层用内置演示题（见 internal/wire 的 loadFakeFixture）。
	FakeChallenges string
	// BundleDir 是要只读挂进容器的 extension bundle 目录（宿主绝对路径）。
	//
	// 为什么它是**部署级**而不是运行意图：它回答的是「这台机器上那份扩展包在哪」，
	// 换台机器就是另一个路径，而运行意图（跑哪几道题、花多少预算）不变。放进
	// RunSpec 会让「换个路径跑同一份配置」被报成配置漂移。
	//
	// ⚠️ 它**不是**用来表达「用哪份解法配置」的——那是 `--profile`。这里只指向
	// 那份配置在磁盘上的位置。
	BundleDir string
}

// WireFunc 由装配层提供：给定 storeDir、本次运行的 RunSpec 与部署选项，接出这一
// 层需要的全部端口。
//
// 为什么签名里有 spec：v0.4 的 `Harness.Run(ctx, RunSpec)` 需要**每次调用**的
// 配置（目标、预算、提交开关），而装配层还要用它决定 ResultDir 之外的部署级
// 字段（镜像、profile）。让 CLI 先把 flag 折成 RunSpec 再交给装配层，可以保证
// 「用户意图」只有一份翻译层（`runFlags.spec()`），装配层只补部署级字段。
//
// ⚠️ **spec 会整份写进 run.json（公开文件）**，所以它里面绝不能有凭据。
type WireFunc func(storeDir string, spec harness.RunSpec, deploy DeployOptions) (Ports, error)

// wired 是当前生效的装配函数。为 nil ⇒ 需要端口的子命令报「未实现」。
//
// 为什么是包级变量而不是 `Main` 的参数：`Main(args, stdout, stderr) int` 是
// 冻结的对外契约（`main.go` 只做 `os.Exit(cli.Main(...))`），装配点只能藏在
// 包内。`cmd/red-harness` 用 `SetWire` 安装它，测试用 `injectWire` 换掉它，
// 或用 `app.Wire` 只覆盖单次调用。
var wired WireFunc

// SetWire 安装装配函数。**这是 `cmd/red-harness` 唯一的接线点**。
//
// 为什么必须导出：装配点必须在**进程入口**（`cmd/red-harness`）而不是本包内，
// 否则 `internal/cli` 就得 import `internal/wire`，那会把「CLI 依赖实现包」
// 这条被明令禁止的边加进依赖图，也让 CLI 的测试再也无法注入记账型 fake。
func SetWire(fn WireFunc) { wired = fn }

// injectWire 安装装配函数并返回恢复函数（`t.Cleanup(injectWire(fn))` 即用即还）。
func injectWire(fn WireFunc) func() {
	prev := wired
	wired = fn
	return func() { wired = prev }
}

// ── 调用状态 ──

// app 是一次调用的全部状态。**不要把它做成包级单例**——注入的 writer 是每次
// 调用一份的。
type app struct {
	// stdout / stderr 是注入的输出。为 nil 时退化到丢弃：CLI 不该在没人接
	// 输出的时候崩，也**绝不**偷偷写到进程自己的 stdout。
	stdout io.Writer
	stderr io.Writer
	// Wire 为零值时用包级 wired；显式设置它可以在测试里只影响一次调用。
	Wire WireFunc
	// Deploy 是本次调用的部署级选项（只有 run 会用到）。
	Deploy DeployOptions
}

func (a *app) out() io.Writer {
	if a.stdout != nil {
		return a.stdout
	}
	return io.Discard
}

func (a *app) errw() io.Writer {
	if a.stderr != nil {
		return a.stderr
	}
	return io.Discard
}

// ports 取本次调用需要的端口。wire 为 nil ⇒ 返回零值 Ports（各子命令据此报
// 「未实现」）；wire 返回的错误原样上抛，**不**改写成「未实现」——装配故障
// （例如 store 目录建不出来）必须让用户看见真原因。
func (a *app) ports(storeDir string, spec harness.RunSpec) (Ports, error) {
	w := a.Wire
	if w == nil {
		w = wired
	}
	if w == nil {
		return Ports{}, nil
	}
	return w(storeDir, spec, a.Deploy)
}

// ── 入口 ──

// Main 是 CLI 的唯一入口。`cmd/red-harness/main.go` 只做
// `os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))`。
//
// 返回退出码而不是自己 `os.Exit`：测试要在同一个进程里跑几十次 Main。
//
// 部署级选项（`--fake-challenges`）由 `main.go` 从环境变量读进来：
// `Main` 的签名是冻结的三参数契约，加第四个参数会让**每一个**测试调用点都要改，
// 而那个 flag 只有 `run` 用得上。环境变量本身不含凭据（它是夹具路径），
// 而凭据路径（`.env`）走的是 piai 的向上查找，不需要 CLI 传。
func Main(args []string, stdout, stderr io.Writer) int {
	return dispatch(args, app{stdout: stdout, stderr: stderr, Deploy: deployFromEnv()})
}

// envFakeChallenges 是离线夹具路径的进程级来源。
//
// 为什么走环境变量而不是 CLI flag：夹具路径是**部署**属性（这台机器上离线验收
// 用的是哪份题集），不是某一次运行的意图；做成 flag 会让它出现在 `--help` 的
// run 选项里，看起来像「每次运行都可以换一批题」。它也不是凭据，可以安全地
// 出现在环境里（argv 会被 `ps` 看到，环境变量只有同用户可见）。
const envFakeChallenges = "RED_HARNESS_FAKE_CHALLENGES"

// deployFromEnv 读部署级选项。**只读路径，不读任何凭据值**。
func deployFromEnv() DeployOptions {
	return DeployOptions{FakeChallenges: strings.TrimSpace(os.Getenv(envFakeChallenges))}
}

// dispatch 解析子命令名并执行。
func dispatch(args []string, a app) int {
	name, rest, ok := splitSubcommand(args)
	switch {
	case name == "help":
		// `help` / `--help` / `-h` 是**成功路径**：退出码 0、写 stdout。
		// `help run` 顺带打该子命令自己的用法。
		//
		// ⚠️ **`help bogus` 也必须失败**：退出 0 等于告诉脚本「这个子命令存在」，
		// 而它拼错了。与「未知子命令」走同一条错误路径（点名 + 退出码 2）。
		// `help --help` / `help -h` 是「给 help 自己求用法」，与裸 `--help` 同义。
		// 不特判的话 `--help` 会被当成子命令名走下面的未知分支，退出码 2 并报
		// 「未知子命令: --help」——而 usageText 结尾正写着「每个子命令都支持
		// --help」，用户照着自己看到的说明敲反而失败。
		if len(rest) > 0 && (rest[0] == "-h" || rest[0] == "--help") {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			fmt.Fprint(a.out(), usageText)
			return exitOK
		}
		if !isSubcommand(rest[0]) {
			return unknownSubcommand(a, rest[0])
		}
		if err := a.exec(rest[0], []string{"--help"}); err != nil {
			return exitCode(err)
		}
		return exitOK
	case !ok:
		// name 为空表示一个参数都没给：只打用法，不点名（没词可点）。
		return unknownSubcommand(a, name)
	}
	if err := a.exec(name, rest); err != nil {
		// parseFlags 已经把消息与用法写进 stderr 了，不要再打一遍。
		if rep, ok := errors.AsType[*reportedError](err); ok {
			return rep.code
		}
		fmt.Fprintf(a.errw(), "red-harness %s: %v\n", name, err)
		return exitCode(err)
	}
	return exitOK
}

// exec 按名字分发到子命令实现。参数化是为了让测试能直接断言 error 本身
// （`Main` 只返回退出码，断言不了 error 的 Kind）。
func (a *app) exec(name string, args []string) error {
	switch name {
	case "doctor":
		return a.doctor(args)
	case "list":
		return a.list(args)
	case "run":
		return a.run(args)
	case "stats":
		return a.stats(args)
	}
	return harness.Ef(harness.KindConfig, "cli.dispatch", "未知子命令: "+name, nil)
}

// splitSubcommand 把 args 拆成「子命令名 + 其余参数」。
//
// ok 为假表示 args[0] 不是已知子命令——此时**仍然把名字返回**，好让调用方在
// 错误里点名它。
func splitSubcommand(args []string) (name string, rest []string, ok bool) {
	if len(args) == 0 {
		return "", nil, false
	}
	switch args[0] {
	case "-h", "--help", "help":
		return "help", args[1:], true
	}
	return args[0], args[1:], isSubcommand(args[0])
}

// unknownSubcommand 是「未知子命令」的**唯一**出口：点名 + 打用法 + 退出码 2。
//
// 三条路径共用它（裸的 `bogus`、`help bogus`、以及一个参数都没给），共用一份
// 实现才能保证它们的退出码与输出面永远一致——测试
// `TestHelpForUnknownSubcommandFails` 钉的正是「`help bogus` 与 `bogus` 行为
// 相同」这条不变量，手抄两份的话它只是碰巧成立。
//
// name 为空表示「一个参数都没给」：此时没词可点，只打用法。
func unknownSubcommand(a app, name string) int {
	if name != "" {
		// 点名用户打的那个词：拼错子命令是最常见的输入，只回一坨用法
		// 等于让用户自己去找错在哪。
		fmt.Fprintf(a.errw(), "未知子命令: %s\n", name)
	}
	fmt.Fprint(a.errw(), usageText)
	return exitUsage
}

func isSubcommand(name string) bool {
	return slices.Contains(subcommands, name)
}

// ── 退出码与错误标记 ──

// notImplementedError 标记「这个子命令还没有可用的装配」。
//
// 它现在是装配缺失（`WireFunc` 为 nil）的表达，退出码走 exitFailure。
type notImplementedError struct{ err error }

func (e *notImplementedError) Error() string { return e.err.Error() }
func (e *notImplementedError) Unwrap() error { return e.err }

// notImplemented 构造「装配缺失」错误。
func notImplemented(name string) error {
	return &notImplementedError{err: harness.Ef(harness.KindConfig, "cli."+name,
		name+" 尚未装配：没有可用的引擎端口（WireFunc 未注入）", nil)}
}

// reportedError 表示「消息已经写进 stderr 了，调用方不要再打一遍」。
//
// 为什么需要它：flag 包自己会把错误消息与用法写出来，dispatch 再打一遍就成了
// 两遍；而 `--help` 走的是同一条 flag 路径，却要写 stdout 且退出码为 0。
type reportedError struct {
	err  error
	code int
}

func (e *reportedError) Error() string { return e.err.Error() }
func (e *reportedError) Unwrap() error { return e.err }

// usageError 是「命令行用法写错了」：退出码 2，消息由 dispatch 统一打印。
//
// 为什么与 reportedError 分开：reportedError 的前提是「消息已经打过了」，
// 而这里的消息还没打。缺 `--scenario`、多给了位置参数都属于这一类——它们和
// 「flag 值非法」是同一层错误，退出码必须一致，否则脚本会把「命令写错了」
// 当成「操作失败了」（前者该改命令，后者该看日志）。
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// usagef 构造一个用法错误。
func usagef(op, format string, args ...any) error {
	return &usageError{err: harness.Ef(harness.KindConfig, op, fmt.Sprintf(format, args...), nil)}
}

// unsolvedError 表示「跑完了，但有题目没解出来」：退出码 exitUnsolved。
type unsolvedError struct{ err error }

func (e *unsolvedError) Error() string { return e.err.Error() }
func (e *unsolvedError) Unwrap() error { return e.err }

// exitCode 把错误映射成退出码。
func exitCode(err error) int {
	if _, ok := errors.AsType[*usageError](err); ok {
		return exitUsage
	}
	if _, ok := errors.AsType[*unsolvedError](err); ok {
		return exitUnsolved
	}
	return exitFailure
}

// ── flag 解析 ──

// parseFlags 统一解析一个子命令的 flag，并处理两种「不是错误」与「是错误」的
// 输出归属：
//
//   - `--help`：成功路径。用法写 **stdout**、返回 help=true、退出码 0。
//   - flag 值非法 / 未知 flag：用法与消息写 **stderr**，返回 `*reportedError`。
//   - 多余的位置参数：子命令里位置参数没有语义，**静默忽略会让
//     `run demo-1`（本意是 `--targets demo-1`）看起来像跑通了**，所以拒绝。
func (a *app) parseFlags(name string, fs *flag.FlagSet, args []string) (help bool, err error) {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.Usage = func() {
		fmt.Fprintf(&buf, "用法：red-harness %s [flags]\n\n", name)
		fs.PrintDefaults()
	}
	if perr := fs.Parse(args); perr != nil {
		if errors.Is(perr, flag.ErrHelp) {
			// flag 包已经调过 fs.Usage() 了（`-h` 未定义时走这条路）。
			fmt.Fprint(a.out(), buf.String())
			return true, nil
		}
		// flag 自己的消息就含 flag 名（`invalid value "abc" for flag
		// -budget-rounds: parse error`），这正是可诊断性需要的。用法**已经**
		// 由 flag 包写进 buf 了（Parse 失败时它会调 fs.Usage()），这里再调一次
		// 会把整份用法打两遍。
		fmt.Fprint(a.errw(), buf.String())
		return false, &reportedError{err: perr, code: exitUsage}
	}
	if fs.NArg() > 0 {
		// 用法错误 ⇒ 退出码 2（与 flag 解析失败一致）：脚本要能区分
		// 「命令行写错了」与「跑起来之后失败了」。
		return false, usagef("cli."+name, "%s 不接受位置参数: %s（%s）", name,
			strings.Join(fs.Args(), " "), positionalHint(name))
	}
	return false, nil
}

// positionalHint 给出该子命令正确的写法。
//
// 共用一条提示对四个子命令是错的：doctor/stats 根本没有「题目列表」这种概念。
// 把用户指向一个不存在的 flag，比不提示更糟。
func positionalHint(name string) string {
	switch name {
	case "run":
		return "题目列表请用 --targets"
	case "stats":
		return "过滤条件请用 --challenge / --category 等 flag"
	default:
		return "本子命令没有位置参数，请用 --help 看可用 flag"
	}
}

// splitList 把逗号分隔的列表拆开并去空白。
//
// 空串返回 nil（调用方据此判断「没给」而不是「给了一个空项」）。
//
// ⚠️ **逐项丢弃空项，所以 `--targets=,` 与「没给 --targets」不可区分。** 这是
// 有意的取舍：一个空项只可能来自手滑多打的逗号，报错会让 `--targets "a,b,"`
// 这种常见脚本写法失败；而它**不会**造成静默漏跑——被丢掉的是空串，不是题目
// 名。（真正危险的是 `--targets ""`：那会落成 Targets=nil，即「全部未完成」。
// 想表达「一道都不跑」必须不启动 run，而不是给空列表。）
func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runIDError 报告 RunID 非法。
//
// **共享契约（必须与 store 的 validRunID 判据一致，store/store.go）**：RunID 是
// 单个目录名——非空、是本地相对路径、不是 `.`、不以点开头、不含路径分隔符。
// 两边各写一份是因为 cli 不导入 store（见 docs/architecture.md 的依赖方向）。
//
// ⚠️ 本函数**比 store 多一条**：拒绝前后空白。那是 CLI 特有的输入卫生（flag
// 值可能带空白，而目录名不会），store 收到的是已经规整过的目录名。
//
// 为什么这不是「防手滑」而是「防越界」：RunID 是 run 目录名，而 `list`/`stats`
// 会把它 join 成结果文件路径——`filepath.Join` 会把 `..` 规整掉，于是
// `--run ../other-run` 的落点是 `<store>/other-run/...`，**整条 runs/ 都被跳过
// 了**。改这里必须同时改 store。
func runIDError(id string) error {
	bad := func() error {
		return usagef("cli",
			"运行 ID %q 非法：必须是单个目录名（不得含路径分隔符、不得以点开头、不得为 ..）", id)
	}
	// 前后空白不会被任何一层去掉，却会被原样当成目录名——直接拒绝，避免
	// 「校验通过、拼路径时又变成另一个名字」。
	if strings.TrimSpace(id) != id {
		return bad()
	}
	// `.` 与 `..` 由 IsLocal 判掉（`..` 不是本地路径，`.` 需要显式排除）；
	// `id[0] == '.'` 是共享契约里那条「不得以点开头」。
	if id == "" || !filepath.IsLocal(id) || id == "." || id[0] == '.' {
		return bad()
	}
	if strings.ContainsAny(id, `/\`) {
		return bad()
	}
	return nil
}

const usageText = `red-harness —— 进攻性安全 Harness（v0.4.0-research）

用法：red-harness <子命令> [flags]

子命令：
  doctor    体检：端口齐备 / Docker / runner 镜像 / provider 凭据 / VPN
  list      列出历史运行（只含指标与指纹，无明文）
  run       新建并执行一次运行（Ctrl-C 取消）
  stats     按 profile / 模型 / 场景 / 题目 / 类别 / 时间聚合指标

每个子命令都支持 --help。`
