package piai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

var stubBin string

// TestMain 编译 stub pi 一次，所有用例共用。必须是**独立进程**：writer 死锁
// 只在真管道上复现（stdin 管道写满才阻塞），进程内的假 reader 永远写不满。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "piai-stub")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stubBin = filepath.Join(dir, "stubpi")
	cmd := exec.Command("go", "build", "-o", stubBin, "./testdata/stubpi")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "编译 stubpi 失败:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// newAgent 造一个指向 stub 的 Agent。默认把超时压到秒级：真跑时 StallTimeout
// 是分钟级，测试不能等。
func newAgent(t *testing.T, scenario string, extraEnv map[string]string) (*Agent, string) {
	t.Helper()
	wd := t.TempDir()
	envFile := filepath.Join(wd, ".env")
	// 空的 .env：让 provider 预检在「没指定 provider」时直接跳过。
	if err := os.WriteFile(envFile, []byte("# 测试用空 .env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(wd, "notes.txt")
	t.Setenv("STUBPI_SCENARIO", scenario)
	t.Setenv("STUBPI_NOTES", notes)
	t.Setenv("STUBPI_CRLF", "")
	t.Setenv("STUBPI_U2028", "")
	t.Setenv("STUBPI_SPAWN_CHILD", "")
	for k, v := range extraEnv {
		t.Setenv(k, v)
	}
	a := &Agent{
		BinPath:        stubBin,
		EnvFile:        envFile,
		Workdir:        wd,
		SessionDir:     filepath.Join(wd, "sessions"),
		StallTimeout:   3 * time.Second,
		ProbeTimeout:   2 * time.Second,
		AbortGrace:     2 * time.Second,
		RestartBackoff: 10 * time.Millisecond,
		MaxRestarts:    2,
	}
	return a, notes
}

func startAgent(t *testing.T, a *Agent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Start(ctx, harness.AgentStart{Workdir: a.Workdir}); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
}

func notesOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func contains[T comparable](xs []T, v T) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ─────────────────────── 正常一轮 ───────────────────────

func TestRoundNormal(t *testing.T) {
	a, notes := newAgent(t, "normal", nil)
	startAgent(t, a)

	var kinds []harness.EventKind
	var toolEnd harness.Event
	res, err := a.Round(context.Background(), "侦察目标", func(ev harness.Event) {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == harness.EventToolEnd {
			toolEnd = ev
		}
	})
	if err != nil {
		t.Fatalf("Round 返回错误: %v", err)
	}
	if res.Err != "" {
		t.Fatalf("RoundResult.Err 应为空: %s", res.Err)
	}
	if res.Reason != harness.ReasonCompleted {
		t.Errorf("Reason = %q", res.Reason)
	}
	// Turns 语义 = 数 tool_execution_start（护栏依赖，不要改）。
	if res.Turns != 1 {
		t.Errorf("Turns = %d，期望 1", res.Turns)
	}
	if res.Text != "预热。第一段文本。第二段文本。" {
		t.Errorf("Text = %q", res.Text)
	}
	if toolEnd.Tool != "bash" || toolEnd.ToolCallID != "call_1" {
		t.Errorf("tool_end 缺 tool/callId: %+v", toolEnd)
	}
	if toolEnd.Output != "total 0\n" {
		t.Errorf("tool_end Output = %q", toolEnd.Output)
	}
	if toolEnd.IsError {
		t.Error("IsError 应为 false")
	}
	// details 原样到达（262 KB 通道）：report_fact 的结构化事实靠它。
	rf, ok := toolEnd.Details["report_fact"].(map[string]any)
	if !ok {
		t.Fatalf("details 里没有 report_fact: %#v", toolEnd.Details)
	}
	if rf["next"] != "试试 /admin" {
		t.Errorf("report_fact.next = %v", rf["next"])
	}
	if s, _ := rf["filler"].(string); len(s) != 4096 {
		t.Errorf("filler 长度 = %d，期望 4096（details 被截断？）", len(s))
	}
	if !contains(kinds, harness.EventSettled) || !contains(kinds, harness.EventToolStart) {
		t.Errorf("事件种类不全: %v", kinds)
	}
	// 正常路径不传 streamingBehavior（传了会静默改成排队语义）。
	if strings.Contains(notesOf(t, notes), "PROMPT_HAS_STREAMING_BEHAVIOR") {
		t.Error("prompt 带了 streamingBehavior，正常路径不该带")
	}
	if !strings.Contains(notesOf(t, notes), "CMD new_session") {
		t.Error("Start 里没发 new_session")
	}
}

// TestEachRoundStartsFreshSession：每题一次 new_session 是评测公平性的前提
// （题目间上下文隔离）。断言「每轮前都发过」而不是「发过一次」——复用会话会把
// 上一题的答案与死胡同带进下一题，而且没有任何可见症状。
func TestEachRoundStartsFreshSession(t *testing.T) {
	a, notes := newAgent(t, "normal", nil)
	startAgent(t, a)

	for i := 0; i < 2; i++ {
		if _, err := a.Round(context.Background(), fmt.Sprintf("第 %d 轮", i), nil); err != nil {
			t.Fatalf("第 %d 轮失败: %v", i, err)
		}
	}
	// Start 的握手 1 次 + 每轮 1 次 = 3。
	if got := strings.Count(notesOf(t, notes), "CMD new_session"); got != 3 {
		t.Errorf("new_session 次数 = %d，期望 3（Start 1 + 两轮各 1）", got)
	}
}

// TestRoundThrottlesButKeepsText：事件流按 1/s 节流，但 RoundResult.Text 是权威
// 产物，必须**不**受节流影响。stub 会先吐一段「预热」文本把节流窗口占掉，
// 再吐两段真正的增量——所以事件数应当明显少于增量数，而 Text 一个字符不少。
func TestRoundThrottlesButKeepsText(t *testing.T) {
	a, _ := newAgent(t, "normal", nil)
	startAgent(t, a)
	var textEvents int
	res, err := a.Round(context.Background(), "节流", func(ev harness.Event) {
		if ev.Kind == harness.EventText {
			textEvents++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "预热。第一段文本。第二段文本。" {
		t.Errorf("Text = %q（节流不该影响权威文本）", res.Text)
	}
	if textEvents != 1 {
		t.Errorf("text 事件数 = %d，期望 1（同一秒内的增量应被节流成 1 条）", textEvents)
	}
}

// ─────────────────────── UI 请求 ───────────────────────

// TestUIAnsweredAndCounted：dialog 不答即挂死（pi 侧阻塞等应答），
// fire-and-forget 应答则是多余的帧且污染 pendingExtensionRequests 语义。
func TestUIAnsweredAndCounted(t *testing.T) {
	a, notes := newAgent(t, "ui", nil)
	startAgent(t, a)

	res, err := a.Round(context.Background(), "触发对话框", nil)
	if err != nil {
		t.Fatalf("Round 失败: %v", err)
	}
	// 两个 dialog（confirm/select）被自动取消。
	if res.CancelledUI != 2 {
		t.Errorf("CancelledUI = %d，期望 2", res.CancelledUI)
	}
	n := notesOf(t, notes)
	for _, want := range []string{"UIRESP ui-confirm-1", "UIRESP ui-select-1"} {
		if !strings.Contains(n, want) {
			t.Errorf("缺少应答 %q（dialog 不答即挂死）:\n%s", want, n)
		}
	}
	// 两个键（id 与 requestId）都要带：stub 缺 requestId 时会写 BAD: 前缀。
	if strings.Contains(n, "BAD:") {
		t.Errorf("应答缺 requestId 键:\n%s", n)
	}
	// fire-and-forget 类不应答。
	for _, bad := range []string{"UIRESP ui-notify-1", "UIRESP ui-status-1", "UIRESP ui-title-1"} {
		if strings.Contains(n, bad) {
			t.Errorf("fire-and-forget 不应答，却发了 %q", bad)
		}
	}
}

func TestUIRequestIsEmitted(t *testing.T) {
	a, _ := newAgent(t, "ui", nil)
	startAgent(t, a)
	var ui []harness.Event
	if _, err := a.Round(context.Background(), "UI 事件", func(ev harness.Event) {
		if ev.Kind == harness.EventUIRequest {
			ui = append(ui, ev)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(ui) != 5 {
		t.Fatalf("UI 事件数 = %d，期望 5（两个 dialog + 三个 fire-and-forget 都要透出）", len(ui))
	}
}

// ─────────────────────── 进程死亡 ───────────────────────

func TestProcessDiedSetsError(t *testing.T) {
	a, _ := newAgent(t, "eof", nil)
	startAgent(t, a)

	res, err := a.Round(context.Background(), "会死的一轮", nil)
	if err == nil {
		t.Fatal("进程死亡时 Round 必须返回 error")
	}
	// 必须置 error：否则 harness 的 provider_failure 护栏（turns==0 && err!=""）
	// 失效，题库会被静默烧掉（前身 280 run / 0 flag 的呈现方式）。
	if !strings.Contains(res.Err, ErrProcessDied) {
		t.Errorf("RoundResult.Err = %q，期望含 %q", res.Err, ErrProcessDied)
	}
	if res.Reason != harness.ReasonError {
		t.Errorf("Reason = %q", res.Reason)
	}
}

// TestDeathIsReportedToHarnessGuard 复现 harness 护栏的实际判定输入：
// turns==0 且 Err!="" ⇒ provider_failure。这是本包最重要的对外契约。
func TestDeathIsReportedToHarnessGuard(t *testing.T) {
	a, _ := newAgent(t, "eof", nil)
	startAgent(t, a)
	res, _ := a.Round(context.Background(), "死亡", nil)
	if res.Turns != 0 || res.Err == "" {
		t.Fatalf("护栏输入错：turns=%d err=%q（必须让 provider_failure 判定成立）", res.Turns, res.Err)
	}
}

// TestRestartAfterDeath：进程死亡后监督器必须重启（带退避），且重启受
// MaxRestarts 上限约束——无限重启会把整道题的墙钟预算耗光，还会让日志看不出
// 真正的死因。
func TestRestartAfterDeath(t *testing.T) {
	a, _ := newAgent(t, "eof", nil)
	startAgent(t, a)

	// eof 场景每轮都会死，所以连续几轮必然把重启计数推到上限。
	var lastErr error
	for i := 0; i < 6; i++ {
		_, lastErr = a.Round(context.Background(), fmt.Sprintf("第 %d 轮", i), nil)
	}
	a.mu.Lock()
	restarts := a.restarts
	a.mu.Unlock()
	if restarts == 0 {
		t.Fatal("进程死亡后没有触发重启")
	}
	if restarts > a.maxRestarts() {
		t.Errorf("重启次数 %d 超过上限 %d", restarts, a.maxRestarts())
	}
	// 上限用尽后必须报出「放弃」，而不是静默返回一个空结果。
	if lastErr == nil || !strings.Contains(lastErr.Error(), "重启") {
		t.Errorf("超过重启上限必须报错: %v", lastErr)
	}
}

// ─────────────────────── 版本校验 ───────────────────────

func TestVersionMismatchIsConfigError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "badpi")
	writeScript(t, script, "#!/bin/sh\necho 9.9.9\n")
	a := &Agent{BinPath: script, EnvFile: filepath.Join(dir, ".env")}
	if err := os.WriteFile(a.EnvFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := a.Start(context.Background(), harness.AgentStart{Workdir: dir})
	if err == nil {
		t.Fatal("版本超区间必须报配置错（跑分中途才炸的版本问题会烧掉整个题库）")
	}
	if !strings.Contains(err.Error(), "不在支持区间") {
		t.Errorf("错误信息不可操作: %v", err)
	}
}

func TestVersionProbeFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "failpi")
	writeScript(t, script, "#!/bin/sh\necho 'boom' >&2\nexit 3\n")
	a := &Agent{BinPath: script, EnvFile: filepath.Join(dir, ".env")}
	if err := os.WriteFile(a.EnvFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := a.Start(context.Background(), harness.AgentStart{Workdir: dir})
	if err == nil || !strings.Contains(err.Error(), "退出码 3") {
		t.Fatalf("版本探测失败的报错必须带退出码与 stderr: %v", err)
	}
}

func TestVersionAccepted(t *testing.T) {
	a, _ := newAgent(t, "normal", nil)
	startAgent(t, a)
	if a.Version() != "0.86.0" {
		t.Errorf("Version() = %q", a.Version())
	}
	if a.SessionID() == "" {
		t.Error("SessionID() 为空")
	}
}

func TestDiscoverBinOrder(t *testing.T) {
	t.Setenv(EnvPiBin, "")
	// 显式路径不存在 ⇒ 报错（不静默回退）。
	if _, err := DiscoverBin(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("显式指定的二进制不存在时必须报错")
	}
	// 环境变量优先。
	dir := t.TempDir()
	fake := filepath.Join(dir, "pi")
	writeScript(t, fake, "#!/bin/sh\nexit 0\n")
	t.Setenv(EnvPiBin, fake)
	got, err := DiscoverBin("")
	if err != nil || got != fake {
		t.Fatalf("DiscoverBin = %q, %v", got, err)
	}
	// 环境变量指向不存在的文件 ⇒ 报错而不是继续往下找（静默降级是事故温床）。
	t.Setenv(EnvPiBin, filepath.Join(dir, "gone"))
	if _, err := DiscoverBin(""); err == nil {
		t.Error("环境变量指向不存在的文件时必须报错")
	}
}

func TestVersionParsing(t *testing.T) {
	if _, ok := parseVersion("0.86.0"); !ok {
		t.Error("parseVersion(0.86.0) 失败")
	}
	if v, ok := parseVersion("0.86.0-beta.1"); !ok || v != [3]int{0, 86, 0} {
		t.Errorf("带后缀的版本解析错: %v %v", v, ok)
	}
	if _, ok := parseVersion("nonsense"); ok {
		t.Error("垃圾输入不该被解析成版本")
	}
	if cmpVersion([3]int{0, 85, 0}, [3]int{0, 85, 0}) != 0 {
		t.Error("同版本应相等")
	}
	if cmpVersion([3]int{0, 84, 9}, [3]int{0, 85, 0}) >= 0 {
		t.Error("0.84.9 应小于 0.85.0")
	}
}

// TestVersionRangeIsClosed：区间是闭区间，端点本身必须被接受；越界必须被拒。
// 用真跑 `--version` 的假二进制，而不是直接调内部比较函数——这样连
// 「环境变量/PATH 有没有传对」也一起验了。
func TestVersionRangeIsClosed(t *testing.T) {
	dir := t.TempDir()
	run := func(v string) error {
		p := filepath.Join(dir, "pi-"+v)
		writeScript(t, p, "#!/bin/sh\necho "+v+"\n")
		_, err := checkVersion(context.Background(), p, os.Environ(), DefaultVersionRange)
		return err
	}
	// 显式写死边界值，**不**引用 DefaultVersionRange：否则改常量时这条用例会
	// 跟着漂，起不到钉住的作用。0.85.1 = 前身生产版本，0.86.0 = M0 实测版本。
	for _, v := range []string{"0.85.0", "0.85.1", "0.86.0", "0.86.99"} {
		if err := run(v); err != nil {
			t.Errorf("%s 在闭区间内，应被接受: %v", v, err)
		}
	}
	for _, v := range []string{"0.84.9", "0.87.0", "0.90.0"} {
		if err := run(v); err == nil {
			t.Errorf("%s 越界，应被拒绝", v)
		}
	}
}

// ─────────────────────── 启动自检 ───────────────────────

func TestStartupSelfCheckExtensionMissing(t *testing.T) {
	a, _ := newAgent(t, "nocommands", nil)
	a.Extensions = []string{"/tmp/report_fact.ts"}
	a.Approve = true
	err := a.Start(context.Background(), harness.AgentStart{Workdir: a.Workdir})
	if err == nil {
		t.Fatal("extension 没加载上必须报配置错")
	}
	if !strings.Contains(err.Error(), "get_commands") {
		t.Errorf("报错要指出用了 get_commands 自检: %v", err)
	}
}

// TestStartupSelfCheckApproveOff 复现 M0 的静默失效：传了 -e 但没开 --approve，
// 项目资源被**静默忽略**——不显式自检就会以「跑完一轮但什么都没发生」的形式烧题库。
func TestStartupSelfCheckApproveOff(t *testing.T) {
	a, _ := newAgent(t, "normal", nil)
	a.Extensions = []string{"/tmp/report_fact.ts"}
	a.Approve = false
	err := a.Start(context.Background(), harness.AgentStart{Workdir: a.Workdir})
	if err == nil {
		t.Fatal("未开 --approve 时 extension 被静默忽略，必须被自检拦住")
	}
	if !strings.Contains(err.Error(), "--approve") {
		t.Errorf("报错要给出可操作的建议: %v", err)
	}
}

func TestStartupExtensionOK(t *testing.T) {
	a, _ := newAgent(t, "normal", nil)
	a.Extensions = []string{"/tmp/report_fact.ts"}
	a.Approve = true
	startAgent(t, a)
}

func TestProviderCredentialPreflight(t *testing.T) {
	a, _ := newAgent(t, "normal", nil)
	a.Provider = "opencode-go"
	// 把进程环境里可能存在的 key 摘掉，模拟「.env 里没凭据」。
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	err := a.Start(context.Background(), harness.AgentStart{Workdir: a.Workdir})
	if err == nil {
		t.Fatal("provider 缺凭据必须拒绝启动（pi 会把它呈现成静默的空会话）")
	}
	if !strings.Contains(err.Error(), "OPENCODE_GO_API_KEY") && !strings.Contains(err.Error(), "OPENCODE_API_KEY") {
		t.Errorf("报错要给出期望的环境变量名: %v", err)
	}
}

func TestProviderCredentialFromEnv(t *testing.T) {
	a, _ := newAgent(t, "normal", nil)
	a.Provider = "opencode-go"
	t.Setenv("OPENCODE_API_KEY", "sk-test")
	startAgent(t, a)
}

// ─────────────────────── 会话复位断言 ───────────────────────

// TestSessionNotResetIsRejected：new_session 后 sessionId 没变 ⇒ 必须报错，
// 而不是「假设它复位了」继续发 prompt（会把上一题的上下文带进下一题）。
func TestSessionNotResetIsRejected(t *testing.T) {
	a, _ := newAgent(t, "noreset", nil)
	a.MaxRestarts = 0 // 不复位是环境问题，重试无益；这里只验证报错内容
	err := a.Start(context.Background(), harness.AgentStart{Workdir: a.Workdir})
	if err == nil {
		t.Fatal("会话未复位时必须报错")
	}
	if !strings.Contains(err.Error(), "会话未复位") {
		t.Errorf("错误信息应当点明会话未复位: %v", err)
	}
}

// ─────────────────────── 看门狗 ───────────────────────

// TestWatchdogWedgedAndRestart：get_state 连续两次探活超时 ⇒ 判 wedged ⇒
// 整组 kill + 重启，本轮 Err 非空。真跑时这是「pi 事件循环死锁」的唯一出路。
func TestWatchdogWedgedAndRestart(t *testing.T) {
	a, _ := newAgent(t, "slowstate", nil)
	a.StallTimeout = 300 * time.Millisecond
	a.ProbeTimeout = 300 * time.Millisecond
	startAgent(t, a)

	a.mu.Lock()
	before := a.cur
	a.mu.Unlock()

	start := time.Now()
	res, err := a.Round(context.Background(), "探活会失败", nil)
	if err == nil {
		t.Fatal("wedged 必须返回 error")
	}
	if !strings.Contains(res.Err, ErrWedged) {
		t.Errorf("RoundResult.Err = %q，期望含 %q", res.Err, ErrWedged)
	}
	if res.Reason != harness.ReasonError {
		t.Errorf("Reason = %q", res.Reason)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("判 wedged 花了 %s，太久", d)
	}
	// wedged ⇒ 必须换进程（那个进程已经不答话了，留着也没用）。
	if !before.wait(3 * time.Second) {
		t.Error("判 wedged 后没有 kill 旧进程组")
	}
}

// TestWatchdogQuietButAliveIsNotWedged：探活成功但状态没变 ⇒ **不**判死。
// 长思考期间 messageCount 天然不动，把它当卡死信号就是前身「无输出即卡死」
// 换个名字重犯（那次误杀了 5–8 分钟的深思考）。
func TestWatchdogQuietButAliveIsNotWedged(t *testing.T) {
	a, _ := newAgent(t, "nosettle", nil)
	a.StallTimeout = 200 * time.Millisecond
	a.ProbeTimeout = time.Second
	startAgent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	res, _ := a.Round(ctx, "安静但不死", nil)
	if strings.Contains(res.Err, ErrWedged) {
		t.Errorf("探活成功却判了 wedged: %q", res.Err)
	}
	if res.Reason != harness.ReasonStopped {
		t.Errorf("Reason = %q，期望 stopped（ctx 到期）", res.Reason)
	}
}

// ─────────────────────── 优雅收尾 ───────────────────────

// TestAbortGracefulStop：ctx 到期 ⇒ 先 abort ⇒ 宽限期内收到 settled ⇒
// Reason=stopped，且**不**判 wedged、不 kill。
func TestAbortGracefulStop(t *testing.T) {
	a, _ := newAgent(t, "nosettle", nil)
	a.StallTimeout = 30 * time.Second
	a.AbortGrace = 3 * time.Second
	startAgent(t, a)

	a.mu.Lock()
	p := a.cur
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := a.Round(ctx, "永不收尾的一轮", nil)
	if err == nil {
		t.Fatal("ctx 到期必须返回 error")
	}
	if res.Reason != harness.ReasonStopped {
		t.Errorf("Reason = %q，期望 %q", res.Reason, harness.ReasonStopped)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v，期望 context.Canceled", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("优雅收尾花了 %s，超过宽限期太多", d)
	}
	// 宽限期内收到 settled ⇒ 进程应当还活着（kill 会丢掉已产出的内容）。
	if p.isDead() {
		t.Error("优雅收尾路径不该 kill 进程")
	}
}

// TestAbortGraceExpiresKills：abort 也不应答（进程根本不读 stdin）⇒ 宽限期到
// ⇒ 整组 kill。pi 不会自己退出，所以 killpg 是唯一可靠的收尾。
func TestAbortGraceExpiresKills(t *testing.T) {
	a, _ := newAgent(t, "stallforever", nil)
	a.StallTimeout = 30 * time.Second
	a.AbortGrace = 500 * time.Millisecond
	startAgent(t, a)

	a.mu.Lock()
	p := a.cur
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := a.Round(ctx, "abort 也不会被应答", nil)
	if err == nil {
		t.Fatal("ctx 到期必须返回 error")
	}
	if res.Reason != harness.ReasonStopped {
		t.Errorf("Reason = %q", res.Reason)
	}
	if !strings.Contains(err.Error(), "宽限期") {
		t.Errorf("应当说明是宽限期到才 kill: %v", err)
	}
	if !p.wait(3 * time.Second) {
		t.Error("宽限期到之后进程组还在（pi 不会自己退出，必须 killpg）")
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("kill 路径花了 %s", d)
	}
}

// ─────────────────────── writer 死锁 ───────────────────────

// TestWriterDoesNotDeadlock 是本包最重要的一条用例（设计 §八 第 3 条）。
//
// 构造：stub 应答 prompt 之后**再也不读 stdin**，同时持续吐 1 MB 事件，并把
// extension_ui_request 塞在这坨事件中间（应答对话框是「reader 想写 stdin」最自然
// 的触发点）。客户端此时必须：(a) 继续消费 stdout，(b) 不因为要写 stdin 而阻塞
// reader 协程——否则 (a) 立刻失守，双方互等 ⇒ 永久挂死。
//
// 断言：ctx 取消后 Round 能返回；期间原始帧数 > 100（reader 一直在消费）；
// 自动取消的对话框计数非 0（说明这条路径确实被走到过）。
func TestWriterDoesNotDeadlock(t *testing.T) {
	a, _ := newAgent(t, "stallforever", nil)
	a.StallTimeout = 30 * time.Second // 不让看门狗介入，专测死锁
	a.AbortGrace = 300 * time.Millisecond
	var frames atomic.Int64
	a.OnRawEvent = func(Envelope, []byte) { frames.Add(1) }
	startAgent(t, a)

	// 大 prompt 先写满一部分 stdin 管道；随后 Steer 继续灌，直到管道写满、
	// 唯一 writer 阻塞在 Write 上。这时若 reader 有任何一次内联写 stdin，
	// 整个客户端就会停摆。
	big := strings.Repeat("P", 256<<10)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	done := make(chan harness.RoundResult, 1)
	go func() {
		res, _ := a.Round(ctx, big, nil)
		done <- res
	}()

	// 并行灌 stdin：这些 Steer 会让唯一 writer 阻塞在管道上（stub 不读）。
	go func() {
		payload := strings.Repeat("S", 32<<10)
		for i := 0; i < 64; i++ {
			sctx, c := context.WithTimeout(context.Background(), 5*time.Millisecond)
			_ = a.Steer(sctx, payload)
			c()
			if ctx.Err() != nil {
				return
			}
		}
	}()

	select {
	case res := <-done:
		// 取消路径必须走到「自动取消对话框」那一步：stub 的 UI 请求就在这坨
		// 事件里，而应答要经过被堵住的 writer。
		if res.CancelledUI == 0 {
			t.Error("没有自动取消任何对话框：stall 场景的 UI 请求没被走到，用例失去意义")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Round 挂死了：writer 死锁复现成功（reader 与 stdin 写互相等待）")
	}
	if n := frames.Load(); n < 100 {
		t.Errorf("期间只收到 %d 帧，reader 疑似被写 stdin 阻塞（死锁前兆）", n)
	}
}

// TestQueuePushNeverBlocks 是上面那条的单元级版本：入队必须永不阻塞，
// 否则「reader 入队 UI 应答」这条路又会把 reader 卡住。
func TestQueuePushNeverBlocks(t *testing.T) {
	q := newFrameQueue()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < queueMaxFrames; i++ {
			if !q.push(Command{Type: CmdGetState}) {
				t.Errorf("第 %d 次 push 失败", i)
				return
			}
		}
		// 超过上限：返回 false（让调用方把对端判死），但**不阻塞**。
		if q.push(Command{Type: CmdGetState}) {
			t.Error("超上限的 push 应当返回 false")
		}
		if !q.didOverflow() {
			t.Error("溢出标记未置位")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("push 阻塞了")
	}
}

// TestQueueKeepsFIFO：Command 与已序列化的 raw 共用一条队列。分两条队列会破坏
// 顺序，而顺序是协议要求（abort 必须在 prompt 之后到达）。
func TestQueueKeepsFIFO(t *testing.T) {
	q := newFrameQueue()
	q.push(Command{Type: CmdPrompt, Msg: "一"})
	q.pushRaw([]byte(`{"type":"extension_ui_response","id":"x"}`))
	q.push(Command{Type: CmdAbort})
	for i, w := range []string{"prompt", "extension_ui_response", "abort"} {
		b, ok := q.pop()
		if !ok {
			t.Fatalf("第 %d 条取不到", i)
		}
		if !strings.Contains(string(b), `"type":"`+w+`"`) {
			t.Errorf("第 %d 条 = %s，期望含 %s", i, b, w)
		}
	}
}

// TestSendAfterDeathIsError：进程死后 send/call 必须报错，而不是静默丢弃
// （静默丢弃会让「pi 已经死了」以看门狗超时的形式晚 90s 才暴露）。
func TestSendAfterDeathIsError(t *testing.T) {
	a, _ := newAgent(t, "eof", nil)
	startAgent(t, a)
	a.mu.Lock()
	p := a.cur
	a.mu.Unlock()
	p.markDead(ErrProcessDied, "测试强制死亡")
	if err := p.send(Command{Type: CmdAbort}); err == nil {
		t.Error("死亡后 send 必须报错")
	}
	if _, err := p.call(context.Background(), Command{Type: CmdGetState}); err == nil {
		t.Error("死亡后 call 必须报错")
	}
}

// ─────────────────────── provider 失败 ───────────────────────

// TestProviderErrorIsSurfaced：M0 实测的 401 呈现方式（stopReason=error +
// 空 content + 照样 agent_settled）必须被识别成本轮错误。
func TestProviderErrorIsSurfaced(t *testing.T) {
	a, _ := newAgent(t, "providerr", nil)
	startAgent(t, a)

	res, err := a.Round(context.Background(), "provider 会失败", nil)
	if err == nil {
		t.Fatal("provider 失败必须返回 error（否则护栏失效）")
	}
	if !strings.Contains(res.Err, "pi_provider_error") {
		t.Errorf("RoundResult.Err = %q", res.Err)
	}
	if !strings.Contains(res.Err, "API key is invalid") {
		t.Errorf("应当带上 provider 的原文: %q", res.Err)
	}
	if res.Reason != harness.ReasonError {
		t.Errorf("Reason = %q", res.Reason)
	}
}

// TestProviderErrorIsRoundScoped 钉住「轮级值必须清零」。
//
// stub 第 1 轮 provider 失败、第 2 轮起正常。判据必须是「这一轮自己有没有出错」：
// 若轮级值不清零，调用方会在会话已经恢复（甚至拿到 flag）之后仍然判成「模型服务
// 挂了」——过度判失败比漏判更难查。
func TestProviderErrorIsRoundScoped(t *testing.T) {
	a, _ := newAgent(t, "provider1st", nil)
	startAgent(t, a)

	res1, _ := a.Round(context.Background(), "第 1 轮：provider 会失败", nil)
	if res1.ProviderError == "" {
		t.Fatal("第 1 轮 ProviderError 为空（护栏失效）")
	}
	if !strings.Contains(res1.ProviderError, "API key is invalid") {
		t.Errorf("ProviderError 应当带上 provider 原文: %q", res1.ProviderError)
	}
	if res1.Err == "" {
		t.Error("第 1 轮 Err 也应为非空（护栏输入）")
	}

	res2, err := a.Round(context.Background(), "第 2 轮：一切正常", nil)
	if err != nil {
		t.Fatalf("第 2 轮不该失败: %v", err)
	}
	if res2.ProviderError != "" {
		t.Errorf("第 2 轮 ProviderError = %q，必须为空（轮级值没清零 ⇒ 后续轮次被污染）", res2.ProviderError)
	}
	if res2.Turns != 1 {
		t.Errorf("第 2 轮 Turns = %d，期望 1", res2.Turns)
	}
	if res2.Reason != harness.ReasonCompleted {
		t.Errorf("第 2 轮 Reason = %q，期望 completed", res2.Reason)
	}
	// 注意：这里**同时**钉住了 Err 的实际语义是**轮级**的，而不是根包 solver.go
	// 注释里写的「轮级累积」。实测第 2 轮 Err 为空——每轮 Round 都新建 acc，
	// 累积从来不存在。根包注释与实现不符，已上报；在那条注释改掉之前，这条断言
	// 就是「Err 是轮级的」这个事实的唯一回归保护。
	if res2.Err != "" {
		t.Errorf("第 2 轮 Err = %q；Err 实际是轮级的（每轮新建 acc），不是累积的", res2.Err)
	}
}

// ─────────────────────── 帧与解码 ───────────────────────

func TestFrameReaderLFOnly(t *testing.T) {
	// U+2028/U+2029 在 JSON 字符串里合法，**不是**分隔符（rpc.md 明确）。
	body := "{\"type\":\"message_update\",\"assistantMessageEvent\":{\"delta\":\"a b c\"}}\n" +
		"{\"type\":\"agent_settled\"}\n"
	r := NewFrameReader(strings.NewReader(body))
	var lines [][]byte
	for {
		l, ok, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		lines = append(lines, l)
	}
	if len(lines) != 2 {
		t.Fatalf("切出 %d 帧，期望 2（U+2028 被当成换行了？）", len(lines))
	}
	if !strings.Contains(string(lines[0]), " ") {
		t.Error("帧内容被改写")
	}
}

func TestFrameReaderCRLF(t *testing.T) {
	r := NewFrameReader(strings.NewReader("{\"a\":1}\r\n{\"b\":2}\r\n"))
	l1, ok, _ := r.Next()
	if !ok || string(l1) != "{\"a\":1}" {
		t.Fatalf("CRLF 未剥离: %q", l1)
	}
}

func TestFrameReaderBlankLinesSkipped(t *testing.T) {
	// 空行不是帧，也不该解出一个 type="" 的怪东西。
	r := NewFrameReader(strings.NewReader("\n\n{\"a\":1}\n"))
	l, ok, err := r.Next()
	if err != nil || !ok || string(l) != "{\"a\":1}" {
		t.Fatalf("空行处理错: %q %v %v", l, ok, err)
	}
}

func TestFrameReaderHalfLineAtEOF(t *testing.T) {
	// 无结尾换行的最后一段：Scanner 作为最后一行返回。它意味着进程死了
	// （reader 的 EOF 处理会覆盖这条路径），这里只锁住「不 panic、不丢内容」。
	r := NewFrameReader(strings.NewReader("{\"a\":1}"))
	l, ok, err := r.Next()
	if err != nil || !ok || string(l) != "{\"a\":1}" {
		t.Fatalf("半行处理错: %q %v %v", l, ok, err)
	}
	if _, ok, _ := r.Next(); ok {
		t.Error("EOF 后不应再返回帧")
	}
}

func TestFrameReaderLargeFrame(t *testing.T) {
	big := strings.Repeat("Z", 512<<10)
	r := NewFrameReader(strings.NewReader("{\"x\":\"" + big + "\"}\n"))
	l, ok, err := r.Next()
	if err != nil || !ok {
		t.Fatalf("大帧读取失败: %v", err)
	}
	if len(l) < 512<<10 {
		t.Fatalf("大帧被截断: %d 字节", len(l))
	}
}

func TestFrameReaderRejectsInvalidUTF8(t *testing.T) {
	r := NewFrameReader(strings.NewReader("\xff\xfe\n"))
	if _, _, err := r.Next(); err == nil {
		t.Error("非法 UTF-8 应当报错（帧边界已经错了，继续解只会得到噪音）")
	}
}

func TestTwoStageDecodeToolEnd(t *testing.T) {
	raw := []byte(`{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","isError":true,
		"result":{"content":[{"type":"text","text":"boom"},{"type":"image","data":"x"}],
		"details":{"report_fact":{"facts":[{"kind":"vuln","content":"/upload"}]}}}}`)
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != "tool_execution_end" {
		t.Fatalf("第一段解出 %q", env.Type)
	}
	var te ToolEnd
	if err := json.Unmarshal(raw, &te); err != nil {
		t.Fatal(err)
	}
	if te.ToolCallID != "c1" || !te.IsError {
		t.Errorf("第二段解错: %+v", te)
	}
	if te.Text() != "boom" {
		t.Errorf("Text() = %q（非 text 块应被忽略）", te.Text())
	}
	if _, ok := te.Result.Details["report_fact"].(map[string]any); !ok {
		t.Fatal("details 丢失")
	}
}

// TestUnknownEventIsIgnored：pi 的发布节奏是 1–2 周/版，未知事件必须被忽略而
// 不是让 reader 死掉。
func TestUnknownEventIsIgnored(t *testing.T) {
	a, _ := newAgent(t, "normal", nil)
	a.OnRawEvent = func(env Envelope, raw []byte) {
		if env.Type != "message_update" {
			return
		}
		a.mu.Lock()
		rs := a.rs
		a.mu.Unlock()
		if rs != nil {
			// 直接投到 reader 的下游：模拟 pi 升版后多出来的事件。
			rs.push(Envelope{Type: "brand_new_event_from_future"},
				[]byte(`{"type":"brand_new_event_from_future","x":1}`))
			// 顺带一条完全不是 JSON 的行（stdout 里混进日志）。
			rs.push(Envelope{}, []byte(`this is not json at all`))
		}
	}
	startAgent(t, a)
	res, err := a.Round(context.Background(), "含未知事件的一轮", nil)
	if err != nil || res.Err != "" {
		t.Fatalf("未知事件与垃圾行都不该影响本轮: %v / %q", err, res.Err)
	}
	if res.Text != "预热。第一段文本。第二段文本。" {
		t.Errorf("Text = %q（reader 中途停了？）", res.Text)
	}
}

// ─────────────────────── 大帧 / CRLF / U+2028 端到端 ───────────────────────

func TestRoundBigFrame(t *testing.T) {
	a, _ := newAgent(t, "big", nil)
	startAgent(t, a)
	var out string
	_, err := a.Round(context.Background(), "大输出", func(ev harness.Event) {
		if ev.Kind == harness.EventToolEnd && ev.ToolCallID == "call_big" {
			out = ev.Output
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 512<<10 {
		t.Fatalf("大帧被截断: %d 字节（期望 %d）", len(out), 512<<10)
	}
}

func TestRoundCRLF(t *testing.T) {
	a, _ := newAgent(t, "normal", map[string]string{"STUBPI_CRLF": "1"})
	startAgent(t, a)
	res, err := a.Round(context.Background(), "CRLF", nil)
	if err != nil || res.Err != "" {
		t.Fatalf("\\r\\n 分帧必须被接受: %v / %q", err, res.Err)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d", res.Turns)
	}
}

func TestRoundU2028InsideFrame(t *testing.T) {
	a, _ := newAgent(t, "normal", map[string]string{"STUBPI_U2028": "1"})
	startAgent(t, a)
	res, err := a.Round(context.Background(), "U+2028", nil)
	if err != nil || res.Err != "" {
		t.Fatalf("U+2028 不应被当成分隔符: %v / %q", err, res.Err)
	}
	if !strings.Contains(res.Text, " ") || !strings.Contains(res.Text, " ") {
		t.Errorf("文本里的 U+2028/U+2029 丢了: %q", res.Text)
	}
}

// ─────────────────────── Steer / Stats / 命令行 ───────────────────────

// TestSteerUsesSteerCommand：hint 必须走 steer 而不是 prompt。空闲时发 prompt
// 会立刻起一次 LLM 调用、脱离 DAG 渲染的意图，既浪费一轮又污染上下文。
func TestSteerUsesSteerCommand(t *testing.T) {
	a, notes := newAgent(t, "normal", nil)
	startAgent(t, a)
	if err := a.Steer(context.Background(), "【平台提示】试试 /admin"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(notesOf(t, notes), "STEER ") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	n := notesOf(t, notes)
	if !strings.Contains(n, "CMD steer") {
		t.Errorf("Steer 没走 steer 命令:\n%s", n)
	}
	if !strings.Contains(n, "STEER 【平台提示】试试 /admin") {
		t.Errorf("steer 正文没到:\n%s", n)
	}
}

func TestStats(t *testing.T) {
	a, _ := newAgent(t, "normal", nil)
	startAgent(t, a)
	if _, err := a.Round(context.Background(), "一轮", nil); err != nil {
		t.Fatal(err)
	}
	st, err := a.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Turns != 1 {
		t.Errorf("Turns = %d（应来自 get_session_stats 的 toolCalls）", st.Turns)
	}
	if st.TokensIn != 1000 || st.TokensOut != 200 {
		t.Errorf("tokens 映射错: %+v", st)
	}
	if st.CostUSD != 0.0123 {
		t.Errorf("cost = %v", st.CostUSD)
	}
	if st.ContextTokens != 4321 || st.ContextWindow != 200000 {
		t.Errorf("contextUsage 映射错: %+v", st)
	}
	if st.SessionID == "" {
		t.Error("sessionId 为空")
	}
}

func TestCloseIsIdempotentAndKillsProcess(t *testing.T) {
	a, _ := newAgent(t, "normal", nil)
	startAgent(t, a)
	a.mu.Lock()
	p := a.cur
	a.mu.Unlock()
	if p == nil {
		t.Fatal("没有活进程")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.wait(3 * time.Second) {
		t.Fatal("Close 之后进程还活着（pi 不会自己退出，必须 killpg）")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("Close 必须幂等: %v", err)
	}
}

// TestCloseKillsChildProcessGroup：pi 会 spawn bundled node 子进程，只杀父进程
// 会留下孤儿（前身 _stop_process_tree 的教训）。stub 自己也 spawn 一个长命
// 子进程，用来断言 killpg 真的覆盖了整组。
func TestCloseKillsChildProcessGroup(t *testing.T) {
	a, _ := newAgent(t, "normal", nil)
	childPidFile := filepath.Join(a.Workdir, "child.pid")
	t.Setenv("STUBPI_SPAWN_CHILD", childPidFile)
	startAgent(t, a)

	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(childPidFile); err == nil {
			if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err == nil && pid > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Skip("stub 未启动子进程，跳过整组 kill 断言")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := syscall.Kill(pid, 0); err != nil {
			return // 子进程没了 = killpg 覆盖到了整组
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("子进程 %d 仍存活：killpg 没有覆盖到进程组", pid)
}

func TestBuildArgs(t *testing.T) {
	got := buildArgs("/s", []string{"ext/a.ts", "/abs/b.ts"}, "opencode-go", "deepseek-v4-flash",
		"medium", "系统提示", true)
	want := []string{"--mode", "rpc", "--session-dir", "/s",
		"--provider", "opencode-go", "--model", "deepseek-v4-flash",
		"-e", mustAbs(t, "ext/a.ts"), "-e", "/abs/b.ts",
		"--approve", "--thinking", "medium", "--append-system-prompt", "系统提示"}
	if len(got) != len(want) {
		t.Fatalf("args 长度 %d != %d\n%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
	// 相对路径必须被绝对化：非交互模式下项目资源默认被忽略。
	if !filepath.IsAbs(got[9]) {
		t.Errorf("-e 的相对路径没有被绝对化: %q", got[9])
	}
	// 不开 approve 时不得出现 --approve。
	no := buildArgs("", nil, "", "", "", "", false)
	if contains(no, "--approve") {
		t.Error("Approve=false 时不该出现 --approve")
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
