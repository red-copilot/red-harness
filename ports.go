package harness

import (
	"context"
	"time"
)

// ── 场景与平台 ──

// Challenge 是一道题的完整信息。
//
// 字段名与类型**与 v0.2 逐字相同**——`dag/store.go` 的 schema 1 把它写进
// graph.json，改名会静默破坏所有现存图。
type Challenge struct {
	Code        string `json:"code"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Difficulty  string `json:"difficulty,omitempty"`
	// FlagCount 是题目要求的 flag 数。0 表示未知——**「通关立即终止」不能只靠它**，
	// 因为 StartResult 不携带它。
	FlagCount int `json:"flagCount,omitempty"`
	// Solved 是已确认的 flag 数（平台侧）。
	Solved int `json:"solved,omitempty"`
	// Addrs 是题目容器地址。
	Addrs []string `json:"addrs,omitempty"`
	// FlagFormat 是题面声明的答案形态。为空时按「以题面为准」渲染。
	FlagFormat string `json:"flagFormat,omitempty"`
}

// Remaining 返回还差几个 flag。FlagCount 未知（0）时返回 0。
func (c Challenge) Remaining() int {
	if c.FlagCount <= 0 {
		return 0
	}
	if c.Solved >= c.FlagCount {
		return 0
	}
	return c.FlagCount - c.Solved
}

// Done 报告这道题是否已全部完成。
func (c Challenge) Done() bool { return c.FlagCount > 0 && c.Solved >= c.FlagCount }

// StartResult 是起题的结果。
type StartResult struct {
	Code string `json:"code"`
	// Addrs 是容器地址。**仅当容器已 available 时非空**——起题是异步的，
	// pending 期间地址为空，需要轮询（见 Scenario.Prepare）。
	Addrs       []string `json:"addrs,omitempty"`
	Description string   `json:"description,omitempty"`
}

// SubmitResult 是平台对一次提交的判定。
type SubmitResult struct {
	Correct bool `json:"correct"`
	// Awarded 是本次得分。
	Awarded int `json:"awarded,omitempty"`
	// Duplicate 为真表示平台回的是幂等命中。**它等价于已确认**，不是错误。
	Duplicate bool   `json:"duplicate,omitempty"`
	Message   string `json:"message,omitempty"`
	// CorrectFlagCount / TotalFlagCount 是**平台权威的**作答进度。
	// 每次提交都会回，这是「通关立即终止」唯一不依赖调用方自觉的判据。
	CorrectFlagCount int `json:"correctFlagCount,omitempty"`
	TotalFlagCount   int `json:"totalFlagCount,omitempty"`
	// MatchedIndex 是命中的第几个 flag（平台语义）。
	MatchedIndex int `json:"matchedIndex,omitempty"`
}

// HintResult 是一次提示请求的结果。Hint 可以为空（平台没给提示）。
type HintResult struct {
	Code string `json:"code"`
	Hint string `json:"hint,omitempty"`
}

// CloseResult 是关题的结果。
type CloseResult struct {
	Code   string `json:"code"`
	Closed bool   `json:"closed"`
}

// Platform 是题目平台的抽象。
//
// **方法集与 v0.2 逐字相同**（含 HealthChecker 的形状），因为 bridge 的
// wire 协议就是照它设计的。v0.2 里这个接口没有任何实现——v0.3 由 bridge 实现。
type Platform interface {
	List(ctx context.Context) ([]Challenge, error)
	Start(ctx context.Context, code string) (StartResult, error)
	Hint(ctx context.Context, code string) (HintResult, error)
	Submit(ctx context.Context, code, flag string) (SubmitResult, error)
	Close(ctx context.Context, code string) (CloseResult, error)
}

// HealthChecker 是平台的可选预检。实现它 ⇒ 引擎在任何平台写操作之前先调它。
//
// TSecBench 的 VPN 预检就是这个语义，且**必须在任何平台调用之前**——对齐官方
// SDK 的上下文管理器行为。前身事故：预检失败被当成「题目不存在」继续跑。
type HealthChecker interface {
	Health(ctx context.Context) error
}

// Scenario 把一个具体场景（TSecBench / fake）映射到通用模型。
//
// 六方法对应题目生命周期的六个阶段。**Reconcile 是其中最容易被忽略、也最容易
// 造成真实损失的一个**：平台写操作超时但实际已生效时，盲目重发会浪费一次提交
// 额度，或把已确认的 flag 记成判错。
type Scenario interface {
	// Discover 列出可选目标（TSecBench 里是 list，已通关的由实现过滤）。
	Discover(ctx context.Context, spec RunSpec) ([]Challenge, error)
	// Prepare 起题并等到容器可用，返回引擎视角的目标。
	Prepare(ctx context.Context, ch Challenge) (Target, error)
	// Hint 请求平台提示。没有提示时返回空串且不报错。
	Hint(ctx context.Context, ch Challenge) (string, error)
	// Evaluate 提交一个候选并返回判定。
	Evaluate(ctx context.Context, ch Challenge, flag string) (Evaluation, error)
	// Reconcile 对账平台进度与容器状态，返回**平台权威**的 Objective。
	Reconcile(ctx context.Context, ch Challenge) (Objective, error)
	// Cleanup 关题。错误单独记录，**不覆盖主要终止原因**。
	Cleanup(ctx context.Context, ch Challenge) error
}

// ── Agent 端口 ──

// Agent 是**持久**的解题 agent：进程常驻，每题一次 new_session，轮循环用它
// 一轮一个意图地推进。
type Agent interface {
	// Start 起进程、握手、new_session，并断言会话已复位。
	Start(ctx context.Context, req AgentStart) error
	// Round 发一次 prompt 并等到本轮结束（或出错/超时）。
	// 事件通过 AgentFactory.New 注入的 EventSink 推送，不走返回值。
	Round(ctx context.Context, req RoundRequest) (RoundResult, error)
	// Steer 中途注入一条消息（hint / 剩余时间提醒）。agent 空闲时等价于 prompt。
	Steer(ctx context.Context, msg string) error
	// Stats 取权威计数（turns/tokens/cost/context）。
	Stats(ctx context.Context) (Stats, error)
	// Close 终止进程组。pi 不会自己退出（M0 实测），必须显式 killpg。
	Close(ctx context.Context) error
}

// AgentFactory 按 AgentSpec 造一个 Agent，并把事件出口接上。
//
// 为什么是工厂而不是实例：一道 run 可能跑多道题，而 agent 是**每题一个**的
// （每题独立 HOME、独立 new_session）。工厂由装配层注入。
type AgentFactory interface {
	New(spec AgentSpec, ev EventSink) (Agent, error)
}

// EventSink 是 agent 把流式事件交给引擎的唯一通道。
//
// v0.2 用 `emit func(Event)` 从 agent 的 reader 协程直接扇出到
// OnEvent / Observer / Gate / Ingest（`harness.go:520-528`）——那意味着一个慢
// 回调会反压 pi 的 stdout 管道，而且任意用户代码跑在别人的协程里。
//
// v0.3 改成显式接口：agent 只**推**事件，谁消费、在哪个协程消费由引擎决定。
// 引擎的实现把事件写进 runLoop 的 channel（带缓冲，满了阻塞——有意的背压）。
type EventSink interface {
	Emit(Event)
}

// RoundRequest 是一次轮次的入参。
type RoundRequest struct {
	// Prompt 是渲染好的本轮 prompt。
	Prompt string
	// Round 是轮号（1 起）。
	Round int
	// IntentID 是本轮执行的意图，供日志与追踪。
	IntentID string
	// Timeout 是本轮墙钟上限。0 表示用 agent 默认。
	Timeout time.Duration
}

// ── 执行器端口 ──

// Executor 是隔离执行环境。v1 只提供 Docker 实现。
type Executor interface {
	// Prepare 按 spec 创建容器与网络，返回句柄。
	Prepare(ctx context.Context, spec ExecSpec) (ExecHandle, error)
	// Exec 在句柄指向的容器里跑一条命令。
	Exec(ctx context.Context, h ExecHandle, cmd []string, opts ExecOptions) (ExecResult, error)
	// Reclaim 按 run label 精确回收本次运行创建的全部容器与网络。
	// 正常结束、取消、宿主重启恢复三条路径都要能调它。
	Reclaim(ctx context.Context, runID RunID) error
	// Available 检查执行器本身可用（Docker 在不在）。
	Available(ctx context.Context) error
}

// ── 规划与渲染 ──

// Planner 决定下一轮做哪个意图，并把轮内事件喂给事实抽取。
//
// **为什么方法名是 ObserveEvent 而不是 Ingest**：`dag.Scheduler` 已有一个
// `Ingest(ev, round) IngestResult`（`dag/schedule.go:131`）。Go 不允许同名的
// 两个方法只因返回值不同而共存；改 dag 的签名会打断读返回值的既有测试。
// 所以接口用新名字，dag 侧加一个薄包装方法。
//
// **为什么 ObserveEvent 必须收进接口**：v0.2 靠 `Session.Ingest func(Event,int)`
// 接线（`harness.go:138`），漏接是**静默**的——DAG 零事实、7 个阶段只有前 4 个
// 可达，而所有包自测全绿（`wiring_test.go:85` 就是为这个写的）。收进接口后
// 漏接变成编译错误。
type Planner interface {
	// Next 返回下一个可执行的意图；返回 nil 表示前沿耗尽。
	Next(ctx context.Context, in PlannerInput) (*IntentRef, error)
	// Activate 标记意图进入执行中。
	Activate(it *IntentRef)
	// Settle 回填一轮的结果。
	Settle(it *IntentRef, res RoundResult)
	// ObserveEvent 把一个轮内事件喂给事实抽取。
	ObserveEvent(ev Event, round int)
}

// Renderer 把当前状态渲染成本轮 prompt。
type Renderer interface {
	Render(ctx context.Context, ch Challenge, it *IntentRef, out *OutcomeView) string
}

// ── 候选闸与账本 ──

// Provenance 是候选答案的**来源族别**。这三族分账是前身 `hallucination.py`
// 的核心原则，直接决定候选能不能被提交：
//
//   - 只有 Observed 族可提交。
//   - Derived 族只记账、永不参与任何干预阈值——前身的影子审计发现
//     「29 条被拒候选里 19 条实为正确答案」，几乎全落在推导族，所以对推导族
//     采取任何动作都是净损失。
//   - Fabricated 族只记账，永不提交。
type Provenance string

const (
	// ProvenanceObserved：答案串首次出现在工具输出里，且产生它的命令里不含
	// 答案形状。这是唯一可提交的族。
	ProvenanceObserved Provenance = "observed"
	// ProvenanceDerived：答案串出现在事实里，但曾被 intent / artifact 引用过
	// （推导族）。记账，默认不提交。
	ProvenanceDerived Provenance = "derived"
	// ProvenanceFabricated：答案串只出现在 agent 的散文或命令参数里，
	// 从未见于工具输出（幻觉族）。只记账。
	ProvenanceFabricated Provenance = "fabricated"
)

// Candidate 是一个待提交的答案候选。它**不属于事实层**——DAG 里没有
// flag/answer 这种 FactKind，答案只存在于 Gate 的账本里。这是硬规矩：
// 平台确认前把候选写进事实库，会让一次重复的本地读取在重载后看起来像新事实，
// 把卡住的题无限续命。
type Candidate struct {
	Flag string
	// Source 是出处（工具调用 id 或命令摘要），用于取证。
	Source string
	// Output 是证据片段（命中处附近的原文）。
	Output string
	// Confidence 0..1。
	Confidence float64
	// Provenance 见上。
	Provenance Provenance
	// ToolCallID 是产出它（或首次观察到它）的那次工具调用，推导链的锚点。
	ToolCallID string
	// IntentID 是产出它的意图。
	IntentID string
	// Round 是产出它的轮次。
	Round int
	// Submitted 为真表示已经提交过（不论对错），永不重提。
	Submitted bool
	// Correct 为真表示平台确认正确。
	Correct bool
	// Duplicate 为真表示平台回的是幂等命中（等价于已确认）。
	Duplicate bool
	// RejectReason 是**族别归因**：为什么这个候选不被信任
	// （agent_authored / self_readback / not_grounded / …）。
	//
	// 与 SubmitError 分开是必要的：报告要能同时回答两个不同的问题——
	// 「gate 为什么认为它可疑」和「平台为什么拒它」。合成一个字段时，
	// 平台判错会覆盖族别归因，前一个问题的答案就永久丢失了。
	RejectReason string
	// SubmitError 是**平台侧**的判错/出错信息（平台判错、传输错误等）。
	SubmitError string
}

// CandidateGate 是候选答案的账本与证据闸。
//
// 契约要点：
//   - Observe 只做记账，**绝不做 IO**（不提交、不写盘）——提交只发生在轮末。
//   - New 返回本次新增且尚未提交的候选，调用方据此提交，避免重复提交。
//   - 候选必须过三闸（格式闸 / 来源闸 / 平台闸）才能提交，平台闸由调用方执行。
//   - SetIntent 在每轮开始前告诉 gate「现在跑哪个意图」，供候选回填推导链锚点。
//     v0.2 靠 `Session.IntentSink func(string,int)` 接线（`harness.go:145`），
//     漏接时所有候选 IntentID 为空、Round 为 0，而族别判定不受影响所以**不报错**
//     ——审计链断掉却无人知道。收进接口后漏接变成编译错误。
type CandidateGate interface {
	Observe(ev Event)
	Candidates() []Candidate
	New() []Candidate
	Mark(flag string, res SubmitResult, err error)
	SetIntent(intentID string, round int)
}

// RejectedLedger 记录被平台判错的答案，用于「同一答案永不重提」与回灌 prompt。
// 回灌时**只给指纹不给明文**——前身 `_scrub_flag_plaintext` 的做法。
type RejectedLedger interface {
	Record(flag string, reason string)
	Has(flag string) bool
	// Fingerprints 返回所有判错答案的指纹，供渲染进下一轮 prompt。
	Fingerprints() []string
}

// ── 策略 ──

// RunPolicy 是运行级策略。引擎在每轮开头与每次提交前咨询它。
type RunPolicy interface {
	// OnRoundStart 决定本轮是否继续；返回 (false, reason) 即终止。
	OnRoundStart(ctx context.Context, in PolicyInput) (bool, string)
	// OnCandidate 决定一个候选是否允许提交。
	OnCandidate(ctx context.Context, c Candidate) bool
}

// ── 存储 ──

// Store 是运行的持久化端口。
//
// **Append 的顺序不可颠倒**：先写事件日志，再原子写快照。先快照后事件会在
// 崩溃时产生「快照指向不存在的事件」，恢复时无法重放。
type Store interface {
	Append(ev DomainEvent) error
	Snapshot(ctx context.Context) (Snapshot, error)
	LoadEvents(afterSeq int64) ([]DomainEvent, error)
	// Private 返回私密账本。**公开文件里绝不能出现候选明文**，明文只在这里。
	Private() EvidenceStore
	Dir() string
}

// EvidenceStore 是私密证据账本。目录权限 0700，文件 0600。
type EvidenceStore interface {
	PutCandidate(c Candidate) error
	PutEvidence(ref string, data []byte) error
	GetEvidence(ref string) ([]byte, error)
	// Rejected 返回已判错答案的指纹集（明文不出 private/）。
	Rejected() ([]string, error)
}
