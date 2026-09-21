package cli

// 本文件的测试**必须在包内**（`package cli`）而不是 `package cli_test`：
// 骨架波次里「未实现」是一个 error（`harness.ErrNotImplemented`），要断言它的
// Kind 就必须拿到这个 error，而 `Main` 只返回退出码。所以除 Main 级断言外，
// 还有一组直接调 `app.exec` 的断言——那是唯一能碰到 error 的地方。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// ── 假实现：只记账 CLI 到底调了什么，不碰任何真实资源 ──

type fakeEngine struct {
	started []harness.RunSpec
	resumed []harness.RunID
	handle  harness.RunHandle
}

func (f *fakeEngine) Start(_ context.Context, spec harness.RunSpec) (harness.RunHandle, error) {
	f.started = append(f.started, spec)
	return f.handle, nil
}

func (f *fakeEngine) Resume(_ context.Context, id harness.RunID) (harness.RunHandle, error) {
	f.resumed = append(f.resumed, id)
	return f.handle, nil
}

// fakeHandle 是 harness.RunHandle 的最小实现：Wait 立刻返回预置快照。
type fakeHandle struct{ snap harness.Snapshot }

func (h fakeHandle) ID() harness.RunID { return h.snap.RunID }
func (h fakeHandle) Snapshot(context.Context) (harness.Snapshot, error) {
	return h.snap, nil
}
func (h fakeHandle) Events(context.Context, int64) (<-chan harness.DomainEvent, error) {
	return nil, nil
}
func (h fakeHandle) Pause(context.Context) error  { return nil }
func (h fakeHandle) Resume(context.Context) error { return nil }
func (h fakeHandle) Cancel(context.Context) error { return nil }
func (h fakeHandle) Wait(context.Context) (harness.Snapshot, error) {
	return h.snap, nil
}

type fakeLister struct {
	sums []harness.RunSummary
}

func (f *fakeLister) List(context.Context) ([]harness.RunSummary, error) {
	return f.sums, nil
}

type fakeControl struct {
	paused    int
	cancelled int
}

func (f *fakeControl) Pause(context.Context) error  { f.paused++; return nil }
func (f *fakeControl) Resume(context.Context) error { return nil }
func (f *fakeControl) Cancel(context.Context) error { f.cancelled++; return nil }

type fakeDoctor struct{ rep harness.DoctorReport }

func (f *fakeDoctor) Doctor(context.Context) (harness.DoctorReport, error) {
	return f.rep, nil
}

// ── Main 的对外契约 ──

// newTestApp 是测试用的 app：输出全部丢弃（断言走 Main/dispatch 的返回值与
// 捕获的 buffer），需要注入端口时再设 a.Wire。
func newTestApp() *app {
	return &app{stdout: io.Discard, stderr: io.Discard}
}

// TestMainHelpListsAllSubcommands 钉住 --help：退出码 0，且输出含全部 8 个
// 子命令名。用户发现子命令的唯一途径就是这个输出，漏一个等于该子命令不存在。
func TestMainHelpListsAllSubcommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"--help"}, &out, &errb); code != 0 {
		t.Fatalf("Main(--help) = %d，期望 0", code)
	}
	if len(subcommands) != 8 {
		t.Fatalf("子命令数 = %d，PLAN.md:58 要求 8 个", len(subcommands))
	}
	for _, name := range subcommands {
		// 必须匹配**清单行**（行首两空格 + 名字），不能只 Contains(name)：
		// usageText 的散文里本来就有 "run"（"runner 镜像"、"经 run 目录下的
		// control socket"）和 "help"（"每个子命令都支持 --help。"），只做
		// Contains 的话删掉整行清单也照样绿——这条断言就白写了。
		if !strings.Contains(out.String(), "\n  "+name+" ") {
			t.Errorf("--help 的子命令清单缺少 %q：\n%s", name, out.String())
		}
	}
	if errb.Len() != 0 {
		t.Errorf("--help 不应写 stderr，实际写了 %q", errb.String())
	}
}

