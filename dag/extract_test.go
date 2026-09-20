package dag

import (
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

func toolEnd(tool, cmd, out string) harness.Event {
	return harness.Event{
		Kind: harness.EventToolEnd, Tool: tool, ToolCallID: "call_1",
		Args: map[string]any{"command": cmd}, Output: out,
	}
}

// ── B14 凭证质量闸：真实噪音语料必须全拒 ──

// 这份语料逐条取自前身 `blackboard.py` 的注释与 `_CRED_PLACEHOLDERS`——
// 前身某题攒了 61 条垃圾凭证事实，注入下一场时 actionable_assets 只取前 5 条，
// 噪音把真信号挤没。这些**必须全部被拒**。
func TestB14NoiseRejected(t *testing.T) {
	noise := []string{
		"login ==", "pass ==", "admin ==", "login: 500", "admin: 500",
		"passwd: HTTPConnectionPool(host='10.x.x.x',",
		"zzzzz", "css", "final", "enable_queue",
		// 前身注释里点名的其它漏网值
		"user: 11111", "admin: 200", "password: admin", "passwd: root",
		"user: test", "password: changeme", "admin: letmein",
	}
	for _, n := range noise {
		if CredQualityOK(n) {
			t.Errorf("B14 噪音 %q 必须被拒", n)
		}
	}
}

// 真凭证必须全过。
func TestB14RealCredsAccepted(t *testing.T) {
	good := []string{
		"admin:Admin@123", "admin:hunter2xyz", "user:john.doe",
		`password: "Sup3rS3cret!"`, "login: hunter2xyz",
		"admin: P@ssw0rd123", "root:toor1234",
	}
	for _, g := range good {
		if !CredQualityOK(g) {
			t.Errorf("真凭证 %q 应通过质量闸", g)
		}
	}
}

// 值层面的判定单独测：前身 `_cred_value_ok` 的每条规则。
func TestCredValueRules(t *testing.T) {
	cases := []struct {
		val    string
		quoted bool
		want   bool
	}{
		{"Admin@123", false, true},  // 含数字+符号
		{"hunter2xyz", false, true}, // 含数字
		{"john.doe", false, true},   // 含符号
		{"css", false, false},       // 纯字母短词
		{"final", false, false},     // 纯字母短词
		{"enable_queue", false, false},
		{"zzzzz", false, false},        // 单字符种类 ≤ 2（重复是填充的指纹）
		{"11111", false, false},        // 无字母
		{"500", false, false},          // 状态码
		{"abcdefghijkl", false, false}, // 纯小写长词 = 标识符/单词，不是凭证
		{"enable_queue", false, false}, // 下划线是标识符字符，不是「像密钥」
		{"CamelCaseKey", false, true},  // 大小写混合且够长
		{"css", true, false},           // 单字符种类 ≤ 2 的串加引号也不认
		{"supersecret", true, true},    // 加引号放宽（人为值的信号）
		{"admin", false, false},        // 占位符
		{"", false, false},
	}
	for _, c := range cases {
		if got := CredValueOK(c.val, c.quoted); got != c.want {
			t.Errorf("CredValueOK(%q, quoted=%v) = %v, want %v", c.val, c.quoted, got, c.want)
		}
	}
}

// 整条内容必须「键=值」到底——前缀匹配会让
// `passwd: HTTPConnectionPool(host='10.x.x.x',` 通过（取到 HTTPConnectionPool）。
func TestCredFullMatchRequired(t *testing.T) {
	if CredQualityOK("passwd: HTTPConnectionPool(host='10.x.x.x',") {
		t.Error("尾部有残留的内容必须被拒（全串匹配要求）")
	}
	// 允许 markdown 加粗尾部
	if !CredQualityOK("admin: Admin@123 **") {
		t.Error("尾部 markdown 加粗应被允许")
	}
}

// ── 不截断输入（前身最大的抽取事故）──

// 前身把输出切到 2000 字符再抽，副作用是**切掉了长输出里的真事实**。
// 这条测试把真事实放在 100 KB 之后——截断实现必然漏掉它。
func TestExtractDoesNotTruncateInput(t *testing.T) {
	g := newTestGraph(t)
	var b strings.Builder
	b.WriteString("starting scan...\n")
	for i := 0; i < 20000; i++ {
		b.WriteString("filler line 0123456789 abcdefghijklmnopqrstuvwxyz\n")
	}
	// 真事实在 ~1.1 MB 处
	pos := b.Len()
	b.WriteString("10.0.0.7:8443 open\n")
	b.WriteString("admin:Admin@123\n")
	out := b.String()

	nodes := g.Extract(toolEnd("bash", "nmap -p- 10.0.0.7", out), 1)
	if len(nodes) == 0 {
		t.Fatal("长输出里的事实必须被抽到（前身就是在这里丢的事实）")
	}
	var foundTarget, foundCred bool
	for _, n := range nodes {
		switch {
		case n.FactKind == FactTarget && n.Content == "10.0.0.7:8443":
			foundTarget = true
			if n.Offset < pos {
				t.Errorf("命中偏移应指向真事实处（>= %d），got %d", pos, n.Offset)
			}
			if n.OutputLen != len(out) {
				t.Errorf("OutputLen 应记录完整输出长度 %d, got %d", len(out), n.OutputLen)
			}
			if n.Fingerprint != Fingerprint(out) {
				t.Error("应记录完整输出的指纹")
			}
		case n.FactKind == FactCredential:
			foundCred = true
		}
	}
	if !foundTarget {
		t.Error("100KB 之后的 target 事实必须被抽到")
	}
	if !foundCred {
		t.Error("100KB 之后的凭证事实必须被抽到")
	}
}

// 落盘的是指纹而不是原文：图里不该出现大盘文本。
func TestExtractStoresFingerprintNotText(t *testing.T) {
	g := newTestGraph(t)
	out := "nmap scan report for 10.0.0.9\n22/tcp open ssh\n"
	nodes := g.Extract(toolEnd("bash", "nmap 10.0.0.9", out), 3)
	if len(nodes) == 0 {
		t.Fatal("应抽到事实")
	}
	fp := Fingerprint(out)
	if len(fp) != 12 {
		t.Errorf("指纹应是 sha256 前 12 位十六进制, got %q", fp)
	}
	for _, n := range nodes {
		if n.Fingerprint != fp {
			t.Errorf("节点 %s 的指纹错: %q", n.Content, n.Fingerprint)
		}
		if n.Trust != TrustHost {
			t.Errorf("宿主抽取的事实信任层应是 host-verified, got %q", n.Trust)
		}
		if n.Source == "" {
			t.Error("抽取的事实必须有来源")
		}
		if len(n.Raw) > maxRawSnippet {
			t.Errorf("Raw 取证片段不该超过 %d 字节, got %d", maxRawSnippet, len(n.Raw))
		}
	}
	// 同一份输出重放 ⇒ 同一批事实（指纹相同）
	again := g.Extract(toolEnd("bash", "nmap 10.0.0.9", out), 3)
	if len(again) != len(nodes) || again[0].Fingerprint != nodes[0].Fingerprint {
		t.Error("同一份输出重放应得到同一批事实（指纹可对账）")
	}
}

// ── 答案形状的内容不进图 ──

// 前身那条洗白路径的完整复现：agent 写 flag 到文件、再读回来。
// 抽取器必须把它丢掉（它该去 gate 的候选账本）。
func TestExtractDropsAnswerShaped(t *testing.T) {
	g := newTestGraph(t)
	out := "flag{laundered_by_myself}\n"
	if nodes := g.Extract(toolEnd("bash", "cat /tmp/f", out), 1); len(nodes) != 0 {
		t.Errorf("输出里的 flag 不得成为事实, got %+v", nodes)
	}
	// 同一个输出里，正常事实仍然要抽到
	out2 := "10.0.0.3:80 open\nflag{laundered_by_myself}\n"
	nodes := g.Extract(toolEnd("bash", "cat /tmp/f", out2), 1)
	if len(nodes) == 0 {
		t.Fatal("同一输出里的正常事实应照常抽取")
	}
	for _, n := range nodes {
		if strings.Contains(n.Content, "flag{") {
			t.Errorf("flag 内容不得入图: %q", n.Content)
		}
	}
}

// 工具调用本身失败时输出是错误信息，抽出来只会是噪音。
func TestExtractSkipsErrors(t *testing.T) {
	g := newTestGraph(t)
	ev := toolEnd("bash", "nmap 10.0.0.1", "bash: nmap: command not found")
	ev.IsError = true
	if nodes := g.Extract(ev, 1); len(nodes) != 0 {
		t.Errorf("失败的调用不该抽出事实, got %+v", nodes)
	}
}

// 非 tool_end 事件不抽。
func TestExtractOnlyToolEnd(t *testing.T) {
	g := newTestGraph(t)
	ev := toolEnd("bash", "nmap", "10.0.0.1:80 open")
	ev.Kind = harness.EventToolStart
	if nodes := g.Extract(ev, 1); len(nodes) != 0 {
		t.Error("只有 tool_end 才抽事实")
	}
}

// ── 抽取内容与边界 ──

func TestExtractServicesAndVersions(t *testing.T) {
	g := newTestGraph(t)
	out := `PORT     STATE SERVICE VERSION
80/tcp   open  http    nginx/1.18.0
22/tcp   open  ssh     OpenSSH 8.2p1 Ubuntu
3306/tcp open  mysql   MySQL 5.7.33
`
	nodes := g.Extract(toolEnd("bash", "nmap -sV 10.0.0.5", out), 1)
	got := map[string]bool{}
	for _, n := range nodes {
		if n.FactKind == FactService {
			got[n.Content] = true
		}
	}
	for _, want := range []string{"nginx/1.18.0", "openssh/8.2p1", "mysql/5.7.33"} {
		if !got[want] {
			t.Errorf("应抽到服务指纹 %q，实际 %v", want, got)
		}
	}
}

// IP 八位组必须校验：前身的正则会把 999.1.2.3 与版本号串当地址。
func TestExtractValidatesIPv4(t *testing.T) {
	g := newTestGraph(t)
	out := "999.999.999.999 bogus\n0.0.0.0 any\n10.0.0.4 real\n"
	nodes := g.Extract(toolEnd("bash", "cat hosts", out), 1)
	for _, n := range nodes {
		if n.FactKind != FactTarget {
			continue
		}
		if strings.HasPrefix(n.Content, "999.") || strings.HasPrefix(n.Content, "0.") {
			t.Errorf("非法地址不该入图: %q", n.Content)
		}
	}
}

func TestExtractPortRange(t *testing.T) {
	g := newTestGraph(t)
	out := "10.0.0.6:99999 out-of-range\n10.0.0.6:65535 ok\n"
	nodes := g.Extract(toolEnd("bash", "cat x", out), 1)
	for _, n := range nodes {
		if n.FactKind == FactTarget && strings.Contains(n.Content, "99999") {
			t.Errorf("越界端口不该入图: %q", n.Content)
		}
	}
}

// 每类事实的抽取上限：一份超大输出不该把「已知事实」段撑爆
// （前身 61 条垃圾凭证的失败模式，换成端口版本）。
func TestExtractPerKindCap(t *testing.T) {
	g := newTestGraph(t)
	var b strings.Builder
	for i := 1; i <= 300; i++ {
		b.WriteString("10.0.0." + itoa(i%256) + ":" + itoa(1000+i) + " open\n")
	}
	nodes := g.Extract(toolEnd("bash", "nmap -p-", b.String()), 1)
	targets := 0
	for _, n := range nodes {
		if n.FactKind == FactTarget {
			targets++
		}
	}
	if targets > maxExtractPerKind {
		t.Errorf("每类抽取应有上限 %d, got %d", maxExtractPerKind, targets)
	}
}

func TestExtractArtifacts(t *testing.T) {
	g := newTestGraph(t)
	out := "found /upload.php and http://10.0.0.2/admin and /var/www/html/index.php\n"
	nodes := g.Extract(toolEnd("bash", "ffuf", out), 1)
	got := map[string]bool{}
	for _, n := range nodes {
		if n.FactKind == FactArtifact {
			got[n.Content] = true
		}
	}
	if !got["/upload.php"] || !got["/var/www/html/index.php"] {
		t.Errorf("应抽到文件路径, got %v", got)
	}
	if !got["/admin"] {
		t.Errorf("应抽到 URL 路径, got %v", got)
	}
}

// 抽取结果入库后凭证归一（键小写、值保持大小写）。
func TestExtractCredentialNormalized(t *testing.T) {
	g := newTestGraph(t)
	nodes := g.Extract(toolEnd("bash", "curl -s http://t/login",
		"Admin : Admin@123\n"), 1)
	var creds []string
	for _, n := range nodes {
		if n.FactKind == FactCredential {
			creds = append(creds, n.Content)
		}
	}
	if len(creds) == 0 {
		t.Fatal("应抽到凭证")
	}
	if creds[0] != "admin:Admin@123" {
		t.Errorf("凭证应归一为 键小写:值原样, got %q", creds[0])
	}
}

// ── report_fact 通道（agent-asserted 层）──

func reportEvent(facts ...map[string]any) harness.Event {
	fs := make([]any, 0, len(facts))
	for _, f := range facts {
		fs = append(fs, f)
	}
	return harness.Event{
		Kind: harness.EventToolEnd, Tool: "report_fact", ToolCallID: "call_rf",
		Output: "已申报 1 条事实。",
		Details: map[string]any{
			"report_fact": map[string]any{"facts": fs, "next": "试试 /admin"},
		},
	}
}

func TestReportFactConfidenceCap(t *testing.T) {
	g := newTestGraph(t)
	ev := reportEvent(map[string]any{
		"kind": "service", "content": "nginx/1.18.0", "confidence": 0.99,
	})
	nodes := g.Extract(ev, 2)
	if len(nodes) != 1 {
		t.Fatalf("应抽到 1 条申报, got %d", len(nodes))
	}
	n := nodes[0]
	if n.Trust != TrustAgent {
		t.Errorf("申报的信任层应是 agent-asserted, got %q", n.Trust)
	}
	if n.Confidence > AgentConfidenceCap {
		t.Errorf("agent 申报的置信度必须封顶 %.1f, got %.2f", AgentConfidenceCap, n.Confidence)
	}
	if n.Source != "report_fact" {
		t.Errorf("来源应标 report_fact, got %q", n.Source)
	}
}

// agent 自述的 vuln/foothold 没有 evidence ⇒ 降级为低置信 artifact，
// **不是丢弃**（它的观察仍然值钱，只是不能自称已确认的漏洞）。
func TestReportFactVulnWithoutEvidenceDowngraded(t *testing.T) {
	g := newTestGraph(t)
	ev := reportEvent(map[string]any{
		"kind": "vuln", "content": "/upload 无类型校验", "confidence": 0.9,
	})
	nodes := g.Extract(ev, 2)
	if len(nodes) != 1 {
		t.Fatalf("应抽到 1 条申报, got %d", len(nodes))
	}
	n := nodes[0]
	if n.FactKind != FactArtifact {
		t.Errorf("无证据的 vuln 应降级为 artifact, got %s", n.FactKind)
	}
	if n.Confidence > 0.3 {
		t.Errorf("降级后的置信度应被压低, got %.2f", n.Confidence)
	}
	// 降级后能正常入库（artifact 不要求证据）
	if _, err := g.AddFact(n); err != nil {
		t.Errorf("降级后的 artifact 应能入库: %v", err)
	}
}

// 带 evidence 的 vuln 保持 vuln，并且能过入库闸。
func TestReportFactVulnWithEvidence(t *testing.T) {
	g := newTestGraph(t)
	ev := reportEvent(map[string]any{
		"kind": "vuln", "content": "/upload 无类型校验",
		"confidence": 0.9, "evidence": "curl -F file=@x.php /upload 返回 200 且可访问",
	})
	nodes := g.Extract(ev, 2)
	if len(nodes) != 1 || nodes[0].FactKind != FactVuln {
		t.Fatalf("带证据的 vuln 应保持 vuln, got %+v", nodes)
	}
	if _, err := g.AddFact(nodes[0]); err != nil {
		t.Errorf("带证据的 vuln 应能入库: %v", err)
	}
}

// agent 不能通过申报通道把 flag 塞进事实库（`flag` 不是 FactKind）。
func TestReportFactCannotSmuggleFlag(t *testing.T) {
	g := newTestGraph(t)
	ev := reportEvent(map[string]any{
		"kind": "flag", "content": "flag{i_made_this_up}", "confidence": 1.0,
	})
	if nodes := g.Extract(ev, 2); len(nodes) != 0 {
		t.Errorf("kind=flag 的申报必须被忽略, got %+v", nodes)
	}
	// 就算它伪装成 artifact，内容也会被答案形状判据拦下
	ev2 := reportEvent(map[string]any{
		"kind": "artifact", "content": "flag{i_made_this_up}", "confidence": 1.0,
	})
	if nodes := g.Extract(ev2, 2); len(nodes) != 0 {
		t.Errorf("伪装的 flag 内容必须被拒, got %+v", nodes)
	}
}

// negative 申报**不走** extract 通道（意图 id 是宿主概念，agent 不知道），
// 由 Scheduler.Ingest 归属后转成 AddNegative。
func TestReportFactNegativeNeedsHostAttribution(t *testing.T) {
	g := newTestGraph(t)
	ev := reportEvent(map[string]any{
		"kind": "negative", "content": "22 端口未开放，SSH 走不通",
	})
	if nodes := g.Extract(ev, 2); len(nodes) != 0 {
		t.Errorf("negative 不该由抽取器直接入图, got %+v", nodes)
	}
	// 走 Scheduler.Ingest：归属到活动意图
	s := NewScheduler(g)
	i := mustIntent(t, g, IntentRecon, "扫端口")
	s.Activate(&harness.IntentRef{ID: i, Kind: string(IntentRecon), Goal: "扫端口"})
	res := s.Ingest(ev, 2)
	if len(res.Negatives) != 1 {
		t.Fatalf("申报的 negative 应转成图上的 negative 事实, got %+v", res)
	}
	if n := g.Node(i); n.State != IntentAbandoned {
		t.Errorf("被证伪的意图应标 abandoned, got %s", n.State)
	}
}

// report_fact 载荷的解析：details 是主通道。
func TestReportPayloadFromDetails(t *testing.T) {
	ev := reportEvent(map[string]any{"kind": "target", "content": "10.0.0.9:443"})
	p, ok := ReportPayload(ev)
	if !ok || len(p.Facts) != 1 || p.Facts[0].Content != "10.0.0.9:443" {
		t.Fatalf("应从 details.report_fact 解析载荷, got %+v ok=%v", p, ok)
	}
	if p.Next == "" {
		t.Error("next 建议应被解析出来（它是 enables 分支的来源）")
	}
	// 非 report_fact 的事件没有载荷
	if _, ok := ReportPayload(toolEnd("bash", "ls", "x")); ok {
		t.Error("普通工具调用不该有 report_fact 载荷")
	}
}

// **纯申报调用**（Output 为空、只有 Details）：载荷在 Details 里，与 stdout 无关。
//
// 这是一个真实缺口的修法：原实现把「读 Details」与「正则抽 Output」一起关在
// `ev.Output == ""` 这道门后面，于是零 stdout 的申报调用里的事实被整条静默丢弃。
// 而 report_fact 恰恰最可能以纯申报形态发出（agent 说「我知道什么」不需要跑命令），
// M7 的宿主扩展一旦按纯申报实现，那条通道就是 100% 丢失——而且离线全绿看不出来。
func TestReportFactWithoutOutput(t *testing.T) {
	mkEv := func(out string) harness.Event {
		return harness.Event{
			Kind: harness.EventToolEnd, Tool: "report_fact", ToolCallID: "call_rf",
			Output: out,
			Details: map[string]any{"report_fact": map[string]any{
				"facts": []any{
					map[string]any{"kind": "service", "content": "apache/2.4.49"},
					map[string]any{"kind": "vuln", "content": "CVE-2021-41773",
						"evidence": "bash: curl 'http://10.0.0.1/cgi-bin/.%2e/etc/passwd'"},
					map[string]any{"kind": "artifact", "content": "flag{smuggled}"},
				},
				"next": "试试 /cgi-bin 的路径穿越",
			}},
		}
	}

	g := newTestGraph(t)
	nodes := g.Extract(mkEv(""), 3)
	if len(nodes) != 2 {
		t.Fatalf("空输出时申报通道仍应抽到 2 条（service + 带证据的 vuln）, got %+v", nodes)
	}
	byContent := map[string]Node{}
	for _, n := range nodes {
		byContent[n.Content] = n
		if n.Trust != TrustAgent {
			t.Errorf("申报通道的信任层应是 agent-asserted, got %q", n.Trust)
		}
		if n.Round != 3 {
			t.Errorf("轮次戳应透传, got %d", n.Round)
		}
	}
	if _, ok := byContent["apache/2.4.49"]; !ok {
		t.Error("空输出时 agent 申报的 service 事实没进图")
	}
	if v, ok := byContent["CVE-2021-41773"]; !ok || v.FactKind != FactVuln {
		t.Errorf("带证据的 vuln 应保持 vuln, got %+v", v)
	}
	// 答案形状的内容仍然不进图（空输出不是绕过答案形状闸的后门）
	if _, ok := byContent["flag{smuggled}"]; ok {
		t.Error("空输出时答案形状的申报内容仍必须被拒")
	}
	// 有输出时行为不变（两条通道都跑）
	if got := len(g.Extract(mkEv("80/tcp open http Apache/2.4.49\n"), 3)); got < 2 {
		t.Errorf("有输出时两条通道都该跑, got %d 条", got)
	}
	// 失败调用两条通道都不走（错误信息里的 JSON 不构成申报）
	bad := mkEv("")
	bad.IsError = true
	if got := g.Extract(bad, 3); len(got) != 0 {
		t.Errorf("失败的事件不该抽, got %+v", got)
	}
}

// ExtractReport 同样不该要求 Output 非空（它就是「只走申报通道」的入口）。
func TestExtractReportWithoutOutput(t *testing.T) {
	ev := reportEvent(map[string]any{"kind": "target", "content": "10.0.0.9:443"})
	ev.Output = ""
	g := newTestGraph(t)
	nodes := g.ExtractReport(ev, 5)
	if len(nodes) != 1 || nodes[0].Content != "10.0.0.9:443" {
		t.Fatalf("空输出时 ExtractReport 仍应抽到申报, got %+v", nodes)
	}
}

// ── 指纹工具 ──

func TestFlagFingerprintNoPlaintext(t *testing.T) {
	fp := FlagFingerprint("flag{secret_value}")
	if strings.Contains(fp, "secret") {
		t.Errorf("指纹绝不能包含明文: %q", fp)
	}
	if !strings.Contains(fp, "len=") || !strings.Contains(fp, "fp:") {
		t.Errorf("指纹格式应是 fp:<sha256[:8]>/len=N/首…尾, got %q", fp)
	}
	// 同一答案稳定、不同答案不同
	if FlagFingerprint("flag{a}") == FlagFingerprint("flag{b}") {
		t.Error("不同答案的指纹必须不同")
	}
	if FlagFingerprint("flag{a}") != FlagFingerprint("flag{a}") {
		t.Error("同一答案的指纹必须稳定")
	}
}

// ── 导出的小工具（gate / 态势台 / M5 轮循环会用）──

// EnvelopeHit 只判信封，不含裸串——它与 answer.Shape.Contains 是两个判定：
// Contains 服务于「从长文本里挖候选」，所以连裸串一起挖；EnvelopeHit 服务于
// 「这条内容里有没有答案」，裸串判据在 answerShaped 里另行叠加。
func TestEnvelopeHitVsContains(t *testing.T) {
	g := newTestGraph(t)
	if !g.EnvelopeHit("结果是 flag{abc}") {
		t.Error("含信封的文本应判命中")
	}
	// 裸值不算信封命中（它的判定在 answerShaped 里，且限定更严）
	if g.EnvelopeHit("just_a_raw_value_12345") {
		t.Error("EnvelopeHit 不该对裸串命中（那是 Contains 的语义）")
	}
	if !g.AnswerShaped("flag{abc}", FactArtifact) {
		t.Error("AnswerShaped 应把信封判为答案")
	}
	// 题面里的格式说明（`flag{...}`）不该被当成答案：内部是省略号、长度合规
	// 与否由 Shape 决定，这里只确认导出判据与内部一致
	if g.AnswerShaped("nginx/1.18.0", FactService) {
		t.Error("正常事实不该被判为答案")
	}
}

// ShapeOf 返回只读形态；Describe 是一行人可读描述（渲染与报告共用，
// 保证两处措辞一致——前身 B55 的教训是同一判定散落多处会漂移）。
func TestShapeOfAndDescribe(t *testing.T) {
	g := newTestGraph(t)
	if g.ShapeOf().Empty() {
		t.Error("ShapeOf 应返回题目的答案形态")
	}
	// SetShape 覆盖后生效（平台在 Start 后才给全 FlagFormat）
	g.SetShape(answer.Shape{Envelopes: []answer.Envelope{{Prefix: "CTF{", Suffix: "}"}}})
	if !g.EnvelopeHit("CTF{abc}") {
		t.Error("覆盖后的形态应立即生效")
	}
	if g.EnvelopeHit("flag{abc}") {
		t.Error("覆盖后旧形态不该再生效")
	}
	// Describe 对事实与意图都给出一行描述
	f := mustFact(t, g, FactService, "nginx/1.18.0", "bash: x")
	if got := Describe(g.Node(f)); !strings.Contains(got, "nginx/1.18.0") || !strings.Contains(got, "service") {
		t.Errorf("事实描述应含类别与内容, got %q", got)
	}
	i := mustIntent(t, g, IntentRecon, "扫端口")
	if got := Describe(g.Node(i)); !strings.Contains(got, "recon") || !strings.Contains(got, "扫端口") {
		t.Errorf("意图描述应含类别与目标, got %q", got)
	}
	if Describe(nil) != "" {
		t.Error("nil 节点的描述应是空串（渲染时不该 panic）")
	}
}

// ExtractReport 是只走申报通道的入口（调用方已有宿主抽取时用）。
func TestExtractReportOnly(t *testing.T) {
	g := newTestGraph(t)
	ev := reportEvent(map[string]any{"kind": "target", "content": "10.0.0.9:443"})
	nodes := g.ExtractReport(ev, 4)
	if len(nodes) != 1 || nodes[0].FactKind != FactTarget {
		t.Fatalf("应只抽到申报通道的事实, got %+v", nodes)
	}
	if nodes[0].Round != 4 {
		t.Errorf("轮次戳应透传, got %d", nodes[0].Round)
	}
	// 普通工具调用没有申报载荷 ⇒ 空
	if got := g.ExtractReport(toolEnd("bash", "ls", "x"), 4); len(got) != 0 {
		t.Errorf("无载荷应返回空, got %+v", got)
	}
	// 失败的事件不抽
	bad := reportEvent(map[string]any{"kind": "target", "content": "10.0.0.9:443"})
	bad.IsError = true
	if got := g.ExtractReport(bad, 4); len(got) != 0 {
		t.Errorf("失败的事件不该抽, got %+v", got)
	}
}

// Expects 报告意图是否把某类别当作预期产出（M5 用它提示 agent 换类别申报）。
func TestExpects(t *testing.T) {
	g := newTestGraph(t)
	i := mustIntent(t, g, IntentRecon, "侦察")
	n := g.Node(i)
	n.Expect = phaseExpect(IntentRecon)
	if !n.Expects(FactTarget) {
		t.Error("recon 应预期 target 事实")
	}
	if n.Expects(FactFoothold) {
		t.Error("recon 不该预期 foothold 事实")
	}
	if (&Node{}).Expects(FactTarget) {
		t.Error("空 Expect 应一律返回 false")
	}
}

// Refute 是「用一条 negative 事实证伪意图」的便捷入口，走的是同一条入库闸。
func TestRefuteConvenience(t *testing.T) {
	g := newTestGraph(t)
	i := mustIntent(t, g, IntentExploit, "试弱口令")
	if err := g.Refute(i, "admin/admin 登录返回 401"); err != nil {
		t.Fatalf("Refute 失败: %v", err)
	}
	neg := g.Negative()
	if len(neg) != 1 {
		t.Fatalf("应写入一条 negative, got %d", len(neg))
	}
	// 传了内容就用内容（渲染进 prompt 时可读），而不是 `已证伪: i1`
	if neg[0].Content != "admin/admin 登录返回 401" {
		t.Errorf("应使用调用方给的理由, got %q", neg[0].Content)
	}
	if neg[0].Refutes != i || neg[0].Source != SourcePlatform || neg[0].Trust != TrustHost {
		t.Errorf("Refute 写的 negative 字段不对: %+v", neg[0])
	}
	if g.Node(i).State != IntentAbandoned {
		t.Error("被证伪的意图应 abandoned")
	}
	// 不传内容时用兜底措辞（仍要能入图，且不能是空串）
	i2 := mustIntent(t, g, IntentExploit, "试另一条路")
	if err := g.Refute(i2, "  "); err != nil {
		t.Fatalf("空内容应走兜底: %v", err)
	}
	found := false
	for _, n := range g.Negative() {
		if n.Refutes == i2 && strings.Contains(n.Content, "已证伪") {
			found = true
		}
	}
	if !found {
		t.Error("空内容应生成可读的兜底理由")
	}
}
