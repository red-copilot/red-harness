package dag

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// fixedClock 让 golden / 确定性测试不依赖真实时间。
func fixedClock() func() time.Time {
	t := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func newTestGraph(t *testing.T, addrs ...string) *Graph {
	t.Helper()
	g := New(harness.Challenge{
		Code: "web-01", Category: "pentest", Difficulty: "medium",
		FlagCount: 2, Description: "拿到 flag 后提交，格式 flag{...}",
		Addrs: addrs,
	})
	g.Now = fixedClock()
	return g
}

func mustFact(t *testing.T, g *Graph, kind FactKind, content, source string) string {
	t.Helper()
	n := Node{Kind: NodeFact, FactKind: kind, Content: content, Source: source}
	if kind == FactVuln || kind == FactFoothold {
		n.ToolCallID = "call_x"
	}
	id, err := g.AddFact(n)
	if err != nil {
		t.Fatalf("AddFact(%s, %q) 失败: %v", kind, content, err)
	}
	return id
}

func mustIntent(t *testing.T, g *Graph, kind IntentKind, goal string) string {
	t.Helper()
	id, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: kind, Goal: goal})
	if err != nil {
		t.Fatalf("AddIntent(%s, %q) 失败: %v", kind, goal, err)
	}
	return id
}

// ── 不变量 1：事实必须有来源 ──

func TestInvariantSourceRequired(t *testing.T) {
	g := newTestGraph(t)
	_, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactService, Content: "nginx"})
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("无来源事实必须被拒，got %v", err)
	}
	if len(g.Facts("")) != 0 {
		t.Error("被拒的事实不得入库")
	}
	if len(g.Rejections()) != 1 {
		t.Errorf("被拒写入必须留审计记录，got %d 条", len(g.Rejections()))
	}
}

// 唯一例外：题目初始化时由平台题面生成的目标事实。
func TestSeedTargetHasPlatformSource(t *testing.T) {
	g := newTestGraph(t, "10.0.0.1:80", "10.0.0.1:8080")
	targets := g.Facts(FactTarget)
	if len(targets) != 2 {
		t.Fatalf("want 2 target facts, got %d", len(targets))
	}
	for _, f := range targets {
		if f.Source != SourcePlatform {
			t.Errorf("题目初始化的目标事实来源应为 platform, got %q", f.Source)
		}
	}
	if g.Rejections() != nil && len(g.Rejections()) != 0 {
		t.Errorf("初始化不该产生被拒记录: %v", g.Rejections())
	}
}

// ── 不变量 2：flag 候选永不入事实库 ──

// 这条测试对应设计文档里最重要的机制：前身事故是 agent 写
// `echo 'flag{x}' > /tmp/f` 再 `cat /tmp/f`，于是自己的猜测被自己读回来，
// 洗成了「观测」。宿主在入库口拒收 ⇒ 那条洗白路径从根上不存在。
func TestInvariantAnswerShapedRejected(t *testing.T) {
	g := newTestGraph(t)
	// agent 自造的 flag（无论走哪条通道）都必须被拒
	_, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: "flag{i_made_this_up}", Source: "bash: cat /tmp/f"})
	if !errors.Is(err, ErrAnswerShaped) {
		t.Fatalf("命中答案形状的内容必须被拒，got %v", err)
	}
	if len(g.Facts("")) != 0 {
		t.Error("被拒内容不得入库")
	}

	// 信封形态在任何事实类别下都拒
	for _, k := range []FactKind{FactTarget, FactService, FactArtifact, FactCredential, FactVuln, FactFoothold} {
		n := Node{Kind: NodeFact, FactKind: k, Content: "flag{abc123}",
			Source: "bash: cat f", ToolCallID: "call_1"}
		if _, err := g.AddFact(n); !errors.Is(err, ErrAnswerShaped) {
			t.Errorf("类别 %s 下的 flag 形状内容必须被拒，got %v", k, err)
		}
	}
}

