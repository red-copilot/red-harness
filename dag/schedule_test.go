package dag

import (
	"context"
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

// ── Scheduler 适配层 ──

// 适配层存在的唯一理由是「把图接到根包的轮循环上」，所以端到端跑一遍：
// Next → Activate → 一轮工具事件 → Settle。这条链断了，整条调度就断了。
func TestSchedulerEndToEnd(t *testing.T) {
	g := newTestGraph(t, "10.0.0.1:80")
	s := NewScheduler(g)
	ch := harness.Challenge{Code: "web-01", Category: "pentest", Addrs: []string{"10.0.0.1:80"}}
	ctx := context.Background()

	it, _ := s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})
	if it == nil {
		t.Fatal("首轮必须给出意图")
	}
	if it.ID == "" || it.Kind == "" || it.Goal == "" {
		t.Fatalf("IntentRef 字段不全: %+v", it)
	}
	if it.Kind != string(IntentRecon) {
		t.Errorf("pentest 首轮应是 recon, got %s", it.Kind)
	}
	if it.Round != 0 {
		t.Errorf("首轮意图的轮次应为 0, got %d", it.Round)
	}
	s.Activate(it)
	if n := g.Node(it.ID); n.State != IntentActive || n.Attempts != 1 {
		t.Fatalf("Activate 后应是 active/attempts=1, got %s/%d", n.State, n.Attempts)
	}

	// 一轮里 agent 跑了 nmap，宿主抽出事实
	res := s.Ingest(toolEnd("bash", "nmap -sV 10.0.0.1",
		"80/tcp open http nginx/1.18.0\n10.0.0.1:8080 open\n"), 1)
	if len(res.Added) == 0 {
		t.Fatalf("应抽到事实, got %+v", res)
	}
	// 同一份输出再来一次 ⇒ 全是重复，不产生新节点
	res2 := s.Ingest(toolEnd("bash", "nmap -sV 10.0.0.1",
		"80/tcp open http nginx/1.18.0\n10.0.0.1:8080 open\n"), 1)
	if len(res2.Added) != 0 || len(res2.Duplicates) == 0 {
		t.Errorf("重复输出应全部判重, got %+v", res2)
	}

	s.Settle(it, harness.RoundResult{Text: "我成功了"})
	if n := g.Node(it.ID); n.State != IntentDone {
		t.Errorf("本轮有新事实 ⇒ 意图应 done, got %s", n.State)
	}
	// 下一条意图：recon 做完后按阶段序应是 exploit
	next, _ := s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})
	if next == nil {
		t.Fatal("还有阶段没做完，应给出下一条意图")
	}
	if next.Kind != string(IntentExploit) {
		t.Errorf("recon 之后应是 exploit, got %s", next.Kind)
	}
	if next.ID == it.ID {
		t.Error("不该重复给出已完成的意图")
	}
}

// 铺链的时机与判据：只在图上一条意图都没有时铺，且**不**重复铺。
// 前身失败模式：每次拼 prompt 都重新铺一遍目标链，于是永远停在第 1 阶段。
func TestSchedulerSeedChainOnce(t *testing.T) {
	g := newTestGraph(t)
	s := NewScheduler(g)
	ch := harness.Challenge{Category: "pentest"}
	ctx := context.Background()

	first, _ := s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})
	if first == nil {
		t.Fatal("应铺链并给出首条意图")
	}
	before := len(g.Facts(""))
	intentsAfterFirst := 0
	for _, n := range g.Nodes() {
		if n.IsIntent() {
			intentsAfterFirst++
		}
	}
	if intentsAfterFirst != len(PhaseChain("pentest")) {
		t.Fatalf("应铺满整条阶段链 %d 条, got %d",
			len(PhaseChain("pentest")), intentsAfterFirst)
	}
	// 再调几次 Next：不得再铺一条链
	for i := 0; i < 3; i++ {
		_, _ = s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})
	}
	count := 0
	for _, n := range g.Nodes() {
		if n.IsIntent() {
			count++
		}
	}
	if count != intentsAfterFirst {
		t.Errorf("重复调用 Next 不得重复铺链: %d → %d", intentsAfterFirst, count)
	}
	if len(g.Facts("")) != before {
		t.Errorf("Next 不该改事实集: %d → %d", before, len(g.Facts("")))
	}

	// 续跑场景：载入一张已有意图的图，Next 不得再铺链
	g2 := newTestGraph(t)
	mustIntent(t, g2, IntentAnalyze, "读源码")
	s2 := NewScheduler(g2)
	_, _ = s2.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})
	n2 := 0
	for _, n := range g2.Nodes() {
		if n.IsIntent() {
			n2++
		}
	}
	if n2 != 1 {
		t.Errorf("已有意图的图不得重新铺链, got %d 条意图", n2)
	}
}

