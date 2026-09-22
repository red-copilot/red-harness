package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// canaryFlag 是泄漏门的固定假值。
//
// **刻意长得像真 flag**：泄漏门要能抓到「有人把 Outcome.Flags 或 Candidates
// 序列化进了公开结果」，而不是只抓到「文件非空」。任何真实凭据都不得出现在
// 本文件里，所以这里用的是明显的假值。
const canaryFlag = "flag{canary-not-a-real-flag}"

// baseTime 是固定的起跑时间。用 time.Date 而不是 time.Now：往返断言要求时间戳
// 逐位相等，而 time.Now 带单调时钟读数（JSON 里不会保留它）。
var baseTime = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

func newResultStore(t *testing.T) *ResultFileStore {
	t.Helper()
	rs, err := NewResultStore(t.TempDir())
	if err != nil {
		t.Fatalf("建结果后端失败: %v", err)
	}
	return rs
}

// saveRun 存一份结果，失败即终止。
func saveRun(t *testing.T, rs *ResultFileStore, r harness.RunResult) {
	t.Helper()
	if err := rs.Save(context.Background(), r); err != nil {
		t.Fatalf("保存结果 %s 失败: %v", r.RunID, err)
	}
}

func mustStats(t *testing.T, rs *ResultFileStore, q harness.StatsQuery) harness.StatsReport {
	t.Helper()
	rep, err := rs.Stats(context.Background(), q)
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	return rep
}

// ── 泄漏门 ──

func TestResultFileStoreNeverPersistsCandidatePlaintext(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	run := harness.RunResult{RunID: "run-1", Err: canaryFlag, Challenges: []harness.ChallengeResult{{
		Challenge: harness.Challenge{Code: "fake"},
		Outcome: harness.OutcomeView{
			Flags:           []string{canaryFlag},
			Candidates:      []harness.Candidate{{Flag: canaryFlag}},
			CleanupFailures: []string{canaryFlag, "agent"},
			// Reason 也会落盘（它是有界的枚举串），同样不得带明文。
			Reason: canaryFlag,
		},
	}}}
	if err := rs.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, resultsDirName, "run-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "" || containsBytes(b, []byte(canaryFlag)) {
		t.Fatalf("结果文件泄漏了候选明文: %s", b)
	}
	// Err 与 Reason 走的是脱敏折叠，明文必须被折成兜底值。
	var p publicResult
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.Err != errClassUnclassified {
		t.Fatalf("errorClass = %q，期望 %q（明文不得透传）", p.Err, errClassUnclassified)
	}
	if len(p.Challenges) != 1 || p.Challenges[0].Reason != reasonUnrecognized {
		t.Fatalf("题级 reason = %+v，期望被折成 %q（它是原样落盘的自由串）",
			p.Challenges, reasonUnrecognized)
	}
}

