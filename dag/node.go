// Package dag 实现宿主侧**权威**的「事实—意图 DAG」。
//
// 这个包是纯数据 + 算法：零外部依赖（除根包契约 harness 与答案形状 answer），
// 不碰网络、不碰进程、不碰磁盘以外的任何东西，因此可以全量单测。
//
// 为什么需要它（前身的血）：前身是一个扁平事实库 + 线性目标链
// （/tmp/base_72ae35f/adapter/blackboard.py）。扁平库能记账，但表达不了三件事——
// 「这条具体路径已证伪」（只有整体刹车 stoploss）、「这个 flag 候选是哪次工具调用
// 推出来的」（于是事后写了 3,592 行 verify.py 去重建 provenance）、「这个事实派生
// 出了哪些分支」。本包只挣这四件事：死胡同剪枝、推导链、分支点、断点续跑。
//
// **诚实的边界**：v1 的调度主干**仍然是目标链**（继承 goals_for_category 的语义，
// 按题目类别给出阶段顺序，阶段序即优先级）。图**不做**拓扑排序式调度，也**不做**
// 攻击路径规划——前身那份从未实现的 MITRE AttackPath 文档是明确的「不复活」项。
package dag

import "time"

// NodeKind 区分图里的两类节点。
type NodeKind string

const (
	NodeFact   NodeKind = "fact"
	NodeIntent NodeKind = "intent"
)

// FactKind 是事实的类别。
//
// **这里刻意没有 flag / answer**：答案不是事实。这是前身立下的硬规矩
// （blackboard.py:260 的注释）：平台确认前把 flag 候选写进事实库，会让一次重复的
// 本地读取在打码/重载之后看起来像「新事实」，把卡住的题无限续命。候选只存在于
// gate 的账本里。
type FactKind string

const (
	// FactTarget 是地址:端口。题目初始化时由平台题面生成，Source 标 platform。
	FactTarget FactKind = "target"
	// FactService 是服务/版本/技术栈指纹。
	FactService FactKind = "service"
	// FactArtifact 是源码/二进制/配置/密文文件。
	FactArtifact FactKind = "artifact"
	// FactCredential 是凭证。必须过 B14 质量闸（见 extract.go），否则不入库。
	FactCredential FactKind = "credential"
	// FactVuln 是已确认可利用的缺陷。必须带证据引用，agent 自述不能单独构成它。
	FactVuln FactKind = "vuln"
	// FactFoothold 是立足点（shell / 执行点）。同样必须带证据引用。
	FactFoothold FactKind = "foothold"
	// FactNegative 是已证伪的路径（死胡同）。必须带被证伪的意图 id（refutes 边）。
	FactNegative FactKind = "negative"
)

// factKinds 是全部合法事实类别。AddFact 用它拒绝未知类别——这样 `flag` / `answer`
// 这类字符串连「假装成事实」的机会都没有（不是靠关键词黑名单，是类型上没有）。
var factKinds = map[FactKind]bool{
	FactTarget: true, FactService: true, FactArtifact: true,
	FactCredential: true, FactVuln: true, FactFoothold: true, FactNegative: true,
}

// Valid 报告这个事实类别是否是已知类别。
func (k FactKind) Valid() bool { return factKinds[k] }

// IntentKind 是意图的类别。
type IntentKind string

const (
	IntentRecon    IntentKind = "recon"    // 枚举/指纹
	IntentAnalyze  IntentKind = "analyze"  // 读源码/逆向/分析密文
	IntentExploit  IntentKind = "exploit"  // 验证并利用某假设
	IntentFoothold IntentKind = "foothold" // 获取初始访问
	IntentEscalate IntentKind = "escalate" // 提权
	IntentLateral  IntentKind = "lateral"  // 横向
	IntentExtract  IntentKind = "extract"  // 从已知立足点取 flag / 取凭证
	IntentVerify   IntentKind = "verify"   // 复核某个 flag 候选
	IntentRecover  IntentKind = "recover"  // 环境不通/容器崩坏的处置
)

var intentKinds = map[IntentKind]bool{
	IntentRecon: true, IntentAnalyze: true, IntentExploit: true, IntentFoothold: true,
	IntentEscalate: true, IntentLateral: true, IntentExtract: true,
	IntentVerify: true, IntentRecover: true,
}

// Valid 报告这个意图类别是否是已知类别。
func (k IntentKind) Valid() bool { return intentKinds[k] }

