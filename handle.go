package harness

import (
	"context"
	"time"
)

// SchemaVersion 是快照与事件日志的 schema 版本。
//
// 恢复时版本不支持必须 fail closed（KindConfig），**不能**按当前版本硬解——
// 那会把新字段读成零值，状态机带着缺失的信息继续跑。
const SchemaVersion = 1

// Snapshot 是一次运行在某个时刻的完整公开状态。
//
// ⚠️ **Snapshot 没有 Flags 字段。** 候选明文只在 private/ 账本与 OutcomeView
// （返回值）里。快照会被写进 run.json、被 CLI 打印、被看板读——那些都是公开面。
type Snapshot struct {
	SchemaVersion int      `json:"schemaVersion"`
	RunID         RunID    `json:"runId"`
	State         RunState `json:"state"`
	Spec          RunSpec  `json:"spec"`
	// SpecDigest 是 Spec.Digest() 的结果。恢复时与当前 RunSpec 比对，
	// 不一致 ⇒ fail closed 并指出漂移字段。
	SpecDigest string `json:"specDigest"`
	// LastAppliedSeq 是已经应用到快照上的最后一个事件序号。恢复以它为基线重放。
	LastAppliedSeq int64         `json:"lastAppliedSeq"`
	Objective      Objective     `json:"objective"`
	BudgetUsed     Budget        `json:"budgetUsed"`
	StartedAt      time.Time     `json:"startedAt"`
	EndedAt        time.Time     `json:"endedAt,omitempty"`
	Reason         string        `json:"reason,omitempty"`
	Err            string        `json:"err,omitempty"`
	Public         PublicSummary `json:"public"`
}

// PublicSummary 是可以公开展示的计数。**只有计数与指纹，没有任何明文。**
type PublicSummary struct {
	CandidatesSeen int `json:"candidatesSeen"`
	// SubmittedConfirmed 是去重后的确认数。
	SubmittedConfirmed int `json:"submittedConfirmed"`
	Duplicates         int `json:"duplicates"`
	Rejected           int `json:"rejected"`
	HintUsed           int `json:"hintUsed"`
	Rounds             int `json:"rounds"`
	IntentDone         int `json:"intentDone"`
	Negative           int `json:"negative"`
	// ConfirmedFP 是已确认答案的指纹集（不是明文）。
	ConfirmedFP []string `json:"confirmedFingerprints,omitempty"`
}

// RunHandle 是一次运行的控制面。它由 Engine.Start / Engine.Resume 返回。
//
// 语义约定：
//   - Snapshot 是**只读**的当前状态。
//   - Events(afterSeq) 返回从 afterSeq 之后的领域事件流；channel 在 ctx 取消
//     或运行结束时关闭。看板用 afterSeq 实现 SSE 的 Last-Event-ID 重连。
//   - Pause/Resume/Cancel 是幂等的：对已处于目标状态的运行调用它们不报错。
//   - Wait 阻塞到运行终局，返回最终快照。
type RunHandle interface {
	ID() RunID
	Snapshot(ctx context.Context) (Snapshot, error)
	Events(ctx context.Context, afterSeq int64) (<-chan DomainEvent, error)
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Cancel(ctx context.Context) error
	Wait(ctx context.Context) (Snapshot, error)
}