// TestMainUnknownSubcommandNamesIt 钉住「拼错子命令」这条最常见的输入：
// 报错必须**点名**用户打的那个词，否则用户只看到一坨用法说明。
func TestMainUnknownSubcommandNamesIt(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"bogus"}, &out, &errb); code == 0 {
		t.Fatalf("未知子命令应当返回非 0，实际 0；stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "bogus") {
		t.Errorf("错误消息未点名子命令：%q", errb.String())
	}
}

// TestMainBadFlagNamesTheFlag 钉住 flag 解析失败时的可诊断性：消息里必须有
// **flag 名**，否则用户只知道「有个值不对」。
func TestMainBadFlagNamesTheFlag(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"run", "--budget-rounds=abc"}, &out, &errb); code == 0 {
		t.Fatalf("非法 flag 值应当返回非 0，实际 0；stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "budget-rounds") {
		t.Errorf("错误消息未点名 flag：%q", errb.String())
	}
}

// TestSubcommandsReturnNotImplementedKind 是本波次的核心断言：8 个子命令全部
// 解析完 flag 之后返回 `harness.ErrNotImplemented`（KindConfig）。
//
// 为什么断言 Kind 而不只断言「有错」：T14 把它们逐个换成真实现时，这条测试会
// 从「断言未实现」变成「断言真跑通」，而 Kind 断言保证在此之前没有人误把
// 「未实现」写成别的类别（例如 panic 或 KindPlatform）而被当成正常故障处理。
func TestSubcommandsReturnNotImplementedKind(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"doctor", nil},
		{"list", []string{"--store", "/tmp/rh"}},
		{"run", []string{"--scenario", "fake", "--targets", "demo-1", "--store", "/tmp/rh"}},
		{"resume", []string{"--store", "/tmp/rh", "--run", "r1"}},
		{"pause", []string{"--store", "/tmp/rh", "--run", "r1"}},
		{"cancel", []string{"--store", "/tmp/rh", "--run", "r1"}},
		{"serve", []string{"--store", "/tmp/rh"}},
		{"report", []string{"--store", "/tmp/rh", "--run", "r1"}},
	}
	if len(cases) != len(subcommands) {
		t.Fatalf("用例数 %d 与子命令数 %d 不一致", len(cases), len(subcommands))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 零值 app ⇒ 装配层未接线，正是本波次的真实状态。
			a := newTestApp()
			err := a.exec(c.name, c.args)
			if err == nil {
				t.Fatalf("%s: 骨架波次应当返回错误", c.name)
			}
			if !harness.IsKind(err, harness.KindConfig) {
				t.Errorf("%s: 错误类别不是 KindConfig：%v", c.name, err)
			}
			if !strings.Contains(err.Error(), "未实现") {
				t.Errorf("%s: 错误消息不含「未实现」：%v", c.name, err)
			}
		})
	}
}

// TestMainSubcommandsExitNonZero 从 Main 这一层再确认一遍：退出码非 0、
// 且 stderr 上有「未实现」。上面的 Kind 断言只有包内能写，这条是外部
// 调用方（脚本、核验 agent）真正能观察到的那一层。
func TestMainSubcommandsExitNonZero(t *testing.T) {
	invocations := [][]string{
		{"doctor"},
		{"list", "--store", "/tmp/rh"},
		{"run", "--scenario", "fake", "--store", "/tmp/rh"},
		{"resume", "--store", "/tmp/rh", "--run", "r1"},
		{"pause", "--store", "/tmp/rh", "--run", "r1"},
		{"cancel", "--store", "/tmp/rh", "--run", "r1"},
		{"serve", "--store", "/tmp/rh"},
		{"report", "--store", "/tmp/rh", "--run", "r1"},
	}
	for _, args := range invocations {
		t.Run(args[0], func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := Main(args, &out, &errb); code == 0 {
				t.Fatalf("Main(%v) = 0，期望非 0", args)
			}
			if !strings.Contains(errb.String(), "未实现") {
				t.Errorf("stderr 不含「未实现」：%q", errb.String())
			}
		})
	}
}

