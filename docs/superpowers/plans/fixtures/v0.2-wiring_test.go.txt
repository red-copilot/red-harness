package harness_test

// 接线回归：核验实测的三个「静默降级」点。
//
// 这三个点不接线时**所有包的自测全绿**——因为它们各自都「实现正确」，只是没有
// 被任何东西调用。核验量化过后果：不接 Ingest 时，DAG 里除平台种子外零事实，
// 7 个阶段里只有前 4 个曾经可达，其余永远 pending；「DAG 驱动」静默退化成
// 「一条写死的阶段链跑满预算」。
//
// 所以这里钉的不是「函数存在」，而是**主循环真的调了它**。

import (
	"context"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// ── 接线探针：记录「谁被调了」 ──

type wireProbe struct {
	ingested  []harness.Event
	rounds    []int
	intents   []string
	intentRnd []int
	saves     int
	saveErr   error
}

func (p *wireProbe) Ingest(ev harness.Event, round int) {
	p.ingested = append(p.ingested, ev)
	p.rounds = append(p.rounds, round)
}

func (p *wireProbe) Save(string) error {
	p.saves++
	return p.saveErr
}

// wireAgent 每轮吐一个带输出的 tool_end，让 Ingest 有东西可吃。
type wireAgent struct{ n int }

func (a *wireAgent) Start(context.Context, harness.AgentStart) error { return nil }
func (a *wireAgent) Round(_ context.Context, _ string, emit func(harness.Event)) (harness.RoundResult, error) {
	a.n++
	emit(harness.Event{Kind: harness.EventToolStart, Tool: "bash", ToolCallID: "c1",
		Args: map[string]any{"command": "nmap -sV 10.0.0.9"}})
	emit(harness.Event{Kind: harness.EventToolEnd, Tool: "bash", ToolCallID: "c1",
		Output: "22/tcp open ssh\n80/tcp open http"})
	return harness.RoundResult{Turns: 1}, nil
}
func (a *wireAgent) Steer(context.Context, string) error { return nil }
func (a *wireAgent) Stats(context.Context) (harness.Stats, error) {
	return harness.Stats{}, nil
}
func (a *wireAgent) Close(context.Context) error { return nil }

type wireSched struct {
	max, n int
	last   *harness.IntentRef
}

func (s *wireSched) Next(context.Context, harness.Challenge, *harness.Outcome) *harness.IntentRef {
	if s.n >= s.max {
		return nil
	}
	s.n++
	s.last = &harness.IntentRef{ID: "intent-" + string(rune('A'+s.n-1)), Kind: "recon", Goal: "g"}
	return s.last
}
func (s *wireSched) Activate(*harness.IntentRef)                    {}
func (s *wireSched) Settle(*harness.IntentRef, harness.RoundResult) {}

type wireRenderer struct{}

func (wireRenderer) Render(context.Context, harness.Challenge, *harness.IntentRef, *harness.Outcome) string {
	return "p"
}

// TestWiring_IngestReceivesRoundScopedEvents 钉住「Ingest 每轮都被调、且轮号正确」。
//
// 轮号正确性不是形式主义：`dag.Scheduler.Settle` 靠轮次新鲜度重建「本轮产出了
// 什么」，轮号不同步会让所有意图判 failed 且**不报错**。
func TestWiring_IngestReceivesRoundScopedEvents(t *testing.T) {
	probe := &wireProbe{}
	sess := &harness.Session{
		Platform: &regPlatform{}, Agent: &wireAgent{},
		Scheduler: &wireSched{max: 3}, Renderer: wireRenderer{},
		Ingest: probe.Ingest,
		Budget: harness.Budget{MaxRounds: 10}, Now: time.Now,
	}
	if _, err := sess.Run(context.Background(), "wire-01"); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if len(probe.ingested) == 0 {
		t.Fatal("Ingest 从未被调用——DAG 会除平台种子外零事实，「DAG 驱动」退化成写死的阶段链")
	}
	// 每轮 2 个事件 × 3 轮 = 6
	if len(probe.ingested) != 6 {
		t.Errorf("每轮的每个事件都应进 Ingest，期望 6 个，实际 %d", len(probe.ingested))
	}
	seen := map[int]bool{}
	for _, r := range probe.rounds {
		seen[r] = true
	}
	for r := 1; r <= 3; r++ {
		if !seen[r] {
			t.Errorf("轮号 %d 没有出现在 Ingest 里（实际轮号集合 %v）—— "+
				"轮号不同步会让所有意图判 failed 且不报错", r, seen)
		}
	}
}

// TestWiring_IntentSinkFillsProvenanceAnchor 钉住「候选的 IntentID/Round 非空」。
//
// 不接 IntentSink 时 gate 的族别判定**依然正确**（所以不会报错），丢的是
// 「这个答案是哪条路试出来的」这条审计链——洗白路径的来源追溯全靠它。
func TestWiring_IntentSinkFillsProvenanceAnchor(t *testing.T) {
	probe := &wireProbe{}
	sink := func(id string, round int) {
		probe.intents = append(probe.intents, id)
		probe.intentRnd = append(probe.intentRnd, round)
	}
	sess := &harness.Session{
		Platform: &regPlatform{}, Agent: &wireAgent{},
		Scheduler: &wireSched{max: 2}, Renderer: wireRenderer{},
		IntentSink: sink,
		Budget:     harness.Budget{MaxRounds: 10}, Now: time.Now,
	}
	if _, err := sess.Run(context.Background(), "wire-02"); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if len(probe.intents) != 2 {
		t.Fatalf("每个意图执行前都应通知 gate，期望 2 次，实际 %d（%v）", len(probe.intents), probe.intents)
	}
	if probe.intents[0] != "intent-A" || probe.intents[1] != "intent-B" {
		t.Errorf("意图 id 应原样传递，实际 %v", probe.intents)
	}
	if probe.intentRnd[0] != 1 || probe.intentRnd[1] != 2 {
		t.Errorf("轮号应从 1 起递增，实际 %v", probe.intentRnd)
	}
}

// TestWiring_SaverCalledEachRound 钉住「每轮落盘一次」。
//
// 断点续跑的粒度就是轮：少落一次，那一轮的进展在进程挂掉后就丢了。
func TestWiring_SaverCalledEachRound(t *testing.T) {
	probe := &wireProbe{}
	sess := &harness.Session{
		Platform: &regPlatform{}, Agent: &wireAgent{},
		Scheduler: &wireSched{max: 4}, Renderer: wireRenderer{},
		Saver: probe, GraphPath: "/tmp/unused-graph.json",
		Budget: harness.Budget{MaxRounds: 10}, Now: time.Now,
	}
	if _, err := sess.Run(context.Background(), "wire-03"); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if probe.saves != 4 {
		t.Errorf("4 轮应落盘 4 次，实际 %d", probe.saves)
	}
}

// TestWiring_SaveFailureIsNotSilent 钉住「落盘失败必须可见」。
//
// 静默的落盘失败比不落盘更糟：调用方以为进展保住了。
func TestWiring_SaveFailureIsNotSilent(t *testing.T) {
	probe := &wireProbe{saveErr: context.DeadlineExceeded}
	var errs []string
	sess := &harness.Session{
		Platform: &regPlatform{}, Agent: &wireAgent{},
		Scheduler: &wireSched{max: 2}, Renderer: wireRenderer{},
		Saver: probe, GraphPath: "/tmp/unused-graph.json",
		OnEvent: func(ev harness.Event) {
			if ev.Kind == harness.EventError {
				errs = append(errs, ev.Err)
			}
		},
		Budget: harness.Budget{MaxRounds: 10}, Now: time.Now,
	}
	if _, err := sess.Run(context.Background(), "wire-04"); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if len(errs) == 0 {
		t.Fatal("落盘失败被静默吞掉了——调用方会以为进展已保住")
	}
}
