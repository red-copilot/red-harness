package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ResearchVersion is the intentionally incompatible v0.4 research SDK line.
// Version remains 0.3.0 for readers of the legacy graph schema.
const ResearchVersion = "0.4.0-research"

// defaultSandboxWorkdir 是容器内工作目录的缺省值，与 executor 的 defaultWorkdir
// 和 runner 镜像里的 /work 一致。它必须由 SandboxSpec 或 Sandbox 实现决定，不能
// 从 ExecutorSpec.Workdir（宿主路径）推导——见 runChallenge 里的说明。
const defaultSandboxWorkdir = "/work"

// SandboxSpec is the resolved, per-target sandbox configuration.
//
// Credentials must not be placed in Env: provider credentials are injected by
// the trusted host-side adapter into the sandbox process environment. Env is
// only for non-secret settings that the sandbox needs at creation time.
//
// **AllowHosts is deliberately absent.** The target allowlist is derived by the
// harness from Scenario.Prepare's Target.Addrs — the authorized platform is the
// only source of reachable endpoints. A caller-supplied list would be a way to
// widen scope, so the field does not exist to be filled in.
//
// **ReadOnly is deliberately absent.** A writable rootfs is not a configurable
// option of a research sandbox; the 307 GB incident is the reason. The sandbox
// implementation always renders --read-only.
type SandboxSpec struct {
	RunID      RunID
	Target     Target
	Image      string
	Workdir    string
	CPUs       float64
	MemoryMB   int
	PidsLimit  int
	ProfileDir string
	Env        map[string]string
}

// ProcessSpec describes the one attached process allowed in a session. The
// process is expected to become PID 1 in the sandbox; callers must not use a
// host-side exec fallback.
type ProcessSpec struct {
	Command []string
	Env     map[string]string
	Workdir string
}

// ProbeResult is deliberately small and safe to print in doctor output.
//
// PiVersion 是**镜像内**的 pi 版本，由 Probe 在同一镜像里核验。为什么必须在这里
// 而不是靠宿主侧的 checkVersion：v0.4 禁止宿主 exec pi，宿主上那份 pi 与容器里
// 那份可能完全是两个版本——「runner 里的 pi 是哪个版本」是排查 agent 行为差异的
// 第一手信息（前身被 0.74.2 静默烧题库咬过）。空串表示未能核验，调用方必须把
// 它读成「未核验」而不是「版本未知但大概没问题」。
type ProbeResult struct {
	ContainerID string
	PID         int
	Image       string
	Workdir     string
	PiVersion   string
}

// ManagedProcess is the attached stdio of the sandbox's main process.
// Kill is idempotent and must terminate the whole process tree by stopping the
// owning sandbox, rather than killing a host-side wrapper only.
type ManagedProcess interface {
	io.ReadWriteCloser
	Wait() error
	Kill() error
}

// SandboxSession owns one isolated target session. Close is idempotent and is
// required on every path, including context cancellation and agent failure.
type SandboxSession interface {
	Probe(context.Context) (ProbeResult, error)
	Launch(context.Context, ProcessSpec) (ManagedProcess, error)
	Close(context.Context) error
}

// Sandbox is the v0.4 execution port.
type Sandbox interface {
	NewSession(context.Context, SandboxSpec) (SandboxSession, error)
	// Reclaim removes the labelled leftovers of one specific run.
	Reclaim(context.Context, RunID) error
	// ReclaimStale removes every labelled container/network/rule whose run is
	// **not** in live, returning the full report: what was deleted, **and what was
	// found but deliberately kept**.
	//
	// 为什么不能只用 Reclaim(新 runID)：新 run 的 ID 是刚生成的，宿主上不可能
	// 有它的遗留——那次调用是空转，而**上次崩溃留下的**容器与网络会一直攒着。
	// 每个孤儿 bridge 占一个网段，攒够之后新 run 连网络都建不出来（表现为
	// 「起题失败」，而真因在几天前的崩溃现场）。所以启动前必须按 label 扫一遍，
	// 只保留 live 里的。
	//
	// ⚠️ **报告必须上抛，Pending 不得再被丢弃**（N0.2 的中间态到此结束）：
	// 「发现了但不该删」是一个判定结果，没有通道的判定等于没做——收错对象与
	// 漏收对象在旧签名（只返回 `[]RunID`）下都会表现为「什么都没发生」。
	// 调用方负责决定明细的去处：公开面只放计数（见 OutcomeView / RunResult），
	// 明细（哪个 runID、哪种资源、为什么没删）是本机资源拓扑，只进私密面或日志。
	//
	// 实现方在**拒绝执行**（例如 owner 不可判定）时返回零值报告加错误：那一刻
	// 连「哪些资源像我的」都还没看，凭空给出一份 Pending 会让调用方以为这是一次
	// 已经做完的判定。
	ReclaimStale(ctx context.Context, live map[RunID]bool) (ReclaimReport, error)
}

// SolverProfile is immutable run input. The bundle is mounted read-only by a
// sandbox implementation; Digest is stable and safe to put in public results.
//
// 一次 Run 里它被冻结（Run 开始时算一次 Digest），摘要同时驱动挂载、prompt 与
// 结果分组——三者用同一份来源，否则「同一 profile」在报告里会变成两个东西。
type SolverProfile struct {
	Name string `json:"name,omitempty"`
	// SystemPrompt 走 pi 的 --append-system-prompt（保留 pi 默认编码能力）。
	// 它由 Run 在每道题 Start 时注入 agent——放在 profile 里而不是 AgentSpec 里，
	// 是因为它属于「一次 Run 冻结的解法配置」，而 AgentSpec 描述的是进程参数。
	SystemPrompt    string `json:"systemPrompt,omitempty"`
	ExtensionBundle string `json:"extensionBundle,omitempty"`
	// Planner / PromptPolicy 在 v0.5 之前是 `map[string]any`。
	//
	// 换成显式结构体的理由有两条，都是「不可见」的那一类：
	//
	//  1. 键名靠约定。拼错一个字母不会报错——消费点取不到键，就静默回落到默认
	//     值，于是「我明明配了 5 轮」和「配置没生效」在报告里长得一模一样。
	//  2. 那个拼错的键**仍然进了 ProfileDigest**（摘要是对整个 map 取的）。于是
	//     改一个拼错的键，在报告里表现为「换了一次实验分组」——两次本该可比的
	//     运行被拆开，而没人会想到去查一个拼写错误。
	//
	// 换成结构体之后，Go 调用方写错键名**编译不过**；文本进来的配置由
	// LoadProfile 的 DisallowUnknownFields 挡下。
	// 用 omitzero 而不是 omitempty：后者对结构体字段**没有效果**（`{}` 照样
	// 序列化出来），于是「没配」会在 JSON 里长成 `"planner":{}`。omitzero 让
	// 零值真的消失，`RunSpec.Digest()` 对一份没用这两个键的配置保持旧值——
	// 少一批无谓的摘要漂移。
	Planner      PlannerConfig `json:"planner,omitzero"`
	PromptPolicy PromptConfig  `json:"promptPolicy,omitzero"`
}

// PlannerConfig 是 SolverProfile.Planner 的 schema。
type PlannerConfig struct {
	// DryRoundsBeforeHint 是连续无进展多少轮后允许请求提示。
	//
	// 0 表示「没配」，由 Run 依次回落到 PolicySpec.DryRoundsBeforeHint 与内置的
	// 2 轮。**0 不是「0 轮就提示」**——见 MinProfileLimit 的注释。
	DryRoundsBeforeHint int `json:"dryRoundsBeforeHint,omitempty"`
}

// PromptConfig 是 SolverProfile.PromptPolicy 的 schema。
//
// ⚠️ MaxFacts 与 MaxNegative 各自管**不同的渲染段**（已知事实段与死胡同段，
// 见 dag/render.go 的 factsSection / negativeSection），两段各有各的回落默认值
// （24 / 12）。所以两者之间**没有**「谁不能大于谁」这类跨字段约束——为了看起来
// 更严而加一条不成立的约束，会让合法配置被拒，而拒绝理由还是编的。
type PromptConfig struct {
	MaxFacts    int `json:"maxFacts,omitempty"`
	MaxNegative int `json:"maxNegative,omitempty"`
}

// profile 数值项的允许区间。
//
// 上界 10000 不是随手取的：这些值直接决定**渲染进 prompt 的条目数**，一个
// 「999999 条事实」的配置会在跑起来之后把 prompt 撑爆，而那时预算已经花了。
// 下界 1 而不是 0：0 在这套 schema 里表示「没配，用默认」，不是「取 0 条」。
const (
	MinProfileLimit = 1
	MaxProfileLimit = 10000
)

// DefaultDryRoundsBeforeHint 是「连续无进展多少轮后允许请求提示」的内置默认值。
//
// 它此前是轮循环里的一个字面量 2，而 v0.3 的 engine.go 注释里写的是 3——两处
// 说法不一致，且没有任何地方说明哪个是生效的。v0.4 实际跑的是 2，所以取 2 并
// 给它一个名字：改它的人要能看见「这是默认值」，而不是在轮循环里改一个数字。
const DefaultDryRoundsBeforeHint = 2

// Validate 检查 profile 每一项的取值。
//
// 调用点在**任何副作用之前**（Run 的开头，早于跨进程锁）：配置错误是纯粹的
// 调用方错误，没有任何理由等起完容器、起完题、花掉平台额度之后再告诉他。
func (p SolverProfile) Validate() error {
	check := func(field string, v int) error {
		if v == 0 {
			return nil // 0 = 没配，交给回落链
		}
		if v < MinProfileLimit || v > MaxProfileLimit {
			return Ef(KindConfig, "harness.profile",
				fmt.Sprintf("%s=%d 超出允许区间 [%d, %d]", field, v, MinProfileLimit, MaxProfileLimit), nil)
		}
		return nil
	}
	if err := check("planner.dryRoundsBeforeHint", p.Planner.DryRoundsBeforeHint); err != nil {
		return err
	}
	if err := check("promptPolicy.maxFacts", p.PromptPolicy.MaxFacts); err != nil {
		return err
	}
	return check("promptPolicy.maxNegative", p.PromptPolicy.MaxNegative)
}

// LoadProfile 从 JSON 读一份 profile。
//
// 为什么需要它，而不是让调用方自己 json.Unmarshal：换成结构体只挡住了 Go 调用
// 方的键名拼写，**文本**进来的配置照样能带未知键，而 encoding/json 的默认行为是
// 静默忽略。那正是这次要消灭的形状。所以唯一的文本入口在这里关掉那个默认，
// 并顺手要求「整个 reader 就是一份 profile」——只读第一个 JSON 值会让一个拼接
// 文件的前半段静默生效。
func LoadProfile(r io.Reader) (SolverProfile, error) {
	var p SolverProfile
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return SolverProfile{}, Ef(KindConfig, "harness.profile", "profile 解析失败（未知键或类型不符）", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return SolverProfile{}, Ef(KindConfig, "harness.profile", "profile 之后还有多余内容", nil)
	}
	if err := p.Validate(); err != nil {
		return SolverProfile{}, err
	}
	return p, nil
}

