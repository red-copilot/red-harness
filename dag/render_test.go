package dag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

// goldenGraph 构造一张内容固定的图：事实/意图/边/状态都写死，时钟也固定，
// 所以 Render 的输出必须逐字节稳定。
func goldenGraph(t *testing.T) *Graph {
	t.Helper()
	g := New(harness.Challenge{
		Code: "web-01", Category: "pentest", Difficulty: "medium",
		FlagCount:   2,
		Description: "目标是一个 PHP 上传站，拿到 flag 后提交，格式 flag{...}。",
		Addrs:       []string{"10.0.0.1:80", "10.0.0.1:8080"},
	})
	g.Now = fixedClock()

	i1 := mustIntent(t, g, IntentRecon, "侦察：枚举目标开放端口与服务指纹（nmap/whatweb/curl -I 等），把看到的地址、服务与版本记录成事实。")
	_ = mustFact(t, g, FactService, "nginx/1.18.0", "bash: whatweb http://10.0.0.1")
	_ = mustFact(t, g, FactCredential, "admin:Admin@123", "bash: curl -s http://10.0.0.1/login")
	mustIntent(t, g, IntentExploit, "验证 /upload 的任意文件上传：尝试上传 webshell 并访问验证。")

	// /upload 的 vuln 事实（agent 申报，带证据）
	vuln, err := g.AddFact(Node{
		Kind: NodeFact, FactKind: FactVuln, Content: "/upload 无类型校验",
		Source: "report_fact", Trust: TrustAgent, Confidence: 0.9,
		ToolCallID: "call_abc123", Evidence: "call_abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.EnableFrom(vuln, Node{Kind: NodeIntent, IntentKind: IntentFoothold,
		Goal:   "获取初始访问：用 /upload 上传 webshell 拿到执行点。",
		Expect: []FactKind{FactFoothold}}); err != nil {
		t.Fatal(err)
	}

	// 一条死胡同
	if _, err := g.AddNegative(i1, "22 端口 SSH 未开放（connection refused）",
		"bash: nmap -p22 10.0.0.1", TrustHost, "call_n1"); err != nil {
		t.Fatal(err)
	}

	// 让 i2 成为本轮的意图：i1 已经 done
	_ = g.Activate(i1)
	if err := g.Settle(i1, nil, harness.RoundResult{}); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestRenderGolden 是 golden 测试：prompt 的形状是「拼字符串」的代码里唯一
// 可靠的回归手段——措辞改一个字都可能让 agent 的行为变掉（尤其是「不要再试」
// 与「不要自己加外壳」这两句），而没人会注意到 diff。
func TestRenderGolden(t *testing.T) {
	g := goldenGraph(t)
	it := g.Node(mustFindIntent(t, g, IntentExploit))
	in := RenderInput{
		Challenge: harness.Challenge{
			Code: "web-01", Category: "pentest", Difficulty: "medium",
			FlagCount:   2,
			Description: "目标是一个 PHP 上传站，拿到 flag 后提交，格式 flag{...}。",
			Addrs:       []string{"10.0.0.1:80", "10.0.0.1:8080"},
		},
		Confirmed: 1,
		Rejected:  []string{"fp:deadbeef/len=12/f…x"},
		Intent:    it,
	}
	got := Render(g, in)

	golden := filepath.Join("testdata", "render_golden.txt")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden 已更新: %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("读 golden 失败（首次运行请用 UPDATE_GOLDEN=1 go test ./dag/）: %v", err)
	}
	if got != string(want) {
		t.Errorf("Render 输出与 golden 不一致。\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
	}
}

func mustFindIntent(t *testing.T, g *Graph, kind IntentKind) string {
	t.Helper()
	for _, id := range g.order {
		if n := g.nodes[id]; n != nil && n.IsIntent() && n.IntentKind == kind {
			return id
		}
	}
	t.Fatalf("找不到 %s 意图", kind)
	return ""
}

// 段的顺序与必备措辞。golden 覆盖整体形状，这条覆盖「哪几句必须有」——
// golden 文件被更新时，措辞的丢失会在这里被抓住。
func TestRenderRequiredSections(t *testing.T) {
	g := goldenGraph(t)
	it := g.Node(mustFindIntent(t, g, IntentExploit))
	out := Render(g, RenderInput{
		Challenge: harness.Challenge{Code: "web-01", Category: "pentest",
			FlagCount: 2, Description: "提交 flag{...}"},
		Confirmed: 1, Intent: it,
	})

	for _, want := range []string{
		"## 本题",
		"## 已知事实（有来源，可直接使用）",
		"## 已证伪，不要再试",
		"## 本轮意图（只做这一件事）",
		"## 交付",
		// 交付段的措辞是前身 `_INTRANET_ORCHESTRATION` 规则 2 的原话
		"答案格式以题目描述为准",
		"不要自己加外壳",
		// 追加而不是覆盖：覆盖会丢掉之前拿到的答案
		"追加，不要覆盖",
		// 死胡同段的明确措辞
		"不要再试",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("渲染结果缺少 %q\n%s", want, out)
		}
	}
	// 段的先后顺序
	order := []string{"## 本题", "## 已知事实", "## 已证伪", "## 本轮意图", "## 交付"}
	last := -1
	for _, sec := range order {
		i := strings.Index(out, sec)
		if i < 0 {
			t.Fatalf("缺少段 %q", sec)
		}
		if i < last {
			t.Errorf("段 %q 的顺序不对", sec)
		}
		last = i
	}
	// 意图段必须最后被强调「只做这一件事」
	if !strings.Contains(out, "只做这一件事") {
		t.Error("意图段必须明确「只做这一件事」（前身最贵的浪费是同轮开多条线）")
	}
}

