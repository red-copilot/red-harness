package harness

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
	// ToolCallID 是产出它（或首次观察到它）的那次工具调用，DAG 推导链的锚点。
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

// Gate 是候选答案的账本与证据闸。
//
// 契约要点：
//   - Observe 只做记账，**绝不做 IO**（不提交、不写盘）——提交只发生在轮末。
//   - New 返回本次新增且尚未提交的候选，调用方据此提交，避免重复提交。
//   - 候选必须过三闸（格式闸 / 来源闸 / 平台闸）才能提交，平台闸由调用方执行。
type Gate interface {
	// Observe 处理一个事件，抽取候选并记账。
	Observe(ev Event)
	// Candidates 返回全部候选（含已提交的），用于报告与续跑。
	Candidates() []Candidate
	// New 返回尚未提交过的候选。
	New() []Candidate
	// Mark 回填提交结果。
	Mark(flag string, res SubmitResult, err error)
}

// RejectedLedger 记录被平台判错的答案，用于「同一答案永不重提」与回灌 prompt。
// 回灌时**只给指纹不给明文**——前身 `_scrub_flag_plaintext` 的做法。
type RejectedLedger interface {
	// Record 记录一个被平台判错的答案。
	Record(flag string, reason string)
	// Has 报告某答案是否已被判错。
	Has(flag string) bool
	// Fingerprints 返回所有判错答案的指纹（sha256[:8] + 长度 + 首尾字符），
	// 供渲染进下一轮 prompt。
	Fingerprints() []string
}
