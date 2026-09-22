package cli

// 本文件的测试**必须在包内**（`package cli`）而不是 `package cli_test`：
// 「装配缺失」「用法错误」「跑完了但没解出来」都是**不同的 error 类型**（退出码
// 也因此不同），要断言它们就必须拿到 error，而 `Main` 只返回退出码。所以除
// Main 级断言外，还有一组直接调 `app.exec` 的断言——那是唯一能碰到 error 的地方。
//
// ⚠️ **本文件绝不读 `.env`、绝不用真实凭据**：所有 provider / token 相关的地方
// 都用明显的假值（`fake-provider` / `test-only`）。夹具里也不含任何真实平台的
// flag 形态——离线演示题的答案是 `flag{demo-offline-acceptance}`，一眼假。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// ── 假实现：只记账 CLI 到底调了什么，不碰任何真实资源 ──

// fakeRunner 记账 `Harness.Run` 的调用，并返回预置结果。
//
// 它是本文件里唯一「能动引擎」的东西：CLI 与引擎之间的全部耦合都经过
// `runner` 这个窄接口，所以一个这样的 fake 就够覆盖 run 的所有分支。
type fakeRunner struct {
	specs []harness.RunSpec
	// res / err 是下一次 Run 的返回值。err 非空时仍然要返回 res——根包的契约
	// 就是「出错也带回已经跑出来的结果」，CLI 必须把两者都打出来。
	res harness.RunResult
	err error
}

func (f *fakeRunner) Run(_ context.Context, spec harness.RunSpec) (harness.RunResult, error) {
	f.specs = append(f.specs, spec)
	return f.res, f.err
}

// fakeDoctor 记账 `Harness.Doctor` 的调用。
type fakeDoctor struct {
	rep   harness.DoctorReport
	calls int
}

func (f *fakeDoctor) Doctor(context.Context) harness.DoctorReport {
	f.calls++
	return f.rep
}

// fakeResults 是记账型 `harness.ResultStore`。
//
// List / Stats 的返回值可预置，调用参数被记下来——`stats` 的核心风险是
// 「过滤条件翻译错了」（例如把 --profile 写进 Scenario），只有断言**收到的
// StatsQuery** 才能钉住它。
type fakeResults struct {
	runs    []harness.RunResult
	report  harness.StatsReport
	queries []harness.StatsQuery
	err     error
}

func (f *fakeResults) Save(context.Context, harness.RunResult) error { return nil }
func (f *fakeResults) Get(context.Context, harness.RunID) (harness.RunResult, error) {
	return harness.RunResult{}, nil
}
func (f *fakeResults) List(context.Context) ([]harness.RunResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.runs, nil
}
func (f *fakeResults) Stats(_ context.Context, q harness.StatsQuery) (harness.StatsReport, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return harness.StatsReport{}, f.err
	}
	return f.report, nil
}

// ── 测试脚手架 ──

// newTestApp 是测试用的 app：输出全部丢弃（断言走 Main/dispatch 的返回值与
// 捕获的 buffer），需要注入端口时再设 a.Wire。
func newTestApp() *app {
	return &app{stdout: io.Discard, stderr: io.Discard}
}

// wirePorts 造一个只覆盖单次调用的装配函数。
//
// 它顺便断言了「CLI 交给装配层的 storeDir 已经是绝对路径」这条纪律——
// 用一个假的绝对根去对，比在每条用例里各写一遍更不容易漏。
func wirePorts(p Ports) WireFunc {
	return func(string, harness.RunSpec, DeployOptions) (Ports, error) { return p, nil }
}

// ── Main 的对外契约 ──

// TestMainHelpListsAllSubcommands 钉住 --help：退出码 0，且输出含全部 4 个
// 子命令名。用户发现子命令的唯一途径就是这个输出，漏一个等于该子命令不存在。
func TestMainHelpListsAllSubcommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"--help"}, &out, &errb); code != 0 {
		t.Fatalf("Main(--help) = %d，期望 0", code)
	}
	if len(subcommands) != 4 {
		t.Fatalf("子命令数 = %d，v0.4 的子命令面是 doctor/list/run/stats 四个", len(subcommands))
	}
	for _, name := range subcommands {
		// 必须匹配**清单行**（行首两空格 + 名字），不能只 Contains(name)：
		// usageText 的散文里本来就有 "run"（"runner 镜像"）与 "help"（"每个
		// 子命令都支持 --help。"），只做 Contains 的话删掉整行清单也照样绿
		// ——这条断言就白写了。
		if !strings.Contains(out.String(), "\n  "+name+" ") {
			t.Errorf("--help 的子命令清单缺少 %q：\n%s", name, out.String())
		}
	}
	// v0.3 的五个子命令必须**真的消失**：留着它们的清单行等于告诉用户
	// 「这些命令存在」，而 v0.4 没有暂停恢复、control socket、Web 与报告接口。
	for _, gone := range []string{"resume", "pause", "cancel", "serve", "report"} {
		if strings.Contains(out.String(), "\n  "+gone+" ") {
			t.Errorf("--help 仍然列着 v0.3 的子命令 %q：\n%s", gone, out.String())
		}
	}
	if errb.Len() != 0 {
		t.Errorf("--help 不应写 stderr，实际写了 %q", errb.String())
	}
}

// TestMainUnknownSubcommandNamesIt 钉住「拼错子命令」这条最常见的输入：
// 报错必须**点名**用户打的那个词，否则用户只看到一坨用法说明。
//
// 同时钉住退出码 2：脚本要能区分「命令写错了」与「跑起来之后失败了」。
func TestMainUnknownSubcommandNamesIt(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"bogus"}, &out, &errb); code != exitUsage {
		t.Fatalf("未知子命令 = %d，期望 %d；stdout=%q", code, exitUsage, out.String())
	}
	if !strings.Contains(errb.String(), "bogus") {
		t.Errorf("错误消息未点名子命令：%q", errb.String())
	}
	// 被删掉的 v0.3 子命令同样是「未知子命令」：不能因为它在旧文档里出现过
	// 就给一条「已废弃」的软路径——那会让脚本以为它还能用。
	for _, gone := range []string{"pause", "resume", "cancel", "serve", "report"} {
		out.Reset()
		errb.Reset()
		if code := Main([]string{gone, "--store", "/tmp/rh"}, &out, &errb); code != exitUsage {
			t.Errorf("已删除的 %s = %d，期望 %d", gone, code, exitUsage)
		}
		if !strings.Contains(errb.String(), gone) {
			t.Errorf("%s 未点名：%q", gone, errb.String())
		}
	}
}

