package gate

import (
	"errors"
	"strings"
	"sync"
	"testing"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

// ── 三闸 ──

// 格式闸：连答案形态都不像的串不进候选账本。
//
// 前身 B14 的真实事故：凭证正则把 HTML 表单字段、状态码、SQLi payload 残片
// 统统当凭证，某题攒了 61 条垃圾事实，注入下一场时把真信号挤没。gate 这一层
// 必须在**候选抽取阶段**就挡住它们 —— 否则噪音会一路走到提交队列。
func TestFormatGateRejectsNoise(t *testing.T) {
	g := NewGate("提交 flag{...} 与管理员密码")
	for _, n := range []string{
		"login ==", "pass ==", "admin ==", "login: 500", "admin: 500",
		"zzzzz", "css", "final", "enable_queue",
	} {
		toolEnd(g, "n-"+n, "bash", "cat config.php", n+"\n")
		for _, c := range g.Candidates() {
			if strings.Contains(c.Flag, strings.TrimSpace(n)) {
				t.Errorf("噪音 %q 不应进入候选账本（got %q，族别 %s）", n, c.Flag, c.Provenance)
			}
		}
	}
	if n := len(g.New()); n != 0 {
		t.Errorf("噪音不得产生可提交候选，got %d 条", n)
	}
}

// B14 噪音里唯一「长得像答案」的那一条：`passwd: HTTPConnectionPool(host='10.x.x.x',`。
//
// answer 包按裸串形态判定时，会从这行里挖出 `HTTPConnectionPool`（形似 camelCase
// 密钥）与 `10.x.x.x`（形似主机名）—— 那是形状真源「宁可多认不可漏认」的正常
// 行为。gate 这一层要挡住的是**整行**：它是一行表单噪音，不是凭证。
func TestFormatGateRejectsConnectionPoolLine(t *testing.T) {
	const line = "passwd: HTTPConnectionPool(host='10.x.x.x',"
	g := NewGate("提交管理员密码")
	toolEnd(g, "n-pool", "bash", "curl -s http://10.0.0.1/login", line+"\n")

	for _, c := range g.Candidates() {
		if strings.Contains(c.Flag, "HTTPConnectionPool") || strings.Contains(c.Flag, "10.x.x.x") {
			t.Errorf("B14 噪音行里的片段不应成为候选: %q", c.Flag)
		}
	}
}

// 真凭证必须全过（观测族，可提交）。
func TestFormatGateAcceptsRealCreds(t *testing.T) {
	for _, cred := range []string{"admin:Admin@123", "admin:hunter2xyz", "john.doe"} {
		g := NewGate("请提交管理员密码")
		toolEnd(g, "c-"+cred, "bash", "curl -s http://10.0.0.1/login", cred+"\n")
		if n := len(g.New()); n == 0 {
			t.Errorf("真凭证 %q 应产出可提交候选", cred)
		}
	}
}

// 来源闸：只有观测族可提交。推导族与幻觉族只记账。
func TestSourceGateOnlyObservedIsSubmittable(t *testing.T) {
	g := gateFor(t)
	// 幻觉族（散文）
	g.Observe(harness.Event{Kind: harness.EventText, Text: "flag{fab}"})
	// 观测族（靶标响应）
	toolEnd(g, "ok", "bash", "curl -s http://t/", "flag{obs}\n")

	new := g.New()
	if len(new) != 1 || new[0].Flag != "flag{obs}" {
		t.Fatalf("New() 只应含观测族候选，got %v", new)
	}
	// 全部候选都在账本里（含不可提交的）—— 报告要能解释每一条。
	if len(g.Candidates()) != 2 {
		t.Errorf("账本应含全部候选，got %d", len(g.Candidates()))
	}
}

// 分族统计：幻觉族与推导族**分列**，且只有幻觉族有访问器。
//
// 前身 hallucination.py 的第一安全性质：推导族永不计数、永不触发任何干预
// —— 前身影子审计发现「29 条被拒候选里 19 条实为正确答案」，几乎全落在推导族。
func TestStatsSeparateFabricatedFromDerived(t *testing.T) {
	g := gateFor(t)
	// 幻觉族两条：命令自造 + 散文
	toolEnd(g, "f1", "bash", "echo 'flag{fab_one}' > /tmp/f", "")
	g.Observe(harness.Event{Kind: harness.EventText, Text: "flag{fab_two}"})
	// 推导族：先猜、后由散文再确认一次，但**从未**在工具输出里坐实
	g.Observe(harness.Event{Kind: harness.EventText, Text: "maybe flag{derived_one}"})
	g.Observe(harness.Event{Kind: harness.EventText, Text: "so flag{derived_one} it is"})

	st := g.Stats()
	if st.Fabricated != 2 {
		t.Errorf("幻觉族应为 2，got %d", st.Fabricated)
	}
	if st.Derived != 1 {
		t.Errorf("推导族应为 1，got %d", st.Derived)
	}
	if st.Observed != 0 {
		t.Errorf("不应有观测族，got %d", st.Observed)
	}
	// 推导族不参与任何干预：Fabrications() 里不得出现它。
	for _, c := range g.Fabrications() {
		if c.Flag == "flag{derived_one}" {
			t.Error("推导族不得进入幻觉族视图（否则会被当幻觉杀，等于掐掉真答案）")
		}
	}
	// 推导族永不可提交。
	if n := len(g.New()); n != 0 {
		t.Errorf("推导族不得提交，got %d 条", n)
	}
}

// ── 去重与提交回填 ──

// 同一 flag 只产出一个候选；Mark 后 New() 不再返回它。
//
// 这是修掉「每次 tool_end 重遍历全部候选重复提交」那个缺陷的核心断言：
// 根包的 harvest 靠 New() 决定提交什么，Mark 之后必须不再返回。
func TestMarkRemovesFromNew(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "a", "bash", "curl -s http://t/", "flag{submit_once}\n")
	toolEnd(g, "b", "bash", "curl -s http://t/again", "flag{submit_once}\n")

	if n := len(g.New()); n != 1 {
		t.Fatalf("同一 flag 应只产出一个候选，got %d", n)
	}
	g.Mark("flag{submit_once}", harness.Evaluation{Accepted: true, Progress: true}, nil)

	if n := len(g.New()); n != 0 {
		t.Fatalf("Mark 后 New() 不得再返回它，got %d 条", n)
	}
	st := g.Stats()
	if st.Submitted != 1 || st.Correct != 1 {
		t.Errorf("回填统计不对: %+v", st)
	}
	// 重复 Mark 不应重复计数（harvest 在每轮末都会调一次）。
	g.Mark("flag{submit_once}", harness.Evaluation{Accepted: true, Progress: true}, nil)
	if st := g.Stats(); st.Submitted != 1 {
		t.Errorf("重复 Mark 不应重复计数: %+v", st)
	}
}

