package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	closeErr    error
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
	return s.sb.closeErr
}

type fakeProcess struct {
	mu     sync.Mutex
	killed bool
	done   chan struct{}
	once   sync.Once
}

type fakeRunLocker struct{}

func (fakeRunLocker) Lock(context.Context) error { return nil }
func (fakeRunLocker) Unlock() error              { return nil }

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
	rounds    int
	started   int
	closed    int
	steers    []string
	prompts   []string
	lastStart AgentStart
	script    func(round int, emit func(Event))
	roundErr  error
	stats     Stats
	block     bool
	entered   chan struct{}
}

func (a *fakeAgent) Start(_ context.Context, req AgentStart) error {
	a.started++
	a.lastStart = req
	return nil
}
func (a *fakeAgent) Round(ctx context.Context, req RoundRequest) (RoundResult, error) {
	a.rounds++
	a.prompts = append(a.prompts, req.Prompt)
	if a.block {
		if a.entered != nil {
			close(a.entered)
		}
		<-ctx.Done()
		return RoundResult{Err: "cancelled"}, ctx.Err()
	}
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
func (a *fakeAgent) Stats(context.Context) (Stats, error) { return a.stats, nil }
func (a *fakeAgent) Close(context.Context) error          { a.closed++; return nil }

// scriptedAgentFactory 让每道题拿到同一个假 agent，并把 sink 接上。
type scriptedAgentFactory struct {
	agent *fakeAgent
	sink  EventSink
}

type sequenceAgentFactory struct {
	agents []*fakeAgent
	sink   EventSink
	next   int
}

func (f *sequenceAgentFactory) New(_ AgentSpec, _ SandboxSession, sink EventSink) (Agent, error) {
	f.sink = sink
	if f.next >= len(f.agents) {
		return nil, errors.New("unexpected agent restart")
	}
	agent := f.agents[f.next]
	f.next++
	return agent, nil
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
	challenges     []Challenge
	answers        map[string]string // code → 正确 flag
	mu             sync.Mutex
	prepared       int
	cleaned        int
	hints          int
	submitted      []string
	evalCalls      int
	reconcileCalls int
	evalErr        error
	cleanupErr     error
	discoverErr    error
}

func (s *stubScenario) Discover(context.Context, RunSpec) ([]Challenge, error) {
	if s.discoverErr != nil {
		return nil, s.discoverErr
	}
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
	s.evalCalls++
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
	s.reconcileCalls++
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
	return s.cleanupErr
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

// TestRunChallengeReclaimsOnProbeFailure：题级失败路径也必须回收资源。
//
// 装配错误（缺 Planner 等）现在被 NewHarness 挡住了，所以「一道题中途失败」这条
// 路径要由**运行期**故障来覆盖：Probe 失败时 session 与场景都必须被清理。
func TestRunChallengeReclaimsOnProbeFailure(t *testing.T) {
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

func TestRunRestartsProcessOnceWithSafeSummary(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}}, answers: map[string]string{"c1": "flag{answer}"}}
	sb := &fakeSandbox{}
	first := &fakeAgent{roundErr: errors.New("process exited")}
	second := &fakeAgent{}
	factory := &sequenceAgentFactory{agents: []*fakeAgent{first, second}}
	second.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call", "curl http://10.9.9.9:8080/", "flag{answer}")
	}
	h := newTestHarness(t, sc, sb, factory, func(Challenge) CandidateGate { return newStubGate() })
	res, err := h.Run(context.Background(), testRunSpec())
	if err != nil || !res.Completed {
		t.Fatalf("process restart did not recover: completed=%v err=%v", res.Completed, err)
	}
	if first.closed != 1 || second.closed != 1 || sb.sessions != 2 || sb.closed != 2 {
		t.Fatalf("restart lifecycle: first=%d second=%d sessions=%d closed=%d", first.closed, second.closed, sb.sessions, sb.closed)
	}
	if len(second.prompts) == 0 || !strings.Contains(second.prompts[0], "平台已确认进度") || strings.Contains(second.prompts[0], "flag{answer}") {
		t.Fatalf("recovery prompt missing safe summary or included answer: %q", second.prompts)
	}
}