// 判错账本回灌**只给指纹不给明文**。
func TestRenderRejectedFingerprintOnly(t *testing.T) {
	g := goldenGraph(t)
	secret := "flag{do_not_leak_me}"
	out := Render(g, RenderInput{
		Challenge: harness.Challenge{Code: "web-01", Description: "提交 flag{...}"},
		Intent:    g.Node(mustFindIntent(t, g, IntentExploit)),
		Rejected:  []string{FlagFingerprint(secret)},
	})
	if strings.Contains(out, secret) || strings.Contains(out, "do_not_leak_me") {
		t.Fatalf("判错回灌不得含明文:\n%s", out)
	}
	if !strings.Contains(out, "fp:") {
		t.Error("判错回灌应含指纹")
	}
}

// 事实段必须带来源与信任层。
func TestRenderFactsCarryProvenance(t *testing.T) {
	g := goldenGraph(t)
	out := Render(g, RenderInput{
		Challenge: harness.Challenge{Code: "web-01", Description: "提交 flag{...}"},
		Intent:    g.Node(mustFindIntent(t, g, IntentExploit)),
	})
	if !strings.Contains(out, "← bash: whatweb http://10.0.0.1") {
		t.Errorf("事实必须带来源:\n%s", out)
	}
	if !strings.Contains(out, "agent 申报") {
		t.Errorf("agent 申报的事实必须标明信任层（否则 agent 会照着自己上轮的猜测往下推）:\n%s", out)
	}
	if !strings.Contains(out, "← platform") {
		t.Errorf("题目初始化的目标事实来源应显示为 platform:\n%s", out)
	}
}

// 死胡同段必须带「源自哪个意图」，否则 agent 看到一句无主的断言。
func TestRenderNegativeShowsOrigin(t *testing.T) {
	g := goldenGraph(t)
	out := Render(g, RenderInput{
		Challenge: harness.Challenge{Code: "web-01", Description: "提交 flag{...}"},
		Intent:    g.Node(mustFindIntent(t, g, IntentExploit)),
	})
	if !strings.Contains(out, "22 端口 SSH 未开放") || !strings.Contains(out, "源自 intent:") {
		t.Errorf("死胡同段应带内容与来源意图:\n%s", out)
	}
	// 大小写原样：内容归一做了空白折叠（`22  端口` ⇒ `22 端口`），但**不**做
	// 小写化——所以这里 `SSH` 保持大写。若哪天有人给 normalize 加上 ToLower，
	// 凭证载荷会被悄悄改错（见 TestNormalizeKeepsPayloadCase），这条断言会同时失败。
	if !strings.Contains(out, "SSH 未开放") {
		t.Errorf("内容归一不得改动大小写:\n%s", out)
	}
}

// 前沿耗尽时渲染「收口」而不是模糊的「继续找找」。
func TestRenderNoIntent(t *testing.T) {
	g := goldenGraph(t)
	out := Render(g, RenderInput{
		Challenge: harness.Challenge{Code: "web-01", Description: "提交 flag{...}"},
		Intent:    nil,
	})
	if !strings.Contains(out, "前沿已空") {
		t.Errorf("意图为 nil 时应渲染收口段:\n%s", out)
	}
	if strings.Contains(out, "report_fact 申报本轮事实") {
		t.Error("收口段不该要求申报本轮事实（没有本轮意图）")
	}
}

// 事实段的条目上限 + 「还有 N 条未列出」的显式提示。
func TestRenderFactLimit(t *testing.T) {
	g := newTestGraph(t)
	for i := 0; i < 60; i++ {
		if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
			Content: "/path/file" + itoa(i) + ".php", Source: "bash: x"}); err != nil {
			t.Fatal(err)
		}
	}
	out := Render(g, RenderInput{
		Challenge: harness.Challenge{Code: "c", Description: "x"},
		MaxFacts:  5,
	})
	lines := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "- [") {
			lines++
		}
	}
	if lines != 5 {
		t.Errorf("MaxFacts=5 应只列 5 条, got %d\n%s", lines, out)
	}
	if !strings.Contains(out, "另有 55 条") {
		t.Errorf("必须显式说明还有多少条被省略（否则 agent 会重复做已做过的事）:\n%s", out)
	}
}

// 渲染必须**稳定**：同一张图渲染两次逐字节相同（golden 的前提）。
func TestRenderDeterministic(t *testing.T) {
	g := goldenGraph(t)
	it := g.Node(mustFindIntent(t, g, IntentExploit))
	in := RenderInput{Challenge: harness.Challenge{Code: "web-01",
		Description: "提交 flag{...}"}, Intent: it}
	first := Render(g, in)
	for i := 0; i < 20; i++ {
		if got := Render(g, in); got != first {
			t.Fatalf("第 %d 次渲染不一致", i)
		}
	}
}

// 图还没建就渲染（平台 Start 失败、或驱动在初始化前被打断）不该 panic：
// 渲染是纯函数，退化成「只有题面与交付约定」比让驱动进程崩掉有用得多。
func TestRenderNilGraph(t *testing.T) {
	out := Render(nil, RenderInput{
		Challenge: harness.Challenge{Code: "web-01", Description: "提交 flag{...}"},
	})
	if !strings.Contains(out, "## 本题") || !strings.Contains(out, "不要自己加外壳") {
		t.Errorf("nil 图也应渲染出题面与交付约定:\n%s", out)
	}
	if strings.Contains(out, "## 已知事实") {
		t.Error("没有图就没有「已知事实」段")
	}
}
