package gate

import (
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

// 本文件钉住 §一 provenance 矩阵的四情形。前身 verify.py 用 3,592 行 shell/AST
// 事后分析做的事，这里靠「首现优先 + 命令文本判定」完成 —— 因为 ToolCallID 让
// 事件自带因果链，不必事后从字符串重建。
//
// 四条里第 1 条与第 4 条的区别**只在命令文本**（`echo flag{x}` vs
// `python3 solve.py`），所以它们必须成对出现：只测一条的话，把
// commandCarriesShape 写成 `strings.Contains(cmd, "flag")` 也能过 —— 而那会
// 顺手把 `curl http://t/flag` 判死。

// gateFor 造一个「题面明说 flag{...}」的 Gate。刻意不用 Infer 的兜底形态：
// 裸串形态会把命令片段当候选，混淆本文件要钉的判定。
func gateFor(t *testing.T) *Gate {
	t.Helper()
	return NewGateShape(answer.New("flag"))
}

// toolEnd 造一次完整的工具调用（start + end），与 pi 的事件顺序一致。
func toolEnd(g *Gate, callID, tool, cmd, output string) {
	g.Observe(harness.Event{Kind: harness.EventToolStart, Tool: tool, ToolCallID: callID,
		Args: map[string]any{"command": cmd}})
	g.Observe(harness.Event{Kind: harness.EventToolEnd, Tool: tool, ToolCallID: callID,
		Args: map[string]any{"command": cmd}, Output: output})
}

// provOf 返回某候选的族别。
func provOf(t *testing.T, g *Gate, flag string) Provenance {
	t.Helper()
	for _, c := range g.Candidates() {
		if c.Flag == flag {
			return c.Provenance
		}
	}
	t.Fatalf("候选 %q 不在账本里", flag)
	return ""
}

func reasonOf(t *testing.T, g *Gate, flag string) string {
	t.Helper()
	for _, c := range g.Candidates() {
		if c.Flag == flag {
			return c.RejectReason
		}
	}
	t.Fatalf("候选 %q 不在账本里", flag)
	return ""
}

// 情形 1：命令参数里就含 flag ⇒ **非观测**。
func TestMatrixEchoFlagIsNotObserved(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "c1", "bash", "echo 'flag{selfmade}' > /tmp/f", "")

	if p := provOf(t, g, "flag{selfmade}"); p != ProvenanceFabricated {
		t.Fatalf("echo 自造应为 fabricated，got %q", p)
	}
	if r := reasonOf(t, g, "flag{selfmade}"); r != ReasonCommandAuthored {
		t.Errorf("拒绝原因应为 %s，got %q", ReasonCommandAuthored, r)
	}
	if n := len(g.New()); n != 0 {
		t.Errorf("非观测候选不得进入 New()，got %d 条", n)
	}
}

// 情形 2：`curl` 输出含 flag ⇒ **观测**。
//
// 注意 `curl http://t/flag` 的命令文本里有 "flag" 这个词 —— 判据必须是
// 「有没有答案形状」而不是「有没有 flag 这个词」，否则这条会被误杀。
func TestMatrixCurlOutputIsObserved(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "c2", "bash", "curl -s http://10.0.0.1/flag", "the flag is flag{real_from_target}\n")

	if p := provOf(t, g, "flag{real_from_target}"); p != ProvenanceObserved {
		t.Fatalf("靶标响应应为 observed，got %q", p)
	}
	nw := g.New()
	if len(nw) != 1 || nw[0].Flag != "flag{real_from_target}" {
		t.Fatalf("观测族候选应进入 New()，got %v", nw)
	}
}

// 情形 2b：同一个词出现在 URL 路径里不算「含答案形状」—— 判据不能退化成
// 子串包含。这条是情形 2 的护栏。
func TestURLPathIsNotAnswerShape(t *testing.T) {
	if commandCarriesShape("curl -s http://10.0.0.1/flag", answer.New("flag")) {
		t.Error("`/flag` 路径不是答案形状，不得因此把观测判成自造")
	}
	if !commandCarriesShape("echo 'flag{x}' > /tmp/f", answer.New("flag")) {
		t.Error("`flag{x}` 是答案形状，必须被认出")
	}
}