// 各阶段链的形状逐条对齐前身 `goals_for_category()`。
func TestPhaseChainByCategory(t *testing.T) {
	cases := map[string][]IntentKind{
		"pentest": {IntentRecon, IntentExploit, IntentFoothold, IntentExtract, IntentLateral, IntentEscalate, IntentVerify},
		// `web` 不在前身表里，走默认链（没有 credential/lateral 阶段）
		"web":       defaultChain,
		"crypto":    {IntentAnalyze, IntentExploit, IntentVerify},
		"misc":      {IntentAnalyze, IntentExploit, IntentVerify},
		"forensics": {IntentAnalyze, IntentExploit, IntentVerify},
		"reverse":   {IntentAnalyze, IntentExploit, IntentVerify},
		// `pwn` 不在前身表里，走默认链
		"pwn":         defaultChain,
		"unknown-cat": defaultChain,
		"":            defaultChain,
	}
	for cat, want := range cases {
		got := PhaseChain(cat)
		if len(got) != len(want) {
			t.Errorf("类别 %q 的阶段链长度 %d, want %d (%v)", cat, len(got), len(want), got)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("类别 %q 第 %d 阶段 %s, want %s", cat, i, got[i], want[i])
			}
		}
	}
	// 大小写与空白不敏感（平台的 category 字段写法不统一）
	if PhaseChain(" Pentest ") == nil {
		t.Error("类别应做归一后再查表")
	}
	if len(PhaseChain(" PENTEST ")) != len(PhaseChain("pentest")) {
		t.Error("类别归一应大小写不敏感")
	}
	// 链上的每个阶段都必须有可执行的措辞（空目标会被 AddIntent 拒绝）
	for _, k := range PhaseChain("pentest") {
		if strings.TrimSpace(PhaseGoal("pentest", k)) == "" {
			t.Errorf("阶段 %s 缺目标措辞", k)
		}
	}
}

// Settle 的产出判定必须**客观**：只看本轮新入图的事实，不看 RoundResult.Text。
// 前身的原则：只用客观事实推进阶段，绝不把 agent 的自我汇报写成事实。
func TestSchedulerSettleIgnoresSelfReport(t *testing.T) {
	g := newTestGraph(t)
	s := NewScheduler(g)
	it, _ := s.Next(context.Background(), harness.PlannerInput{Challenge: harness.Challenge{Category: "pentest"}, Outcome: harness.OutcomeView{}})
	s.Activate(it)
	// agent 说自己成功了，但一条新事实都没有
	s.Settle(it, harness.RoundResult{Text: "我已经拿到了 flag，成功了！"})
	if n := g.Node(it.ID); n.State == IntentDone {
		t.Error("没有客观新事实时不得判 done（agent 的自述不是成功）")
	}
	if n := g.Node(it.ID); n.Attempts != 1 {
		t.Errorf("失败也应记一次尝试, got %d", n.Attempts)
	}
	// 下一条意图仍应是同一阶段（失败可重试），而不是跳到下一阶段
	next, _ := s.Next(context.Background(), harness.PlannerInput{Challenge: harness.Challenge{Category: "pentest"}, Outcome: harness.OutcomeView{}})
	if next == nil || next.ID != it.ID {
		t.Errorf("失败后应重试同一意图, got %+v", next)
	}
}

// 试够上限后必须放弃并往下走——否则 agent 会在一个死方向上烧光预算。
func TestSchedulerAbandonsAfterCap(t *testing.T) {
	g := newTestGraph(t)
	s := NewScheduler(g)
	ctx := context.Background()
	ch := harness.Challenge{Category: "pentest"}
	it, _ := s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})
	for i := 0; i < DefaultMaxAttempts; i++ {
		s.Activate(it)
		s.Settle(it, harness.RoundResult{})
	}
	if n := g.Node(it.ID); n.State != IntentAbandoned {
		t.Fatalf("试够 %d 次后应 abandoned, got %s", DefaultMaxAttempts, n.State)
	}
	next, _ := s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})
	if next == nil {
		t.Fatal("放弃一个方向后应给出下一个方向")
	}
	if next.ID == it.ID {
		t.Error("abandoned 的意图不得再被挑出来")
	}
}

