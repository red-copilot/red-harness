package gate

import (
	"sort"
	"strings"
	"sync"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

// Gate 是候选答案的账本。
//
// 并发：Observe 可能从 reader 协程被调用，而 harvest 在主循环里调
// Candidates/New/Mark，所以全部入口都要加锁。这不是理论风险 —— pi 的
// 事件是异步推来的，前身那套「事件回调里顺手提交」的写法一旦并行就是数据竞争。
type Gate struct {
	mu sync.Mutex

	shape answer.Shape

	// first 是首现表：答案串 → 它首次出现时判定的族别与出处。**只写一次**，
	// 之后任何事件都不再改它 —— 这就是首现优先的全部实现（前身用 3,592 行
	// 事后重建，这里用一条不可回退的记账规则）。
	first map[string]firstSeen

	order  []string
	byFlag map[string]*harness.Candidate

	// formatRejects / formatRejectFP 记被格式闸挡下的串。**只留指纹不留明文**：
	// 前身 flag 明文泄漏进持久文件的事故（_scrub_flag_plaintext）。
	formatRejects  map[string]string
	formatRejectFP []string

	stats Stats

	// currentIntent / currentRound 由根包在每轮开头注入（Session.Scheduler 的
	// 语义在 dag 包里，gate 不能依赖它，所以只能被动接受）。
	currentIntent string
	currentRound  int
}

// firstSeen 是一个答案串的**首现判定**。写进去之后就不再改（唯一的例外是
// SetProvenance 允许单向升族）。
type firstSeen struct {
	prov   Provenance
	reason string
	round  int
	// locked 表示这个族别是被**观测事件**钉死的（命令里不含答案形状、
	// 输出里逐字出现），不再需要靠后续事件坐实。
	//
	// 它解决一个真实的错杀：agent 先在思考里写下候选（幻觉族），随后
	// `curl` 的输出里真的出现了同一个串 —— 那是逆向/密码题的常见路径，
	// 必须能升族。但升族只允许发生在「候选还没被观测坐实」之前，否则
	// `cat /tmp/f` 会把自己写的猜测一路洗成观测。locked 就是这个开关。
	locked bool
}

// Stats 是候选账本的分族统计。
//
// 前身 hallucination.py 的第一安全性质：**推导族永不计数、永不触发任何干预**。
// 前身的影子审计发现「29 条被拒候选里 19 条实为正确答案」，几乎全落在推导族
// —— 对推导族采取任何动作都是净损失。所以这里 Derived 与 Fabricated 分列，
// 阈值判断只允许读 Fabricated。
type Stats struct {
	Observed   int
	Derived    int
	Fabricated int
	// Submitted 是已提交过的候选数（含判错），Correct 是平台确认数。
	Submitted int
	Correct   int
	Rejected  int
}

// NewGate 用题面推断出的答案形态构造 Gate。
func NewGate(description string) *Gate { return NewGateShape(answer.Infer(description)) }

// NewGateShape 用显式形态构造 Gate（测试与显式配置用）。
func NewGateShape(sh answer.Shape) *Gate {
	return &Gate{
		shape:  sh,
		first:  map[string]firstSeen{},
		byFlag: map[string]*harness.Candidate{},
	}
}

// Shape 返回本题的答案形态（只读，便于调用方复用同一口径）。
func (g *Gate) Shape() answer.Shape { return g.shape }

// Stats 返回分族统计快照。异常一律吞掉 —— 记账模块不能因为自身故障影响主流程
// （前身 hallucination.py 的第三安全性质）。
func (g *Gate) Stats() Stats {
	if g == nil {
		return Stats{}
	}
	defer func() { _ = recover() }()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// SetIntent 记录当前意图与轮次，写进之后抽出的候选里（DAG 推导链需要）。
func (g *Gate) SetIntent(intentID string, round int) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.currentIntent, g.currentRound = intentID, round
}

// ── Observe ──

// Observe 处理一个事件，抽取候选并记账。**绝不做 IO**（不提交、不写盘）。
//
// 这里的纪律不是洁癖：前身把提交放进事件回调，于是每次 tool_execution_end
// 都重遍历全部候选重复提交一次，同一道题把平台配额打光。提交只发生在轮末。
func (g *Gate) Observe(ev harness.Event) {
	if g == nil {
		return
	}
	// 记账模块的任何异常都必须吞掉：gate 挂掉不能把解题主流程拖挂。
	defer func() { _ = recover() }()

	g.mu.Lock()
	defer g.mu.Unlock()

	switch ev.Kind {
	case harness.EventToolStart:
		g.observeToolStartLocked(ev)
	case harness.EventToolEnd:
		g.observeToolEndLocked(ev)
	case harness.EventText, harness.EventThinking:
		g.observeProseLocked(ev)
	}
}

func (g *Gate) observeToolStartLocked(ev harness.Event) {
	cmd := commandOf(ev)
	if cmd == "" {
		return
	}
	// 命令里含答案形状 ⇒ 这些候选是 agent 自己敲进去的，此刻就定族（幻觉）。
	// 不能等到 ToolEnd —— `echo 'flag{x}' > /tmp/f` 本身没有输出，若只在
	// 「有输出」时判定，这个候选要等 `cat /tmp/f` 才被记账，而那时它已经
	// 出现在工具输出里了，看起来完全像观测。这是洗白路径的关键一步。
	if commandCarriesShape(cmd, g.shape) {
		g.noteFabricatedLocked(shapeCandidates(g.shape, cmd), refOf(ev, cmd), ReasonCommandAuthored, cmd)
	}
	// 引号载荷里的**裸串**候选：`echo 'hunter2xyz' > /tmp/f` 这条命令没有
	// `{` 形状标记，上一步看不见它 —— 于是这个候选要等到 `cat /tmp/f` 才被
	// 记账，那时它已经在工具输出里，看起来完全像观测。这是洗白路径在裸串
	// 形态下的同一个洞，必须在这里堵。
	//
	// 只看引号内容是有意的：引号是 shell 里传递字面值的可靠方式，agent 把
	// 自己编的值写进文件/回喂校验器时几乎总带引号；而不带引号的那条路径
	// （`printf %s hunter2xyz`）由下面的 markMaterializedLocked 对**已记账**
	// 候选覆盖。反过来，若把命令里所有裸串 token 都当候选，`nmap -sV 10.0.0.1`
	// 会往账本里塞进 `10.0.0.1` —— 形状真源「宁可多认」的口径在命令这一侧
	// 只会制造噪音。
	g.noteFabricatedLocked(quotedCommandCandidates(g.shape, cmd), refOf(ev, cmd), ReasonCommandAuthored, cmd)
	// 常见编码变体：`python3 -c "print(b64decode('ZmxhZ3t4fQ=='))"` 里没有
	// `flag{` 字面标记，但它把答案物化进了命令。这一步只能对**已记账的
	// 候选**做（否则要枚举整个命令的编码空间）。
	g.markMaterializedLocked(cmd, refOf(ev, cmd), cmd)
}

func (g *Gate) observeToolEndLocked(ev harness.Event) {
	cmd := commandOf(ev)
	ref := refOf(ev, cmd)

	// 输出里的候选：按首现优先定族。多数候选在这里定案。
	g.observeOutputLocked(outputCandidates(g.shape, ev.Output), cmd, ev.Output, ref)

	// 本事件**没能**在输出里坐实、而命令里又含答案形状的候选 ⇒ 幻觉。
	// 顺序在「输出里的候选」之后，所以先出现优先于后出现的自造。
	if commandCarriesShape(cmd, g.shape) {
		for _, cand := range shapeCandidates(g.shape, cmd) {
			if strings.Contains(ev.Output, cand) {
				continue // 已被上面的输出判定处理（含「命令自造」的情形）
			}
			g.noteFabricatedLocked([]string{cand}, ref, ReasonCommandAuthored, cmd)
		}
	}

	// 注意：这里**不**需要为「先猜后验」另建暂存表。升族所需的证据就是**本条
	// 事件自己的输出**（上面那一次 observeOutputLocked 已经用过它了），所以
	// 一个首现于散文的候选，会在它第一次被真观测逐字坐实的那条事件上直接升族
	// —— 与它的首现相隔多久无关。多存一份输出只会让内存无界增长（前身
	// hallucination.py 的 _MAX_MARKS 就是为这件事设的上限）。
}

func (g *Gate) observeProseLocked(ev harness.Event) {
	text := ev.Text
	if text == "" {
		return
	}
	// 散文里重复出现的候选：只在**第一次**重复时标成推导族，之后不再动它。
	// 这条通道的作用是让「先猜后验」有个可分辨的中间态（前身 verify.py 的
	// local_derived 口径），而不是把散文里的每一次重复都算成新证据。
	g.promoteProseRepeatLocked(text)
	// 散文里的候选只记账：agent 在思考里写下的 flag 是**它的假设**，不是系统
	// 产出。前身 verify.flag_confidence 的注释写得很直白：observed_output 含
	// 助手文本，LLM 幻觉的 flag 会被自己的文本「grounded」化导致误放行。
	//
	// 但有一条单向通道必须留着：agent 先在思考里猜到答案、之后靶标产物里真的
	// 出现同一个串 —— 这是逆向/密码题的正解路径，前身实测「29 条被拒候选里
	// 19 条实为正确答案」，掐掉它等于掐掉那 19 条。所以散文只把族别标成
	// 「尚未坐实」（locked=false），真正的升族留给 observeOutputLocked。
	g.noteFabricatedLocked(shapeCandidates(g.shape, text), refOf(ev, ""), ReasonNotGrounded, text)
}

// promoteProseRepeatLocked 把「散文里第二次出现」的候选标成推导族。
//
// 为什么要有这个中间态：前身 verify.py 把 `local_derived` 单列一类，正是为了
// 把「真推导但没资格」与「凭空编」分开记账 —— 它的影子审计发现被误拒的
// 29 条里 19 条是真答案，几乎全在这条路径上。gate 保留这个分账，但**不给它
// 任何动作**：推导族永不计数、永不触发干预（前身第一安全性质）。
func (g *Gate) promoteProseRepeatLocked(text string) {
	for _, cand := range g.order {
		f, seen := g.first[cand]
		if !seen || f.locked || f.prov != ProvenanceFabricated {
			continue
		}
		if !strings.Contains(text, cand) {
			continue
		}
		g.setProvenanceLocked(cand, ProvenanceDerived)
	}
}

// observeOutputLocked 处理「工具输出里出现的候选」。这是唯一能把候选判成
// 观测族（可提交）的地方。
//
// 首现优先在这里落地：
//   - 未定族的候选：命令里没有它 ⇒ 观测族；命令里含它（自造/读自己的文件）
//     ⇒ 幻觉族，并**钉死**（locked）。
//   - 已定族且未钉死的候选：若这是它第一次被工具输出逐字坐实，升到观测族。
//     这条通道覆盖「先猜后验」（散文先猜、靶标产物后验），前身影子审计的
//     19/29 真答案走的就是它。
//   - 已钉死的候选：族别不变，只刷新出处。`cat /tmp/f` 读回自己写的猜测
//     就是这一类 —— 首现优先的全部价值在此。
func (g *Gate) observeOutputLocked(cands []string, cmd, output string, ref harness.Event) {
	for _, cand := range cands {
		if !g.shape.LooksLike(cand) {
			g.rejectLocked(cand, ReasonFormatRejected)
			continue
		}
		authored := cmd != "" && g.commandAuthoredLocked(cmd, cand)
		if f, seen := g.first[cand]; seen {
			if authored {
				f.locked = true
				g.first[cand] = f
			} else if !f.locked && f.prov != ProvenanceObserved {
				g.setProvenanceLocked(cand, ProvenanceObserved)
				g.first[cand] = firstSeen{prov: ProvenanceObserved, round: g.currentRound, locked: true}
			}
			g.touchLocked(cand, ref, output)
			continue
		}
		if authored {
			r := ReasonCommandAuthored
			if readsOwnState(cmd) {
				r = ReasonSelfReadback
			}
			g.recordLocked(cand, ProvenanceFabricated, r, ref, output, 0.2, true)
			continue
		}
		g.recordLocked(cand, ProvenanceObserved, "", ref, snippet(output, cand), 0.9, true)
	}
}

// noteFabricatedLocked 记录一批**已知自造**的候选（命令里含形状 / 散文）。
//
// locked 只给「命令自造」与「读自己的文件」两类：它们是洗白路径的入口，必须
// 钉死。散文（not_grounded）**不钉**，因为「先猜后验」要靠它起步。
func (g *Gate) noteFabricatedLocked(cands []string, ref harness.Event, reason, src string) {
	pin := reason == ReasonCommandAuthored || reason == ReasonSelfReadback
	for _, cand := range cands {
		if !g.shape.LooksLike(cand) {
			g.rejectLocked(cand, ReasonFormatRejected)
			continue
		}
		if f, seen := g.first[cand]; seen {
			if pin && !f.locked {
				f.locked = true
				g.first[cand] = f
			}
			g.touchLocked(cand, ref, src)
			continue
		}
		g.recordLocked(cand, ProvenanceFabricated, reason, ref, src,
			confidenceOf(ProvenanceFabricated, reason), pin)
	}
}

// touchLocked 刷新一个已定族候选的出处，**不改变族别**。
//
// 为什么值得单独一个函数：`cat /tmp/f` 读回自己写的猜测时，族别必须是幻觉
// （首现优先），但**出处**要更新成那次工具调用 —— 否则报告里说不清它是怎么
// 被读回来的，取证链就断了。
func (g *Gate) touchLocked(cand string, ref harness.Event, src string) {
	c, ok := g.byFlag[cand]
	if !ok {
		return
	}
	if c.Output == "" && src != "" {
		c.Output = snippet(src, cand)
	}
	// 出处更新：候选被一条**不同的**工具调用再次看到时，把出处改成最新那一次。
	//
	// 为什么是「最新」而不是「最早」：洗白路径的取证价值在**读回它的那次**
	// 调用 —— `echo flag{x} > /tmp/f` 是来源，`cat /tmp/f` 是它怎么变成
	// 「工具输出里的 flag」的。族别由首现钉死，出处由最后一次观测记录，
	// 两者合起来才说得清一条候选的完整轨迹。
	if ref.ToolCallID != "" && ref.ToolCallID != c.ToolCallID {
		c.ToolCallID = ref.ToolCallID
		c.Source = sourceOf(ref)
	}
}

// markMaterializedLocked 处理「候选的编码变体出现在命令里」这一条路径。
//
// 只对**已记账的候选**做：一个尚未被任何事件抽取过的串，我们无从知道它是不是
// 候选，也就无从谈「被物化」。
func (g *Gate) markMaterializedLocked(cmd string, ref harness.Event, src string) {
	if cmd == "" {
		return
	}
	bare := stripHeredocBodies(cmd)
	for _, cand := range g.order {
		if _, seen := g.first[cand]; seen {
			continue
		}
		if !materializedInCommand(bare, cand) {
			continue
		}
		// locked=true：编码物化是自造的一条确定路径（`python3 -c "…b64decode('…')"`），
		// 与直接写 `echo flag{x}` 同级，必须钉死。
		g.recordLocked(cand, ProvenanceFabricated, ReasonCommandAuthored, ref, src, 0.1, true)
	}
}

// commandAuthoredLocked 报告「产生这条输出的命令」是否把候选物化/读回了。
//
// 三档判据，从强到弱：
//  1. 命令里含答案**形状标记**（`flag{`）—— 最直白的一类（echo / printf /
//     heredoc 写入）。注意这里的 cmd 已经摘掉 heredoc 载荷。
//  2. 命令在读 agent 自己的状态文件（`cat FLAG`）—— 读回自写内容。
//  3. 命令把候选的**字面变体**（原文/裸 body/hex/base64）写进了引号载荷 ——
//     `./validate 'flag{x}'`、`python3 -c "…b64decode('…')"`。
//
// 第 3 档看的是引号载荷的**内容**（不是「摘掉引号后的骨架」）：裸串形态的
// 答案常常就是引号里的那个词，摘掉就什么都看不见了。带 scheme 的载荷（URL）
// 对裸串候选跳过 —— 见 stripURLs 的注释。
func (g *Gate) commandAuthoredLocked(cmd, cand string) bool {
	if cmd == "" {
		return false
	}
	bare := stripHeredocBodies(cmd)
	if commandCarriesShape(bare, g.shape) {
		return true
	}
	if readsOwnState(bare) {
		return true
	}
	if materializedInCommand(bare, cand) {
		return true
	}
	return false
}

// shapeCandidates 从一段文本里取**信封形态**的候选。
//
// 为什么不用 shape.Match 的完整结果：裸串形态下 Match 会把命令片段（`/tmp/f`、
// `python3`、`solve.py`）也当候选 —— 那是形状真源按「值像不像答案」判定的
// 正常行为（漏认的代价是丢分），但在 gate 这一层把命令片段记成候选纯属噪音，
// 而且会污染「命令是否含答案形状」的判断。信封形态（`xxx{...}`）是答案的主体
// 形态，这里只取它；工具输出里的裸串候选由 outputCandidates 覆盖。
//
// 关键：一律走 answer 包的 Match，不自己写正则 —— 前身 B55 事故就是「同一个
// 判定在四个调用点各写一遍，长期漂移」。
func shapeCandidates(sh answer.Shape, text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	for _, cand := range sh.Match(text) {
		if _, ok := envelopeBody(cand); ok {
			out = append(out, cand)
		}
	}
	return out
}

// recordLocked 是唯一的记账入口：**首现优先**在这里落地。
//
// 已经定族的候选直接返回 —— 一条 `cat /tmp/f` 读到自己的猜测，永远改不了
// 它「首现于命令参数」的族别。这就是洗白路径的对策（出处与置信度的刷新走
// touchLocked，不走这里）。
func (g *Gate) recordLocked(flag string, prov Provenance, reason string, ref harness.Event, src string, conf float64, locked bool) {
	flag = strings.TrimSpace(flag)
	if flag == "" {
		return
	}
	// 格式闸：连答案形态都不像的串不进候选账本。
	// 这一道闸不是多余的 —— 前身 B14 的真实事故是凭证正则把 HTML 表单字段
	// （login ==）、状态码（admin: 500）、SQLi payload 残片统统当凭证，某题
	// 攒了 61 条垃圾事实，注入下一场时把真信号挤没。形态判定只认 answer 包
	// 这一个真源（前身 B55：四处各写一套 ⇒ 长期漂移）。
	if !g.shape.LooksLike(flag) {
		g.rejectLocked(flag, ReasonFormatRejected)
		return
	}

	if _, seen := g.first[flag]; seen {
		if c, ok := g.byFlag[flag]; ok && c.Output == "" {
			c.Output = src
		}
		return
	}

	// 裸串候选（非信封形态）只保留首尾各 120 个字符：前身实测某题的
	// 61 条垃圾凭证就是这么把真信号挤没的（B14），而裸串候选在真实数据里
	// 绝大多数是命令片段/URL。信封候选一律完整保留 —— 它是答案的主体形态。
	//
	// 【本实现的收紧，不在前身清单里】前身只裁了「事实条数」的上限，没有裁
	// 单条候选携带的输出片段长度；这条是本实现为了让候选账本在裸串题上不被
	// 整段工具输出撑爆而加的。
	src = clipRaw(src, flag)

	c := &harness.Candidate{
		Flag:         flag,
		Source:       sourceOf(ref),
		Output:       src,
		Confidence:   conf,
		Provenance:   prov,
		ToolCallID:   ref.ToolCallID,
		IntentID:     g.currentIntent,
		Round:        g.currentRound,
		RejectReason: reason,
	}
	g.first[flag] = firstSeen{prov: prov, reason: reason, round: g.currentRound, locked: locked}
	g.byFlag[flag] = c
	g.order = append(g.order, flag)

	switch prov {
	case ProvenanceObserved:
		g.stats.Observed++
	case ProvenanceDerived:
		g.stats.Derived++
	default:
		g.stats.Fabricated++
	}
}

// rejectLocked 记录被格式闸挡下的串（只留指纹，不留明文）。
func (g *Gate) rejectLocked(flag, reason string) {
	if _, ok := g.formatRejects[flag]; ok {
		return
	}
	if g.formatRejects == nil {
		g.formatRejects = map[string]string{}
	}
	g.formatRejects[flag] = reason
	g.formatRejectFP = append(g.formatRejectFP, Fingerprint(flag))
}

func confidenceOf(prov Provenance, reason string) float64 {
	switch prov {
	case ProvenanceObserved:
		return 0.9
	case ProvenanceDerived:
		return 0.5
	default:
		if reason == ReasonNotGrounded {
			return 0.1
		}
		return 0.2
	}
}

// ── Gate 接口的其余三个方法 ──

// Candidates 返回全部候选（含已提交的），保序，用于报告与续跑。
func (g *Gate) Candidates() []harness.Candidate {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]harness.Candidate, 0, len(g.order))
	for _, f := range g.order {
		out = append(out, *g.byFlag[f])
	}
	return out
}

