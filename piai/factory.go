package piai

import (
	"context"
	"errors"

	harness "github.com/red-copilot/red-harness"
)

// Factory 是 v0.4 的 pi 适配工厂。sandbox session 是**强制**的：这条路径上
// 既不会去宿主上发现 pi 二进制，也不会在宿主上起进程（v0.4 硬规矩）。
type Factory struct {
	// BinPath 是**容器内**的 pi 名字或路径。为空时用 "pi"（容器 PATH 解析）。
	// 这里填宿主绝对路径是配置错误：那个路径在容器里不存在。
	BinPath      string
	EnvFile      string
	VersionRange VersionRange
}

var _ harness.AgentFactory = (*Factory)(nil)

func (f *Factory) New(spec harness.AgentSpec, session harness.SandboxSession, sink harness.EventSink) (harness.Agent, error) {
	if session == nil {
		return nil, errors.New("piai: sandbox session 不能为空（v0.4 不允许宿主进程启动路径）")
	}
	if sink == nil {
		return nil, errors.New("piai: event sink 不能为空（事件出口缺失会让轮次结果不可解释）")
	}
	if f == nil {
		f = &Factory{}
	}
	a := &Agent{Session: session, BinPath: f.BinPath, EnvFile: f.EnvFile, VersionRange: f.VersionRange,
		Provider: spec.Provider, Model: spec.Model, Thinking: spec.Thinking, Extensions: spec.Extensions,
		Approve: spec.Approve, SessionDir: spec.SessionDir, HomeDir: spec.HomeDir}
	return &adapter{agent: a, sink: sink}, nil
}

type adapter struct {
	agent *Agent
	sink  harness.EventSink
}

func (a *adapter) Start(ctx context.Context, req harness.AgentStart) error {
	return a.agent.Start(ctx, req)
}

// Round 把 sink 作为 emit 回调交给 Agent.Round。
//
// 事件只有**这一条**出口：Agent.handleEvent 在 Round 协程里把规范化后的
// harness.Event 交给 emit，emit 直接就是 sink.Emit。所以「事件到达 sink」这件事
// 与 Agent 内部的帧分发是同一段代码路径，不存在第二条会重复发送的旁路——
// 也正因为如此，Factory.New **不**去挂 a.OnRawEvent。
//
// 为什么不能把 sink 也接到 OnRawEvent 上（曾经那版就是这么写的，而且挂了一个
// 什么都不做的 emitNormalized）：OnRawEvent 在 **proc 的 reader 协程**里被调用，
// 它拿到的是**原始帧**而不是规范化事件，而且它每轮都会触发（Round 之外也在
// 触发：Start 的握手、Steer 的应答）。在那里推 sink 会同时踩三件事：
//   - 重复：同一帧经 handleEvent 再推一次；
//   - 时序错乱：reader 协程的推送会插在 Round 的事件序列里，而 Round 是唯一
//     保证 emit 单线程、按序的地方（handleEvent 的注释就是这么写的）；
//   - 背压方向反了：reader 一停，pi 的 stdout 管道就会写满 ⇒ 死锁。
//
// 所以 OnRawEvent 保持「取证/调试钩子」的语义（默认 nil，调用方自己挂），
// 事件出口只有 sink 一条。
func (a *adapter) Round(ctx context.Context, req harness.RoundRequest) (harness.RoundResult, error) {
	return a.agent.Round(ctx, req.Prompt, a.sink.Emit)
}
func (a *adapter) Steer(ctx context.Context, msg string) error      { return a.agent.Steer(ctx, msg) }
func (a *adapter) Stats(ctx context.Context) (harness.Stats, error) { return a.agent.Stats(ctx) }
func (a *adapter) Close(ctx context.Context) error                  { return a.agent.Close(ctx) }

var _ harness.Agent = (*adapter)(nil)

// 编译期把「adapter 真的满足 harness.Agent」钉住：v0.3 时代 piai 的 Round 签名
// 与契约不兼容却编译通过（没有断言），这类漂移只能在集成时才暴露。
