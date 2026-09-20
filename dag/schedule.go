package dag

import (
	"context"
	"errors"
	"strings"

	harness "github.com/red-copilot/red-harness"
)

// ── Scheduler：harness.Planner 的实现 ──
//
// 它把图接到根包的轮循环上。**注意它的定位**：调度主干仍是目标链
// （`seedChain` 按类别铺一条阶段链，阶段序即优先级），图在这里挣的是
// 「跳过被证伪的 / 跳过前置未满足的 / 跳过试够了的」这三条剪枝。
// 不要往这里加拓扑排序式调度或攻击路径规划（设计文档 §一「诚实的边界」）。
type Scheduler struct {
	G *Graph

	// activeID 是当前正在执行的意图。Ingest 用它给 agent 申报的 negative 事实
	// 归属被证伪者——**意图 id 是宿主的概念，agent 不知道**，所以归属判定必须
	// 在这里做，不能让 agent 传。
	activeID string
	// known 是 Activate 时图上已有的事实 id 集合。Settle 靠它算出「本轮产出了
	// 什么」——harness.Planner 的 Settle 签名只给 RoundResult，没有 produced 列表。
	known map[string]bool

	// LastErr 记录 Activate/Settle 的内部错误。接口没有 error 返回值，所以
	// 错误必须落在某处可读的地方（静默吞掉会让「意图状态没更新」无从发现）。
	LastErr error
}

// NewScheduler 用一张图构造调度器。
func NewScheduler(g *Graph) *Scheduler { return &Scheduler{G: g} }

// 编译期断言：Scheduler 满足根包契约。
var _ harness.Planner = (*Scheduler)(nil)

// Next 返回下一个可执行的意图；前沿耗尽时返回 nil。
//
// 首次调用时会**铺阶段链**（如果图上一条意图都没有）。铺链的时机选在这里而不是
// New/NewScheduler：断点续跑的图里已经有意图了，重新铺一条链会让阶段重复执行
// （前身「每次 prompt 都被告知仍在做 recon」的另一面）。
//
// in.Challenge 只用来补图的身份字段（类别/编号）：图的 Category 决定铺哪条阶段链，
// 而 `New(harness.Challenge{})` 建出来的图没有类别，会静默走默认链——crypto 题
// 于是从端口扫描开始（前身 goals_for_category 的全部意义就是避免这件事）。
// in.Outcome 刻意不用：见下方「平台判错的答案为什么不转成 negative」。
//
// 错误返回：只有「图本身不可用」才算错误。前沿耗尽不是错误——它是正常的终局，
// 用 (nil, nil) 表达。把两者混成一个 error 会让轮循环无法区分「做完了」与
// 「图坏了」。
func (s *Scheduler) Next(_ context.Context, in harness.PlannerInput) (*harness.IntentRef, error) {
	if s.G == nil {
		return nil, nil
	}
	ch := in.Challenge
	if s.G.Category == "" {
		s.G.Category = ch.Category
	}
	if s.G.Code == "" {
		s.G.Code = ch.Code
	}
	s.seedChain()
	n := s.G.NextIntent()
	if n == nil {
		return nil, nil
	}
	return &harness.IntentRef{ID: n.ID, Kind: string(n.IntentKind), Goal: n.Goal, Round: n.Round}, nil
}

// Activate 标记意图进入执行中，并记下「此刻图上有什么」作为产出判定的水位线。
func (s *Scheduler) Activate(it *harness.IntentRef) {
	if it == nil || s.G == nil {
		return
	}
	s.activeID = it.ID
	s.known = map[string]bool{}
	for _, n := range s.G.factsIn("") {
		s.known[n.ID] = true
	}
	if err := s.G.Activate(it.ID); err != nil {
		// 意图已是终态（例如本轮开始后被一条 negative 事实证伪）不是致命错误：
		// 轮循环已经拿到 prompt 了，这一轮照跑，只是状态不再变。记下来供报告用。
		s.LastErr = err
	}
}

