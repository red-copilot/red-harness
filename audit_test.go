package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// ── 候选审计：这组用例对应 R1 的出口条件「候选审计对账」 ──
//
// 要证明的不是「有个函数会被调用」，而是两件具体的事：
//
//  1. **每一次提交尝试都留下一行**，包括「提交结果不确定」那一档——那正是
//     「平台写超时、这一条到底算不算提交」的场景，而只记成功的审计恰好答不出它；
//  2. 公开面的计数（Submitted / Duplicates / Rejected）与这些行**对得上**。
//
// 在此之前 Duplicates 与 Rejected 是**死字段**（model.go 里定义、全仓零赋值点），
// 而「提交了 147 次、判错 146 条」这类事实因此在公开面上根本看不见。

// auditResults 是同时实现 ResultStore 与 AuditStore 的记账型假件。
//
// 它复刻的是 store.ResultFileStore 在装配里的角色：审计与公开结果是同一棵树上
// 的两个目录，由同一个实现提供——所以「能不能审计」由 Results 决定。
type auditResults struct {
	recordingResults
	mu      sync.Mutex
	records []CandidateAudit
}

func (r *auditResults) AppendAudit(_ context.Context, _ RunID, _ string, rec CandidateAudit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	return nil
}

func (r *auditResults) all() []CandidateAudit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CandidateAudit(nil), r.records...)
}

// failingAuditResults 是「**只有审计写不出去、公开结果照常写得进去**」的假件。
//
// 它存在的理由就是击穿旧实现里那句 `_ = audits.AppendAudit(...)` 的自辩——「能写坏
// 这次追加的机器，紧接着也会写坏 results.Save」。Save 继承自 recordingResults，
// **永远返回 nil**；AppendAudit 在第 failAfter 行之后永远返回 err。于是「两件事
// 同时坏」这个前提在本假件下**为假**，而旧实现把两处处置写成同一个。
//
// 对应物不是人造的：store/audit.go 的审计有它**自己**的两条上限（单行 64 KiB、
// 单文件 16 MiB）与自己的 ctx 检查，与 results/<runID>.json 完全独立——一条超长
// 候选行会被拒，而公开指标照常落盘。
type failingAuditResults struct {
	auditResults
	// failAfter 是**成功**写入多少行之后开始失败（0 表示第一次就失败）。
	failAfter int
	err       error
}

func (r *failingAuditResults) AppendAudit(ctx context.Context, runID RunID, code string, rec CandidateAudit) error {
	r.mu.Lock()
	n := len(r.records)
	r.mu.Unlock()
	if n >= r.failAfter {
		return r.err
	}
	return r.auditResults.AppendAudit(ctx, runID, code, rec)
}

// newAuditHarness 与 newTestHarness 同形，但 Results 换成可审计的假件。
func newAuditHarness(t *testing.T, sc Scenario, sb Sandbox, af AgentFactory) (*Harness, *auditResults) {
	t.Helper()
	res := &auditResults{}
	return newAuditHarnessWithResults(t, sc, sb, af, res), res
}

// newAuditHarnessWithResults 允许注入任意 ResultStore。
//
// 这一维正是本组用例要区分的东西：「Results 是不是审计落点」决定的是**能不能记账**，
// 与「这次运行要不要做平台写操作」（RunSpec.Submit）是两件事，两条组合的处置完全不同。
func newAuditHarnessWithResults(t *testing.T, sc Scenario, sb Sandbox, af AgentFactory, res ResultStore) *Harness {
	t.Helper()
	h, err := NewHarness(HarnessOptions{Scenario: sc, Sandbox: sb, Agents: af,
		Gate:     func(Challenge) CandidateGate { return newStubGate() },
		Results:  res,
		Locker:   fakeRunLocker{},
		Planner:  func(Challenge) Planner { return &stubPlanner{} },
		Renderer: func(Challenge) Renderer { return stubRenderer{} }})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	return h
}