// Activate 时记的水位线是产出判定的基准：激活**之前**就存在的事实不算本轮产出。
func TestSchedulerProducedUsesWatermark(t *testing.T) {
	g := newTestGraph(t, "10.0.0.1:80")
	s := NewScheduler(g)
	ctx := context.Background()
	ch := harness.Challenge{Category: "pentest"}

	it, _ := s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})
	s.Activate(it)
	// 激活后新入图的事实
	newID, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactService,
		Content: "nginx/1.18.0", Source: "bash: whatweb"})
	if err != nil {
		t.Fatal(err)
	}
	s.Settle(it, harness.RoundResult{})
	// 产出的判定在 Settle 内部完成；这里用图的 produces 边核对
	got := g.Produced(it.ID)
	found := false
	for _, id := range got {
		if id == newID {
			found = true
		}
	}
	if !found {
		t.Errorf("本轮新事实应记为产出, got %v (新事实 %s)", got, newID)
	}
	// 激活前就有的目标事实不该被记为产出
	for _, id := range got {
		if n := g.Node(id); n != nil && n.FactKind == FactTarget {
			t.Errorf("激活前已有的事实不该算本轮产出: %s", id)
		}
	}
	if g.Node(it.ID).State != IntentDone {
		t.Errorf("有客观产出应判 done, got %s", g.Node(it.ID).State)
	}
}

// agent 申报的 negative 由宿主归属到当前活动意图（agent 不知道意图 id）。
func TestSchedulerIngestRoutesNegative(t *testing.T) {
	g := newTestGraph(t)
	s := NewScheduler(g)
	it, _ := s.Next(context.Background(), harness.PlannerInput{Challenge: harness.Challenge{Category: "pentest"}, Outcome: harness.OutcomeView{}})
	s.Activate(it)

	ev := reportEvent(map[string]any{
		"kind": "negative", "content": "8080 端口连不上，connection refused",
		"evidence": "nmap -p8080 10.0.0.1 超时",
	})
	res := s.Ingest(ev, 1)
	if len(res.Negatives) != 1 {
		t.Fatalf("negative 申报应转成图上的死胡同, got %+v", res)
	}
	n := g.Node(res.Negatives[0])
	if n == nil || n.FactKind != FactNegative || n.Refutes != it.ID {
		t.Fatalf("死胡同应指向活动意图 %s, got %+v", it.ID, n)
	}
	if n.Source == "" || !strings.HasPrefix(n.Source, "report_fact") {
		t.Errorf("死胡同的来源应标 report_fact, got %q", n.Source)
	}
	// 没有活动意图时，申报的 negative **不能**入图（没有 refutes 目标）
	s2 := NewScheduler(newTestGraph(t))
	res2 := s2.Ingest(ev, 1)
	if len(res2.Negatives) != 0 {
		t.Errorf("无活动意图时 negative 不该入图, got %+v", res2)
	}
	// 非 negative 的申报不走这条通道
	res3 := s.Ingest(reportEvent(map[string]any{
		"kind": "service", "content": "nginx/1.18.0"}), 1)
	if len(res3.Negatives) != 0 || len(res3.Added) != 1 {
		t.Errorf("普通申报应走事实通道, got %+v", res3)
	}
}

