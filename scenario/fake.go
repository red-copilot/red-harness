package scenario

import (
	"context"
	"sync"

	harness "github.com/red-copilot/red-harness"
)

// Fake is a deterministic offline scenario for harness wiring and regression
// tests. It only accepts Answers declared by the test author.
type Fake struct {
	Challenges []harness.Challenge
	Answers    map[string]map[string]bool
	mu         sync.Mutex
	started    map[string]bool
	confirmed  map[string]map[string]bool
}

func (f *Fake) Discover(context.Context, harness.RunSpec) ([]harness.Challenge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]harness.Challenge(nil), f.Challenges...), nil
}

func (f *Fake) Prepare(_ context.Context, ch harness.Challenge) (harness.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started == nil {
		f.started = map[string]bool{}
	}
	f.started[ch.Code] = true
	return harness.Target{Code: ch.Code, Addrs: append([]string(nil), ch.Addrs...), Network: "tcp"}, nil
}

func (f *Fake) Hint(context.Context, harness.Challenge) (string, error) { return "", nil }

func (f *Fake) Evaluate(_ context.Context, ch harness.Challenge, answer string) (harness.Evaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Answers[ch.Code][answer] {
		return harness.Evaluation{Message: "rejected"}, nil
	}
	if f.confirmed == nil {
		f.confirmed = map[string]map[string]bool{}
	}
	if f.confirmed[ch.Code] == nil {
		f.confirmed[ch.Code] = map[string]bool{}
	}
	dup := f.confirmed[ch.Code][answer]
	f.confirmed[ch.Code][answer] = true
	want := ch.FlagCount
	got := len(f.confirmed[ch.Code])
	return harness.Evaluation{Accepted: true, Progress: !dup, Completed: want > 0 && got >= want, Score: 1, Message: "accepted"}, nil
}

func (f *Fake) Reconcile(_ context.Context, ch harness.Challenge) (harness.Objective, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	got := len(f.confirmed[ch.Code])
	return harness.Objective{Kind: "flag_count", Want: ch.FlagCount, Got: got, Completed: ch.FlagCount > 0 && got >= ch.FlagCount}, nil
}

func (f *Fake) Cleanup(_ context.Context, ch harness.Challenge) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started != nil {
		delete(f.started, ch.Code)
	}
	return nil
}