// AddIntent 的目标里带答案形状：**意图保留，明文被净化成指纹**。
//
// 这条测试钉的是一个具体的泄漏面：`report_fact` 载荷的 `next` 字段设计上要经
// EnableFrom 变成意图，而意图的 Goal 会被渲染进下一轮 prompt（render.go 的
// writeIntent）。原实现「不判答案形状」的注释只说对了一半——整条拒掉会丢掉
// agent 有价值的猜测，但**原样存下**等于让 flag 明文绕开事实库的答案形状闸、
// 直接出现在 prompt / transcript / 报告 / 落盘文件里（前身 B52 的同款事故）。
//
// 所以分寸是：意图接受，目标净化。三条断言缺一不可。
func TestInvariantIntentGoalSanitizedNotRejected(t *testing.T) {
	g := newTestGraph(t)
	const secret = "flag{goal_carries_answer}"
	id, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentVerify,
		Goal: "验证 " + secret + " 是否正确并提交"})
	if err != nil {
		t.Fatalf("带答案形状目标的意图必须被接受（猜测是合法意图）: %v", err)
	}
	n := g.Node(id)
	if n == nil || n.IntentKind != IntentVerify {
		t.Fatalf("意图应完整保留: %+v", n)
	}
	if strings.Contains(n.Goal, secret) || strings.Contains(n.Goal, "goal_carries_answer") {
		t.Fatalf("存下的目标里不得有明文: %q", n.Goal)
	}
	fp := FlagFingerprint(secret)
	if !strings.Contains(n.Goal, fp) {
		t.Errorf("明文应被替换为指纹 %s，got %q", fp, n.Goal)
	}
	// 净化后的目标仍必须可读——只剩一个指纹的意图对 agent 毫无意义，而
	// 「验证 <指纹> 是否正确并提交」正好表达「复核这个候选」这件事本身。
	if !strings.Contains(n.Goal, "验证") {
		t.Errorf("净化不该把目标切成碎片: %q", n.Goal)
	}
	// 渲染出的 prompt 里也不能有明文
	out := Render(g, RenderInput{
		Challenge: harness.Challenge{Code: "web-01", Description: "拿到 flag 后提交，格式 flag{...}"},
		Intent:    n,
	})
	if strings.Contains(out, secret) || strings.Contains(out, "goal_carries_answer") {
		t.Fatalf("渲染结果里出现了意图目标中的明文:\n%s", out)
	}
	if !strings.Contains(out, fp) {
		t.Errorf("渲染结果里应能看到指纹（否则 agent 无从判断这是哪个候选）:\n%s", out)
	}
	// 落盘同样不能有（净化发生在入库口，落盘口只是最后一道防线）
	dir := t.TempDir()
	path := filepath.Join(dir, "dag.json")
	if err := g.Save(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "goal_carries_answer") {
		t.Fatalf("明文不得落盘:\n%s", string(b))
	}

	// ── 第二个入口：agent 的 `next` 建议 ──
	//
	// 这条路径必须单独测，因为它才是「明文绕开事实库闸门」的真实来路：`next` 是
	// 模型自由文本，里面的 flag 明文既不是事实（不经过 AddFact 的答案形状闸），
	// 又会被 Ingest 经 EnableFrom 变成意图、再渲染进下一轮 prompt。
	// 只测 AddIntent 会漏掉「report_fact → next → EnableFrom」这条链。
	g2 := newTestGraph(t, "10.0.0.1:80")
	s := NewScheduler(g2)
	it := s.Next(context.Background(), harness.Challenge{Category: "pentest"}, &harness.Outcome{})
	s.Activate(it)
	ev := reportEvent(map[string]any{"kind": "service", "content": "nginx/1.18.0"})
	ev.Details["report_fact"].(map[string]any)["next"] = "提交 " + secret + " 试试"
	res := s.Ingest(ev, 1)
	if len(res.Enabled) != 1 {
		t.Fatalf("next 建议应派生出候选意图, got %+v", res)
	}
	cand := g2.Node(res.Enabled[0])
	if cand == nil {
		t.Fatalf("候选意图不存在: %s", res.Enabled[0])
	}
	if strings.Contains(cand.Goal, secret) || strings.Contains(cand.Goal, "goal_carries_answer") {
		t.Fatalf("next 建议里的明文不得进入候选意图的目标: %q", cand.Goal)
	}
	out2 := Render(g2, RenderInput{
		Challenge: harness.Challenge{Code: "web-01", Description: "拿到 flag 后提交，格式 flag{...}"},
		Intent:    cand,
	})
	if strings.Contains(out2, secret) {
		t.Fatalf("next 建议里的明文被渲染进了 prompt:\n%s", out2)
	}
}

// 净化**不得**误伤阶段链的固定措辞。这条是回归闸：如果哪天有人把裸串形态也接进
// sanitizeGoal（看起来更安全），它会立刻失败——因为裸串判据会把整句目标当成
// 「一个像答案的裸串」。
//
// 实测（本机跑出来的，不是推演）：在裸串题（题面提到「密码」⇒ AllowRaw=true）上，
// exploit / lateral / verify 三个阶段的固定措辞都命中裸串判据。若在那里整句替换，
// 阶段链上会出现一条目标只有 `fp:xxxx/len=NN/…` 的意图，agent 拿到它完全不知道
// 要做什么——而这条链是 v1 的调度主干，等于把主干打瘸。
func TestSanitizeGoalDoesNotManglePhaseGoals(t *testing.T) {
	for _, desc := range []string{
		"从目标机拿到管理员密码，提交原始值（password）",
		"拿到 flag 后提交，格式 flag{...}",
		"",
	} {
		g := New(harness.Challenge{Code: "c", Category: "pentest", Description: desc})
		for _, k := range PhaseChain("pentest") {
			goal := PhaseGoal("pentest", k)
			if got := g.sanitizeGoal(goal); got != goal {
				t.Errorf("题面 %q 下阶段 %s 的固定措辞被改动了:\n原: %q\n净: %q",
					desc, k, goal, got)
			}
		}
	}
	// 反向：信封形态**必须**被净化（否则这条闸是空转的）
	g := newTestGraph(t)
	const secret = "flag{phase_goal_probe}"
	if got := g.sanitizeGoal("验证 " + secret + " 是否正确"); strings.Contains(got, secret) {
		t.Errorf("信封形态必须被净化, got %q", got)
	}
}