// 情形 3：`cat FLAG`（读自己的投递文件）⇒ **非观测**。
func TestMatrixCatOwnFlagFileIsNotObserved(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "c3", "bash", "cat FLAG", "flag{my_own_guess}\n")

	if p := provOf(t, g, "flag{my_own_guess}"); p != ProvenanceFabricated {
		t.Fatalf("读回自写内容应为 fabricated，got %q", p)
	}
	if r := reasonOf(t, g, "flag{my_own_guess}"); r != ReasonSelfReadback {
		t.Errorf("拒绝原因应为 %s，got %q", ReasonSelfReadback, r)
	}
}

// 情形 3b：小写 `/flag` 是靶标端点路径，不是 agent 的状态文件。
// 前身 B37 的教训：URL 必须先摘除，否则靶标端点 `/flag` 会被当成自己的文件。
func TestTargetPathFlagIsNotOwnStateFile(t *testing.T) {
	if readsOwnState("curl -s http://10.0.0.1/flag") {
		t.Error("靶标端点 /flag 不应被当成 agent 自己的状态文件")
	}
	for _, cmd := range []string{"cat FLAG", "cat /tmp/work/MEMORY.md", "cat _blackboard.json",
		"cat tried_commands.md", "cat notes.txt", "echo x > FLAG", "cat FLAG.txt"} {
		if !readsOwnState(cmd) {
			t.Errorf("%q 应被认作读写自己的状态文件", cmd)
		}
	}
}