// TestMainBadFlagNamesTheFlag 钉住 flag 解析失败时的可诊断性：消息里必须有
// **flag 名**，否则用户只知道「有个值不对」。
func TestMainBadFlagNamesTheFlag(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"run", "--budget-rounds=abc"}, &out, &errb); code != exitUsage {
		t.Fatalf("非法 flag 值 = %d，期望 %d", code, exitUsage)
	}
	if !strings.Contains(errb.String(), "budget-rounds") {
		t.Errorf("错误消息未点名 flag：%q", errb.String())
	}
}

// TestSubcommandsReportNotImplementedWithoutWire 钉住「装配层没接线」这条
// 真实状态（`cmd/red-harness` 之外直接调 Main 时就是它）。
//
// 为什么断言 Kind 而不只断言「有错」：装配缺失是**配置**问题（用户该检查部署），
// 与「跑起来之后平台挂了」不是一类。断言 Kind 保证没人把它写成别的类别。
func TestSubcommandsReportNotImplementedWithoutWire(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"doctor", nil},
		{"list", []string{"--store", "/tmp/rh"}},
		{"run", []string{"--scenario", "fake", "--targets", "demo-1", "--store", "/tmp/rh"}},
		{"stats", []string{"--store", "/tmp/rh"}},
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
				t.Fatalf("%s: 未接线时应当返回错误", c.name)
			}
			if !harness.IsKind(err, harness.KindConfig) {
				t.Errorf("%s: 错误类别不是 KindConfig：%v", c.name, err)
			}
			if !strings.Contains(err.Error(), "尚未装配") {
				t.Errorf("%s: 错误消息不含「尚未装配」：%v", c.name, err)
			}
		})
	}
}

// TestMainSubcommandsExitNonZeroWithoutWire 从 Main 这一层再确认一遍。
func TestMainSubcommandsExitNonZeroWithoutWire(t *testing.T) {
	invocations := [][]string{
		{"doctor"},
		{"list", "--store", "/tmp/rh"},
		{"run", "--scenario", "fake", "--store", "/tmp/rh"},
		{"stats", "--store", "/tmp/rh"},
	}
	for _, args := range invocations {
		t.Run(args[0], func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := Main(args, &out, &errb); code != exitFailure {
				t.Fatalf("Main(%v) = %d，期望 %d", args, code, exitFailure)
			}
			if !strings.Contains(errb.String(), "尚未装配") {
				t.Errorf("stderr 不含「尚未装配」：%q", errb.String())
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
		{[]string{"list"}, exitFailure},
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
	if code := Main(nil, &out, &errb); code != exitUsage {
		t.Errorf("无参数 = %d，期望 %d（用户没说要做什么）", code, exitUsage)
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
//
// 提示语必须**按子命令区分**：把用户指向一个不存在的 flag 比不提示更糟。
func TestSubcommandRejectsExtraPositionalArgs(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		hint string
	}{
		{"run", []string{"demo-1"}, "--targets"},
		{"stats", []string{"demo-1"}, "--challenge"},
		{"list", []string{"demo-1"}, "--help"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp()
			err := a.exec(c.name, c.args)
			if err == nil || !strings.Contains(err.Error(), "demo-1") {
				t.Fatalf("多余位置参数应被拒绝且点名：%v", err)
			}
			if !strings.Contains(err.Error(), c.hint) {
				t.Errorf("%s 的提示未指向 %s：%v", c.name, c.hint, err)
			}
		})
	}
}

// ── 接线后的行为：窄接口真的被用上了 ──

// TestInjectWireFeedsMain 断言装配层的注入点真的接进了 Main。
// `cmd/red-harness` 就是靠这个点把 `wire.New` 装进来的。
func TestInjectWireFeedsMain(t *testing.T) {
	// 用 injectWire 的返回值做恢复：它是这个 helper 的**唯一**用法
	// （`t.Cleanup(injectWire(fn))`），手写 prev/cleanup 会让返回值那条
	// 恢复路径永远没有覆盖。
	t.Cleanup(injectWire(func(string, harness.RunSpec, DeployOptions) (Ports, error) {
		return Ports{Results: &fakeResults{runs: []harness.RunResult{{
			RunID: "r-1", Scenario: "fake", Completed: true,
		}}}}, nil
	}))

	var out, errb bytes.Buffer
	if code := Main([]string{"list", "--store", "/tmp/rh"}, &out, &errb); code != 0 {
		t.Fatalf("接线后 list 应当成功，实际 %d（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), "r-1") {
		t.Errorf("list 未打印运行：%q", out.String())
	}
}

// TestSetWireIsTheProductionInjectionPoint 钉住 `SetWire` 与包级 `wired`
// 是同一件事：`cmd/red-harness` 的 init 走的是导出入口，而测试走的是包内的
// `injectWire`——两条路径必须落到同一个变量，否则「生产装上了、测试看到的却是
// 空的」会长期存在而没人发现。
func TestSetWireIsTheProductionInjectionPoint(t *testing.T) {
	defer injectWire(nil)()
	SetWire(func(string, harness.RunSpec, DeployOptions) (Ports, error) {
		return Ports{Doctor: &fakeDoctor{rep: harness.DoctorReport{OK: true}}}, nil
	})
	var out, errb bytes.Buffer
	if code := Main([]string{"doctor"}, &out, &errb); code != 0 {
		t.Fatalf("SetWire 之后 doctor = %d，期望 0（stderr=%q）", code, errb.String())
	}
}

// TestRunWiredMapsFlagsToRunSpec 钉住 flag → RunSpec 的映射。
// 这是 CLI 与引擎之间唯一的「用户意图」翻译层，错一个字段就会让
// 预算护栏或摘要校验失真。
func TestRunWiredMapsFlagsToRunSpec(t *testing.T) {
	eng := &fakeRunner{res: harness.RunResult{
		RunID: "r-1", Scenario: "fake",
		Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}}},
	}}
	var gotStore string
	a := newTestApp()
	a.Wire = func(storeDir string, _ harness.RunSpec, _ DeployOptions) (Ports, error) {
		gotStore = storeDir
		return Ports{Harness: eng}, nil
	}

	args := []string{
		"run", "--scenario", "fake", "--targets", "demo-1, demo-2",
		"--store", "/tmp/rh", "--provider", "fake-provider", "--model", "fake-model", "--image", "test-runner:v1",
		"--budget-rounds", "7", "--budget-turns", "99", "--budget-cost", "1.5",
		"--hint", harness.HintAlways, "--submit=false",
	}
	if code := dispatch(args, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d，期望 0", code)
	}
	if gotStore != "/tmp/rh" {
		t.Errorf("装配函数收到的 storeDir = %q，期望 /tmp/rh", gotStore)
	}
	if len(eng.specs) != 1 {
		t.Fatalf("Run 调用次数 = %d，期望 1", len(eng.specs))
	}
	spec := eng.specs[0]
	if spec.Scenario != "fake" {
		t.Errorf("Scenario = %q", spec.Scenario)
	}
	if len(spec.Targets) != 2 || spec.Targets[0] != "demo-1" || spec.Targets[1] != "demo-2" {
		t.Errorf("Targets = %v，期望按逗号拆分并去空白", spec.Targets)
	}
	// `--store` 刻意**不**在这里断言：v0.4 起运行目录属于装配配置，不进 RunSpec。
	// 它去哪了由 TestRelativeStoreReachesPortsAsAbsolutePath 覆盖。
	if spec.Agent.Provider != "fake-provider" || spec.Agent.Model != "fake-model" {
		t.Errorf("Agent = %+v", spec.Agent)
	}
	if spec.Sandbox.Image != "test-runner:v1" {
		t.Errorf("Sandbox.Image = %q", spec.Sandbox.Image)
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
	eng := &fakeRunner{res: harness.RunResult{RunID: "r-1", Reason: harness.ReasonSolved,
		Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}}}}}
	a := newTestApp()
	a.Wire = wirePorts(Ports{Harness: eng})

	if code := dispatch([]string{"run", "--scenario", "fake", "--store", "/tmp/rh"}, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d", code)
	}
	if len(eng.specs) != 1 {
		t.Fatalf("Run 调用次数 = %d", len(eng.specs))
	}
	want := harness.DefaultBudget()
	if eng.specs[0].Budget != want {
		t.Errorf("默认预算 = %+v，期望 %+v", eng.specs[0].Budget, want)
	}
}