// Settle 回填一轮的结果。
//
// 产出的判定是**客观的**：本轮新入图的、且不被本次结算之前的意图拥有的新事实。
// 不使用 res.Text 做任何判定——「agent 说它成功了」不是成功（继承前身
// next_open_goal 的原则：只用客观事实推进阶段，绝不把推断写成事实）。
//
// known 为 nil（Activate 没被调用过就直接 Settle）时退化为「图上全部事实」：
// 这样结算不会把历史事实全部算成本轮产出（那会让每个意图一激活就判 done）。
func (s *Scheduler) Settle(it *harness.IntentRef, res harness.RoundResult) {
	if it == nil || s.G == nil {
		return
	}
	var produced []string
	for _, n := range s.G.factsIn("") {
		if s.known != nil && !s.known[n.ID] {
			produced = append(produced, n.ID)
		}
	}
	if err := s.G.Settle(it.ID, produced, res); err != nil {
		s.LastErr = err
	}
	s.activeID = ""
	s.known = nil
}

// Ingest 把一次工具调用事件里的事实灌进图。这是 M5 的接线点：轮循环里
// `for _, ev := range events { sched.Ingest(ev, round) }` 一行接上。
//
// 四条通道在这里汇合，且**全部走 AddFact / AddNegative 这两个校验入口**：
//   - 宿主抽取（host-verified，指纹匹配，不截断输入）；
//   - report_fact 申报（agent-asserted，置信度封顶、vuln 无证据降级）；
//   - agent 申报的 negative（归属当前活动意图，转成 AddNegative）；
//   - agent 的 `next` 建议（转成挂在**本轮产出事实**上的候选意图，见 linkNext）。
//
// 返回值里的 AnswerShaped 是**给 gate 的路由提示**：命中答案形状的内容被图拒收，
// 但它恰恰是 gate 要的候选。图不替 gate 记账（那是两个账本），只是告诉调用方
// 「这些内容你得自己看一眼」。
type IngestResult struct {
	Added        []string
	Duplicates   []string
	AnswerShaped []string
	Negatives    []string
	// Enabled 是 agent 的 `next` 建议派生出的意图 id。它只是**候选**：进了前沿
	// 也要按阶段序/轮次/序号排队，不保证下一轮就被执行（agent 建议不等于调度）。
	Enabled []string
}

// ObserveEvent 实现 harness.Planner。
//
// **为什么是薄包装而不是把 Ingest 改名**：Ingest 返回 IngestResult，而
// harness.Planner 的方法集里 ObserveEvent 没有返回值。Go 不允许同名方法只因
// 返回值不同而共存，所以保留 Ingest（既有测试读它的返回值），再加这一层。
//
// **为什么必须收进接口**（v0.2 的教训）：v0.2 靠 `Session.Ingest func(Event,int)`
// 接线，漏接是**静默**的——DAG 零事实、7 个阶段只有前 4 个可达，而所有包自测
// 全绿（`wiring_test.go:85` 就是为这个写的）。收进接口后漏接变成编译错误。
//
// 返回值被丢弃是**有意的**：AnswerShaped 是给 gate 的路由提示，而 gate 在同一
// 条事件路径上自己也会看到这个事件（engine 的 runLoop 会调 Gate.Observe）。
// 两个账本各自记账，图不替 gate 转交。
func (s *Scheduler) ObserveEvent(ev harness.Event, round int) {
	_ = s.Ingest(ev, round)
}

func (s *Scheduler) Ingest(ev harness.Event, round int) IngestResult {
	var res IngestResult
	if s.G == nil {
		return res
	}
	// 把图上的「当前轮次」推到这个 round。图的产出判定（Settle 里「本轮新建的
	// 事实」）依赖它，而轮次只有调用方知道。不同步的后果是**静默的**：所有事实
	// 的 Round 都大于 g.Round，于是每一轮都被判 failed，阶段链永远推不动，
	// 而表面上一切正常（意图在跑、事实在进图）。
	if round > s.G.Round {
		s.G.Round = round
	}
	for _, n := range s.G.Extract(ev, round) {
		id, err := s.G.AddFact(n)
		switch {
		case err == nil:
			res.Added = append(res.Added, id)
		case isDuplicate(err):
			res.Duplicates = append(res.Duplicates, id)
		case isAnswerShaped(err):
			res.AnswerShaped = append(res.AnswerShaped, n.Content)
		}
	}
	// agent 申报的 negative：意图归属在这里补上（agent 不知道意图 id）。
	if payload, ok := ReportPayload(ev); ok && s.activeID != "" {
		for _, rf := range payload.Facts {
			if !strings.EqualFold(strings.TrimSpace(rf.Kind), string(FactNegative)) {
				continue
			}
			if strings.TrimSpace(rf.Content) == "" {
				continue
			}
			// 来源只记 report_fact，**不带 agent 给的 evidence**：evidence 是
			// agent 自己写的散文，会被渲染进 prompt 的「已证伪」段。凭证/flag
			// 之类的内容混在里面就等于把它又写回上下文。取证要的是「这次调用
			// 的 id」，那在 ToolCallID 里，而不是 agent 的叙述。
			id, err := s.G.AddNegative(s.activeID, rf.Content, "report_fact", TrustAgent, ev.ToolCallID)
			if err == nil {
				res.Negatives = append(res.Negatives, id)
			}
		}
	}
	// agent 的 `next` 建议：转成意图候选。
	if payload, ok := ReportPayload(ev); ok {
		res.Enabled = s.linkNext(payload.Next, res.Added)
	}
	return res
}