func (p SolverProfile) Digest() string {
	// Reuse the same canonical digest mechanism as RunSpec without exposing a
	// second hashing policy in the public API.
	b := struct {
		Name, SystemPrompt, ExtensionBundle string
		Planner                             PlannerConfig
		PromptPolicy                        PromptConfig
	}{p.Name, p.SystemPrompt, p.ExtensionBundle, p.Planner, p.PromptPolicy}
	return digestJSON(b)
}

// Empty 报告这个 profile 是否「一个字段都没填」。
//
// 它是「该不该回落到默认 profile」的唯一判据，装配层（internal/wire.resolve）
// 必须调它，不能自己再抄一份字段列表：两处一旦漂移，Run 实际生效的 profile
// 与 RunSpec 里那份就不是同一个东西，而摘要看起来仍然正常。
func (p SolverProfile) Empty() bool {
	// 结构体可以直接比较（字段全是可比较类型）。两处零值判据与 Validate 的
	// 「0 = 没配」是同一个约定，改字段时三处要一起看。
	return p.Name == "" && p.SystemPrompt == "" && p.ExtensionBundle == "" &&
		p.Planner == PlannerConfig{} && p.PromptPolicy == PromptConfig{}
}

// ResultStore is intentionally separate from the event/snapshot Store. It
// stores aggregate metrics and fingerprints, never candidate plaintext.
type ResultStore interface {
	Save(context.Context, RunResult) error
	Get(context.Context, RunID) (RunResult, error)
	List(context.Context) ([]RunResult, error)
	Stats(context.Context, StatsQuery) (StatsReport, error)
}

// TraceStore is an optional private event sink. Implementations must keep raw
// events outside public result files and restrict directory/file permissions.
type TraceStore interface {
	AppendTrace(context.Context, RunID, string, Event) error
}

// CandidateAudit 是一条**私密**的候选审计记录：这次提交了什么，平台怎么判的。
//
// 为什么必须有它，而不是靠 trace 反推：trace 只接 agent 事件（`sink.on` 的输入
// 全是 agent Emit），而 `Evaluate` / `Mark` 的结果**从不经过**它。于是「提交了
// 哪一条、平台怎么判的」在落盘面上不可回答——公开结果只有聚合计数，判定只在
// 内存里活到本题结束。一次真跑里「147 次提交、146 条判错」这种事实，事后只能靠
// 数聚合计数去猜，而「猜」正是这份记录要消灭的东西。
//
// ⚠️ **这是私密面**：Flag 是明文，Message 是平台原话。实现方必须把它写进
// 0700/0600 的私密目录，绝不能进公开结果、CLI 输出或图。
//
// Fingerprint **不在这里**：它是 `answer.Fingerprint` 的产物，而根包不许 import
// 任何子包（`answer` 是子包）。所以由实现方（store 已 import answer）用那个唯一
// 实现算出来写进审计行——见 store/audit.go。
type CandidateAudit struct {
	// Source / Provenance / IntentID / Round 是这条候选的出处与推导链锚点。
	Source     string
	Provenance string
	IntentID   string
	Round      int
	// SubmittedAt 是**发起**这次提交的宿主时间。
	SubmittedAt time.Time
	// Verdict 是 Gate 派生出的判定（含 Duplicate 的派生）。
	Verdict SubmissionVerdict
	// Score 是平台给的本次得分。
	Score int
	// Message 是平台原样返回的说明，用于诊断。只进私密面。
	Message string
	// SubmitError 是平台侧判错/出错的信息。只进私密面。
	SubmitError string
	// Flag 是候选**明文**。它与结果一起构成「提交了什么」的完整答案。
	Flag string
}

// AuditStore is an optional private candidate-audit sink.
//
// 与 TraceStore 同形（可选端口，按类型断言取用），理由也一样：不接它不影响任何
// 一次运行的成败，但**不接就等于没有审计**——所以它必须能被单独看见。
type AuditStore interface {
	AppendAudit(ctx context.Context, runID RunID, challenge string, rec CandidateAudit) error
}

// RunLocker 是**跨进程**的单运行锁。
//
// 为什么必须有它，而进程内的 Mutex 不够：同一台宿主上两个 red-harness 进程
// 同时跑，会在平台侧互相踩（重复起题、重复提交、把同一道题的额度打光），
// 而且两边都按 run label 回收容器与网络时，会**互相删掉对方正在用的资源**——
// 后者比平台侧的问题更隐蔽：表现为「容器莫名其妙没了」。
//
// Lock 必须是**阻塞但可取消**的：拿不到锁时等待并在 ctx 到期时返回错误，
// 而不是立刻失败（否则并发调用方要靠重试轮询，那是更差的接口）。
type RunLocker interface {
	Lock(ctx context.Context) error
	// Unlock 释放锁。必须幂等——defer 路径与显式路径都会调它。
	Unlock() error
}

type StatsQuery struct {
	ProfileDigest string
	// BundleDigest 按**扩展包内容**摘要过滤。
	//
	// 为什么 ProfileDigest 之外还要它：ProfileDigest 是对 profile 规格取的，而
	// 其中的 ExtensionBundle 只是一个**路径字符串**。同一个路径下的内容换了，
	// ProfileDigest 不变——于是只按它分组，会把两份不同的扩展包算作同一次实验，
	// profile 对照实验的结论随之失真。要正确分组，两个维度都得用上。
	BundleDigest string
	Model        string
	Scenario     string
	Challenge    string
	Category     string
	Since        time.Time
	Until        time.Time
}

// StatsReport 是按维度聚合的公开指标。
//
// ⚠️ **口径（v0.4 变更）**：ConfirmedFlags 是**本次新增确认**数，不是最终累计
// 进度；RemainingAtStart 是起跑时剩余量之和。召回率 = ConfirmedFlags /
// RemainingAtStart，**分母为 0 表示有挑战的分母未知**（FlagCount 未知），此时
// 不得宣称召回率——两个计数器都会把该挑战排除在外。
type StatsReport struct {
	Runs                    int     `json:"runs"`
	Completed               int     `json:"completed"`
	CompletionRate          float64 `json:"completionRate"`
	Challenges              int     `json:"challenges"`
	SolvedChallenges        int     `json:"solvedChallenges"`
	ChallengeCompletionRate float64 `json:"challengeCompletionRate"`
	ConfirmedFlags          int     `json:"confirmedFlags"`
	RemainingAtStart        int     `json:"remainingAtStart"`
	RecallRate              float64 `json:"recallRate"`
	Score                   int     `json:"score"`
	CostUSD                 float64 `json:"costUSD"`
	DurationSeconds         float64 `json:"durationSeconds"`
	HintedRuns              int     `json:"hintedRuns"`
	ProviderFailures        int     `json:"providerFailures"`
}

// ChallengeResult is the return-only view for one target. A result store must
// persist only its Public metrics; Flags and Candidates stay in memory/private.
type ChallengeResult struct {
	Challenge Challenge
	Outcome   OutcomeView
	StartedAt time.Time
	EndedAt   time.Time
}

type RunResult struct {
	RunID         RunID
	Scenario      string
	ProfileDigest string
	// BundleDigest 是本次运行实际挂载的扩展包**内容**摘要。
	//
	// 与 ProfileDigest 并列、不合并：ProfileDigest 描述的是「解法配置是什么」
	// （含 bundle 的路径），BundleDigest 描述的是「那份配置指向的内容是什么」。
	// 两者合一会让「同一 profile 换了 bundle 内容」看起来像同一次实验。
	//
	// 空串表示**未核验**（没有配 bundle），语义与 ProbeResult.PiVersion 一致：
	// 调用方不得把它读成「内容没问题」。
	BundleDigest string
	Model        string
	StartedAt    time.Time
	EndedAt      time.Time
	Challenges   []ChallengeResult
	// State 是**运行怎么结束的**（finished / failed / cancelled）。
	// 它与 Completed 回答的是两个不同的问题，见 RunFinished 的注释。
	State RunState
	// Completed 是**解出来了没有**（有题目达成平台权威的目标）。
	//
	// ⚠️ 不要把「运行没报错」当成它：正常跑完但一道题没解出来是
	// ReasonNoProgress，Completed 为假——那正是前身「280 run / 0 flag」的形状。
	Completed bool
	Reason    string
	Err       string
	// Manifest 是本次运行**实际生效的配置与产物身份**。
	Manifest RunManifest
	// ReclaimedStale / PendingStale 是**启动前那次**遗留资源回收的两个计数。
	//
	// 只放计数：`Pending` 的明细（哪个 runID、哪种资源、为什么没删）是本机资源
	// 拓扑，进了这份文件就等于把它拷进了会被转发、归档、贴工单的结果里。明细的
	// 去处由 `Sandbox.ReclaimStale` 的调用方决定（私密面或日志）。
	//
	// 为什么计数必须上公开面：`Pending` 非空意味着宿主上躺着**归属无法证明**的
	// 容器与网络——旧签名把这一整类判定丢掉了，于是「回收跑过了、什么都没删」
	// 与「宿主上本来就没有孤儿」在结果里完全同形。
	ReclaimedStale int
	PendingStale   int
}

// RunManifest 是冻结下来的运行清单。
//
// 为什么需要它：RunResult 此前只有 16 个十六进制字符的 ProfileDigest，而摘要
// **不可逆**——它能证明「两次运行不一样」，却无法回答「差在哪」。于是
// 「这次跑的是 2 轮还是 5 轮停滞阈值」「用的是哪个镜像」这类问题在落盘之后
// 一律无解，而它们恰恰是复现一次实验所必需的。清单把「请求值」与「生效值」
// 分开记账：两者不同才是常态（回落链、tag 漂移）。
//
// ⚠️ **这是公开产物**：每一个字段都必须过 store 的白名单闸（见
// store/results.go 的 publicManifest）。不许放路径、明文或凭据。
type RunManifest struct {
	// PlannerDryRounds 是**回落链的终点**：本次运行实际生效的停滞阈值。
	// 与 SolverProfile.Planner.DryRoundsBeforeHint 的区别正是「请求值 vs 生效值」。
	PlannerDryRounds int
	// PromptMaxFacts / PromptMaxNegative 是配置值。
	//
	// 0 表示「由渲染层的默认值决定」（dag.DefaultMaxFacts / DefaultMaxNegative）
	// ——那两份默认值在 dag 包里，根包看不到，所以这里**不折算**，如实记 0。
	PromptMaxFacts    int
	PromptMaxNegative int
	// MaxSubmissions 是**实际生效**的每题提交次数上限（回落链的终点）。
	MaxSubmissions int
	// HintPolicy 是实际生效的提示策略（空串已折成 HintAuto）。
	HintPolicy string
	// RequestedImage 是配置里请求的镜像，通常是一个 tag——**tag 会漂移**。
	// 空串表示「由装配层/执行器的默认值决定」。
	RequestedImage string
	// Image 是 Probe 解析出的镜像 ID（`sha256:…`）。
	//
	// 与 RequestedImage 并列而不是取代它：「tag 没变但内容变了」只有把两者
	// 放在一起才看得出来，而那正是「换了次实验却以为是同一次」的成因。
	// 空串表示**未核验**（例如 Fake sandbox 不解析镜像 ID），语义与
	// ProbeResult.PiVersion 一致：不得读成「没问题」。
	Image string
	// PiVersion 是镜像内实测的 pi 版本。空串表示**未核验**。
	PiVersion string
	// ImageMismatch 为真表示同一 run 内 Probe 报了不同的镜像 ID。
	//
	// 既不静默覆盖（那会让「这次用的哪个镜像」变成**最后一道题**的答案），
	// 也不当成运行失败（镜像一致性是 executor 层的性质，由它自己拒绝启动），
	// 只记账——与 GraphSaveFailures 同一档处置。
	ImageMismatch bool
}