// TestRunEarlyFailureReportsFailedState：起跑前就失败的运行必须是 failed 终态。
//
// State 的契约是「Run 返回了错误，State 就必是 failed 或 cancelled」。留空串会
// 让调用方退回解析错误字符串去判断发生了什么，而 errors.go 明令禁止那么做
// （「不要用字符串匹配错误消息来判断」）。
//
// 这几条路径都发生在任何题目起跑之前，用户也没按 Ctrl-C，所以是 failed 而不是
// cancelled——把它们报成取消会让人去找一个不存在的信号。
func TestRunEarlyFailureReportsFailedState(t *testing.T) {
	sc := &stubScenario{discoverErr: errors.New("平台不可达")}
	h := newTestHarness(t, sc, &fakeSandbox{}, &scriptedAgentFactory{agent: &fakeAgent{}},
		func(Challenge) CandidateGate { return newStubGate() })

	res, err := h.Run(context.Background(), testRunSpec())
	if err == nil {
		t.Fatal("Discover 失败必须让 Run 返回错误")
	}
	if res.State != RunFailed {
		t.Errorf("State = %q，期望 %q", res.State, RunFailed)
	}
	if res.Completed {
		t.Error("起跑前就失败的运行不得被记成解出")
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

// TestRunStagnationSwitchesBranchAfterHint：停滞 → 提示一次 → 仍然停滞 ⇒ 放弃
// 当前分支，调度未尝试的方向。
//
// 这是 M3 里唯一没落地的行为。判据是「连续两轮既无平台进度、也无新增宿主验证
// 事实」，处置是「提示后再次连续两轮停滞则放弃当前分支」。
//
// 轮次账（threshold = 2，HintAuto）：
//
//	1: a, dry=1
//	2: a, dry=2 ⇒ 请求提示（每题唯一一次），dry 归零
//	3: a, dry=1
//	4: a, dry=2 ⇒ 放弃 a
//	5: b, dry=1
//	6: b, dry=2 ⇒ 放弃 b
//	7: 前沿耗尽 ⇒ no_intent
func TestRunStagnationSwitchesBranchAfterHint(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{never}"}}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	// 每轮都产出一条**抽不出事实**的输出：既无平台进度，也无宿主新事实。
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call", "ls", "nothing here\n")
	}
	planner := &multiIntentPlanner{intents: []string{"intent-a", "intent-b"}}

	h := newTestHarnessWithPlanner(t, sc, &fakeSandbox{}, factory, planner)
	spec := testRunSpec()
	spec.HintPolicy = HintAuto

	res, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	cr := res.Challenges[0].Outcome
	if sc.hints != 1 {
		t.Errorf("提示请求 %d 次，期望 1（每题最多一次，换支不得放宽这条）", sc.hints)
	}
	if got := planner.abandoned; len(got) != 2 || got[0] != "intent-a" || got[1] != "intent-b" {
		t.Errorf("被放弃的分支 = %v，期望先 a 后 b", got)
	}
	if cr.BranchesAbandoned != 2 {
		t.Errorf("BranchesAbandoned = %d，期望 2（报告要能解释为什么停）", cr.BranchesAbandoned)
	}
	if cr.Reason != ReasonNoIntent {
		t.Errorf("Reason = %q，期望 %q（两支都被放弃后前沿为空）", cr.Reason, ReasonNoIntent)
	}
}

// TestRunHostFactsPreventStagnation：有新增宿主事实的轮次**不算**停滞。
//
// 只看平台进度是不够的——一道题在拿到 flag 之前往往先积累一批真实事实
// （banner、凭据线索、可达服务），那正是有进展的样子。把它们读成停滞，agent
// 会在真的推进时被反复打断，甚至被换支。
func TestRunHostFactsPreventStagnation(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{never}"}}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call", "nmap", "80/tcp open http\n")
	}
	// 每见一个事件就抬一格宿主事实水位线：平台进度始终为 0，但事实在增长。
	planner := &multiIntentPlanner{intents: []string{"intent-a", "intent-b"}, bumpOnObserve: true}

	h := newTestHarnessWithPlanner(t, sc, &fakeSandbox{}, factory, planner)
	spec := testRunSpec()
	spec.HintPolicy = HintAuto
	spec.Budget = Budget{MaxRounds: 5, MaxWall: time.Minute, MaxTurns: 100}

	res, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	cr := res.Challenges[0].Outcome
	if sc.hints != 0 {
		t.Errorf("提示请求 %d 次，期望 0——宿主事实在增长就不是停滞", sc.hints)
	}
	if cr.BranchesAbandoned != 0 {
		t.Errorf("BranchesAbandoned = %d，期望 0——不得把有进展的分支换掉", cr.BranchesAbandoned)
	}
	// 跑到轮次预算耗尽，说明它一直在正常推进而不是被判停滞。
	if cr.Reason != ReasonMaxRounds {
		t.Errorf("Reason = %q，期望 %q", cr.Reason, ReasonMaxRounds)
	}
}