// linkNext 把 agent 的 `next` 建议转成一条意图候选。
//
// 为什么必须有这个消费者（核验发现的缺口）：`reportPayload.Next` 原本**没有任何
// 调用点**——设计文档 §一 明写「`next` 作为 EdgeEnables 派生的意图候选」，宿主
// 扩展的参数描述也写着「框架会作为意图候选」，但没有任何代码读它。后果是 agent
// 最有价值的一条信号（「我认为下一步该做什么」）被静默忽略，而它恰恰是「DAG 驱动」
// 相对「写死的阶段链」多出来的那部分。
//
// 四条刻意的分寸：
//
//  1. **挂在产出事实上，而不是活动意图上**。`enables` 边的方向是 fact → intent
//     （edge.go 的方向表），语义是「这个事实派生出了这个候选」。挂在本轮新产出的
//     事实（Added[0]）上，图的审计链才成立：`EnableFrom` 的文档注释写的就是
//     「agent 的 next 建议就走这里」。
//  2. **没有新事实时不连**。纯申报调用（没有 stdout）里 Added 可能是空的，这时
//     「本轮」的判据只剩水位线（`known`），而水位线只在 Activate 时被刷新——
//     一个**没经过 Activate** 的调度器（测试、或宿主先喂事件再 Next）会把图里
//     全部历史事实都当成「本轮的」，于是 agent 的 next 被挂到一条毫不相干的老
//     事实上。悬空边是坏图（Validate 会报「终点缺失」），乱挂的边比悬空更难发现，
//     所以这里宁可放弃：**建议是可选信号，丢一条不影响正确性**。
//  3. **意图类别从阶段链推**，而不是自造一个。IntentKind 是枚举，agent 的建议是一句
//     自然语言；猜类别（「横向」→ lateral）会造出与阶段链无关的意图，抢在真实阶段
//     前面。用**还没做完的那个阶段**最诚实：它表达的正是「这条建议该插在哪儿」。
//  4. **候选不等于调度**（设计文档 §一 的原话）。它只是一个普通意图节点，和阶段链
//     上的意图一起排队，能不能被选中由前沿优先级决定。
//
// 失败一律静默：这是**可选**的加分信号，让一条自然语言建议的解析失败影响事实入库
// 是本末倒置。但意图为空目标会被 AddIntent 拒（ErrNoContent），所以空 `next` 在
// 这里就返回，不留空意图。
func (s *Scheduler) linkNext(next string, added []string) []string {
	goal := normalize(next)
	if goal == "" {
		return nil // 空建议 ⇒ 不造空意图（空目标会被 AddIntent 拒，而且它会占一个前沿名额）
	}
	// 目标长度封顶：`next` 是模型自由文本，一条几千字的建议会把 prompt 的
	// 「本轮意图」段整个撑满（那段是全 prompt 最贵的位置）。截断而不是拒收——
	// 前 400 字符足够表达「下一步做什么」。
	goal = truncate(goal, maxNextGoalLen)

	// 锚点取**本轮新产出**的事实：`added` 只装 `AddFact` 返回 nil 的那些（重复的
	// 进 Duplicates、被拒的进 AnswerShaped），所以它天然就是「本轮新建的事实」，
	// 不需要再按轮次过滤。这很关键——enables 边的语义是「这个事实派生出了这个
	// 候选」，挂到一条早就存在的事实上会让「谁派生了这条建议」这条审计链失真，
	// 而失真的审计链比没有链更糟（它会给出一个看起来合理的错误答案）。
	anchor := ""
	for _, id := range added {
		if n := s.G.nodes[id]; n != nil && n.IsFact() {
			anchor = id
			break
		}
	}
	if anchor == "" {
		return nil
	}
	id, err := s.G.EnableFrom(anchor, Node{
		Kind:       NodeIntent,
		IntentKind: s.nextIntentKind(),
		Goal:       goal,
	})
	// 重复的建议（同一段文本第二次出现）不再回报：EnableFrom 会把 enables 边补上
	// （首现优先，派生关系不丢），但**没有新节点产生**。回报它会让调用方把「同一个
	// 候选被重复建议」误当成「又发现了一个方向」——而 agent 在一轮里重复说同一句
	// 建议是常态（它在每个 report_fact 调用里都会带上 next）。
	if err != nil {
		return nil
	}
	return []string{id}
}