func TestResultRoundTripKeepsMetricsAndDropsPlaintext(t *testing.T) {
	rs := newResultStore(t)
	ended := baseTime.Add(90 * time.Second)
	run := harness.RunResult{RunID: "run-rt", Scenario: "sc", ProfileDigest: "pd", Model: "m",
		StartedAt: baseTime, EndedAt: ended, Completed: true, Reason: harness.ReasonCompleted,
		Err: "*harness.Error",
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "c1", Category: "cat"},
			Outcome: harness.OutcomeView{Code: "c1", Reason: harness.ReasonSolved,
				Flags: []string{canaryFlag}, Candidates: []harness.Candidate{{Flag: canaryFlag}},
				Submitted: 2, ProgressConfirmed: 7, ProgressTotal: 10, RemainingAtStart: 5,
				Score: 42, Stats: harness.Stats{CostUSD: 1.25}, Rounds: 3, HintUsed: 1,
				StartedAt: baseTime, EndedAt: ended},
			StartedAt: baseTime, EndedAt: ended,
		}}}
	saveRun(t, rs, run)

	got, err := rs.Get(context.Background(), "run-rt")
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != run.RunID || got.Scenario != run.Scenario || got.ProfileDigest != run.ProfileDigest ||
		got.Model != run.Model || got.Completed != run.Completed || got.Reason != run.Reason ||
		got.Err != run.Err {
		t.Fatalf("run 级字段往返不一致: %+v", got)
	}
	if !got.StartedAt.Equal(run.StartedAt) || !got.EndedAt.Equal(run.EndedAt) {
		t.Fatalf("时间戳往返不一致: %v / %v", got.StartedAt, got.EndedAt)
	}
	if len(got.Challenges) != 1 {
		t.Fatalf("挑战数 = %d，期望 1", len(got.Challenges))
	}
	c, want := got.Challenges[0], run.Challenges[0]
	if c.Challenge.Code != want.Challenge.Code || c.Challenge.Category != want.Challenge.Category {
		t.Fatalf("挑战标识往返不一致: %+v", c.Challenge)
	}
	if c.Outcome.Submitted != want.Outcome.Submitted ||
		c.Outcome.ProgressConfirmed != want.Outcome.ProgressConfirmed ||
		c.Outcome.ProgressTotal != want.Outcome.ProgressTotal ||
		c.Outcome.RemainingAtStart != want.Outcome.RemainingAtStart ||
		c.Outcome.Score != want.Outcome.Score ||
		c.Outcome.Rounds != want.Outcome.Rounds ||
		c.Outcome.HintUsed != want.Outcome.HintUsed {
		t.Fatalf("挑战指标往返不一致: %+v", c.Outcome)
	}
	if math.Abs(c.Outcome.Stats.CostUSD-want.Outcome.Stats.CostUSD) > 1e-9 {
		t.Fatalf("成本往返不一致: %v", c.Outcome.Stats.CostUSD)
	}
	if c.Outcome.Duration() != want.Outcome.Duration() {
		t.Fatalf("耗时往返不一致: %v", c.Outcome.Duration())
	}
	// 明文字段**允许**丢失——那是有意的（返回值不是落盘物）。
	if len(c.Outcome.Flags) != 0 || len(c.Outcome.Candidates) != 0 {
		t.Fatalf("明文不该被读回来: %+v", c.Outcome)
	}
}