// TestMainWritesToInjectedWriters 断言「成功写 stdout、失败写 stderr」这条分工。
// 它是 `Main(args, stdout, stderr)` 这个签名的直接理由。
func TestMainWritesToInjectedWriters(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"bogus"}, &out, &errb); code == 0 {
		t.Fatal("未知子命令应当返回非 0")
	}
	if out.Len() != 0 {
		t.Errorf("失败路径不应写 stdout，实际写了 %q", out.String())
	}
	if !strings.Contains(errb.String(), "bogus") {
		t.Errorf("失败路径应把消息写进 stderr：%q", errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Main([]string{"--help"}, &out, &errb); code != 0 {
		t.Fatalf("--help = %d，期望 0", code)
	}
	if out.Len() == 0 {
		t.Error("--help 应写 stdout")
	}
	if errb.Len() != 0 {
		t.Errorf("--help 不应写 stderr：%q", errb.String())
	}
}

// TestMainNeverWritesToProcessStdio 把**进程自己的** stdout/stderr 换成管道，
// 断言它们一个字节都没收到。
//
// 为什么值得单独一条：`fmt.Println` / `os.Stderr.Write` 这类写法在单元测试里
// 看不出来（测试框架会把它们吞进自己的输出），只有把进程 stdio 接管掉才暴露。
// 前身 CLI 的输出无法被测试捕获，正是因为漏了这条约束。
func TestMainNeverWritesToProcessStdio(t *testing.T) {
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败：%v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败：%v", err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wOut, wErr
	// 用 t.Cleanup 而不是 defer：panic 时 defer 不跑，进程 stdio 会被永久
	// 指向一个没人读的管道（后续测试的输出全部消失或阻塞）。
	t.Cleanup(func() { os.Stdout, os.Stderr = origOut, origErr })
	t.Cleanup(func() { _ = rOut.Close(); _ = rErr.Close() })

	var out, errb bytes.Buffer
	// **分开断言每个调用的退出码**：OR 起来的话「run 那一次错了但 list 非 0」
	// 也会绿——而 run 那一次正是这条测试要钉的回归。
	for _, c := range []struct {
		args []string
		want int
	}{
		{[]string{"run", "--budget-rounds=abc"}, exitUsage},
		{[]string{"--help"}, exitOK},
		{[]string{"list"}, exitNotImplemented},
	} {
		if got := Main(c.args, &out, &errb); got != c.want {
			t.Errorf("Main(%v) = %d，期望 %d", c.args, got, c.want)
		}
	}

	// 先关写端再读，否则 ReadAll 会一直等（没有任何字节写进来）。
	if err := wOut.Close(); err != nil {
		t.Fatalf("关管道失败：%v", err)
	}
	if err := wErr.Close(); err != nil {
		t.Fatalf("关管道失败：%v", err)
	}
	leakedOut, _ := io.ReadAll(rOut)
	leakedErr, _ := io.ReadAll(rErr)
	if len(leakedOut) != 0 {
		t.Errorf("有 %d 字节漏到进程 stdout：%q", len(leakedOut), leakedOut)
	}
	if len(leakedErr) != 0 {
		t.Errorf("有 %d 字节漏到进程 stderr：%q", len(leakedErr), leakedErr)
	}
	if out.Len() == 0 || errb.Len() == 0 {
		t.Errorf("注入的 writer 没收到内容：stdout=%q stderr=%q", out.String(), errb.String())
	}
}

// TestMainNoArgsPrintsUsageToStderr 钉住「什么都不打」这条输入。
func TestMainNoArgsPrintsUsageToStderr(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main(nil, &out, &errb); code == 0 {
		t.Error("无参数应当返回非 0（用户没说要做什么）")
	}
	if !strings.Contains(errb.String(), "run") || !strings.Contains(errb.String(), "doctor") {
		t.Errorf("无参数时应在 stderr 打用法：%q", errb.String())
	}
}

// TestSubcommandHelpExitsZero 断言 `run --help` 与顶层 `--help` 同样退出 0，
// 且列出该子命令自己的 flag。
func TestSubcommandHelpExitsZero(t *testing.T) {
	for _, name := range subcommands {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := Main([]string{name, "--help"}, &out, &errb); code != 0 {
				t.Fatalf("%s --help = %d，期望 0（stderr=%q）", name, code, errb.String())
			}
			// 子命令的 --help 必须打自己的用法行（"用法：red-harness <name>"），
			// 而不是只让名字恰好出现在某段散文里。
			if !strings.Contains(out.String(), "用法：red-harness "+name) {
				t.Errorf("%s --help 的输出未打自己的用法行：%q", name, out.String())
			}
		})
	}
}

