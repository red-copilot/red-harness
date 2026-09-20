package harness_test

// 根包轮循环的回归测试。与核验 agent 的临时文件（zzz_verify_*.go）刻意使用
// 不同的 fake 类型名（`reg*` 前缀），避免同名冲突——那批文件是核验产物，
// 不属于交付物，随时可能被删除。

import (
	"context"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/gate"
)

// ── 回归用 fake ──

type regPlatform struct {
	challenges []harness.Challenge
	submitRes  harness.SubmitResult
	submits    int
	closed     int
}

func (p *regPlatform) List(context.Context) ([]harness.Challenge, error) { return p.challenges, nil }
func (p *regPlatform) Start(_ context.Context, code string) (harness.StartResult, error) {
	return harness.StartResult{Code: code, Addrs: []string{"10.0.0.1:80"}, Description: "提交 flag{...}"}, nil
}
func (p *regPlatform) Hint(context.Context, string) (harness.HintResult, error) {
	return harness.HintResult{}, nil
}
func (p *regPlatform) Submit(context.Context, string, string) (harness.SubmitResult, error) {
	p.submits++
	return p.submitRes, nil
}
func (p *regPlatform) Close(context.Context, string) (harness.CloseResult, error) {
	p.closed++
	return harness.CloseResult{Closed: true}, nil
}

type regAgent struct {
	// perRound 决定第 n 轮吐出什么事件。返回的 RoundResult 由调用方给。
	perRound func(round int, emit func(harness.Event)) harness.RoundResult
	rounds   int
}

func (a *regAgent) Start(context.Context, harness.AgentStart) error { return nil }
func (a *regAgent) Round(_ context.Context, _ string, emit func(harness.Event)) (harness.RoundResult, error) {
	a.rounds++
	return a.perRound(a.rounds, emit), nil
}
func (a *regAgent) Steer(context.Context, string) error { return nil }
func (a *regAgent) Stats(context.Context) (harness.Stats, error) {
	return harness.Stats{Turns: a.rounds}, nil
}
func (a *regAgent) Close(context.Context) error { return nil }

// regSched 是一个「想要跑很多轮」的调度器：只要还有额度就给意图。
// 它的作用是让「通关立即终止」成为**唯一**能让循环停下来的东西——
// 若那条检查失效，测试会看到 rounds 远大于 1。
type regSched struct {
	max   int
	n     int
	last  *harness.IntentRef
	stops int
}

func (s *regSched) Next(context.Context, harness.Challenge, *harness.Outcome) *harness.IntentRef {
	if s.n >= s.max {
		return nil
	}
	s.n++
	s.last = &harness.IntentRef{ID: "i" + string(rune('0'+s.n)), Kind: "recon", Goal: "g"}
	return s.last
}
func (s *regSched) Activate(*harness.IntentRef)                    {}
func (s *regSched) Settle(*harness.IntentRef, harness.RoundResult) { s.stops++ }

type regRenderer struct{}

func (regRenderer) Render(context.Context, harness.Challenge, *harness.IntentRef, *harness.Outcome) string {
	return "prompt"
}

// ── 回归 1：通关立即终止（不依赖 FlagCount） ──

// 这条测试守的是一个**真实发生过两次**的缺陷类别：护栏写成了死代码。
//
// 旧契约里 `Challenge.Description` 是死路径（StartResult 不带该字段，分支永不
// 触发）；我在重写轮循环时又埋了同一个：`StartResult` 不携带 FlagCount，所以
// `ch.FlagCount > 0` 恒假，「通关立即终止」永不生效——平台已经确认 2/2 了，
// 循环还在跑。
//
// 所以这里**刻意不给 Session.Challenge、也不让 Platform.List 返回任何东西**，
// 只让 Submit 回权威进度。若停条件只认 FlagCount，这条测试会跑满 maxRounds。
func TestRegression_SolvedStopsWithoutFlagCount(t *testing.T) {
	p := &regPlatform{
		// 注意：List 返回 nil —— 调用方也拿不到 FlagCount。
		submitRes: harness.SubmitResult{Correct: true, CorrectFlagCount: 2, TotalFlagCount: 2},
	}
	a := &regAgent{perRound: func(_ int, emit func(harness.Event)) harness.RoundResult {
		emit(harness.Event{
			Kind: harness.EventToolEnd, Tool: "bash", ToolCallID: "c1",
			Args:   map[string]any{"command": "curl -s http://10.0.0.1/flag"},
			Output: "flag{solved_immediately}\n",
		})
		return harness.RoundResult{Turns: 1}
	}}
	sched := &regSched{max: 5}

	sess := &harness.Session{
		Platform: p, Agent: a, Gate: gate.NewGate("提交 flag{...}"), Ledger: gate.NewLedger(),
		Scheduler: sched, Renderer: regRenderer{},
		Budget: harness.Budget{MaxRounds: 10}, Submit: true, Now: time.Now,
	}
	out, err := sess.Run(context.Background(), "web-01")
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if out.Rounds != 1 {
		t.Errorf("平台已确认 2/2，循环应跑 1 轮就停，实际 %d 轮（Reason=%q）—— "+
			"「通关立即终止」又变成死代码了", out.Rounds, out.Reason)
	}
	if out.Reason != harness.ReasonSolved {
		t.Errorf("Reason 应为 %q，实际 %q", harness.ReasonSolved, out.Reason)
	}
	if out.ProgressConfirmed != 2 || out.ProgressTotal != 2 {
		t.Errorf("平台权威进度应被记录，实际 %d/%d", out.ProgressConfirmed, out.ProgressTotal)
	}
	if p.closed == 0 {
		t.Error("题目容器应被关闭")
	}
}