// TestAuditReconcilesWithPublicCounts 是这组的核心：一次运行里提交了三条候选
// （一条确认、一条幂等重复、一条判错），审计行数与公开计数必须互相印证。
func TestAuditReconcilesWithPublicCounts(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{right}"},
	}
	// 让 Evaluate 按候选给三种判定：正确 / 幂等重复 / 判错。
	sc.evalHook = func(flag string) Evaluation {
		switch flag {
		case "flag{right}":
			return Evaluation{Accepted: true, Progress: true, Score: 10}
		case "flag{dup}":
			// Accepted 但没有进度 ⇒ 幂等命中。
			return Evaluation{Accepted: true, Progress: false}
		default:
			return Evaluation{Message: "平台说这个不对"}
		}
	}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	// 三次工具调用、三条候选。**刻意不写成一次输出里三个 flag**：stubGate 用
	// extractFlag 只取第一个（与 gate.Gate 的多候选抽取不是一回事），一次输出
	// 只会得到一条候选，用例会退化成「只提交一条」而不报错。
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call-1", "curl http://10.9.9.9:8080/a", "flag{right}\n")
		emitToolEnd(factory.sink, "call-2", "curl http://10.9.9.9:8080/b", "flag{dup}\n")
		emitToolEnd(factory.sink, "call-3", "curl http://10.9.9.9:8080/c", "flag{wrong}\n")
	}
	h, res := newAuditHarness(t, sc, sb, factory)

	spec := testRunSpec()
	spec.Budget = Budget{MaxRounds: 1, MaxWall: 0, MaxTurns: 0}
	got, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	records := res.all()
	if len(records) == 0 {
		t.Fatal("没有任何审计记录——提交了候选却没有审计行")
	}

	// 对账 1：审计行数 == 本题的提交尝试次数（逐条判定与公开计数一起对）。
	//
	// 口径按根包契约来：**Correct 含幂等命中**（Duplicate 是它的子集），所以
	// Submitted 数的是全部 Correct，Duplicates 单独再数一次其中重复的那些。
	// 把 Duplicate 当成与 Correct 互斥的一档去数，会得到 Submitted=2 而
	// 「正确」=1，看起来像计数错了——错的是数法。
	var correct, dup, rejected int
	for _, r := range records {
		switch {
		case r.Verdict.Uncertain:
			t.Errorf("本用例不该出现不确定提交: %+v", r)
		case r.Verdict.Rejected:
			rejected++
		case r.Verdict.Correct:
			correct++
			if r.Verdict.Duplicate {
				dup++
			}
		default:
			t.Errorf("审计行既没判定也没标错: %+v", r)
		}
	}
	cr := got.Challenges[0]
	if got := cr.Outcome.Submitted; got != correct {
		t.Errorf("公开的 Submitted = %d，审计里确认的有 %d 条", got, correct)
	}
	if got := cr.Outcome.Duplicates; got != dup {
		t.Errorf("公开的 Duplicates = %d，审计里幂等命中的有 %d 条", got, dup)
	}
	if got := cr.Outcome.Rejected; got != rejected {
		t.Errorf("公开的 Rejected = %d，审计里判错的有 %d 条", got, rejected)
	}
	// 这两个字段在 v0.5 之前是**死字段**（model.go 里有定义、全仓零赋值点），
	// 本用例顺带钉住它们活了。三条候选必须恰好覆盖三档判定，否则这一组用例
	// 可能因为「只提交了一条」而全绿地空转。
	if cr.Outcome.Duplicates == 0 || cr.Outcome.Rejected == 0 {
		t.Fatalf("Duplicates/Rejected 仍未被回填: %+v", cr.Outcome)
	}
	if correct != 2 || dup != 1 || rejected != 1 || len(records) != 3 {
		t.Fatalf("三条候选的判定分布不对：correct=%d dup=%d rejected=%d 行数=%d",
			correct, dup, rejected, len(records))
	}

	// 对账 2：每条审计必须带上明文与「提交了什么」所需的锚点。
	for _, r := range records {
		if r.Flag == "" {
			t.Error("审计行没有明文——「提交了什么」就答不出来了")
		}
		if r.SubmittedAt.IsZero() {
			t.Error("审计行没有提交时间")
		}
	}
}

