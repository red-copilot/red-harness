// Package scenario contains deliberately small, platform-facing adapters.
// It owns lifecycle reconciliation; it does not execute tools or expand a
// target beyond the addresses returned by the authorized platform.
package scenario

import (
	"context"
	"errors"
	"fmt"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// TSecBench 是真实平台的场景适配器。它只做三件事：把平台写操作收进六方法、
// 把平台的判定映射进根包模型、把**平台给出的地址**原样交给引擎当白名单。
// 它不执行工具、不推断可达范围。
type TSecBench struct {
	Platform harness.Platform
	// PollEvery 是 Prepare 轮询起题状态的间隔；<=0 时取 500ms。
	PollEvery time.Duration
	// PollLimit 是 Prepare 等待容器就绪的总时限；<=0 时取 2 分钟。
	PollLimit time.Duration
}

var _ harness.Scenario = (*TSecBench)(nil)

// Discover 列出可选目标。spec.Targets 为空 ⇒ 全部未完成的题；非空 ⇒ 按 code
// 精确匹配，匹配不到即 KindScope（目标不在授权集合里，重试无意义）。
func (s *TSecBench) Discover(ctx context.Context, spec harness.RunSpec) ([]harness.Challenge, error) {
	if s == nil || s.Platform == nil {
		return nil, harness.Ef(harness.KindConfig, "scenario.discover", "TSecBench platform 未设置", nil)
	}
	items, err := s.Platform.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(spec.Targets) == 0 {
		// 必须新建切片而不是 items[:0]：那是**原地复用平台返回的底层数组**，
		// 会把调用方（bridge 客户端）交回来的缓冲区写坏。平台实现可能缓存并
		// 复用那份切片，改写它就是让下一次 List 的返回值凭空少几道题。
		out := make([]harness.Challenge, 0, len(items))
		for _, ch := range items {
			if !ch.Done() {
				out = append(out, ch)
			}
		}
		return out, nil
	}
	byCode := map[string]harness.Challenge{}
	for _, ch := range items {
		byCode[ch.Code] = ch
	}
	out := make([]harness.Challenge, 0, len(spec.Targets))
	for _, code := range spec.Targets {
		ch, ok := byCode[code]
		if !ok {
			return nil, harness.Ef(harness.KindScope, "scenario.discover", "目标不在平台题目集合中", fmt.Errorf("challenge=%s", code))
		}
		// 指名要求一道已完成的题**不是**越权：它是幂等重跑的正常输入。
		// 这里只跳过，不报错——报错会让「补跑一道已经做掉的题」变成硬失败。
		if !ch.Done() {
			out = append(out, ch)
		}
	}
	return out, nil
}

// Prepare 起题并等到容器可用，返回引擎视角的目标。
//
// **它是目标白名单的唯一来源**（v0.4 硬规矩）：executor 的 session 直接读
// 返回的 Target.Addrs 渲染 iptables，调用方无权扩大。所以这里只转发平台
// 给出的地址，绝不拼接、绝不推断、绝不回落到题目元数据里的旧地址。
//
// 起题是异步的：Start 返回时容器还是 pending，Addrs 为空，必须轮询 List 直到
// available（地址非空）为止——把 pending 期间的题当成「没地址可打」直接返回，
// 会让 agent 面对一个空白名单，出站全被拒绝却看不出原因。
func (s *TSecBench) Prepare(ctx context.Context, ch harness.Challenge) (harness.Target, error) {
	if s == nil || s.Platform == nil {
		return harness.Target{}, harness.Ef(harness.KindConfig, "scenario.prepare", "TSecBench platform 未设置", nil)
	}
	if _, err := s.Platform.Start(ctx, ch.Code); err != nil {
		return harness.Target{}, classifyCtx(ctx, "scenario.prepare", "起题失败", err)
	}
	interval, limit := s.PollEvery, s.PollLimit
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	if limit <= 0 {
		limit = 2 * time.Minute
	}
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		items, err := s.Platform.List(ctx)
		if err != nil {
			return harness.Target{}, classifyCtx(ctx, "scenario.prepare", "起题后查询题目状态失败", err)
		}
		for _, item := range items {
			if item.Code == ch.Code && len(item.Addrs) > 0 {
				return harness.Target{Code: ch.Code, Addrs: append([]string(nil), item.Addrs...), Network: "tcp"}, nil
			}
		}
		select {
		case <-ctx.Done():
			return harness.Target{}, harness.Ef(harness.KindCancelled, "scenario.prepare", "起题被取消", ctx.Err())
		case <-deadline.C:
			// 期限到了但 ctx 还活着：这是平台侧没能在时限内让容器可用，
			// 不是用户取消。两者必须分开——前者可重试，后者不该被当成平台故障。
			return harness.Target{}, harness.Ef(harness.KindPlatform, "scenario.prepare", "题目容器在期限内未就绪", context.DeadlineExceeded)
		case <-tick.C:
		}
	}
}

