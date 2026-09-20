package harness

import (
	"encoding/json"
	"time"
)

// DomainEvent 是一条带单调序号的领域事件。恢复以它为重放单位。
//
// **Payload 里不得出现候选明文。** 候选相关的事件只带指纹——公开文件
// （run.json / events.jsonl / graph.json / report.* / 看板）是可能被拷走、
// 被贴进工单、被上传的，明文只允许存在于 private/ 与返回值里。
type DomainEvent struct {
	// Seq 是单调递增的序号，从 1 开始。Store.Append 拒绝跳号。
	Seq   int64           `json:"seq"`
	At    time.Time       `json:"at"`
	Type  DomainEventType `json:"type"`
	RunID RunID           `json:"runId"`
	// Round 是关联轮号（0 表示与具体轮次无关）。
	Round int `json:"round,omitempty"`
	// Payload 是事件负载。每种事件有各自的形状，由重放逻辑解释。
	Payload json.RawMessage `json:"payload,omitempty"`
}

// DomainEventType 是领域事件种类。
//
// **新增种类必须同时更新重放逻辑**——否则旧 run 恢复到新代码上时，新事件会
// 被当成未知类型静默忽略，状态机停在半路。
type DomainEventType string

const (
	EvRunCreated   DomainEventType = "run_created"
	EvRunPreparing DomainEventType = "run_preparing"
	// EvTargetStarted 表示一道题已起容器。
	EvTargetStarted DomainEventType = "target_started"
	EvRoundStarted  DomainEventType = "round_started"
	EvIntentActive  DomainEventType = "intent_active"
	EvIntentSettled DomainEventType = "intent_settled"
	// EvCandidateSeen 只含指纹。
	EvCandidateSeen DomainEventType = "candidate_seen"
	// EvSubmitResult 只含指纹与判定。
	EvSubmitResult  DomainEventType = "submit_result"
	EvHintRequested DomainEventType = "hint_requested"
	EvFactLearned   DomainEventType = "fact_learned"
	EvBudgetUsed    DomainEventType = "budget_used"
	// EvRunPaused 在暂停时落盘，且此时被中断的 intent 必须标记为 interrupted
	// ——恢复后**不假定中断的动作成功**。
	EvRunPaused  DomainEventType = "run_paused"
	EvRunResumed DomainEventType = "run_resumed"
	EvRunEnded   DomainEventType = "run_ended"
)