// TestAuditRecordsUncertainSubmission：「结果不确定」也必须留一行。
//
// 这一档最容易被漏掉：它不是「成功」也不是「判错」，而是「我们不知道平台收没
// 收到」。而事后要判断「这一条到底算不算提交过」，唯一的依据就是这行审计。
func TestAuditRecordsUncertainSubmission(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{x}"},
		evalErr:    errors.New("platform write timeout"),
	}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call-1", "curl http://10.9.9.9:8080/", "flag{x}\n")
	}
	h, res := newAuditHarness(t, sc, sb, factory)

	got, err := h.Run(context.Background(), testRunSpec())
	if err == nil {
		t.Fatal("提交结果不确定时应以错误收场")
	}
	records := res.all()
	if len(records) != 1 {
		t.Fatalf("审计行数 = %d，期望 1（不确定的那一次也必须留痕）", len(records))
	}
	if !records[0].Verdict.Uncertain {
		t.Errorf("这行应标为 uncertain: %+v", records[0])
	}
	if records[0].SubmitError == "" {
		t.Error("不确定提交应带上错误文本，否则事后无从判断平台到底怎么了")
	}
	if got.Challenges[0].Outcome.Reason != ReasonError {
		t.Errorf("本题 Reason = %q，期望 %q", got.Challenges[0].Outcome.Reason, ReasonError)
	}
}

// TestAuditIsOptionalPort：不接审计端口时**干跑**照常，不得失败。
//
// 它与 GraphSaver 同档：可选端口。但「没接」必须可见——Doctor 有一行
// candidate_audit，本用例把那一行也钉住。
//
// ⚠️ 这里的 spec 是 `Submit=false`：审计端口**只在干跑下**可选。Submit=true 时
// 它缺席会在任何平台调用之前被拒（下面 TestAuditFailWithoutPortRejectsSubmitRun
// 钉住那一档）——两次运行各写一次「不可追回的平台写操作」，而没有审计就永远答不出
// 「提交了什么、平台怎么判的」。
func TestAuditIsOptionalPort(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{x}"},
	}
	sb := &fakeSandbox{}
	// recordingResults —— **不**实现 AuditStore。
	h := newAuditHarnessWithResults(t, sc, sb, &scriptedAgentFactory{agent: &fakeAgent{}},
		&recordingResults{})
	spec := testRunSpec()
	spec.Submit = false
	if _, err := h.Run(context.Background(), spec); err != nil {
		t.Fatalf("干跑不接审计端口不该让运行失败: %v", err)
	}
	// 干跑的定义就是「没有平台写操作」——这一条同时说明上面那句「照常」不是
	// 靠把候选都过滤掉实现的。
	if sc.evalCalls != 0 {
		t.Errorf("干跑调了 %d 次 Evaluate，期望 0（干跑不该有平台写操作）", sc.evalCalls)
	}

	rep := h.Doctor(context.Background())
	var found *DoctorCheck
	for i := range rep.Checks {
		if rep.Checks[i].Name == "candidate_audit" {
			found = &rep.Checks[i]
		}
	}
	if found == nil {
		t.Fatal("Doctor 里没有 candidate_audit 一行：不接审计会变成一次静默的缺失")
	}
	if found.OK {
		t.Error("没接审计却报 OK")
	}
	if found.Fatal {
		t.Error("审计是可选端口，不该是 Fatal——那会让「没接审计」挡住整次运行")
	}
	if found.Detail == "" {
		t.Error("未接入时的文案必须说明后果，否则读的人只会看到一行「没接」")
	}
}

