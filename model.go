package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ── 通用模型 ──
//
// 这一层刻意与 TSecBench 无关：Target/Objective/Candidate/Evaluation 是任何
// 授权安全场景都有的概念，平台特有的 challenge/container/flag 映射由
// Scenario 适配器负责。

// Target 是一个授权目标。Addrs 是**引擎视角**可达的地址；容器内视角的地址
// 由 ExecHandle.Addrs 给出（两者可能不同——NAT、端口映射）。
type Target struct {
	// Code 是目标在场景里的唯一标识（TSecBench 的 unique_code）。
	Code string `json:"code"`
	// Addrs 是目标端点，形如 "10.0.0.1:8080"。
	Addrs []string `json:"addrs,omitempty"`
	// Network 是目标流量的协议族："tcp" / "http" / "unix"。
	Network string `json:"network,omitempty"`
}

// Objective 是成功条件。Kind 决定 Want/Got 怎么解读。
type Objective struct {
	// Kind 取值："flag_count" / "score" / "predicate"。
	Kind string `json:"kind"`
	// Want 是目标量；Got 是当前已确认量。
	Want int `json:"want"`
	Got  int `json:"got"`
	// Completed 由场景的权威进度判定，不靠本地推测。
	Completed bool `json:"completed"`
}

// Evaluation 是一次候选提交的判定结果。
//
// 字段与平台无关：TSecBench 的 SubmitResult 由 Scenario 映射进来。
type Evaluation struct {
	// Accepted 为真表示平台确认这个答案正确。**幂等命中也算正确**
	// （平台回 Duplicate 时 Accepted 仍为真——它等价于「这个答案已经被确认过」）。
	Accepted bool `json:"accepted"`
	// Progress 为真表示这次提交让平台侧进度前进了。
	Progress bool `json:"progress"`
	// Completed 为真表示目标已全部达成。
	Completed bool `json:"completed"`
	// Score 是本次得分（平台语义）。
	Score int `json:"score"`
	// Message 是平台原样返回的说明，用于报告与诊断。
	Message string `json:"message,omitempty"`
}

// ── 运行状态 ──

type RunState string

const (
	RunCreated   RunState = "created"
	RunPreparing RunState = "preparing"
	RunRunning   RunState = "running"
	RunPaused    RunState = "paused"
	RunCompleted RunState = "completed"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
	// RunFinished 是 v0.4 的运行终态：Run 的轮循环**正常走到了终点**。
	//
	// 为什么不复用 RunCompleted，以及为什么必须有这个值：
	// `RunResult.Completed` 的含义是「**有题目解出来了**」（平台权威的
	// Objective.Completed），而一次运行完全可以正常结束却一道题都没解出来——
	// 那正是前身「280 run / 0 flag」的形状。用一个词表示两件事，报告就无法区分
	// 「跑完了且解出来了」与「跑完了但什么都没解出来」，而后者恰恰是最需要被
	// 看见的那一类。
	//
	// 所以 v0.4 把它们拆成两个问题：
	//   - `State`：运行**怎么结束的**（finished / failed / cancelled）；
	//   - `Completed`：运行**解出来了没有**。
	//
	// RunCompleted 保留不动：它是 v0.3 快照面的值，旧图与旧快照的读取路径仍在。
	RunFinished RunState = "finished"
)

// Terminal 报告这个状态是否已经终局。
//
// 为什么要它：恢复一个已经结束的 run 是最容易伤到真实用户的输入之一——
// 合理预期是明确拒绝，而不是重新跑一轮、重复向平台提交 flag、重复消耗预算。
// 引擎的 Resume 必须先用它挡一道。
func (s RunState) Terminal() bool {
	switch s {
	case RunCompleted, RunFailed, RunCancelled, RunFinished:
		return true
	}
	return false
}

// RunID 标识一次运行。它是 run 目录名，所以必须是文件系统安全的。
type RunID string

// ── 运行规格 ──

