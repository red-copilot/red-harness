package harness

// EventKind 是归一化后的事件种类。pi 的原始事件名（tool_execution_start 等）
// 在 piai 包里被映射到这些种类，其余子系统只认这里。
type EventKind string

const (
	EventToolStart    EventKind = "tool_start"
	EventToolEnd      EventKind = "tool_end"
	EventToolProgress EventKind = "tool_progress"
	EventText         EventKind = "text"
	EventThinking     EventKind = "thinking"
	EventTurnDone     EventKind = "turn_done"
	EventSettled      EventKind = "settled"
	EventCompaction   EventKind = "compaction"
	EventRetry        EventKind = "retry"
	EventUIRequest    EventKind = "ui_request"
	EventError        EventKind = "error"
)

// Event 是一个归一化事件。
//
// ⚠️ **Event 会被扇出到公开面**（看板、日志、transcript），所以它**不得携带
// 候选明文**。gate 从 Output/Args 里抽候选是内部行为，抽出来的明文只进
// private/ 账本。
type Event struct {
	Kind EventKind
	Tool string
	Args map[string]any
	// ToolCallID 是 pi 的 toolCallId。它是推导链的锚点：事实靠它回溯到
	// 「哪一次工具调用产出了我」，gate 的 provenance 判定也靠它。
	ToolCallID string
	// Output 是工具输出的文本（已由 piai 从 pi 的 content 块里拼好）。
	Output string
	// Details 是 tool_execution_end.result.details 的原始载荷。report_fact
	// 的结构化事实就走这里（M0 实测 262 KB 不截断）。
	Details map[string]any
	// IsError 为真表示这次工具调用本身失败了。
	IsError bool
	Text    string
	Err     string
}

// 解题终止原因。护栏依赖这些字符串，不要改。
//
// **可以新增，不得改变已有值**——三个回归测试钉死了它们
// （`docs/superpowers/plans/fixtures/v0.2-regression_test.go.txt`）。
const (
	ReasonCompleted = "completed"
	ReasonTimeout   = "timeout"
	// ReasonStalled 在 v1 未被写入任何代码路径。**保留常量**：它是公开 API 的
	// 一部分（调用方可能已经按它写分支），删掉是破坏性变更。
	ReasonStalled  = "stalled"
	ReasonStopped  = "stopped"
	ReasonMaxTurns = "max_turns"
	ReasonError    = "error"
	// ReasonMaxRounds 是 v0.3 新增：轮次预算耗尽。
	//
	// v0.2 里轮次耗尽返回 ReasonMaxTurns（`harness.go:33`），与工具调用次数
	// 耗尽合成一个字符串——报告因此无法回答「为什么停」。
	ReasonMaxRounds = "max_rounds"
	// ReasonMaxCost 是 v0.3 新增：成本预算耗尽。
	//
	// v0.2 里成本耗尽返回 ReasonTimeout（`harness.go:42`，从墙钟分支拷来的）。
	// 而且那条分支**从轮循环不可达**——`used.MaxCostUSD` 取自 `out.Stats.CostUSD`，
	// 而 `out.Stats` 只在循环结束后才赋值一次。成本预算在 v0.2 实际是死代码。
	ReasonMaxCost = "max_cost"
	// ReasonNoIntent 表示意图前沿耗尽——所有方向都试过或都已证伪。
	ReasonNoIntent = "no_intent"
	// ReasonSolved 表示全部 flag 已被平台确认。
	ReasonSolved = "solved"
	// ReasonProviderFailure 表示 provider 故障——pi 把 provider 错误呈现为
	// 一次静默的空会话。这是前身「280 run / 0 flag / 63 题」事故的护栏。
	ReasonProviderFailure = "provider_failure"
	// ReasonSubmitLimit 是 v0.5 新增：本题撞到了 PolicySpec.MaxSubmissionsPerChallenge
	// 的提交次数上限，已停止提交并结束本题。
	//
	// 为什么单列一个原因而不是复用 ReasonError 或 ReasonMaxRounds：这三种「停」
	// 指向完全不同的处置——轮次用尽是规划层的问题（意图拆得不够细），提交超限是
	// 候选集合失控（多半是答案形态判宽了），而以错误收场是运行坏了。2026-09-22
	// 那次 147 次提交的事故里，公开面上只能看到一个「提交过」的计数，没有任何东西
	// 指出「这次提交次数不正常」。
	ReasonSubmitLimit = "submit_limit"
	// ReasonNoProgress 是 v0.4 新增：一次运行**正常跑完**，但没有任何一道题
	// 达成目标。
	//
	// 为什么必须与 ReasonCompleted 分开：v0.2/v0.3 把「没有错误」当成成功，
	// 于是「280 run / 0 flag」在报告里是一片绿。运行层面的成功与题目层面的
	// 成功是两个问题，报告要能分别回答。
	ReasonNoProgress = "no_progress"
)