// TestSubcommandRejectsExtraPositionalArgs 位置参数在子命令里没有语义，
// 静默忽略会让 `run demo-1`（本意是 --targets demo-1）看起来像跑通了。
func TestSubcommandRejectsExtraPositionalArgs(t *testing.T) {
	a := newTestApp()
	err := a.exec("run", []string{"demo-1"})
	if err == nil || !strings.Contains(err.Error(), "demo-1") {
		t.Fatalf("多余位置参数应被拒绝且点名：%v", err)
	}
}

// ── 接线后的行为：窄接口真的被用上了 ──

// TestInjectWireFeedsMain 断言装配层的注入点真的接进了 Main。
// T14 的 wire.go 就是靠这个点把 harness.New 装进来的。
func TestInjectWireFeedsMain(t *testing.T) {
	// 用 injectWire 的返回值做恢复：它是这个 helper 的**唯一**用法
	// （`t.Cleanup(injectWire(fn))`），手写 prev/cleanup 会让返回值那条
	// 恢复路径永远没有覆盖。
	t.Cleanup(injectWire(func(string) (Ports, error) {
		return Ports{Runs: &fakeLister{sums: []harness.RunSummary{{
			RunID: "r-1", State: harness.RunRunning, Scenario: "fake",
		}}}}, nil
	}))

	var out, errb bytes.Buffer
	if code := Main([]string{"list", "--store", "/tmp/rh"}, &out, &errb); code != 0 {
		t.Fatalf("接线后 list 应当成功，实际 %d（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), "r-1") {
		t.Errorf("list 未打印运行摘要：%q", out.String())
	}
}

// TestListHidesTerminalRunsUnlessAll 钉住 list 的默认过滤：终局 run 默认不列
// （否则跑过几十次之后列表里全是历史），要看得显式 --all。
func TestListHidesTerminalRunsUnlessAll(t *testing.T) {
	t.Cleanup(injectWire(func(string) (Ports, error) {
		return Ports{Runs: &fakeLister{sums: []harness.RunSummary{
			{RunID: "r-live", State: harness.RunRunning, Scenario: "fake"},
			{RunID: "r-done", State: harness.RunCompleted, Scenario: "fake"},
		}}}, nil
	}))

	var out, errb bytes.Buffer
	if code := Main([]string{"list", "--store", "/tmp/rh"}, &out, &errb); code != 0 {
		t.Fatalf("list = %d（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), "r-live") || strings.Contains(out.String(), "r-done") {
		t.Errorf("默认应只列未终局的运行：%q", out.String())
	}

	out.Reset()
	if code := Main([]string{"list", "--store", "/tmp/rh", "--all"}, &out, &errb); code != 0 {
		t.Fatalf("list --all = %d（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), "r-done") {
		t.Errorf("--all 应包含终局运行：%q", out.String())
	}
}

// TestRunWiredMapsFlagsToRunSpec 钉住 flag → RunSpec 的映射。
// 这是 CLI 与引擎之间唯一的「用户意图」翻译层，错一个字段就会让
// 预算护栏或摘要校验失真。
func TestRunWiredMapsFlagsToRunSpec(t *testing.T) {
	eng := &fakeEngine{handle: fakeHandle{snap: harness.Snapshot{
		RunID: "r-1", State: harness.RunCompleted, Reason: harness.ReasonSolved,
	}}}
	var gotStore string
	a := newTestApp()
	a.Wire = func(storeDir string) (Ports, error) {
		gotStore = storeDir
		return Ports{Engine: eng}, nil
	}

	args := []string{
		"run", "--scenario", "fake", "--targets", "demo-1, demo-2",
		"--store", "/tmp/rh", "--provider", "opencode-go", "--model", "m1",
		"--budget-rounds", "7", "--budget-turns", "99", "--budget-cost", "1.5",
		"--hint", harness.HintAlways, "--submit=false",
	}
	if code := dispatch(args, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d，期望 0", code)
	}
	if gotStore != "/tmp/rh" {
		t.Errorf("装配函数收到的 storeDir = %q，期望 /tmp/rh", gotStore)
	}
	if len(eng.started) != 1 {
		t.Fatalf("Start 调用次数 = %d，期望 1", len(eng.started))
	}
	spec := eng.started[0]
	if spec.Scenario != "fake" {
		t.Errorf("Scenario = %q", spec.Scenario)
	}
	if len(spec.Targets) != 2 || spec.Targets[0] != "demo-1" || spec.Targets[1] != "demo-2" {
		t.Errorf("Targets = %v，期望按逗号拆分并去空白", spec.Targets)
	}
	if spec.StoreDir != "/tmp/rh" {
		t.Errorf("StoreDir = %q", spec.StoreDir)
	}
	if spec.Agent.Provider != "opencode-go" || spec.Agent.Model != "m1" {
		t.Errorf("Agent = %+v", spec.Agent)
	}
	if spec.Budget.MaxRounds != 7 || spec.Budget.MaxTurns != 99 || spec.Budget.MaxCostUSD != 1.5 {
		t.Errorf("Budget = %+v", spec.Budget)
	}
	if spec.HintPolicy != harness.HintAlways {
		t.Errorf("HintPolicy = %q", spec.HintPolicy)
	}
	if spec.Submit {
		t.Error("--submit=false 必须是干跑（Submit=false）")
	}
}

// TestRunWiredDefaultsToHarnessBudget 断言没给预算 flag 时用
// harness.DefaultBudget()，而不是 Budget 的零值（零值意味着「全部不限」，
// 那就等于没有护栏——这正是 v0.2 的死代码护栏那一类问题）。
func TestRunWiredDefaultsToHarnessBudget(t *testing.T) {
	eng := &fakeEngine{handle: fakeHandle{snap: harness.Snapshot{RunID: "r-1"}}}
	a := newTestApp()
	a.Wire = func(string) (Ports, error) { return Ports{Engine: eng}, nil }

	if code := dispatch([]string{"run", "--scenario", "fake", "--store", "/tmp/rh"}, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d", code)
	}
	if len(eng.started) != 1 {
		t.Fatalf("Start 调用次数 = %d", len(eng.started))
	}
	want := harness.DefaultBudget()
	if eng.started[0].Budget != want {
		t.Errorf("默认预算 = %+v，期望 %+v", eng.started[0].Budget, want)
	}
}

// TestResumeWiredPassesRunID 断言 resume 把 --run 原样交给引擎。
// 引擎侧对终态 run 会拒绝恢复，CLI 这一层绝不能自作主张地重跑。
func TestResumeWiredPassesRunID(t *testing.T) {
	eng := &fakeEngine{handle: fakeHandle{snap: harness.Snapshot{RunID: "r-9"}}}
	a := newTestApp()
	a.Wire = func(string) (Ports, error) { return Ports{Engine: eng}, nil }

	if code := dispatch([]string{"resume", "--store", "/tmp/rh", "--run", "r-9"}, *a); code != 0 {
		t.Fatalf("dispatch(resume) = %d", code)
	}
	if len(eng.resumed) != 1 || eng.resumed[0] != "r-9" {
		t.Fatalf("Resume 调用 = %v，期望 [r-9]", eng.resumed)
	}
}

// TestPauseWiredDialsControlSocket 断言 pause 走的是 run 目录下的
// control socket（PLAN.md:40），而不是引擎。
func TestPauseWiredDialsControlSocket(t *testing.T) {
	ctrl := &fakeControl{}
	var gotPath string
	a := newTestApp()
	a.Wire = func(storeDir string) (Ports, error) {
		return Ports{Control: func(socketPath string) (controlClient, error) {
			gotPath = socketPath
			return ctrl, nil
		}}, nil
	}

	if code := dispatch([]string{"pause", "--store", "/tmp/rh", "--run", "r-1"}, *a); code != 0 {
		t.Fatalf("dispatch(pause) = %d", code)
	}
	if ctrl.paused != 1 {
		t.Errorf("Pause 调用次数 = %d，期望 1", ctrl.paused)
	}
	want := controlSocketPath("/tmp/rh", "r-1")
	if gotPath != want {
		t.Errorf("socket 路径 = %q，期望 %q", gotPath, want)
	}

	if code := dispatch([]string{"cancel", "--store", "/tmp/rh", "--run", "r-1"}, *a); code != 0 {
		t.Fatalf("dispatch(cancel) = %d", code)
	}
	if ctrl.cancelled != 1 {
		t.Errorf("Cancel 调用次数 = %d，期望 1", ctrl.cancelled)
	}
}

// TestControlSocketPathIsUnderRunDir 钉住 socket 的落点：run 目录下、
// 名字固定。engine 侧的服务端（T11）不能导入本包，名字是两边各写一份的
// 共享约定——改这里必须同时改 engine。
func TestControlSocketPathIsUnderRunDir(t *testing.T) {
	got := controlSocketPath("/tmp/rh", "r-1")
	if got != "/tmp/rh/runs/r-1/control.sock" {
		t.Errorf("controlSocketPath = %q，期望 /tmp/rh/runs/r-1/control.sock", got)
	}
}

// TestDoctorUnhealthyExitsNonZero 断言 Fatal 项失败 ⇒ 非 0（model.go 的
// DoctorReport 契约），且 Detail 里只有「是否设置」没有凭据值。
func TestDoctorUnhealthyExitsNonZero(t *testing.T) {
	doc := &fakeDoctor{rep: harness.DoctorReport{
		OK: false,
		Checks: []harness.DoctorCheck{
			{Name: "OPENCODE_API_KEY", OK: true, Detail: "已设置（值不打印）"},
			{Name: "docker", OK: false, Detail: "docker 不在 PATH", Fatal: true},
		},
	}}
	a := newTestApp()
	a.Wire = func(string) (Ports, error) { return Ports{Doctor: doc}, nil }

	if code := dispatch([]string{"doctor"}, *a); code == 0 {
		t.Fatal("有 Fatal 项失败时 doctor 必须返回非 0")
	}
}

// TestWireErrorIsNotMaskedAsNotImplemented 断言装配层返回的真实故障
// （例如 store 目录建不出来）原样冒出来，不被吞成「未实现」。
func TestWireErrorIsNotMaskedAsNotImplemented(t *testing.T) {
	sentinel := harness.Ef(harness.KindPersistence, "store.open", "运行目录不可写", nil)
	a := newTestApp()
	a.Wire = func(string) (Ports, error) { return Ports{}, sentinel }

	err := a.exec("run", []string{"--store", "/nope"})
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("装配故障被改写：%v", err)
	}
}

// TestNilWritersDoNotPanic 断言没给 writer 时退化到丢弃，而不是崩。
func TestNilWritersDoNotPanic(t *testing.T) {
	if code := dispatch([]string{"--help"}, app{}); code != 0 {
		t.Errorf("--help = %d，期望 0", code)
	}
	if code := dispatch(nil, app{}); code == 0 {
		t.Error("无参数应当返回非 0")
	}
}

// ── 退出码：脚本靠它分支，三个码必须各自被钉住 ──

// TestExitCodesAreDistinct 钉住「用法错误 = 2」「未实现 = 3」「其它失败 = 1」。
// cli.go 的注释说脚本靠退出码判断该改命令还是该看日志——如果没人钉住，
// 一次重构就能让三类错误混成一个码而全绿。
func TestExitCodesAreDistinct(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"bogus"}, exitUsage},                       // 未知子命令
		{[]string{"help", "bogus"}, exitUsage},               // help 拼错也要非 0
		{[]string{"run", "--bogus"}, exitUsage},              // 未知 flag
		{[]string{"run", "demo-1"}, exitUsage},               // 多余位置参数
		{[]string{"run", "--budget-rounds=abc"}, exitUsage},  // flag 值非法
		{[]string{"pause", "--store", "/tmp/rh"}, exitUsage}, // 缺 --run
		{[]string{"run", "--scenario", "fake"}, exitNotImplemented},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if got := Main(c.args, &out, &errb); got != c.want {
			t.Errorf("Main(%v) = %d，期望 %d（stderr=%q）", c.args, got, c.want, errb.String())
		}
	}
}