// resolveManifest 冻结本次运行实际生效的配置。
//
// 它必须在任何题目起跑**之前**跑，且结果只算一次：清单要在事后回答「当时生效
// 的是什么」，而不是「跑到一半被人改成了什么」。
func resolveManifest(spec RunSpec) RunManifest {
	m := RunManifest{
		PromptMaxFacts:    spec.Profile.PromptPolicy.MaxFacts,
		PromptMaxNegative: spec.Profile.PromptPolicy.MaxNegative,
		HintPolicy:        spec.HintPolicy,
		RequestedImage:    spec.Sandbox.Image,
	}
	// 与轮循环用的回落链**是同一段逻辑**：清单不另算一遍，否则两处一旦漂移，
	// 清单记的就不是真正生效的那个值——而清单的全部意义就在于它记的是生效值。
	m.PlannerDryRounds = resolveDryRounds(spec)
	m.MaxSubmissions = resolveSubmitCap(spec)
	if m.HintPolicy == "" {
		m.HintPolicy = HintAuto
	}
	if m.RequestedImage == "" {
		// 与 runChallenge 里 sb.Image 的回落同源。
		m.RequestedImage = spec.Executor.Image
	}
	return m
}

// resolveSubmitCap 是每题提交次数上限的回落链。
//
// 与 resolveDryRounds 同形、同理由：清单与轮循环必须用**同一份**判定，
// 否则清单记一个值、实际生效另一个值，而两者看起来都正常。
func resolveSubmitCap(spec RunSpec) int {
	if n := spec.Policy.MaxSubmissionsPerChallenge; n > 0 {
		return n
	}
	return DefaultMaxSubmissionsPerChallenge
}

// resolveDryRounds 是「连续无进展多少轮后允许请求提示」的回落链。
//
// 单独一个函数是为了让清单与轮循环用**同一份**判定：两处各写一遍的话，
// 清单会记一个值、实际生效另一个值，而两者都看起来正常。
func resolveDryRounds(spec RunSpec) int {
	if n := spec.Policy.DryRoundsBeforeHint; n > 0 {
		return n
	}
	if n := spec.Profile.Planner.DryRoundsBeforeHint; n > 0 {
		return n
	}
	return DefaultDryRoundsBeforeHint
}

// observeProbe 把一次 Probe 的结果记进清单。
//
// 第一次成功 Probe 冻结镜像身份，后续题只做一致性核对。
func (m *RunManifest) observeProbe(p ProbeResult) {
	if m.Image == "" {
		m.Image, m.PiVersion = p.Image, p.PiVersion
		return
	}
	if p.Image != "" && p.Image != m.Image {
		m.ImageMismatch = true
	}
}

// Harness is the synchronous v0.4 façade. It intentionally contains no pause,
// resume, control socket or web lifecycle.
type Harness struct {
	scenario Scenario
	sandbox  Sandbox
	agents   AgentFactory
	results  ResultStore
	locker   RunLocker
	graphs   GraphSaver
	// audits 是**可选**的私密候选审计落点，从 Results 上取（两者是同一棵树上的
	// 两个目录，由同一个实现提供）。nil 表示不接审计。
	audits            AuditStore
	gate              func(Challenge) CandidateGate
	solverWithProfile func(Challenge, SolverProfile) (Planner, Renderer)
	solver            func(Challenge) (Planner, Renderer)
	planner           func(Challenge) Planner
	renderer          func(Challenge) Renderer
	profile           SolverProfile
	now               func() time.Time
	// mu guards running only. It is **not** the cross-process lock: see RunLocker.
	mu      sync.Mutex
	running bool
}

type HarnessOptions struct {
	Scenario Scenario
	Sandbox  Sandbox
	Agents   AgentFactory
	Results  ResultStore
	// Locker is required for all production runs, including direct SDK use.
	Locker RunLocker
	// Graphs 为 nil 表示不落盘 DAG。它是**可选**端口：图是研究辅助面，
	// 直接调 SDK 的调用方可能根本不关心（见 GraphSaver 的注释）。
	// 缺了它不会静默——Doctor 会报出来，装配层恒提供。
	Graphs            GraphSaver
	Gate              func(Challenge) CandidateGate
	SolverWithProfile func(Challenge, SolverProfile) (Planner, Renderer)
	// Solver creates a Planner and Renderer over the same per-challenge state.
	// If set, it takes precedence over the separate legacy factories.
	Solver   func(Challenge) (Planner, Renderer)
	Planner  func(Challenge) Planner
	Renderer func(Challenge) Renderer
	Profile  SolverProfile
	Now      func() time.Time
}

func NewHarness(opts HarnessOptions) (*Harness, error) {
	var missing []string
	if opts.Scenario == nil {
		missing = append(missing, "Scenario")
	}
	if opts.Sandbox == nil {
		missing = append(missing, "Sandbox")
	}
	if opts.Agents == nil {
		missing = append(missing, "Agents")
	}
	// Planner / Renderer / Gate / Results 同样是**生产必需**端口，缺了它们这台
	// Harness 会以「静默错误」的方式失败：
	//   - 缺 Planner/Renderer/Gate：每道题都会走「未执行任何轮次」那条路，报告
	//     把一次装配错误读成「模型不行」；
	//   - 缺 Results：Run 里的 `if h.results != nil` 会让「跑完不落盘」变成一次
	//     静默成功，表现为「跑了几十次，stats 说零次」。
	// 两者都是「看起来跑通了、其实什么都没验证」的形状，必须在启动时拒绝。
	if opts.SolverWithProfile == nil && opts.Solver == nil && opts.Planner == nil {
		missing = append(missing, "Planner")
	}
	if opts.SolverWithProfile == nil && opts.Solver == nil && opts.Renderer == nil {
		missing = append(missing, "Renderer")
	}
	if opts.Gate == nil {
		missing = append(missing, "Gate")
	}
	if opts.Results == nil {
		missing = append(missing, "Results")
	}
	if opts.Locker == nil {
		missing = append(missing, "Locker")
	}
	// Profile 是装配层的**默认** profile（RunSpec 里那份非空时它不生效）。它配错
	// 了必须在构造时炸，而不是等某次运行跑到轮循环里才发现——那时容器已经起了、
	// 预算已经开始花了。
	if err := opts.Profile.Validate(); err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, Ef(KindConfig, "harness.new", "缺少必需端口: "+strings.Join(missing, ", "), nil)
	}
	// 审计端口是**可选**的，所以不进 missing 列表：不接它不影响任何一次运行的
	// 成败。但接了就要被看见（Doctor 会打印一行），而且它是从 Results 上取的
	// ——公开结果与私密审计是同一棵树上的两个目录。
	audits, _ := opts.Results.(AuditStore)
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Harness{scenario: opts.Scenario, sandbox: opts.Sandbox, agents: opts.Agents,
		results: opts.Results, locker: opts.Locker, graphs: opts.Graphs, audits: audits, gate: opts.Gate, solverWithProfile: opts.SolverWithProfile, solver: opts.Solver, planner: opts.Planner,
		renderer: opts.Renderer, profile: opts.Profile, now: now}, nil
}

// graphSaverDetail 给体检写一句人话，明确「图会不会落盘」。
//
// 写成两句而不是只在失败时报错：Doctor 的输出是给人判断「这次跑出来的东西里
// 有没有图可看」的，缺席时要能一眼看出是「没接」而不是「接了没写出来」。
// ⚠️ 这是**装配在位**的断言，不是「本次真的写出了图」的证明。
//
// 两者必须分开说：`dagGraphSaver` 在题目没登记过图时返回 `GraphAbsent`
// （那不是失败，是「这一题没有图」），所以「已接入」永远不蕴含「文件在盘上」。把装配
// 当作产物来报，会让「图没写出来」变成一次看不见的缺失——而 open items 那套
// 判据正是拿「有没有人看得见」当标准的。
func graphSaverDetail(h *Harness) string {
	if h == nil || h.graphs == nil {
		return "未接入：本次运行不会落盘 DAG（<StoreDir>/runs/<runID>/graph.json 不会出现）"
	}
	return "已接入（仅表示端口在位）：每题终态尝试落盘 DAG；是否真的写出见公开结果的 graphSaveFailures"
}

// auditDetail 报告候选审计是否接线。
//
// ⚠️ 这是**装配在位**的断言（只看端口非 nil），不是「本次真的写出了审计行」的
// 证明——与 graph_saver 同一档。文案必须把这两件事分开，否则读者会把它当成后者。
func auditDetail(h *Harness) string {
	if h == nil || h.audits == nil {
		return "未接入：私密面不会出现候选审计（<ResultDir>/private/<runID>/submissions.jsonl 不会出现）" +
			"；公开面仍有提交/判错计数，但「提交了哪一条、平台怎么判的」将不可回答"
	}
	return "已接入：每次提交落一行私密审计（写明是否真的写出仍需看该文件）"
}

// graphSaveStage 把图落盘的失败折成公开面允许的枚举值。
//
// 为什么要这一步：公开结果里**不能**出现原始错误文本（可能带路径、目标地址、
// 平台响应片段）。原始错误仍在 err 链上（它不参与序列化），供调用方查日志。
// 三档对应三种不同的处境，见 ErrGraph* 的注释。第四档 `"unknown"` 是给
// **不是本包产生的错误**留的：`GraphSaver` 是一个公开端口，任何实现都能注入，
// 而一个自定义实现的失败原因不会是这三个哨兵之一。
//
// 为什么必须有它：原先的 `default` 把**任何**未知错误都标成 `"marshal"`——
// 那是在断言一件我们并不知道的事（「图没序列化出来」），而读报告的人会照着它
// 去查序列化。一个编出来的原因比「原因未知」有害得多。
func graphSaveStage(err error) string {
	switch {
	case errors.Is(err, ErrGraphExport):
		return "export"
	case errors.Is(err, ErrGraphWrite):
		return "write"
	case errors.Is(err, ErrGraphMarshal):
		return "marshal"
	default:
		return graphStageUnknown
	}
}