// TestStateFileReCINarrowed 钉死 stateFileReCI 的收窄。
//
// 缺陷：CI 变体让裸小写 `notes` / `memory` / `todolist` / `_blackboard*` /
// `tried_commands*` / `_transcripts` 也大小写不敏感匹配，于是靶标端点上**恰好
// 叫这些名字的路径**（`/notes`、`/memory`、`/flag.txt`）被误判成「agent 在读
// 自己的状态文件」⇒ provenance 记 self_readback ⇒ gate 置 locked=true ⇒
// **该候选永久不可提交**。
//
// 这正是 provenance.go 自己记录的「29 条被拒里 19 条实为正确答案」那一类：
// 误判的代价是把真观测掐掉，而掐掉之后没有任何报错。
//
// 收窄方式：CI 变体只保留 `flag(?:\.(?:txt|md|json|log))`（小写 `flag.txt`
// 确实是前身状态文件的实际名字），其余名字回到大小写敏感。
func TestStateFileReCINarrowed(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool // true = 判为读写自己的状态文件
		why  string
	}{
		// ── 必须仍然命中：真状态文件 ──
		{"cat FLAG", true, "大写 FLAG 是前身状态文件"},
		{"cat FLAG.txt", true, "带扩展名的大写 FLAG"},
		{"cat flag.txt", true, "小写 flag.txt 也是状态文件名（CI 变体保留这一项）"},
		{"cat flag.json", true, "同上，带 json 扩展名"},
		{"cat MEMORY.md", true, "大写 MEMORY.md 是前身状态文件"},
		{"cat notes.txt", true, "notes 前缀"},
		{"cat _blackboard.json", true, "前身黑板书"},
		{"cat tried_commands.md", true, "前身已试命令账本"},
		{"cat _transcripts", true, "前身 transcript"},
		{"cat todolist.md", true, "前身待办"},
		{"cat /tmp/work/SOURCE.md", true, "大写 SOURCE.md 是前身状态文件"},

		// ── 收窄后**不再**命中：小写变体 ──
		// 这是收窄的代价，也是刻意的取舍：这些名字在靶标端点上太常见
		// （`/notes`、`/memory`、`/todolist` 都是常见的题目路径），
		// 误判的后果是永久不可提交，而漏判的后果只是多走一遍首现优先。
		{"cat memory.md", false,
			"小写 memory.md 不再命中：靶标端点 /memory 太常见，误判代价高于漏判"},
		{"cat source.md", false, "小写 source.md 同理"},
		{"cat TODO.md", false, "TODO 不在名单里"},

		// ── 必须不再命中：靶标端点上恰好同名的路径 ──
		{"curl -s http://t/flag.txt", false,
			"靶标端点 /flag.txt 是真观测；判 self_readback 会让该候选永久不可提交"},
		{"curl -s http://10.0.0.1/notes", false,
			"靶标端点 /notes 是常见的题目路径"},
		{"curl -s http://10.0.0.1/memory", false,
			"靶标端点 /memory 同理"},
		{"grep -r memory /etc", false,
			"在系统目录里搜 memory 是正常侦察，不是读自己的状态文件"},
		{"curl -s http://10.0.0.1/FLAG", false,
			"靶标端点上的 /FLAG 是大写路径，不是 agent 的文件"},
		{"curl -s http://10.0.0.1/TODO", false,
			"靶标端点上的普通词"},
		{"curl -s http://t/api/blackboard", false,
			"URL 路径里的 blackboard 段不是状态文件名（前面有 /api/ 而非路径起点）"},

		// ── 边界：URL 已由调用方摘除的场景 ──
		{"curl -s http://10.0.0.1/flag", false,
			"裸 /flag 是靶标端点（已有的 TestTargetPathFlagIsNotOwnStateFile 覆盖）"},

		// ── pi 自己的会话记录与 HOME ──
		{"cat /work/.pi-sessions/*.jsonl 2>/dev/null | head -c 3000", true,
			"pi 的会话记录装着完整对话（含 harness prompt 原文），读它等于读自己的输入"},
		{"ls -la /work/.pi-sessions", true, "同上，不带 glob 的形态"},
		{"cat /work/.pi-home/config.json", true, "pi 的 HOME 同样是自己的状态"},

		// ── 边界：靶标上恰好有同名路径不能误判 ──
		{"curl -s http://10.0.0.1/.pi-sessions/list", false,
			"URL 上的同名路径不是本地会话文件（stripURLs 之后再判）"},
	}
	for _, c := range cases {
		if got := readsOwnState(c.cmd); got != c.want {
			verdict := "应判为状态文件"
			if !c.want {
				verdict = "不应判为状态文件"
			}
			t.Errorf("%q %s（%s）—— 收窄后 got=%v", c.cmd, verdict, c.why, got)
		}
	}
}

// 情形 4：`python3 solve.py` 输出含 flag ⇒ **观测**。
//
// 与情形 1 的唯一区别是命令文本 —— 所以这两条必须一起过。
func TestMatrixScriptOutputIsObserved(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "c4", "bash", "python3 solve.py", "computed: flag{computed_locally}\n")

	if p := provOf(t, g, "flag{computed_locally}"); p != ProvenanceObserved {
		t.Fatalf("脚本输出应为 observed，got %q", p)
	}
}

// 读 **pi 自己的会话文件** 得到的候选 ⇒ 不是观测族、不可提交。
//
// 回归（授权环境实测事故）：pi 把整段对话（含 harness 注入的 prompt 原文）存进
// `/work/.pi-sessions/*.jsonl`，而 prompt 里带着格式示例 `flag{...}`。agent 把那
// 个文件 `cat` 出来做侦察，gate 在输出里看到模板，判成观测族 ⇒ harness 真的把
// `flag{...}` 提交给了平台。
//
// 原实现只查「候选是否出现在**命令行**里」（`commandAuthoredLocked`），而这里的
// 候选来自**被读文件的内容**，命令行里并没有它——所以漏网。会话文件与 FLAG /
// MEMORY / notes 一样是「自己的状态」，读它的输出不构成靶标证据。
func TestPiSessionCatIsNotGrounding(t *testing.T) {
	g := gateFor(t)
	const cmd = `cat /work/.pi-sessions/*.jsonl 2>/dev/null | head -c 3000`
	out := `{"type":"session","version":3,"text":"答案格式以题面为准——题目要求 ` +
		`flag{...} 就写 flag{...}，不要自己加外壳"}`

	toolEnd(g, "c-pi", "bash", cmd, out)

	if p := provOf(t, g, "flag{...}"); p == ProvenanceObserved {
		t.Fatalf("自读 pi 会话文件得到的候选不得是观测族，got %q", p)
	}
	for _, c := range g.NewAll() {
		if c.Flag == "flag{...}" {
			t.Fatal("自读会话文件得到的候选必须不可提交（NewAll 不该返回它）")
		}
	}
}