// TestRunStateSeparatesFinishFromSolved：运行终态与「题目是否解出」是两个问题。
//
// 一次运行可以正常结束却一道题都没解出来（ReasonNoProgress），也可以被取消。
// 用同一个字段表示这两件事，报告就分不清「跑完了但什么都没解出来」与
// 「跑完了且解出来了」——前者正是前身「280 run / 0 flag」的形状。
func TestRunStateSeparatesFinishFromSolved(t *testing.T) {
	t.Run("跑完但没解出", func(t *testing.T) {
		sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
			answers: map[string]string{"c1": "flag{never}"}}
		ag := &fakeAgent{}
		factory := &scriptedAgentFactory{agent: ag}
		ag.script = func(_ int, _ func(Event)) { emitToolEnd(factory.sink, "call", "ls", "nothing\n") }
		h := newTestHarness(t, sc, &fakeSandbox{}, factory, func(Challenge) CandidateGate { return newStubGate() })
		spec := testRunSpec()
		spec.Budget = Budget{MaxRounds: 2, MaxWall: time.Minute, MaxTurns: 100}

		res, err := h.Run(context.Background(), spec)
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
		if res.State != RunFinished {
			t.Errorf("State = %q，期望 %q", res.State, RunFinished)
		}
		if res.Completed {
			t.Error("没解出任何题时 Completed 必须为假")
		}
		if res.Reason != ReasonNoProgress {
			t.Errorf("Reason = %q，期望 %q", res.Reason, ReasonNoProgress)
		}
	})

	t.Run("解出", func(t *testing.T) {
		sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
			answers: map[string]string{"c1": "flag{real-answer}"}}
		ag := &fakeAgent{}
		factory := &scriptedAgentFactory{agent: ag}
		ag.script = func(_ int, _ func(Event)) {
			emitToolEnd(factory.sink, "call", "cat", "flag{real-answer}\n")
		}
		h := newTestHarness(t, sc, &fakeSandbox{}, factory, func(Challenge) CandidateGate { return newStubGate() })

		res, err := h.Run(context.Background(), testRunSpec())
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
		if res.State != RunFinished || !res.Completed {
			t.Errorf("State/Completed = %q/%v，期望 %q/true", res.State, res.Completed, RunFinished)
		}
	})
}