// graphStageUnknown 是阶段枚举里「不是本包的三种失败」的那一格。
//
// 它被两处引用：graphSaveStage 的 default，以及调用点给「实现方报了 GraphFailed
// 却没带 err」补的那一格。写成常量而不是两处字面量，是因为它是**公开面**上的值
// （进 GraphSaveFailures，随结果落盘），两处写不一致就会让同一个含义出现两个值。
const graphStageUnknown = "unknown"

// foldGraphState 把「图落盘走到哪一步」折成 GraphState 的四态之一。
//
// 签名切到四态直返之后（N0），它只剩两件事——**不再需要从 nil error 里猜
// 「有没有文件产生」**，那一格由实现方如实回答（见 ports.go 的 GraphSaver）：
//
//	端口不在位（saver == nil）  ⇒ GraphDisabled
//	有失败阶段（failures 非空）  ⇒ GraphFailed，卡在哪由阶段枚举说明
//	其余                        ⇒ **照实现方返回的值**
//
// 第一档是**装配事实**，所以只能由这里折算，实现方明令不得返回它（ports.go）：
// 「端口在位但这一题不需要图」与「压根没有端口」合流之后，后者——配置错误里
// 最需要被看见的那一档——就消失了。同理，实现方若返回 `disabled`，那是它违反了
// 端口契约而不是它有权做的判定：端口在位，所以它的答案只能是某种「没有文件」，
// 折成 GraphAbsent。
//
// 第二档压过第三档，是为了保住不变式（ports.go）：
// `GraphState == GraphFailed` ⟺ `len(failures) > 0`。**方向只能是这一边**——
// 以阶段为准反推状态，而不是以状态为准伪造一个阶段：前者的最坏结果是多报一次
// 失败（读的人去目录里看一眼就知道真相），后者是让公开面声称一件我们并不知道
// 的事（「图没序列化出来」），而那正是 graphSaveStage 的注释里记着的旧事故。
func foldGraphState(saver GraphSaver, state GraphState, failures []string) GraphState {
	switch {
	case saver == nil:
		return GraphDisabled
	case len(failures) > 0:
		return GraphFailed
	case state == GraphSaved || state == GraphAbsent:
		return state
	default:
		// 四态之外的值（含空串）或实现方不该返回的 disabled：端口在位 ⇒ 这一格
		// 只能是「没有文件产生」。宁可少报一档（读的人去目录里看一眼就会发现
		// 文件其实在），也不要为了好看折成 saved——那一栏会从此永久说谎。
		return GraphAbsent
	}
}

func (h *Harness) Doctor(ctx context.Context) DoctorReport {
	r := DoctorReport{OK: true}
	checks := []DoctorCheck{{Name: "scenario", OK: h != nil && h.scenario != nil, Fatal: true},
		{Name: "sandbox", OK: h != nil && h.sandbox != nil, Fatal: true},
		{Name: "agent_factory", OK: h != nil && h.agents != nil, Fatal: true},
		{Name: "planner", OK: h != nil && (h.planner != nil || h.solver != nil || h.solverWithProfile != nil), Fatal: true},
		{Name: "renderer", OK: h != nil && (h.renderer != nil || h.solver != nil || h.solverWithProfile != nil), Fatal: true},
		{Name: "gate", OK: h != nil && h.gate != nil, Fatal: true}}
	checks = append(checks, DoctorCheck{Name: "locker", OK: h != nil && h.locker != nil, Fatal: true})
	// 图落盘是**可选**端口，所以这条**不是** Fatal：不接它不影响任何一次运行的
	// 成败。但它必须出现在体检里——否则「这道题为什么没有图」会变成一次静默的
	// 缺失，而 open items 那套判据正是拿「有没有人看得见」当标准的。
	checks = append(checks, DoctorCheck{Name: "graph_saver", OK: h != nil && h.graphs != nil, Fatal: false,
		Detail: graphSaverDetail(h)})
	// 候选审计同样是可选端口，同样必须被看得见：不接它时「这次运行没有审计」
	// 与「这次运行没提交过任何候选」在公开面上长得一模一样，而这两件事指向
	// 完全不同的结论。判据取 `h.results` 是否实现了 AuditStore ——审计落点
	// 与公开结果是同一棵树上的两个目录，由同一个实现提供。
	checks = append(checks, DoctorCheck{Name: "candidate_audit", OK: h != nil && h.audits != nil, Fatal: false,
		Detail: auditDetail(h)})
	if h != nil && h.sandbox != nil {
		// Reclaim is deliberately not called here: doctor must not mutate runtime
		// state. Presence checks are enough at this layer.
		_ = ctx
	}
	for _, c := range checks {
		if c.Fatal && !c.OK {
			r.OK = false
		}
		r.Checks = append(r.Checks, c)
	}
	return r
}

func (h *Harness) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	if h == nil {
		return RunResult{}, Ef(KindConfig, "harness.run", "Harness 为空", nil)
	}
	// 配置校验在**一切副作用之前**，包括跨进程锁：`locker.Lock` 会落下锁文件
	// （位置由锁实现决定——装配层的默认实现在 `/run/lock/red-harness/` 下按
	// daemon 端点取文件名，**不在 StoreDir 里**；理由见 local/lock.go）。
	// 校验是纯函数，没有理由让一份写错的配置先落下副作用再被拒绝。
	//
	// State 显式置 RunFailed：State 的契约是「Run 返回了错误，State 就必是
	// failed 或 cancelled」——留空串会让调用方退回解析错误字符串，而 errors.go
	// 明令禁止那么做。
	if err := spec.Validate(); err != nil {
		return RunResult{State: RunFailed}, err
	}
	// `Submit=true` 却没有审计端口 ⇒ **在任何平台调用之前拒绝整次运行**。
	//
	// 为什么这一档不能像图落盘那样「缺席就缺席」：`Submit=true` 意味着一串
	// **不可追回的平台写操作**（起题、提交、关题），而没有审计就永远答不出
	// 「提交了什么、平台怎么判的」——2026-09-22 那次授权真跑里「147 次提交、
	// 146 条判错」的事后追查正是卡在这里，只能靠数聚合计数去猜。可选端口的缺席
	// 最多让产物少一面；它的缺席让**已经发生过的写操作**不可解释。
	//
	// 判据用构造函数冻结的 `h.audits`，不在这里重新断言一次类型：同一件事有两个
	// 判据就会有两套答案，而 Doctor 的 candidate_audit 一行读的也是它——「体检说
	// 没接审计」与「这样跑会被拒」必须是同一个事实。
	//
	// 位置：早于 Discover（Run 里的第一处平台调用），也早于跨进程锁。拒绝必须发生
	// 在副作用之前，否则「拒绝」本身已经踩过平台了。
	if spec.Submit && h.audits == nil {
		return RunResult{State: RunFailed}, Ef(KindConfig, "harness.audit",
			"Submit=true 但没有候选审计落点（Results 未实现 AuditStore）：提交记录将不可回答；"+
				"请接上审计，或改用 Submit=false 的干跑", nil)
	}
	if h.locker == nil {
		return RunResult{}, Ef(KindConfig, "harness.run", "缺少跨进程单运行锁", nil)
	}
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return RunResult{}, Ef(KindConfig, "harness.run", "已有运行进行中", nil)
	}
	h.running = true
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.running = false; h.mu.Unlock() }()

	// 跨进程单运行锁在**一切副作用之前**取。放到 Discover 之后等于已经起过题
	// 了再发现别人在跑，那时平台侧已经被踩过。
	if err := h.locker.Lock(ctx); err != nil {
		return RunResult{}, Ef(KindConfig, "harness.lock", "获取单运行锁失败", err)
	}
	defer func() { _ = h.locker.Unlock() }()

	profile := spec.Profile
	if profile.Empty() {
		profile = h.profile
	}
	// JSON round-trip copies nested maps. A caller mutating Profile during Run
	// must not change prompt/parameters after the digest has been recorded.
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		return RunResult{}, Ef(KindConfig, "harness.profile", "profile 无法序列化", err)
	}
	if err := json.Unmarshal(profileJSON, &spec.Profile); err != nil {
		return RunResult{}, Ef(KindConfig, "harness.profile", "profile 无法冻结", err)
	}

	started := h.now()
	runID := RunID(fmt.Sprintf("run-%d", started.UnixNano()))
	result := RunResult{RunID: runID, Scenario: spec.Scenario, ProfileDigest: spec.Profile.Digest(), Model: spec.Agent.Model, StartedAt: started}
	// 运行清单在起跑前冻结。`&result.Manifest` 会一路交给 runChallenge——它在那里
	// 记 Probe 解析出的镜像身份，所以清单是**跑到哪记到哪**的一份（其余字段在
	// 这里就定了）。
	result.Manifest = resolveManifest(spec)
	// 扩展包内容摘要在**任何副作用之前**算并冻结：它要描述的是本次运行实际
	// 挂载的那份 bundle，而不是跑到一半被人替换后的样子。算不出来就 fail closed
	// ——一份「摘要未知」的 profile 无法与别的运行分组比较，而分组错了会让整个
	// profile 对照实验的结论失效。
	digest, err := bundleDigest(spec.Profile.ExtensionBundle)
	if err != nil {
		result.State = RunFailed
		return result, Ef(KindConfig, "harness.profile", "扩展包内容摘要无法计算", err)
	}
	result.BundleDigest = digest

	// 先按 label 扫掉**上次崩溃**留下的容器/网络/规则，再建本题的资源。
	//
	// 为什么不是 Reclaim(ctx, runID)：runID 是上面刚生成的，宿主上不可能有它的
	// 遗留——那次调用是空转，而孤儿会一直攒着（每个 bridge 占一个网段，攒够
	// 之后新 run 连网络都建不出来）。live 集合里只有本次 run，所以本次自己的
	// 资源不会被误删。
	//
	// 下面这三处提前返回都要显式置 RunFailed：State 的契约是「Run 返回了错误，
	// State 就必是 failed 或 cancelled」——留空串会让调用方退回解析错误字符串，
	// 而 errors.go 明令禁止那么做。这几条路径都发生在**任何题目起跑之前**，
	// 所以是 failed 而不是 cancelled（用户没按 Ctrl-C）。
	// 报告**先折进公开结果再判错误**：回收失败时实现方返回的是「已经删掉的那批」
	// 加错误（它不回滚已完成的删除），所以即使这次调用报错，那两个计数仍然描述了
	// 宿主上真实发生的事情——把它们丢掉会让「删了一半就失败」在结果里变成
	// 「什么都没发生」。
	rep, reclaimErr := h.reclaimStale(ctx, runID)
	result.ReclaimedStale = rep.ReclaimedTotal()
	result.PendingStale = rep.PendingTotal()
	if reclaimErr != nil {
		result.State = RunFailed
		return result, reclaimErr
	}
	challenges, err := h.scenario.Discover(ctx, spec)
	if err != nil {
		result.State = RunFailed
		return result, err
	}
	// firstErr 保存**原始错误**（不是折叠后的分类串）。
	//
	// 为什么不能只留 result.Err：它是 `%T` 折出的分类串，专门给公开结果用的
	// （不得带平台响应体、路径或凭据）。但调用方拿到 Run 的返回值时要能
	// `IsKind(err, KindConfig)` 做分支——把分类在返回路径上丢掉，调用方就只能
	// 去解析字符串，而 errors.go 明令禁止那么做。
	var firstErr error
	for _, ch := range challenges {
		// ctx 取消优先于一切：一次 Ctrl-C 之后继续把剩下的题跑完，会让用户
		// 以为取消没生效，而平台侧已经在起题了。
		if err := ctx.Err(); err != nil {
			firstErr = Ef(KindCancelled, "harness.run", "运行被取消", err)
			result.Err = safeError(firstErr)
			break
		}
		if len(spec.Targets) > 0 && !contains(spec.Targets, ch.Code) {
			continue
		}
		cr, runErr := h.runChallenge(ctx, runID, spec, ch, &result.Manifest)
		result.Challenges = append(result.Challenges, cr)
		if runErr != nil {
			if firstErr == nil {
				firstErr = runErr
			} else if IsKind(runErr, KindPersistence) {
				// 落盘故障是这次运行**为什么停**的那个错，必须排在链首：result.Err
				// 取的是链上第一个分类，被前面某道题的题级错误挡在前面，它就会在
				// 公开面上消失——而「看不见」正是这一类故障的常态（见 KindPersistence
				// 的注释：吞掉意味着调用方以为进展保住了）。
				firstErr = errors.Join(runErr, firstErr)
			}
			result.Err = safeError(firstErr)
		}
		// ⚠️ **只有落盘故障中断整次运行**，其余错误一律只结束本题（既有语义，
		// 见轮循环里「再次失败则结束当前题目并继续下一题」那条注释）：一道题起不来
		// 不该让剩下的几十道题陪葬。
		//
		// 但候选审计写不出去不是「这道题的事」：它意味着**这次运行的提交记录已经
		// 不完整了**，而 Submit=true 的每一次提交都是不可追回的平台写操作。继续跑
		// 只会让缺口越拉越大，事后没有任何办法回答「到底提交了什么、平台怎么判的」
		// ——那正是这份审计存在的全部理由。所以它按运行级故障处理：停下来，等调用
		// 方修好落盘面再重跑。
		//
		// 与 `harness.result`（results.Save 失败）的区别：那一处 Run 直接返回错误，
		// 因为它已经没有结果可交；这里本题的成绩**仍然保留在 cr.Outcome 里**——
		// 停止不等于抹掉。
		if IsKind(runErr, KindPersistence) {
			break
		}
	}
	result.EndedAt = h.now()
	result.Completed = runCompleted(result)
	result.State = runState(result, firstErr)
	switch {
	case result.State == RunCancelled:
		result.Reason = ReasonStopped
	case result.Err != "":
		if IsKind(firstErr, KindProvider) {
			result.Reason = ReasonProviderFailure
		} else {
			result.Reason = ReasonError
		}
	case result.Completed:
		result.Reason = ReasonCompleted
	default:
		// 跑到这里说明没有任何错误、也没有任何一道题达成目标。v0.4 要求
		// 「运行无错误」不等于「解题成功」——把这个区别显式写进 Reason，
		// 而不是让调用方从 Completed 反推。
		result.Reason = ReasonNoProgress
	}
	saveCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		saveCtx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
	}
	if err := h.results.Save(saveCtx, result); err != nil {
		return result, errors.Join(firstErr, Ef(KindPersistence, "harness.result", "保存运行指标失败", err))
	}
	return result, firstErr
}