// 对照组：同一个候选串出现在**靶标产出**里仍然要能坐实观测族。
//
// 没有这一条的话，上面那条断言可以被「把所有输出都判成自读」这种粗暴实现蒙过去
// —— 而那会掐掉「先猜后验」这条真实解题路径。
func TestTargetOutputStillGroundsSameCandidate(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "c-target", "bash", "curl -s http://10.0.0.1/flag.txt", "flag{from_target}\n")

	if p := provOf(t, g, "flag{from_target}"); p != ProvenanceObserved {
		t.Fatalf("靶标产出应为 observed，got %q", p)
	}
}

// 洗白路径（前身那条致命事故）：写文件再读回来。
//
// `echo 'flag{x}' > /tmp/f`（无输出）→ `cat /tmp/f`（输出里有 flag）。
// 首现优先必须把族别钉在写的那一刻。注意 echo 那条命令**没有输出** ——
// 只在「有输出」时判定的实现会漏掉它，候选要等到 cat 才被记账，那时它已经
// 出现在工具输出里，看起来完全像观测。
func TestLaunderingViaWriteThenReadIsCaught(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "w1", "bash", "echo 'flag{laundered}' > /tmp/f", "")
	toolEnd(g, "r1", "bash", "cat /tmp/f", "flag{laundered}\n")

	if p := provOf(t, g, "flag{laundered}"); p != ProvenanceFabricated {
		t.Fatalf("洗白路径必须被判 fabricated，got %q", p)
	}
	if n := len(g.New()); n != 0 {
		t.Errorf("洗白候选不得进入 New()，got %d 条", n)
	}
	// 出处应指向读回它的那次调用 —— 取证链要说得清它是怎么回来的。
	for _, c := range g.Candidates() {
		if c.Flag == "flag{laundered}" && c.ToolCallID != "r1" {
			t.Errorf("出处应更新为读回它的工具调用 r1，got %q", c.ToolCallID)
		}
	}
}

// 洗白的另一条路径：写入命令自带输出（`tee`）。命令里有形状 ⇒ 非观测，
// 且输出里同样出现 ⇒ 不得因此升族。
func TestLaunderingViaTeeIsCaught(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "t1", "bash", "echo 'flag{tee_laundered}' | tee /tmp/f", "flag{tee_laundered}\n")
	if p := provOf(t, g, "flag{tee_laundered}"); p != ProvenanceFabricated {
		t.Fatalf("tee 洗白必须被判 fabricated，got %q", p)
	}
}

// 洗白的第三条路径：写进脚本再执行。
// 前身 B40：脚本体（heredoc 载荷）是 agent 的代码，不是命令参数 ——
// `python3 - <<'EOF'` 里出现候选，同样是自造。
func TestLaunderingViaHeredocScriptIsCaught(t *testing.T) {
	g := gateFor(t)
	cmd := "python3 - <<'EOF'\nprint('flag{from_heredoc}')\nEOF"
	toolEnd(g, "h1", "bash", cmd, "flag{from_heredoc}\n")

	if p := provOf(t, g, "flag{from_heredoc}"); p != ProvenanceFabricated {
		t.Fatalf("heredoc 自造必须被判 fabricated，got %q", p)
	}
}