// TestBundleDigestTracksContent：扩展包内容摘要必须随**内容**变化。
//
// SolverProfile.Digest() 只对结构体 JSON 取摘要，而 ExtensionBundle 在那里面是
// 一个路径字符串——同一个路径下内容换了，摘要不变，于是报告会把两份不同的扩展包
// 算作同一组，profile 对照实验的结论是错的。
func TestBundleDigestTracksContent(t *testing.T) {
	// 空路径 = 未核验。空串是它与「内容没问题」的区别所在。
	if got, err := bundleDigest(""); err != nil || got != "" {
		t.Fatalf("空路径应得到空串（未核验），got %q err %v", got, err)
	}
	// 不存在或不是目录 ⇒ fail closed，绝不静默返回空串。
	if _, err := bundleDigest(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("路径不存在时必须报错，而不是当成「未核验」")
	}

	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.js", "one")
	write("sub/b.js", "two")
	base, err := bundleDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if base == "" {
		t.Fatal("有内容的 bundle 不该得到空摘要")
	}
	// 同一份内容重复算必须稳定（目录遍历顺序不稳定，不排序就会漂）。
	again, err := bundleDigest(dir)
	if err != nil || again != base {
		t.Fatalf("同一份内容两次摘要不同: %q vs %q (err=%v)", base, again, err)
	}

	// 改内容 ⇒ 摘要变。
	write("a.js", "ONE")
	changed, err := bundleDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if changed == base {
		t.Error("扩展包内容变了但摘要没变——profile 分组会把两次不同的实验算成一组")
	}

	// 只追加一个空文件也要变（路径本身参与摘要）。
	write("c.js", "")
	added, err := bundleDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if added == changed {
		t.Error("新增文件后摘要必须变")
	}

	// 长度前缀：拼接歧义必须被区分开，否则 "ab"+"c" 与 "a"+"bc" 会撞成同一个摘要。
	split := t.TempDir()
	if err := os.WriteFile(filepath.Join(split, "x"), []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(split, "y"), []byte("c"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "x"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "y"), []byte("bc"), 0o600); err != nil {
		t.Fatal(err)
	}
	d1, err1 := bundleDigest(split)
	d2, err2 := bundleDigest(other)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if d1 == d2 {
		t.Error("不同切分的内容算出了同一个摘要——长度前缀没起作用")
	}
}

// TestRunRecordsChallengeDuration：题目耗时必须真的被记下来。
//
// 回归：`OutcomeView.StartedAt/EndedAt` 曾经**从未被赋值**（只设了
// ChallengeResult 上那两个同名字段），而耗时——CLI 摘要的「耗时」、公开结果的
// `durationSeconds`、stats 的累计耗时——三处全部走 `OutcomeView.Duration()`。
// 于是它们恒为 0：字段在、文档写了、落盘了，但永远是零。实测现场：一次真跑里
// 题目实际耗时约 10 分钟，公开结果写的是 `durationSeconds: 0`。
//
// 用可注入时钟让断言确定：真实时钟下两次 now() 之差也可能被算成 0。
func TestRunRecordsChallengeDuration(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{never}"}}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	ag.script = func(_ int, _ func(Event)) { emitToolEnd(factory.sink, "call", "ls", "nothing\n") }

	base := time.Unix(1700000000, 0)
	var tick int
	h, err := NewHarness(HarnessOptions{Scenario: sc, Sandbox: &fakeSandbox{}, Agents: factory,
		Gate:    func(Challenge) CandidateGate { return newStubGate() },
		Results: &recordingResults{}, Locker: fakeRunLocker{},
		Planner:  func(Challenge) Planner { return &stubPlanner{} },
		Renderer: func(Challenge) Renderer { return stubRenderer{} },
		Now:      func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Second) }})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	spec := testRunSpec()
	spec.Budget = Budget{MaxRounds: 1, MaxWall: time.Hour, MaxTurns: 100}

	res, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if got := res.Challenges[0].Outcome.Duration(); got <= 0 {
		t.Fatalf("题目耗时 = %v，必须为正——CLI 摘要、公开结果与 stats 都读它", got)
	}
}

func TestRunCostBudgetUsesAgentStats(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}}, answers: map[string]string{"c1": "flag{never}"}}
	ag := &fakeAgent{stats: Stats{Turns: 2, CostUSD: 0.02}}
	h := newTestHarness(t, sc, &fakeSandbox{}, &scriptedAgentFactory{agent: ag}, func(Challenge) CandidateGate { return newStubGate() })
	spec := testRunSpec()
	spec.Budget.MaxCostUSD = 0.01
	res, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Challenges[0].Outcome
	if got.Reason != ReasonMaxCost || got.Rounds != 1 || got.Stats.CostUSD != 0.02 {
		t.Fatalf("cost budget did not stop after measured round: %+v", got)
	}
}