// TestAuditFailStopsRunAndKeepsConfirmedOutcome 是 N0.4 的核心：**审计写不出去是
// 运行级故障，但它绝不允许改写已经确认的成绩**。
//
// 三件事同时断言，缺一条这个用例就退化成「有个错误返回了」：
//
//  1. 返回的错误是 KindPersistence 且带 ErrAuditIncomplete 哨兵——调用方据此停机；
//  2. 本题**已确认**的成绩原样保留（Submitted / Flags / Score），Reason 与 Err 换成
//     审计那一档；把解出来的题改写成没解出来，是比丢一行审计更坏的事故；
//  3. **后续题目不再执行**——这道题停下不等于整次运行悄悄继续跑。
//
// 为什么要用 `failAfter: 1`：它让「本题的第一次提交已经成功、审计也写了」这一档
// 成为前提，于是「保留已确认成绩」这句话才有东西可保留；顺带钉住 Attempts 计的是
// **尝试**（2）而不是确认数（1）——审计失败的正是第二次尝试。
func TestAuditFailStopsRunAndKeepsConfirmedOutcome(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 2}, {Code: "c2", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{never}"},
	}
	// 任何候选都被平台确认：本题的结果与 stubGate 的 map 迭代顺序无关（否则
	// 「哪一条先提交」会随运行变化，断言就成了掷骰子）。
	sc.evalHook = func(string) Evaluation {
		return Evaluation{Accepted: true, Progress: true, Score: 10, Message: "平台确认"}
	}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call-1", "curl http://t/a", "flag{one}\n")
		emitToolEnd(factory.sink, "call-2", "curl http://t/b", "flag{two}\n")
	}
	res := &failingAuditResults{failAfter: 1, err: errors.New("审计单行超过 64 KiB 上限")}
	h := newAuditHarnessWithResults(t, sc, sb, factory, res)

	spec := testRunSpec()
	spec.Budget = Budget{MaxRounds: 1, MaxWall: 0, MaxTurns: 0}
	got, err := h.Run(context.Background(), spec)
	if err == nil {
		t.Fatal("审计写不出去必须以错误收场——旧实现把它 `_ =` 丢掉了")
	}
	if !IsKind(err, KindPersistence) {
		t.Fatalf("错误分类 = %v，期望 KindPersistence", err)
	}
	if !errors.Is(err, ErrAuditIncomplete) {
		t.Errorf("错误链上缺 ErrAuditIncomplete 哨兵，调用方只能去解析消息: %v", err)
	}
	if got.State != RunFailed {
		t.Errorf("State = %q，期望 %q", got.State, RunFailed)
	}
	if got.Err != string(KindPersistence) {
		t.Errorf("RunResult.Err = %q，期望 %q（公开面只放分类）", got.Err, KindPersistence)
	}

	cr := got.Challenges[0]
	// 1）本题被正确标记。
	if !cr.Outcome.AuditIncomplete {
		t.Error("AuditIncomplete 未被置真——公开面上「这次运行的提交记录不完整」不可见")
	}
	if cr.Outcome.Reason != ReasonError {
		t.Errorf("Reason = %q，期望 %q", cr.Outcome.Reason, ReasonError)
	}
	if cr.Outcome.Err != "候选审计未完整落盘" {
		t.Errorf("本题 Err = %q，必须是固定文案（原始错误可能带平台响应片段）", cr.Outcome.Err)
	}
	// 2）**已确认的成绩原样保留**。
	if cr.Outcome.Submitted != 1 || len(cr.Outcome.Flags) != 1 {
		t.Errorf("审计失败把已确认的成绩抹掉了：Submitted=%d Flags=%v（第一次提交平台已经确认过，审计也写成功了）",
			cr.Outcome.Submitted, cr.Outcome.Flags)
	}
	if cr.Outcome.Score != 10 {
		t.Errorf("Score = %d，期望 10——已确认的得分不得因落盘故障清零", cr.Outcome.Score)
	}
	// Attempts 与 Submitted 在这条路径上**必须不同**：失败的正是第二次尝试。
	if cr.Outcome.Attempts != 2 {
		t.Errorf("Attempts = %d，期望 2（含判不出结果/未记账的那一次）", cr.Outcome.Attempts)
	}
	if sc.evalCalls != 2 {
		t.Errorf("Evaluate 调用 = %d 次，期望 2", sc.evalCalls)
	}
	// 审计假件自己也要被核对：失败的确实是第 2 行。
	if n := len(res.all()); n != 1 {
		t.Errorf("成功写入的审计行 = %d，期望 1", n)
	}
	// 3）公开结果仍然写得进去——这正是旧实现那句自辩所断言「不可能」的情形。
	if len(res.saved) != 1 {
		t.Errorf("公开结果落盘 %d 次，期望 1——审计与 results.Save 是两条独立的路径，"+
			"「审计坏了公开结果也会坏」这个前提是假的", len(res.saved))
	}
	// 4）后续题目不再执行：只有落盘故障打断整次运行。
	if len(got.Challenges) != 1 {
		t.Errorf("跑了 %d 道题，期望 1——审计不完整之后继续跑只会把缺口拉大", len(got.Challenges))
	}
	if sc.prepared != 1 || sc.cleaned != 1 {
		t.Errorf("Scenario 起题/清理 = %d/%d，期望 1/1（第二题不得被起题）", sc.prepared, sc.cleaned)
	}
	if sc.discovered != 1 {
		t.Errorf("Discover 调用 = %d 次，期望 1", sc.discovered)
	}
}

