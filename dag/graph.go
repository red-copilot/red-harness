package dag

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

// 违规错误。全部是**具名**错误，因为调用方需要按类别分流：
//   - ErrAnswerShaped 要被路由到 gate 的候选账本（不是丢弃）；
//   - ErrDuplicate 要拿到已存在节点的 id 继续连边；
//   - 其余是真正的编程错误。
var (
	// ErrNoSource 事实必须有来源。
	ErrNoSource = errors.New("dag: 事实必须有来源（Source 为空）")
	// ErrNoContent 事实/意图必须有非空内容。
	ErrNoContent = errors.New("dag: 内容不能为空")
	// ErrAnswerShaped 内容命中本题的答案形状 ⇒ 拒入事实库。
	ErrAnswerShaped = errors.New("dag: 内容命中答案形状，拒入事实库（答案只属于 gate 的候选账本）")
	// ErrUnknownKind 未知的类别。
	ErrUnknownKind = errors.New("dag: 未知的类别")
	// ErrDuplicate 已存在（事实按 (FactKind, normalize(Content))、意图按
	// (IntentKind, normalize(Goal)) 去重）。返回的 id 是已存在节点的 id。
	ErrDuplicate = errors.New("dag: 重复，已存在")
	// ErrNotFound 节点不存在。
	ErrNotFound = errors.New("dag: 节点不存在")
	// ErrNegativeNoRefutes negative 事实必须带被证伪的意图 id。
	ErrNegativeNoRefutes = errors.New("dag: negative 事实必须带被证伪的意图 id（refutes 边的存在性是入库条件）")
	// ErrNoEvidence vuln/foothold 事实必须带证据引用。
	ErrNoEvidence = errors.New("dag: vuln/foothold 事实必须带证据引用（ToolCallID 或 Evidence）")
	// ErrInferred vuln/foothold 不能只靠推断成立。
	ErrInferred = errors.New("dag: inferred 事实不能单独构成 vuln/foothold")
	// ErrEdgeKind 边的端点类型不合法。
	ErrEdgeKind = errors.New("dag: 边的端点类型不合法")
	// ErrBadState 意图当前状态不允许该操作。
	ErrBadState = errors.New("dag: 意图状态不允许此操作")
)

// Rejection 是一条被拒写入的审计记录。
//
// 为什么要留它（前身 B14 的教训）：前身某题攒了 61 条垃圾凭证事实，注入下一场时
// 把真信号挤没——但**没人知道**这件事，因为没有账。被拒的东西必须看得见，否则
// 「质量闸太紧」和「质量闸太松」都无法被发现。
type Rejection struct {
	Kind     NodeKind `json:"kind"`
	FactKind FactKind `json:"factKind,omitempty"`
	Content  string   `json:"content,omitempty"`
	Reason   string   `json:"reason"`
	Round    int      `json:"round,omitempty"`
}

// Graph 是宿主侧的权威事实—意图图。
//
// 它是**唯一**的写入通道：所有写操作都过校验，违规返回 error 而不是 panic——
// 图跑在长驻的驱动进程里，一次 panic 会带走整道题（甚至整轮跑分）。
type Graph struct {
	// Code / Category 是题目身份，落盘时保留，Load 后无需再喂 Challenge。
	Code     string `json:"code"`
	Category string `json:"category"`
	// Shape 是本题的答案形态（答案形状的唯一真源在 answer 包）。它是
	// 「答案形状内容拒入图」这条不变量的判据。
	Shape answer.Shape `json:"shape"`

	nodes     map[string]*Node
	order     []string
	edges     []Edge
	edgeSeen  map[string]bool
	factIdx   map[string]string // (FactKind, normalize(Content)) → id
	intentIdx map[string]string // (IntentKind, normalize(Goal))   → id
	rejected  []Rejection

	seq int
	// Round 是当前轮次。AddFact/AddIntent 用它给节点打时间戳，Link 用它记连边
	// 发生的轮次（取证要能回答「第几轮才知道的」）。
	Round int
	// Now 可注入时钟（测试与 golden 用）。为 nil 时用 time.Now。
	Now func() time.Time
}

// New 用题目初始化一张图。
//
// 题目给出的地址会写成 FactTarget 事实，Source 标 platform——这是「事实必须有
// 来源」这条不变量的**唯一例外**：平台题面不是工具输出，但它同样是客观来源，
// 而不是 agent 的自述。seed 路径专门为它存在（见 addFact 的 seeding 参数）。
func New(ch harness.Challenge) *Graph {
	g := &Graph{
		Code:     ch.Code,
		Category: ch.Category,
		Shape:    inferShape(ch),
	}
	g.init()
	for _, a := range ch.Addrs {
		_, _ = g.addFact(Node{
			Kind:       NodeFact,
			FactKind:   FactTarget,
			Content:    a,
			Source:     SourcePlatform,
			Confidence: 1,
			Trust:      TrustHost,
		}, true)
	}
	return g
}

// inferShape 从题面 + 平台下发的格式推断答案形态。
//
// 两个来源都喂进去：题面可能只说「提交密码」（裸串），平台可能另外下发
// `flag{...}`（信封）。任何一处漏掉都会让形态判定偏窄，而漏认的代价是丢分。
func inferShape(ch harness.Challenge) answer.Shape {
	src := strings.TrimSpace(ch.Description + " " + ch.FlagFormat)
	if src == "" {
		// 空题面也要给出兜底形态：Infer("") 会同时认信封与裸串（宁可多认）。
		return answer.Infer("")
	}
	return answer.Infer(src)
}

// SetShape 覆盖本题的答案形态。
//
// 用途：平台在 Start 时才给全 FlagFormat，而图可能已经用题面推断建好了；调用方
// 拿到更准的形态后可以覆盖它。注意覆盖**不会**回溯清洗已入图的内容——所以
// 调用方应在建图后立刻设置（Load 之后设置也要重新 Validate 一遍）。
func (g *Graph) SetShape(s answer.Shape) { g.Shape = s }

func (g *Graph) init() {
	if g.nodes == nil {
		g.nodes = map[string]*Node{}
		g.edgeSeen = map[string]bool{}
		g.factIdx = map[string]string{}
		g.intentIdx = map[string]string{}
	}
}

func (g *Graph) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// ── 读取 ──
//
// 所有导出的读方法都返回**克隆**。这不是洁癖：图的不变量靠「写入即校验」维持，
// 如果外部能拿到活指针直接改 State/Content，校验就被绕过了（例如把一条
// agent 自述的 artifact 改成 vuln）。克隆换来的是「唯一的写入通道」这条性质。