func TestErrorClassSanitized(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空串保持空", "", ""},
		{"类型名放行", "*harness.Error", "*harness.Error"},
		{"包裹类型放行", "*fmt.wrapError", "*fmt.wrapError"},
		{"契约错误类别放行", string(harness.KindProvider), string(harness.KindProvider)},
		{"明文折叠", canaryFlag, errClassUnclassified},
		{"含空白的明文折叠", "提交失败: " + canaryFlag, errClassUnclassified},
		{"无包限定的裸串折叠", "run_error", errClassUnclassified},
		{"超长折叠", "*harness.Error" + strings.Repeat("x", maxErrorClassLen), errClassUnclassified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeErrorClass(tc.in); got != tc.want {
				t.Fatalf("sanitizeErrorClass(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReasonSanitized(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空串保持空", "", ""},
		{"契约常量放行", harness.ReasonCompleted, "completed"},
		{"题级常量放行", harness.ReasonSolved, "solved"},
		{"明文折叠", canaryFlag, reasonUnrecognized},
		{"带空格折叠", "提交超时 3 次", reasonUnrecognized},
		{"超长折叠", strings.Repeat("a", maxReasonLen+1), reasonUnrecognized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeReason(tc.in); got != tc.want {
				t.Fatalf("sanitizeReason(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
	// 全部 Reason* 常量都必须原样通过（新增常量时这条会立刻暴露脱敏规则太严）。
	for _, r := range []string{harness.ReasonCompleted, harness.ReasonTimeout, harness.ReasonStalled,
		harness.ReasonStopped, harness.ReasonMaxTurns, harness.ReasonError, harness.ReasonMaxRounds,
		harness.ReasonMaxCost, harness.ReasonNoIntent, harness.ReasonSolved, harness.ReasonProviderFailure} {
		if got := sanitizeReason(r); got != r {
			t.Fatalf("契约常量 %q 被折成了 %q——脱敏规则不能吃掉合法的 Reason", r, got)
		}
	}
}

// ── 权限 ──

func TestResultFileAndDirPermissions(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	saveRun(t, rs, harness.RunResult{RunID: "run-perm", StartedAt: baseTime})

	dirInfo, err := os.Stat(filepath.Join(root, resultsDirName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("结果目录权限 = %o，期望 700", perm)
	}
	fileInfo, err := os.Stat(filepath.Join(root, resultsDirName, "run-perm.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 0600 而不是 0644：内容层面是公开面（只有指标与指纹），权限层面跟随
	// 结果目录的 0700/0600 纪律——见 Save 的注释。
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("结果文件权限 = %o，期望 600", perm)
	}
}

// ── 召回率口径 ──

// TestStatsRecallUsesRemainingAtStart 是主缺陷的回归：分母是起跑剩余量，
// 分子是本次新增确认，而不是最终累计进度。
func TestStatsRecallUsesRemainingAtStart(t *testing.T) {
	rs := newResultStore(t)
	saveRun(t, rs, harness.RunResult{RunID: "run-1", StartedAt: baseTime,
		Challenges: []harness.ChallengeResult{
			// 起跑时 10 题已解 5（剩余 5），最终确认 7 ⇒ 本次新增 2。
			{Challenge: harness.Challenge{Code: "c1"},
				Outcome: harness.OutcomeView{ProgressTotal: 10, ProgressConfirmed: 7, RemainingAtStart: 5}},
			// 分母未知：分子分母都不计（它的 9 不能进 ConfirmedFlags）。
			{Challenge: harness.Challenge{Code: "c2"},
				Outcome: harness.OutcomeView{ProgressTotal: 9, ProgressConfirmed: 9, RemainingAtStart: 0}},
		}})

	rep := mustStats(t, rs, harness.StatsQuery{})
	if rep.ConfirmedFlags != 2 {
		t.Fatalf("ConfirmedFlags = %d，期望 2（最终累计进度 16 不得算成本次确认）", rep.ConfirmedFlags)
	}
	if rep.RemainingAtStart != 5 {
		t.Fatalf("RemainingAtStart = %d，期望 5", rep.RemainingAtStart)
	}
	if math.Abs(rep.RecallRate-0.4) > 1e-9 {
		t.Fatalf("RecallRate = %v，期望 0.4", rep.RecallRate)
	}
}

// TestStatsUnknownDenominatorKeepsCountersAtZero：分母未知时两个计数器都是 0，
// 公开数字里不得出现 NaN / null。
func TestStatsUnknownDenominatorKeepsCountersAtZero(t *testing.T) {
	rs := newResultStore(t)
	saveRun(t, rs, harness.RunResult{RunID: "run-1", StartedAt: baseTime,
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "c1"},
			Outcome:   harness.OutcomeView{ProgressTotal: 4, ProgressConfirmed: 4, RemainingAtStart: 0},
		}}})

	rep := mustStats(t, rs, harness.StatsQuery{})
	if rep.RemainingAtStart != 0 || rep.ConfirmedFlags != 0 {
		t.Fatalf("分母未知时 counters = %d/%d，期望 0/0",
			rep.ConfirmedFlags, rep.RemainingAtStart)
	}
	if rep.RecallRate != 0 {
		t.Fatalf("RecallRate = %v，期望 0（0/0 不得是 NaN）", rep.RecallRate)
	}
	// 分母为 0 ⇒ 召回率按定义是 0（0/0 不是 0，NaN 会顺着 JSON 变成 null）。
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "NaN") || strings.Contains(s, "null") {
		t.Fatalf("聚合结果里出现了 NaN/null: %s", s)
	}
	// 分母未知时**必然没有分子**：否则比率会被抬高。
	if rep.RemainingAtStart == 0 && rep.ConfirmedFlags != 0 {
		t.Fatal("没有分母却有分子——分母未知的挑战必须同时退出分子与分母")
	}
}

// TestRecallDeltaEdgeCases 直接钉住单题口径的边界。
func TestRecallDeltaEdgeCases(t *testing.T) {
	cases := []struct {
		name          string
		o             harness.OutcomeView
		wantDelta     int
		wantRemaining int
		wantOK        bool
	}{
		{"部分完成", harness.OutcomeView{ProgressTotal: 10, ProgressConfirmed: 7, RemainingAtStart: 5}, 2, 5, true},
		{"一题没解", harness.OutcomeView{ProgressTotal: 3, ProgressConfirmed: 0, RemainingAtStart: 3}, 0, 3, true},
		{"全解", harness.OutcomeView{ProgressTotal: 3, ProgressConfirmed: 3, RemainingAtStart: 3}, 3, 3, true},
		{"分母未知", harness.OutcomeView{ProgressTotal: 3, ProgressConfirmed: 3, RemainingAtStart: 0}, 0, 0, false},
		{"进度回退不记负数", harness.OutcomeView{ProgressTotal: 10, ProgressConfirmed: 4, RemainingAtStart: 5}, 0, 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delta, remaining, ok := recallDelta(tc.o)
			if delta != tc.wantDelta || remaining != tc.wantRemaining || ok != tc.wantOK {
				t.Fatalf("recallDelta = (%d, %d, %v)，期望 (%d, %d, %v)",
					delta, remaining, ok, tc.wantDelta, tc.wantRemaining, tc.wantOK)
			}
		})
	}
}

// ── 过滤维度 ──

// seedFilterRuns 建两份结果，覆盖全部过滤维度。
func seedFilterRuns(t *testing.T, rs *ResultFileStore) {
	t.Helper()
	saveRun(t, rs, harness.RunResult{RunID: "run-a", Scenario: "sc-a", ProfileDigest: "pd-a",
		Model: "m-a", StartedAt: baseTime, Completed: true,
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "c-a", Category: "cat-a"},
			Outcome: harness.OutcomeView{ProgressTotal: 10, ProgressConfirmed: 7,
				RemainingAtStart: 5, Score: 10, Stats: harness.Stats{CostUSD: 0.5}, HintUsed: 1},
		}}})
	saveRun(t, rs, harness.RunResult{RunID: "run-b", Scenario: "sc-b", ProfileDigest: "pd-b",
		Model: "m-b", StartedAt: baseTime.Add(2 * time.Hour),
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "c-b", Category: "cat-b"},
			Outcome: harness.OutcomeView{ProgressTotal: 4, ProgressConfirmed: 1,
				RemainingAtStart: 4, Score: 5, Stats: harness.Stats{CostUSD: 0.25}},
		}}})
}