// TestAuditFailWithoutPortRejectsSubmitRun：`Submit=true` 却没有审计落点 ⇒ 在
// **任何平台调用之前**拒绝整次运行。
//
// 为什么它不是「可选端口缺席就缺席」：Submit=true 意味着一串不可追回的平台写操作。
// 没有审计，事后永远答不出「提交了什么、平台怎么判的」——2026-09-22 那次授权真跑里
// 「147 次提交、146 条判错」的事后追查正是卡在这里。所以拒绝必须发生在 Discover
// 之前（Discover 是 Run 里的第一处平台调用），否则「拒绝」本身已经踩过平台了。
func TestAuditFailWithoutPortRejectsSubmitRun(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{x}"},
	}
	sb := &fakeSandbox{}
	factory := &scriptedAgentFactory{agent: &fakeAgent{}}
	// recordingResults 不是 AuditStore —— 与 Doctor 说「没接审计」是同一个事实。
	h := newAuditHarnessWithResults(t, sc, sb, factory, &recordingResults{})

	got, err := h.Run(context.Background(), testRunSpec()) // Submit=true
	if err == nil {
		t.Fatal("Submit=true 且没有审计落点必须被拒")
	}
	if !IsKind(err, KindConfig) {
		t.Errorf("错误分类 = %v，期望 KindConfig（这是配置错，重试无意义）", err)
	}
	// 它**不是**「审计没写完」那一档：没有任何东西写过，也没有任何东西被写坏。
	if errors.Is(err, ErrAuditIncomplete) {
		t.Error("这是「没接审计」，不是「审计没写完」——两个哨兵不能混成一个")
	}
	if got.State != RunFailed {
		t.Errorf("State = %q，期望 %q", got.State, RunFailed)
	}
	// 副作用的三道闸：平台调用、取锁、沙箱。
	if sc.discovered != 0 {
		t.Errorf("Discover 被调了 %d 次，期望 0——拒绝必须早于任何平台调用", sc.discovered)
	}
	if sc.prepared != 0 || sc.evalCalls != 0 {
		t.Errorf("Prepare/Evaluate = %d/%d，期望 0/0", sc.prepared, sc.evalCalls)
	}
	if sb.sessions != 0 || sb.staleCalls != 0 {
		t.Errorf("Sandbox 被调用：sessions=%d stale=%d，期望均为 0", sb.sessions, sb.staleCalls)
	}
	// 文案必须点名缺的是什么，否则读的人不知道该接哪个端口。
	if !strings.Contains(err.Error(), "AuditStore") {
		t.Errorf("错误文案没有点名 AuditStore: %v", err)
	}
}