// TestRunSolvedExitsZero 钉住成功路径：有题解出来 ⇒ 退出码 0。
func TestRunSolvedExitsZero(t *testing.T) {
	eng := &fakeRunner{res: harness.RunResult{RunID: "r-1", Scenario: "fake", Completed: true,
		Reason: harness.ReasonCompleted,
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "demo-1"},
			Outcome:   harness.OutcomeView{Reason: harness.ReasonSolved},
		}}}}
	a := newTestApp()
	a.Wire = wirePorts(Ports{Harness: eng})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb

	if code := dispatch([]string{"run", "--store", "/tmp/rh"}, *a); code != exitOK {
		t.Fatalf("有题解出来 = %d，期望 %d（stderr=%q）", code, exitOK, errb.String())
	}
	if !strings.Contains(out.String(), "demo-1") {
		t.Errorf("run 的摘要里没有题号：%q", out.String())
	}
}

// TestRunUnsolvedExitsUnsolved 是 v0.4 第一条纪律的回归测试：
// **「运行无错误」不等于「解题成功」**。跑完了但没解出来必须是**独立的退出码**，
// 否则脚本会把「模型没解出来」当成「跑通了」——前身 280 run / 0 flag 正是这么来的。
func TestRunUnsolvedExitsUnsolved(t *testing.T) {
	eng := &fakeRunner{res: harness.RunResult{RunID: "r-1", Scenario: "fake",
		Reason: harness.ReasonNoProgress,
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "demo-1"},
			Outcome:   harness.OutcomeView{Reason: harness.ReasonNoIntent},
		}}}}
	a := newTestApp()
	a.Wire = wirePorts(Ports{Harness: eng})

	err := a.exec("run", []string{"--store", "/tmp/rh"})
	if err == nil {
		t.Fatal("没有题解出来时 run 必须返回错误")
	}
	var un *unsolvedError
	if !errors.As(err, &un) {
		t.Fatalf("应当是 unsolvedError（退出码 %d），得到 %T: %v", exitUnsolved, err, err)
	}
	if got := exitCode(err); got != exitUnsolved {
		t.Fatalf("退出码 = %d，期望 %d（必须与「出错」区分开）", got, exitUnsolved)
	}
}

// TestRunPrintsResultOnErrorPath 断言**失败路径也要打结果**。
//
// 为什么：一次「跑了 30 轮然后超时」的运行，如果只打一行错误，用户拿不到任何
// 进度信息——而那正是判断「要不要加预算再跑一次」的唯一依据。
func TestRunPrintsResultOnErrorPath(t *testing.T) {
	eng := &fakeRunner{
		res: harness.RunResult{RunID: "r-9", Scenario: "fake", Reason: harness.ReasonError,
			Err: "fakeError",
			Challenges: []harness.ChallengeResult{{
				Challenge: harness.Challenge{Code: "demo-1"},
				Outcome:   harness.OutcomeView{Reason: harness.ReasonError, Rounds: 30},
			}}},
		err: harness.Ef(harness.KindPlatform, "harness.round", "轮次以错误收场", nil),
	}
	a := newTestApp()
	a.Wire = wirePorts(Ports{Harness: eng})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb

	if code := dispatch([]string{"run", "--store", "/tmp/rh"}, *a); code != exitFailure {
		t.Fatalf("出错 = %d，期望 %d", code, exitFailure)
	}
	if !strings.Contains(out.String(), "r-9") || !strings.Contains(out.String(), "轮次 30") {
		t.Errorf("失败路径没打结果摘要：%q", out.String())
	}
}

