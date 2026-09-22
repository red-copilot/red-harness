package dag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

// mermaidGraph 是导出器的固定夹具。
//
// **刻意不复用 render_test.go 的 goldenGraph**：那个夹具锚的是 prompt 措辞契约，
// 这个锚的是图形状契约，两者的评审标准不同。共用一份夹具会把它们耦合成一个
// golden——改一句 prompt 措辞会同时震两份 golden，而其中一份本该纹丝不动。
func mermaidGraph(t *testing.T) *Graph {
	t.Helper()
	g := New(harness.Challenge{
		Code: "web-01", Category: "pentest", Difficulty: "medium",
		FlagCount: 2, Description: "拿到 flag 后提交，格式 flag{...}",
		Addrs: []string{"10.0.0.1:80"},
	})
	g.Now = fixedClock()

	recon := mustIntent(t, g, IntentRecon, "侦察：枚举目标开放端口与服务指纹")
	_ = recon
	_ = mustFact(t, g, FactService, "nginx/1.18.0", "bash: whatweb http://10.0.0.1")
	_ = mustFact(t, g, FactArtifact, "/var/www/html/upload.php", "bash: ls -la /var/www")
	exploit := mustIntent(t, g, IntentExploit, "验证 /upload 的任意文件上传")

	// 宿主验证的 vuln（可信度最高那一档）
	vuln, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactVuln,
		Content: "/upload 无类型校验", Source: "bash: curl -F file=@x.php",
		Trust: TrustHost, ToolCallID: "call_v1", Evidence: "call_v1"})
	if err != nil {
		t.Fatal(err)
	}
	// agent 自述的事实（虚线框）
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactCredential,
		Content: "admin:Admin@123", Source: "report_fact",
		Trust: TrustAgent, Confidence: 0.7}); err != nil {
		t.Fatal(err)
	}
	// 推导出来的事实
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactService,
		Content: "PHP/7.4", Source: "inferred", Trust: TrustInferred}); err != nil {
		t.Fatal(err)
	}

	foothold := mustIntent(t, g, IntentFoothold, "拿到初始访问")
	extract := mustIntent(t, g, IntentExtract, "从立足点取 flag")
	verify := mustIntent(t, g, IntentVerify, "复核候选")

	// 三种 abandoned 成因各来一个：refutes / supersedes / 都没有（停滞放弃）
	stalled := mustIntent(t, g, IntentEscalate, "提权到 root")
	lateral := mustIntent(t, g, IntentLateral, "横向到数据库")

	if err := g.Require(exploit, vuln); err != nil {
		t.Fatal(err)
	}
	// enables 是 fact → intent（不是 intent → intent）。
	if err := g.Link(vuln, exploit, EdgeEnables, 1); err != nil {
		t.Fatal(err)
	}
	if err := g.Link(exploit, foothold, EdgeSupersedes, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddNegative(verify, "候选 flag 是题面里的示例", "bash: grep flag /var/www", TrustHost, "call_n1"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetState(stalled, IntentAbandoned); err != nil {
		t.Fatal(err)
	}
	if err := g.SetState(lateral, IntentAbandoned); err != nil {
		t.Fatal(err)
	}
	if err := g.SetState(foothold, IntentAbandoned); err != nil {
		t.Fatal(err)
	}
	if err := g.SetState(extract, IntentDone); err != nil {
		t.Fatal(err)
	}
	if err := g.SetState(verify, IntentFailed); err != nil {
		t.Fatal(err)
	}
	g.Round = 4
	_ = extract
	return g
}

// TestMermaidGolden 是 golden 测试：图是拼字符串拼出来的，而它的形状是给人读的
// 契约——样式/标签/类名改一个字，复盘时看到的东西就变了，而不会有任何测试变红。
//
// ⚠️ `UPDATE_GOLDEN=1 go test ./dag/` 会**同时**重写 render_golden.txt 与
// mermaid_golden.txt。重生成后必须 `git diff dag/testdata/` 确认只有预期那份变了。
func TestMermaidGolden(t *testing.T) {
	got := Mermaid(mermaidGraph(t))
	golden := filepath.Join("testdata", "mermaid_golden.txt")
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
		t.Errorf("Mermaid 输出与 golden 不一致。\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
	}
}

// TestMermaidAbandonedCauses：三种「意图不在了」的成因必须能分辨。
//
// 这是这张图最值得回答的问题：一个方向被放弃，是事实层发现了死胡同（refutes）、
// 编排层主动换了方向（supersedes），还是编排层因为停滞把它扔了（都没有入边）？
// 三者指向完全不同的改法，而只看状态 `abandoned` 分不出来。
func TestMermaidAbandonedCauses(t *testing.T) {
	g := mermaidGraph(t)
	out := Mermaid(g)
	for _, want := range []string{"被证伪剪枝", "被换方向取代", "因停滞放弃"} {
		if !strings.Contains(out, want) {
			t.Errorf("abandoned 的成因 %q 没有出现在图里（三种成因必须可分辨）:\n%s", want, out)
		}
	}
}

// TestMermaidCoversEveryKind：每个 kind / state 都要有样式映射。
//
// 这是防「新增一个 FactKind 时图里静默少一类节点」的唯一防线——`Mermaid` 对未知
// 值会落到 default 分支，图照样能渲染出来，只是新那类失去了区分度。
func TestMermaidCoversEveryKind(t *testing.T) {
	for k := range factKinds {
		if got := nodeClass(&Node{Kind: NodeFact, FactKind: k}); got == "" {
			t.Errorf("FactKind %q 没有样式类", k)
		}
	}
	for k := range intentKinds {
		if got := nodeClass(&Node{Kind: NodeIntent, IntentKind: k, State: IntentPending}); got == "" {
			t.Errorf("IntentKind %q 没有样式类", k)
		}
	}
	for s := range intentStates {
		if got := nodeClass(&Node{Kind: NodeIntent, IntentKind: IntentRecon, State: s}); got == "" {
			t.Errorf("IntentState %q 没有样式类", s)
		}
	}
	// 每个状态的样式都必须有定义，否则 mermaid 会因为引用未定义的 class 而报错。
	for s := range intentStates {
		name := "intent_" + string(s)
		if !strings.Contains(mermaidClassStyle(name), "fill") {
			t.Errorf("状态 %q 的样式没定义（mermaid 会因引用未定义的类报错）", s)
		}
	}
	for _, k := range []EdgeKind{EdgeRequires, EdgeProduces, EdgeEnables, EdgeRefutes, EdgeDerivedFrom, EdgeSupersedes} {
		arrow, label := mermaidEdge(Edge{Kind: k})
		if arrow == "" || label == "" {
			t.Errorf("EdgeKind %q 没有箭头/标签映射", k)
		}
	}
}

// TestMermaidEscapesHostileLabels：标签里的内容不能把图弄坏或弄错。
//
// 每条规则都对应一种真的会出问题的输入：换行会把节点声明截断（而工具输出里换行
// 是常态）；`"` 是标签终止符；`#` 是 mermaid 的实体起始符，不转义会让正文里的
// `#` 与后面字符凑成实体、渲染成别的字。中文必须原样输出。
func TestMermaidEscapesHostileLabels(t *testing.T) {
	// 用**精确期望**而不是「包含某个片段」：片段断言会被恰好也含那段文本的输入
	// 骗过去（第一版就是这么写的——语料里自带 `#35;`，于是去掉 `#` 的转义后
	// 测试照样绿）。
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"双引号", `说 "你好" 然后停`, `说 #quot;你好#quot; 然后停`},
		{"井号", "flag 与 c# 语言", "flag 与 c#35; 语言"},
		{"井号后紧跟数字", "#35", "#35;35"},
		{"换行", "第一行\n第二行", "第一行 第二行"},
		{"制表符与控制字符", "a\tb\x01c", "a b c"},
		{"首尾空白", "  x  ", "x"},
		// `-->` 在**引号包住的标签内**是安全的（引号划定了边界），所以不转义。
		// 要防的是它出现在标识符位置——那由「标识符自生成」保证，见
		// TestMermaidHostileNodeIDsNotUsedAsIdentifiers。
		{"箭头", "a --> b", "a --> b"},
		{"中文与全角", "侦察：目标「web-01」", "侦察：目标「web-01」"},
		{"空串", "", "(无内容)"},
		{"只有空白", "   \n\t ", "(无内容)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := escapeMermaidLabel(tc.in)
			if got != tc.want {
				t.Errorf("escapeMermaidLabel(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("标签里不得有换行（mermaid 逐行解析，会把节点声明截断）: %q", got)
			}
		})
	}
	// 长文本按 rune 截断，不能切出半个中文。
	long := strings.Repeat("中", mermaidLabelMax+10)
	got := escapeMermaidLabel(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("超长标签应截断并加省略号: %q", got)
	}
	if n := len([]rune(strings.TrimSuffix(got, "…"))); n != mermaidLabelMax {
		t.Errorf("截断后长度 = %d rune，期望 %d", n, mermaidLabelMax)
	}
}