// 洗白的第四条路径：**不经过形状标记**的写入。
//
// `echo 'hunter2xyz' > /tmp/f` 的命令文本里没有 `{`，所以 commandCarriesShape
// 看不见它；这条路径只能靠物化匹配（候选的完整字面值出现在命令里）挡住。
// 它必须单独钉一条，否则「只挡信封形态」的实现也能通过前面三条洗白测试。
func TestLaunderingWithoutShapeMarkerIsCaught(t *testing.T) {
	g := NewGateShape(answer.Shape{AllowRaw: true})
	toolEnd(g, "w", "bash", "echo 'hunter2xyz' > /tmp/f", "")
	toolEnd(g, "r", "bash", "cat /tmp/f", "hunter2xyz\n")

	if p := provOf(t, g, "hunter2xyz"); p != ProvenanceFabricated {
		t.Fatalf("无形状标记的自造（写入时就把候选物化进命令）必须判 fabricated，got %q", p)
	}
	if n := len(g.New()); n != 0 {
		t.Errorf("洗白候选不得提交，got %d 条", n)
	}
}

// 对称的反例：同一条命令文本、同样的输出，但命令里**没有**候选 —— 那是真观测。
// 这两条必须同时过：只有前者的话，一个「凡输出命中就判自造」的实现也能过。
func TestRawFormOutputWithoutMaterializationIsObserved(t *testing.T) {
	g := NewGateShape(answer.Shape{AllowRaw: true})
	toolEnd(g, "o", "bash", "curl -s http://10.0.0.1/login", "password=Admin@123\n")

	if p := provOf(t, g, "password=Admin@123"); p != ProvenanceObserved {
		t.Fatalf("靶标响应里的裸串凭证应为 observed，got %q", p)
	}
	if n := len(g.New()); n != 1 {
		t.Errorf("观测族裸串凭证应可提交，got %d 条", n)
	}
}

// 裸串候选的输出片段要裁剪：前身实测某题攒了 61 条垃圾凭证把真信号挤没，
// 而裸串候选在真实数据里绝大多数是命令片段/URL，整段输出会被记进候选。
// 信封候选是答案主体形态，一律完整保留。
func TestRawCandidateOutputIsClipped(t *testing.T) {
	g := NewGateShape(answer.Shape{AllowRaw: true})
	long := "password=Admin@123 " + strings.Repeat("filler ", 100)
	toolEnd(g, "c", "bash", "curl -s http://10.0.0.1/login", long)
	for _, c := range g.Candidates() {
		if c.Flag != "password=Admin@123" {
			continue
		}
		if len(c.Output) > 200 {
			t.Errorf("裸串候选的输出片段应被裁剪，got %d 字节", len(c.Output))
		}
	}
	// 信封候选不裁剪
	g2 := gateFor(t)
	toolEnd(g2, "e", "bash", "curl -s http://t/", strings.Repeat("x", 400)+" flag{kept}\n")
	for _, c := range g2.Candidates() {
		if c.Flag == "flag{kept}" && !strings.Contains(c.Output, "flag{kept}") {
			t.Errorf("信封候选的输出片段应完整保留，got %q", c.Output)
		}
	}
}

// 命令引号载荷里的**地址/路径**不得进账本：`nmap -sV '10.0.0.1'`、
// `cat '/etc/passwd'` 的引号内容恰好通过形状真源的裸串判定（「宁可多认」是
// 为了不漏答案），但把它们记成幻觉族候选就是前身 B14 那条老路 —— 而幻觉族
// 的计数是要参与阈值判断的。
func TestCommandPayloadAddressIsNotCandidate(t *testing.T) {
	g := NewGateShape(answer.Shape{AllowRaw: true})
	for _, cmd := range []string{"nmap -sV '10.0.0.1'", "cat '/etc/passwd'", "grep -rn 'password' /var/www"} {
		toolEnd(g, cmd, "bash", cmd, "")
	}
	if n := len(g.Candidates()); n != 0 {
		t.Errorf("地址/路径形态不应进候选账本，got %+v", g.Candidates())
	}
	if st := g.Stats(); st.Fabricated != 0 {
		t.Errorf("不应产生幻觉族计数（它会参与阈值判断）: %+v", st)
	}
}

