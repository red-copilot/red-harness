// Package cli 是 red-harness 的命令行入口。
//
// 三个设计要点（都是为什么长这样，而不是随手写成这样）：
//
//   - **`Main(args, stdout, stderr) int`** 而不是直接读 `os.Args`、写 `os.Stdout`：
//     CLI 的输出必须能被测试捕获。把输出散在 `fmt.Println` 里的话，「--help 是否
//     列全了 8 个子命令」「未知子命令有没有点名」这类断言根本写不出来。
//   - **每个子命令一个 `flag.FlagSet`**：`flag` 包的全局 `CommandLine` 会让子命令
//     之间的 flag 互相污染，而且 `go test` 里 testing 包自己也用它。
//   - **引擎通过本包内定义的窄接口注入**（见 `Ports` 与 `WireFunc`），而不是
//     `import` `engine/`。原因有两条：本波次 `engine/` 还不存在（T11 在建），
//     编译依赖它会让 T10 卡在别人手上；而且 CLI 真正用到的只是
//     `Start`/`Resume`/`List`/`Doctor` 四个动作，写成一个窄接口后测试可以注入
//     记账型 fake，装配层（T14 的 `wire.go`）再把 `harness.New` 接进来。
package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	harness "github.com/red-copilot/red-harness"
)

// subcommands 是全部子命令，顺序即 --help 的输出顺序（PLAN.md:58）。
//
// 为什么是包级变量而不是散在 switch 里：`--help` 必须列全它们，而「列全」这条
// 保证只能靠一份清单来钉（T10 的测试逐名断言）。
var subcommands = []string{"doctor", "list", "run", "resume", "pause", "cancel", "serve", "report"}

// 退出码。脚本靠它判断「这次跑成功了吗」。
//
// 为什么把「尚未实现」单列成 3 而不是复用 1：本波次全部子命令都返回它，核验
// 时能一眼区分「骨架没接上」与「接了但真出错」——后者才是需要看日志的。
const (
	exitOK             = 0
	exitFailure        = 1
	exitUsage          = 2
	exitNotImplemented = 3
)

// ── 注入点 ──

// engine 是 CLI 需要的引擎动作。
//
// ⚠️ **不要**改成直接依赖 `harness.Engine`：接口越小，测试的 fake 越小，而且
// 它把「CLI 到底用了引擎的哪几个方法」变成可读的事实。
// `Pause`/`Resume`/`Cancel` 不在这里——它们走 control socket（见 control.go），
// 因为它们作用的对象是**另一个进程**里的运行，不是本地引擎对象。
type engine interface {
	Start(ctx context.Context, spec harness.RunSpec) (harness.RunHandle, error)
	Resume(ctx context.Context, id harness.RunID) (harness.RunHandle, error)
}

// runLister 覆盖 `harness.Engine.List`。单独一个接口是为了让只读命令
// （`list`）不必拿到一个能改平台状态的引擎。
type runLister interface {
	List(ctx context.Context) ([]harness.RunSummary, error)
}

// doctorReporter 覆盖 `harness.Engine.Doctor`。
type doctorReporter interface {
	Doctor(ctx context.Context) (harness.DoctorReport, error)
}

// Ports 是 CLI 需要的外部能力。**零值表示「装配层还没接线」**——此时需要它的
// 子命令返回 `harness.ErrNotImplemented`。本波次全部子命令都处于这个状态。
type Ports struct {
	Engine  engine
	Runs    runLister
	Doctor  doctorReporter
	Control controlDialer
}

// WireFunc 由装配层提供：给定 StoreDir，接出这一层需要的全部端口。
//
// 参数只有 storeDir，因为它是 CLI 唯一需要先知道的东西（`run.json`、
// `events.jsonl`、`control.sock` 都在 `<storeDir>/runs/<id>/` 下）；其余端口的
// 装配细节属于装配层。**T14 的 `wire.go` 会在里面调 `harness.New`**，那一步
// 会校验必需端口齐备（缺端口 ⇒ KindConfig，启动第一秒就报）。
type WireFunc func(storeDir string) (Ports, error)

// wired 是当前生效的装配函数。为 nil ⇒ 需要端口的子命令报「未实现」。
//
// 为什么是包级变量而不是 `Main` 的参数：`Main(args, stdout, stderr) int` 是
// T10 冻结的对外契约（`main.go` 只做 `os.Exit(cli.Main(...))`），装配点只能
// 藏在包内。测试用 `injectWire` 换掉它，或用 `app.Wire` 只覆盖单次调用。
var wired WireFunc

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
func (a *app) ports(storeDir string) (Ports, error) {
	w := a.Wire
	if w == nil {
		w = wired
	}
	if w == nil {
		return Ports{}, nil
	}
	return w(storeDir)
}