// TestMermaidHostileNodeIDsNotUsedAsIdentifiers：人工编辑过的 id 不得变成语法。
//
// `graph.json` 是允许人工修的（schema 注释里写明真实运维一定会有人去改它），而
// `Validate` 不校验 id 形态。所以 id 可以是 `x-->y`、`end`、`a b` 这些 mermaid
// 语法——导出器必须自生成标识符，把原始 id 只放进标签文本。
func TestMermaidHostileNodeIDsNotUsedAsIdentifiers(t *testing.T) {
	g := newTestGraph(t)
	// 直接构造带恶意 id 的节点（走 addFact 的 seeding 路径之外的合法入口：
	// 图允许调用方自带 id）。
	if _, err := g.AddFact(Node{ID: "x-->y", Kind: NodeFact, FactKind: FactArtifact,
		Content: "/tmp/a", Source: "bash: ls"}); err != nil {
		t.Fatalf("自带 id 的事实应能入图（前置条件）: %v", err)
	}
	if _, err := g.AddIntent(Node{ID: "end", Kind: NodeIntent,
		IntentKind: IntentRecon, Goal: "扫端口"}); err != nil {
		t.Fatalf("自带 id 的意图应能入图（前置条件）: %v", err)
	}
	out := Mermaid(g)
	for _, line := range strings.Split(out, "\n") {
		// 标识符位置出现 `x-->y` 就等于往图里注入了一条边。
		if strings.HasPrefix(strings.TrimSpace(line), "x-->y") {
			t.Errorf("恶意 id 被当成了标识符:\n%s", line)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "end ") {
			t.Errorf("保留字 id 被当成了标识符:\n%s", line)
		}
	}
	// 但它必须仍然出现在标签里——否则「有这个节点」这件事就丢了。
	if !strings.Contains(out, "x--&gt;y") && !strings.Contains(out, "x-->y") {
		t.Error("原始 id 应当出现在标签文本里（信息不能丢，只是不能当标识符）")
	}
}