// appendAudit 把一次提交写进私密候选审计，并把**写不出去**这件事交给调用方。
//
// **为什么必须返回错误**（这一条此前被 `_ =` 显式丢弃）：原来的理由是「能写坏
// 这次追加的机器，紧接着也会写坏 `results.Save`」——**这个推理不成立**。审计有它
// **自己**的两条上限（单行 64 KiB、单文件 16 MiB，见 store/audit.go）与自己的 ctx
// 检查，与 results.Save 完全独立：一行超长的候选审计会被拒，而公开指标照常写得
// 进去。于是在旧实现下，`store/audit.go` 里两条 KindPersistence 分类**在生产路径
// 上永远没有人看得到**——公开计数照常递增、State 不变、日志干净，而「提交了什么、
// 平台怎么判的」已经答不出来了。这正是本仓库反复记为「比缺失更糟」的形状：
// 字段在、机制在，只是没接线。
//
// 返回 nil 的两条路径：写入成功，以及端口未接线（见下）。
func (h *Harness) appendAudit(ctx context.Context, runID RunID, code string, c Candidate,
	v SubmissionVerdict, eval Evaluation, evalErr error, at time.Time) error {
	audits := h.audits
	if audits == nil {
		// 没接审计端口。与 GraphSaver 同档：可选端口，不接不影响任何一次运行的
		// 成败。但「没接」必须是**可见的**——否则「这次运行没有审计」会与
		// 「这次运行没提交过」长得一样。见 Doctor 的 candidate_audit 一行。
		//
		// ⚠️ 这里返回 nil **不等于**「不接审计无所谓」：`Submit=true` 时它在 Run
		// 的开头就被拒了（那条守卫读的是**同一个**构造函数冻结的字段）。走到这里
		// 只可能是干跑——没有平台写操作，所以没有「不可追回的写操作没人记」的问题。
		//
		// ⚠️ 读冻结字段而不是在这里重新对 `h.results` 断言一次类型：那就是同一个
		// 问题有两个判据，而两套答案会以最糟的方式分叉——Run 开头照 h.audits 放行，
		// 之后有人换掉 Results，提交继续发生而审计**静默地不再落盘**，恰是本函数
		// 要根除的那个形状。
		return nil
	}
	rec := CandidateAudit{
		Source: c.Source, Provenance: string(c.Provenance),
		IntentID: c.IntentID, Round: c.Round,
		SubmittedAt: at, Verdict: v,
		Score: eval.Score, Message: eval.Message,
		Flag: c.Flag,
	}
	// SubmitError 只装**传输/平台异常**的文本；判错的说明在 Message 里（平台
	// 原话）。两者分开的理由与根包契约相同（见 Candidate 的 RejectReason /
	// SubmitError 注释）：合成一个字段会让「平台说它错了」与「我们没能问成平台」
	// 变成同一件事，而这两者的处置完全相反——前者不该重试，后者要先对账。
	//
	// ⚠️ `c` 是 Mark **之前**从 gate 取出的副本，所以 c.SubmitError 在这里恒为
	// 空（它是 Mark 填的）。不要从这里读它。
	if evalErr != nil {
		rec.SubmitError = evalErr.Error()
	}
	if err := audits.AppendAudit(ctx, runID, code, rec); err != nil {
		// 公开面只拿固定文案（写不出去的原因可能是平台响应片段或路径），原始错误
		// 留在 err 链上——与 graphSaveStage 同一档处置。
		//
		// 哨兵与分类都放进链上：分类回答「哪一类」（调用方据此决定停机），哨兵回答
		// 「错的是哪一件事」（同一分类下还有别的落盘故障）。
		return Ef(KindPersistence, "harness.audit",
			"候选审计未能写入：本题的提交记录不完整", errors.Join(ErrAuditIncomplete, err))
	}
	return nil
}

// reclaimStale 回收上一次运行留下的、本次不用的沙箱资源。
//
// 返回**完整报告**（删了什么 + 发现了但没删什么）加错误：报告由调用方折进公开
// 结果的计数，错误决定这次运行还要不要继续。两者都要，因为它们是两件事——
// 「回收跑完了但宿主上还有归属不明的资源」不是失败，而「回收跑到一半 docker
// 挂了」才是。
func (h *Harness) reclaimStale(ctx context.Context, keep RunID) (ReclaimReport, error) {
	live := map[RunID]bool{keep: true}
	rep, err := h.sandbox.ReclaimStale(ctx, live)
	if err == nil || errors.Is(err, context.Canceled) {
		// 取消是**预期**的收场（Ctrl-C / 超时），不是回收故障：把它报成错误会让
		// 一次用户主动中断在结果里长成一次「回收失败」。
		//
		// ⚠️ 报告仍然原样返回：取消时实现方可能已经删掉了一批，那批计数必须留下。
		return rep, nil
	}
	return rep, Ef(KindExecutor, "harness.reclaim", "回收遗留 sandbox 失败", err)
}

// runCompleted 判定一次运行是否「成功」。
//
// 语义刻意区分两件事：
//   - RunFinished：所有题目都跑完了（没有异常收场）；
//   - ChallengeSolved：某道题的目标真的达成了（平台权威的 Objective.Completed）。
//
// 只有后者能让 Completed 为真。把「没报错」当成「解出来了」，正是前身
// 「280 run / 0 flag」那种数字的来源。
func runCompleted(r RunResult) bool {
	if r.Err != "" || len(r.Challenges) == 0 {
		return false
	}
	for _, c := range r.Challenges {
		if c.Outcome.Reason == ReasonSolved {
			return true
		}
	}
	return false
}

// runState 判定一次运行的终态：finished / failed / cancelled。
//
// **取消优先于失败**：一次 Ctrl-C 之后即使某道题恰好以错误收场，用户该看到的是
// 「被取消」——那是他自己按的键，而不是他需要去排查的故障。反过来把取消报成
// 失败，会让人去查一个不存在的 bug。
//
// 取消的判据有两处，缺一不可：
//   - `firstErr` 的分类：绝大多数取消路径会带着 KindCancelled 返回；
//   - 题级 `ReasonStopped`：轮循环**开头**的 ctx 判定是直接 `break` 并以
//     `(cr, nil)` 收场的（那一轮根本没开始跑），所以 firstErr 会是 nil。只看
//     firstErr 会让「刚好在轮首被取消」的那次运行被判成 finished。
//
// ReasonStopped 只在取消路径上被写入，所以拿它当判据是安全的（它不是「跑完了」
// 的同义词——正常跑完是 ReasonSolved / ReasonNoProgress / ReasonMax* 等）。
func runState(r RunResult, firstErr error) RunState {
	if IsKind(firstErr, KindCancelled) {
		return RunCancelled
	}
	for _, c := range r.Challenges {
		if c.Outcome.Reason == ReasonStopped {
			return RunCancelled
		}
	}
	if r.Err != "" {
		return RunFailed
	}
	return RunFinished
}