// 净化后的去重键：两个**内容不同**的猜测不能因为净化成同一个形态而互相判重。
// （净化发生在算键之前，所以键算的是净化后的目标——这正是要钉的次序。）
func TestIntentDedupKeyComputedAfterSanitize(t *testing.T) {
	g := newTestGraph(t)
	a, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentVerify,
		Goal: "验证 flag{guess_a} 是否正确"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentVerify,
		Goal: "验证 flag{guess_b} 是否正确"})
	if err != nil {
		t.Fatalf("两个不同的猜测应各是一条意图: %v", err)
	}
	if a == b {
		t.Fatal("不同的猜测被误判为重复意图")
	}
	// 同一个猜测再来一次 ⇒ 判重（净化是纯函数，两次得到同一个键）
	if _, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentVerify,
		Goal: "验证 flag{guess_a} 是否正确"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("同一目标应判重, got %v", err)
	}
	// 净化不破坏不变量：载回后 Validate 必须干净（否则 Load 会报「图坏了」）
	if errs := g.Validate(); len(errs) != 0 {
		t.Errorf("净化后的图不该有违规: %v", errs)
	}
}

// 裸串形态：题面要求提交密码时，一条**整串就是答案**的 artifact 事实被拒。
func TestInvariantAnswerShapedRaw(t *testing.T) {
	g := New(harness.Challenge{Code: "c1", Description: "请提交管理员密码"})
	g.Now = fixedClock()
	_, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: "Admin@123", Source: "bash: cat /etc/pass"})
	if !errors.Is(err, ErrAnswerShaped) {
		t.Fatalf("裸串答案必须被拒，got %v", err)
	}
}

// 反向断言：**正常事实不能被误杀**。误杀的代价是丢掉攻击面，
// 比漏收一条可疑内容严重得多（所以裸串判定刻意做得更窄）。
func TestNormalFactsNotAnswerShaped(t *testing.T) {
	g := New(harness.Challenge{Code: "c1", Description: "提交 flag{...} 与管理员密码"})
	g.Now = fixedClock()
	ok := []struct {
		kind    FactKind
		content string
	}{
		{FactTarget, "10.0.0.1:8080"},
		{FactService, "nginx/1.18.0"},
		{FactArtifact, "/var/www/html/index.php"},
		{FactCredential, "admin:Admin@123"},
		{FactArtifact, "flag 格式示例不是事实"},
		{FactArtifact, "/etc/passwd"},
		{FactService, "PHP 7.4.3 running on Apache/2.4.41"},
	}
	for _, c := range ok {
		n := Node{Kind: NodeFact, FactKind: c.kind, Content: c.content, Source: "bash: x"}
		if _, err := g.AddFact(n); err != nil {
			t.Errorf("%s 事实 %q 不该被拒: %v", c.kind, c.content, err)
		}
	}
}

// flag/answer 不是 FactKind —— 类型上没有，AddFact 也就拒收。
func TestFlagIsNotAFactKind(t *testing.T) {
	for _, k := range []FactKind{"flag", "answer", "flag_candidate", "password"} {
		if k.Valid() {
			t.Errorf("%q 不应该是合法 FactKind", k)
		}
	}
	g := newTestGraph(t)
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: "flag",
		Content: "whatever", Source: "agent"}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("未知类别必须被拒，got %v", err)
	}
}

// ── 不变量 3：negative 必须带被证伪的意图 ──

func TestInvariantNegativeNeedsRefutes(t *testing.T) {
	g := newTestGraph(t)
	_, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactNegative,
		Content: "SSH 未开放", Source: "bash: nmap"})
	if !errors.Is(err, ErrNegativeNoRefutes) {
		t.Fatalf("无 refutes 的 negative 必须被拒，got %v", err)
	}
	// 指向不存在的意图也拒
	_, err = g.AddFact(Node{Kind: NodeFact, FactKind: FactNegative,
		Content: "SSH 未开放", Source: "bash: nmap", Refutes: "i_nope"})
	if !errors.Is(err, ErrNegativeNoRefutes) {
		t.Fatalf("refutes 指向不存在的意图必须被拒，got %v", err)
	}
	// 指向事实（而非意图）也拒
	fid := mustFact(t, g, FactService, "nginx", "bash: x")
	_, err = g.AddFact(Node{Kind: NodeFact, FactKind: FactNegative,
		Content: "SSH 未开放", Source: "bash: nmap", Refutes: fid})
	if !errors.Is(err, ErrNegativeNoRefutes) {
		t.Fatalf("refutes 指向事实必须被拒，got %v", err)
	}
}