// maxNextGoalLen 是 agent 建议的目标文本上限（见 linkNext 的说明）。
const maxNextGoalLen = 400

// nextIntentKind 给出「agent 建议的下一步」该用哪个意图类别：阶段链里下一个还
// 没做完的阶段。
//
// 为什么不猜（例如按建议文本里的关键词映射到 lateral/escalate）：IntentKind 是
// 枚举，关键词映射会造出与阶段链无关的意图，而前沿排序的第一键就是阶段序——
// 一个「猜出来的」类别可能插到真实阶段前面，把阶段链的推进顺序打乱。
// 阶段链的推进本来就有自己的判据（Settle 的客观产出），agent 的建议只是让它
// 提前出现在前沿里，不该改变「现在处于哪个阶段」。
//
// 链上阶段全做完时用 verify：那时前沿本来就快空了，一条额外的候选正好补位。
func (s *Scheduler) nextIntentKind() IntentKind {
	chain := PhaseChain(s.G.Category)
	for _, k := range chain {
		// 「做完」的判据与 executable 一致：终态（done/abandoned）之外的都还没完。
		// 这里不看具体哪个意图节点，只看这个阶段是否还有活着的意图——种子链
		// 铺满全部阶段，所以没做完的阶段一定在。
		alive := false
		for _, id := range s.G.order {
			n := s.G.nodes[id]
			if n.IsIntent() && n.IntentKind == k &&
				n.State != IntentDone && n.State != IntentAbandoned {
				alive = true
				break
			}
		}
		if alive {
			return k
		}
	}
	return IntentVerify
}

// 关于「平台判错的答案」为什么**不**在这里转成 negative 事实：
//
// 一开始的设计是把它转成 negative 写进图，让「这个方向被平台否了」进入下一轮的
// 「已证伪」段。但那会引入一个**新的 flag 明文泄漏面**：negative 事实的内容会被
// 渲染进 prompt、落盘进 dag.json、推给态势台，而判错账本（Ledger）本来只回灌
// 指纹（`_scrub_flag_plaintext` 的教训）。而且一个 flag 候选被否，并不等于「这条
// 攻击路径死了」——它往往只是值抄错了。所以判错的回灌仍由 Ledger 的
// `Fingerprints()` 负责（渲染器的 Rejected 字段），图只记真正的死胡同。
//
// 这段注释是刻意留的：删除一个看起来有用的机制，必须留下「为什么不这么做」，
// 否则下一个人会把它加回来。

// seedChain 铺目标链。只在图上一条意图都没有时执行一次。
//
// 阶段链的语义逐条移植前身 `goals_for_category()`：pentest 走
// recon→exploit→foothold→extract→lateral→escalate→verify，
// crypto/misc/forensics/reverse 走 analyze→solve→verify，
// 其余走 recon→exploit→foothold→escalate→verify。
//
// **不**把链上的阶段连成 requires 边：阶段顺序已经由优先级表达（阶段序是第一
// 排序键），再连 requires 只会让「前置未满足就跳过」这条剪枝把整条链锁死——
// 而真实情况是 agent 常常在 recon 之前就先读了题面源码（analyze）。
// requires 留给**真实的**前置依赖（例如「提权」需要先有一条 foothold 事实）。
//
// 判据必须看**全部节点**，而不是「前沿为空」：跑完若干轮后意图全都 done/abandoned
// 时前沿也为空，那时再铺一次链会让已经做完的阶段重来一遍（前身「每次都从 recon
// 开始」的同款失败）。
func (s *Scheduler) seedChain() {
	if s.G == nil {
		return
	}
	for _, n := range s.G.Nodes() {
		if n.IsIntent() {
			return
		}
	}
	for _, k := range PhaseChain(s.G.Category) {
		_, _ = s.G.AddIntent(Node{
			Kind:       NodeIntent,
			IntentKind: k,
			Goal:       PhaseGoal(s.G.Category, k),
			Expect:     phaseExpect(k),
		})
	}
}