// New 返回**尚未提交过**的候选。这是修掉「每次 tool_end 重遍历全部候选重复
// 提交」那个缺陷的关键：调用方（轮末 harvest）只提交这里返回的东西。
//
// 返回的候选经过两道闸的过滤：
//   - 格式闸：在 recordLocked 里已过（不合法形态根本进不了账本）。
//   - 来源闸：只返回 ProvenanceObserved。推导族与幻觉族**永不提交**。
//
// 平台闸由调用方执行（harvest 调 Platform.Submit）。
func (g *Gate) New() []harness.Candidate {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []harness.Candidate
	for _, f := range g.order {
		c := g.byFlag[f]
		if c.Submitted || c.Provenance != ProvenanceObserved {
			continue
		}
		out = append(out, *c)
	}
	return out
}

// NewAll is the v0.4 submission view. Observed and derived candidates are both
// grounded enough to submit; fabricated candidates remain excluded. New keeps
// the v0.3 observed-only behavior for legacy callers.
func (g *Gate) NewAll() []harness.Candidate {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []harness.Candidate
	for _, f := range g.order {
		c := g.byFlag[f]
		if c.Submitted || c.Provenance == harness.ProvenanceFabricated {
			continue
		}
		out = append(out, *c)
	}
	return append([]harness.Candidate(nil), out...)
}