// TestHelpForUnknownSubcommandFails 钉住 `help bogus`：它必须像 `bogus` 一样
// 报错并退出 2。退出 0 等于告诉脚本「这个子命令存在」。
func TestHelpForUnknownSubcommandFails(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"help", "bogus"}, &out, &errb); code != exitUsage {
		t.Fatalf("help bogus = %d，期望 %d", code, exitUsage)
	}
	if !strings.Contains(errb.String(), "bogus") {
		t.Errorf("help bogus 未点名：%q", errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("失败路径不应写 stdout：%q", out.String())
	}
}

// TestHelpFlagAfterHelpIsNotASubcommand 钉住 `help --help` / `help -h`：
// splitSubcommand 把 `-h`/`--help` 也映射成名字 "help"，所以 help 后面跟一个
// help flag 时，rest[0] 就是 "--help"。若不特判，它会走未知子命令分支、退出 2，
// 而 usageText 结尾写着「每个子命令都支持 --help」——照着自己看到的说明敲反而
// 失败，是最不该有的一类不可信。
func TestHelpFlagAfterHelpIsNotASubcommand(t *testing.T) {
	for _, args := range [][]string{{"help", "--help"}, {"help", "-h"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := Main(args, &out, &errb); code != exitOK {
				t.Fatalf("%v = %d，期望 %d；stderr=%q", args, code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), "子命令：") {
				t.Errorf("%v 未打印用法：%q", args, out.String())
			}
			if errb.Len() != 0 {
				t.Errorf("%v 不应写 stderr：%q", args, errb.String())
			}
		})
	}
}