func TestStatsFilters(t *testing.T) {
	rs := newResultStore(t)
	seedFilterRuns(t, rs)

	cases := []struct {
		name          string
		q             harness.StatsQuery
		wantRuns      int
		wantConfirmed int
		wantRemaining int
	}{
		{"ProfileDigest 命中", harness.StatsQuery{ProfileDigest: "pd-a"}, 1, 2, 5},
		{"ProfileDigest 不命中", harness.StatsQuery{ProfileDigest: "pd-z"}, 0, 0, 0},
		{"Model 命中", harness.StatsQuery{Model: "m-b"}, 1, 1, 4},
		{"Model 不命中", harness.StatsQuery{Model: "m-z"}, 0, 0, 0},
		{"Scenario 命中", harness.StatsQuery{Scenario: "sc-a"}, 1, 2, 5},
		{"Scenario 不命中", harness.StatsQuery{Scenario: "sc-z"}, 0, 0, 0},
		{"Challenge 命中", harness.StatsQuery{Challenge: "c-b"}, 1, 1, 4},
		{"Challenge 不命中", harness.StatsQuery{Challenge: "c-z"}, 0, 0, 0},
		{"Category 命中", harness.StatsQuery{Category: "cat-a"}, 1, 2, 5},
		{"Category 不命中", harness.StatsQuery{Category: "cat-z"}, 0, 0, 0},
		{"Since 命中", harness.StatsQuery{Since: baseTime.Add(time.Hour)}, 1, 1, 4},
		{"Since 不命中", harness.StatsQuery{Since: baseTime.Add(3 * time.Hour)}, 0, 0, 0},
		{"Until 命中", harness.StatsQuery{Until: baseTime.Add(time.Hour)}, 1, 2, 5},
		{"Until 不命中", harness.StatsQuery{Until: baseTime.Add(-time.Hour)}, 0, 0, 0},
		{"无过滤", harness.StatsQuery{}, 2, 3, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := mustStats(t, rs, tc.q)
			if rep.Runs != tc.wantRuns {
				t.Fatalf("Runs = %d，期望 %d", rep.Runs, tc.wantRuns)
			}
			if rep.ConfirmedFlags != tc.wantConfirmed || rep.RemainingAtStart != tc.wantRemaining {
				t.Fatalf("Confirmed/Remaining = %d/%d，期望 %d/%d",
					rep.ConfirmedFlags, rep.RemainingAtStart, tc.wantConfirmed, tc.wantRemaining)
			}
			// 按题过滤时，被过滤掉的 run 不得计入 Runs——否则 CompletionRate
			// 会被不含该题的 run 稀释。
			if (tc.q.Challenge != "" || tc.q.Category != "") && rep.Runs > 0 && rep.ConfirmedFlags == 0 {
				t.Fatalf("按题过滤下 Runs=%d 但没有任何确认量，口径不一致", rep.Runs)
			}
		})
	}
}