// Node 返回节点的克隆；不存在时返回 nil。
func (g *Graph) Node(id string) *Node { return cloneNode(g.nodes[id]) }

func cloneNode(n *Node) *Node {
	if n == nil {
		return nil
	}
	c := *n
	if n.Expect != nil {
		c.Expect = append([]FactKind(nil), n.Expect...)
	}
	return &c
}

// Nodes 返回全部节点（创建顺序，确定性）。态势台与报告用。
func (g *Graph) Nodes() []*Node {
	out := make([]*Node, 0, len(g.order))
	for _, id := range g.order {
		out = append(out, cloneNode(g.nodes[id]))
	}
	return out
}

// Edges 返回全部边（创建顺序）。
func (g *Graph) Edges() []Edge { return append([]Edge(nil), g.edges...) }

// Rejections 返回被拒写入的审计记录。
func (g *Graph) Rejections() []Rejection { return append([]Rejection(nil), g.rejected...) }

// Facts 返回某类别的事实；kind 为空时返回全部事实。按 (Round, Seq) 排序，
// 这样渲染进 prompt 的顺序不随 map 迭代漂移。
func (g *Graph) Facts(kind FactKind) []*Node {
	var out []*Node
	for _, id := range g.order {
		n := g.nodes[id]
		if !n.IsFact() {
			continue
		}
		if kind != "" && n.FactKind != kind {
			continue
		}
		out = append(out, cloneNode(n))
	}
	sortByRoundSeq(out)
	return out
}

// factsIn 与 Facts 相同，但返回**内部指针**。它是包内专用（未导出）：渲染器与
// 调度器要读几十条事实的字段，逐条深拷贝纯属浪费。**导出 API 一律走 Facts（返回
// 克隆）**——单写通道（只有 AddFact/AddIntent 能改图）是这份实现最重要的性质，
// 不能在「只是读一下」的借口下破坏。
func (g *Graph) factsIn(kind FactKind) []*Node {
	var out []*Node
	for _, id := range g.order {
		n := g.nodes[id]
		if !n.IsFact() {
			continue
		}
		if kind != "" && n.FactKind != kind {
			continue
		}
		out = append(out, n)
	}
	sortByRoundSeq(out)
	return out
}

// Negative 返回全部死胡同事实。它们通过 refutes 边把意图永久移出前沿，是
// 「不要再试」的唯一真源。
func (g *Graph) Negative() []*Node { return g.Facts(FactNegative) }

// Stats 是给态势台/报告的一行摘要。
type Stats struct {
	Facts    int                 `json:"facts"`
	Intents  int                 `json:"intents"`
	Edges    int                 `json:"edges"`
	Negative int                 `json:"negative"`
	Rejected int                 `json:"rejected"`
	ByFact   map[FactKind]int    `json:"byFact,omitempty"`
	ByState  map[IntentState]int `json:"byState,omitempty"`
}

// Stats 汇总当前图的状态。
func (g *Graph) Stats() Stats {
	st := Stats{Edges: len(g.edges), Rejected: len(g.rejected),
		ByFact: map[FactKind]int{}, ByState: map[IntentState]int{}}
	for _, id := range g.order {
		n := g.nodes[id]
		if n.IsFact() {
			st.Facts++
			st.ByFact[n.FactKind]++
			if n.FactKind == FactNegative {
				st.Negative++
			}
			continue
		}
		st.Intents++
		st.ByState[n.State]++
	}
	return st
}

// ── 写入：事实 ──

// AddFact 写入一条事实，返回节点 id。违规返回 error（不 panic）。
//
// 六条不变量在这里逐条落地（详见每条分支的注释）。**注意 duplicate 的特殊性**：
// 重复不是错误状态，但必须让调用方知道「没有新事实产生」——轮循环靠它判断
// 本轮是否 dry，意图结算靠它判断 done/failed。所以返回已存在节点的 id **加上**
// ErrDuplicate，调用方用 errors.Is 分流。
func (g *Graph) AddFact(f Node) (string, error) { return g.addFact(f, false) }

// addFact 是唯一的事实写入口。seeding 为真时走「题目初始化」例外路径：
// Source 缺省填 platform（平台题面是客观来源，不是 agent 自述）。
func (g *Graph) addFact(f Node, seeding bool) (string, error) {
	g.init()
	f.Kind = NodeFact
	if f.Content == "" || strings.TrimSpace(f.Content) == "" {
		return "", g.reject(f, ErrNoContent) // 不变量 6
	}
	if f.Source == "" {
		if !seeding {
			return "", g.reject(f, ErrNoSource) // 不变量 1
		}
		f.Source = SourcePlatform
	}
	if !f.FactKind.Valid() {
		// 不变量 2 的一半：`flag` / `answer` 不是 FactKind，类型上就不存在。
		return "", g.reject(f, fmt.Errorf("%w: %q", ErrUnknownKind, f.FactKind))
	}
	if g.answerShaped(f.Content, f.FactKind) {
		// 不变量 2 的另一半，也是整个设计里最重要的一条：agent 写
		// `echo 'flag{x}' > /tmp/f` 再 `cat /tmp/f`，输出里就出现了 flag——自己的
		// 猜测被自己读回来，洗成了「观测」。这里在**入库口**拒收，于是那条洗白
		// 路径从根上不存在（前身为此写了 3,592 行事后分析）。
		return "", g.reject(f, ErrAnswerShaped)
	}
	if f.Trust == "" {
		f.Trust = TrustHost
	}
	switch f.FactKind {
	case FactNegative:
		// 不变量 3：negative 事实必须指明被证伪的意图，且该意图必须存在——
		// 「refutes 边的存在性」是入库条件，而不是入库之后的补充动作。
		if f.Refutes == "" {
			return "", g.reject(f, ErrNegativeNoRefutes)
		}
		tgt := g.nodes[f.Refutes]
		if tgt == nil || !tgt.IsIntent() {
			return "", g.reject(f, fmt.Errorf("%w: refutes 指向的意图 %q 不存在", ErrNegativeNoRefutes, f.Refutes))
		}
	case FactVuln, FactFoothold:
		// 不变量 4：vuln/foothold 必须有证据引用。agent 自述不能单独构成 vuln——
		// 这条是「不信任 agent」的执行点。
		if f.ToolCallID == "" && f.Evidence == "" {
			return "", g.reject(f, ErrNoEvidence)
		}
		if f.Trust == TrustInferred {
			// inferred 由其它事实推导而来，它**永不单独构成** vuln/foothold：
			// 推导链本身不是证据，除非它挂在一条带证据的事实上（Validate 复查）。
			return "", g.reject(f, ErrInferred)
		}
	}
	if f.Confidence == 0 {
		f.Confidence = defaultConfidence(f.FactKind)
	}
	if f.Trust == TrustAgent && f.Confidence > AgentConfidenceCap {
		// agent 自述的置信度封顶：它可以很确信，但宿主不允许它把确信度当证据。
		f.Confidence = AgentConfidenceCap
	}

	key := factKey(f.FactKind, f.Content)
	if id, ok := g.factIdx[key]; ok {
		return id, ErrDuplicate // 不变量 5
	}

	if f.ID == "" {
		f.ID = g.nextID("f")
	}
	f.CreatedAt = g.now()
	f.UpdatedAt = f.CreatedAt
	if f.Round == 0 {
		f.Round = g.Round
	}
	g.seq++
	f.Seq = g.seq
	f.Content = normalize(f.Content)

	stored := f
	g.nodes[f.ID] = &stored
	g.order = append(g.order, f.ID)
	g.factIdx[key] = f.ID

	// negative 事实的副作用：连 refutes 边并把被证伪的意图标 abandoned。
	// 放在这里而不是让调用方记得调用 Refute——死胡同必须立刻生效，否则
	// 下一轮 NextIntent 还会把它挑出来（那正是前身最贵的浪费）。
	if f.FactKind == FactNegative {
		if err := g.Link(f.ID, f.Refutes, EdgeRefutes, g.Round); err != nil {
			return f.ID, err
		}
	}
	return f.ID, nil
}