// ── 回归 2：零回合超时不得被误判成 provider 故障 ──

// 这条守的是我自己写错过一次的分支顺序：provider 护栏
// `turns == 0 && err != ""` 排在 ctx 判断之前，于是一次**零回合的墙钟超时**
// （err 是 context.DeadlineExceeded）会被记成「模型服务挂了」。
type regCtxAgent struct{}

func (regCtxAgent) Start(context.Context, harness.AgentStart) error { return nil }
func (regCtxAgent) Round(ctx context.Context, _ string, _ func(harness.Event)) (harness.RoundResult, error) {
	<-ctx.Done()
	return harness.RoundResult{Reason: harness.ReasonStopped}, ctx.Err()
}
func (regCtxAgent) Steer(context.Context, string) error { return nil }
func (regCtxAgent) Stats(context.Context) (harness.Stats, error) {
	return harness.Stats{}, nil
}
func (regCtxAgent) Close(context.Context) error { return nil }

func TestRegression_ZeroTurnTimeoutIsNotProviderFailure(t *testing.T) {
	p := &regPlatform{}
	sched := &regSched{max: 5}
	sess := &harness.Session{
		Platform: p, Agent: regCtxAgent{}, Gate: gate.NewGate("提交 flag{...}"),
		Ledger: gate.NewLedger(), Scheduler: sched, Renderer: regRenderer{},
		Budget: harness.Budget{MaxRounds: 10}, Submit: true, Now: time.Now,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	out, err := sess.Run(ctx, "web-02")
	if err != nil {
		t.Fatalf("ctx 超时不应让 Run 返回 error: %v", err)
	}
	if out.Reason != harness.ReasonStopped {
		t.Errorf("零回合超时的 Reason 应为 %q，实际 %q（Err=%q）—— "+
			"这是分支顺序错误：provider 护栏抢在 ctx 之前命中了",
			harness.ReasonStopped, out.Reason, out.Err)
	}
	if out.Reason == harness.ReasonProviderFailure {
		t.Error("一次正常超时被记成了 provider 故障")
	}
}

// ── 回归 3：本轮独立的 ProviderError 判据 ──

// `Err` 是「本轮任何异常」的混合字段（进程死亡、extension 错误、provider 错误
// 都往里塞），所以判 provider 故障必须有一个干净的字段。这条测试确认
// 根包用的确实是 ProviderError 而不是 Err。
type regProviderErrAgent struct{ round int }

func (a *regProviderErrAgent) Start(context.Context, harness.AgentStart) error { return nil }
func (a *regProviderErrAgent) Round(context.Context, string, func(harness.Event)) (harness.RoundResult, error) {
	a.round = 1
	// 关键：Turns > 0，所以 `turns == 0 && err != ""` 那条护栏**不会**触发。
	// 只有 ProviderError 能抓住它。
	return harness.RoundResult{
		Turns: 3, Err: "pi_provider_error: API key is invalid",
		ProviderError: "pi_provider_error: API key is invalid",
	}, nil
}
func (a *regProviderErrAgent) Steer(context.Context, string) error { return nil }
func (a *regProviderErrAgent) Stats(context.Context) (harness.Stats, error) {
	return harness.Stats{}, nil
}
func (a *regProviderErrAgent) Close(context.Context) error { return nil }

func TestRegression_ProviderErrorCaughtEvenWithTurns(t *testing.T) {
	p := &regPlatform{}
	sched := &regSched{max: 5}
	sess := &harness.Session{
		Platform: p, Agent: &regProviderErrAgent{}, Gate: gate.NewGate("提交 flag{...}"),
		Ledger: gate.NewLedger(), Scheduler: sched, Renderer: regRenderer{},
		Budget: harness.Budget{MaxRounds: 10}, Submit: true, Now: time.Now,
	}
	out, err := sess.Run(context.Background(), "web-03")
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if out.Reason != harness.ReasonProviderFailure {
		t.Errorf("第 2 轮之后的 provider 故障必须被识别（Turns=%d 时 0 回合护栏不生效），"+
			"实际 Reason=%q", 3, out.Reason)
	}
}