// AgentStart 是启动一个持久 agent 会话所需的全部信息。
//
// 与 v0.2 的差异：多了 HomeDir 与 SessionReuse。
type AgentStart struct {
	Workdir string
	// SystemPrompt 走 --append-system-prompt（保留 pi 默认编码能力）。
	SystemPrompt string
	// Extensions 是 extension 文件的**绝对路径**，走 -e。
	//
	// M0 实测：非交互模式下相对路径被静默忽略，且不开 Approve 时项目本地资源
	// 也被静默忽略。所以必须绝对路径 + Approve。
	Extensions []string
	SessionDir string
	Provider   string
	Model      string
	// Approve 对应 pi 的 --approve。
	Approve bool
	// Thinking 对应 pi 的 --thinking（off/minimal/low/medium/high/xhigh/max）。
	Thinking string
	// HomeDir 是**每题独立**的 HOME。
	//
	// pi 从 `$HOME/.pi/agent/*` 发现扩展 / 角色 / 技能——共享 HOME 会跨题污染
	// （前身已踩过）。为空时用进程默认 HOME。
	HomeDir string
	// SessionReuse 为假时每题强制新进程（前身降级开关
	// REDCOPILOT_PI_SESSION_REUSE=0 的等价物）。
	SessionReuse bool
}

// RoundResult 是**一轮**的结果。一轮 = 一个意图 = 一次 prompt → 等本轮结束。
type RoundResult struct {
	// Turns 是这一轮里的工具调用次数（沿用前身语义：数 tool_execution_start）。
	// 护栏依赖它，不要改成别的含义。
	Turns int
	// Text 是本轮 agent 的最终文本。
	Text string
	// Reason 见 Reason* 常量。
	Reason string
	// Err 非空表示本轮异常。**pi 进程中途死亡必须在这里置非空**，否则 0 回合
	// 护栏失效（前身事故）。
	//
	// 语义上是**本轮**的：它是「本轮任何异常」的混合字段——进程死亡、extension
	// 错误、provider 错误都往这里塞。实测确认（piai 的 `provider1st` 场景）：
	// 第 1 轮 provider 失败、第 2 轮正常时，第 2 轮的 Err 是**空**的，因为
	// piai 的轮级累加器每轮新建。
	Err string
	// ProviderError 是**只针对 provider 故障**的干净判据，与 Err 的「什么都装」
	// 刻意分开。
	//
	// 为什么不能直接用 Err：`Err != ""` 无法区分「模型服务挂了」与「进程死了 /
	// extension 报错」——而这两种情形的处置完全不同（前者该换 provider 或停机，
	// 后者该重启进程）。判 provider 故障必须有一个不会被别的异常污染的字段。
	ProviderError string
	// CancelledUI 是被自动取消的对话框次数。它意味着 agent 索要输入却拿不到，
	// 该题结果的可解释性依赖这个事实，所以要透出。
	CancelledUI int
}

// Stats 是 agent 会话的权威计数。M0 实测 get_session_stats 的字段已确认。
type Stats struct {
	Turns      int     `json:"turns"`
	TokensIn   int     `json:"tokensIn"`
	TokensOut  int     `json:"tokensOut"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	CostUSD    float64 `json:"costUSD"`
	SessionID  string  `json:"sessionId"`
	// ContextTokens / ContextWindow 用于观测上下文压力。
	ContextTokens int `json:"contextTokens"`
	ContextWindow int `json:"contextWindow"`
}