// TestStatsScoreCostAndHintedRuns 钉住 v0.4 之前**从未赋值**的字段。
func TestStatsScoreCostAndHintedRuns(t *testing.T) {
	rs := newResultStore(t)
	seedFilterRuns(t, rs)
	rep := mustStats(t, rs, harness.StatsQuery{})
	if rep.Score != 15 {
		t.Fatalf("Score = %d，期望 15", rep.Score)
	}
	if math.Abs(rep.CostUSD-0.75) > 1e-9 {
		t.Fatalf("CostUSD = %v，期望 0.75", rep.CostUSD)
	}
	// HintedRuns 是 **run 级**：run-a 有一道题用过提示 ⇒ 1，而不是「挑战数」。
	if rep.HintedRuns != 1 {
		t.Fatalf("HintedRuns = %d，期望 1（run 级，不是挑战数）", rep.HintedRuns)
	}
}

func TestStatsHintedRunsCountsRunsNotChallenges(t *testing.T) {
	rs := newResultStore(t)
	saveRun(t, rs, harness.RunResult{RunID: "run-1", StartedAt: baseTime,
		Challenges: []harness.ChallengeResult{
			{Challenge: harness.Challenge{Code: "c1"},
				Outcome: harness.OutcomeView{HintUsed: 1, RemainingAtStart: 1, ProgressTotal: 1, ProgressConfirmed: 0}},
			{Challenge: harness.Challenge{Code: "c2"},
				Outcome: harness.OutcomeView{HintUsed: 2, RemainingAtStart: 1, ProgressTotal: 1, ProgressConfirmed: 0}},
		}})
	rep := mustStats(t, rs, harness.StatsQuery{})
	if rep.HintedRuns != 1 {
		t.Fatalf("HintedRuns = %d，期望 1（同一 run 内两道题用过提示仍只算一个 run）", rep.HintedRuns)
	}
}

// TestStatsProviderFailuresUsesReasonConstant：判据是契约常量，不是错误文本。
func TestStatsProviderFailuresUsesReasonConstant(t *testing.T) {
	rs := newResultStore(t)
	saveRun(t, rs, harness.RunResult{RunID: "run-1", StartedAt: baseTime,
		Reason: harness.ReasonProviderFailure})
	saveRun(t, rs, harness.RunResult{RunID: "run-2", StartedAt: baseTime,
		Reason: harness.ReasonCompleted})
	rep := mustStats(t, rs, harness.StatsQuery{})
	if rep.ProviderFailures != 1 {
		t.Fatalf("ProviderFailures = %d，期望 1", rep.ProviderFailures)
	}
}

func TestStatsCompletionRate(t *testing.T) {
	rs := newResultStore(t)
	saveRun(t, rs, harness.RunResult{RunID: "run-1", StartedAt: baseTime, Completed: true})
	saveRun(t, rs, harness.RunResult{RunID: "run-2", StartedAt: baseTime})
	rep := mustStats(t, rs, harness.StatsQuery{})
	if rep.Runs != 2 || rep.Completed != 1 || math.Abs(rep.CompletionRate-0.5) > 1e-9 {
		t.Fatalf("完成率口径不对: %+v", rep)
	}
	// 空集合：完成率必须是 0 而不是 NaN。
	empty := mustStats(t, newResultStore(t), harness.StatsQuery{})
	if empty.Runs != 0 || empty.CompletionRate != 0 {
		t.Fatalf("空集合: %+v", empty)
	}
}

func TestStatsChallengeCompletionRate(t *testing.T) {
	rs := newResultStore(t)
	saveRun(t, rs, harness.RunResult{RunID: "run-1", StartedAt: baseTime, Completed: true,
		Challenges: []harness.ChallengeResult{
			{Challenge: harness.Challenge{Code: "a"}, Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}},
			{Challenge: harness.Challenge{Code: "b"}, Outcome: harness.OutcomeView{Reason: harness.ReasonNoProgress}},
		}})
	rep := mustStats(t, rs, harness.StatsQuery{})
	if rep.Challenges != 2 || rep.SolvedChallenges != 1 || rep.ChallengeCompletionRate != 0.5 {
		t.Fatalf("challenge completion rate: %+v", rep)
	}
	filtered := mustStats(t, rs, harness.StatsQuery{Challenge: "b"})
	if filtered.Challenges != 1 || filtered.SolvedChallenges != 0 || filtered.ChallengeCompletionRate != 0 {
		t.Fatalf("filtered challenge completion rate: %+v", filtered)
	}
}

// ── 错误分类 ──