// agent 的 `next` 建议（agent 说「下一步该做什么」）必须被转成一条意图候选，
// 并挂**派生它的那条事实**上（enables 边）。
//
// 这条测试钉的是一个真实的缺口：`reportPayload.Next` 原本没有任何调用点——
// 设计文档 §一 写着「next 作为 EdgeEnables 派生的意图候选」，宿主扩展的参数描述
// 也写着「框架会作为意图候选」，但没有任何代码读它。于是 agent 最有价值的两条
// 信号（「我知道什么」+「我建议下一步做什么」）里，后一条被静默忽略。
func TestSchedulerIngestEnablesFromNext(t *testing.T) {
	g := newTestGraph(t)
	s := NewScheduler(g)
	it, _ := s.Next(context.Background(), harness.PlannerInput{Challenge: harness.Challenge{Category: "pentest"}, Outcome: harness.OutcomeView{}})
	s.Activate(it)

	ev := reportEvent(map[string]any{
		"kind": "service", "content": "apache/2.4.49",
	})
	ev.Details["report_fact"].(map[string]any)["next"] = "试试 /cgi-bin 的路径穿越"
	res := s.Ingest(ev, 1)
	if len(res.Added) != 1 {
		t.Fatalf("前置条件：本轮应产出一条事实, got %+v", res)
	}
	if len(res.Enabled) != 1 {
		t.Fatalf("agent 的 next 建议应派生出 1 条意图候选, got %+v", res)
	}
	cand := g.Node(res.Enabled[0])
	if cand == nil || !cand.IsIntent() {
		t.Fatalf("派生的应是意图节点, got %+v", cand)
	}
	if cand.Goal != "试试 /cgi-bin 的路径穿越" {
		t.Errorf("候选目标应是 agent 的建议原文, got %q", cand.Goal)
	}
	// enables 边的方向是 fact → intent，来源必须是**本轮产出的那条事实**
	from := g.EnabledFrom(cand.ID)
	if len(from) != 1 || from[0] != res.Added[0] {
		t.Errorf("候选应挂在本轮产出的事实 %v 上, got %v", res.Added, from)
	}
	// 候选只是一个普通意图：它和阶段链上的意图一起排队，不保证被选中
	// （agent 建议不等于调度）。这里断言它至少是 pending 且在前沿里可见。
	if cand.State != IntentPending {
		t.Errorf("候选应是 pending, got %s", cand.State)
	}
	found := false
	for _, n := range g.Frontier() {
		if n.ID == cand.ID {
			found = true
		}
	}
	if !found {
		t.Error("候选意图应在前沿里（能不能被选中由优先级决定，但必须参与排队）")
	}
	// 图必须仍然是干净的（enables 边的端点类型错了 Validate 会报）
	if errs := g.Validate(); len(errs) != 0 {
		t.Errorf("派生候选后图不该有违规: %v", errs)
	}

	// 空 next ⇒ 不造空意图（空目标会被 AddIntent 拒，且它会白占一个前沿名额）
	g2 := newTestGraph(t)
	s2 := NewScheduler(g2)
	it2, _ := s2.Next(context.Background(), harness.PlannerInput{Challenge: harness.Challenge{Category: "pentest"}, Outcome: harness.OutcomeView{}})
	s2.Activate(it2)
	ev2 := reportEvent(map[string]any{"kind": "service", "content": "nginx/1.18.0"})
	ev2.Details["report_fact"].(map[string]any)["next"] = "   "
	res2 := s2.Ingest(ev2, 1)
	if len(res2.Enabled) != 0 {
		t.Errorf("空 next 不该派生意图, got %+v", res2)
	}
	for _, n := range g2.Nodes() {
		if n.IsIntent() && strings.TrimSpace(n.Goal) == "" {
			t.Errorf("图里出现了空目标意图: %+v", n)
		}
	}

	// 本轮没有新事实（纯申报调用、申报内容也全是重复）⇒ 不派生：悬空/乱挂的
	// enables 边比丢掉一条建议更糟（它会给出看起来合理的错误 provenance）。
	g3 := newTestGraph(t, "10.0.0.1:80")
	s3 := NewScheduler(g3)
	it3, _ := s3.Next(context.Background(), harness.PlannerInput{Challenge: harness.Challenge{Category: "pentest"}, Outcome: harness.OutcomeView{}})
	s3.Activate(it3)
	ev3 := reportEvent(map[string]any{"kind": "target", "content": "10.0.0.1:80"}) // 与平台种子重复
	ev3.Details["report_fact"].(map[string]any)["next"] = "试试 8443 端口"
	res3 := s3.Ingest(ev3, 1)
	if len(res3.Added) != 0 {
		t.Fatalf("前置条件：这条申报应与平台种子判重, got %+v", res3)
	}
	if len(res3.Enabled) != 0 {
		t.Errorf("本轮没有新事实时不该派生候选（避免乱挂 enables 边）, got %+v", res3)
	}

	// 重复的建议不重复回报（agent 在每个 report_fact 里都带 next 是常态）
	res4 := s.Ingest(ev, 2)
	if len(res4.Enabled) != 0 {
		t.Errorf("同一句建议第二次出现不该再回报一次候选, got %+v", res4)
	}
	// 但 enables 边仍在（首现优先：派生关系不因为重复发现而丢）
	if got := g.EnabledFrom(cand.ID); len(got) != 1 {
		t.Errorf("重复建议后 enables 边仍应在, got %v", got)
	}
}