// AgentConfidenceCap 是 agent 自述事实的置信度上限。0.7 是设计文档定的：
// 它足以让 agent 的判断参与排序，但不足以让它单独压过宿主指纹匹配的结果。
const AgentConfidenceCap = 0.7

func defaultConfidence(k FactKind) float64 {
	switch k {
	case FactTarget:
		return 1.0
	case FactService, FactFoothold:
		return 0.8
	case FactVuln:
		return 0.7
	case FactCredential:
		return 0.6
	case FactArtifact:
		return 0.5
	case FactNegative:
		return 0.9 // 死胡同是高置信的：证伪比证实容易做对
	}
	return 0.5
}

// ── 写入：意图 ──

// AddIntent 写入一条意图，返回节点 id。违规返回 error。
//
// ── 目标里的答案形状：净化，不拒收 ──
//
// 与事实不同，意图的 Goal **不**做答案形状拒收：agent 说「验证 flag{...} 是否正确」
// 是**合法且有价值**的意图（它就是要去验证那个猜测），而「这个串是猜的还是观测到
// 的」由 gate 的**首现优先**判定——首现于 intent 散文 ⇒ 幻觉族，永不提交。整条
// 拒掉只会让猜测消失得无影无踪，反而丢掉了判定所需的证据。
//
// 但**原样存下也不行**：Goal 会被渲染进下一轮 prompt（render.go 的 writeIntent），
// 而 prompt 会进 transcript / 报告 / 日志。这是一条真实的泄漏面——`report_fact`
// 载荷的 `next` 字段（agent 建议的下一步）设计上要经 EnableFrom 变成意图，于是
// agent 只要把 flag 明文写进 next，明文就绕开事实库的答案形状闸、直接出现在下一轮
// prompt 里。前身 B52 就是这么漏的（flag 明文进了 MEMORY.md / _blackboard.json，
// 事后靠 `_scrub_flag_plaintext` 擦）。
//
// 所以分寸是：**意图保留，目标文本净化**——命中的信封形态原地换成指纹
// （FlagFingerprint，只留哈希前 8 位 + 长度 + 首尾字符，见 store.go）。
//
// 为什么裸串形态不在这里替换（判据为什么与 AddFact **不完全**同一套）：见
// sanitizeGoal 的三条理由，核心是**阶段目标措辞本身**会被裸串判据误判（题面提到
// 「密码/密钥」⇒ AllowRaw 为真 ⇒ 「取出答案（flag / 密码 / 密钥 / 提交所需的值）」
// 这种固定措辞被整句替换成指纹，agent 拿到一条只有指纹的意图）。裸串那一路由
// 落盘口的 scrub 与 gate 的首现优先共同覆盖。
//
// 去重键在**净化之后**算：否则「验证 flag{a}」与「验证 flag{b}」会先各自拿到唯一
// 的键、再以同样的净化结果双双入库，出现两条目标逐字相同的意图（而 flag{a} 与
// flag{b} 的指纹不同，本来就该是两条）。
func (g *Graph) AddIntent(n Node) (string, error) {
	g.init()
	n.Kind = NodeIntent
	if strings.TrimSpace(n.Goal) == "" {
		return "", g.reject(n, ErrNoContent)
	}
	if !n.IntentKind.Valid() {
		return "", g.reject(n, fmt.Errorf("%w: %q", ErrUnknownKind, n.IntentKind))
	}
	n.Goal = g.sanitizeGoal(n.Goal)
	key := intentKey(n.IntentKind, n.Goal)
	if id, ok := g.intentIdx[key]; ok {
		return id, ErrDuplicate
	}
	if n.ID == "" {
		n.ID = g.nextID("i")
	}
	if n.State == "" {
		n.State = IntentPending
	}
	if n.MaxAttempts <= 0 {
		n.MaxAttempts = DefaultMaxAttempts
	}
	n.CreatedAt = g.now()
	n.UpdatedAt = n.CreatedAt
	if n.Round == 0 {
		n.Round = g.Round
	}
	g.seq++
	n.Seq = g.seq
	n.Goal = normalize(n.Goal)

	stored := n
	g.nodes[n.ID] = &stored
	g.order = append(g.order, n.ID)
	g.intentIdx[key] = n.ID
	return n.ID, nil
}

// ── 去重键与规范化 ──

func factKey(k FactKind, content string) string {
	return string(k) + "\x00" + fold(content)
}

func intentKey(k IntentKind, goal string) string { return string(k) + "\x00" + fold(goal) }

// trailingSlashRe 去掉 URL 结尾的斜杠，让 `http://10.0.0.1:80/` 与
// `http://10.0.0.1:80` 归一到同一条事实。
var trailingSlashRe = regexp.MustCompile(`/+$`)