func TestNegativeCreatesRefutesEdgeAndPrunes(t *testing.T) {
	g := newTestGraph(t)
	i1 := mustIntent(t, g, IntentRecon, "扫端口")
	i2 := mustIntent(t, g, IntentExploit, "试弱口令")

	id, err := g.AddNegative(i1, "22 端口 SSH 未开放", "bash: nmap", TrustHost, "call_1")
	if err != nil {
		t.Fatalf("AddNegative 失败: %v", err)
	}
	if got := g.RefutedBy(i1); len(got) != 1 || got[0] != id {
		t.Errorf("应连一条 refutes 边，got %v", got)
	}
	if n := g.Node(i1); n.State != IntentAbandoned {
		t.Errorf("被证伪的意图应标 abandoned，got %s", n.State)
	}
	// 前沿里不该再出现被证伪的意图
	for _, f := range g.Frontier() {
		if f.ID == i1 {
			t.Error("被证伪的意图必须永久移出前沿（死胡同剪枝）")
		}
	}
	if f := g.NextIntent(); f == nil || f.ID != i2 {
		t.Errorf("下一个应是未被证伪的 %s, got %v", i2, f)
	}
	if len(g.Negative()) != 1 {
		t.Errorf("want 1 negative fact, got %d", len(g.Negative()))
	}
}

// ── 不变量 4：vuln / foothold 必须带证据引用 ──

func TestInvariantVulnNeedsEvidence(t *testing.T) {
	g := newTestGraph(t)
	// agent 自述（无 evidence、无 ToolCallID）不能单独构成 vuln
	_, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactVuln,
		Content: "/upload 无类型校验", Source: "report_fact", Trust: TrustAgent})
	if !errors.Is(err, ErrNoEvidence) {
		t.Fatalf("无证据的 vuln 必须被拒，got %v", err)
	}
	_, err = g.AddFact(Node{Kind: NodeFact, FactKind: FactFoothold,
		Content: "拿到了 www-data shell", Source: "report_fact"})
	if !errors.Is(err, ErrNoEvidence) {
		t.Fatalf("无证据的 foothold 必须被拒，got %v", err)
	}
	// 带 ToolCallID 或 Evidence 都算
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactVuln,
		Content: "/upload 无类型校验", Source: "report_fact", ToolCallID: "call_abc"}); err != nil {
		t.Errorf("带 ToolCallID 的 vuln 应通过: %v", err)
	}
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactVuln,
		Content: "/admin 越权", Source: "report_fact", Evidence: "curl -s /admin 返回 200"}); err != nil {
		t.Errorf("带 Evidence 的 vuln 应通过: %v", err)
	}
}

// inferred 事实永不单独构成 vuln/foothold。
func TestInferredNeverVuln(t *testing.T) {
	g := newTestGraph(t)
	_, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactVuln,
		Content: "可能存在 SQL 注入", Source: "inferred: 版本指纹",
		Trust: TrustInferred, ToolCallID: "call_1"})
	if !errors.Is(err, ErrInferred) {
		t.Fatalf("inferred vuln 必须被拒，got %v", err)
	}
	// 但 inferred 的 artifact 是允许的（它是线索，不是结论）
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: "可能存在的配置文件 /app/config.php", Source: "inferred",
		Trust: TrustInferred}); err != nil {
		t.Errorf("inferred artifact 应允许: %v", err)
	}
}

// ── 不变量 5：去重 ──

func TestDedupByKindAndNormalizedContent(t *testing.T) {
	g := newTestGraph(t)
	id1 := mustFact(t, g, FactService, "nginx", "bash: whatweb")
	// 大小写 / 空白不同 ⇒ 同一条
	id2, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactService,
		Content: "  NGINX  ", Source: "bash: whatweb"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("归一化后相同的内容必须判重，got %v", err)
	}
	if id1 != id2 {
		t.Errorf("重复时应返回已存在节点 id: %s vs %s", id1, id2)
	}
	// 不同 kind ⇒ 不同事实（service:nginx 与 artifact:nginx 是两条）
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: "nginx", Source: "bash: x"}); err != nil {
		t.Errorf("不同类别不该判重: %v", err)
	}
	if len(g.Facts("")) != 1+1 { // seed 无地址 ⇒ 0；这里 2 条
		t.Errorf("want 2 facts, got %d", len(g.Facts("")))
	}
}