// phaseGoals 是各阶段的自然语言指令（渲染进 prompt 的那句话）。
//
// 措辞要求：一句话说清「做什么」+「产出什么算成功」。前身的目标链描述是
// 「侦察: 发现目标服务、端口和技术栈」这种短语，agent 拿到后还得自己翻译成命令；
// 这里写成可直接执行的动作，省掉一层翻译（也减少翻译走样的机会）。
var phaseGoals = map[IntentKind]string{
	IntentRecon: "侦察：枚举目标开放端口与服务指纹（nmap/whatweb/curl -I 等），" +
		"把看到的地址、服务与版本记录成事实。",
	IntentAnalyze: "分析：读题目给的材料（源码/二进制/密文/附件/题面细节），" +
		"弄清它的结构与编码/加密方式，不要急着写 exploit。",
	IntentExploit: "验证并利用一个具体假设：挑一条最有把握的线索，" +
		"动手验证它是否真的成立（成立则给出可复现的命令与输出）。",
	IntentFoothold: "获取初始访问：拿到一个能执行命令的立足点" +
		"（shell / RCE / 任意文件上传 / 已知口令登录）。",
	IntentExtract: "提取：从已有立足点或已知线索中取出答案" +
		"（flag / 密码 / 密钥 / 提交所需的值），注意按题目要求的原始格式。",
	IntentLateral: "横向：用已获凭证移动到其他主机或服务，" +
		"记录新发现的地址与凭证。",
	IntentEscalate: "提权：把当前权限提升到 root/admin，" +
		"（读 /root、/etc/shadow、管理员目录等）并记录证据。",
	IntentVerify: "复核：确认候选答案的形态与来源，按题目要求的格式投递，" +
		"并核对它确实来自目标而不是自己写下的内容。",
	IntentRecover: "环境处置：目标不通 / 容器崩坏时确认实际状态" +
		"（端口、DNS、VPN），给出可继续的下一步或明确标记环境阻塞。",
}

// PhaseGoal 返回某类别某阶段的目标描述。
func PhaseGoal(category string, k IntentKind) string {
	if g, ok := phaseGoals[k]; ok {
		return g
	}
	// 链外类别（自定义意图）没有预设措辞：用类别名兜底，总比空目标好——
	// 空目标会被 AddIntent 拒绝（不变量 6）。
	return string(k) + "：推进该方向并记录事实。"
}

// phaseExpect 给出各阶段的预期产出（用于渲染「预期产出」与人工判读）。
//
// 注意它**不参与 done/failed 判定**（判定看的是客观新事实，见 Settle）。保留
// 这个字段是为了渲染：告诉 agent「这一步要产出什么类别的事实」，它才知道该
// 申报什么——前身没有这个信息，agent 常常拿到了立足点却申报成 service。
func phaseExpect(k IntentKind) []FactKind {
	switch k {
	case IntentRecon:
		return []FactKind{FactTarget, FactService}
	case IntentAnalyze:
		return []FactKind{FactArtifact, FactVuln}
	case IntentExploit:
		return []FactKind{FactVuln, FactNegative}
	case IntentFoothold:
		return []FactKind{FactFoothold, FactNegative}
	case IntentExtract:
		return []FactKind{FactCredential, FactFoothold}
	case IntentLateral:
		return []FactKind{FactTarget, FactCredential}
	case IntentEscalate:
		return []FactKind{FactFoothold, FactNegative}
	case IntentVerify:
		return []FactKind{FactArtifact}
	case IntentRecover:
		return []FactKind{FactNegative}
	}
	return nil
}

// ── Renderer：harness.Renderer 的实现 ──

// Renderer 把图渲染成本轮 prompt。它是 Render 纯函数的薄适配层——所有逻辑都在
// Render 里（那才能做 golden 测试），这里只负责从 harness 的类型里取输入。
type Renderer struct {
	G *Graph
	// MaxFacts / MaxNegative 透传给 RenderInput（0 用默认值）。
	MaxFacts    int
	MaxNegative int
}