// TestMermaidSkipsDanglingEdges：端点不存在的边整条跳过。
//
// `Link` 允许 requires 指向不存在的事实（见它的注释）。画成占位节点会让图与事实
// 不符——复盘的人会以为有个节点，而实际没有。
func TestMermaidSkipsDanglingEdges(t *testing.T) {
	g := newTestGraph(t)
	i := mustIntent(t, g, IntentRecon, "扫端口")
	if err := g.Link(i, "ghost-fact-id", EdgeRequires, 1); err != nil {
		t.Fatalf("前置条件：指向缺失端点的边应当能连上: %v", err)
	}
	out := Mermaid(g)
	if strings.Contains(out, "ghost-fact-id") {
		t.Errorf("悬空边的端点不得出现在图里:\n%s", out)
	}
	if strings.Count(out, "-->") != 0 {
		t.Errorf("悬空边应整条跳过:\n%s", out)
	}
}

// TestMermaidNoPlaintext：答案明文不得出现在导出里。
//
// 导出器从 `document()` 渲染（与落盘同一份、已擦洗），所以这条同时钉住了
// 「`.mmd` 不会成为绕过 scrub 的新出口」——`MarshalJSON` 曾经就是这样漏的。
func TestMermaidNoPlaintext(t *testing.T) {
	g := mermaidGraph(t)
	const secret = "flag{mermaid_must_not_leak}"
	const fragment = "mermaid_must_not_leak"

	// 1) 被拒收的答案形状事实（拒收审计里带原文）
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: secret, Source: "bash: cat /tmp/f"}); err == nil {
		t.Fatal("前置条件：答案形状的事实必须被拒")
	}
	// 2) 普通事实的 Raw 里嵌明文
	if _, err := g.AddFact(Node{Kind: NodeFact, FactKind: FactArtifact,
		Content: "/tmp/leak.txt", Raw: "cat → " + secret,
		Source: "bash: cat /tmp/leak.txt"}); err != nil {
		t.Fatal(err)
	}
	// 3) 意图目标里嵌明文
	if _, err := g.AddIntent(Node{Kind: NodeIntent, IntentKind: IntentVerify,
		Goal: "复核 " + secret + " 是否成立"}); err != nil {
		t.Fatal(err)
	}

	out := Mermaid(g)
	if strings.Contains(out, secret) || strings.Contains(out, fragment) {
		t.Fatalf("导出里出现了答案明文:\n%s", out)
	}
}

// TestMermaidEmptyAndNilGraph：空图与 nil 都要产出可解析的文档。
func TestMermaidEmptyAndNilGraph(t *testing.T) {
	if got := Mermaid(nil); !strings.HasPrefix(got, "flowchart TD") {
		t.Errorf("nil 图应返回最小可解析文档，得到 %q", got)
	}
	// 只有种子事实的图（没有任何意图/边）
	empty := newTestGraph(t, "10.0.0.1:80")
	out := Mermaid(empty)
	if !strings.HasPrefix(out, "flowchart TD") {
		t.Errorf("空图也要以 flowchart 开头: %q", out)
	}
	if !strings.Contains(out, "10.0.0.1:80") {
		t.Error("种子事实（题目地址）应当出现在图里")
	}
	// classDef 只在有节点时写：全空图不该有一堆用不上的样式。
	if strings.Contains(Mermaid(nil), "classDef") {
		t.Error("nil 图不该写 classDef")
	}
}

// TestMermaidDeterministic：同一张图渲染两次逐字节相同。
//
// 图内部用 map 存节点，任何一处遍历 map 的写法都会让输出随运行变化——而 golden
// 测试的前提正是「内容固定则输出固定」。
func TestMermaidDeterministic(t *testing.T) {
	g := mermaidGraph(t)
	first := Mermaid(g)
	for i := 0; i < 5; i++ {
		if got := Mermaid(g); got != first {
			t.Fatalf("第 %d 次渲染与首次不一致（有 map 迭代顺序泄漏进输出）", i+2)
		}
	}
}