// normalize 是入库内容的归一：去首尾空白 + 折叠内部空白 + 去尾部斜杠。
//
// **刻意不做小写化**。这一点与直觉相反（大小写不同看起来是同一件事），但凭证
// 事实的载荷是**大小写敏感**的：`Admin@123` 与 `admin@123` 是两个不同的口令。
// 前身 blackboard 的 `_norm()` 做了小写化，于是 `admin:Admin@123` 落库后变成
// `admin:admin@123`——注入下一场时 agent 拿到的密码是错的，而它完全无法察觉
// （看起来完全正常，只会反复登录失败）。这是移植时必须修掉的一处。
//
// 大小写只在**去重键**里折叠（见 fold）。方向是对的：等价判定比存下来的内容
// **更宽松**是安全的（把 `NGINX` 与 `nginx` 当同一条服务，不改变任何载荷）；
// 反过来——判定严格而内容被悄悄改写——才是上面那类事故。
//
// 同样刻意**不**做更激进的等价（例如把 http/https、带端口/不带端口合并）：过度
// 归一会把「两个不同的服务」并成一条，而漏掉一个端口的代价是丢掉一条攻击面。
// 去重的收益远小于误并的代价，所以这里只做无争议的规范化。
func normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	return trailingSlashRe.ReplaceAllString(s, "")
}

// fold 是**去重键专用**的大小写折叠。
func fold(s string) string { return strings.ToLower(normalize(s)) }

// credObservationRe 判定「键: 值」形态的观察记录（`admin:Admin@123`）。
//
// 与 answer 包内 assignmentRe 的关系：**同源但更窄**。answer 的 assignmentRe 服务
// 于「这段文本是不是答案」的判定，它要处理表单字段（`login ==`）、状态码
// （`admin: 500`）等噪音；这里只服务于「这条事实的内容是不是答案本身」这一件事，
// 所以键表只留凭证相关的键，且要求值非空。
//
// 为什么需要它：凭证事实的形态天然是 `键: 值`（observation），而答案本身是裸值。
// 若把整串 `admin:Admin@123` 当作答案形状拒收，所有正常凭证都进不了图——
// 而它正是渲染进 prompt 的「已知事实」里最有用的一类。
var credObservationRe = regexp.MustCompile(
	`^[ \t]*(?i:username|user|login|admin|root|password|passwd|pwd|pass|name|` +
		`email|account|token|key|secret)[ \t]*[:=][ \t]*\S`)

// answerShaped 判定一条事实的内容是否**就是**本题的答案。
//
// 两条判据，覆盖两种答案形态：
//
//  1. **信封形态**：内容里出现 `flag{...}` 之类。无条件检查，任何事实类别都不例外。
//     这一条直接对应前身那条洗白路径（`echo 'flag{x}' > /tmp/f; cat /tmp/f`）。
//  2. **裸串形态**：只对「可能逐字承载答案」的事实类别生效，且要求内容是**单个
//     无空白的 token**、不是 `键: 值` 观察记录。
//
// 为什么裸串这一半要加这么多限定：answer.LooksLike 是为**单个短串**设计的判据
// （任何含数字且长度 ≥ 6 的串都算「像答案」），直接套在事实内容上会大面积误杀——
// `10.0.0.1:80`、`/var/www/html/index.php`、`22 端口 SSH 未开放` 全都会被判成
// 答案形状。误杀事实的代价（丢掉攻击面）远大于漏收一条可疑内容的代价，所以这里
// 采取**更窄**的判定：只有「整条内容就是一个裸值」才算。
func (g *Graph) answerShaped(content string, kind FactKind) bool {
	if envelopeHit(g.Shape, content) {
		return true
	}
	return g.bareAnswerShaped(content, kind)
}

// bareAnswerShaped 是裸串那一半的判定：整条内容就是**一个裸值**，且它像答案。
//
// 为什么 target / service 被排除：这两类的内容天然是地址与指纹
// （`10.0.0.1:80`、`nginx/1.18.0`），它们含数字含符号，会被 answer.LooksLike 一律
// 判成「像答案」。误杀它们的代价是丢掉整个攻击面——比漏收一条可疑内容严重得多。
//
// artifact **不在排除之列**：它可能是源码路径（有斜杠、有空白、多半带扩展名，
// 走不到这一步），也可能是 agent 从输出里抄下来的一条裸串——而那正是前身那条
// 洗白路径的落点（`echo 'flag{x}' > /tmp/f` 的内容以 artifact 的身份入图）。
func (g *Graph) bareAnswerShaped(content string, kind FactKind) bool {
	if !g.Shape.AllowRaw {
		return false
	}
	switch kind {
	case FactTarget, FactService:
		return false
	}
	c := strings.TrimSpace(content)
	if strings.ContainsAny(c, " \t\r\n") {
		return false // 有空白 ⇒ 是描述，不是答案本身
	}
	if strings.Contains(c, "/") {
		// 含斜杠 ⇒ 是路径或 URL（`/var/www/html/index.php`、`/admin`、
		// `http://10.0.0.1:8080/upload`），不是裸答案。这条规则是必要的：路径
		// 含数字含符号，会被 answer.LooksLike 一律判成「像答案」，而误杀一条
		// artifact 就等于丢掉一个攻击面。代价是漏认「含斜杠的 base64 答案」，
		// 但那类答案的真正观测路径在 gate（工具输出首现），不依赖事实库。
		return false
	}
	if credObservationRe.MatchString(c) {
		return false // `键: 值` ⇒ 是观察记录，不是答案本身
	}
	return g.Shape.LooksLike(c)
}

// sanitizeGoal 净化意图目标：命中的答案形状换成指纹，其余原样保留。
//
// 判据与 AddFact 的 answerShaped 是同一套（信封无条件 + 裸串看整条），只是处置
// 方式不同：事实那边是**拒收**（事实库必须干净），意图这边是**替换**（意图本身
// 有价值，见 AddIntent 的注释）。两处用不同判据会让某些串从缝隙里漏过去——
// 「能被拒入事实库的」与「能被净化掉的」必须是同一批。
//
// 信封形态复用 store.go 的 scrub：它已经在落盘口做同一件事（而且做了很久），
// 再写一份等价的遍历必然漂移。差别只在调用时机——落盘口是最后一道防线，这里是
// **第一道**：明文从进图那一刻就不存在，于是它不会出现在 prompt、报告、日志里，
// 也不依赖「Save 被调用过」。
//
// 裸串那一半**故意不做**，理由与 bareAnswerShaped 在事实通道上的那些限定同源：
//
//  1. 判定与上下文强耦合，替换会误伤。裸串的判据是「像答案」（含数字且长度 ≥ 6），
//     而目标是一句散文——`PhaseGoal(IntentExtract)` 的措辞是「取出答案（flag / 密码
//     / 密钥 / 提交所需的值）」，`IntentVerify` 是「确认候选答案的形态与来源」。
//     题面里出现「密码」「密钥」字样时 Shape.AllowRaw 为真，于是这些**固定的阶段
//     目标措辞**会在每次铺链时被判定成裸串并被整体替换成指纹——阶段链上出现一条
//     目标只有 `fp:xxxx/len=37/取…值` 的意图，agent 完全不知道要做什么。这不是
//     假想的：它正是「误杀的代价大于漏收」这条原则在意图通道上的翻版。
//  2. 落盘口本来就会洗一遍。`scrub` 覆盖了**所有**会进 prompt 或报告的自由文本
//     （Content / Raw / Goal / Evidence / 被拒审计），是既有的最后一道防线。
//  3. 首现优先是这条路径的**根本**对策，不是落盘口。即使明文一时留在内存里，
//     gate 的判定「首现于 intent 散文 ⇒ 幻觉族，永不提交」也不依赖文本被擦过。
//
// 信封形态没有这些问题：`flag{...}` 是**结构**（前缀 + 无空白的载荷 + 后缀），
// 与上下文无关，替换后剩下的散文仍然可读（「验证 fp:... 是否正确并提交」）。
func (g *Graph) sanitizeGoal(goal string) string {
	return g.scrub(goal)
}