// Mark 回填提交结果。
//
// 无论对错都置 Submitted —— 「同一答案永不重提」是硬规矩（前身：重提只是白烧
// 平台配额）。注意 err != nil 时也置 Submitted 是刻意的：网络抖动下重提的
// 收益远小于风险（重复提交会被平台记幂等，但连续重试会打光配额）。
func (g *Gate) Mark(flag string, res harness.SubmitResult, err error) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.byFlag[flag]
	if !ok {
		return
	}
	if c.Submitted {
		return
	}
	c.Submitted = true
	g.stats.Submitted++
	switch {
	case err != nil:
		// 传输/平台错误：记在 SubmitError，**不动 RejectReason**——族别归因
		// （为什么 gate 认为它可疑）与平台侧结果回答的是两个不同问题，
		// 合并会永久丢掉前一个答案（这是根包契约把 Reject 拆成
		// RejectReason/SubmitError 两个字段的原因）。
		c.SubmitError = err.Error()
	case res.Duplicate:
		c.Duplicate = true
		c.Correct = true
		g.stats.Correct++
	case res.Correct:
		c.Correct = true
		g.stats.Correct++
	default:
		c.SubmitError = "platform_rejected"
		g.stats.Rejected++
	}
}

// SetProvenance 把一个候选显式**升**到某一族。降级请求一律被忽略。
//
// 为什么留这个口子：dag 包会在事实层独立拒收一次答案形状内容（两道防线），
// 而它若判定某候选来自 host-verified 事实，gate 应当能采纳 —— 但方向永远
// 是「更可信」，绝不能反向把一个观测族候选降级（那会重演前身 29 条里
// 19 条真答案被误杀的 fail-closed 事故）。
func (g *Gate) SetProvenance(flag string, prov Provenance) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setProvenanceLocked(flag, prov)
}