// TestAuditAttemptsCountsEveryEvaluateCall：Attempts 是**尝试次数**，不是确认数。
//
// 这两个数回答的是不同的问题：「我们试了多少次」是候选集合与平台额度的问题，
// 「平台认了多少条」是答案质量的问题。合成一个数，一次「候选集合失控」与一次
// 「答案质量差」在公开面上就长得一模一样。
func TestAuditAttemptsCountsEveryEvaluateCall(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{right}"},
	}
	sc.evalHook = func(flag string) Evaluation {
		switch flag {
		case "flag{right}":
			return Evaluation{Accepted: true, Progress: true, Score: 10}
		case "flag{dup}":
			return Evaluation{Accepted: true, Progress: false}
		default:
			return Evaluation{Message: "平台说这个不对"}
		}
	}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call-1", "curl http://t/a", "flag{right}\n")
		emitToolEnd(factory.sink, "call-2", "curl http://t/b", "flag{dup}\n")
		emitToolEnd(factory.sink, "call-3", "curl http://t/c", "flag{wrong}\n")
	}
	h, res := newAuditHarness(t, sc, sb, factory)

	spec := testRunSpec()
	spec.Budget = Budget{MaxRounds: 1, MaxWall: 0, MaxTurns: 0}
	got, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cr := got.Challenges[0]
	if cr.Outcome.Attempts != 3 {
		t.Errorf("Attempts = %d，期望 3（三次提交尝试都调了 Evaluate）", cr.Outcome.Attempts)
	}
	if cr.Outcome.Submitted != 2 {
		t.Errorf("Submitted = %d，期望 2（确认 + 幂等命中）", cr.Outcome.Submitted)
	}
	if cr.Outcome.Attempts == cr.Outcome.Submitted {
		t.Error("两个数相等 ⇒ Attempts 退化成了 Submitted 的别名，这个字段就没有存在的理由")
	}
	// 与审计对账：Attempts 必须等于审计行数（每次尝试一行，迟早会不同就是有人漏记）。
	if n := len(res.all()); n != cr.Outcome.Attempts {
		t.Errorf("审计行数 = %d，Attempts = %d，两者必须一致", n, cr.Outcome.Attempts)
	}
}

// TestAuditAttemptsStaysZeroOnDryRun 是上一条的反面对照：干跑没有平台调用，
// Attempts 必须是 0。没有这条，「Attempts 有赋值点」可能只是「不管怎样都 +1」。
func TestAuditAttemptsStaysZeroOnDryRun(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{right}"},
	}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call-1", "curl http://t/a", "flag{right}\n")
	}
	h, res := newAuditHarness(t, sc, sb, factory)

	spec := testRunSpec()
	spec.Submit = false
	spec.Budget = Budget{MaxRounds: 1, MaxWall: 0, MaxTurns: 0}
	got, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := got.Challenges[0].Outcome.Attempts; n != 0 {
		t.Errorf("干跑的 Attempts = %d，期望 0", n)
	}
	// 干跑也**不该有审计行**：没有提交，就没有可记账的事。
	if n := len(res.all()); n != 0 {
		t.Errorf("干跑写了 %d 行审计，期望 0", n)
	}
}

// ── 提交次数上限 ──