// envelopeHit 报告 content 里是否出现本题的信封形态。
//
// 这是 answer.Shape.Contains 的**更窄**版本：只认信封，不认裸串（Contains 的
// Match 会连裸串一起挖）。answer 没有导出「只判信封」的入口，而在 dag 包里做
// 等价判定比为了一个布尔值去改已冻结的 answer 包更合适。
func envelopeHit(s answer.Shape, content string) bool {
	for _, e := range s.Envelopes {
		p, suf := e.Prefix, e.Suffix
		if p == "" || suf == "" {
			continue
		}
		for idx := strings.Index(content, p); idx >= 0; {
			rest := content[idx+len(p):]
			end := strings.Index(rest, suf)
			if end >= 1 && end <= answer.MaxEnvelopeLen &&
				!strings.ContainsAny(rest[:end], " \t\r\n") {
				return true
			}
			next := strings.Index(content[idx+1:], p)
			if next < 0 {
				break
			}
			idx += 1 + next
		}
	}
	return false
}

// reject 记一条被拒审计并返回错误。**所有**被拒的写入都走这里，保证
// 「为什么这条事实没进图」永远可回答（前身 61 条垃圾凭证事故的教训）。
func (g *Graph) reject(n Node, err error) error {
	g.rejected = append(g.rejected, Rejection{
		Kind: n.Kind, FactKind: n.FactKind, Content: truncate(n.Content, 200),
		Reason: err.Error(), Round: g.Round,
	})
	return err
}

// truncate 只用于审计记录的显示，不参与任何判定。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// nextID 生成节点 id。前缀 + 递增序号，不用随机数——id 要能在两次跑之间
// 稳定比较（golden 测试与 diff 都依赖它）。
func (g *Graph) nextID(prefix string) string {
	g.seq++
	return fmt.Sprintf("%s%d", prefix, g.seq)
}

// sortByRoundSeq 按 (Round, Seq) 升序。Seq 是全局递增的创建序号，所以这个排序
// 是全序的——不存在「同轮同序」的两条，输出完全确定。
func sortByRoundSeq(ns []*Node) {
	sort.Slice(ns, func(i, j int) bool {
		if ns[i].Round != ns[j].Round {
			return ns[i].Round < ns[j].Round
		}
		return ns[i].Seq < ns[j].Seq
	})
}

// ── 边 ──

// Link 连一条边。端点必须存在，且端点类型必须与边语义一致——类型错了不会
// 让程序崩，只会让图变得不可信（例如 refutes 指向一个事实），而不可信的图比
// 报错更糟：它会把错误的结论渲染进 prompt。
func (g *Graph) Link(from, to string, kind EdgeKind, round int) error {
	g.init()
	if !kind.Valid() {
		return fmt.Errorf("%w: 边类别 %q", ErrUnknownKind, kind)
	}
	a, b := g.nodes[from], g.nodes[to]
	if a == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, from)
	}
	if b == nil {
		// **requires 允许悬空**：前置条件可以指向一条还没发现的事实——「提权需要
		// 先有 foothold」在 foothold 存在之前就该能声明，否则调用方只能等事实出现
		// 后再补边，而那时意图可能已经被挑走执行过一次了。悬空的 requires 让意图
		// 进 blocked，前置事实一入图就自动解锁（executable 每次重算状态）。
		if kind != EdgeRequires {
			return fmt.Errorf("%w: %s", ErrNotFound, to)
		}
	} else if err := checkEndpoints(kind, a, b); err != nil {
		return err
	}
	e := Edge{From: from, To: to, Kind: kind, Round: round}
	if k := edgeKey(e); g.edgeSeen[k] {
		return nil // 幂等：连两次同一条边不是错误
	}
	g.edgeSeen[edgeKey(e)] = true
	g.edges = append(g.edges, e)

	// refutes 边的副作用：被证伪的意图立刻退出前沿。不等到 NextIntent 再过滤，
	// 是因为「已证伪」是一个**终态**——它不该在多次调度之间反复被重新判定，
	// 也不该在渲染 prompt 时还有机会被当成待办。
	if kind == EdgeRefutes {
		if b.IsIntent() && b.State != IntentDone {
			b.State = IntentAbandoned
			b.UpdatedAt = g.now()
		}
	}
	// supersedes 的副作用：**被取代者（To）**作废但保留（不删），审计痕迹要留着
	// ——「当初为什么换方向」在报告里是值钱的信息。方向定义：From 是新的，
	// To 是被取代的旧的（`supersedes: new → old`）。
	if kind == EdgeSupersedes {
		if b.IsIntent() && b.State != IntentDone {
			b.State = IntentAbandoned
			b.UpdatedAt = g.now()
		}
	}
	return nil
}

// checkEndpoints 校验边的端点类型。
func checkEndpoints(kind EdgeKind, from, to *Node) error {
	bad := func() error {
		return fmt.Errorf("%w: %s(%s) -[%s]-> %s(%s)",
			ErrEdgeKind, from.ID, from.Kind, kind, to.ID, to.Kind)
	}
	switch kind {
	case EdgeRequires, EdgeProduces:
		if !from.IsIntent() || !to.IsFact() {
			return bad()
		}
	case EdgeRefutes:
		// 方向是 negative 事实 → 被证伪的意图（设计文档 §一 的边表）。写反会让
		// 「死胡同剪枝」变成一句空话：refutes 边永远连不上，被证伪的意图继续留在
		// 前沿里被反复挑中——那正是前身最贵的那类浪费。
		if !from.IsFact() || !to.IsIntent() {
			return bad()
		}
	case EdgeEnables:
		if !from.IsFact() || !to.IsIntent() {
			return bad()
		}
	case EdgeDerivedFrom:
		if !from.IsFact() || !to.IsFact() {
			return bad()
		}
	case EdgeSupersedes:
		if !from.IsIntent() || !to.IsIntent() {
			return bad()
		}
	}
	// requires 只能指向前置条件，不能指向 negative——「前置是某条死胡同」
	// 是自相矛盾的，只会让意图永远无法执行。
	if kind == EdgeRequires && to != nil && to.FactKind == FactNegative {
		return fmt.Errorf("%w: requires 不能指向 negative 事实 %s", ErrEdgeKind, to.ID)
	}
	return nil
}