// Hint 请求平台提示。平台没有提示时返回空串且不报错（契约如此）：
// 「没有提示」是正常状态，不是失败。
func (s *TSecBench) Hint(ctx context.Context, ch harness.Challenge) (string, error) {
	r, err := s.Platform.Hint(ctx, ch.Code)
	if err != nil {
		return "", err
	}
	return r.Hint, nil
}

// Evaluate 提交一个候选，把平台判定映射进根包模型。
//
// 映射的两条硬规矩：
//   - Duplicate（平台幂等命中）**等价于已确认**：Accepted=true。把它当失败会
//     让「同一答案重提」看起来像判错，进而把已确认的 flag 记成未解。
//   - 但 Duplicate 的 Progress=false：这次提交没有让平台侧前进。引擎用它做
//     停滞检测，幂等命中若也算进展，卡住的题会永远看不到停滞。
//
// Score 直接取平台的 Awarded（平台语义的累计/本次得分，见 harness.SubmitResult），
// 不在这里二次计算——分数口径只能有一个真源。
func (s *TSecBench) Evaluate(ctx context.Context, ch harness.Challenge, flag string) (harness.Evaluation, error) {
	r, err := s.Platform.Submit(ctx, ch.Code, flag)
	if err != nil {
		return harness.Evaluation{}, err
	}
	return harness.Evaluation{Accepted: r.Correct || r.Duplicate, Progress: r.Correct && !r.Duplicate,
		Completed: r.TotalFlagCount > 0 && r.CorrectFlagCount >= r.TotalFlagCount,
		Score:     r.Awarded, Message: r.Message}, nil
}

// Reconcile 返回**平台权威**的进度。
//
// 为什么必须回平台问一次，而不是用本地账本推算：提交的写操作可能超时但实际
// 已生效（引擎在 Evaluate 出错后会先 Reconcile 再决定要不要重发）。本地推算
// 在这种时候恰好是最不可靠的那个来源。平台侧查不到这道题 ⇒ KindPlatform，
// 因为「题目在平台当前进度里消失了」不是调用方能修的错。
func (s *TSecBench) Reconcile(ctx context.Context, ch harness.Challenge) (harness.Objective, error) {
	items, err := s.Platform.List(ctx)
	if err != nil {
		return harness.Objective{}, err
	}
	for _, item := range items {
		if item.Code == ch.Code {
			// Completed 用 Challenge.Done()（要求 FlagCount > 0）：分母未知时
			// 不得宣称完成，这与根包契约对「FlagCount 0 = 未知」的定义一致。
			return harness.Objective{Kind: "flag_count", Want: item.FlagCount, Got: item.Solved, Completed: item.Done()}, nil
		}
	}
	return harness.Objective{}, harness.Ef(harness.KindPlatform, "scenario.reconcile", "题目不在平台当前进度中", nil)
}

// Cleanup 关题。
//
// 错误**原样上抛**，不重分类、不吞掉：调用方（引擎）把 Cleanup 的错误单独
// 记录，绝不用它覆盖主要终止原因，而平台适配器给出的分类（bridge 的 Kind）
// 比这里能猜出来的更精确。上抛而不是吞掉，是因为「关题失败」意味着容器可能
// 还在跑、还在计费。
func (s *TSecBench) Cleanup(ctx context.Context, ch harness.Challenge) error {
	_, err := s.Platform.Close(ctx, ch.Code)
	return err
}

// classifyCtx 在 ctx 已经结束的情况下，把平台调用返回的错误改判成取消/超时。
//
// 为什么需要它：ctx 取消后，平台调用返回的往往是底层传输错误（"context
// canceled" / 半截响应），分类串会被引擎记成「平台故障」。而根包契约把
// KindCancelled 单列出来，正是为了区分「用户按了 Ctrl-C」与「平台挂了」——
// 报告要能回答「为什么停」。Prepare 是唯一需要它的方法，因为只有它会在
// ctx 结束后继续向平台发请求（轮询循环），其余方法的 ctx 结束即整体结束。
func classifyCtx(ctx context.Context, op, msg string, err error) error {
	if ctx == nil {
		return err
	}
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return harness.Ef(harness.KindCancelled, op, msg+"（ctx 已取消）", err)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return harness.Ef(harness.KindPlatform, op, msg+"（ctx 已超时）", err)
	}
	return err
}