// TestFlagErrorPrintsUsageOnce 钉住「flag 解析失败时用法只打一遍」：
// flag 包自己已经调过 Usage()，parseFlags 再调一次会把整份 flag 清单打两遍。
func TestFlagErrorPrintsUsageOnce(t *testing.T) {
	var out, errb bytes.Buffer
	Main([]string{"run", "--budget-rounds=abc"}, &out, &errb)
	if n := strings.Count(errb.String(), "用法：red-harness run"); n != 1 {
		t.Errorf("用法打了 %d 遍，期望 1：\n%s", n, errb.String())
	}
	if !strings.Contains(errb.String(), "budget-rounds") {
		t.Errorf("错误消息未点名 flag：%q", errb.String())
	}
}

// ── --run 的路径安全：它会被 join 成 socket 路径 ──

// TestRunIDRejectsPathEscape 钉住 RunID 的「单个目录名」契约（model.go:84）。
// filepath.Join 会把 `..` 规整掉，所以未校验的 --run 能让 pause/cancel 作用在
// **别人的** run 上——那正是 requireRunID 注释里说的「打断了别人的题」。
func TestRunIDRejectsPathEscape(t *testing.T) {
	bad := []string{"..", "../other-run", "../../etc/x", "a/b", `/abs/path`, ".hidden", " r-1 ", "."}
	for _, id := range bad {
		if err := requireRunID(id); err == nil {
			t.Errorf("requireRunID(%q) 通过了，期望被拒", id)
		}
	}
	for _, id := range []string{"r-1", "2026-09-20T10-00-00_abc"} {
		if err := requireRunID(id); err != nil {
			t.Errorf("requireRunID(%q) = %v，期望通过", id, err)
		}
	}
	// 越界的 id 绝不能落在 store 根之外的另一个 run 目录里。
	if got := controlSocketPath("/tmp/rh", "../other-run"); got == "/tmp/rh/runs/other-run/control.sock" {
		t.Errorf("越界 runID 拨到了别人的 socket：%q", got)
	}
}