// Requires 返回某意图的全部前置事实 id。
//
// 用 outgoing 而不是 incoming：requires 边的方向是 intent → fact（见 edge.go 的
// 方向表），所以「某意图的前置」是从它出发的边。写成 incoming 会永远返回空——
// 而空前置的后果是**静默的**：`executable` 里那道「前置未满足就跳过」的闸门
// 从不生效，提权会在没有 foothold 时就开跑，白烧一轮预算。
func (g *Graph) Requires(intentID string) []string { return g.outgoing(intentID, EdgeRequires) }

// Produced 返回某意图实际产出的事实 id。
func (g *Graph) Produced(intentID string) []string { return g.outgoing(intentID, EdgeProduces) }

// RefutedBy 返回证伪某意图的 negative 事实 id。
func (g *Graph) RefutedBy(intentID string) []string { return g.incoming(intentID, EdgeRefutes) }

// EnabledFrom 返回派生某意图的事实 id（分支点的来源）。
func (g *Graph) EnabledFrom(intentID string) []string { return g.incoming(intentID, EdgeEnables) }

// DerivedFrom 返回某事实的推导来源（provenance 链）。
func (g *Graph) DerivedFrom(factID string) []string { return g.outgoing(factID, EdgeDerivedFrom) }

// incoming 返回以 id 为终点、类别为 kind 的边的起点（按连边顺序）。
func (g *Graph) incoming(id string, kind EdgeKind) []string {
	var out []string
	for _, e := range g.edges {
		if e.To == id && e.Kind == kind {
			out = append(out, e.From)
		}
	}
	return out
}

// outgoing 返回以 id 为起点、类别为 kind 的边的终点（按连边顺序）。
func (g *Graph) outgoing(id string, kind EdgeKind) []string {
	var out []string
	for _, e := range g.edges {
		if e.From == id && e.Kind == kind {
			out = append(out, e.To)
		}
	}
	return out
}

// ── 意图生命周期 ──

// Activate 把意图标记为执行中，并消耗一次尝试。
//
// Attempts 在**进入执行**时 +1 而不是在结算时：如果进程在轮中途被杀（前身
// 「子进程跑赢父进程」那类事故），结算永远不会发生，计数就会漏掉这次尝试，
// 于是一个卡死的意图会被无限重试。
func (g *Graph) Activate(id string) error {
	n := g.nodes[id]
	if n == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if !n.IsIntent() {
		return fmt.Errorf("%w: %s 不是意图", ErrEdgeKind, id)
	}
	if n.State == IntentDone || n.State == IntentAbandoned {
		return fmt.Errorf("%w: %s 已是终态 %s", ErrBadState, id, n.State)
	}
	n.State = IntentActive
	n.Attempts++
	n.UpdatedAt = g.now()
	return nil
}

// SetState 直接设置一个意图的状态，并把 UpdatedAt 推到当前轮。
//
// **只给「外部事件驱动的状态迁移」用**：暂停打断时把 active 标成 interrupted
// 是引擎（或宿主）知道的、图自己算不出来的事实。常规的 done/failed 判定一律走
// Settle，被证伪一律走 refutes 边——**不要用这个方法绕过那些不变量**。
//
// 已知状态才允许写入：写一个拼错的状态会让意图在 executable 的 switch 里落到
// default（不可执行），表现与「已做完」一样，整条阶段链静默停住。
func (g *Graph) SetState(id string, st IntentState) error {
	n := g.nodes[id]
	if n == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if !n.IsIntent() {
		return fmt.Errorf("%w: %s 不是意图", ErrEdgeKind, id)
	}
	if !st.Valid() {
		return fmt.Errorf("%w: %s 未知意图状态 %q", ErrBadState, id, st)
	}
	n.State = st
	n.UpdatedAt = g.now()
	return nil
}

// Settle 结算一轮：按「实际产出了什么」决定 done / failed，并把 produces 边
// 连上（这条边同时是 vuln/foothold 的证据引用目标）。
//
// 判定规则刻意是**客观**的（继承前身 next_open_goal 的原则：只用客观事实推进
// 阶段，绝不把推断写成事实）：
//
//  1. 产出了新事实（produced 里的 id 指向本轮新建的事实）⇒ done。
//  2. 没产出新事实，但 agent 明确申报了 negative ⇒ done（把死路走完也是进展）。
//  3. 都没 ⇒ failed（尝试过，无新事实）。
//
// res 目前只用于取证与未来扩展（例如按 Reason 区分失败原因），不参与判定——
// 「agent 说它成功了」不是成功。
func (g *Graph) Settle(id string, produced []string, res harness.RoundResult) error {
	n := g.nodes[id]
	if n == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if !n.IsIntent() {
		return fmt.Errorf("%w: %s 不是意图", ErrEdgeKind, id)
	}

	fresh := 0
	for _, pid := range produced {
		p := g.nodes[pid]
		if p == nil || !p.IsFact() {
			continue
		}
		if err := g.Link(id, pid, EdgeProduces, g.Round); err != nil {
			return err
		}
		// 「新事实」的判据是**本轮创建的**，不是「不在 produced 里」——
		// 一条事实可能被两个意图同时产出，第二个不该算进展。
		if p.Round == g.Round {
			fresh++
		}
	}

	switch {
	case fresh > 0:
		n.State = IntentDone
	case producedNegative(g, produced):
		n.State = IntentDone
	case n.Attempts >= n.attemptCap():
		// 试够了。前身只能靠 stoploss 的「连续 N 轮无新事实」整体刹车，
		// 这里可以精确到单条路径。
		n.State = IntentAbandoned
	default:
		n.State = IntentFailed
	}
	n.UpdatedAt = g.now()
	return nil
}

// producedNegative 报告本轮产出里是否有 negative 事实。
func producedNegative(g *Graph, produced []string) bool {
	for _, pid := range produced {
		if p := g.nodes[pid]; p != nil && p.IsFact() && p.FactKind == FactNegative {
			return true
		}
	}
	return false
}