// 编码物化：命令里没有 `flag{` 字面标记，但 base64 里就是答案。
// 前身专门为它写过 _candidate_materializations —— 只按形状标记判定会漏掉。
func TestMaterializedBase64InCommandIsCaught(t *testing.T) {
	g := gateFor(t)
	// 先让候选被账本认识（散文里出现），再让命令把它物化。
	g.Observe(harness.Event{Kind: harness.EventText, Text: "I think the answer is flag{encoded_secret}"})
	b64 := base64Std("flag{encoded_secret}")
	toolEnd(g, "b1", "bash", "python3 -c \"print(__import__('base64').b64decode('"+b64+"'))\"", "")

	if p := provOf(t, g, "flag{encoded_secret}"); p != ProvenanceFabricated {
		t.Fatalf("编码物化的候选必须被判 fabricated，got %q", p)
	}
}

// 反例：编码物化判定不能把「命令里恰好有很短的相同串」当自造。
func TestMaterializationIgnoresShortVariants(t *testing.T) {
	for _, v := range materializations("flag{x}") {
		if len(v) < 8 {
			t.Errorf("物化变体 %q 短于 8，会带来大量假阳性", v)
		}
	}
}

// 散文里出现的候选 ⇒ 只记账、不可提交（前身：LLM 幻觉被自己的文本 grounded 化）。
func TestProseCandidateIsNotSubmittable(t *testing.T) {
	g := gateFor(t)
	g.Observe(harness.Event{Kind: harness.EventText, Text: "maybe the flag is flag{in_my_head}"})

	if p := provOf(t, g, "flag{in_my_head}"); p != ProvenanceFabricated {
		t.Fatalf("散文候选应为 fabricated，got %q", p)
	}
	if n := len(g.New()); n != 0 {
		t.Errorf("散文候选不得进入 New()，got %d 条", n)
	}
}

// 先猜后验（逆向/密码题的正解路径）：散文里先出现，之后靶标产物里真的出现。
// 前身影子审计「29 条被拒候选里 19 条实为正确答案」几乎全在这条路径上，
// 所以它必须能升族 —— 这是对推导族/幻觉族「永不干预」原则的落实。
func TestProseGuessLaterGroundedIsObserved(t *testing.T) {
	g := gateFor(t)
	g.Observe(harness.Event{Kind: harness.EventText, Text: "I suspect the flag is flag{guessed_then_proven}"})
	toolEnd(g, "g1", "bash", "cat output.bin", "decoded payload: flag{guessed_then_proven}\n")

	if p := provOf(t, g, "flag{guessed_then_proven}"); p != ProvenanceObserved {
		t.Fatalf("先猜后验应升为 observed，got %q", p)
	}
	if n := len(g.New()); n != 1 {
		t.Errorf("升族后应进入 New()，got %d 条", n)
	}
}

// 升族通道**不能**被洗白利用：散文猜测 → 自己写文件读回来，仍然是幻觉。
// 这是上面那条的边界 —— 两条必须同时过，否则升族就是个漏洞。
func TestProseGuessThenSelfReadbackStaysFabricated(t *testing.T) {
	g := gateFor(t)
	g.Observe(harness.Event{Kind: harness.EventText, Text: "I suspect flag{guessed_then_laundered}"})
	toolEnd(g, "w2", "bash", "echo 'flag{guessed_then_laundered}' > /tmp/f", "")
	toolEnd(g, "r2", "bash", "cat /tmp/f", "flag{guessed_then_laundered}\n")

	if p := provOf(t, g, "flag{guessed_then_laundered}"); p != ProvenanceFabricated {
		t.Fatalf("自读回不得升族，got %q", p)
	}
}

// 首现优先的另一个方向：观测在前，之后 agent 在散文里重复它 ⇒ 仍是观测。
func TestObservedThenProseStaysObserved(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "o1", "bash", "curl -s http://t/", "flag{first_observed}\n")
	g.Observe(harness.Event{Kind: harness.EventText, Text: "great, flag{first_observed} works"})

	if p := provOf(t, g, "flag{first_observed}"); p != ProvenanceObserved {
		t.Fatalf("首现于观测的候选不得被散文降级，got %q", p)
	}
}