func TestDedupIntent(t *testing.T) {
	g := newTestGraph(t)
	i1 := mustIntent(t, g, IntentExploit, "试弱口令")
	i2, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentExploit, Goal: "  试弱口令 "})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("意图应按 (kind, normalize(goal)) 去重，got %v", err)
	}
	if i1 != i2 {
		t.Errorf("重复意图应返回已有 id: %s vs %s", i1, i2)
	}
}

// ── 不变量 6：内容非空 ──

func TestInvariantContentRequired(t *testing.T) {
	g := newTestGraph(t)
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactService, Source: "bash: x"}); !errors.Is(err, ErrNoContent) {
		t.Fatalf("空内容事实必须被拒，got %v", err)
	}
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactService, Content: "   ", Source: "bash: x"}); !errors.Is(err, ErrNoContent) {
		t.Fatalf("空白内容事实必须被拒，got %v", err)
	}
	if _, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentRecon}); !errors.Is(err, ErrNoContent) {
		t.Fatalf("空目标意图必须被拒，got %v", err)
	}
	if _, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: "nope", Goal: "x"}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("未知意图类别必须被拒，got %v", err)
	}
}

// ── 不变量 5 的边界：归一不得改动载荷的大小写 ──

// 前身 `_norm()` 对小写化不加区分，于是 `admin:Admin@123` 落库成
// `admin:admin@123`，注入下一场时 agent 拿到的是**错的密码**，而它无法察觉
// （看起来完全正常，只会反复登录失败）。凭证的载荷是大小写敏感的，所以：
// 去重键折叠大小写（等价判定宽松是安全的），存下来的内容保持原样。
func TestNormalizeKeepsPayloadCase(t *testing.T) {
	g := newTestGraph(t)
	secret := "Admin@123"
	id := mustFact(t, g, FactCredential, "admin:"+secret, "bash: curl")
	got := g.Node(id).Content
	if !strings.Contains(got, secret) {
		t.Fatalf("凭证载荷的大小写必须原样保留，got %q（应为 admin:%s）", got, secret)
	}
	// 但去重键仍然折叠大小写：同一个凭证换个大小写写法必须判重
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactCredential,
		Content: "ADMIN:" + secret, Source: "bash: curl"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("大小写不同的同一凭证应判重，got %v", err)
	}
	// 内部空白折叠（`Admin : Admin@123` 与 `Admin: Admin@123` 是同一形态）
	id2 := mustFact(t, g, FactCredential, "Admin :  Admin@123 ", "bash: curl")
	if g.Node(id2).Content != "Admin : Admin@123" {
		t.Errorf("空白应被折叠，got %q", g.Node(id2).Content)
	}
}

// ── 边 ──

func TestLinkEndpointTypes(t *testing.T) {
	g := newTestGraph(t)
	i := mustIntent(t, g, IntentExploit, "试弱口令")
	f := mustFact(t, g, FactCredential, "admin:Admin@123", "bash: curl")

	cases := []struct {
		from, to string
		kind     EdgeKind
		ok       bool
	}{
		{i, f, EdgeProduces, true},
		{i, f, EdgeRequires, true},
		{f, i, EdgeEnables, true},
		{i, i, EdgeSupersedes, true},
		// 类型错
		{f, i, EdgeProduces, false},
		{i, f, EdgeEnables, false},
		{f, f, EdgeEnables, false},
		{i, i, EdgeProduces, false},
	}
	for _, c := range cases {
		err := g.Link(c.from, c.to, c.kind, 1)
		if c.ok && err != nil {
			t.Errorf("Link(%s->%s, %s) 应成功: %v", c.from, c.to, c.kind, err)
		}
		if !c.ok && err == nil {
			t.Errorf("Link(%s->%s, %s) 应被拒（端点类型错）", c.from, c.to, c.kind)
		}
	}
	if err := g.Link("nope", f, EdgeProduces, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的端点应报 ErrNotFound, got %v", err)
	}
	if err := g.Link(i, f, "bogus", 1); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("未知边类别应被拒, got %v", err)
	}
	// 幂等：连两次同一条边不是错误
	if err := g.Link(i, f, EdgeProduces, 1); err != nil {
		t.Errorf("重复连边应幂等: %v", err)
	}
}