// Refute 用一条 negative 事实证伪某个意图。
//
// 签名与设计文档一致（intentID, factID），但**实现走 AddNegative 那条路**：negative
// 事实的入库条件就是「带 refutes」，所以造事实与指定被证伪者必须是一个原子动作，
// 不能分成「先造事实再连边」两步——两步之间进程被杀，图里就会留下一条无主的
// negative 事实，而它按不变量本该被拒绝。
//
// factID 为空时自动生成内容；非空时用它当内容（调用方有自己的取证描述，例如
// 「nmap -p22 返回 connection refused」），这样「被证伪」的理由在渲染进 prompt 时
// 是可读的，而不是一句 `已证伪: i3`。
//
// 名字与参数名沿用了设计文档的写法，但它其实是**内容**而不是事实 id——文档里
// `Refute(intentID, factID string)` 的第二个参数在实际调用点（M5 轮循环）只会拿到
// 一段描述。签名保持不动是为了不与文档漂移；语义写在这里，免得下一个人按参数名
// 去找一条不存在的节点。
func (g *Graph) Refute(intentID, factID string) error {
	content := strings.TrimSpace(factID)
	if content == "" {
		content = "已证伪: " + intentID
	}
	_, err := g.AddNegative(intentID, content, SourcePlatform, TrustHost, "")
	return err
}

// AddNegative 造一条 negative 事实并证伪指定意图，返回事实 id。
//
// 这是宿主侧记录死胡同的**唯一**入口（agent 侧的通道是 report_fact 的
// kind=negative，由 extract.go 转成同一个调用）。
func (g *Graph) AddNegative(intentID, content, source, trust, toolCallID string) (string, error) {
	if g.nodes[intentID] == nil || !g.nodes[intentID].IsIntent() {
		return "", fmt.Errorf("%w: 被证伪的意图 %q 不存在", ErrNegativeNoRefutes, intentID)
	}
	return g.addFact(Node{
		Kind:       NodeFact,
		FactKind:   FactNegative,
		Content:    content,
		Source:     source,
		Trust:      trust,
		ToolCallID: toolCallID,
		Evidence:   toolCallID,
		Refutes:    intentID,
		Confidence: 0.9,
	}, false)
}

// ── 前沿与优先级 ──

// phaseChains 是各类别的阶段链，逐条移植前身 goals_for_category() 的语义。
//
// 为什么保留「按类别给链」这种看似朴素的机制：它是前身少数被验证有效的调度
// 主干——crypto/misc/forensics/reverse 的题里去做端口扫描毫无意义。v1 的调度
// 主干**就是**它，图只负责在这条链上做剪枝与分支（设计文档 §一「诚实的边界」）。
var phaseChains = map[string][]IntentKind{
	"pentest": {IntentRecon, IntentExploit, IntentFoothold, IntentExtract,
		IntentLateral, IntentEscalate, IntentVerify},
	"crypto":    {IntentAnalyze, IntentExploit, IntentVerify},
	"misc":      {IntentAnalyze, IntentExploit, IntentVerify},
	"forensics": {IntentAnalyze, IntentExploit, IntentVerify},
	"reverse":   {IntentAnalyze, IntentExploit, IntentVerify},
}

// defaultChain 是未匹配类别时的链。与 pentest 的区别：没有 credential/lateral
// 这两个只在内网渗透里有意义的阶段。
var defaultChain = []IntentKind{IntentRecon, IntentExploit, IntentFoothold, IntentEscalate, IntentVerify}

// PhaseChain 返回某类别的阶段链（导出给测试与态势台用）。
//
// 类别查表用 fold（大小写折叠）：平台的 category 字段写法不统一（`Pentest` /
// `pentest` / ` WEB `），而类别不是载荷——折叠它不会改坏任何东西（对比内容归一
// 刻意不做小写化，见 normalize 的说明）。
func PhaseChain(category string) []IntentKind {
	c := fold(category)
	if ch, ok := phaseChains[c]; ok {
		return append([]IntentKind(nil), ch...)
	}
	return append([]IntentKind(nil), defaultChain...)
}

// phaseRank 返回意图类别在阶段链里的位置。链外的类别（recover）排在最后——
// 它是异常处置，不该抢在正常路径前面。
func phaseRank(category string, k IntentKind) int {
	for i, x := range PhaseChain(category) {
		if x == k {
			return i
		}
	}
	return len(PhaseChain(category))
}