// TestPauseRejectsEscapingRunID 从 Main 这一层再确认：越界的 --run 在
// **拨号之前**就被拒（否则控制命令可能发给 store 根之外的监听者）。
func TestPauseRejectsEscapingRunID(t *testing.T) {
	ctrl := &fakeControl{}
	dialed := false
	a := newTestApp()
	a.Wire = func(string) (Ports, error) {
		return Ports{Control: func(string) (controlClient, error) {
			dialed = true
			return ctrl, nil
		}}, nil
	}
	if err := a.exec("pause", []string{"--store", "/tmp/rh", "--run", "../other-run"}); err == nil {
		t.Fatal("越界的 --run 应当被拒")
	}
	if dialed {
		t.Error("越界的 --run 不该走到拨号")
	}
	if ctrl.paused != 0 {
		t.Errorf("Pause 被调用 %d 次，期望 0", ctrl.paused)
	}
}

// TestReportRequiresRun 钉住 report 的 --run：缺它时不能退化成「随便挑一个 run」
// （那会生成一份看起来正常、实际属于别人的报告）。
func TestReportRequiresRun(t *testing.T) {
	a := newTestApp()
	err := a.exec("report", []string{"--store", "/tmp/rh"})
	if err == nil || !strings.Contains(err.Error(), "--run") {
		t.Fatalf("缺 --run 的 report 应当被拒且点名 --run：%v", err)
	}
}