// 平台幂等命中（Duplicate）等价于已确认 —— 前身契约里这里统计错过。
func TestMarkDuplicateCountsAsCorrect(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "a", "bash", "curl -s http://t/", "flag{dup}\n")
	g.Mark("flag{dup}", harness.Evaluation{Accepted: true}, nil)

	for _, c := range g.Candidates() {
		if c.Flag == "flag{dup}" {
			if !c.Correct || !c.Duplicate {
				t.Errorf("幂等命中应记为已确认: %+v", c)
			}
		}
	}
}

// 提交出错也置 Submitted：同一答案永不重提是硬规矩（重试的收益远小于打光配额）。
func TestMarkErrorStillMarksSubmitted(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "a", "bash", "curl -s http://t/", "flag{net_fail}\n")
	g.Mark("flag{net_fail}", harness.Evaluation{}, errors.New("connection reset"))

	if n := len(g.New()); n != 0 {
		t.Errorf("提交出错后不得重提，got %d 条", n)
	}
	if st := g.Stats(); st.Rejected != 0 {
		t.Errorf("提交出错不应计入判错（可能是网络问题）: %+v", st)
	}
}

// 判错计入统计，并且候选带原因（报告的可解释性依赖它）。
func TestMarkRejectedRecordsReason(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "a", "bash", "curl -s http://t/", "flag{wrong_guess}\n")
	g.Mark("flag{wrong_guess}", harness.Evaluation{}, nil)

	st := g.Stats()
	if st.Rejected != 1 || st.Correct != 0 {
		t.Errorf("判错统计不对: %+v", st)
	}
	// 平台判错记在 SubmitError（平台侧结果），**不是** RejectReason
	// （族别归因）—— 两者回答的是不同问题，报告要同时展示。
	for _, c := range g.Candidates() {
		if c.Flag == "flag{wrong_guess}" && c.SubmitError != "platform_rejected" {
			t.Errorf("平台判错原因未记入 SubmitError: %+v", c)
		}
	}
}