// TestRunNeverPrintsCandidatePlaintext 钉住明文纪律在**输出面**上的落实。
//
// `OutcomeView.Flags` 是候选明文（契约允许它在返回值里），但打印出来会进终端
// scrollback、工单与 CI 日志。这条测试给一个**明显的假 flag**，断言它一个字都
// 没出现在输出里。
func TestRunNeverPrintsCandidatePlaintext(t *testing.T) {
	const fakeFlag = "flag{test-only-never-print-me}"
	eng := &fakeRunner{res: harness.RunResult{RunID: "r-1", Scenario: "fake",
		Reason: harness.ReasonCompleted,
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "demo-1"},
			Outcome: harness.OutcomeView{Reason: harness.ReasonSolved,
				Flags: []string{fakeFlag}, Submitted: 1},
		}}}}
	a := newTestApp()
	a.Wire = wirePorts(Ports{Harness: eng})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb

	dispatch([]string{"run", "--store", "/tmp/rh"}, *a)
	if strings.Contains(out.String(), fakeFlag) || strings.Contains(errb.String(), fakeFlag) {
		t.Fatalf("候选明文被打印了：stdout=%q stderr=%q", out.String(), errb.String())
	}
}

// TestRunChallengeLinePrintsGraphSaveFailures 钉住「图没落盘」在命令行上是**可见**的。
//
// 图是研究辅助面：写失败不算本题失败（Reason 不变），所以公开指标里没有别的痕迹
// 能说明「这次运行的图没留下来」。此前它只进结果文件，命令行用户完全看不到——
// 而「没写出去」被读成「写了」的代价是：事后拿不到图，却以为图本来就没有。
func TestRunChallengeLinePrintsGraphSaveFailures(t *testing.T) {
	for _, c := range []struct {
		name     string
		failures []string
		wantCol  bool
	}{
		{"图正常落盘", nil, false},
		{"图没写出去", []string{"write"}, true},
		{"未知阶段也要打出来", []string{"unknown"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp()
			a.Wire = wirePorts(Ports{Harness: &fakeRunner{res: harness.RunResult{
				RunID: "r-1", Scenario: "fake",
				Reason: harness.ReasonSolved, State: harness.RunFinished,
				Challenges: []harness.ChallengeResult{{
					Challenge: harness.Challenge{Code: "demo-1"},
					Outcome: harness.OutcomeView{
						Reason: harness.ReasonSolved, GraphSaveFailures: c.failures,
						ProgressConfirmed: 1, ProgressTotal: 1, Submitted: 1, Rounds: 2,
					},
				}},
			}}})
			var out, errb bytes.Buffer
			a.stdout, a.stderr = &out, &errb

			dispatch([]string{"run", "--store", "/tmp/rh"}, *a)
			got := out.String()
			if c.wantCol {
				if !strings.Contains(got, "图未落盘 "+c.failures[0]) {
					t.Fatalf("图落盘失败必须在摘要里可见：%q", got)
				}
				return
			}
			// 常规摘要行的形状不能变。
			const want = "  demo-1\t已解出\t进度 1/1\t确认 1\t重复 0\t判错 0\t轮次 2\t耗时 0s\n"
			if !strings.Contains(got, want) {
				t.Errorf("常规摘要行变了：\n得到 %q\n期望含 %q", got, want)
			}
		})
	}
}

// TestRunSummaryPrintsTerminalState 钉住摘要行的**运行终态**。
//
// 为什么必须打它：`Reason` 与 `State` 回答的是两个不同的问题（见 printRunResult
// 的注释）。少了终态，操作员在终端上分不出「正常跑完但一道题都没解出来」与
// 「跑到一半被取消」——两者的 Reason 可以长得一模一样，而下一步动作正好相反
// （前者要调题目或阈值，后者只要重跑一趟）。
func TestRunSummaryPrintsTerminalState(t *testing.T) {
	for _, c := range []struct {
		name  string
		state harness.RunState
		want  string
	}{
		{"正常结束", harness.RunFinished, "正常结束"},
		{"失败", harness.RunFailed, "失败"},
		{"被取消", harness.RunCancelled, "被取消"},
		// 空串是「未记录」，不是「不认识的值」：混成同一个输出就没法区分
		// 「这份结果没带终态」与「根包加了 CLI 还不认识的终态」。
		{"未记录", "", "未记录"},
		// 未识别的值原样打印——与 reasonText 同一条约定（RunState 是公开 API，
		// 旧值不改、新值可能加，回显原始串比「未知」更有用）。
		{"未知值原样打印", harness.RunState("quarantined"), "quarantined"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp()
			a.Wire = wirePorts(Ports{Harness: &fakeRunner{res: harness.RunResult{
				RunID: "r-1", Scenario: "fake",
				Reason: harness.ReasonNoProgress, State: c.state,
				Challenges: []harness.ChallengeResult{{
					Challenge: harness.Challenge{Code: "demo-1"},
					Outcome:   harness.OutcomeView{Reason: harness.ReasonNoIntent},
				}},
			}}})
			var out, errb bytes.Buffer
			a.stdout, a.stderr = &out, &errb

			dispatch([]string{"run", "--store", "/tmp/rh"}, *a)
			got := out.String()
			if !strings.Contains(got, "终态 "+c.want) {
				t.Fatalf("摘要未打出终态 %q：%q", c.want, got)
			}
			// 终态这一栏**永远**不得退化成「未知」：那正是 stateText 存在的理由。
			if strings.Contains(got, "终态 未知") {
				t.Errorf("终态被打成了「未知」，未识别的值应原样打印：%q", got)
			}
		})
	}
}

