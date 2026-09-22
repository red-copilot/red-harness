// Package legacy 是 v0.3 的公开面，v0.5 起从根包移出。
//
// 为什么单独一个包，而不是就地删掉：这些符号在 v0.4 的生产路径上**零调用方**
// （见各文件顶部的说明），但它们是 v0.3 的公开 API，且旧图读取能力按仓库契约
// 必须保留。移出根包让「哪些端口是活的」不再需要靠读注释判断——那是这一轮
// 整理的全部目的。
//
// ⚠️ **本包是叶子**：只准 import 标准库与根包。任何实现包（store / executor /
// dag / …）都不得被它 import——那会让它变成传递依赖，把两个本该独立的子系统
// 锁死。这条规矩由 legacy/layering_test.go 用 go/parser 扫描全仓 import 图来钉。
package legacy

import (
	"encoding/json"
	"time"

	harness "github.com/red-copilot/red-harness"
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
	RunID harness.RunID   `json:"runId"`
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
