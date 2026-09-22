package legacy

import (
	"context"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// ── v0.3 的端口 ──
//
// 这一组端口在 v0.4 的生产路径上**零运行时调用方**：v0.4 的 Harness 走
// Scenario / Sandbox(SandboxSession) / AgentFactory / Planner / Renderer /
// CandidateGate / ResultStore / GraphSaver / RunLocker，与这里的每一个都不同。
//
// 它们仍然留在源码里，是因为两件不能丢的东西：
//
//   - **v0.3 的公开 API**：调用方可能已经按它们写了代码，删掉是破坏性变更；
//   - **旧图/旧快照的读取能力**：`RunSpec` / `Snapshot` / `DomainEvent` 的形状
//     是写进过文件的，读取路径必须继续成立。
//
// 它们**不再属于根包**：根包公开面的每一项都应当是「活的」，而这三个端口需要
// 靠读注释才能判断死活——那正是这次整理要消灭的东西。

// Executor 是 v0.3 的隔离执行环境端口。v0.4 用 Sandbox / SandboxSession。
//
// ⚠️ **注意它引用的 ExecSpec / ExecResult / ExecHandle / ExecOptions 仍在根包**：
// 那几个 DTO 在 v0.4 的**活路径**上（`executor/session.go` 的 NewSession 构造
// ExecSpec，dockerSession 消费它）。它们只是名字带 v0.3 血统，不是死面——
// 把它们一起搬进 legacy 会让「哪些类型是活的」重新变得需要猜。
//
// `executor.Docker` 同时满足本接口与 `harness.Sandbox`（两个编译期断言并存），
// 这是「同一个实现服务两代端口」的显式记录。
type Executor interface {
	// Prepare 按 spec 创建容器与网络，返回句柄。
	Prepare(ctx context.Context, spec harness.ExecSpec) (harness.ExecHandle, error)
	// Exec 在句柄指向的容器里跑一条命令。
	Exec(ctx context.Context, h harness.ExecHandle, cmd []string, opts harness.ExecOptions) (harness.ExecResult, error)
	// Reclaim 按 run label 精确回收本次运行创建的全部容器与网络。
	// 正常结束、取消、宿主重启恢复三条路径都要能调它。
	Reclaim(ctx context.Context, runID harness.RunID) error
	// Available 检查执行器本身可用（Docker 在不在）。
	Available(ctx context.Context) error
}

// RunPolicy 是 v0.3 的运行级策略端口。v0.4 用 PolicySpec / Budget / HintPolicy
// 三个显式配置面替代它，并且**在副作用之前校验**（见根包 RunSpec.Validate）。
//
// 唯一的消费者是 Options.Policy。
type RunPolicy interface {
	// OnRoundStart 决定本轮是否继续；返回 (false, reason) 即终止。
	OnRoundStart(ctx context.Context, in PolicyInput) (bool, string)
	// OnCandidate 决定一个候选是否允许提交。
	OnCandidate(ctx context.Context, c harness.Candidate) bool
}

// PolicyInput 是 RunPolicy.OnRoundStart 的入参。
type PolicyInput struct {
	Challenge  harness.Challenge
	Outcome    harness.OutcomeView
	BudgetUsed harness.Budget
	Round      int
}

// RunSummary 是 v0.3 的 Engine.List 返回项：足以在 CLI 里列出一次运行，且
// **不含任何明文**。
//
// v0.4 的 list 直接读公开结果（`harness.ResultStore`），不经过它。
type RunSummary struct {
	RunID     harness.RunID     `json:"runId"`
	State     harness.RunState  `json:"state"`
	Scenario  string            `json:"scenario"`
	Targets   []string          `json:"targets,omitempty"`
	StartedAt time.Time         `json:"startedAt"`
	EndedAt   time.Time         `json:"endedAt,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	Objective harness.Objective `json:"objective"`
	Score     int               `json:"score"`
}

// ── 存储 ──

// GraphBlob 是 DAG 的序列化字节。**它是一个不透明的载荷**：store 只负责路径与
// 原子性，不理解它的内容。
//
// 为什么用 []byte 而不是让 store 认识 dag 的类型：`dag/store.go` 的 schema 1 +
// migrate 是前向兼容的**唯一**实现，store 再写一份 DAG 序列化就会有第二份实现
// （v0.2 的 `dag.FlagFingerprint` 就是被这样分叉出来的）。而且 store 一旦导入
// dag，两者就不能并行开发了——而它们本来就是不同波次的独立子系统。
type GraphBlob = []byte

// GraphStore 是 v0.3 的 DAG 落盘旁路端口。
//
// ⚠️ v0.4 的图落盘**不走它**：走的是 `harness.GraphSaver`（端口接 runID +
// Challenge，由装配层的 dagGraphSaver 实现，生产恒装配）。本端口零生产调用方。
//
// **为什么 DAG 不走 Store.Append**：领域事件流是「引擎状态」的日志，而 DAG 是
// 一个独立的、由 Planner 拥有的数据结构——它自己决定何时落盘。
type GraphStore interface {
	PutGraph(blob GraphBlob) error
	GetGraph() (GraphBlob, error)
}

// Store 是 v0.3 运行的持久化端口。
//
// **Append 的顺序不可颠倒**：先写事件日志，再原子写快照。先快照后事件会在
// 崩溃时产生「快照指向不存在的事件」，恢复时无法重放。这条契约至今仍由
// `store` 包的测试钉着（注入快照写失败来验），所以端口搬走了、纪律没搬走。
//
// ⚠️ 注意 `store.FileStore` 本身**不是死代码**：装配层仍然 `store.New(...)`
// 并用它的 `ForRun` / `PutGraph` / `PutGraphExport` 写 graph.json 与 graph.mmd
// ——但那三个方法**本来就在本端口之外**（见 store.go 的 PutSnapshot 注释）。
type Store interface {
	Append(ev DomainEvent) error
	Snapshot(ctx context.Context) (Snapshot, error)
	LoadEvents(afterSeq int64) ([]DomainEvent, error)
	// Private 返回私密账本。**公开文件里绝不能出现候选明文**，明文只在这里。
	Private() EvidenceStore
	Dir() string
}

// EvidenceStore 是 v0.3 的私密证据账本。目录权限 0700，文件 0600。
//
// v0.4 的私密面是另一条路径：`<ResultDir>/private/<runID>/`（trace 与候选审计）。
// 本端口零生产调用方——它对应的 `candidates.jsonl` 落点至今没有写入方。
type EvidenceStore interface {
	PutCandidate(c harness.Candidate) error
	PutEvidence(ref string, data []byte) error
	GetEvidence(ref string) ([]byte, error)
	// Rejected 返回已判错答案的指纹集（明文不出 private/）。
	Rejected() ([]string, error)
}