// ── 入口 ──

// Main 是 CLI 的唯一入口。`cmd/red-harness/main.go` 只做
// `os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))`。
//
// 返回退出码而不是自己 `os.Exit`：测试要在同一个进程里跑几十次 Main。
func Main(args []string, stdout, stderr io.Writer) int {
	return dispatch(args, app{stdout: stdout, stderr: stderr})
}

// dispatch 解析子命令名并执行。
func dispatch(args []string, a app) int {
	name, rest, ok := splitSubcommand(args)
	switch {
	case name == "help":
		// `help` / `--help` / `-h` 是**成功路径**：退出码 0、写 stdout。
		// `help run` 顺带打该子命令自己的用法。
		if len(rest) > 0 && isSubcommand(rest[0]) {
			if err := a.exec(rest[0], []string{"--help"}); err != nil {
				return exitCode(err)
			}
			return exitOK
		}
		fmt.Fprint(a.out(), usageText)
		return exitOK
	case !ok:
		if name != "" {
			// 点名用户打的那个词：拼错子命令是最常见的输入，只回一坨用法
			// 等于让用户自己去找错在哪。
			fmt.Fprintf(a.errw(), "未知子命令: %s\n", name)
		}
		fmt.Fprint(a.errw(), usageText)
		return exitUsage
	}
	if err := a.exec(name, rest); err != nil {
		// parseFlags 已经把消息与用法写进 stderr 了，不要再打一遍。
		var rep *reportedError
		if errors.As(err, &rep) {
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
	case "resume":
		return a.resume(args)
	case "pause":
		return a.pause(args)
	case "cancel":
		return a.cancel(args)
	case "serve":
		return a.serve(args)
	case "report":
		return a.report(args)
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

func isSubcommand(name string) bool {
	for _, s := range subcommands {
		if s == name {
			return true
		}
	}
	return false
}

// ── 退出码与错误标记 ──

// notImplementedError 标记「这个子命令本波次还没实现」。
//
// 为什么还要自己包一层 `harness.ErrNotImplemented`：那个函数返回的是
// KindConfig，而 KindConfig 在 T14 之后还会表示「配置错了」（缺端口、摘要漂移）。
// 退出码要区分这两种，只能靠一个显式标记类型——**不要**去匹配消息文本，
// 那正是 errors.go 警告的坑。
type notImplementedError struct{ err error }

func (e *notImplementedError) Error() string { return e.err.Error() }
func (e *notImplementedError) Unwrap() error { return e.err }

// notImplemented 构造「尚未实现」错误。骨架波次每个子命令的收尾都是它，
// T14 逐个换成真实现时测试会从「断言返回未实现」变成「断言真跑通」。
func notImplemented(name string) error {
	return &notImplementedError{err: harness.ErrNotImplemented(name)}
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

// exitCode 把错误映射成退出码。
func exitCode(err error) int {
	var ni *notImplementedError
	if errors.As(err, &ni) {
		return exitNotImplemented
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
		// -budget-rounds: parse error`），这正是可诊断性需要的；再补上用法，
		// 免得用户为了看用法再跑一次 --help。
		fs.Usage()
		fmt.Fprint(a.errw(), buf.String())
		return false, &reportedError{err: perr, code: exitUsage}
	}
	if fs.NArg() > 0 {
		return false, harness.Ef(harness.KindConfig, "cli."+name,
			fmt.Sprintf("%s 不接受位置参数: %s（题目列表请用 --targets）", name,
				strings.Join(fs.Args(), " ")), nil)
	}
	return false, nil
}

// splitList 把逗号分隔的列表拆开并去空白。空串返回 nil（调用方据此判断
// 「没给」而不是「给了一个空项」）。
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// requireRunID 校验 `--run`。
//
// 缺它时**不能**退化成「随便挑一个 run」：pause/cancel/resume 会作用在真实的
// 运行上，猜错一个等于打断了别人的题。
func requireRunID(runID string) error {
	if strings.TrimSpace(runID) == "" {
		return harness.Ef(harness.KindConfig, "cli", "缺少 --run（运行 ID）", nil)
	}
	return nil
}

const usageText = `red-harness —— 进攻性安全 Harness（v0.3.0）

用法：red-harness <子命令> [flags]

子命令：
  doctor    体检：Python SDK / 凭据 / VPN / Docker / runner 镜像 / pi 版本 / provider
  list      列出运行
  run       新建并执行一次运行
  resume    恢复一次运行（以事件日志重放为基线）
  pause     暂停运行中的进程（经 run 目录下的 control socket）
  cancel    取消运行中的进程（经 run 目录下的 control socket）
  serve     本地看板
  report    生成报告

每个子命令都支持 --help。`