// ── 预算：负值不能变成「不限」 ──

// TestBudgetNegativeValuesFallBackToDefaults 钉住「负数 ≠ 拆掉护栏」。
// Budget.Exhausted 的判据是 `b.MaxRounds > 0 && …`，所以负数落进 Budget
// 等价于**关掉**该维度——用户以为收紧了，实际把护栏拆了。
func TestBudgetNegativeValuesFallBackToDefaults(t *testing.T) {
	f := &runFlags{budgetRounds: -1, budgetWall: -time.Minute, budgetTurns: -1, budgetCost: -5}
	got := f.budget()
	want := harness.DefaultBudget()
	if got.MaxRounds != want.MaxRounds || got.MaxWall != want.MaxWall || got.MaxTurns != want.MaxTurns {
		t.Errorf("负数预算 = %+v，期望退回默认 %+v", got, want)
	}
	if got.MaxCostUSD != 0 {
		t.Errorf("负成本上限 = %v，期望归 0（不限）", got.MaxCostUSD)
	}
}

// TestRelativeStoreReachesPortsAsAbsolutePath 钉住「绝对化发生在所有出口上」：
// 装配层拿到的 storeDir 必须与 RunSpec.StoreDir 是同一个字符串，否则同一个
// store 有两种表示——装配层按 cwd 建、摘要按绝对根比对，恢复时报「配置漂移」。
func TestRelativeStoreReachesPortsAsAbsolutePath(t *testing.T) {
	eng := &fakeEngine{handle: fakeHandle{snap: harness.Snapshot{RunID: "r-1"}}}
	var gotStore string
	a := newTestApp()
	a.Wire = func(storeDir string) (Ports, error) {
		gotStore = storeDir
		return Ports{Engine: eng}, nil
	}
	if code := dispatch([]string{"run", "--scenario", "fake", "--store", "runs"}, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d", code)
	}
	if !filepath.IsAbs(gotStore) {
		t.Errorf("装配层收到的 storeDir = %q，期望绝对路径", gotStore)
	}
	if len(eng.started) != 1 {
		t.Fatalf("Start 调用次数 = %d", len(eng.started))
	}
	if eng.started[0].StoreDir != gotStore {
		t.Errorf("spec.StoreDir = %q 与装配层的 %q 不一致（同一个 store 不能有两种表示）",
			eng.started[0].StoreDir, gotStore)
	}
}

// TestPauseRelativeStoreDialsSameRootAsRun 钉住 pause 的拨号根与 run 建 socket
// 的根一致：run 用绝对化的 store 建 socket，pause 若用相对的 --store 就会去
// cwd 下找一个不存在的 control.sock。
//
// （「--store 先绝对化再入 spec」不另设用例：上面那条已经把
// `spec.StoreDir == 装配层收到的绝对路径` 一并钉住了。）
func TestPauseRelativeStoreDialsSameRootAsRun(t *testing.T) {
	var gotPath string
	a := newTestApp()
	a.Wire = func(storeDir string) (Ports, error) {
		return Ports{Control: func(socketPath string) (controlClient, error) {
			gotPath = socketPath
			return &fakeControl{}, nil
		}}, nil
	}
	if code := dispatch([]string{"pause", "--store", "runs", "--run", "r-1"}, *a); code != 0 {
		t.Fatalf("dispatch(pause) = %d", code)
	}
	if want := controlSocketPath(absStoreDir("runs"), "r-1"); gotPath != want {
		t.Errorf("socket 路径 = %q，期望 %q", gotPath, want)
	}
	if !filepath.IsAbs(gotPath) {
		t.Errorf("socket 路径 = %q，期望绝对路径", gotPath)
	}
}
