package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// **not** in live, and returns what it reclaimed.
	//
	// 为什么不能只用 Reclaim(新 runID)：新 run 的 ID 是刚生成的，宿主上不可能
	// 有它的遗留——那次调用是空转，而**上次崩溃留下的**容器与网络会一直攒着。
	// 每个孤儿 bridge 占一个网段，攒够之后新 run 连网络都建不出来（表现为
	// 「起题失败」，而真因在几天前的崩溃现场）。所以启动前必须按 label 扫一遍，
	// 只保留 live 里的。
	ReclaimStale(ctx context.Context, live map[RunID]bool) ([]RunID, error)
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
	SystemPrompt    string         `json:"systemPrompt,omitempty"`
	ExtensionBundle string         `json:"extensionBundle,omitempty"`
	Planner         map[string]any `json:"planner,omitempty"`
	PromptPolicy    map[string]any `json:"promptPolicy,omitempty"`
}

func (p SolverProfile) Digest() string {
	// Reuse the same canonical digest mechanism as RunSpec without exposing a
	// second hashing policy in the public API.
	b := struct {
		Name, SystemPrompt, ExtensionBundle string
		Planner, PromptPolicy               map[string]any
	}{p.Name, p.SystemPrompt, p.ExtensionBundle, p.Planner, p.PromptPolicy}
	return digestJSON(b)
}

// Empty 报告这个 profile 是否「一个字段都没填」。
//
// 它是「该不该回落到默认 profile」的唯一判据，装配层（internal/wire.resolve）
// 必须调它，不能自己再抄一份字段列表：两处一旦漂移，Run 实际生效的 profile
// 与 RunSpec 里那份就不是同一个东西，而摘要看起来仍然正常。
func (p SolverProfile) Empty() bool {
	return p.Name == "" && p.SystemPrompt == "" && p.ExtensionBundle == "" &&
		len(p.Planner) == 0 && len(p.PromptPolicy) == 0
}

func profilePositiveInt(values map[string]any, key string) (int, bool) {
	v, exists := values[key]
	if !exists {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, n > 0 && n <= 10000
	case float64:
		i := int(n)
		return i, n > 0 && n <= 10000 && float64(i) == n
	}
	return 0, false
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
	Model         string
	Scenario      string
	Challenge     string
	Category      string
	Since         time.Time
	Until         time.Time
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
	Model         string
	StartedAt     time.Time
	EndedAt       time.Time
	Challenges    []ChallengeResult
	Completed     bool
	Reason        string
	Err           string
}

// Harness is the synchronous v0.4 façade. It intentionally contains no pause,
// resume, control socket or web lifecycle.
type Harness struct {
	scenario          Scenario
	sandbox           Sandbox
	agents            AgentFactory
	results           ResultStore
	locker            RunLocker
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
	Locker            RunLocker
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
	if len(missing) > 0 {
		return nil, Ef(KindConfig, "harness.new", "缺少必需端口: "+strings.Join(missing, ", "), nil)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Harness{scenario: opts.Scenario, sandbox: opts.Sandbox, agents: opts.Agents,
		results: opts.Results, locker: opts.Locker, gate: opts.Gate, solverWithProfile: opts.SolverWithProfile, solver: opts.Solver, planner: opts.Planner,
		renderer: opts.Renderer, profile: opts.Profile, now: now}, nil
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

	// 先按 label 扫掉**上次崩溃**留下的容器/网络/规则，再建本题的资源。
	//
	// 为什么不是 Reclaim(ctx, runID)：runID 是上面刚生成的，宿主上不可能有它的
	// 遗留——那次调用是空转，而孤儿会一直攒着（每个 bridge 占一个网段，攒够
	// 之后新 run 连网络都建不出来）。live 集合里只有本次 run，所以本次自己的
	// 资源不会被误删。
	if err := h.reclaimStale(ctx, runID); err != nil {
		return result, err
	}
	challenges, err := h.scenario.Discover(ctx, spec)
	if err != nil {
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
		cr, runErr := h.runChallenge(ctx, runID, spec, ch)
		result.Challenges = append(result.Challenges, cr)
		if runErr != nil && firstErr == nil {
			firstErr = runErr
			result.Err = safeError(runErr)
		}
	}
	result.EndedAt = h.now()
	result.Completed = runCompleted(result)
	switch {
	case IsKind(firstErr, KindCancelled):
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

// reclaimStale 回收上一次运行留下的、本次不用的沙箱资源。
func (h *Harness) reclaimStale(ctx context.Context, keep RunID) error {
	live := map[RunID]bool{keep: true}
	_, err := h.sandbox.ReclaimStale(ctx, live)
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return Ef(KindExecutor, "harness.reclaim", "回收遗留 sandbox 失败", err)
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

func (h *Harness) runChallenge(ctx context.Context, runID RunID, spec RunSpec, ch Challenge) (cr ChallengeResult, runErr error) {
	cr = ChallengeResult{Challenge: ch, StartedAt: h.now()}
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
	if _, err := ss.Probe(ctx); err != nil {
		cr.EndedAt = h.now()
		return cr, err
	}
	sink := newEventSink(ctx)
	defer sink.Close()
	var planner Planner
	var renderer Renderer
	var gate CandidateGate
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
					_, err = ss.Probe(ctx)
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
		for _, c := range candidates {
			if c.Provenance == ProvenanceFabricated || !spec.Submit {
				continue
			}
			eval, evalErr := h.scenario.Evaluate(ctx, ch, c.Flag)
			if evalErr != nil {
				// 平台写超时后，总进度不足以证明这一条候选的状态。记录本次
				// 提交，读取权威总进度，然后以不确定终态结束本题，禁止盲目重试。
				gate.Mark(c.Flag, Evaluation{}, evalErr)
				if obj, reconcileErr := h.scenario.Reconcile(ctx, ch); reconcileErr == nil {
					cr.Outcome.ProgressConfirmed, cr.Outcome.ProgressTotal = obj.Got, obj.Want
				}
				cr.Outcome.Reason = ReasonError
				cr.Outcome.Err = "提交结果不确定"
				cr.EndedAt = h.now()
				return cr, Ef(KindPlatform, "harness.evaluate", "提交结果不确定", evalErr)
			}
			// 判定原样交给 gate：把 Evaluation 映射成账本字段（含 Duplicate 的
			// 派生）是 gate 的职责，放在这里等于让每个调用方各抄一份映射。
			gate.Mark(c.Flag, eval, nil)
			if eval.Progress {
				progressed = true
			}
			if eval.Accepted {
				cr.Outcome.Flags = append(cr.Outcome.Flags, c.Flag)
				cr.Outcome.Submitted++
				if eval.Score > cr.Outcome.Score {
					cr.Outcome.Score = eval.Score
				}
			}
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
		if progressed {
			dryRounds = 0
		} else {
			dryRounds++
		}
		threshold := spec.Policy.DryRoundsBeforeHint
		if threshold <= 0 {
			if n, ok := profilePositiveInt(spec.Profile.Planner, "dryRoundsBeforeHint"); ok {
				threshold = n
			}
		}
		if threshold <= 0 {
			threshold = 2
		}
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
