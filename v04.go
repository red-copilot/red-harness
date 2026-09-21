package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ResearchVersion is the intentionally incompatible v0.4 research SDK line.
// Version remains 0.3.0 for readers of the legacy graph schema.
const ResearchVersion = "0.4.0-research"

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
type ProbeResult struct {
	ContainerID string
	PID         int
	Image       string
	Workdir     string
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

// Sandbox is the v0.4 execution port. Reclaim is used before starting a new
// run to remove labelled leftovers from an earlier crash.
type Sandbox interface {
	NewSession(context.Context, SandboxSpec) (SandboxSession, error)
	Reclaim(context.Context, RunID) error
}

// SolverProfile is immutable run input. The bundle is mounted read-only by a
// sandbox implementation; Digest is stable and safe to put in public results.
type SolverProfile struct {
	Name            string         `json:"name,omitempty"`
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

// ResultStore is intentionally separate from the event/snapshot Store. It
// stores aggregate metrics and fingerprints, never candidate plaintext.
type ResultStore interface {
	Save(context.Context, RunResult) error
	Get(context.Context, RunID) (RunResult, error)
	List(context.Context) ([]RunResult, error)
	Stats(context.Context, StatsQuery) (StatsReport, error)
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

type StatsReport struct {
	Runs              int     `json:"runs"`
	Completed         int     `json:"completed"`
	CompletionRate    float64 `json:"completionRate"`
	ConfirmedFlags    int     `json:"confirmedFlags"`
	RemainingAtStart  int     `json:"remainingAtStart"`
	Score             int     `json:"score"`
	CostUSD           float64 `json:"costUSD"`
	DurationSeconds   float64 `json:"durationSeconds"`
	HintedRuns        int     `json:"hintedRuns"`
	ProviderFailures  int     `json:"providerFailures"`
	ExecutionFailures int     `json:"executionFailures"`
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
	scenario Scenario
	sandbox  Sandbox
	agents   AgentFactory
	results  ResultStore
	gate     func(Challenge) CandidateGate
	planner  func(Challenge) Planner
	renderer func(Challenge) Renderer
	profile  SolverProfile
	now      func() time.Time
	mu       sync.Mutex
	running  bool
}

type HarnessOptions struct {
	Scenario Scenario
	Sandbox  Sandbox
	Agents   AgentFactory
	Results  ResultStore
	Gate     func(Challenge) CandidateGate
	Planner  func(Challenge) Planner
	Renderer func(Challenge) Renderer
	Profile  SolverProfile
	Now      func() time.Time
}

func NewHarness(opts HarnessOptions) (*Harness, error) {
	missing := ""
	if opts.Scenario == nil {
		missing = "Scenario"
	}
	if opts.Sandbox == nil {
		if missing != "" {
			missing += ", "
		}
		missing += "Sandbox"
	}
	if opts.Agents == nil {
		if missing != "" {
			missing += ", "
		}
		missing += "Agents"
	}
	if missing != "" {
		return nil, Ef(KindConfig, "harness.new", "缺少必需端口: "+missing, nil)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Harness{scenario: opts.Scenario, sandbox: opts.Sandbox, agents: opts.Agents,
		results: opts.Results, gate: opts.Gate, planner: opts.Planner,
		renderer: opts.Renderer, profile: opts.Profile, now: now}, nil
}

func (h *Harness) Doctor(ctx context.Context) DoctorReport {
	r := DoctorReport{OK: true}
	checks := []DoctorCheck{{Name: "scenario", OK: h != nil && h.scenario != nil, Fatal: true},
		{Name: "sandbox", OK: h != nil && h.sandbox != nil, Fatal: true},
		{Name: "agent_factory", OK: h != nil && h.agents != nil, Fatal: true}}
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
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return RunResult{}, Ef(KindConfig, "harness.run", "已有运行进行中", nil)
	}
	h.running = true
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.running = false; h.mu.Unlock() }()

	started := h.now()
	runID := RunID(fmt.Sprintf("run-%d", started.UnixNano()))
	result := RunResult{RunID: runID, Scenario: spec.Scenario, ProfileDigest: h.profile.Digest(), Model: spec.Agent.Model, StartedAt: started}
	if err := h.sandbox.Reclaim(ctx, runID); err != nil && !errors.Is(err, context.Canceled) {
		return result, Ef(KindExecutor, "harness.reclaim", "回收遗留 sandbox 失败", err)
	}
	challenges, err := h.scenario.Discover(ctx, spec)
	if err != nil {
		return result, err
	}
	for _, ch := range challenges {
		if len(spec.Targets) > 0 && !contains(spec.Targets, ch.Code) {
			continue
		}
		cr, runErr := h.runChallenge(ctx, runID, spec, ch)
		result.Challenges = append(result.Challenges, cr)
		if runErr != nil && result.Err == "" {
			result.Err = safeError(runErr)
		}
	}
	result.EndedAt = h.now()
	result.Completed = result.Err == "" && len(result.Challenges) > 0
	if result.Completed {
		result.Reason = ReasonCompleted
	} else if result.Err != "" {
		result.Reason = ReasonError
	}
	if h.results != nil {
		if err := h.results.Save(ctx, result); err != nil {
			return result, Ef(KindPersistence, "harness.result", "保存运行指标失败", err)
		}
	}
	if result.Err != "" {
		return result, errors.New(result.Err)
	}
	return result, nil
}

func (h *Harness) runChallenge(ctx context.Context, runID RunID, spec RunSpec, ch Challenge) (ChallengeResult, error) {
	cr := ChallengeResult{Challenge: ch, StartedAt: h.now()}
	target, err := h.scenario.Prepare(ctx, ch)
	if err != nil {
		cr.EndedAt = h.now()
		return cr, err
	}
	sb := spec.Sandbox
	if sb.Image == "" {
		sb.Image = spec.Executor.Image
	}
	if sb.Workdir == "" {
		sb.Workdir = spec.Executor.Workdir
		if sb.Workdir == "" {
			sb.Workdir = "/work"
		}
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
	ss, err := h.sandbox.NewSession(ctx, sb)
	if err != nil {
		cr.EndedAt = h.now()
		_ = h.scenario.Cleanup(context.Background(), ch)
		return cr, err
	}
	defer func() { _ = ss.Close(context.Background()); _ = h.scenario.Cleanup(context.Background(), ch) }()
	if _, err := ss.Probe(ctx); err != nil {
		cr.EndedAt = h.now()
		return cr, err
	}
	sink := &eventSink{}
	var planner Planner
	var renderer Renderer
	var gate CandidateGate
	if h.planner != nil && h.renderer != nil && h.gate != nil {
		planner, renderer, gate = h.planner(ch), h.renderer(ch), h.gate(ch)
		sink.on = func(e Event, round int) { gate.Observe(e); planner.ObserveEvent(e, round) }
	}
	ag, err := h.agents.New(spec.Agent, ss, sink)
	if err != nil {
		cr.EndedAt = h.now()
		return cr, err
	}
	defer func() { _ = ag.Close(context.Background()) }()
	if err := ag.Start(ctx, AgentStart{Workdir: spec.Executor.Workdir, Provider: spec.Agent.Provider,
		Model: spec.Agent.Model, Thinking: spec.Agent.Thinking, Extensions: spec.Agent.Extensions,
		Approve: spec.Agent.Approve, SessionDir: spec.Agent.SessionDir, HomeDir: spec.Agent.HomeDir}); err != nil {
		cr.EndedAt = h.now()
		return cr, err
	}
	if planner == nil || renderer == nil || gate == nil {
		cr.EndedAt = h.now()
		return cr, nil
	}
	var used Budget
	dryRounds, hintUsed, lastProgress := 0, 0, ch.Solved
	hintPolicy := spec.HintPolicy
	if hintPolicy == "" {
		hintPolicy = HintAuto
	}
	for round := 1; ; round++ {
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
		res, err := ag.Round(ctx, RoundRequest{Prompt: renderer.Render(ctx, ch, it, &cr.Outcome), Round: round, IntentID: it.ID, Timeout: DefaultRoundTimeout})
		used.MaxRounds++
		used.MaxTurns += res.Turns
		cr.Outcome.Rounds++
		if err != nil && res.Err == "" {
			cr.EndedAt = h.now()
			return cr, err
		}
		planner.Settle(it, res)
		progressed := false
		candidates := gate.New()
		if v04Gate, ok := gate.(interface{ NewAll() []Candidate }); ok {
			candidates = v04Gate.NewAll()
		}
		for _, c := range candidates {
			if c.Provenance == ProvenanceFabricated || !spec.Submit {
				continue
			}
			eval, evalErr := h.scenario.Evaluate(ctx, ch, c.Flag)
			gate.Mark(c.Flag, SubmitResult{Correct: eval.Accepted, Awarded: eval.Score, Duplicate: eval.Accepted && !eval.Progress, Message: eval.Message}, evalErr)
			if evalErr != nil {
				// A timed-out platform write is ambiguous. Reconcile once before
				// surfacing the error; never blindly submit the same candidate again.
				if _, reconcileErr := h.scenario.Reconcile(ctx, ch); reconcileErr == nil {
					continue
				}
				return cr, evalErr
			}
			if eval.Progress {
				progressed = true
			}
			if eval.Accepted {
				cr.Outcome.Flags = append(cr.Outcome.Flags, c.Flag)
				cr.Outcome.Submitted++
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
			threshold = 2
		}
		shouldHint := hintPolicy == HintAlways && hintUsed == 0
		if hintPolicy == HintAuto && hintUsed == 0 && dryRounds >= threshold {
			shouldHint = true
		}
		if shouldHint {
			if hint, hintErr := h.scenario.Hint(ctx, ch); hintErr != nil {
				return cr, hintErr
			} else {
				hintUsed++
				cr.Outcome.HintUsed = hintUsed
				if hint != "" {
					if err := ag.Steer(ctx, hint); err != nil {
						return cr, err
					}
				}
			}
		}
	}
	cr.EndedAt = h.now()
	return cr, nil
}

type eventSink struct {
	mu     sync.Mutex
	events []Event
	round  int
	on     func(Event, int)
}

func (s *eventSink) Emit(e Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	round, on := s.round, s.on
	s.mu.Unlock()
	if on != nil {
		on(e, round)
	}
}
func (s *eventSink) setRound(round int) { s.mu.Lock(); s.round = round; s.mu.Unlock() }

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

// safeError 把错误折成一个**可进公开结果**的分类串。
//
// 为什么只留类型名：err.Error() 可能带平台响应体、路径甚至凭据，而 RunResult.Err
// 会写进公开的 results/<runID>.json。真正的诊断信息留在调用方手里的 error 里。
func safeError(err error) string {
	if err == nil {
		return ""
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