// RunSpec 是一次运行的完整配置。创建后计算摘要（Digest），恢复时拒绝漂移。
//
// ⚠️ **不要把凭据放进这里。** RunSpec 会整份写进 run.json（公开文件）。
// provider 的 API key 走进程环境变量，平台 token 走 bridge 子进程环境变量。
type RunSpec struct {
	// Scenario 是场景名，对应 Options.Scenarios 的键。
	Scenario string `json:"scenario"`
	// Targets 是题目 code 列表。空表示「全部未完成的」。
	//
	// 顺序敏感：Digest 把顺序算进去，因为执行顺序会影响平台侧状态。
	Targets  []string     `json:"targets,omitempty"`
	Agent    AgentSpec    `json:"agent"`
	Executor ExecutorSpec `json:"executor"`
	// Sandbox is the v0.4 execution configuration. Executor remains for the
	// v0.3 compatibility surface and old graph snapshots.
	Sandbox SandboxSpec `json:"sandbox,omitempty"`
	Budget  Budget      `json:"budget"`
	// HintPolicy 见 HintOff / HintAuto / HintAlways。
	HintPolicy string `json:"hintPolicy"`
	// Submit 为假时只记账不提交（干跑）。干跑用于离线验证与对照。
	Submit bool `json:"submit"`
	// ⚠️ **运行目录不在 RunSpec 里。** StoreDir / ResultDir 曾是这里的字段，
	// v0.4 把它们移出：它们是**部署级配置**（这台机器把结果写在哪），不是运行
	// 意图（要跑哪几道题、花多少预算）。留在 RunSpec 里的代价是它们会进 Digest，
	// 于是「换个 cwd 跑同一份配置」被报成配置漂移——而更糟的是，调用方会以为
	// 自己填的那个值生效了，实际装配层每次都会把它盖掉（见 wire.resolve）。
	// 现在唯一的来源是装配配置（`internal/wire.Options`），CLI 不再往 spec 里写。
	Profile SolverProfile `json:"profile,omitempty"`
	Policy  PolicySpec    `json:"policy"`
}

