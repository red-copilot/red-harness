package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── 全生命周期：Fake Scenario → 假 Sandbox → 假 Agent → DAG/Gate → Evaluate → Cleanup ──
//
// 这是 M1 的纵向验收：**一条命令跑通离线题目的全生命周期**。它不碰 Docker（真实
// 容器那条由 executor 的 integration tag 覆盖），但它证明编排本身的接线是通的
// ——而接线漏接正是这个仓库反复踩的坑（v0.2 的 Ingest 漏接是**静默**的，所有包
// 自测全绿而 DAG 零事实）。
//
// 假件都在本文件里，不引第三方依赖，也不碰 .env。

// fakeSandbox 记录会话与主进程，用来断言「谁来起进程」。
type fakeSandbox struct {
	mu          sync.Mutex
	sessions    int
	closed      int
	reclaimed   []RunID
	staleCalls  int
	specs       []SandboxSpec
	launchCount int
	launchSpecs []ProcessSpec
	probeErr    error
	launchErr   error
}

func (s *fakeSandbox) NewSession(_ context.Context, spec SandboxSpec) (SandboxSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions++
	s.specs = append(s.specs, spec)
	return &fakeSession{sb: s, spec: spec}, nil
}

func (s *fakeSandbox) Reclaim(_ context.Context, id RunID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reclaimed = append(s.reclaimed, id)
	return nil
}

func (s *fakeSandbox) ReclaimStale(_ context.Context, live map[RunID]bool) ([]RunID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staleCalls++
	// 假件模拟「宿主上有一个上次崩溃的遗留 run」。
	if _, ok := live["run-crashed"]; !ok {
		return []RunID{"run-crashed"}, nil
	}
	return nil, nil
}

type fakeSession struct {
	sb     *fakeSandbox
	spec   SandboxSpec
	mu     sync.Mutex
	closed bool
}

func (s *fakeSession) Probe(context.Context) (ProbeResult, error) {
	if s.sb.probeErr != nil {
		return ProbeResult{}, s.sb.probeErr
	}
	return ProbeResult{ContainerID: "fake-ctr", Image: s.spec.Image, Workdir: s.spec.Workdir}, nil
}

func (s *fakeSession) Launch(_ context.Context, ps ProcessSpec) (ManagedProcess, error) {
	s.sb.mu.Lock()
	s.sb.launchCount++
	s.sb.launchSpecs = append(s.sb.launchSpecs, ps)
	err := s.sb.launchErr
	s.sb.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &fakeProcess{done: make(chan struct{})}, nil
}

func (s *fakeSession) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.sb.mu.Lock()
		s.sb.closed++
		s.sb.mu.Unlock()
	}
	return nil
}

type fakeProcess struct {
	mu     sync.Mutex
	killed bool
	done   chan struct{}
	once   sync.Once
}

func (p *fakeProcess) Read([]byte) (int, error)    { <-p.done; return 0, errors.New("closed") }
func (p *fakeProcess) Write(b []byte) (int, error) { return len(b), nil }
func (p *fakeProcess) Close() error                { return p.Kill() }
func (p *fakeProcess) Wait() error                 { <-p.done; return nil }
func (p *fakeProcess) Kill() error {
	p.once.Do(func() { close(p.done) })
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	return nil
}

// fakeAgent 按脚本逐轮产出事件。它**不启动任何进程**——它是编排测试里的替身。
type fakeAgent struct {
	rounds   int
	started  int
	closed   int
	steers   []string
	script   func(round int, emit func(Event))
	roundErr error
}

func (a *fakeAgent) Start(context.Context, AgentStart) error { a.started++; return nil }
func (a *fakeAgent) Round(_ context.Context, req RoundRequest) (RoundResult, error) {
	a.rounds++
	if a.roundErr != nil {
		return RoundResult{Reason: ReasonError, Err: "假 agent 故障"}, a.roundErr
	}
	if a.script != nil {
		a.script(req.Round, func(e Event) {})
	}
	return RoundResult{Turns: 1, Reason: ReasonCompleted}, nil
}
func (a *fakeAgent) Steer(_ context.Context, msg string) error {
	a.steers = append(a.steers, msg)
	return nil
}
func (a *fakeAgent) Stats(context.Context) (Stats, error) { return Stats{}, nil }
func (a *fakeAgent) Close(context.Context) error          { a.closed++; return nil }

// scriptedAgentFactory 让每道题拿到同一个假 agent，并把 sink 接上。
type scriptedAgentFactory struct {
	agent *fakeAgent
	sink  EventSink
}