// Frontier 返回当前**可执行**的意图（pending/failed 且未被证伪、前置满足、
// 尝试未超限），按优先级降序排列。
//
// 排序键：阶段序 → 创建轮次 → 创建序号。第三项保证全序，因此同一张图在任何
// 时候调用都返回同样的顺序——没有它，map 迭代顺序会让调度随机漂移，同一道题
// 跑两次得到不同的行为（评测里不可接受，也让人无法复现 bug）。
func (g *Graph) Frontier() []*Node {
	var out []*Node
	for _, id := range g.order {
		n := g.nodes[id]
		if !n.IsIntent() {
			continue
		}
		if g.executable(n) {
			out = append(out, cloneNode(n))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := phaseRank(g.Category, out[i].IntentKind), phaseRank(g.Category, out[j].IntentKind)
		if ri != rj {
			return ri < rj
		}
		if out[i].Round != out[j].Round {
			return out[i].Round < out[j].Round
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

// executable 报告一个意图当前是否可执行。它是 Frontier / NextIntent / Render 的
// 共同判据，只写一次——三处各写一遍必然会漂移（前身 B55 的教训：同一判定散落
// 四处，长期漂移）。
func (g *Graph) executable(n *Node) bool {
	if n == nil || !n.IsIntent() {
		return false
	}
	switch n.State {
	case IntentPending, IntentFailed, IntentInterrupted:
		// failed 可以重试（还有尝试余额）；done / abandoned 是终态。
		//
		// interrupted 也可重试：暂停打断的那一轮动作是否生效未知，恢复后由
		// Scenario.Reconcile 对账决定。**不能假定它成功**（会漏掉一次真实的
		// 平台写操作），也不能假定它失败（会重复一次可能已生效的写操作）——
		// 所以先让它回到可执行，把判断交给对账。
	default:
		return false
	}
	if n.Attempts >= n.attemptCap() {
		return false
	}
	// 被 refutes 边证伪 ⇒ 永久移出前沿。这是「死胡同剪枝」的直接实现：
	// 前身最贵的浪费就是反复重试同一条已被证伪的路径。
	if len(g.RefutedBy(n.ID)) > 0 {
		return false
	}
	// requires 未满足 ⇒ 跳过。Blocked 状态是**算出来的**（不落盘），所以前置
	// 一旦被满足就自动解锁。
	for _, f := range g.Requires(n.ID) {
		if g.nodes[f] == nil {
			return false
		}
	}
	return true
}

// NextIntent 返回前沿里优先级最高的可执行意图；前沿为空时返回 nil（收尾信号）。
func (g *Graph) NextIntent() *Node {
	f := g.Frontier()
	if len(f) == 0 {
		return nil
	}
	return f[0]
}

// Validate 复查全图的不变量。
//
// 它不是防御性的重复检查，而是**载入时的守门人**：dag.json 可能被人工编辑、
// 被旧版本写过、或落盘中途被杀（半个文件）。前身 `_load` 里那段「净化存量噪音
// 凭证」就是这个需求的体现——不净化的话，历史脏数据会一直留在图里影响调度。
//
// 返回的问题列表是**可读的**（每条指明节点与原因），因为修复它们要靠人。
func (g *Graph) Validate() []error {
	g.init()
	var errs []error
	for _, id := range g.order {
		n := g.nodes[id]
		if n.IsFact() {
			if strings.TrimSpace(n.Content) == "" {
				errs = append(errs, fmt.Errorf("节点 %s: 事实内容为空", id))
			}
			if n.Source == "" {
				errs = append(errs, fmt.Errorf("节点 %s: 事实无来源", id))
			}
			if !n.FactKind.Valid() {
				errs = append(errs, fmt.Errorf("节点 %s: 未知事实类别 %q", id, n.FactKind))
			}
			if g.answerShaped(n.Content, n.FactKind) {
				errs = append(errs, fmt.Errorf("节点 %s: 内容命中答案形状（应只存在于 gate 账本）", id))
			}
			switch n.FactKind {
			case FactNegative:
				if n.Refutes == "" || g.nodes[n.Refutes] == nil {
					errs = append(errs, fmt.Errorf("节点 %s: negative 事实未指向有效意图", id))
				}
			case FactVuln, FactFoothold:
				if n.ToolCallID == "" && n.Evidence == "" {
					errs = append(errs, fmt.Errorf("节点 %s: %s 事实无证据引用", id, n.FactKind))
				}
				if n.Trust == TrustInferred {
					errs = append(errs, fmt.Errorf("节点 %s: inferred 事实不能单独构成 %s", id, n.FactKind))
				}
			}
			continue
		}
		if n.IsIntent() {
			if strings.TrimSpace(n.Goal) == "" {
				errs = append(errs, fmt.Errorf("节点 %s: 意图目标为空", id))
			}
			if !n.IntentKind.Valid() {
				errs = append(errs, fmt.Errorf("节点 %s: 未知意图类别 %q", id, n.IntentKind))
			}
			// 状态是**从 JSON 反序列化来的**，所以必须显式校验：未知状态在
			// executable 的 switch 里落到 default（不可执行），表现与「已做完」
			// 完全一样——一个拼错的状态会让整条阶段链静默停住，没有任何报错。
			if !n.State.Valid() {
				errs = append(errs, fmt.Errorf("节点 %s: 未知意图状态 %q", id, n.State))
			}
			continue
		}
		errs = append(errs, fmt.Errorf("节点 %s: 未知节点类别 %q", id, n.Kind))
	}
	for _, e := range g.edges {
		a, b := g.nodes[e.From], g.nodes[e.To]
		if a == nil {
			errs = append(errs, fmt.Errorf("边 %s -[%s]-> %s: 起点缺失", e.From, e.Kind, e.To))
			continue
		}
		if b == nil {
			// **悬空的 requires 是合法状态**，不是坏图：前置事实可能还没被发现
			// （「提权需要先有 foothold」）。其它类别的边指向不存在的节点则说明
			// 图被改坏了——那会让推导链断掉、证据引用失效。
			if e.Kind != EdgeRequires {
				errs = append(errs, fmt.Errorf("边 %s -[%s]-> %s: 终点缺失", e.From, e.Kind, e.To))
			}
			continue
		}
		if err := checkEndpoints(e.Kind, a, b); err != nil {
			errs = append(errs, err)
		}
	}
	// 去重索引的完整性：索引是去重的唯一依据，它坏了会静默放进重复事实。
	for _, id := range g.order {
		n := g.nodes[id]
		if !n.IsFact() {
			continue
		}
		if g.factIdx[factKey(n.FactKind, n.Content)] != id {
			errs = append(errs, fmt.Errorf("节点 %s: 事实去重索引不一致", id))
		}
	}
	return errs
}

// Equivalent 报告两张图是否等价（节点内容、边、状态一致）。
//
// 它服务于持久化往返测试：Save→Load 之后图必须**等价**，而不是「差不多」。
// 比较时忽略时间戳与 id 之外的内部序号——那些是实现的产物，不是图的语义。
func (g *Graph) Equivalent(other *Graph) error {
	if other == nil {
		return errors.New("另一张图为 nil")
	}
	if len(g.order) != len(other.order) {
		return fmt.Errorf("节点数不同: %d vs %d", len(g.order), len(other.order))
	}
	byKey := func(gr *Graph) map[string]*Node {
		m := map[string]*Node{}
		for _, id := range gr.order {
			n := gr.nodes[id]
			var k string
			if n.IsFact() {
				k = "F\x00" + factKey(n.FactKind, n.Content)
			} else {
				k = "I\x00" + intentKey(n.IntentKind, n.Goal)
			}
			m[k] = n
		}
		return m
	}
	a, b := byKey(g), byKey(other)
	for k, n := range a {
		o, ok := b[k]
		if !ok {
			return fmt.Errorf("缺少节点 %s", k)
		}
		if n.Kind != o.Kind || n.FactKind != o.FactKind || n.IntentKind != o.IntentKind {
			return fmt.Errorf("节点 %s 类别不同", k)
		}
		if n.State != o.State || n.Attempts != o.Attempts {
			return fmt.Errorf("节点 %s 状态不同: %s/%d vs %s/%d", k, n.State, n.Attempts, o.State, o.Attempts)
		}
		if n.Source != o.Source || n.Trust != o.Trust || n.Evidence != o.Evidence ||
			n.ToolCallID != o.ToolCallID || n.Refutes != o.Refutes {
			return fmt.Errorf("节点 %s 来源/证据字段不同", k)
		}
	}
	ek := func(gr *Graph) map[string]Edge {
		m := map[string]Edge{}
		for _, e := range gr.edges {
			m[edgeKey(e)] = e
		}
		return m
	}
	ea, eb := ek(g), ek(other)
	if len(ea) != len(eb) {
		return fmt.Errorf("边数不同: %d vs %d", len(ea), len(eb))
	}
	for k := range ea {
		if _, ok := eb[k]; !ok {
			return fmt.Errorf("缺少边 %s", k)
		}
	}
	return nil
}