// 同一 flag 只产出一个候选。
func TestDedupSingleCandidate(t *testing.T) {
	g := gateFor(t)
	for i, id := range []string{"a", "b", "c"} {
		toolEnd(g, id, "bash", "curl -s http://t/", "flag{only_once}\n")
		_ = i
	}
	seen := 0
	for _, c := range g.Candidates() {
		if c.Flag == "flag{only_once}" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("同一 flag 应只产出一个候选，got %d", seen)
	}
	if st := g.Stats(); st.Observed != 1 {
		t.Errorf("重复观测不应重复计数，Observed=%d", st.Observed)
	}
}

// 记录当前工具调用 id（诊断用）。
func TestCandidateCarriesToolCallID(t *testing.T) {
	g := gateFor(t)
	g.SetIntent("intent-7", 3)
	toolEnd(g, "call-42", "bash", "curl -s http://t/", "flag{with_anchor}\n")

	for _, c := range g.Candidates() {
		if c.Flag != "flag{with_anchor}" {
			continue
		}
		if c.ToolCallID != "call-42" {
			t.Errorf("ToolCallID 应为 call-42，got %q", c.ToolCallID)
		}
		if c.IntentID != "intent-7" || c.Round != 3 {
			t.Errorf("意图/轮次未回填: %+v", c)
		}
	}
}

// 指纹不含明文 —— 前身 flag 明文泄漏进持久文件的事故。
//
// 格式已统一到 `answer.Fingerprint`（唯一真源）：`fp:<hex8>/len=N/<首>…<尾>`。
// 统一之前 gate 与 dag 各有一份实现、格式还不同，跨包对照（判错账本 ↔ prompt
// 回灌）**字符串永远不匹配**，报告层无法把两边对上。
func TestFingerprintHidesPlaintext(t *testing.T) {
	const flag = "flag{super_secret_value}"
	fp := Fingerprint(flag)
	if strings.Contains(fp, flag) || strings.Contains(fp, "super_secret_value") {
		t.Fatalf("指纹泄漏明文: %q", fp)
	}
	// 指纹要能对上号：fp: + sha256[:8] + 长度 + 首尾
	parts := strings.Split(fp, "/")
	if len(parts) != 3 {
		t.Fatalf("指纹格式应为 fp:<hex8>/len=N/<首>…<尾>，got %q", fp)
	}
	if !strings.HasPrefix(parts[0], "fp:") || len(parts[0]) != 11 {
		t.Errorf("前缀应为 fp: + 8 位哈希，got %q", parts[0])
	}
	if parts[1] != "len=24" {
		t.Errorf("长度应为 24（按字符数），got %q", parts[1])
	}
	if parts[2] != "f…}" {
		t.Errorf("首尾应为 f…}，got %q", parts[2])
	}
	// 不同答案的指纹必须不同
	if Fingerprint(flag) == Fingerprint("flag{other_value}") {
		t.Error("不同答案的指纹不应相同")
	}
	// 与唯一真源一致：转发不应引入偏差
	if fp != answer.Fingerprint(flag) {
		t.Errorf("gate.Fingerprint 应等于 answer.Fingerprint，got %q vs %q", fp, answer.Fingerprint(flag))
	}
}

// 长度按字符数而非字节数：非 ASCII 答案不能被切出半个字符。
func TestFingerprintCountsRunes(t *testing.T) {
	fp := Fingerprint("密码abc")
	if !strings.HasSuffix(fp, "/len=5/密…c") {
		t.Errorf("非 ASCII 答案的指纹应按字符数计，got %q", fp)
	}
}