// manifest 是本次运行共享的运行清单，本函数负责把 Probe 解析出的镜像身份记进去
// （第一次成功 Probe 冻结，后续只做一致性核对）。题目串行，所以不需要加锁。
func (h *Harness) runChallenge(ctx context.Context, runID RunID, spec RunSpec, ch Challenge, manifest *RunManifest) (cr ChallengeResult, runErr error) {
	cr = ChallengeResult{Challenge: ch, StartedAt: h.now()}
	// ⚠️ **必须回填进 OutcomeView**：耗时（CLI 摘要、公开结果的 durationSeconds、
	// stats 的累计耗时）全部走 `OutcomeView.Duration()`，而它读的是 OutcomeView 自己
	// 的 StartedAt/EndedAt。只设 ChallengeResult 上那两个字段的话，三处都恒为 0
	// ——字段在、文档写了、落盘了，但永远是零。实测：一次真跑里题目实际耗时
	// 约 10 分钟，公开结果写的是 `durationSeconds: 0`。
	cr.Outcome.StartedAt = cr.StartedAt
	// 图产物的**默认**结果先落下来，早于 Prepare/沙箱/Probe 的每一条出口：那几条
	// 出口上「本题没有图」是**可证的**（我们连 SaveGraph 都没调到），落到这里的是
	// 「端口不在位」（disabled）与「端口在位但还没有文件产生」（absent）两档；
	// 真走到落盘的那条路由下面的 defer 用实现方如实返回的状态覆盖。
	//
	// 与 StartedAt/EndedAt 同一条理由：逐条出口补必然漏，而漏掉的那一格会是一个
	// **没定义**的值（空串），读的人只能自己猜它是什么意思——GraphState 的四个取值
	// 是穷举的，空串不在其中。
	cr.Outcome.GraphState = foldGraphState(h.graphs, GraphAbsent, nil)
	target, err := h.scenario.Prepare(ctx, ch)
	if err != nil {
		cr.EndedAt = h.now()
		return cr, err
	}
	var ss SandboxSession
	var ag Agent
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if ag != nil {
			if err := ag.Close(cleanupCtx); err != nil {
				cr.Outcome.CleanupFailures = append(cr.Outcome.CleanupFailures, "agent")
			}
		}
		if ss != nil {
			if err := ss.Close(cleanupCtx); err != nil {
				cr.Outcome.CleanupFailures = append(cr.Outcome.CleanupFailures, "sandbox")
			}
		}
		if err := h.scenario.Cleanup(cleanupCtx, ch); err != nil {
			cr.Outcome.CleanupFailures = append(cr.Outcome.CleanupFailures, "scenario")
		}
		if cr.EndedAt.IsZero() {
			cr.EndedAt = h.now()
		}
		// 与 StartedAt 同理：OutcomeView 的耗时才是三处消费点真正读的那一份。
		// 放在这里而不是每个 return 之前，是因为本题的出口有十几条（取消、轮级
		// 错误、重启失败、预算耗尽……），逐条补必然漏。
		cr.Outcome.EndedAt = cr.EndedAt
	}()
	sb := spec.Sandbox
	if sb.Image == "" {
		sb.Image = spec.Executor.Image
	}
	// Workdir 是**容器内**的工作目录。
	//
	// ⚠️ 这里刻意不再回落到 `spec.Executor.Workdir`：那个字段的历史含义是「唯一
	// 允许挂进容器的宿主目录」（见 CLI 的 --workdir 说明与 model.go 的 ExecSpec），
	// 而 SandboxSpec.Workdir 是容器内路径。两者混用会把宿主路径塞进容器里的
	// `--workdir`，pi 的会话目录随之落到一个不存在的位置，而失败是**静默**的
	// （pi 自己找地方落盘，get_entries 游标指向别处）。v0.4 不再挂载可写宿主目录，
	// 所以这个字段现在只有一个来源：SandboxSpec.Workdir，缺省 /work。
	if sb.Workdir == "" {
		sb.Workdir = defaultSandboxWorkdir
	}
	if sb.CPUs == 0 {
		sb.CPUs = spec.Executor.CPUs
	}
	if sb.MemoryMB == 0 {
		sb.MemoryMB = spec.Executor.MemoryMB
	}
	if sb.PidsLimit == 0 {
		sb.PidsLimit = spec.Executor.PidsLimit
	}
	sb.RunID, sb.Target = runID, target
	if sb.ProfileDir == "" {
		sb.ProfileDir = spec.Profile.ExtensionBundle
	}
	// RemainingAtStart 必须在**任何提交之前**记下：它就是「起跑时还差几个」，
	// 也就是增量召回率的分母。放到轮循环之后记等于把本次成绩算进基线。
	cr.Outcome.RemainingAtStart = ch.Remaining()
	ss, err = h.sandbox.NewSession(ctx, sb)
	if err != nil {
		cr.EndedAt = h.now()
		return cr, err
	}
	// Probe 的返回值此前被 `_` 丢弃——镜像 ID 与 pi 版本因此从来没有进过任何落盘
	// 产物，而「这次跑的到底是哪个镜像」恰恰是复现一次实验的前提。现在它进运行清单。
	if pr, err := ss.Probe(ctx); err != nil {
		cr.EndedAt = h.now()
		return cr, err
	} else {
		manifest.observeProbe(pr)
	}
	var planner Planner
	var renderer Renderer
	var gate CandidateGate
	sink := newEventSink(ctx)
	// ⚠️ **Close 与图落盘必须在同一个 defer 里，顺序显式写死**：
	//
	//  1. 先 sink.Close() 等事件消费者协程退出。本题的若干错误出口（轮次错误、
	//     预算耗尽、提交不确定…）**不调 sink.Flush**，那条路上消费者可能仍在
	//     处理残留事件、也就是仍在写图（ObserveEvent → 图）。序列化要遍历全部
	//     节点并 marshal，窗口远大于一次计数，撞上就是数据竞争。
	//  2. 再落盘。
	//
	// 不拆成两个 defer 靠 LIFO 定序：那是隐式的，将来有人在中间插一行 defer 就会
	// 静默翻转——而翻转的表现是数据竞争，不是编译错误。
	defer func() {
		sink.Close()
		// ⚠️ `h.graphs == nil` 就是接口值本身为 nil，**不用反射或 typed-nil 的
		// 检测技巧**：一个 typed-nil（例如 `(*dagGraphSaver)(nil)` 装进接口）在
		// 这里**不是** nil，那是调用方的类型错误，而把它当 nil 处理只会让它绕过
		// GraphSaver 的端口契约、得到一个静默的 disabled。
		if h.graphs != nil {
			// 独立的有界 context：走到这里时 ctx 可能已被取消（取消/超时路径），
			// 而图恰恰是那些路径上最值得留下的东西。与 cleanup 同一个理由。
			saveCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			state, err := h.graphs.SaveGraph(saveCtx, runID, ch)
			if err != nil {
				cr.Outcome.GraphSaveFailures = append(cr.Outcome.GraphSaveFailures, graphSaveStage(err))
			}
			if state == GraphFailed && err == nil {
				// 实现方报了失败却没给错误。端口契约（ports.go）要求失败必须带
				// err——阶段是从 err 链上的哨兵推出来的。补一条 unknown 阶段，
				// 保住「failed ⟺ 阶段非空」这条不变式；否则公开面会出现「状态说
				// 失败、却没有任何阶段可查」的自相矛盾。
				cr.Outcome.GraphSaveFailures = append(cr.Outcome.GraphSaveFailures, graphStageUnknown)
			}
			cr.Outcome.GraphState = foldGraphState(h.graphs, state, cr.Outcome.GraphSaveFailures)
		}
		// 端口不在位时这里**什么都不做**：函数开头落下的 GraphDisabled 已经是
		// 正确答案，而它只由这一处（根包）折算——实现方不得返回它。
	}()
	if h.gate != nil && (h.solverWithProfile != nil || h.solver != nil || h.planner != nil && h.renderer != nil) {
		if h.solverWithProfile != nil {
			planner, renderer = h.solverWithProfile(ch, spec.Profile)
		} else if h.solver != nil {
			planner, renderer = h.solver(ch)
		} else {
			planner, renderer = h.planner(ch), h.renderer(ch)
		}
		gate = h.gate(ch)
		sink.on = func(e Event, round int) error {
			if traces, ok := h.results.(TraceStore); ok {
				if err := traces.AppendTrace(ctx, runID, ch.Code, e); err != nil {
					return err
				}
			}
			gate.Observe(e)
			planner.ObserveEvent(e, round)
			return nil
		}
	}
	ag, err = h.agents.New(spec.Agent, ss, sink)
	if err != nil {
		cr.EndedAt = h.now()
		return cr, err
	}
	if err := ag.Start(ctx, AgentStart{Workdir: sb.Workdir, SystemPrompt: spec.Profile.SystemPrompt,
		Provider: spec.Agent.Provider,
		Model:    spec.Agent.Model, Thinking: spec.Agent.Thinking, Extensions: spec.Agent.Extensions,
		Approve: spec.Agent.Approve, SessionDir: spec.Agent.SessionDir, HomeDir: spec.Agent.HomeDir}); err != nil {
		cr.EndedAt = h.now()
		return cr, err
	}
	if planner == nil || renderer == nil || gate == nil {
		// 缺 Planner/Renderer/Gate 时**不能静默返回一道「没解出来」的题**：
		// 那会让报告把装配错误读成「模型不行」。明确记成配置错误。
		cr.Outcome.Reason = ReasonError
		cr.Outcome.Err = "缺少 Planner/Renderer/Gate：本题未执行任何轮次"
		cr.EndedAt = h.now()
		return cr, Ef(KindConfig, "harness.challenge", cr.Outcome.Err, nil)
	}
	var used Budget
	dryRounds, hintUsed, lastProgress := 0, 0, ch.Solved
	// 本题已向平台提交的次数。**跨轮累计**：上限约束的是「这道题一共提交了多少
	// 次」，而不是「这一轮提交了多少次」——失控的形状正是「每轮都提交一批」。
	submitted := 0
	submitCap := resolveSubmitCap(spec)
	// 停滞判据的另一半：宿主已验证事实的水位线。初值在**进循环之前**取，
	// 这样第 1 轮就有一个可比的基线（建图时入图的授权地址也算数）。
	lastHostFacts := planner.HostFacts()
	restarted := false
	var previousTurns int
	var previousCost float64
	recoveryNote := ""
	hintPolicy := spec.HintPolicy
	if hintPolicy == "" {
		hintPolicy = HintAuto
	}
	for round := 1; ; round++ {
		// ctx 判定必须先于预算与 provider 护栏：一次零回合的墙钟超时如果被
		// 记成「模型服务挂了」，报告会把用户自己按的 Ctrl-C 读成 provider 故障。
		if err := ctx.Err(); err != nil {
			cr.Outcome.Reason = ReasonStopped
			cr.Outcome.Err = "运行被取消"
			break
		}
		// 墙钟要**真的累加进 used**：v0.2 的 used.MaxWall 永远是 0，于是
		// MaxWall 那条分支不可达，墙钟预算实际上是死代码。
		used.MaxWall = h.now().Sub(cr.StartedAt)
		if exhausted, reason := spec.Budget.Exhausted(used); exhausted {
			cr.Outcome.Reason = reason
			break
		}
		it, err := planner.Next(ctx, PlannerInput{Challenge: ch, Outcome: cr.Outcome, Round: round})
		if err != nil {
			cr.EndedAt = h.now()
			return cr, err
		}
		if it == nil {
			cr.Outcome.Reason = ReasonNoIntent
			break
		}
		sink.setRound(round)
		planner.Activate(it)
		gate.SetIntent(it.ID, round)
		prompt := recoveryNote + renderer.Render(ctx, ch, it, &cr.Outcome)
		recoveryNote = ""
		res, err := ag.Round(ctx, RoundRequest{Prompt: prompt, Round: round, IntentID: it.ID, Timeout: DefaultRoundTimeout})
		used.MaxRounds++
		used.MaxTurns += res.Turns
		cr.Outcome.Rounds++
		if ctx.Err() != nil {
			cr.Outcome.Reason = ReasonStopped
			cr.Outcome.Err = "运行被取消"
			cr.EndedAt = h.now()
			return cr, Ef(KindCancelled, "harness.round", "运行被取消", ctx.Err())
		}
		if err != nil && res.Err == "" {
			res.Err = "Agent 进程或 RPC 故障"
		}
		// 轮级错误必须先被识别再谈进展：0 回合 + 有错误的「跑完了」正是前身
		// 280 run / 0 flag 的呈现方式，不能让它继续走提交与对账。
		if res.Err != "" {
			if err != nil && res.ProviderError == "" && !restarted {
				restarted = true
				planner.Settle(it, res)
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				closeErr := errors.Join(ag.Close(cleanupCtx), ss.Close(cleanupCtx))
				cancel()
				if closeErr != nil {
					cr.Outcome.CleanupFailures = append(cr.Outcome.CleanupFailures, "agent", "sandbox")
					cr.Outcome.Reason = ReasonError
					cr.Outcome.Err = "Agent 重启前清理失败"
					return cr, Ef(KindExecutor, "harness.restart", cr.Outcome.Err, closeErr)
				}
				ag, ss = nil, nil
				previousTurns = cr.Outcome.Stats.Turns
				previousCost = cr.Outcome.Stats.CostUSD
				ss, err = h.sandbox.NewSession(ctx, sb)
				if err == nil {
					// 重启后的 Probe 同样进清单：它走的是同一次核对，不是
					// 「顺手看一眼」——换了镜像的会话不该被当成本来的那个。
					var pr ProbeResult
					if pr, err = ss.Probe(ctx); err == nil {
						manifest.observeProbe(pr)
					}
				}
				if err == nil {
					ag, err = h.agents.New(spec.Agent, ss, sink)
				}
				if err == nil {
					err = ag.Start(ctx, AgentStart{Workdir: sb.Workdir, SystemPrompt: spec.Profile.SystemPrompt,
						Provider: spec.Agent.Provider, Model: spec.Agent.Model, Thinking: spec.Agent.Thinking,
						Extensions: spec.Agent.Extensions, Approve: spec.Agent.Approve,
						SessionDir: spec.Agent.SessionDir, HomeDir: spec.Agent.HomeDir})
				}
				if err != nil {
					cr.Outcome.Reason = ReasonError
					cr.Outcome.Err = "Agent 重启失败"
					return cr, Ef(KindExecutor, "harness.restart", cr.Outcome.Err, err)
				}
				// Only host-verified counts cross the session boundary. Raw tool
				// output, candidates, and the prior transcript are never replayed.
				recoveryNote = fmt.Sprintf("上一 Agent 会话故障后已重启。平台已确认进度 %d/%d；已执行 %d 轮。请继续当前授权目标。\n",
					cr.Outcome.ProgressConfirmed, cr.Outcome.ProgressTotal, cr.Outcome.Rounds)
				continue
			}
			// 轮级错误必须终止**本题**：0 回合 + 有错误的「跑完了」正是前身
			// 280 run / 0 flag 的呈现方式，不能让它继续走提交与对账。
			//
			// 但它不终止整次运行——v0.4 的要求是「再次失败则结束当前题目并继续
			// 下一题」。所以这里返回错误（由 Run 记账后继续），而不是直接放弃。
			kind := KindExecutor
			cr.Outcome.Reason = ReasonError
			if res.ProviderError != "" {
				kind = KindProvider
				cr.Outcome.Reason = ReasonProviderFailure
			}
			cr.Outcome.Err = res.Err
			cr.EndedAt = h.now()
			return cr, Ef(kind, "harness.round", "轮次以错误收场", errors.New(res.Err))
		}
		if err := sink.Flush(ctx); err != nil {
			// ctx 已取消时 Flush 一定回 ctx.Err()（消费者的 AppendTrace 同理）。
			// 这两条路径必须和轮循环里的取消判定同形：否则一次 Ctrl-C 会被记成
			// executor 故障，公开结果与 stats 里的失败类别跟着一起错。
			if cerr := ctx.Err(); cerr != nil {
				cr.Outcome.Reason = ReasonStopped
				cr.Outcome.Err = "运行被取消"
				cr.EndedAt = h.now()
				return cr, Ef(KindCancelled, "harness.round", "运行被取消", cerr)
			}
			cr.Outcome.Reason = ReasonError
			cr.EndedAt = h.now()
			return cr, Ef(KindExecutor, "harness.events", "处理 Agent 事件失败", err)
		}
		stats, statsErr := ag.Stats(ctx)
		if statsErr != nil {
			if cerr := ctx.Err(); cerr != nil {
				cr.Outcome.Reason = ReasonStopped
				cr.Outcome.Err = "运行被取消"
				cr.EndedAt = h.now()
				return cr, Ef(KindCancelled, "harness.round", "运行被取消", cerr)
			}
			cr.Outcome.Reason = ReasonError
			cr.EndedAt = h.now()
			return cr, Ef(KindExecutor, "harness.stats", "读取 Agent 统计失败", statsErr)
		}
		stats.Turns += previousTurns
		stats.CostUSD += previousCost
		cr.Outcome.Stats = stats
		if stats.Turns > used.MaxTurns {
			used.MaxTurns = stats.Turns
		}
		used.MaxCostUSD = stats.CostUSD
		planner.Settle(it, res)
		progressed := false
		// NewAll 是 v0.4 的可提交集合（observed + derived）。它现在是接口方法，
		// 不再靠运行时断言取用——断言失败会静默回落到 observed-only，表现为
		// 「推导族的正确答案再也提交不出去」而所有测试全绿。
		candidates := gate.NewAll()
		capped := false
		for i, c := range candidates {
			if c.Provenance == ProvenanceFabricated || !spec.Submit {
				continue
			}
			// ⚠️ **上限判定必须排在平台调用之前。** 排在后面等于「停下来」的那一刻
			// 已经又提交了一次——上限就成了摆设，而且超出的量取决于候选有多少。
			if submitted >= submitCap {
				capped = true
				// 记下**这一轮里还剩多少候选没提交**。它与 Reason 配对才回答得了
				// 「上限定紧了，还是候选集合失控了」。
				cr.Outcome.SubmissionsCapped += len(candidates) - i
				break
			}
			eval, evalErr := h.scenario.Evaluate(ctx, ch, c.Flag)
			submitted++
			// Attempts 是这次调用的**公开**视图，赋值必须紧跟在这里，而不是等循环
			// 结束后回填：下面两条路径都会直接 return，回填会把它们漏掉——而
			// 「结果不确定」那一档恰恰最需要被计数（平台写超时，这一条到底算不算
			// 提交过）。它与局部变量 submitted 是同一个量的两个视图，不另算一遍。
			cr.Outcome.Attempts = submitted
			// 审计在**两条**路径上都要落：正常判定与「结果不确定」。后者尤其
			// 重要——它正是「平台写超时、这一条到底算不算提交」的那一档，而
			// 只记成功的审计恰好答不出这个问题。
			submittedAt := h.now()
			if evalErr != nil {
				// 平台写超时后，总进度不足以证明这一条候选的状态。记录本次
				// 提交，读取权威总进度，然后以不确定终态结束本题，禁止盲目重试。
				v := gate.Mark(c.Flag, Evaluation{}, evalErr)
				auditErr := h.appendAudit(ctx, runID, ch.Code, c, v, Evaluation{}, evalErr, submittedAt)
				if obj, reconcileErr := h.scenario.Reconcile(ctx, ch); reconcileErr == nil {
					cr.Outcome.ProgressConfirmed, cr.Outcome.ProgressTotal = obj.Got, obj.Want
				}
				cr.Outcome.Reason = ReasonError
				cr.Outcome.Err = "提交结果不确定"
				cr.EndedAt = h.now()
				if auditErr != nil {
					// 审计也没写出去。两件事都要留下：Err 保留「不确定」那句（它
					// 是这一行**没写成的审计**唯一的载体，也是本题停下来的原因），
					// AuditIncomplete 单独记审计那一笔。返回值给审计错误——它的
					// 停止粒度是**整次运行**（见 Run 的题目循环）。
					cr.Outcome.AuditIncomplete = true
					return cr, auditErr
				}
				return cr, Ef(KindPlatform, "harness.evaluate", "提交结果不确定", evalErr)
			}
			// 判定原样交给 gate：把 Evaluation 映射成账本字段（含 Duplicate 的
			// 派生）是 gate 的职责，放在这里等于让每个调用方各抄一份映射。
			// 它返回的 SubmissionVerdict 就是那份派生的结论——下面按它计数，
			// 而不是自己再判一次 `Accepted && !Progress`（两处各写一遍必然漂移，
			// 漂移的表现是幂等命中被记成新增确认，通过率系统性偏高）。
			v := gate.Mark(c.Flag, eval, nil)
			if auditErr := h.appendAudit(ctx, runID, ch.Code, c, v, eval, nil, submittedAt); auditErr != nil {
				// ⚠️ **不碰 Submitted / Flags / Score。** 它们是审计失败**之前**
				// 平台已经确认的事实（上面几条候选，以及本次提交在平台侧的判定），
				// 清零等于把「已经解出来了」改写成「没解出来」——而 State 与
				// Completed 本来就是两个问题（见 RunFinished 的注释）。这里的
				// 「停」只影响后续：不再提交、不再跑下一题。
				cr.Outcome.AuditIncomplete = true
				cr.Outcome.Reason = ReasonError
				cr.Outcome.Err = "候选审计未完整落盘"
				cr.EndedAt = h.now()
				return cr, auditErr
			}
			if eval.Progress {
				progressed = true
			}
			// 计数口径（三者互斥，由 verdict 保证）：
			//   Correct   —— 平台确认（**含幂等命中**），进 Flags 与 Submitted；
			//   Duplicate —— Correct 的子集，另记一笔「这次是重复确认」；
			//   Rejected  —— 平台明确判错。
			// Uncertain 不会走到这里：它在上面就 return 了，由一个私密审计行
			// 与本题的 ReasonError 承担，不需要一个公开计数。
			if v.Correct {
				cr.Outcome.Flags = append(cr.Outcome.Flags, c.Flag)
				cr.Outcome.Submitted++
				if v.Duplicate {
					cr.Outcome.Duplicates++
				}
				if eval.Score > cr.Outcome.Score {
					cr.Outcome.Score = eval.Score
				}
			} else if v.Rejected {
				cr.Outcome.Rejected++
			}
		}
		if capped {
			// 撞上限即结束本题。**不继续跑轮次**：后面的轮次只会产出更多无法提交
			// 的候选，继续跑等于把预算花在一个已经不能再提交的方向上。
			//
			// 用 ReasonSubmitLimit 而不是 ReasonError：这不是「运行坏了」，而是
			// 「这次运行的候选集合不正常」——两者的处置完全相反（前者查日志，
			// 后者多半要去看答案形态判定是不是放宽了）。
			cr.Outcome.Reason = ReasonSubmitLimit
			break
		}
		if obj, e := h.scenario.Reconcile(ctx, ch); e == nil {
			cr.Outcome.ProgressConfirmed, cr.Outcome.ProgressTotal = obj.Got, obj.Want
			if obj.Got > lastProgress {
				progressed = true
				lastProgress = obj.Got
			}
			if obj.Completed {
				cr.Outcome.Reason = ReasonSolved
				break
			}
		}
		// 停滞的第二个判据：**宿主新增了已验证事实**。
		//
		// 只看平台进度是不够的——一道题在拿到 flag 之前往往先积累一批真实事实
		// （banner、凭据线索、可达服务），那正是有进展的样子；把它们读成停滞
		// 会让 agent 在真的推进时被反复打断，甚至被换支，而每一次打断都要付
		// 提示额度或一条分支的代价。
		//
		// 读在这里而不是轮首：事实是本轮事件经 sink.Flush 灌进图的，轮首读到
		// 的水位线还是上一轮结束时的值。只数宿主族（见 HostFacts 的注释）——
		// 否则 agent 只要反复 report_fact 就能永远不被判停滞。
		if facts := planner.HostFacts(); facts > lastHostFacts {
			progressed = true
			lastHostFacts = facts
		}
		if progressed {
			dryRounds = 0
		} else {
			dryRounds++
		}
		// 阈值直接取自运行清单，而**不是**在这里再算一遍回落链。
		//
		// 为什么这很重要：清单的全部意义是「它记的是生效值」。若轮循环自己算一份、
		// 清单另算一份，两处一旦漂移，公开结果里那个数就只是「另一个算过一遍的数」
		// ——看起来正常，却与实际行为无关。取同一份来源，这条漂移就不存在。
		threshold := manifest.PlannerDryRounds
		// 提示**每题最多一次**。HintAlways 也受这条守卫——v0.2 的文档写着
		// 「每轮都提示」而代码里没有守卫，那会让提示额度被瞬间打光。
		shouldHint := hintUsed == 0 && (hintPolicy == HintAlways ||
			(hintPolicy == HintAuto && dryRounds >= threshold))
		if shouldHint {
			hint, hintErr := h.scenario.Hint(ctx, ch)
			if hintErr != nil {
				cr.EndedAt = h.now()
				return cr, hintErr
			}
			hintUsed++
			cr.Outcome.HintUsed = hintUsed
			if hint != "" {
				if err := ag.Steer(ctx, hint); err != nil {
					cr.EndedAt = h.now()
					return cr, err
				}
			}
			// 提示之后**重新开始数**：换支的判据是「提示后又连续停滞 threshold
			// 轮」，而不是「提示那一轮本来就够阈值了」。不归零的话下一轮立刻触发
			// 换支，提示等于白给——那既浪费一次平台提示额度，也浪费一个分支。
			dryRounds = 0
		} else if hintUsed > 0 && dryRounds >= threshold {
			// 提示过、也给够了机会，仍然停滞 ⇒ 放弃当前分支，转去未尝试的方向。
			//
			// 为什么不在提示时就换支：提示是最便宜的一次纠偏，先给它一次机会；
			// 给了还不动，才说明问题出在这一支本身（方向选错了，或者 agent 在这
			// 一点上反复打转），继续把剩余额度投进去没有意义。
			//
			// **HintOff 下不会走到这里**（hintUsed 恒为 0）：那一档的契约是
			// 「从不请求提示」，而规范里的换支是提示链的下游——用户既然关掉了
			// 自动干预，编排层就不该背着他改换方向。
			//
			// 终止性不依赖这里：Abandon 把意图移出前沿后，若前沿真的空了，下一轮
			// Next 返回 (nil, nil)，轮循环以 ReasonNoIntent 收场。
			planner.Abandon(it)
			cr.Outcome.BranchesAbandoned++
			dryRounds = 0
		}
	}
	cr.Outcome.Code = ch.Code
	cr.EndedAt = h.now()
	return cr, nil
}