func (f *scriptedAgentFactory) New(_ AgentSpec, _ SandboxSession, ev EventSink) (Agent, error) {
	f.sink = ev
	return f.agent, nil
}

// emitToolEnd 往 sink 推一次完整的工具调用（start + end），与 pi 的事件顺序一致。
func emitToolEnd(sink EventSink, callID, cmd, output string) {
	sink.Emit(Event{Kind: EventToolStart, Tool: "bash", ToolCallID: callID,
		Args: map[string]any{"command": cmd}})
	sink.Emit(Event{Kind: EventToolEnd, Tool: "bash", ToolCallID: callID,
		Args: map[string]any{"command": cmd}, Output: output})
}

// ── 编排用的假 Scenario ──
//
// 刻意不复用 scenario.Fake：那个包不依赖根包之外的东西，而根包**不能**导入它
// （依赖方向是单向的：实现包依赖根包）。这里的假场景只实现编排需要的最小语义。

type stubScenario struct {
	challenges []Challenge
	answers    map[string]string // code → 正确 flag
	mu         sync.Mutex
	prepared   int
	cleaned    int
	hints      int
	submitted  []string
	evalErr    error
}

func (s *stubScenario) Discover(context.Context, RunSpec) ([]Challenge, error) {
	return append([]Challenge(nil), s.challenges...), nil
}

func (s *stubScenario) Prepare(_ context.Context, ch Challenge) (Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepared++
	return Target{Code: ch.Code, Addrs: []string{"10.9.9.9:8080"}, Network: "tcp"}, nil
}

func (s *stubScenario) Hint(context.Context, Challenge) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hints++
	return "试试别的入口", nil
}

func (s *stubScenario) Evaluate(_ context.Context, ch Challenge, flag string) (Evaluation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.evalErr != nil {
		return Evaluation{}, s.evalErr
	}
	s.submitted = append(s.submitted, flag)
	ok := flag == s.answers[ch.Code]
	return Evaluation{Accepted: ok, Progress: ok, Completed: ok, Score: boolScore(ok)}, nil
}

func (s *stubScenario) Reconcile(_ context.Context, ch Challenge) (Objective, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	got := 0
	for _, f := range s.submitted {
		if f == s.answers[ch.Code] {
			got = 1
		}
	}
	return Objective{Kind: "flag_count", Want: 1, Got: got, Completed: got >= 1}, nil
}

func (s *stubScenario) Cleanup(context.Context, Challenge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleaned++
	return nil
}

func boolScore(ok bool) int {
	if ok {
		return 10
	}
	return 0
}

// ── 用例 ──

// TestRunFullLifecycleOffline 是 M1 的纵向验收：一道离线题从 Discover 走到
// Cleanup，且候选被提交、平台确认、题目完成。
func TestRunFullLifecycleOffline(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", Category: "web", FlagCount: 1, Addrs: []string{"10.9.9.9:8080"}}},
		answers:    map[string]string{"c1": "flag{real-answer}"},
	}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}

	// 第一轮：agent 从靶标响应里读到答案（观测族 ⇒ 可提交）。
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call-1", "curl -s http://10.9.9.9:8080/",
			"HTTP/1.1 200 OK\n\nflag{real-answer}\n")
	}

	h := newTestHarness(t, sc, sb, factory, func(Challenge) CandidateGate { return newStubGate() })

	res, err := h.Run(context.Background(), testRunSpec())
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if len(res.Challenges) != 1 {
		t.Fatalf("题目数 = %d，期望 1", len(res.Challenges))
	}
	cr := res.Challenges[0]
	if cr.Outcome.Reason != ReasonSolved {
		t.Errorf("Reason = %q，期望 %q", cr.Outcome.Reason, ReasonSolved)
	}
	if !res.Completed {
		t.Errorf("Completed = false，期望 true（平台确认了目标）")
	}
	if cr.Outcome.RemainingAtStart != 1 {
		t.Errorf("RemainingAtStart = %d，期望 1（起跑时还差 1 个）", cr.Outcome.RemainingAtStart)
	}
	if len(cr.Outcome.Flags) != 1 || cr.Outcome.Flags[0] != "flag{real-answer}" {
		t.Errorf("确认的答案 = %v", cr.Outcome.Flags)
	}
	if cr.Outcome.Score != 10 {
		t.Errorf("Score = %d，期望 10", cr.Outcome.Score)
	}
	// 资源回收：三条路径都必须走到。
	if sb.sessions != 1 || sb.closed != 1 {
		t.Errorf("session 数 = %d，close 数 = %d，期望各 1", sb.sessions, sb.closed)
	}
	if sc.cleaned != 1 {
		t.Errorf("Cleanup 调用 %d 次，期望 1", sc.cleaned)
	}
	if ag.closed != 1 {
		t.Errorf("agent.Close 调用 %d 次，期望 1", ag.closed)
	}
}