// requires 不能指向 negative 事实（自相矛盾：前置是一条死胡同）。
func TestRequiresCannotPointAtNegative(t *testing.T) {
	g := newTestGraph(t)
	i1 := mustIntent(t, g, IntentRecon, "扫端口")
	i2 := mustIntent(t, g, IntentExploit, "利用")
	nid, err := g.AddNegative(i1, "SSH 未开放", "bash: nmap", TrustHost, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Require(i2, nid); err == nil {
		t.Error("requires 指向 negative 事实应被拒")
	}
}

// ── 意图生命周期 ──

func TestActivateSettleDone(t *testing.T) {
	g := newTestGraph(t)
	i := mustIntent(t, g, IntentRecon, "扫端口")
	if err := g.Activate(i); err != nil {
		t.Fatal(err)
	}
	if n := g.Node(i); n.State != IntentActive || n.Attempts != 1 {
		t.Errorf("Activate 后应为 active/attempts=1, got %s/%d", n.State, n.Attempts)
	}
	f := mustFact(t, g, FactService, "nginx", "bash: nmap")
	if err := g.Settle(i, []string{f}, harness.RoundResult{Turns: 3}); err != nil {
		t.Fatal(err)
	}
	n := g.Node(i)
	if n.State != IntentDone {
		t.Errorf("有新事实产出 ⇒ done, got %s", n.State)
	}
	if got := g.Produced(i); len(got) != 1 || got[0] != f {
		t.Errorf("Settle 应连 produces 边, got %v", got)
	}
	// 终态不可再激活
	if err := g.Activate(i); !errors.Is(err, ErrBadState) {
		t.Errorf("done 的意图不可再 Activate, got %v", err)
	}
}

// 「有新事实」的判据是**本轮创建的**，不是「不在 produced 里」——
// 否则同一事实被第二个意图产出时会算成进展（把失败的轮次记成成功）。
func TestSettleOnlyCountsFreshFacts(t *testing.T) {
	g := newTestGraph(t)
	i1 := mustIntent(t, g, IntentRecon, "扫端口")
	g.Round = 1
	_ = g.Activate(i1)
	f := mustFact(t, g, FactService, "nginx", "bash: nmap")
	_ = g.Settle(i1, []string{f}, harness.RoundResult{})

	i2 := mustIntent(t, g, IntentAnalyze, "读源码")
	g.Round = 2
	_ = g.Activate(i2)
	// 把上一轮的事实再"产出"一次：不该算新事实
	_ = g.Settle(i2, []string{f}, harness.RoundResult{})
	if n := g.Node(i2); n.State != IntentFailed {
		t.Errorf("重复产出旧事实应判 failed, got %s", n.State)
	}
}

// 产出 negative 也算进展（把死路走完是有价值的）——前身只能靠整体刹车，
// 无法表达「这条具体路径已试过」。
func TestSettleNegativeIsProgress(t *testing.T) {
	g := newTestGraph(t)
	i := mustIntent(t, g, IntentExploit, "试弱口令")
	_ = g.Activate(i)
	nid, err := g.AddNegative(i, "admin/admin 登录失败", "bash: curl", TrustHost, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Settle(i, []string{nid}, harness.RoundResult{}); err != nil {
		t.Fatal(err)
	}
	if n := g.Node(i); n.State != IntentDone {
		t.Errorf("产出 negative 应算 done, got %s", n.State)
	}
}

func TestAttemptsCapAbandons(t *testing.T) {
	g := newTestGraph(t)
	id, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentExploit,
		Goal: "试弱口令", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := g.Activate(id); err != nil {
			t.Fatalf("第 %d 次 Activate 失败: %v", i+1, err)
		}
		if err := g.Settle(id, nil, harness.RoundResult{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := g.Node(id); n.State != IntentAbandoned {
		t.Errorf("到上限应标 abandoned, got %s (attempts=%d)", n.State, n.Attempts)
	}
	if f := g.NextIntent(); f != nil {
		t.Errorf("到上限的意图必须退出前沿, got %v", f.ID)
	}
}

// ── 前沿与优先级 ──

// 阶段序 > 创建轮次 > 创建序号，且平局必须确定性。
func TestFrontierPriority(t *testing.T) {
	g := newTestGraph(t)
	// 故意按乱序创建：analyze 在前，recon 在后；pentest 链里 recon 排在 exploit 前
	a := mustIntent(t, g, IntentExploit, "利用 A")
	b := mustIntent(t, g, IntentRecon, "侦察 B")
	c := mustIntent(t, g, IntentExploit, "利用 C")

	f := g.Frontier()
	if len(f) != 3 {
		t.Fatalf("want 3 frontier nodes, got %d", len(f))
	}
	// 阶段序第一：recon(b) 必须排第一，exploit 之间按创建序号 a < c
	if f[0].ID != b {
		t.Errorf("阶段序应优先: want %s(recon), got %s(%s)", b, f[0].ID, f[0].IntentKind)
	}
	if f[1].ID != a || f[2].ID != c {
		t.Errorf("同阶段应按创建序号: want %s,%s got %s,%s", a, c, f[1].ID, f[2].ID)
	}
}

// 同一张图反复调用 Frontier / NextIntent 必须得到**完全相同**的顺序。
// 没有这条性质，调度会随 map 迭代顺序漂移，同一道题跑两次行为不同。
func TestFrontierDeterministic(t *testing.T) {
	g := newTestGraph(t)
	for i := 0; i < 30; i++ {
		_, _ = g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentExploit,
			Goal: "exploit-" + string(rune('a'+i%26)) + "-" + itoa(i)})
	}
	first := ids(g.Frontier())
	for k := 0; k < 50; k++ {
		got := ids(g.Frontier())
		if len(got) != len(first) {
			t.Fatalf("前沿长度不稳定: %d vs %d", len(got), len(first))
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("第 %d 次调用顺序漂移:\n%v\n%v", k, first, got)
			}
		}
	}
}

func ids(ns []*Node) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.ID)
	}
	return out
}