// 命中答案形状的内容被图拒收时，Ingest 要把内容**原样回报**给调用方——
// 那正是 gate 要的候选，图不替 gate 记账，但必须让调用方看见。
func TestSchedulerIngestReportsAnswerShaped(t *testing.T) {
	g := newTestGraph(t)
	s := NewScheduler(g)
	ev := reportEvent(map[string]any{
		"kind": "artifact", "content": "flag{smuggled_via_report}", "confidence": 1.0,
	})
	res := s.Ingest(ev, 1)
	if len(res.Added) != 0 {
		t.Fatalf("答案形状的内容不得入图, got %+v", res)
	}
	// 抽取器在入库前就把它丢了，所以 AnswerShaped 不一定非有；但图里绝不能有它
	for _, f := range g.Facts("") {
		if strings.Contains(f.Content, "smuggled") {
			t.Errorf("答案形状内容入图了: %+v", f)
		}
	}
	// 直接走 AddFact 的路径必须被明确分类为 AnswerShaped
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: "flag{smuggled_via_report}", Source: "bash: cat f"}); !isAnswerShaped(err) {
		t.Errorf("答案形状应被分类为 AnswerShaped, got %v", err)
	}
}

// 接口没有 error 返回值，所以内部错误必须落在 LastErr 上（静默吞掉会让
// 「意图状态没更新」无从发现）。
func TestSchedulerRecordsLastErr(t *testing.T) {
	g := newTestGraph(t)
	s := NewScheduler(g)
	// 一个不存在的意图：Activate 必然失败
	s.Activate(&harness.IntentRef{ID: "i999", Kind: string(IntentRecon), Goal: "x"})
	if s.LastErr == nil {
		t.Error("Activate 失败必须记在 LastErr 上")
	}
	if err := s.G.Settle("i999", nil, harness.RoundResult{}); err == nil {
		t.Fatal("对不存在的意图 Settle 应报错（前置条件）")
	}
	s.LastErr = nil
	s.Settle(&harness.IntentRef{ID: "i999"}, harness.RoundResult{})
	if s.LastErr == nil {
		t.Error("Settle 失败必须记在 LastErr 上")
	}
	// nil 意图是合法的 no-op（轮循环在收尾时会这么调），不该产生错误
	s.LastErr = nil
	s.Activate(nil)
	s.Settle(nil, harness.RoundResult{})
	if s.LastErr != nil {
		t.Errorf("nil 意图应是 no-op, got %v", s.LastErr)
	}
}

// 前沿耗尽时 Next 返回 nil（轮循环据此收尾）。
func TestSchedulerNextNilWhenExhausted(t *testing.T) {
	g := newTestGraph(t)
	s := NewScheduler(g)
	ctx := context.Background()
	ch := harness.Challenge{Category: "pentest"}
	for i := 0; i < 50; i++ {
		it, _ := s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})
		if it == nil {
			return // 正常收尾
		}
		s.Activate(it)
		// 每轮都产出新事实，让阶段能推进
		if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
			Content: "/path/" + itoa(i) + ".txt", Source: "bash: x"}); err != nil {
			t.Fatal(err)
		}
		s.Settle(it, harness.RoundResult{})
	}
	t.Fatal("阶段链有限，Next 最终应返回 nil")
}

// ── Renderer 适配层 ──

// Renderer 是 Render 的薄适配层：从 harness 的类型里取输入，逻辑全在 Render 里。
func TestRendererAdapter(t *testing.T) {
	g := newTestGraph(t, "10.0.0.1:80")
	s := NewScheduler(g)
	ctx := context.Background()
	ch := harness.Challenge{Code: "web-01", Category: "pentest", FlagCount: 2,
		Description: "拿到 flag 后提交", Addrs: []string{"10.0.0.1:80"}}
	it, _ := s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})

	r := &Renderer{G: g}
	out := r.Render(ctx, ch, it, &harness.OutcomeView{})
	if !strings.Contains(out, "## 本题") || !strings.Contains(out, it.Goal) {
		t.Errorf("渲染结果应含题面与本轮意图:\n%s", out)
	}
	// 意图为 nil（收尾轮）不该 panic
	if got := r.Render(ctx, ch, nil, nil); !strings.Contains(got, "前沿已空") {
		t.Errorf("意图为 nil 时应渲染收口段:\n%s", got)
	}
	// outcome 为 nil 不该 panic
	if got := r.Render(ctx, ch, it, nil); got == "" {
		t.Error("outcome 为 nil 也应渲染")
	}
}