// 未提交过的候选不会被 Mark 影响（Mark 一个不存在的 flag 不应 panic）。
func TestMarkUnknownFlagIsNoop(t *testing.T) {
	g := gateFor(t)
	g.Mark("flag{never_seen}", harness.Evaluation{Accepted: true, Progress: true}, nil)
	if st := g.Stats(); st.Submitted != 0 {
		t.Errorf("Mark 未知 flag 应为 no-op: %+v", st)
	}
}

// ── 记账模块的健壮性（前身第三安全性质：异常一律吞掉）──

func TestObserveSwallowsPanics(t *testing.T) {
	g := gateFor(t)
	// 畸形事件：Args 里塞了非字符串、Output 里塞了超长文本、Kind 未知。
	// 记账模块绝不能因为自身故障把解题主流程拖挂（前身第三安全性质）。
	g.Observe(harness.Event{Kind: harness.EventKind("bogus"), Args: map[string]any{"command": 42}})
	g.Observe(harness.Event{Kind: harness.EventToolEnd, Args: map[string]any{"command": nil},
		Output: strings.Repeat("x", 10000)})
	g.Observe(harness.Event{Kind: harness.EventToolStart, Args: nil})
	g.Observe(harness.Event{Kind: harness.EventToolEnd, Output: "flag{unterminated",
		Args: map[string]any{"command": strings.Repeat("a{", 5000)}})
	// 走到这里就说明没 panic；账本仍可用。
	if st := g.Stats(); st.Observed+st.Derived+st.Fabricated < 0 {
		t.Fatal("不可达")
	}
}

func TestNilGateIsSafe(t *testing.T) {
	var g *Gate
	g.Observe(harness.Event{Kind: harness.EventToolEnd, Output: "flag{x}"})
	if n := len(g.New()); n != 0 {
		t.Errorf("nil Gate 的 New() 应为空")
	}
	g.Mark("flag{x}", harness.Evaluation{}, nil)
	if st := g.Stats(); st.Observed != 0 {
		t.Errorf("nil Gate 的 Stats() 应为零值")
	}
	var l *Ledger
	l.Record("flag{x}", "r")
	if l.Has("flag{x}") {
		t.Error("nil Ledger 的 Has 应为 false")
	}
	if len(l.Fingerprints()) != 0 {
		t.Error("nil Ledger 的 Fingerprints 应为空")
	}
}

// 并发：Observe 从 reader 协程来、harvest 在主循环里调 New/Mark。
// 这不是理论风险 —— 前身那套「事件回调里顺手提交」的写法一旦并行就是数据竞争。
// 用 -race 跑这个测试才有意义。
func TestConcurrentObserveAndHarvest(t *testing.T) {
	g := gateFor(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				toolEnd(g, "id", "bash", "curl -s http://t/", "flag{concurrent}\n")
				_ = g.New()
				_ = g.Candidates()
				_ = g.Stats()
				g.Mark("flag{concurrent}", harness.Evaluation{Accepted: true, Progress: true}, nil)
			}
		}(i)
	}
	wg.Wait()
	if st := g.Stats(); st.Observed != 1 || st.Submitted != 1 {
		t.Errorf("并发下统计应仍为 1/1: %+v", st)
	}
}

// ── 首现优先的记账口径 ──

// 首现于散文的候选：即便之后被同一段散文重复，也**不**因重复而升族。
func TestProseRepetitionDoesNotPromote(t *testing.T) {
	g := gateFor(t)
	for i := 0; i < 5; i++ {
		g.Observe(harness.Event{Kind: harness.EventText, Text: "flag{repeated_in_prose}"})
	}
	if p := provOf(t, g, "flag{repeated_in_prose}"); p == ProvenanceObserved {
		t.Fatal("散文里重复出现不得升为观测族（前身：LLM 幻觉被自己的文本 grounded 化）")
	}
	if n := len(g.New()); n != 0 {
		t.Errorf("散文候选不得提交，got %d 条", n)
	}
}

