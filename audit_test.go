package harness

import (
	"context"
	"errors"
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

// newAuditHarness 与 newTestHarness 同形，但 Results 换成可审计的假件。
func newAuditHarness(t *testing.T, sc Scenario, sb Sandbox, af AgentFactory) (*Harness, *auditResults) {
	t.Helper()
	res := &auditResults{}
	h, err := NewHarness(HarnessOptions{Scenario: sc, Sandbox: sb, Agents: af,
		Gate:     func(Challenge) CandidateGate { return newStubGate() },
		Results:  res,
		Locker:   fakeRunLocker{},
		Planner:  func(Challenge) Planner { return &stubPlanner{} },
		Renderer: func(Challenge) Renderer { return stubRenderer{} }})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	return h, res
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

// TestAuditIsOptionalPort：不接审计端口时运行照常，**不得**失败。
//
// 它与 GraphSaver 同档：可选端口。但「没接」必须可见——Doctor 有一行
// candidate_audit，本用例把那一行也钉住。
func TestAuditIsOptionalPort(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{x}"},
	}
	sb := &fakeSandbox{}
	// newTestHarness 的 Results 是 recordingResults —— **不**实现 AuditStore。
	h := newTestHarness(t, sc, sb, &scriptedAgentFactory{agent: &fakeAgent{}},
		func(Challenge) CandidateGate { return newStubGate() })
	if _, err := h.Run(context.Background(), testRunSpec()); err != nil {
		t.Fatalf("不接审计端口不该让运行失败: %v", err)
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