// TestRunChallengeLinePrintsBranchesAbandonedOnlyWhenNonZero 钉住换支列的
// **条件打印**，两个方向都要钉：
//
//   - 为 0 时**不许出现**：常规运行里它恒为 0，无条件加一列会让每行都变宽，
//     也会悄悄改掉既有摘要的形状（既有测试正是靠形状在钉别的东西）；
//   - 非 0 时**必须出现**：一次 `no_intent` 收场时，「方向都做完了」与
//     「编排层把几个方向判成停滞扔掉了」指向完全不同的改法，这一列是唯一的
//     现场证据（见 model.go 的 BranchesAbandoned 注释）。
//
// 打印的仍然只是**计数**，不是候选明文。
func TestRunChallengeLinePrintsBranchesAbandonedOnlyWhenNonZero(t *testing.T) {
	for _, c := range []struct {
		name      string
		abandoned int
		wantCol   bool
	}{
		{"没有换支", 0, false},
		{"换过支", 3, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp()
			a.Wire = wirePorts(Ports{Harness: &fakeRunner{res: harness.RunResult{
				RunID: "r-1", Scenario: "fake",
				Reason: harness.ReasonNoProgress, State: harness.RunFinished,
				Challenges: []harness.ChallengeResult{{
					Challenge: harness.Challenge{Code: "demo-1"},
					Outcome: harness.OutcomeView{
						Reason: harness.ReasonNoIntent, BranchesAbandoned: c.abandoned,
						ProgressConfirmed: 1, ProgressTotal: 4, Submitted: 1, Rounds: 3,
					},
				}},
			}}})
			var out, errb bytes.Buffer
			a.stdout, a.stderr = &out, &errb

			dispatch([]string{"run", "--store", "/tmp/rh"}, *a)
			got := out.String()
			if c.wantCol {
				if !strings.Contains(got, "换支 3") {
					t.Fatalf("换支非 0 时必须打出这一列：%q", got)
				}
				return
			}
			if strings.Contains(got, "换支") {
				t.Fatalf("换支为 0 时不得出现这一列（常规摘要的形状不能变）：%q", got)
			}
			// 逐字钉住常规摘要行：这条断言就是「输出形状不变」这句话本身。
			const want = "  demo-1\t意图耗尽\t进度 1/4\t确认 1\t重复 0\t判错 0\t轮次 3\t耗时 0s\n"
			if !strings.Contains(got, want) {
				t.Errorf("常规摘要行变了：\n得到 %q\n期望含 %q", got, want)
			}
		})
	}
}