// SetProvenance 只允许单向升族 —— 降级永远被拒。
//
// 前身的 fail-closed 事故（29 条被拒里 19 条是真答案）就是降级思维的直接代价：
// 一个候选一旦被观测坐实，任何后续事件都不能把它打回幻觉族。
func TestSetProvenanceIsMonotonic(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "o", "bash", "curl -s http://t/", "flag{sticky}\n")
	if p := provOf(t, g, "flag{sticky}"); p != ProvenanceObserved {
		t.Fatalf("应为 observed，got %q", p)
	}
	g.SetProvenance("flag{sticky}", ProvenanceFabricated)
	if p := provOf(t, g, "flag{sticky}"); p != ProvenanceObserved {
		t.Errorf("观测族不得被降级，got %q", p)
	}
	// 升级方向可用（dag 包在事实层独立判定后回填时走这里）。
	g.SetProvenance("flag{sticky}", ProvenanceObserved)
	if st := g.Stats(); st.Observed != 1 {
		t.Errorf("重复升级不应重复计数: %+v", st)
	}
}

// 已提交的候选不得被改判（提交结果已经出去了，改族别只会让报告自相矛盾）。
func TestSetProvenanceIgnoresSubmitted(t *testing.T) {
	g := gateFor(t)
	g.Observe(harness.Event{Kind: harness.EventText, Text: "flag{submitted_first}"})
	g.Mark("flag{submitted_first}", harness.Evaluation{Accepted: true, Progress: true}, nil)
	g.SetProvenance("flag{submitted_first}", ProvenanceObserved)
	if p := provOf(t, g, "flag{submitted_first}"); p != ProvenanceFabricated {
		t.Errorf("已提交候选不得改判，got %q", p)
	}
}

// Candidates 保序：报告与续跑依赖稳定顺序。
func TestCandidatesKeepInsertionOrder(t *testing.T) {
	g := gateFor(t)
	for _, f := range []string{"flag{one}", "flag{two}", "flag{three}"} {
		g.Observe(harness.Event{Kind: harness.EventText, Text: f})
	}
	var got []string
	for _, c := range g.Candidates() {
		got = append(got, c.Flag)
	}
	want := []string{"flag{one}", "flag{two}", "flag{three}"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序不稳定: got %v, want %v", got, want)
		}
	}
}

// 写文件类工具（path/content）与 echo 自造同源 —— 不能因为「没有 command 字段」
// 就漏判。前身 B40 记录过这类工具调用。
func TestWriteFileToolIsTreatedAsAuthored(t *testing.T) {
	g := gateFor(t)
	g.Observe(harness.Event{Kind: harness.EventToolStart, Tool: "write_file", ToolCallID: "wf",
		Args: map[string]any{"path": "FLAG", "content": "flag{written_by_tool}"}})
	g.Observe(harness.Event{Kind: harness.EventToolEnd, Tool: "write_file", ToolCallID: "wf",
		Args: map[string]any{"path": "FLAG", "content": "flag{written_by_tool}"}})
	toolEnd(g, "rd", "bash", "cat FLAG", "flag{written_by_tool}\n")

	if p := provOf(t, g, "flag{written_by_tool}"); p != ProvenanceFabricated {
		t.Fatalf("工具写文件再读回必须被判 fabricated，got %q", p)
	}
}

// 格式闸的拒绝记录只留指纹，不留明文（前身 flag 明文泄漏进持久文件的事故）。
//
// 直接测 recordLocked 而不是绕事件路径：形状真源在 Match 阶段就已经过滤掉大
// 部分非法串，事件路径上格式闸很少触发，绕过去测会得到一个永远通过的假测试。
func TestFormatRejectsKeepFingerprintsOnly(t *testing.T) {
	g := gateFor(t)
	// 绕过抽取阶段，直接把一个「不像答案」的串送进记账入口。
	g.recordLocked("not-an-answer", ProvenanceObserved, "", harness.Event{}, "", 0.9, true)
	g.recordLocked("flag{secret_value}", ProvenanceObserved, "", harness.Event{}, "", 0.9, true)

	if len(g.Candidates()) != 1 {
		t.Fatalf("只有合法形态应进候选账本，got %+v", g.Candidates())
	}
	fps := g.FormatRejected()
	if len(fps) != 1 {
		t.Fatalf("应记录 1 条格式拒绝，got %v", fps)
	}
	if strings.Contains(fps[0], "not-an-answer") {
		t.Errorf("格式闸拒绝记录泄漏明文: %q", fps[0])
	}
	// 零值 Shape 不认任何形态。
	g2 := NewGateShape(answer.Shape{})
	g2.recordLocked("flag{anything}", ProvenanceObserved, "", harness.Event{}, "", 0.9, true)
	if len(g2.Candidates()) != 0 {
		t.Error("零值 Shape 不应接受任何候选")
	}
}