// TestRunReclaimsStaleBeforeStart：启动前必须按 label 扫掉上次崩溃的遗留。
//
// 这是回归：原先用的是 Reclaim(ctx, runID)，而 runID 是刚生成的——宿主上不可能
// 有它的遗留，那次调用是空转，孤儿容器与网络会一直攒着。
func TestRunReclaimsStaleBeforeStart(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{x}"}}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}

	h := newTestHarness(t, sc, sb, factory, func(Challenge) CandidateGate { return newStubGate() })
	if _, err := h.Run(context.Background(), testRunSpec()); err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if sb.staleCalls == 0 {
		t.Error("启动前没有做 ReclaimStale：上次崩溃的容器与网络不会被回收")
	}
	// 本次 run 自己的资源不在被回收之列（live 里只有它）。
	for _, id := range sb.reclaimed {
		if id == "run-crashed" {
			t.Error("Reclaim 不得回收本次 run 之外的资源")
		}
	}
}

// TestRunCancelStopsBeforeNextChallenge：一次 Ctrl-C 之后不得继续起下一道题。
func TestRunCancelStopsBeforeNextChallenge(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}, {Code: "c2", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{a}", "c2": "flag{b}"},
	}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}

	ctx, cancel := context.WithCancel(context.Background())
	// 第一道题的 Prepare 之后立刻取消：第二道题不该被 Prepare。
	ag.script = func(_ int, _ func(Event)) { cancel() }

	h := newTestHarness(t, sc, sb, factory, func(Challenge) CandidateGate { return newStubGate() })
	res, err := h.Run(ctx, testRunSpec())
	if err == nil {
		t.Fatal("取消必须让 Run 返回错误，否则调用方以为跑完了")
	}
	if !IsKind(err, KindCancelled) {
		t.Errorf("错误分类 = %v，期望 KindCancelled", err)
	}
	if sc.prepared != 1 {
		t.Errorf("Prepare 调用 %d 次，期望 1（取消后不得再起题）", sc.prepared)
	}
	if res.Err == "" {
		t.Error("取消后 RunResult.Err 必须非空，否则报告会把取消读成正常结束")
	}
}

// TestRunNoProgressIsNotSuccess：跑完但一题未解，不得记成 Completed。
//
// v0.4 要求把 RunFinished 与 ChallengeSolved 分开——把「没报错」当成「解出来了」
// 正是前身「280 run / 0 flag」那片绿的数字来源。
func TestRunNoProgressIsNotSuccess(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{never-seen}"}}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	// agent 只吐普通输出，没有任何候选。
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call-1", "ls", "nothing interesting\n")
	}

	h := newTestHarness(t, sc, sb, factory, func(Challenge) CandidateGate { return newStubGate() })
	res, err := h.Run(context.Background(), testRunSpec())
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if res.Completed {
		t.Error("Completed = true，但没有任何题目达成目标")
	}
	if res.Reason != ReasonNoProgress {
		t.Errorf("Reason = %q，期望 %q", res.Reason, ReasonNoProgress)
	}
}

// TestRunMissingPlannerIsConfigError：缺 Planner/Renderer/Gate 时必须明确报错，
// 不能静默返回一道「空轮次」的题——那会让装配错误被读成模型不行。
func TestRunMissingPlannerIsConfigError(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{x}"}}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}

	h := newTestHarness(t, sc, sb, factory, nil) // 不给 planner/renderer/gate
	res, err := h.Run(context.Background(), testRunSpec())
	if err == nil {
		t.Fatal("缺 Planner/Renderer/Gate 时 Run 必须返回错误")
	}
	if !IsKind(err, KindConfig) {
		t.Errorf("错误分类 = %v，期望 KindConfig", err)
	}
	if res.Err == "" {
		t.Error("RunResult.Err 必须非空")
	}
	// 资源仍必须回收。
	if sb.closed != 1 || sc.cleaned != 1 {
		t.Errorf("失败路径没有回收资源：close=%d cleanup=%d", sb.closed, sc.cleaned)
	}
}