// IntentState 是意图的生命周期状态。
type IntentState string

const (
	// IntentPending 在待执行。
	IntentPending IntentState = "pending"
	// IntentActive 正在执行（本轮的意图）。
	IntentActive IntentState = "active"
	// IntentDone 达成：产出了预期类别的事实。
	IntentDone IntentState = "done"
	// IntentFailed 尝试过，无新事实。
	IntentFailed IntentState = "failed"
	// IntentBlocked 前置条件（requires）未满足。它是**算出来的**，不是存下来的：
	// 每次 NextIntent 重新判定，所以前置一旦满足就自动解锁，不需要谁去改状态。
	IntentBlocked IntentState = "blocked"
	// IntentAbandoned 预算耗尽 / 被 negative 事实证伪。
	IntentAbandoned IntentState = "abandoned"
	// IntentInterrupted 本轮被**暂停**打断，动作是否生效未知。
	//
	// 为什么单列一个状态而不是复用 failed：暂停时 agent 可能已经把命令发出去了
	// （写操作可能已生效），但本轮没有等到结果。恢复时**不能假定它成功**，也不能
	// 假定它失败——所以它既不是 done 也不是 failed，而是一个需要重新对账的
	// 中间态。恢复后由 Scenario.Reconcile 对账决定它最终落到 done 还是 failed。
	//
	// 为什么必须落盘：进程被杀时内存里的状态没了，图是唯一能告诉恢复路径
	// 「这一轮被打断过」的地方。不落盘的话恢复后会把它当成 pending 重跑一遍，
	// 而重跑一个可能已生效的写操作正是设计文档里点名的第 3 类真实损失。
	IntentInterrupted IntentState = "interrupted"
)

// Valid 报告这个状态是否是已知状态。
//
// 为什么需要它：IntentState 是从 JSON 反序列化来的，旧文档、人工编辑过的文档、
// 或未来版本写的新状态都可能出现。未知状态在 `executable` 的 switch 里会落到
// default 分支（不可执行），看起来像「意图做完了」——一个拼错的状态会让整条
// 阶段链静默停住。所以读取路径上要显式校验。
//
// 校验与**枚举**共用 intentStates 这一份：状态散在两处（一个 map 供遍历、一个
// switch 供判定）就会漂移，而漂移的表现是「新状态被判为非法」或「渲染时少一类
// 样式」——两者都不会让任何测试变红，除非有东西能遍历全部状态。`factKinds` 与
// `intentKinds` 早就是这个形状，状态跟着它们对齐。
func (s IntentState) Valid() bool { return intentStates[s] }

var intentStates = map[IntentState]bool{
	IntentPending:     true,
	IntentActive:      true,
	IntentDone:        true,
	IntentFailed:      true,
	IntentBlocked:     true,
	IntentAbandoned:   true,
	IntentInterrupted: true,
}

// 三层信任（见 extract.go 与设计文档 §一）。
//
// 分层的目的不是「给事实打分」这么抽象的东西，而是让**一个具体的事故**不可能
// 重演：前身把「输出截断到 2000 字符」当作防噪音手段，实际副作用是切掉了长输出
// 里的真事实；又因为没记来源，只能事后写 3,592 行去重建 provenance。
const (
	// TrustHost 宿主从工具输出**指纹匹配**得到。抽取输入是完整输出，落盘的是
	// sha256[:12] + 命中偏移。可直接入图。
	TrustHost = "host-verified"
	// TrustAgent 由 agent 的 report_fact 申报。Confidence 封顶 0.7；vuln/foothold
	// 必须带 evidence，否则降级。
	TrustAgent = "agent-asserted"
	// TrustInferred 由其它事实推导。永不单独构成 vuln/foothold。
	TrustInferred = "inferred"
)

// SourcePlatform 是题目初始化时写入的目标事实的来源标记。它是「事实必须有来源」
// 这条不变量的唯一例外：平台题面不是工具输出，但它同样是**客观**来源。
const SourcePlatform = "platform"

// DefaultMaxAttempts 是一个意图的默认尝试上限。到达上限后它永久退出前沿——
// 前身只能靠 stoploss 的「连续 N 轮无新事实」整体刹车，无法表达「这条具体路径
// 已经试够了」。
const DefaultMaxAttempts = 3