// 观测族的置信度高于幻觉族 —— 报告排序依赖它。
func TestObservedConfidenceHigherThanFabricated(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "o", "bash", "curl -s http://t/", "flag{obs_conf}\n")
	g.Observe(harness.Event{Kind: harness.EventText, Text: "flag{fab_conf}"})
	conf := map[string]float64{}
	for _, c := range g.Candidates() {
		conf[c.Flag] = c.Confidence
	}
	if conf["flag{obs_conf}"] <= conf["flag{fab_conf}"] {
		t.Errorf("观测族置信度应更高: %v", conf)
	}
}

// 引用路径（前身 verify.py 的 local_derived 语义的**反向**）：候选首现于工具
// 输出（观测族），之后 agent 又把它当作参数回喂给本地工具 —— 族别必须**保持**
// 观测族。前身把这类调用也判成「agent 自造」是 fail-closed 的一半来源。
func TestCitedAfterObservationStaysObserved(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "o", "bash", "curl -s http://10.0.0.1/api", "response: flag{cited_later}\n")
	// agent 把已知答案当参数回喂（如本地校验器 / 二次查询）
	toolEnd(g, "r", "bash", "python3 verify.py 'flag{cited_later}'", "OK\n")

	if p := provOf(t, g, "flag{cited_later}"); p != ProvenanceObserved {
		t.Fatalf("已观测的候选被引用后仍应是 observed，got %q", p)
	}
	if n := len(g.New()); n != 1 {
		t.Errorf("引用不得影响可提交性，got %d 条", n)
	}
}

// 轮末 harvest 的语义（根包 harvest 的行为在这里固化）：
// 同一 flag 在轮内被多次 tool_end 看到，New() 只给一次；Mark 之后不再给。
// 这是修掉「每次 tool_end 重遍历全部候选重复提交」缺陷的核心断言。
func TestHarvestSemanticsAcrossThreeToolEnds(t *testing.T) {
	g := gateFor(t)
	toolEnd(g, "1", "bash", "curl -s http://t/", "flag{harvest_once}\n")
	toolEnd(g, "2", "bash", "curl -s http://t/", "flag{harvest_once}\n")
	toolEnd(g, "3", "bash", "curl -s http://t/", "flag{harvest_once}\n")

	submits := 0
	// 第一轮末
	for _, c := range g.New() {
		submits++
		g.Mark(c.Flag, harness.Evaluation{Accepted: true, Progress: true}, nil)
	}
	// 第二轮末（同一候选）
	for _, c := range g.New() {
		submits++
		g.Mark(c.Flag, harness.Evaluation{Accepted: true, Progress: true}, nil)
	}
	if submits != 1 {
		t.Fatalf("同一 flag 只应提交一次，got %d 次", submits)
	}
	if st := g.Stats(); st.Observed != 1 || st.Submitted != 1 || st.Correct != 1 {
		t.Errorf("统计不对: %+v", st)
	}
}

// 判错账本与 gate 的配合：判错的答案进账本、带指纹，且账本 Has 能挡住重提。
func TestLedgerBlocksResubmission(t *testing.T) {
	g := gateFor(t)
	l := NewLedger()
	toolEnd(g, "1", "bash", "curl -s http://t/", "flag{wrong_once}\n")
	for _, c := range g.New() {
		if l.Has(c.Flag) {
			continue
		}
		g.Mark(c.Flag, harness.Evaluation{}, nil)
		l.Record(c.Flag, "platform_rejected")
	}
	if !l.Has("flag{wrong_once}") {
		t.Fatal("判错答案应进账本")
	}
	if len(l.Fingerprints()) != 1 {
		t.Fatalf("账本应有 1 条指纹，got %v", l.Fingerprints())
	}
	if strings.Contains(l.Fingerprints()[0], "wrong_once") {
		t.Errorf("账本指纹泄漏明文: %q", l.Fingerprints()[0])
	}
}