var _ harness.Renderer = (*Renderer)(nil)

// Render 实现 harness.Renderer。
//
// 签名用 *OutcomeView（v0.2 是 *Outcome）：`Flags` 与 `Candidates` 两个字段的
// 语义完全没变，所以函数体只有类型名的差异。
func (r *Renderer) Render(_ context.Context, ch harness.Challenge, it *harness.IntentRef, out *harness.OutcomeView) string {
	if r.G == nil {
		return ""
	}
	in := RenderInput{
		Challenge:   ch,
		MaxFacts:    r.MaxFacts,
		MaxNegative: r.MaxNegative,
	}
	if out != nil {
		in.Confirmed = len(out.Flags)
		// 判错账本回灌**只给指纹**。去重：同一答案可能被记多次（多个候选同值）。
		//
		// 判据是 SubmitError（**平台说不对**），不是 RejectReason（gate 觉得可疑）。
		// 两者是不同的东西，混用会造成一个具体的伤害：gate 把某候选标成
		// agent_authored 只是因为命令里出现过它，但那个答案可能完全正确 ——
		// 把它当「已试过、错的」回灌，会让 agent 主动避开正确答案。
		// 只有平台明确判错的才回灌。
		seen := map[string]bool{}
		for _, c := range out.Candidates {
			if c.SubmitError == "" || c.Flag == "" {
				continue
			}
			fp := FlagFingerprint(c.Flag)
			if seen[fp] {
				continue
			}
			seen[fp] = true
			in.Rejected = append(in.Rejected, fp)
		}
	}
	if it != nil {
		in.Intent = r.G.Node(it.ID)
	}
	// ch 可能缺 Description（平台在 Start 才给全）——从图里的题面事实兜底：
	// 目标地址本来就是从 FactTarget 渲染的，这里只需保证题面不为空。
	return Render(r.G, in)
}

// ── 便利方法（供宿主与测试构造分支点/前置）──

// Require 声明 intent 的前置事实。
//
// 只在**真实**前置上用它（「提权」需要先有 foothold 事实）。滥用会让意图永久
// 卡在 blocked——那是「跳过 requires 未满足」这条剪枝的反面代价。
func (g *Graph) Require(intentID, factID string) error {
	return g.Link(intentID, factID, EdgeRequires, g.Round)
}

// EnableFrom 由一个事实派生出新意图（分支点：一个事实 enables 多个候选意图）。
//
// agent 的 `next` 建议就走这里——**但仍需过宿主的前沿优先级**（agent 建议不等于
// 调度）：它只是一个普通意图节点，和阶段链上的意图一起排队。
func (g *Graph) EnableFrom(factID string, n Node) (string, error) {
	id, err := g.AddIntent(n)
	if err != nil {
		if isDuplicate(err) {
			// 重复的意图也要把 enables 边连上：这样「谁派生了它」不会因为
			// 第二次发现而丢失（首现优先）。
			_ = g.Link(factID, id, EdgeEnables, g.Round)
		}
		return id, err
	}
	if err := g.Link(factID, id, EdgeEnables, g.Round); err != nil {
		return id, err
	}
	return id, nil
}

// DeriveFrom 声明一条事实由另一条事实推导而来（provenance 链）。
//
// 用途：宿主规则推导出的 inferred 事实必须挂在它的来源上，否则推导链断掉，
// gate 的证据闸就无从判断「这条推断到底站不站得住」。
func (g *Graph) DeriveFrom(factID, fromFactID string) error {
	return g.Link(factID, fromFactID, EdgeDerivedFrom, g.Round)
}

// Supersede 用新意图取代旧意图（换方向）。
func (g *Graph) Supersede(oldIntentID, newIntentID string) error {
	return g.Link(newIntentID, oldIntentID, EdgeSupersedes, g.Round)
}

// isDuplicate / isAnswerShaped 是给调用方的分类判据（避免它们各自 import errors
// 再拼 errors.Is——分类只有一处定义才不会漂移）。
func isDuplicate(err error) bool    { return err != nil && errors.Is(err, ErrDuplicate) }
func isAnswerShaped(err error) bool { return err != nil && errors.Is(err, ErrAnswerShaped) }