// TestListOnlyCompletedByDefault 钉住 list 的默认过滤：默认只列**已完成**的运行，
// 要看得显式 --all。
//
// v0.4 的 `RunResult.Completed` 语义是「有一道题达成目标」——与「跑完了」不是
// 一回事，所以「跑过但没解出来」的历史默认不列（否则会迅速淹没列表）。
func TestListOnlyCompletedByDefault(t *testing.T) {
	res := &fakeResults{runs: []harness.RunResult{
		{RunID: "r-done", Scenario: "fake", Completed: true},
		{RunID: "r-ran", Scenario: "fake", Completed: false},
	}}
	a := newTestApp()
	a.Wire = wirePorts(Ports{Results: res})

	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb
	if code := dispatch([]string{"list", "--store", "/tmp/rh"}, *a); code != 0 {
		t.Fatalf("list = %d（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), "r-done") || strings.Contains(out.String(), "r-ran") {
		t.Errorf("默认应只列已完成的运行：%q", out.String())
	}

	out.Reset()
	if code := dispatch([]string{"list", "--store", "/tmp/rh", "--all"}, *a); code != 0 {
		t.Fatalf("list --all = %d（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), "r-ran") {
		t.Errorf("--all 应包含未完成的运行：%q", out.String())
	}
}

// TestListEmptySaysWhy 钉住空列表的输出：必须说清「为什么空」。
//
// 一个什么都不打印的命令会让人以为结果目录是空的，而实际原因常常是「都被默认
// 过滤掉了」——两者的处置完全不同。
func TestListEmptySaysWhy(t *testing.T) {
	a := newTestApp()
	a.Wire = wirePorts(Ports{Results: &fakeResults{}})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb
	if code := dispatch([]string{"list", "--store", "/tmp/rh"}, *a); code != 0 {
		t.Fatalf("list = %d（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), "--all") {
		t.Errorf("空列表要说明 --all 的存在：%q", out.String())
	}
}

// TestStatsPrintsRecallRate 钉住召回率正常路径：分母已知时打百分比与分数。
func TestStatsPrintsRecallRate(t *testing.T) {
	res := &fakeResults{report: harness.StatsReport{
		Runs: 4, Completed: 1, CompletionRate: 0.25,
		ConfirmedFlags: 3, RemainingAtStart: 12, RecallRate: 0.25,
		Score: 3, CostUSD: 0.5, DurationSeconds: 90, HintedRuns: 1, ProviderFailures: 0,
	}}
	a := newTestApp()
	a.Wire = wirePorts(Ports{Results: res})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb

	if code := dispatch([]string{"stats", "--store", "/tmp/rh"}, *a); code != 0 {
		t.Fatalf("stats = %d（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), "25.0%") || !strings.Contains(out.String(), "3/12") {
		t.Errorf("stats 未打印召回率与分子分母：%q", out.String())
	}
	if strings.Contains(out.String(), "不得宣称召回率") {
		t.Errorf("分母已知时不该打「分母未知」：%q", out.String())
	}
}

// TestStatsSaysDenominatorUnknown 是 v0.4 指标纪律的回归测试：
// **分母未知时绝不能宣称召回率**。
//
// `RemainingAtStart` 为 0 表示命中的题目都没报 FlagCount（`store.recallDelta`
// 把它们整体排除在分子分母之外），此时 `StatsReport.RecallRate` 是 0——那是
// 「0/0 不许是 NaN」的规约结果，**不是**「一道都没解出来」。直接把 0 打成
// "0.0%" 就是把「不知道」说成「零」，是报告层最严重的一类谎话。
func TestStatsSaysDenominatorUnknown(t *testing.T) {
	res := &fakeResults{report: harness.StatsReport{
		Runs: 2, Completed: 0,
		ConfirmedFlags: 0, RemainingAtStart: 0, RecallRate: 0,
	}}
	a := newTestApp()
	a.Wire = wirePorts(Ports{Results: res})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb

	if code := dispatch([]string{"stats", "--store", "/tmp/rh"}, *a); code != 0 {
		t.Fatalf("stats = %d（stderr=%q）", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "分母未知，不得宣称召回率") {
		t.Fatalf("分母为 0 时必须明确标注「不得宣称召回率」：%q", got)
	}
	if strings.Contains(got, "召回率：0.0%") {
		t.Fatalf("分母为 0 时不能打 0.0%%（那是把「不知道」说成「零」）：%q", got)
	}
	// 完成率的分母（Runs）不为 0，所以它仍然要打出来——这条区分很关键：
	// 「无样本」只该用在真正没有分母的那个比率上。
	if !strings.Contains(got, "完成率 0.0%") {
		t.Errorf("Runs=2 时完成率应当照常打印：%q", got)
	}
}

// TestStatsZeroRunsSaysNoSample 钉住样本量为 0 时的完成率：
// 0 次运行 ⇒ 完成率的分母也是 0，同样不能打 0.0%。
func TestStatsZeroRunsSaysNoSample(t *testing.T) {
	a := newTestApp()
	a.Wire = wirePorts(Ports{Results: &fakeResults{}})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb
	if code := dispatch([]string{"stats", "--store", "/tmp/rh"}, *a); code != 0 {
		t.Fatalf("stats = %d（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), "无样本") {
		t.Errorf("0 次运行时完成率应报「无样本」：%q", out.String())
	}
}

// TestStatsMapsFiltersToQuery 钉住 flag → StatsQuery 的映射。
//
// 这是过滤语义的唯一翻译层：把 --profile 写进 Scenario（或反过来）会让一次
// 聚合悄悄算错一批运行，而输出上完全看不出来。
func TestStatsMapsFiltersToQuery(t *testing.T) {
	res := &fakeResults{}
	a := newTestApp()
	a.Wire = wirePorts(Ports{Results: res})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb

	args := []string{"stats", "--store", "/tmp/rh",
		"--profile", "p1", "--bundle", "b1", "--model", "m1", "--scenario", "fake",
		"--challenge", "c1", "--category", "web",
		"--since", "2026-01-02", "--until", "2026-01-03T04:05:06Z"}
	if code := dispatch(args, *a); code != 0 {
		t.Fatalf("stats = %d（stderr=%q）", code, errb.String())
	}
	if len(res.queries) != 1 {
		t.Fatalf("Stats 调用次数 = %d，期望 1", len(res.queries))
	}
	q := res.queries[0]
	if q.ProfileDigest != "p1" || q.BundleDigest != "b1" || q.Model != "m1" || q.Scenario != "fake" ||
		q.Challenge != "c1" || q.Category != "web" {
		t.Errorf("过滤条件映射错了：%+v", q)
	}
	// 只给日期时按**本地时区**解释：用户敲 `--since 2026-01-02` 想的是「我本地
	// 那天之后」，按 UTC 解释会让东八区用户丢掉当天早上 8 小时的数据。
	wantSince, _ := time.ParseInLocation("2006-01-02", "2026-01-02", time.Local)
	if !q.Since.Equal(wantSince) {
		t.Errorf("Since = %v，期望 %v（按本地时区）", q.Since, wantSince)
	}
	if q.Until.IsZero() || q.Until.UTC().Hour() != 4 {
		t.Errorf("Until = %v，期望 RFC3339 解析结果", q.Until)
	}
	// 生效的过滤条件必须回显：一份聚合数字脱离过滤条件就没有意义。
	if !strings.Contains(out.String(), "过滤条件：") {
		t.Errorf("stats 未回显过滤条件：%q", out.String())
	}
}

// TestStatsRejectsBadTimeWindow 钉住两种时间输入错误：
// 解析不了的时间、以及写反的窗口。
//
// 写反的窗口**永远匹配不到东西**，而「0 条结果」与「时间写反了」在输出上完全
// 同形——不拒绝就等于让用户去猜。
func TestStatsRejectsBadTimeWindow(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
	}{
		{"since 解析不了", []string{"--since", "昨天"}},
		{"until 解析不了", []string{"--until", "2026/01/02"}},
		{"窗口写反", []string{"--since", "2026-02-01", "--until", "2026-01-01"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp()
			a.Wire = wirePorts(Ports{Results: &fakeResults{}})
			err := a.exec("stats", append([]string{"--store", "/tmp/rh"}, c.args...))
			if err == nil {
				t.Fatal("非法时间窗应当被拒绝")
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（命令写错了，不是运行失败）", got, exitUsage)
			}
		})
	}
}

// TestDoctorUnhealthyExitsNonZero 断言 Fatal 项失败 ⇒ 非 0（model.go 的
// DoctorReport 契约），且 Detail 里只有「是否设置」没有凭据值。
func TestDoctorUnhealthyExitsNonZero(t *testing.T) {
	doc := &fakeDoctor{rep: harness.DoctorReport{
		OK: false,
		Checks: []harness.DoctorCheck{
			{Name: "OPENCODE_API_KEY", OK: true, Detail: "已设置（值不打印）"},
			{Name: "docker", OK: false, Detail: "docker daemon 不可用", Fatal: true},
		},
	}}
	a := newTestApp()
	a.Wire = wirePorts(Ports{Doctor: doc})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb

	if code := dispatch([]string{"doctor"}, *a); code == 0 {
		t.Fatal("有 Fatal 项失败时 doctor 必须返回非 0")
	}
	if doc.calls != 1 {
		t.Errorf("Doctor 调用次数 = %d，期望 1", doc.calls)
	}
	if !strings.Contains(out.String(), "docker") {
		t.Errorf("doctor 未打印检查项：%q", out.String())
	}
}

// TestDoctorOKExitsZero 钉住正向路径：全绿 ⇒ 退出码 0。
func TestDoctorOKExitsZero(t *testing.T) {
	a := newTestApp()
	a.Wire = wirePorts(Ports{Doctor: &fakeDoctor{rep: harness.DoctorReport{
		OK: true, Checks: []harness.DoctorCheck{{Name: "docker", OK: true, Fatal: true}},
	}}})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb
	if code := dispatch([]string{"doctor"}, *a); code != exitOK {
		t.Fatalf("体检通过 = %d，期望 0（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), "体检通过") {
		t.Errorf("doctor 未打印结论：%q", out.String())
	}
}

// TestDoctorJSONOutputsReport 钉住 `--json`：CI 门禁靠它读 `.ok`。
func TestDoctorJSONOutputsReport(t *testing.T) {
	a := newTestApp()
	a.Wire = wirePorts(Ports{Doctor: &fakeDoctor{rep: harness.DoctorReport{
		OK: true, Checks: []harness.DoctorCheck{{Name: "docker", OK: true, Fatal: true}},
	}}})
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb
	if code := dispatch([]string{"doctor", "--json"}, *a); code != exitOK {
		t.Fatalf("doctor --json = %d（stderr=%q）", code, errb.String())
	}
	if !strings.Contains(out.String(), `"ok": true`) {
		t.Errorf("--json 的输出里没有 ok 字段：%q", out.String())
	}
}

// TestWireErrorIsNotMaskedAsNotImplemented 断言装配层返回的真实故障
// （例如 store 目录建不出来）原样冒出来，不被吞成「未装配」。
func TestWireErrorIsNotMaskedAsNotImplemented(t *testing.T) {
	sentinel := harness.Ef(harness.KindPersistence, "store.open", "运行目录不可写", nil)
	a := newTestApp()
	a.Wire = func(string, harness.RunSpec, DeployOptions) (Ports, error) { return Ports{}, sentinel }

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

// ── 退出码：脚本靠它分支，四个码必须各自被钉住 ──

// TestExitCodesAreDistinct 钉住「用法错误 = 2」「跑完但没解出来 = 3」
// 「其它失败 = 1」「成功 = 0」。
//
// cli.go 的注释说脚本靠退出码判断该改命令还是该看日志——如果没人钉住，
// 一次重构就能让几类错误混成一个码而全绿。
func TestExitCodesAreDistinct(t *testing.T) {
	unsolved := &fakeRunner{res: harness.RunResult{RunID: "r-1",
		Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonNoIntent}}}}}
	cases := []struct {
		args []string
		want int
		wire Ports
	}{
		{[]string{"bogus"}, exitUsage, Ports{}},                       // 未知子命令
		{[]string{"help", "bogus"}, exitUsage, Ports{}},               // help 拼错也要非 0
		{[]string{"run", "--bogus"}, exitUsage, Ports{}},              // 未知 flag
		{[]string{"run", "demo-1"}, exitUsage, Ports{}},               // 多余位置参数
		{[]string{"run", "--budget-rounds=abc"}, exitUsage, Ports{}},  // flag 值非法
		{[]string{"stats", "--since", "昨天"}, exitUsage, Ports{}},      // 时间解析不了
		{[]string{"run", "--scenario", "fake"}, exitFailure, Ports{}}, // 装配缺失
		{[]string{"run", "--store", "/tmp/rh"}, exitUnsolved, Ports{Harness: unsolved}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			var out, errb bytes.Buffer
			a := app{stdout: &out, stderr: &errb, Wire: wirePorts(c.wire)}
			if got := dispatch(c.args, a); got != c.want {
				t.Errorf("dispatch(%v) = %d，期望 %d（stderr=%q）", c.args, got, c.want, errb.String())
			}
		})
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

// ── RunID 的路径安全 ──

// TestRunIDRejectsPathEscape 钉住 RunID 的「单个目录名」契约。
//
// 为什么保留它（v0.4 已经没有 --run 这个 flag）：`runIDError` 是 CLI 与 store
// 共享的**输入卫生判据**，而 `list`/`stats` 会把 runID join 成结果文件路径——
// `filepath.Join` 会把 `..` 规整掉，于是 `--run ../other-run` 的落点是
// `<store>/other-run/...`，**整条 runs/ 都被跳过了**。判据留着，将来任何一处
// 接受用户给的 runID 都必须先过它。
func TestRunIDRejectsPathEscape(t *testing.T) {
	bad := []string{"..", "../other-run", "../../etc/x", "a/b", `/abs/path`, ".hidden", " r-1 ", "."}
	for _, id := range bad {
		if err := runIDError(id); err == nil {
			t.Errorf("runIDError(%q) 通过了，期望被拒", id)
		}
	}
	for _, id := range []string{"r-1", "2026-09-20T10-00-00_abc"} {
		if err := runIDError(id); err != nil {
			t.Errorf("runIDError(%q) = %v，期望通过", id, err)
		}
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
// 装配层拿到的是折好的绝对根，而不是用户原样输入的相对路径。
//
// 为什么必须绝对化：装配层按它建结果目录、`list`/`stats` 按它去找。三条路径若
// 各折一次，换个 cwd 就会分叉——而分叉的形态不是报错，是「list 说没有运行」。
//
// v0.4 起 store 根**不再进 RunSpec**（它是部署级配置，不是运行意图），所以这里
// 只钉装配层这一个出口；run 与 list/stats 是否指向同一个根，由
// TestListAndStatsShareRunStoreRoot 覆盖。
func TestRelativeStoreReachesPortsAsAbsolutePath(t *testing.T) {
	eng := &fakeRunner{res: harness.RunResult{RunID: "r-1",
		Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}}}}}
	var gotStore string
	a := newTestApp()
	a.Wire = func(storeDir string, _ harness.RunSpec, _ DeployOptions) (Ports, error) {
		gotStore = storeDir
		return Ports{Harness: eng}, nil
	}
	if code := dispatch([]string{"run", "--scenario", "fake", "--store", "runs"}, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d", code)
	}
	// 断言**确切值**而不只是 IsAbs：IsAbs 对「绝对化成了另一个目录」是瞎的，
	// 而那正是这条测试要防的错。
	want, err := filepath.Abs("runs")
	if err != nil {
		t.Fatal(err)
	}
	if gotStore != want {
		t.Errorf("装配层收到的 storeDir = %q，期望 %q", gotStore, want)
	}
}

// TestListAndStatsShareRunStoreRoot 钉住「run 在哪儿写、list/stats 去哪儿读」
// 用的是同一个绝对根。
//
// 三条路径若各折一次 store 字符串，换个 cwd 就会分叉——而分叉的形态是
// 「list 说没有运行」，不是报错。
func TestListAndStatsShareRunStoreRoot(t *testing.T) {
	var got []string
	a := newTestApp()
	a.Wire = func(storeDir string, _ harness.RunSpec, _ DeployOptions) (Ports, error) {
		got = append(got, storeDir)
		return Ports{Results: &fakeResults{}, Harness: &fakeRunner{
			res: harness.RunResult{RunID: "r-1",
				Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}}}}}}, nil
	}
	a.stdout, a.stderr = io.Discard, io.Discard
	for _, args := range [][]string{
		{"run", "--scenario", "fake", "--store", "runs"},
		{"list", "--store", "runs"},
		{"stats", "--store", "runs"},
	} {
		if code := dispatch(args, *a); code != 0 {
			t.Fatalf("dispatch(%v) = %d", args, code)
		}
	}
	if len(got) != 3 {
		t.Fatalf("装配函数被调用 %d 次，期望 3", len(got))
	}
	for i, s := range got {
		if !filepath.IsAbs(s) || s != got[0] {
			t.Errorf("第 %d 次装配拿到的 storeDir = %q，期望与第一次相同且为绝对路径（%q）", i+1, s, got[0])
		}
	}
}

// TestDeployOptionsCarryFakeChallenges 钉住部署级选项真的被交给了装配层。
//
// 夹具路径走部署选项而不是 RunSpec：夹具里**含答案明文**，而 RunSpec 会整份写进
// 公开的 run.json——它一旦进去，明文就顺着公开面扩散出去了。
func TestDeployOptionsCarryFakeChallenges(t *testing.T) {
	const fixture = "/tmp/fake-only-fixture.json"
	var got DeployOptions
	a := newTestApp()
	a.Deploy = DeployOptions{FakeChallenges: fixture}
	a.Wire = func(_ string, _ harness.RunSpec, d DeployOptions) (Ports, error) {
		got = d
		return Ports{Harness: &fakeRunner{res: harness.RunResult{RunID: "r-1",
			Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}}}}}}, nil
	}
	a.stdout, a.stderr = io.Discard, io.Discard
	if code := dispatch([]string{"run", "--store", "/tmp/rh"}, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d", code)
	}
	if got.FakeChallenges != fixture {
		t.Fatalf("部署选项没传到装配层：%+v", got)
	}
}