func TestRunSubmitUncertainReconcilesAndStops(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{answer}"}, evalErr: errors.New("platform write timeout")}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call", "curl http://10.9.9.9:8080/", "flag{answer}")
	}
	h := newTestHarness(t, sc, &fakeSandbox{}, factory, func(Challenge) CandidateGate { return newStubGate() })
	res, err := h.Run(context.Background(), testRunSpec())
	if !IsKind(err, KindPlatform) {
		t.Fatalf("uncertain submit kind = %v, want platform", err)
	}
	if sc.evalCalls != 1 || sc.reconcileCalls != 1 || ag.rounds != 1 {
		t.Fatalf("uncertain submit was retried or not reconciled: eval=%d reconcile=%d rounds=%d", sc.evalCalls, sc.reconcileCalls, ag.rounds)
	}
	if got := res.Challenges[0].Outcome; got.Err != "提交结果不确定" || got.Reason != ReasonError || got.Submitted != 0 {
		t.Fatalf("uncertain submit classified as success: %+v", got)
	}
}

func TestRunCancellationPersistsResultAndCleanupFailures(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}}, cleanupErr: errors.New("cleanup failed")}
	sb := &fakeSandbox{closeErr: errors.New("sandbox cleanup failed")}
	ag := &fakeAgent{block: true, entered: make(chan struct{})}
	h := newTestHarness(t, sc, sb, &scriptedAgentFactory{agent: ag}, func(Challenge) CandidateGate { return newStubGate() })
	recorded := &recordingResults{}
	h.results = recorded
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type runReturn struct {
		res RunResult
		err error
	}
	done := make(chan runReturn, 1)
	go func() { res, err := h.Run(ctx, testRunSpec()); done <- runReturn{res, err} }()
	select {
	case <-ag.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("agent round did not start")
	}
	cancel()
	select {
	case got := <-done:
		if !IsKind(got.err, KindCancelled) || got.res.Reason != ReasonStopped {
			t.Fatalf("cancellation classification: reason=%q err=%v", got.res.Reason, got.err)
		}
		if len(recorded.saved) != 1 || recorded.saveCtxErr != nil {
			t.Fatalf("cancelled result was not saved with a fresh context: saves=%d ctxErr=%v", len(recorded.saved), recorded.saveCtxErr)
		}
		failures := got.res.Challenges[0].Outcome.CleanupFailures
		if len(failures) != 2 || failures[0] != "sandbox" || failures[1] != "scenario" {
			t.Fatalf("cleanup failures were not recorded separately: %v", failures)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled Run did not terminate")
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

// TestRunFreezesProfileIntoAgentStart：profile 的 system prompt 必须真的到达 agent。
//
// 回归：Run 只转发 spec.Agent.*，profile 的 SystemPrompt 没有任何注入路径——
// 它在 SolverProfile 里存在、进 Digest、写进公开结果，却从不生效。一个「看起来
// 在、实际没生效」的字段比没有这个字段更糟：它会让 profile 对照实验得出错误结论。
func TestRunFreezesProfileIntoAgentStart(t *testing.T) {
	sc := &stubScenario{challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers: map[string]string{"c1": "flag{x}"}}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}

	opts := HarnessOptions{Scenario: sc, Sandbox: sb, Agents: factory, Locker: fakeRunLocker{},
		Planner:  func(Challenge) Planner { return &stubPlanner{} },
		Renderer: func(Challenge) Renderer { return stubRenderer{} },
		Gate:     func(Challenge) CandidateGate { return newStubGate() },
		Results:  &recordingResults{},
		Profile:  SolverProfile{Name: "p1", SystemPrompt: "你是一个授权的 CTF 解题助手"}}
	h, err := NewHarness(opts)
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	if _, err := h.Run(context.Background(), testRunSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ag.lastStart.SystemPrompt != "你是一个授权的 CTF 解题助手" {
		t.Errorf("AgentStart.SystemPrompt = %q，profile 的 prompt 没有到达 agent",
			ag.lastStart.SystemPrompt)
	}
	// Workdir 必须是容器内路径，不是宿主路径。
	if ag.lastStart.Workdir != "/work" {
		t.Errorf("AgentStart.Workdir = %q，期望容器内路径 /work", ag.lastStart.Workdir)
	}
	// bundle 必须是一个**真实存在**的目录：Run 会在任何副作用之前算它的内容摘要
	// 并冻结（算不出来就 fail closed），所以这里不能用 /tmp 下的假路径。
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "ext.js"), []byte("// solver extension\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	override := SolverProfile{Name: "run-specific", SystemPrompt: "本次运行的提示", ExtensionBundle: bundle}
	spec := testRunSpec()
	spec.Profile = override
	res, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("override Run: %v", err)
	}
	if ag.lastStart.SystemPrompt != override.SystemPrompt || res.ProfileDigest != override.Digest() {
		t.Errorf("RunSpec.Profile 未统一驱动 prompt 与摘要: prompt=%q digest=%q", ag.lastStart.SystemPrompt, res.ProfileDigest)
	}
	if sb.specs[len(sb.specs)-1].ProfileDir != override.ExtensionBundle {
		t.Error("RunSpec.Profile 的 bundle 未进入 sandbox")
	}
	if res.BundleDigest == "" {
		t.Error("配了 bundle 就必须冻结内容摘要，空串是「未核验」的意思")
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
	opts := HarnessOptions{Scenario: sc, Sandbox: sb, Agents: af, Gate: gate, Results: &recordingResults{}, Locker: fakeRunLocker{}}
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

// newTestHarnessWithPlanner 与 newTestHarness 相同，但允许注入自己实现的
// Planner——换支与停滞用例需要「放弃一支之后能拿到另一支」，而默认的
// stubPlanner 永远返回同一个意图。
func newTestHarnessWithPlanner(t *testing.T, sc Scenario, sb Sandbox, af AgentFactory,
	planner Planner) *Harness {
	t.Helper()
	h, err := NewHarness(HarnessOptions{Scenario: sc, Sandbox: sb, Agents: af,
		Gate:    func(Challenge) CandidateGate { return newStubGate() },
		Results: &recordingResults{}, Locker: fakeRunLocker{},
		Planner:  func(Challenge) Planner { return planner },
		Renderer: func(Challenge) Renderer { return stubRenderer{} }})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	return h
}

// recordingResults 是记账型 ResultStore：只记 Save 调用，不落盘。
//
// 它同时也是「NewHarness 拒绝缺 Results」这条守卫的对照物——缺了它这台 Harness
// 会把「跑完不落盘」变成一次静默成功。
type recordingResults struct {
	mu         sync.Mutex
	saved      []RunResult
	saveCtxErr error
}

func (r *recordingResults) Save(ctx context.Context, res RunResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saved = append(r.saved, res)
	r.saveCtxErr = ctx.Err()
	return nil
}
func (r *recordingResults) Get(context.Context, RunID) (RunResult, error) {
	return RunResult{}, errors.New("未实现")
}
func (r *recordingResults) List(context.Context) ([]RunResult, error) { return nil, nil }
func (r *recordingResults) Stats(context.Context, StatsQuery) (StatsReport, error) {
	return StatsReport{}, nil
}

// TestNewHarnessRejectsMissingPorts：生产必需端口缺一个就启动失败。
//
// 缺 Planner/Renderer/Gate 时每道题都会走「未执行任何轮次」，报告把装配错误读成
// 「模型不行」；缺 Results 时「跑完不落盘」是一次静默成功，表现为「跑了几十次，
// stats 说零次」。两者都必须在启动时挡住。
func TestNewHarnessRejectsMissingPorts(t *testing.T) {
	base := func() HarnessOptions {
		return HarnessOptions{
			Scenario: &stubScenario{}, Sandbox: &fakeSandbox{},
			Agents:   &scriptedAgentFactory{agent: &fakeAgent{}},
			Planner:  func(Challenge) Planner { return &stubPlanner{} },
			Renderer: func(Challenge) Renderer { return stubRenderer{} },
			Gate:     func(Challenge) CandidateGate { return newStubGate() },
			Results:  &recordingResults{},
			Locker:   fakeRunLocker{},
		}
	}
	if _, err := NewHarness(base()); err != nil {
		t.Fatalf("齐备端口不应失败: %v", err)
	}
	for _, tc := range []struct {
		name string
		drop func(*HarnessOptions)
	}{
		{"Scenario", func(o *HarnessOptions) { o.Scenario = nil }},
		{"Sandbox", func(o *HarnessOptions) { o.Sandbox = nil }},
		{"Agents", func(o *HarnessOptions) { o.Agents = nil }},
		{"Planner", func(o *HarnessOptions) { o.Planner = nil }},
		{"Renderer", func(o *HarnessOptions) { o.Renderer = nil }},
		{"Gate", func(o *HarnessOptions) { o.Gate = nil }},
		{"Results", func(o *HarnessOptions) { o.Results = nil }},
		{"Locker", func(o *HarnessOptions) { o.Locker = nil }},
	} {
		o := base()
		tc.drop(&o)
		_, err := NewHarness(o)
		if err == nil {
			t.Errorf("缺 %s 时必须启动失败", tc.name)
			continue
		}
		if !IsKind(err, KindConfig) {
			t.Errorf("缺 %s 的错误分类 = %v，期望 KindConfig", tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.name) {
			t.Errorf("缺 %s 的错误消息必须点名它: %v", tc.name, err)
		}
	}
}

// stubPlanner 永远返回同一个意图，直到预算耗尽。
//
// abandoned / hostFacts 是给停滞与换支用例用的可编程点：默认零值等价于「从不
// 放弃、从不产生宿主事实」，也就是旧行为，所以既有用例不受影响。
type stubPlanner struct {
	activated int
	abandoned []string
	hostFacts int
}

func (p *stubPlanner) Next(context.Context, PlannerInput) (*IntentRef, error) {
	p.activated++
	return &IntentRef{ID: "intent-1", Kind: "recon", Goal: "看看目标"}, nil
}
func (p *stubPlanner) Activate(*IntentRef)            {}
func (p *stubPlanner) Settle(*IntentRef, RoundResult) {}
func (p *stubPlanner) ObserveEvent(Event, int)        {}
func (p *stubPlanner) HostFacts() int                 { return p.hostFacts }
func (p *stubPlanner) Abandon(it *IntentRef) {
	if it == nil {
		return
	}
	p.abandoned = append(p.abandoned, it.ID)
}

// multiIntentPlanner 按**前沿语义**提供多个意图：Next 返回第一个尚未被放弃的，
// 全被放弃后返回 (nil, nil)（与 dag.Scheduler 的收尾信号同形）。
//
// 为什么不能用 stubPlanner 测换支：它永远返回同一个意图，于是「放弃之后拿到了
// 另一支」这件事在它身上根本无法表达——测试会通过，而换支并没有发生。
type multiIntentPlanner struct {
	intents   []string
	abandoned []string
	hostFacts int
	// bumpOnObserve 为真时每个事件都把宿主事实水位线抬一格，用来模拟「本轮真的
	// 从工具输出里抽到了新事实」。
	bumpOnObserve bool
	// current 记录 Next 最近一次返回的意图，供断言「agent 在哪一支上跑」。
	current string
}

func (p *multiIntentPlanner) Next(context.Context, PlannerInput) (*IntentRef, error) {
	for _, id := range p.intents {
		if p.wasAbandoned(id) {
			continue
		}
		p.current = id
		return &IntentRef{ID: id, Kind: "recon", Goal: "目标 " + id}, nil
	}
	p.current = ""
	return nil, nil
}

func (p *multiIntentPlanner) wasAbandoned(id string) bool {
	for _, a := range p.abandoned {
		if a == id {
			return true
		}
	}
	return false
}

func (p *multiIntentPlanner) Activate(*IntentRef)            {}
func (p *multiIntentPlanner) Settle(*IntentRef, RoundResult) {}
func (p *multiIntentPlanner) ObserveEvent(Event, int) {
	if p.bumpOnObserve {
		p.hostFacts++
	}
}
func (p *multiIntentPlanner) HostFacts() int { return p.hostFacts }
func (p *multiIntentPlanner) Abandon(it *IntentRef) {
	if it == nil {
		return
	}
	p.abandoned = append(p.abandoned, it.ID)
}

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

func (g *stubGate) Mark(flag string, res Evaluation, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.seen[flag]
	if !ok {
		return
	}
	c.Submitted = true
	c.Correct = res.Accepted
	// Duplicate 的派生与 gate.Gate 保持一致（Accepted && !Progress）：两份实现
	// 在这里漂移的话，编排测试断言的「重复计入确认」就会与生产行为不符。
	c.Duplicate = res.Accepted && !res.Progress
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
