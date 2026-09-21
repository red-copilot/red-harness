package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	harness "github.com/red-copilot/red-harness"
)

const resultsDirName = "results"

// ResultFileStore persists only public aggregate metrics. It never marshals
// OutcomeView, Candidate or Flags; those are return/private values.
type ResultFileStore struct{ root string }

var _ harness.ResultStore = (*ResultFileStore)(nil)

func NewResultStore(dir string) (*ResultFileStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, harness.Ef(harness.KindConfig, "resultstore.new", "结果目录为空", nil)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, harness.Ef(harness.KindConfig, "resultstore.new", "解析结果目录失败", err)
	}
	if err := mkdirAllPrivate(filepath.Join(abs, resultsDirName), dirPerm); err != nil {
		return nil, harness.Ef(harness.KindPersistence, "resultstore.new", "创建结果目录失败", err)
	}
	return &ResultFileStore{root: filepath.Join(abs, resultsDirName)}, nil
}

type publicResult struct {
	RunID         string            `json:"runId"`
	Scenario      string            `json:"scenario"`
	ProfileDigest string            `json:"profileDigest,omitempty"`
	Model         string            `json:"model,omitempty"`
	StartedAt     time.Time         `json:"startedAt"`
	EndedAt       time.Time         `json:"endedAt"`
	Completed     bool              `json:"completed"`
	Reason        string            `json:"reason,omitempty"`
	Err           string            `json:"errorClass,omitempty"`
	Challenges    []publicChallenge `json:"challenges,omitempty"`
}
type publicChallenge struct {
	Code              string    `json:"code"`
	Category          string    `json:"category,omitempty"`
	StartedAt         time.Time `json:"startedAt"`
	EndedAt           time.Time `json:"endedAt"`
	Reason            string    `json:"reason,omitempty"`
	Submitted         int       `json:"submitted"`
	ProgressConfirmed int       `json:"progressConfirmed"`
	ProgressTotal     int       `json:"progressTotal"`
	Rounds            int       `json:"rounds"`
	HintUsed          int       `json:"hintUsed"`
	DurationSeconds   float64   `json:"durationSeconds"`
}

func toPublic(r harness.RunResult) publicResult {
	errClass := ""
	if r.Err != "" {
		errClass = "run_error"
	}
	p := publicResult{RunID: string(r.RunID), Scenario: r.Scenario, ProfileDigest: r.ProfileDigest,
		Model: r.Model, StartedAt: r.StartedAt, EndedAt: r.EndedAt, Completed: r.Completed,
		Reason: r.Reason, Err: errClass}
	for _, c := range r.Challenges {
		p.Challenges = append(p.Challenges, publicChallenge{Code: c.Challenge.Code,
			Category: c.Challenge.Category, StartedAt: c.StartedAt, EndedAt: c.EndedAt,
			Reason: c.Outcome.Reason, Submitted: c.Outcome.Submitted,
			ProgressConfirmed: c.Outcome.ProgressConfirmed, ProgressTotal: c.Outcome.ProgressTotal,
			Rounds: c.Outcome.Rounds, HintUsed: c.Outcome.HintUsed,
			DurationSeconds: c.Outcome.Duration().Seconds()})
	}
	return p
}

func fromPublic(p publicResult) harness.RunResult {
	r := harness.RunResult{RunID: harness.RunID(p.RunID), Scenario: p.Scenario,
		ProfileDigest: p.ProfileDigest, Model: p.Model, StartedAt: p.StartedAt,
		EndedAt: p.EndedAt, Completed: p.Completed, Reason: p.Reason, Err: p.Err}
	for _, c := range p.Challenges {
		r.Challenges = append(r.Challenges, harness.ChallengeResult{
			Challenge: harness.Challenge{Code: c.Code, Category: c.Category},
			Outcome: harness.OutcomeView{Code: c.Code, Reason: c.Reason, Submitted: c.Submitted,
				ProgressConfirmed: c.ProgressConfirmed, ProgressTotal: c.ProgressTotal,
				Rounds: c.Rounds, HintUsed: c.HintUsed, StartedAt: c.StartedAt, EndedAt: c.EndedAt},
			StartedAt: c.StartedAt, EndedAt: c.EndedAt})
	}
	return r
}

func (s *ResultFileStore) Save(ctx context.Context, r harness.RunResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.RunID == "" {
		return harness.Ef(harness.KindConfig, "resultstore.save", "RunID 为空", nil)
	}
	if err := validRunID(r.RunID); err != nil {
		return err
	}
	b, err := json.Marshal(toPublic(r))
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.root, string(r.RunID)+".json"), b, privatePerm)
}

func (s *ResultFileStore) Get(ctx context.Context, id harness.RunID) (harness.RunResult, error) {
	if err := ctx.Err(); err != nil {
		return harness.RunResult{}, err
	}
	if err := validRunID(id); err != nil {
		return harness.RunResult{}, err
	}
	b, err := os.ReadFile(filepath.Join(s.root, string(id)+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return harness.RunResult{}, harness.Ef(harness.KindPersistence, "resultstore.get", "结果不存在", err)
		}
		return harness.RunResult{}, err
	}
	var p publicResult
	if err := json.Unmarshal(b, &p); err != nil {
		return harness.RunResult{}, harness.Ef(harness.KindPersistence, "resultstore.get", "结果文件损坏", err)
	}
	return fromPublic(p), nil
}

func (s *ResultFileStore) List(ctx context.Context) ([]harness.RunResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var out []harness.RunResult
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		r, err := s.Get(ctx, harness.RunID(id))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *ResultFileStore) Stats(ctx context.Context, q harness.StatsQuery) (harness.StatsReport, error) {
	runs, err := s.List(ctx)
	if err != nil {
		return harness.StatsReport{}, err
	}
	var out harness.StatsReport
	for _, r := range runs {
		if q.ProfileDigest != "" && r.ProfileDigest != q.ProfileDigest ||
			q.Model != "" && r.Model != q.Model ||
			q.Scenario != "" && r.Scenario != q.Scenario {
			continue
		}
		if !q.Since.IsZero() && r.StartedAt.Before(q.Since) ||
			!q.Until.IsZero() && r.StartedAt.After(q.Until) {
			continue
		}
		out.Runs++
		if r.Completed {
			out.Completed++
		}
		for _, c := range r.Challenges {
			if q.Challenge != "" && c.Challenge.Code != q.Challenge ||
				q.Category != "" && c.Challenge.Category != q.Category {
				continue
			}
			out.ConfirmedFlags += c.Outcome.ProgressConfirmed
			out.DurationSeconds += c.Outcome.Duration().Seconds()
			out.HintedRuns += boolInt(c.Outcome.HintUsed > 0)
		}
	}
	if out.Runs > 0 {
		out.CompletionRate = float64(out.Completed) / float64(out.Runs)
	}
	return out, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