// TestDeployFromEnvReadsOnlyPath 钉住环境变量的读取面：只读夹具路径，
// **不读任何凭据**（凭据只经子进程环境变量传给 piai / bridge）。
func TestDeployFromEnvReadsOnlyPath(t *testing.T) {
	t.Setenv(envFakeChallenges, " /tmp/fixture.json ")
	got := deployFromEnv()
	if got.FakeChallenges != "/tmp/fixture.json" {
		t.Fatalf("夹具路径 = %q，期望去空白后的值", got.FakeChallenges)
	}
	t.Setenv(envFakeChallenges, "   ")
	if got := deployFromEnv(); got.FakeChallenges != "" {
		t.Fatalf("空白值应当读成「没给」：%q", got.FakeChallenges)
	}
}

// TestDoctorPassesProviderAndScenarioToAssembly 钉住 `doctor` 的两个 flag 真的传到了
// 装配层。
//
// 回归：体检此前只传零值 RunSpec，于是有两处形同虚设：
//
//   - `provider_credentials` 要靠 provider 名字才知道查哪个环境变量名 ⇒ 恒 FAIL 且
//     是 Fatal ⇒ **`doctor` 永远非零退出**。一条永远不过的检查比没有检查更糟：
//     它教会使用者忽略体检输出，于是真正该看的那几项也一起被忽略。
//   - 场景名恒为空 ⇒ 装配层按 fake 处理 ⇒ **平台侧的检查（VPN 连通、平台 token）
//     从不运行**。而 doctor 的文档定位正是「在任何平台写操作之前体检环境」——
//     一个从不检查 VPN 与 token 的预检，恰好漏掉它唯一存在的理由。
//
// 这里断言的是「传进去了」，不是「判对了」——判据在 wire 的 credentialCheck 与
// 场景装配那边。
func TestDoctorPassesProviderAndScenarioToAssembly(t *testing.T) {
	var gotSpec harness.RunSpec
	a := newTestApp()
	a.Wire = func(_ string, spec harness.RunSpec, _ DeployOptions) (Ports, error) {
		gotSpec = spec
		return Ports{Doctor: &fakeDoctor{rep: harness.DoctorReport{OK: true,
			Checks: []harness.DoctorCheck{{Name: "docker", OK: true, Fatal: true}}}}}, nil
	}
	var out, errb bytes.Buffer
	a.stdout, a.stderr = &out, &errb
	if code := dispatch([]string{"doctor", "--provider", "opencode-go", "--scenario", "tsecbench"}, *a); code != exitOK {
		t.Fatalf("doctor = %d，期望 0（stderr=%q）", code, errb.String())
	}
	if gotSpec.Agent.Provider != "opencode-go" {
		t.Errorf("装配层收到的 provider = %q，期望 opencode-go（不给它这项体检必然 FAIL）",
			gotSpec.Agent.Provider)
	}
	if gotSpec.Scenario != "tsecbench" {
		t.Errorf("装配层收到的 scenario = %q，期望 tsecbench（为空时按 fake 处理，平台侧检查不会运行）",
			gotSpec.Scenario)
	}
	// provider 的名字不是凭据，但 spec 会进公开面：不能因为多加这两个 flag 就把
	// 别的东西（模型、目标、预算）也顺带塞进去。
	if gotSpec.Agent.Model != "" || len(gotSpec.Targets) != 0 {
		t.Errorf("doctor 只该传 provider 与 scenario，实际传了 %+v / %+v", gotSpec.Agent, gotSpec.Targets)
	}
}