// setProvenanceLocked 是 SetProvenance 的**调用方已持锁**版本。
//
// 为什么必须分成两个：observeOutputLocked 与 promoteProseRepeatLocked 都在
// 持锁状态下判族，直接调公开版会自锁死（这不是理论问题 —— 第一版就是这么
// 写的，go test 30 秒超时挂死）。公开版只给包外调用方用。
func (g *Gate) setProvenanceLocked(flag string, prov Provenance) {
	c, ok := g.byFlag[flag]
	if !ok || c.Submitted {
		return
	}
	f := g.first[flag]
	if rank(prov) <= rank(f.prov) {
		return
	}
	g.stats.add(-1, f.prov)
	g.stats.add(1, prov)
	f.prov = prov
	f.reason = ""
	g.first[flag] = f
	c.Provenance = prov
	c.RejectReason = ""
	c.Confidence = confidenceOf(prov, "")
}

func rank(p Provenance) int {
	switch p {
	case ProvenanceObserved:
		return 2
	case ProvenanceDerived:
		return 1
	default:
		return 0
	}
}

// add 按族别加减计数。只有 setProvenanceLocked 用它（升族时要把旧族的计数
// 减掉），其余路径一律在 recordLocked 里一次性 +1。
func (s *Stats) add(n int, p Provenance) {
	switch p {
	case ProvenanceObserved:
		s.Observed += n
	case ProvenanceDerived:
		s.Derived += n
	default:
		s.Fabricated += n
	}
}