// 判错账本：Has / Record / Fingerprints，且指纹不含明文。
func TestLedgerRecordsWithoutPlaintext(t *testing.T) {
	l := NewLedger()
	const flag = "flag{rejected_by_platform}"
	l.Record(flag, "platform_rejected")

	if !l.Has(flag) {
		t.Error("Has 应报告已判错")
	}
	if l.Has("flag{never_seen}") {
		t.Error("未判错的答案不应被 Has 命中")
	}
	fps := l.Fingerprints()
	if len(fps) != 1 {
		t.Fatalf("应有 1 条指纹，got %v", fps)
	}
	if strings.Contains(fps[0], flag) || strings.Contains(fps[0], "rejected_by_platform") {
		t.Fatalf("账本指纹泄漏明文: %q", fps[0])
	}
	if l.Reasons()[fps[0]] != "platform_rejected" {
		t.Errorf("原因未记录: %v", l.Reasons())
	}
	// 重复记录不产生重复指纹
	l.Record(flag, "platform_rejected")
	if len(l.Fingerprints()) != 1 {
		t.Errorf("重复记录应去重，got %v", l.Fingerprints())
	}
}

// Ledger 必须满足根包契约接口（编译期断言）。
var _ harness.RejectedLedger = (*Ledger)(nil)

// Gate 必须满足根包契约接口。
var _ harness.CandidateGate = (*Gate)(nil)

// ── 前身洗白路径的完整回放（端到端，事件序与 pi 一致）──

// 完整回放：agent 编一个 flag 写进文件、读回来、再当成参数回喂校验器。
// 三种事件序混在一起，族别必须在**第一步**就钉死。
func TestFullLaunderingReplay(t *testing.T) {
	g := gateFor(t)
	// 1) agent 在思考里编了一个
	g.Observe(harness.Event{Kind: harness.EventThinking, Text: "let me try flag{my_guess}"})
	// 2) 写进文件（无输出）
	toolEnd(g, "s1", "bash", "printf 'flag{my_guess}' > /tmp/f", "")
	// 3) 读回来（输出里有）
	toolEnd(g, "s2", "bash", "cat /tmp/f", "flag{my_guess}\n")
	// 4) 回喂给本地校验器
	toolEnd(g, "s3", "bash", "python3 check.py 'flag{my_guess}'", "INVALID\n")
	// 5) agent 在总结里又写了一遍
	g.Observe(harness.Event{Kind: harness.EventText, Text: "so the flag is flag{my_guess}"})

	if p := provOf(t, g, "flag{my_guess}"); p != ProvenanceFabricated {
		t.Fatalf("整条洗白链走完仍必须是 fabricated，got %q", p)
	}
	if n := len(g.New()); n != 0 {
		t.Errorf("洗白候选一次都不得提交，got %d 条", n)
	}
	if st := g.Stats(); st.Fabricated != 1 {
		t.Errorf("应只记 1 条幻觉族（不得因重复出现重复计数）: %+v", st)
	}
}

// 与上一条对称：真正的观测路径走完必须能提交。
// 这两条必须同时过 —— 只测一条的话，一个「永远返回非观测」的实现也能过。
func TestFullObservationReplay(t *testing.T) {
	g := gateFor(t)
	// 1) agent 探测服务
	toolEnd(g, "s1", "bash", "nmap -sV 10.0.0.1", "80/tcp open http\n")
	// 2) 打端点，靶标返回 flag
	toolEnd(g, "s2", "bash", "curl -s http://10.0.0.1/admin/flag", "{\"flag\":\"flag{from_target}\"}\n")
	// 3) agent 在总结里复述
	g.Observe(harness.Event{Kind: harness.EventText, Text: "got flag{from_target}"})
	// 4) 再打一次同样的端点
	toolEnd(g, "s3", "bash", "curl -s http://10.0.0.1/admin/flag", "{\"flag\":\"flag{from_target}\"}\n")

	if p := provOf(t, g, "flag{from_target}"); p != ProvenanceObserved {
		t.Fatalf("真观测链必须是 observed，got %q", p)
	}
	if n := len(g.New()); n != 1 {
		t.Fatalf("真观测应产出 1 条可提交候选，got %d", n)
	}
}