// TestSubmitCapStopsAtLimit 是这一项的**关键**断言：撞上限时平台调用次数
// **恰好等于**上限，一次都不多。
//
// 为什么这条比「Reason 对不对」更重要：上限判定若排在平台调用之后（很自然的写法
// ——先提交再记账），「停下来」的那一刻已经又提交了一次，超出的量取决于候选有多少。
// 那种实现能满足「Reason 是 submit_limit」却完全没起到约束作用，所以必须用调用
// 次数来钉，而不是看 Reason。
func TestSubmitCapStopsAtLimit(t *testing.T) {
	const cap = 3
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{none-of-these}"},
	}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	// 一次产出 10 条候选，远多于上限。
	ag.script = func(_ int, _ func(Event)) {
		for i := 0; i < 10; i++ {
			emitToolEnd(factory.sink, "call", "curl http://t/",
				"flag{candidate-"+string(rune('a'+i))+"}\n")
		}
	}
	h, res := newAuditHarness(t, sc, sb, factory)

	spec := testRunSpec()
	spec.Budget = Budget{MaxRounds: 1, MaxWall: 0, MaxTurns: 0}
	spec.Policy = PolicySpec{MaxSubmissionsPerChallenge: cap}
	got, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sc.evalCalls != cap {
		t.Fatalf("平台提交次数 = %d，期望恰好 %d（上限判定必须排在平台调用之前）", sc.evalCalls, cap)
	}
	cr := got.Challenges[0]
	if cr.Outcome.Reason != ReasonSubmitLimit {
		t.Errorf("Reason = %q，期望 %q", cr.Outcome.Reason, ReasonSubmitLimit)
	}
	if cr.Outcome.SubmissionsCapped == 0 {
		t.Error("撞上限时必须记下还剩多少候选没提交——只有 Reason 分不出「上限定紧了」与「候选失控」")
	}
	// 审计与提交次数对账：三条提交三行审计。
	if n := len(res.all()); n != cap {
		t.Errorf("审计行数 = %d，期望 %d（与提交次数一致）", n, cap)
	}
}

// TestSubmitCapDoesNotTriggerBelowLimit 是反面对照：正常提交不该被上限误伤。
func TestSubmitCapDoesNotTriggerBelowLimit(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{right}"},
	}
	sb := &fakeSandbox{}
	ag := &fakeAgent{}
	factory := &scriptedAgentFactory{agent: ag}
	ag.script = func(_ int, _ func(Event)) {
		emitToolEnd(factory.sink, "call-1", "curl http://t/a", "flag{right}\n")
		emitToolEnd(factory.sink, "call-2", "curl http://t/b", "flag{wrong}\n")
	}
	h, res := newAuditHarness(t, sc, sb, factory)

	spec := testRunSpec()
	spec.Policy = PolicySpec{MaxSubmissionsPerChallenge: 10}
	got, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cr := got.Challenges[0]
	if cr.Outcome.Reason == ReasonSubmitLimit {
		t.Fatal("两条提交不该撞上上限 10")
	}
	if cr.Outcome.SubmissionsCapped != 0 {
		t.Errorf("SubmissionsCapped = %d，期望 0", cr.Outcome.SubmissionsCapped)
	}
	if n := len(res.all()); n != 2 {
		t.Errorf("审计行数 = %d，期望 2", n)
	}
}

// TestSubmitCapDefaultIsRecordedInManifest：上限是「生效值」，必须进清单。
//
// 与 PlannerDryRounds 同一条理由：事后要能回答「这次跑的上限是多少」，
// 否则一次 submit_limit 终止无法与另一档配置的运行比较。
func TestSubmitCapDefaultIsRecordedInManifest(t *testing.T) {
	if got := resolveSubmitCap(RunSpec{}); got != DefaultMaxSubmissionsPerChallenge {
		t.Errorf("默认上限 = %d，期望 %d", got, DefaultMaxSubmissionsPerChallenge)
	}
	if got := resolveManifest(RunSpec{}).MaxSubmissions; got != DefaultMaxSubmissionsPerChallenge {
		t.Errorf("清单记的上限 = %d，期望 %d（清单必须记生效值）", got, DefaultMaxSubmissionsPerChallenge)
	}
	if got := resolveManifest(RunSpec{Policy: PolicySpec{MaxSubmissionsPerChallenge: 7}}).MaxSubmissions; got != 7 {
		t.Errorf("清单应记配置值 7，得到 %d", got)
	}
}