// Fabrications 返回幻觉族的候选（只读，供调用方做阈值判断）。
//
// **推导族没有对应的访问器** —— 这是刻意的。前身 hallucination.py 的第一
// 安全性质是「推导族永不计数、永不触发任何干预」，所以只要不提供读推导族的
// 入口，调用方就没有机会对它做错事。
func (g *Gate) Fabrications() []harness.Candidate {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []harness.Candidate
	for _, f := range g.order {
		if c := g.byFlag[f]; c.Provenance == ProvenanceFabricated {
			out = append(out, *c)
		}
	}
	return out
}

// FormatRejected 返回被格式闸挡下的串的**指纹**（不含明文）。
func (g *Gate) FormatRejected() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.formatRejectFP...)
}

// ── RejectedLedger ──

// Ledger 是判错账本：记录被平台判错的答案，用于「同一答案永不重提」与回灌
// prompt。回灌时**只给指纹不给明文** —— 前身 _scrub_flag_plaintext 的教训是
// flag 明文泄漏进了会回灌下一场的文件（MEMORY.md / _blackboard.json /
// tried_commands.md）。
type Ledger struct {
	mu    sync.Mutex
	order []string
	byFP  map[string]string // 指纹 → 原因
	seen  map[string]bool   // 明文 → 已判错（明文只存在内存里，不落盘）
}