// Digest 返回配置摘要（sha256 前 16 个十六进制字符）。
//
// 恢复时用它做 fail closed：摘要不一致就拒绝恢复，并指出漂移字段。
// 静默用新配置继续会让预算护栏与报告同时失真——用户改了预算再恢复，
// 报告里的「为什么停」就变成假的了。
func (s RunSpec) Digest() string {
	// 结构体整体序列化：字段顺序由结构体定义固定，map（ExecutorSpec.Env）
	// 的键由 encoding/json 排序，所以同一份配置总是得到同一个摘要。
	b, err := json.Marshal(s)
	if err != nil {
		// RunSpec 全是可序列化类型；真出错说明有人加了不可序列化的字段，
		// 那时宁可要一个显眼的摘要也不要静默的空串。
		return "digest-error"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Validate 检查 RunSpec 里那些「配错了不会报错、只会静默不生效」的字段。
//
// 为什么需要它：v0.5 之前 HintPolicy **没有任何取值校验**——Run 的轮循环只补了
// 空串默认，然后 `shouldHint` 用 `== HintAlways` / `== HintAuto` 判断，于是传
// "alwayss" 与传 "off" 完全等价，且完全静默。这类「看起来在、实际没生效」的
// 字段在本仓库被明确记为一类比缺失更糟的缺陷（见 v04_test.go 里那条注释）。
//
// 调用点在 Harness.Run 的**最开头**，早于跨进程锁：`locker.Lock` 会写
// `<StoreDir>/run.lock`，那是一处文件系统副作用，而配置错误是纯粹的调用方
// 错误——没有任何理由先落下副作用再报错。
func (s RunSpec) Validate() error {
	if err := s.Profile.Validate(); err != nil {
		return err
	}
	switch s.HintPolicy {
	case "", HintOff, HintAuto, HintAlways:
		return nil
	default:
		return Ef(KindConfig, "harness.spec",
			fmt.Sprintf("hintPolicy=%q 不是 off/auto/always 之一", s.HintPolicy), nil)
	}
}

// AgentSpec 是 pi（或别的 agent 后端）的启动配置。
type AgentSpec struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Thinking 对应 pi 的 --thinking（off/minimal/low/medium/high/xhigh/max）。
	Thinking string `json:"thinking,omitempty"`
	// Extensions 是 extension 文件的**绝对路径**，走 -e。
	//
	// M0 实测：非交互模式下相对路径被**静默忽略**，且不开 Approve 时项目本地
	// 资源也被静默忽略。所以这里必须是绝对路径，Approve 必须是真。
	//
	// 前身那条「LLM 一条命令写满 307 GB 磁盘」的事故，对策是一个 bash 护栏
	// extension——契约层在这里留了装载位，extension 本体不在本计划范围内。
	Extensions []string `json:"extensions,omitempty"`
	// Approve 对应 pi 的 --approve。不开则项目资源被静默忽略。
	Approve bool `json:"approve"`
	// SessionDir 是 pi 的 --session-dir。
	SessionDir string `json:"sessionDir,omitempty"`
	// HomeDir 是**每题独立**的 HOME。
	//
	// pi 从 `$HOME/.pi/agent/*` 发现扩展 / 角色 / 技能——共享 HOME 会跨题污染，
	// 前身已经踩过这个坑。为空时由引擎按 `<runDir>/.pi-home` 生成。
	HomeDir string `json:"homeDir,omitempty"`
	// SessionReuse 为假时每题强制新进程（前身降级开关
	// REDCOPILOT_PI_SESSION_REUSE=0 的等价物）。
	SessionReuse bool `json:"sessionReuse"`
}

// ExecutorSpec 是隔离执行器的配置。引擎把它加上运行身份与网络边界之后，
// 交给 Executor 的是 ExecSpec。
type ExecutorSpec struct {
	Image     string  `json:"image"`
	Workdir   string  `json:"workdir"`
	CPUs      float64 `json:"cpus,omitempty"`
	MemoryMB  int     `json:"memoryMB,omitempty"`
	PidsLimit int     `json:"pidsLimit,omitempty"`
	ReadOnly  bool    `json:"readOnly"`
	// AllowHosts 是目标地址白名单（IP:port）。其余出站默认拒绝。
	AllowHosts []string          `json:"allowHosts,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// PolicySpec 是运行级策略。0 值由引擎填默认值。
type PolicySpec struct {
	// MaxAttemptsPerIntent 是同一意图的最大重试轮数（沿用 dag.DefaultMaxAttempts）。
	MaxAttemptsPerIntent int `json:"maxAttemptsPerIntent,omitempty"`
	// DryRoundsBeforeHint 是连续无进展多少轮后允许请求提示（沿用 HintAuto 的阈值）。
	DryRoundsBeforeHint int `json:"dryRoundsBeforeHint,omitempty"`
}

// ── 预算 ──

// Budget 是一道题的预算。任一维度耗尽即终止。
//
// 字段与 DefaultBudget 的默认值沿用 v0.2（40 rounds / 30 分钟 / 600 tool turns），
// 但修掉了 Exhausted 的两个缺陷。
type Budget struct {
	// MaxRounds 是意图轮次上限（一轮 = 一个意图）。
	MaxRounds int `json:"maxRounds"`
	// MaxWall 是墙钟上限。
	MaxWall time.Duration `json:"maxWall"`
	// MaxTurns 是工具调用总数上限（跨轮累计），护栏语义沿用前身。
	MaxTurns int `json:"maxTurns"`
	// MaxCostUSD 是累计成本上限（0 表示不限）。
	MaxCostUSD float64 `json:"maxCostUSD"`
}

// DefaultBudget 是保守默认值，参考前身 `ADAPTER_*` 的量级。
func DefaultBudget() Budget {
	return Budget{MaxRounds: 40, MaxWall: 30 * time.Minute, MaxTurns: 600}
}

// Exhausted 报告预算是否已耗尽，并给出原因。
//
// 与 v0.2 的两处差异（都是缺陷修正，见设计文档 §2.2）：
//
//   - rounds 耗尽返回 ReasonMaxRounds 而不是 ReasonMaxTurns。**这是行为变更**：
//     报告里「轮次用完了」与「工具调用次数用完了」是两件事，合成一个字符串
//     让报告无法回答「为什么停」。
//   - 成本耗尽返回 ReasonMaxCost 而不是 ReasonTimeout（后者是从墙钟分支拷来的）。
//
// **分支顺序是契约**：多个维度同时耗尽时，靠前的那个决定 Reason。顺序沿用
// v0.2（rounds → wall → turns → cost）。
func (b Budget) Exhausted(used Budget) (bool, string) {
	if b.MaxRounds > 0 && used.MaxRounds >= b.MaxRounds {
		return true, ReasonMaxRounds
	}
	if b.MaxWall > 0 && used.MaxWall >= b.MaxWall {
		return true, ReasonTimeout
	}
	if b.MaxTurns > 0 && used.MaxTurns >= b.MaxTurns {
		return true, ReasonMaxTurns
	}
	if b.MaxCostUSD > 0 && used.MaxCostUSD >= b.MaxCostUSD {
		return true, ReasonMaxCost
	}
	return false, ""
}

// ── 一道题的结果视图 ──

// OutcomeView 是一道题的最终结果，供 Renderer 与报告消费。
//
// 与 v0.2 的 Outcome 的差异：
//   - `Solve SolveResult` 随 Solver 接口一并删除（一次性后端已移除）。
//   - `Duration()` 移到这里；`Solved()` 删除——它的语义被 Objective.Completed
//     取代，留着两个「解出来了吗」的判据只会让调用方各挑一个。
//   - `IntentDone`/`Negative`/`Report` 三个字段在 v0.2 是**死字段**（零赋值点），
//     现在由引擎真实回填。
type OutcomeView struct {
	Code string
	// Reason 见 Reason* 常量。
	Reason string
	// Flags 是被平台确认正确的答案。
	//
	// ⚠️ **这是返回值，不是落盘物。** 明文绝不进入 run.json / events.jsonl /
	// graph.json / 报告 / 看板。
	Flags []string
	// Candidates 是全部候选（含被拒的），用于报告的可解释性。
	Candidates []Candidate
	// Submitted 是**去重后的确认数**（不是提交次数——v0.2 契约这里统计错了）。
	Submitted int
	// Duplicates 是平台幂等命中数（等价于已确认，所以计入 Submitted）。
	Duplicates int
	// Rejected 是被平台判错的候选数。
	Rejected int
	// Rounds 是实际消耗的意图轮次。
	Rounds int
	// IntentDone 是达成的意图数（来自图的状态统计）。
	IntentDone int
	// Negative 是写入的死胡同（已证伪方向）数。
	Negative int
	// HintUsed 是提示次数（提示会按比例扣分，所以要单列）。
	HintUsed int
	// BranchesAbandoned 是被放弃的分支数（换支次数）。
	//
	// 为什么要单列：一次运行以 `no_intent` 收场时，报告必须能回答「是因为所有
	// 方向都做完了，还是因为编排层把几个方向判成了停滞而扔掉」——这两个答案指向
	// 完全不同的改法（前者是题目确实做不动，后者是阈值或提示策略需要调）。
	BranchesAbandoned int
	// ProgressConfirmed / ProgressTotal 是**平台权威的**作答进度。
	//
	// 为什么不用 `len(Flags)` 代替：「通关立即终止」必须能在**不知道 FlagCount**
	// 的情况下也生效——StartResult 不携带 FlagCount，调用方也可能没填
	// Challenge。而平台每次 Submit 都会回权威进度，这是唯一不依赖调用方自觉的
	// 判据。前身那条「通关即停」是死代码，事故现场就是这么来的。
	ProgressConfirmed int
	ProgressTotal     int
	// RemainingAtStart 是**起跑时**还差几个 flag（Discover 时从平台进度算出）。
	//
	// 为什么必须单独记：「本次新增确认了多少」不能用最终累计进度代替。平台上的
	// 题目可能已经做掉一部分，把历史进度算成本次能力会让通过率结论系统性偏高，
	// 而且两批题目组成不同时完全不可比。增量 = ProgressConfirmed -
	// (ProgressTotal - RemainingAtStart)；RemainingAtStart 为 0 表示分母未知
	// （FlagCount 未知），此时不得宣称召回率。
	RemainingAtStart int
	// Score 是平台给出的累计得分（来自 Evaluation.Score）。
	Score int
	// Stats 是 agent 会话的权威计数。
	Stats Stats
	// Report 是报告文件路径（若已生成）。
	Report string
	// Err 非空表示这道题以异常收场。
	Err string
	// CleanupFailures records failed cleanup stages without exposing raw errors.
	CleanupFailures []string
	// GraphSaveFailures 记录图落盘的失败阶段（"marshal" / "write"），不携带原始
	// 错误文本。
	//
	// **为什么不复用 CleanupFailures**：它的落盘白名单只放行 agent/sandbox/scenario
	// （见 store/results.go 的 sanitizeCleanupFailures），别的值会被**静默丢弃**——
	// 那样「账记了但看不见」，等于没记。
	//
	// 为什么图落盘失败只记账、不算本题失败：图是研究辅助面，把它算成失败会把
	// 「模型解出来了」在公开指标里降级成「跑坏了」，而 Reason 的语义是「为什么停」
	// （见 Reason* 与 BranchesAbandoned 的注释）。但「没写出去」也绝不能读成
	// 「写了」，所以失败必须出现在公开结果里。
	GraphSaveFailures []string
	// StartedAt / EndedAt 用于报告。
	StartedAt time.Time
	EndedAt   time.Time
}

// Duration 返回耗时。任一时间戳为零时返回 0。
func (o OutcomeView) Duration() time.Duration {
	if o.StartedAt.IsZero() || o.EndedAt.IsZero() {
		return 0
	}
	return o.EndedAt.Sub(o.StartedAt)
}

// ── 端口入参 ──

// PlannerInput 是 Planner.Next 的入参。
type PlannerInput struct {
	Challenge Challenge
	Outcome   OutcomeView
	// Round 是即将开始的轮号（1 起）。
	Round int
}

// ── 体检 ──

// DoctorReport 是 Engine.Doctor 的结果。任何 Fatal 检查失败 ⇒ 退出码非 0。
type DoctorReport struct {
	Checks []DoctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// DoctorCheck 是一项体检结果。
//
// Detail 里**不得出现凭据明文**——CLI 只报告「是否设置」，不报告值。
type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	// Fatal 为真表示这一项失败必须阻止任何平台写操作。
	Fatal bool `json:"fatal"`
}

// ── 执行器 ──

// ExecSpec 是 ExecutorSpec 的**已解析**形式：RunSpec 里是用户写的配置，
// ExecSpec 是引擎加上运行身份与网络边界之后交给执行器的东西。
type ExecSpec struct {
	RunID  RunID
	Target Target
	// Executor 是用户写的原始配置。
	Executor ExecutorSpec
	// Network 是本次运行独占的 bridge 网络名。
	Network string
	// Workdir 是唯一允许挂进容器的宿主目录。
	//
	// **运行状态、token 和私有证据绝不挂载进容器**——它们是宿主的资产，
	// 容器里的 agent 是被约束方，不是被信任方。
	Workdir string
}

// ExecHandle 是容器起来之后执行器交回引擎的句柄。
type ExecHandle struct {
	ContainerID string
	Network     string
	// Addrs 是**容器内视角**可达的目标地址。
	Addrs []string
}

// ExecOptions 是一次命令执行的参数。
type ExecOptions struct {
	Env     map[string]string
	Workdir string
	Timeout time.Duration
	Stdin   io.Reader
}

// ExecResult 是一次命令执行的结果。
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}