const (
	eventQueueSize   = 256
	eventMaxBytes    = 1 << 20
	eventMaxPerRound = 10000
)

type queuedEvent struct {
	event Event
	round int
	ack   chan struct{}
}

type eventSink struct {
	ctx    context.Context
	mu     sync.Mutex
	round  int
	count  int
	err    error
	on     func(Event, int) error
	queue  chan queuedEvent
	stop   chan struct{}
	done   chan struct{}
	closed sync.Once
}

func newEventSink(ctx context.Context) *eventSink {
	s := &eventSink{ctx: ctx, queue: make(chan queuedEvent, eventQueueSize), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			select {
			case item := <-s.queue:
				if item.ack != nil {
					close(item.ack)
				} else if s.on != nil {
					if err := s.on(item.event, item.round); err != nil {
						s.mu.Lock()
						if s.err == nil {
							s.err = err
						}
						s.mu.Unlock()
					}
				}
			case <-s.stop:
				return
			}
		}
	}()
	return s
}

func (s *eventSink) Emit(e Event) {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return
	}
	s.count++
	if s.count > eventMaxPerRound {
		s.err = errors.New("单轮事件数超限")
		s.mu.Unlock()
		return
	}
	round := s.round
	s.mu.Unlock()
	encoded, err := json.Marshal(e)
	if err != nil || len(encoded) > eventMaxBytes {
		s.mu.Lock()
		s.err = errors.New("Agent 事件无法编码或体积超限")
		s.mu.Unlock()
		return
	}
	select {
	case s.queue <- queuedEvent{event: e, round: round}:
	case <-s.ctx.Done():
	case <-s.stop:
	}
}