// NewLedger 构造一个空账本。
func NewLedger() *Ledger {
	return &Ledger{byFP: map[string]string{}, seen: map[string]bool{}}
}

// Record 记录一个被平台判错的答案。
func (l *Ledger) Record(flag string, reason string) {
	if l == nil {
		return
	}
	defer func() { _ = recover() }()
	flag = strings.TrimSpace(flag)
	if flag == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byFP == nil {
		l.byFP, l.seen = map[string]string{}, map[string]bool{}
	}
	l.seen[flag] = true
	fp := Fingerprint(flag)
	if _, ok := l.byFP[fp]; !ok {
		l.order = append(l.order, fp)
	}
	l.byFP[fp] = reason
}

// Has 报告某答案是否已被判错。
func (l *Ledger) Has(flag string) bool {
	if l == nil {
		return false
	}
	defer func() { _ = recover() }()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[strings.TrimSpace(flag)]
}

// Fingerprints 返回所有判错答案的指纹，按记录顺序，供渲染进下一轮 prompt。
func (l *Ledger) Fingerprints() []string {
	if l == nil {
		return nil
	}
	defer func() { _ = recover() }()
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.order...)
}

// Reasons 返回「指纹 → 原因」，用于报告的可解释性。
func (l *Ledger) Reasons() map[string]string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]string, len(l.byFP))
	for k, v := range l.byFP {
		out[k] = v
	}
	return out
}