func TestFrontierSkipsRequiresUnmet(t *testing.T) {
	g := newTestGraph(t)
	i1 := mustIntent(t, g, IntentEscalate, "提权")
	i2 := mustIntent(t, g, IntentRecon, "侦察")
	// 提权需要两条前置事实，它们此刻都还没被发现。悬空的 requires 必须允许——
	// 「提权需要先有 foothold」在 foothold 出现之前就该能声明，否则调用方只能
	// 等事实出现后再补边，而那时意图可能已经被挑走执行过一次了。
	// 前置的 id 由调用方**预先指定**（AddFact 接受调用方给的 ID），这样声明前置
	// 与后来创建那条事实能对上。
	if err := g.Require(i1, "f_foothold"); err != nil {
		t.Fatal(err)
	}
	if err := g.Require(i1, "f_creds"); err != nil {
		t.Fatal(err)
	}
	f := g.NextIntent()
	if f == nil || f.ID != i2 {
		t.Fatalf("前置未满足的意图应被跳过，got %v", f)
	}
	// 满足**第一条**前置：还不够——只要有一条前置悬空就必须跳过。
	// 这条断言是刻意留的：只检查「至少有一条前置满足」的实现会在这里放行一个
	// 前置不全的意图（提权在没有凭证时会空跑一轮，白烧预算）。
	if _, err := g.AddFact(Node{ID: "f_foothold", Kind: NodeFact, FactKind: FactFoothold,
		Content: "www-data shell", Source: "bash: nc", ToolCallID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if got := g.Requires(i1); len(got) != 2 {
		t.Fatalf("两条 requires 边都应保留, got %v", got)
	}
	for _, n := range g.Frontier() {
		if n.ID == i1 {
			t.Error("仍有前置悬空时，意图不该在前沿里")
		}
	}
	// 第二条前置也入图 ⇒ 自动解锁（Blocked 是算出来的，不落盘）
	if _, err := g.AddFact(Node{ID: "f_creds", Kind: NodeFact, FactKind: FactCredential,
		Content: "root:toor1234", Source: "bash: cat"}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range g.Frontier() {
		if n.ID == i1 {
			found = true
		}
	}
	if !found {
		t.Error("前置事实全部入图后，意图应自动解锁（blocked 是算出来的）")
	}
	// 悬空的 requires 不该让 Validate 报错（它是合法状态，不是坏图）
	for _, err := range g.Validate() {
		if strings.Contains(err.Error(), "requires") {
			t.Errorf("悬空的 requires 不该被 Validate 判为坏图: %v", err)
		}
	}
}

func TestNextIntentNilWhenFrontierEmpty(t *testing.T) {
	g := newTestGraph(t)
	if f := g.NextIntent(); f != nil {
		t.Errorf("空图的前沿应为 nil, got %v", f.ID)
	}
	// 意图全部进入终态后，前沿为空（收尾信号）。failed 是可重试的中间态，
	// 所以要走满尝试上限才会真的离开前沿。
	i := mustIntent(t, g, IntentRecon, "侦察")
	for k := 0; k < DefaultMaxAttempts; k++ {
		if err := g.Activate(i); err != nil {
			t.Fatal(err)
		}
		if err := g.Settle(i, nil, harness.RoundResult{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := g.Node(i); n.State != IntentAbandoned {
		t.Fatalf("走满尝试上限后应 abandoned, got %s", n.State)
	}
	if f := g.NextIntent(); f != nil {
		t.Errorf("唯一意图进入终态后前沿应为 nil, got %v", f.ID)
	}
}

// 分支点：一个事实 enables 多个候选意图，一条失败后能回到另一条。
func TestBranchingFromOneFact(t *testing.T) {
	g := newTestGraph(t)
	f := mustFact(t, g, FactArtifact, "/admin", "bash: ffuf")
	i1, err := g.EnableFrom(f, Node{Kind: NodeIntent, IntentKind: IntentExploit, Goal: "试 /admin 弱口令"})
	if err != nil {
		t.Fatal(err)
	}
	i2, err := g.EnableFrom(f, Node{Kind: NodeIntent, IntentKind: IntentExploit, Goal: "试 /admin 越权"})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.EnabledFrom(i1); len(got) != 1 || got[0] != f {
		t.Errorf("应连 enables 边, got %v", got)
	}
	// 第一条失败（含 negative）后，第二条仍在前沿
	_ = g.Activate(i1)
	_, _ = g.AddNegative(i1, "/admin 弱口令失败", "bash: curl", TrustHost, "c1")
	_ = g.Settle(i1, nil, harness.RoundResult{})
	f2 := g.NextIntent()
	if f2 == nil || f2.ID != i2 {
		t.Fatalf("一条分支失败后应回到另一条, got %v", f2)
	}
	// 重复 enables 同一意图：不报错，且不丢边
	if id, err := g.EnableFrom(f, Node{Kind: NodeIntent, IntentKind: IntentExploit, Goal: "试 /admin 弱口令"}); !errors.Is(err, ErrDuplicate) || id != i1 {
		t.Errorf("重复派生应返回已有意图 + ErrDuplicate, got %s %v", id, err)
	}
}

func TestSupersede(t *testing.T) {
	g := newTestGraph(t)
	old := mustIntent(t, g, IntentExploit, "旧的利用方向")
	nw := mustIntent(t, g, IntentExploit, "新的利用方向")
	if err := g.Supersede(old, nw); err != nil {
		t.Fatal(err)
	}
	if n := g.Node(old); n.State != IntentAbandoned {
		t.Errorf("被取代的意图应标 abandoned, got %s", n.State)
	}
	// 保留而不是删除（审计痕迹）
	if g.Node(old) == nil {
		t.Error("被取代的意图不该被删除")
	}
}

// ── 读取接口返回克隆（唯一写入通道） ──

func TestReadsReturnClones(t *testing.T) {
	g := newTestGraph(t)
	id := mustFact(t, g, FactService, "nginx", "bash: x")
	n := g.Node(id)
	n.Content = "被篡改了"
	n.FactKind = FactVuln
	if got := g.Node(id).Content; got != "nginx" {
		t.Errorf("外部修改不应影响图内节点, got %q", got)
	}
	for _, f := range g.Facts("") {
		f.Content = "改"
	}
	if got := g.Facts("")[0].Content; got != "nginx" {
		t.Errorf("Facts 返回的必须是克隆, got %q", got)
	}
	es := g.Edges()
	if len(es) > 0 {
		es[0].Kind = "bogus"
		if g.Edges()[0].Kind == "bogus" {
			t.Error("Edges 返回的必须是副本")
		}
	}
}

func TestStats(t *testing.T) {
	g := newTestGraph(t)
	i := mustIntent(t, g, IntentRecon, "侦察")
	_ = mustFact(t, g, FactService, "nginx", "bash: x")
	_ = g.Activate(i)
	st := g.Stats()
	if st.Facts != 1 || st.Intents != 1 {
		t.Errorf("stats: %+v", st)
	}
	if st.ByFact[FactService] != 1 || st.ByState[IntentActive] != 1 {
		t.Errorf("分类统计错: %+v", st)
	}
}

func TestValidateClean(t *testing.T) {
	g := newTestGraph(t, "10.0.0.1:80")
	i := mustIntent(t, g, IntentRecon, "侦察")
	_, _ = g.AddNegative(i, "22 未开放", "bash: nmap", TrustHost, "c1")
	if errs := g.Validate(); len(errs) != 0 {
		t.Errorf("干净的图不该有违规: %v", errs)
	}
}

// Validate 必须能发现被人工改坏的图（dag.json 是会被人工编辑的）。
func TestValidateDetectsTampering(t *testing.T) {
	g := newTestGraph(t)
	// 直接改内部结构，模拟「有人手改了 dag.json」
	g.nodes["f_bad"] = &Node{ID: "f_bad", Kind: NodeFact, FactKind: FactService, Content: "x"}
	g.order = append(g.order, "f_bad") // 无 Source
	g.nodes["f_flag"] = &Node{ID: "f_flag", Kind: NodeFact, FactKind: FactArtifact,
		Content: "flag{injected}", Source: "manual"}
	g.order = append(g.order, "f_flag")
	errs := g.Validate()
	if len(errs) < 2 {
		t.Fatalf("应报出至少 2 处违规（无来源 + 答案形状），got %v", errs)
	}
}

func TestEquivalent(t *testing.T) {
	mk := func() *Graph {
		g := newTestGraph(t, "10.0.0.1:80")
		i := mustIntent(t, g, IntentRecon, "侦察")
		f := mustFact(t, g, FactService, "nginx", "bash: x")
		_ = g.Link(i, f, EdgeProduces, 1)
		_, _ = g.AddNegative(i, "22 未开放", "bash: nmap", TrustHost, "c1")
		return g
	}
	a, b := mk(), mk()
	if err := a.Equivalent(b); err != nil {
		t.Errorf("等价图应判等价: %v", err)
	}
	_ = mustFact(t, b, FactService, "apache", "bash: x")
	if err := a.Equivalent(b); err == nil {
		t.Error("节点数不同应判不等价")
	}
}

// itoa 是包内小工具（shape.go 里有同名私有函数，这里避免跨包依赖）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