func (s *eventSink) setRound(round int) {
	s.mu.Lock()
	s.round, s.count = round, 0
	s.mu.Unlock()
}

func (s *eventSink) Flush(ctx context.Context) error {
	ack := make(chan struct{})
	select {
	case s.queue <- queuedEvent{ack: ack}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return errors.New("事件消费者已停止")
	}
	select {
	case <-ack:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("事件消费者已停止")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *eventSink) Close() {
	s.closed.Do(func() { close(s.stop) })
	<-s.done
}

// contains 报告 xs 里是否有 want。刻意不引 slices：根包是纯契约层，工具函数
// 越少越好，而这里只有一处调用。
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// safeError 把错误折成一个**可进公开结果**的失败分类。
//
// 优先用错误分类（Kind）：它是枚举，可比较、无明文，且正好回答报告要问的
// 「怎么失败的」——provider 故障与执行器故障必须是两个串，否则通过率结论会把
// 它们混成一类。拿不到分类时才退回类型名。
//
// 为什么类型名只能当兜底：`*harness.Error` 对统计毫无信息量（所有被包装过的
// 错误都是它），而 `%T` 的输出形态也不稳定。但它比空串好——空串与「没有失败」
// 同形，调用方会把一次失败读成成功。
//
// 两条路径都只产出标识符形态的串，所以 RunResult.Err 可以安全落进公开结果。
func safeError(err error) string {
	if err == nil {
		return ""
	}
	if k, ok := KindOf(err); ok {
		return string(k)
	}
	return fmt.Sprintf("%T", err)
}

func digestJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "digest-error"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// bundleDigest 计算扩展包目录的**内容**摘要。
//
// 为什么需要它：`SolverProfile.Digest()` 是对结构体 JSON 取的，而
// `ExtensionBundle` 在那个结构体里是一个**路径字符串**——同一个路径下的内容换
// 了，摘要不变。于是报告会把两份不同的扩展包算作同一组，profile 对照实验得出
// 的结论是错的（改了 bundle 却观察到同样结果，会被归因到模型身上）。
//
// 口径与 digestJSON 一致（sha256 的 hex[:8]），但输入是目录内容：按相对路径排序
// 后逐个累积「相对路径 + 长度 + 内容」。**排序是必须的**——目录遍历顺序不稳定，
// 不排序会让同一份 bundle 每次算出不同摘要，那比没有摘要更糟（它看起来在工作）。
// 长度前缀也是必须的：只写内容的话，"ab"+"c" 与 "a"+"bc" 会撞成同一个摘要。
//
// 用流式 io.Copy 而不是 os.ReadFile：这个仓库对「一个大文件把宿主撑爆」是有
// 前科的（见 SandboxSpec 关于 307 GB 的注释），摘要没有理由把整个文件读进内存。
//
// **空路径返回空串**，语义与 `ProbeResult.PiVersion` 一致：空串表示**未核验**
// （本题没配扩展包），调用方不得把它读成「内容没问题」。路径存在但读不了则返回
// 错误——那是 fail closed，与 executor/session.go 对 ProfileDir 的校验同一态度。
func bundleDigest(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("扩展包不是目录")
	}
	var files []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// 统一用斜杠：Windows 上反斜杠会让同一份 bundle 在两种平台算出不同摘要。
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", rel, fi.Size())
		_, copyErr := io.Copy(h, f)
		_ = f.Close()
		if copyErr != nil {
			return "", copyErr
		}
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8]), nil
}