// TestRunWallClockBudgetIsReachable：墙钟预算必须真的能触发。
//
// 回归：v0.2 的 used.MaxWall 恒为 0，MaxWall 那条分支**不可达**——墙钟护栏是
// 死代码，而报告里还留着 ReasonTimeout 这个字符串。
func TestRunWallClockBudgetIsReachable(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{x}"}}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}

	// 时钟每次调用前进 10 秒：第一轮之后的墙钟检查必然超限。
	base := time.Now()
	var ticks int
	var mu sync.Mutex
	h := newTestHarness(t, sc, sb, factory, func(Challenge) CandidateGate { return newStubGate() })
	h.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		ticks++
		return base.Add(time.Duration(ticks) * 10 * time.Second)
	}
	spec := testRunSpec()
	spec.Budget = Budget{MaxRounds: 40, MaxWall: 5 * time.Second, MaxTurns: 600}

	res, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if got := res.Challenges[0].Outcome.Reason; got != ReasonTimeout {
		t.Errorf("Reason = %q，期望 %q（墙钟护栏不可达 = 死代码）", got, ReasonTimeout)
	}
	if ag.rounds > 3 {
		t.Errorf("跑了 %d 轮，墙钟预算应在第一轮之后立刻生效", ag.rounds)
	}
}

// TestRunRoundErrorStopsChallenge：轮级错误必须终止本题，不得继续走提交与对账。
//
// 回归：0 回合 + 有错误的「跑完了」正是前身 280 run / 0 flag 的呈现方式。
func TestRunRoundErrorStopsChallenge(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{x}"}}
	sb := &fakeSandbox{}
	ag := &fakeAgent{roundErr: errors.New("假进程死了")}
	factory := &scriptedAgentFactory{agent: ag}

	h := newTestHarness(t, sc, sb, factory, func(Challenge) CandidateGate { return newStubGate() })
	res, err := h.Run(context.Background(), testRunSpec())
	if err == nil {
		t.Fatal("轮级错误必须让 Run 返回错误")
	}
	cr := res.Challenges[0]
	if cr.Outcome.Reason != ReasonError {
		t.Errorf("Reason = %q，期望 %q", cr.Outcome.Reason, ReasonError)
	}
	if cr.Outcome.Err == "" {
		t.Error("题级 Err 必须非空，否则报告看不出这轮出过事")
	}
	if len(sc.submitted) != 0 {
		t.Errorf("轮级错误之后不得提交任何候选，got %v", sc.submitted)
	}
}

// TestRunHintRequestedOncePerChallenge：两轮无进展只请求一次提示，之后不再请求。
func TestRunHintRequestedOncePerChallenge(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{never}"}}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call", "ls", "nothing\n")
	}

	h := newTestHarness(t, sc, sb, factory, func(Challenge) CandidateGate { return newStubGate() })
	spec := testRunSpec()
	spec.Budget = Budget{MaxRounds: 6, MaxWall: time.Minute, MaxTurns: 600}
	spec.HintPolicy = HintAuto

	res, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if sc.hints != 1 {
		t.Errorf("提示请求 %d 次，期望 1（每题最多一次）", sc.hints)
	}
	if res.Challenges[0].Outcome.HintUsed != 1 {
		t.Errorf("HintUsed = %d，期望 1", res.Challenges[0].Outcome.HintUsed)
	}
}

// TestRunPolicyZeroValueDoesNotPanic：RunPolicy 的零值路径不得 panic。
//
// PolicySpec 的零值在 model.go 里写着「0 值由引擎填默认值」，而 RunSpec 是用户
// 直接构造的公开类型——零值必须能跑。
func TestRunPolicyZeroValueDoesNotPanic(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{x}"}}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}

	h := newTestHarness(t, sc, sb, factory, func(Challenge) CandidateGate { return newStubGate() })
	spec := testRunSpec()
	spec.Policy = PolicySpec{} // 全零
	spec.HintPolicy = ""

	if _, err := h.Run(context.Background(), spec); err != nil {
		t.Fatalf("零值 Policy/HintPolicy 不得让 Run 失败: %v", err)
	}
}

// TestRunResultErrCarriesFailureClass：公开结果里的失败类别必须是**枚举分类**，
// 而不是 `*harness.Error` 这种对统计毫无信息量的类型名。
//
// 回归：safeError 原先只输出 %T，于是 provider 故障与执行器故障在 results.json
// 里变成同一个串——通过率结论无法把它们分开，而分开正是 M3 的要求。
func TestRunResultErrCarriesFailureClass(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{x}"}}
	sb := &fakeSandbox{probeErr: Ef(KindExecutor, "sandbox.probe", "假探测失败", nil)}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}

	h := newTestHarness(t, sc, sb, factory, func(Challenge) CandidateGate { return newStubGate() })
	res, err := h.Run(context.Background(), testRunSpec())
	if err == nil {
		t.Fatal("Probe 失败必须让 Run 返回错误")
	}
	if !IsKind(err, KindExecutor) {
		t.Errorf("错误分类 = %v，期望 KindExecutor", err)
	}
	if res.Err != string(KindExecutor) {
		t.Errorf("RunResult.Err = %q，期望 %q（分类而不是类型名）", res.Err, KindExecutor)
	}
}