// SortedFingerprints 返回排序后的指纹（报告用，保证输出稳定）。
func (l *Ledger) SortedFingerprints() []string {
	fps := l.Fingerprints()
	sort.Strings(fps)
	return fps
}

// ── 小工具 ──

// commandOf 从事件参数里取命令文本。pi 的 bash 工具用 `command`，写文件类
// 工具用 path/content —— 后者也要看，因为 agent 可以把候选写进脚本再执行。
func commandOf(ev harness.Event) string {
	if len(ev.Args) == 0 {
		return ""
	}
	for _, k := range []string{"command", "cmd", "script"} {
		if v, ok := ev.Args[k].(string); ok && v != "" {
			return v
		}
	}
	// 写文件类工具：把 path 与 content 拼起来判定（`write_file(path=FLAG,
	// content=flag{x})` 与 echo 自造同源）。
	var parts []string
	for _, k := range []string{"path", "file_path", "filePath", "filename", "file"} {
		if v, ok := ev.Args[k].(string); ok && v != "" {
			parts = append(parts, v)
		}
	}
	for _, k := range []string{"content", "file_text", "new_str", "new_string", "text", "body"} {
		if v, ok := ev.Args[k].(string); ok && v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " ")
}

// refOf 把事件压成一个便于传递的出处引用。
func refOf(ev harness.Event, cmd string) harness.Event {
	return harness.Event{Kind: ev.Kind, Tool: ev.Tool, ToolCallID: ev.ToolCallID, Text: cmd}
}

func sourceOf(ref harness.Event) string {
	switch {
	case ref.Tool != "" && ref.Text != "":
		return ref.Tool + ": " + clip(ref.Text, 160)
	case ref.Tool != "":
		return ref.Tool
	default:
		return clip(ref.Text, 160)
	}
}

// snippet 取命中处附近的原文（取证用）。命中位置找不到时退回开头 —— 与
// answer.Match 的（可能被大小写/编码归一过的）匹配口径保持一致，不假装精确。
// clipRaw 给裸串候选的输出片段设上限（信封候选原样返回）。
func clipRaw(src, cand string) string {
	if src == "" {
		return src
	}
	if _, isEnvelope := envelopeBody(cand); isEnvelope {
		return src
	}
	return clip(src, 120)
}

func snippet(text, cand string) string {
	idx := strings.Index(text, cand)
	if idx < 0 {
		return clip(text, 240)
	}
	start := idx - 80
	if start < 0 {
		start = 0
	}
	end := idx + len(cand) + 80
	if end > len(text) {
		end = len(text)
	}
	return text[start:end]
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