// 已确认数来自 Outcome.Flags；判错指纹来自 Candidates 里 Reject 非空的项，
// 且**必须去重**（同一答案可能被多个候选记为同值）。
func TestRendererInjectsOutcome(t *testing.T) {
	g := newTestGraph(t, "10.0.0.1:80")
	s := NewScheduler(g)
	ctx := context.Background()
	ch := harness.Challenge{Code: "web-01", Category: "pentest", FlagCount: 3,
		Description: "提交 flag{...}", Addrs: []string{"10.0.0.1:80"}}
	it, _ := s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})

	r := &Renderer{G: g}
	out := r.Render(ctx, ch, it, &harness.OutcomeView{
		Flags: []string{"flag{a}", "flag{b}"},
		Candidates: []harness.Candidate{
			{Flag: "flag{wrong1}", SubmitError: "平台判错"},
			{Flag: "flag{wrong1}", SubmitError: "平台判错"}, // 同值重复
			{Flag: "flag{ok}", Correct: true},           // 没被拒：不回灌
		},
	})
	if !strings.Contains(out, "flag 2/3 已确认") {
		t.Errorf("已确认数应从 Outcome.Flags 取:\n%s", out)
	}
	if strings.Contains(out, "flag{wrong1}") {
		t.Fatalf("判错回灌不得含明文:\n%s", out)
	}
	fp := FlagFingerprint("flag{wrong1}")
	if strings.Count(out, fp) != 1 {
		t.Errorf("判错指纹应去重后出现一次, got %d:\n%s", strings.Count(out, fp), out)
	}
}

// 回灌的判据必须是 SubmitError（平台说不对），**不是** RejectReason（gate 觉得
// 可疑）。混用会造成一个具体的伤害：gate 把某候选标成 `agent_authored` 只是因为
// 命令里出现过它，而那个答案可能完全正确——把它当「已试过、错的」回灌进 prompt，
// 会让 agent **主动避开正确答案**。
//
// 这条测试是回归闸：Candidate 的字段一旦再合并回一个 Reject，它会立刻失败。
func TestRendererDoesNotFeedBackGateSuspects(t *testing.T) {
	g := newTestGraph(t, "10.0.0.1:80")
	s := NewScheduler(g)
	ctx := context.Background()
	ch := harness.Challenge{Code: "web-01", Category: "pentest",
		Description: "提交 flag{...}", Addrs: []string{"10.0.0.1:80"}}
	it, _ := s.Next(ctx, harness.PlannerInput{Challenge: ch, Outcome: harness.OutcomeView{}})

	r := &Renderer{G: g}
	out := r.Render(ctx, ch, it, &harness.OutcomeView{
		Candidates: []harness.Candidate{
			// gate 的族别归因：可疑，但平台从没说过它是错的
			{Flag: "flag{suspect_but_maybe_right}", RejectReason: "agent_authored"},
		},
	})
	if strings.Contains(out, FlagFingerprint("flag{suspect_but_maybe_right}")) {
		t.Fatalf("只是 gate 觉得可疑的候选不得回灌（会让 agent 避开正确答案）:\n%s", out)
	}
	if strings.Contains(out, "不要再提交") {
		t.Errorf("没有平台判错的候选时不该出现判错段:\n%s", out)
	}
	// 平台判错的才回灌
	out2 := r.Render(ctx, ch, it, &harness.OutcomeView{
		Candidates: []harness.Candidate{
			{Flag: "flag{really_wrong}", SubmitError: "平台判错"},
		},
	})
	if !strings.Contains(out2, FlagFingerprint("flag{really_wrong}")) {
		t.Errorf("平台判错的候选必须回灌:\n%s", out2)
	}
}

