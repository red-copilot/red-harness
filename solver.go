package harness

import "context"

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

type Event struct {
	Kind EventKind
	Tool string
	Args map[string]any
	// ToolCallID 是 pi 的 toolCallId。它是 DAG 推导链的锚点：事实靠它回溯到
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

// DefaultFlagFormat 是**未知答案形态时的占位提示**，不是判定依据。
// 真正的判定依据是 answer.Shape（从题面推断）。渲染进 prompt 时若题目没有
// 明确形态，应当把「答案格式以题面为准」这句话原样带上，而不是硬塞 flag{...}。
const DefaultFlagFormat = "flag{...}"

// 解题终止原因。护栏依赖这些字符串，不要改。
const (
	ReasonCompleted = "completed"
	ReasonTimeout   = "timeout"
	ReasonStalled   = "stalled"
	ReasonStopped   = "stopped"
	ReasonMaxTurns  = "max_turns"
	ReasonError     = "error"
	// ReasonNoIntent 表示意图前沿耗尽——所有方向都试过或都已证伪。
	ReasonNoIntent = "no_intent"
	// ReasonSolved 表示全部 flag 已被平台确认。
	ReasonSolved = "solved"
	// ReasonProviderFailure 表示 0 回合 + 有错误——pi 把 provider 错误呈现为
	// 一次静默的空会话。这是前身「280 run / 0 flag / 63 题」事故的护栏。
	ReasonProviderFailure = "provider_failure"
)

type SolveRequest struct {
	Prompt     string
	Workdir    string
	FlagFormat string
}

type SolveResult struct {
	Flags     []string
	FinalText string
	Turns     int
	Reason    string
	Err       string
}

// Solver 是**一次性**求解后端：给一个 prompt，跑完，返回。保留它用于降级与
// 对照（例如 REDCOPILOT_PI_SESSION_REUSE=0 或替换成别的引擎）。
type Solver interface {
	Solve(ctx context.Context, req SolveRequest, emit func(Event)) (SolveResult, error)
}

// AgentStart 是启动一个持久 agent 会话所需的全部信息。
type AgentStart struct {
	Workdir string
	// SystemPrompt 走 --append-system-prompt（保留 pi 默认编码能力）。
	SystemPrompt string
	// Extensions 是 extension 文件的**绝对路径**，走 -e。M0 实测：非交互模式下
	// 项目本地资源默认被忽略，所以必须绝对路径 + Approve。
	Extensions []string
	SessionDir string
	Provider   string
	Model      string
	// Approve 对应 pi 的 --approve。不开的话项目资源会被静默忽略。
	Approve bool
	// Thinking 对应 pi 的 --thinking（off/minimal/low/medium/high/xhigh/max）。
	Thinking string
}

// RoundResult 是**一轮**的结果。一轮 = 一个意图 = 一次 prompt → 等 agent_settled。
type RoundResult struct {
	// Turns 是这一轮里的工具调用次数（沿用前身语义：数 tool_execution_start）。
	// 护栏依赖它，不要改成别的含义。
	Turns int
	// Text 是本轮 agent 的最终文本（text_delta 拼接）。
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

// Stats 是 pi 会话的权威计数。M0 实测 get_session_stats 的字段已确认。
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

// Agent 是**持久**的解题 agent：进程常驻，每题一次 new_session，DAG 轮循环
// 用它一轮一个意图地推进。
type Agent interface {
	// Start 起进程、握手、new_session，并断言会话已复位。
	Start(ctx context.Context, req AgentStart) error
	// Round 发一次 prompt 并等到 agent_settled（或出错/超时）。
	Round(ctx context.Context, prompt string, emit func(Event)) (RoundResult, error)
	// Steer 中途注入一条消息（hint / 剩余时间提醒）。agent 空闲时等价于 prompt。
	Steer(ctx context.Context, msg string) error
	// Stats 取权威计数（turns/tokens/cost/context）。
	Stats(ctx context.Context) (Stats, error)
	// Close 终止进程组。pi 不会自己退出（M0 实测），必须显式 killpg。
	Close(ctx context.Context) error
}
