package piai

import (
	"context"
	"fmt"

	harness "github.com/red-copilot/red-harness"
)

// 看门狗：从「猜」变「问」。
//
// 前身用「480s 无 stdout 字节即判卡死」——那是猜，代价是长思考被误杀（真跑时
// 一次深思考可能就 5–8 分钟）。RPC 下可以**问**：无事件超过 StallTimeout 就发
// 一次 get_state，看 isStreaming / messageCount / pendingMessageCount 是否推进。
// M0 实测长思考期间 get_state 仍能往返（12/12，最坏 414ms），所以这条路可用。
//
// 判定规则（设计 §二）：
//   - 探活成功且状态**有推进**（messageCount / pendingMessageCount 变大，或
//     isStreaming 翻转）⇒ 不是卡死，调用方重新计时。
//   - 探活成功但状态**完全没变** ⇒ 只是「安静」，**不**判死：messageCount 只在
//     一条消息完成时才涨，长思考期间它天然不动。把它当卡死信号，等于把前身
//     「无输出即卡死」的错误换个名字重犯。
//   - 连续两次**往返失败或超时** ⇒ wedged ⇒ 调用方整组 kill + 重启，本轮 Err
//     置非空。
//
// 返回 (progressed, wedged)。fails 是跨次调用的连续失败计数，由调用方持有。
func (a *Agent) watchdog(ctx context.Context, emit func(harness.Event),
	fails *int, prev *SessionState, havePrev *bool) (progressed bool, wedged bool) {

	st, err := a.probe(ctx)
	if err != nil {
		*fails++
		emit(harness.Event{Kind: harness.EventError,
			Err: fmt.Sprintf("看门狗探活失败（%d/2）: %v", *fails, err)})
		return false, *fails >= 2
	}
	*fails = 0

	if !*havePrev {
		*prev = st
		*havePrev = true
		// 第一次探活只看它是否还在 streaming：还在跑就说明活着。
		return st.IsStreaming, false
	}
	prog := st.MessageCount != prev.MessageCount || st.PendingMessageCount != prev.PendingMessageCount
	if !prog && st.IsStreaming != prev.IsStreaming {
		prog = true
	}
	emit(harness.Event{Kind: harness.EventThinking,
		Text: fmt.Sprintf("看门狗：messages %d→%d, pending %d→%d, streaming=%v, compacting=%v",
			prev.MessageCount, st.MessageCount, prev.PendingMessageCount, st.PendingMessageCount,
			st.IsStreaming, st.IsCompacting)})
	*prev = st
	return prog, false
}
