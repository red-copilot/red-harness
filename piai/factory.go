package piai

import (
	"context"
	"errors"

	harness "github.com/red-copilot/red-harness"
)

// Factory is the v0.4 pi adapter. A sandbox session is mandatory; the
// factory never resolves or starts a host-side pi binary on this path.
type Factory struct {
	BinPath      string
	EnvFile      string
	VersionRange VersionRange
}

var _ harness.AgentFactory = (*Factory)(nil)

func (f *Factory) New(spec harness.AgentSpec, session harness.SandboxSession, sink harness.EventSink) (harness.Agent, error) {
	if session == nil {
		return nil, errors.New("piai: sandbox session 不能为空")
	}
	if sink == nil {
		return nil, errors.New("piai: event sink 不能为空")
	}
	if f == nil {
		f = &Factory{}
	}
	a := &Agent{Session: session, BinPath: f.BinPath, EnvFile: f.EnvFile, VersionRange: f.VersionRange,
		Provider: spec.Provider, Model: spec.Model, Thinking: spec.Thinking, Extensions: spec.Extensions,
		Approve: spec.Approve, SessionDir: spec.SessionDir, HomeDir: spec.HomeDir}
	a.OnRawEvent = func(env Envelope, raw []byte) { a.emitNormalized(env, raw, sink) }
	return &adapter{agent: a, sink: sink}, nil
}

type adapter struct {
	agent *Agent
	sink  harness.EventSink
}

func (a *adapter) Start(ctx context.Context, req harness.AgentStart) error {
	return a.agent.Start(ctx, req)
}
func (a *adapter) Round(ctx context.Context, req harness.RoundRequest) (harness.RoundResult, error) {
	return a.agent.Round(ctx, req.Prompt, a.sink.Emit)
}
func (a *adapter) Steer(ctx context.Context, msg string) error      { return a.agent.Steer(ctx, msg) }
func (a *adapter) Stats(ctx context.Context) (harness.Stats, error) { return a.agent.Stats(ctx) }
func (a *adapter) Close(ctx context.Context) error                  { return a.agent.Close(ctx) }

// emitNormalized intentionally handles only events produced by the existing
// protocol decoder. The sink receives the canonical Event, never raw frames.
func (a *Agent) emitNormalized(env Envelope, raw []byte, sink harness.EventSink) {
	// OnRawEvent is also used by callers that need the full frame. The regular
	// Round callback is the authoritative normalized event path, so this hook is
	// intentionally empty to avoid duplicate events.
	_, _ = env, raw
	_ = sink
}