// TestKindOf：取分类的语义边界。
func TestKindOf(t *testing.T) {
	if _, ok := KindOf(nil); ok {
		t.Error("nil 错误不得给出分类")
	}
	if _, ok := KindOf(errors.New("普通错误")); ok {
		t.Error("非 *harness.Error 不得给出分类")
	}
	if _, ok := KindOf(&Error{Kind: "", Op: "x"}); ok {
		t.Error("空 Kind 与「没有分类」同形，必须返回 ok=false")
	}
	// 包裹之后仍能取到分类。
	wrapped := errors.New("外层")
	if k, ok := KindOf(Ef(KindProvider, "round", "内层", wrapped)); !ok || k != KindProvider {
		t.Errorf("KindOf = (%q, %v)，期望 (%q, true)", k, ok, KindProvider)
	}
}

// ── 夹具 ──

func testRunSpec() RunSpec {
	return RunSpec{
		Scenario: "stub",
		Agent:    AgentSpec{Provider: "test", Model: "test-model"},
		Sandbox:  SandboxSpec{Image: "test-image", Workdir: "/work"},
		Budget:   Budget{MaxRounds: 10, MaxWall: time.Minute, MaxTurns: 100},
		Submit:   true,
	}
}

func newTestHarness(t *testing.T, sc Scenario, sb Sandbox, af AgentFactory,
	gate func(Challenge) CandidateGate) *Harness {
	t.Helper()
	opts := HarnessOptions{Scenario: sc, Sandbox: sb, Agents: af, Gate: gate}
	if gate != nil {
		opts.Planner = func(Challenge) Planner { return &stubPlanner{} }
		opts.Renderer = func(Challenge) Renderer { return stubRenderer{} }
	}
	h, err := NewHarness(opts)
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	return h
}

// stubPlanner 永远返回同一个意图，直到预算耗尽。
type stubPlanner struct{ activated int }

func (p *stubPlanner) Next(context.Context, PlannerInput) (*IntentRef, error) {
	p.activated++
	return &IntentRef{ID: "intent-1", Kind: "recon", Goal: "看看目标"}, nil
}
func (p *stubPlanner) Activate(*IntentRef)            {}
func (p *stubPlanner) Settle(*IntentRef, RoundResult) {}
func (p *stubPlanner) ObserveEvent(Event, int)        {}

type stubRenderer struct{}

func (stubRenderer) Render(context.Context, Challenge, *IntentRef, *OutcomeView) string {
	return "去看一眼目标"
}

// stubGate 是最小的候选账本：Observe 把工具输出里出现的 flag{...} 收成观测族候选。
//
// 为什么手写而不是用 gate.Gate：根包**不能**导入 gate（依赖方向单向），而这里
// 要测的是编排，不是候选抽取的判定质量（那由 gate 包自己的测试覆盖）。
type stubGate struct {
	mu   sync.Mutex
	seen map[string]Candidate
}

func newStubGate() *stubGate { return &stubGate{seen: map[string]Candidate{}} }

func (g *stubGate) Observe(ev Event) {
	if ev.Kind != EventToolEnd || ev.Output == "" {
		return
	}
	flag, ok := extractFlag(ev.Output)
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, dup := g.seen[flag]; dup {
		return
	}
	g.seen[flag] = Candidate{Flag: flag, Provenance: ProvenanceObserved, ToolCallID: ev.ToolCallID}
}

func (g *stubGate) Candidates() []Candidate {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Candidate, 0, len(g.seen))
	for _, c := range g.seen {
		out = append(out, c)
	}
	return out
}

func (g *stubGate) New() []Candidate { return g.NewAll() }

func (g *stubGate) NewAll() []Candidate {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Candidate
	for _, c := range g.seen {
		if !c.Submitted && c.Provenance != ProvenanceFabricated {
			out = append(out, c)
		}
	}
	return out
}

func (g *stubGate) Mark(flag string, res SubmitResult, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.seen[flag]
	if !ok {
		return
	}
	c.Submitted = true
	c.Correct = res.Correct
	g.seen[flag] = c
}

func (g *stubGate) SetIntent(string, int) {}

// extractFlag 从输出里取第一个 flag{...}。
func extractFlag(s string) (string, bool) {
	i := strings.Index(s, "flag{")
	if i < 0 {
		return "", false
	}
	j := strings.IndexByte(s[i:], '}')
	if j < 0 {
		return "", false
	}
	return s[i : i+j+1], true
}
