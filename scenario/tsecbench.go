// Package scenario contains deliberately small, platform-facing adapters.
// It owns lifecycle reconciliation; it does not execute tools or expand a
// target beyond the addresses returned by the authorized platform.
package scenario

import (
	"context"
	"fmt"
	"time"

	harness "github.com/red-copilot/red-harness"
)

type TSecBench struct {
	Platform  harness.Platform
	PollEvery time.Duration
	PollLimit time.Duration
}

var _ harness.Scenario = (*TSecBench)(nil)

func (s *TSecBench) Discover(ctx context.Context, spec harness.RunSpec) ([]harness.Challenge, error) {
	if s == nil || s.Platform == nil {
		return nil, harness.Ef(harness.KindConfig, "scenario.discover", "TSecBench platform 未设置", nil)
	}
	items, err := s.Platform.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(spec.Targets) == 0 {
		out := items[:0]
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
		if !ch.Done() {
			out = append(out, ch)
		}
	}
	return out, nil
}

func (s *TSecBench) Prepare(ctx context.Context, ch harness.Challenge) (harness.Target, error) {
	if _, err := s.Platform.Start(ctx, ch.Code); err != nil {
		return harness.Target{}, err
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
			return harness.Target{}, err
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
			return harness.Target{}, harness.Ef(harness.KindPlatform, "scenario.prepare", "题目容器在期限内未就绪", context.DeadlineExceeded)
		case <-tick.C:
		}
	}
}

func (s *TSecBench) Hint(ctx context.Context, ch harness.Challenge) (string, error) {
	r, err := s.Platform.Hint(ctx, ch.Code)
	if err != nil {
		return "", err
	}
	return r.Hint, nil
}

func (s *TSecBench) Evaluate(ctx context.Context, ch harness.Challenge, flag string) (harness.Evaluation, error) {
	r, err := s.Platform.Submit(ctx, ch.Code, flag)
	if err != nil {
		return harness.Evaluation{}, err
	}
	return harness.Evaluation{Accepted: r.Correct || r.Duplicate, Progress: r.Correct && !r.Duplicate,
		Completed: r.TotalFlagCount > 0 && r.CorrectFlagCount >= r.TotalFlagCount,
		Score:     r.Awarded, Message: r.Message}, nil
}

func (s *TSecBench) Reconcile(ctx context.Context, ch harness.Challenge) (harness.Objective, error) {
	items, err := s.Platform.List(ctx)
	if err != nil {
		return harness.Objective{}, err
	}
	for _, item := range items {
		if item.Code == ch.Code {
			return harness.Objective{Kind: "flag_count", Want: item.FlagCount, Got: item.Solved, Completed: item.Done()}, nil
		}
	}
	return harness.Objective{}, harness.Ef(harness.KindPlatform, "scenario.reconcile", "题目不在平台当前进度中", nil)
}

func (s *TSecBench) Cleanup(ctx context.Context, ch harness.Challenge) error {
	_, err := s.Platform.Close(ctx, ch.Code)
	return err
}
