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
	"strings"
	"testing"

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
	storeDir  string
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
		if !strings.Contains(out.String(), name) {
			t.Errorf("--help 输出缺少子命令 %q：\n%s", name, out.String())
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
			a := &app{stdout: io.Discard, stderr: io.Discard}
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
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	var out, errb bytes.Buffer
	code := Main([]string{"run", "--budget-rounds=abc"}, &out, &errb)
	code |= Main([]string{"--help"}, &out, &errb)
	code |= Main([]string{"list"}, &out, &errb)
	if code == 0 {
		t.Errorf("三个调用都应当非 0（前两个之外的 list 在骨架波次也是非 0）")
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
			if !strings.Contains(out.String(), name) {
				t.Errorf("%s --help 的输出未提到自己：%q", name, out.String())
			}
		})
	}
}

// TestSubcommandRejectsExtraPositionalArgs 位置参数在子命令里没有语义，
// 静默忽略会让 `run demo-1`（本意是 --targets demo-1）看起来像跑通了。
func TestSubcommandRejectsExtraPositionalArgs(t *testing.T) {
	a := &app{stdout: io.Discard, stderr: io.Discard}
	err := a.exec("run", []string{"demo-1"})
	if err == nil || !strings.Contains(err.Error(), "demo-1") {
		t.Fatalf("多余位置参数应被拒绝且点名：%v", err)
	}
}

// ── 接线后的行为：窄接口真的被用上了 ──

// TestInjectWireFeedsMain 断言装配层的注入点真的接进了 Main。
// T14 的 wire.go 就是靠这个点把 harness.New 装进来的。
func TestInjectWireFeedsMain(t *testing.T) {
	prev := wired
	t.Cleanup(func() { wired = prev })

	lister := &fakeLister{sums: []harness.RunSummary{{
		RunID: "r-1", State: harness.RunRunning, Scenario: "fake",
	}}}
	injectWire(func(string) (Ports, error) { return Ports{Runs: lister}, nil })

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
	prev := wired
	t.Cleanup(func() { wired = prev })

	lister := &fakeLister{sums: []harness.RunSummary{
		{RunID: "r-live", State: harness.RunRunning, Scenario: "fake"},
		{RunID: "r-done", State: harness.RunCompleted, Scenario: "fake"},
	}}
	injectWire(func(string) (Ports, error) { return Ports{Runs: lister}, nil })

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
	a := &app{stdout: io.Discard, stderr: io.Discard}
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
	a := &app{stdout: io.Discard, stderr: io.Discard}
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
	a := &app{stdout: io.Discard, stderr: io.Discard}
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
	a := &app{stdout: io.Discard, stderr: io.Discard}
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
	a := &app{stdout: io.Discard, stderr: io.Discard}
	a.Wire = func(string) (Ports, error) { return Ports{Doctor: doc}, nil }

	if code := dispatch([]string{"doctor"}, *a); code == 0 {
		t.Fatal("有 Fatal 项失败时 doctor 必须返回非 0")
	}
}

// TestWireErrorIsNotMaskedAsNotImplemented 断言装配层返回的真实故障
// （例如 store 目录建不出来）原样冒出来，不被吞成「未实现」。
func TestWireErrorIsNotMaskedAsNotImplemented(t *testing.T) {
	sentinel := harness.Ef(harness.KindPersistence, "store.open", "运行目录不可写", nil)
	a := &app{stdout: io.Discard, stderr: io.Discard}
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