func TestGetErrorKinds(t *testing.T) {
	rs := newResultStore(t)
	saveRun(t, rs, harness.RunResult{RunID: "run-1", StartedAt: baseTime})

	if _, err := rs.Get(context.Background(), "run-nope"); !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("不存在的 runID: err = %v，期望 KindPersistence", err)
	}
	for _, bad := range []harness.RunID{"", "..", "a/b", "/abs", ".hidden"} {
		if _, err := rs.Get(context.Background(), bad); !harness.IsKind(err, harness.KindConfig) {
			t.Fatalf("非法 runID %q: err = %v，期望 KindConfig", bad, err)
		}
	}
}

func TestSaveRejectsBadRunID(t *testing.T) {
	rs := newResultStore(t)
	for _, bad := range []harness.RunID{"", "..", "a/b", ".hidden"} {
		if err := rs.Save(context.Background(), harness.RunResult{RunID: bad}); !harness.IsKind(err, harness.KindConfig) {
			t.Fatalf("非法 runID %q: err = %v，期望 KindConfig", bad, err)
		}
	}
	// 空目录必须被挡在建后端之前。
	if _, err := NewResultStore("   "); !harness.IsKind(err, harness.KindConfig) {
		t.Fatalf("空目录: err = %v，期望 KindConfig", err)
	}
}

// TestListFailsClosedOnCorruptResult：合法 id 的结果文件损坏 ⇒ 整体失败。
//
// 这是**有意的**取舍（见 List 的注释）：静默跳过会让召回率的分母悄悄变小，
// 而「少算了几道题」在通过率结论里完全看不出来。这条测试钉住这个选择，
// 将来若要改成「跳过 + 记录」，必须先改这条断言并说明理由。
func TestListFailsClosedOnCorruptResult(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	saveRun(t, rs, harness.RunResult{RunID: "run-good", StartedAt: baseTime})
	if err := os.WriteFile(filepath.Join(root, resultsDirName, "run-broken.json"),
		[]byte("{ 半截 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.List(context.Background()); !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("损坏结果文件: err = %v，期望 KindPersistence（fail closed）", err)
	}
	if _, err := rs.Stats(context.Background(), harness.StatsQuery{}); !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("损坏结果文件下的 stats: err = %v，期望 KindPersistence", err)
	}
	// 错误里必须带 RunID，调用方才能定位坏文件。
	if err := func() error { _, e := rs.Get(context.Background(), "run-broken"); return e }(); !strings.Contains(err.Error(), "run-broken") {
		t.Fatalf("错误信息里没有 RunID: %v", err)
	}
}

// TestListSkipsFilesThatAreNotResults：非结果文件（杂散、临时、子目录）被跳过。
//
// 与上一条的区别：那些文件**不是 store 写的**，跳过它们不会让样本变小；
// 而报错会让一个无关文件把 list / stats 一起锁死。
func TestListSkipsFilesThatAreNotResults(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	saveRun(t, rs, harness.RunResult{RunID: "run-good", StartedAt: baseTime})
	dir := filepath.Join(root, resultsDirName)
	// 临时文件（writeFileAtomic 残留）、隐藏文件、非 json、以及一个同名目录。
	for _, name := range []string{"run-good.json.tmp.1.2", ".hidden.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("noise"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "adir.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	runs, err := rs.List(context.Background())
	if err != nil {
		t.Fatalf("杂散文件不该让 List 失败: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != "run-good" {
		t.Fatalf("List = %+v，期望只有 run-good", runs)
	}
}

func TestListAndStatsOnEmptyStore(t *testing.T) {
	rs := newResultStore(t)
	runs, err := rs.List(context.Background())
	if err != nil || len(runs) != 0 {
		t.Fatalf("空存储: runs = %v, err = %v", runs, err)
	}
	// 结果目录整个消失也不能报错：与 FileStore.ListRuns 的语义一致。
	if err := os.RemoveAll(rs.root); err != nil {
		t.Fatal(err)
	}
	if runs, err := rs.List(context.Background()); err != nil || len(runs) != 0 {
		t.Fatalf("目录缺失: runs = %v, err = %v", runs, err)
	}
}

func TestSaveAndListRespectContext(t *testing.T) {
	rs := newResultStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rs.Save(ctx, harness.RunResult{RunID: "run-1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save 未尊重 ctx: %v", err)
	}
	if _, err := rs.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List 未尊重 ctx: %v", err)
	}
	if _, err := rs.Stats(ctx, harness.StatsQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stats 未尊重 ctx: %v", err)
	}
}

// containsBytes 报告 haystack 里是否含 needle（泄漏门用，不引 strings 之外的依赖）。
func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