// Node 是图里的一个节点：要么是事实，要么是意图。
//
// 事实与意图共用一个结构体（而不是两个类型 + 接口），是因为它们在图算法里
// 被同等对待（都要排序、都要连边、都要落盘），分型只会带来无收益的类型断言。
// 代价是字段有一半对另一半无意义——用 Kind 区分，校验在 AddFact/AddIntent 里。
type Node struct {
	ID   string   `json:"id"`
	Kind NodeKind `json:"kind"`

	// ── fact ──
	FactKind FactKind `json:"factKind,omitempty"`
	// Content 是规范化后的内容，也是去重的键。
	Content string `json:"content,omitempty"`
	// Raw 是取证用的原始片段（不是整份工具输出——见 extract.go 的说明）。
	Raw string `json:"raw,omitempty"`
	// Source 是人可读的来源，例如 `bash: nmap -sV 10.0.0.1` / `report_fact` /
	// `platform`。**空 Source 的事实拒绝入库**。
	Source string `json:"source,omitempty"`
	// Confidence 0..1。
	Confidence float64 `json:"confidence,omitempty"`
	// Trust 见 TrustHost / TrustAgent / TrustInferred。
	Trust string `json:"trust,omitempty"`
	// ToolCallID 是产出这条事实的那次工具调用（pi 的 toolCallId）。它是推导链的
	// 锚点：gate 的 provenance 判定、证据引用都靠它，不必事后从字符串重建。
	ToolCallID string `json:"toolCallId,omitempty"`
	// Fingerprint 是产生这条事实的工具输出的 sha256[:12]。落盘的是指纹而不是
	// 原文——既不掉真事实（抽取用的是完整输出），也不把大盘文本灌进图。
	Fingerprint string `json:"fingerprint,omitempty"`
	// Offset 是命中处在**完整输出**里的字节偏移。它是「我们确实是在完整输出里
	// 找到的」的凭据。
	Offset int `json:"offset,omitempty"`
	// OutputLen 是完整输出的字节长度，用来证明没有截断过。
	OutputLen int `json:"outputLen,omitempty"`
	// Evidence 是证据引用：指向产生它的 produces 边或 ToolCallID。
	// vuln / foothold 事实**必须**有它。
	Evidence string `json:"evidence,omitempty"`
	// Refutes 是 negative 事实被证伪的那个意图 id。它是 negative 事实的入库条件。
	Refutes string `json:"refutes,omitempty"`

	// ── intent ──
	IntentKind IntentKind `json:"intentKind,omitempty"`
	// Goal 是要渲染进 prompt 的那句自然语言指令。
	Goal string `json:"goal,omitempty"`
	// Expect 是预期产出，用于判定 done。
	Expect []FactKind  `json:"expect,omitempty"`
	State  IntentState `json:"state,omitempty"`
	// Attempts 是已消耗的尝试次数（Activate 时 +1）。
	Attempts int `json:"attempts,omitempty"`
	// MaxAttempts 是尝试上限，<=0 时用 DefaultMaxAttempts。
	MaxAttempts int `json:"maxAttempts,omitempty"`

	// Round 是创建轮次。
	Round int `json:"round,omitempty"`
	// Seq 是创建序号，用于**确定性平局判定**：同阶段同轮次的意图必须有稳定的
	// 先后。没有它，前沿顺序会随 map 迭代顺序漂移，同一份图跑两次得到不同的
	// 调度——这类不确定性在评测里是不可接受的。
	Seq       int       `json:"seq,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// IsFact / IsIntent 是给调用方的小判据，避免到处写 Kind == NodeFact。
func (n *Node) IsFact() bool   { return n != nil && n.Kind == NodeFact }
func (n *Node) IsIntent() bool { return n != nil && n.Kind == NodeIntent }

// attemptCap 返回这个意图的尝试上限。
func (n *Node) attemptCap() int {
	if n.MaxAttempts > 0 {
		return n.MaxAttempts
	}
	return DefaultMaxAttempts
}

// Expects 报告这条意图是否把 kind 当作预期产出。
//
// 目前**没有生产调用点**（预期产出只用于渲染，不做判定），保留它是因为 M5 的
// 轮循环要用它做「agent 申报的 kind 与本轮预期不符 ⇒ 提示它换个类别申报」，
// 而那时再回来加一个方法、补一条测试的成本高于现在留着。
func (n *Node) Expects(kind FactKind) bool {
	for _, k := range n.Expect {
		if k == kind {
			return true
		}
	}
	return false
}