// 渲染器可配置段长上限（透传到 RenderInput）。
func TestRendererLimits(t *testing.T) {
	g := newTestGraph(t, "10.0.0.1:80")
	for i := 0; i < 10; i++ {
		if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
			Content: "/p/" + itoa(i) + ".txt", Source: "bash: x"}); err != nil {
			t.Fatal(err)
		}
	}
	r := &Renderer{G: g, MaxFacts: 2}
	out := r.Render(context.Background(), harness.Challenge{Code: "c"}, nil, nil)
	lines := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "- [") {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("MaxFacts=2 应只列 2 条, got %d\n%s", lines, out)
	}
}

// 便利方法：Require / EnableFrom / DeriveFrom / Supersede 的语义。
func TestGraphConvenienceMethods(t *testing.T) {
	g := newTestGraph(t)
	i := mustIntent(t, g, IntentEscalate, "提权")
	f := mustFact(t, g, FactFoothold, "www-data shell", "bash: curl")

	// Require 声明真实前置；前置未满足时该意图不可执行
	if err := g.Require(i, f); err != nil {
		t.Fatalf("Require 失败: %v", err)
	}
	if got := g.Requires(i); len(got) != 1 || got[0] != f {
		t.Errorf("Requires 应返回前置事实: %v", got)
	}
	// 前置是 foothold ⇒ 该意图可执行（事实已存在）
	found := false
	for _, n := range g.Frontier() {
		if n.ID == i {
			found = true
		}
	}
	if !found {
		t.Error("前置已满足的意图应在前沿里")
	}

	// Require 指向不存在的事实：允许（前置可能还没被发现），但意图不可执行
	i2 := mustIntent(t, g, IntentLateral, "横向")
	if err := g.Require(i2, "f999"); err != nil {
		t.Fatalf("悬挂的 requires 应被允许（前置可能尚未发现）: %v", err)
	}
	for _, n := range g.Frontier() {
		if n.ID == i2 {
			t.Error("前置缺失的意图不该在前沿里")
		}
	}

	// EnableFrom：由一个事实派生出新意图（分支点）
	bid, err := g.EnableFrom(f, Node{Kind: NodeIntent, IntentKind: IntentExtract,
		Goal: "从 shell 里读 flag"})
	if err != nil {
		t.Fatalf("EnableFrom 失败: %v", err)
	}
	if got := g.EnabledFrom(bid); len(got) != 1 || got[0] != f {
		t.Errorf("enables 边应记录来源事实: %v", got)
	}
	// 重复的意图也要把 enables 边连上（首现优先，但派生关系不丢）
	bid2, err := g.EnableFrom(f, Node{Kind: NodeIntent, IntentKind: IntentExtract,
		Goal: "从 shell 里读 flag"})
	if !isDuplicate(err) {
		t.Errorf("重复意图应报 ErrDuplicate, got %v", err)
	}
	if bid2 != bid {
		t.Errorf("重复意图应返回已有 id: %s vs %s", bid, bid2)
	}

	// DeriveFrom：推导链
	inferred, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: "webroot 在 /var/www/html", Source: "host: rule", Trust: TrustInferred})
	if err != nil {
		t.Fatalf("inferred 事实应能入图: %v", err)
	}
	if err := g.DeriveFrom(inferred, f); err != nil {
		t.Fatalf("DeriveFrom 失败: %v", err)
	}
	if got := g.DerivedFrom(inferred); len(got) != 1 || got[0] != f {
		t.Errorf("derived_from 边应记录来源事实: %v", got)
	}

	// Supersede：新意图取代旧意图（旧的被标 abandoned）
	old := mustIntent(t, g, IntentExploit, "试 /admin 弱口令")
	newer := mustIntent(t, g, IntentExploit, "试 /api 的 JWT 伪造")
	if err := g.Supersede(old, newer); err != nil {
		t.Fatalf("Supersede 失败: %v", err)
	}
	if n := g.Node(old); n.State != IntentAbandoned {
		t.Errorf("被取代的意图应 abandoned, got %s", n.State)
	}
	if n := g.Node(newer); n.State == IntentAbandoned {
		t.Error("取代者不该被放弃")
	}
	// 端点类型错误必须报错（supersedes 是 intent → intent）
	if err := g.Supersede(f, newer); err == nil {
		t.Error("supersedes 的端点类型不合法应报错")
	}
}
