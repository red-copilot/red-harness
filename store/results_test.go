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

// bundleDigestSample 是一份**合法形态**的扩展包摘要：16 个十六进制字符
// （sha256 的 hex[:8]，见 v04.go 的 bundleDigest）。它必须是假值，不是任何真实
// 内容的摘要。
const bundleDigestSample = "0123456789abcdef"

// profileDigestSample 同理，但用一个**不同**的值：两个摘要字段在往返断言里
// 各占一个位置，取同值会让「有没有串位」这类缺陷测不出来。
const profileDigestSample = "fedcba9876543210"

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
	// State 与 BundleDigest 是同一类「自由串字段落在公开文件里」的风险：两者都
	// 必须被脱敏折叠，不能被明文透传（State 折成 stateUnrecognized，摘要折成空串）。
	run.State = canaryFlag
	run.BundleDigest = canaryFlag
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
	if p.State != stateUnrecognized {
		t.Fatalf("state = %q，期望 %q（明文不得透传）", p.State, stateUnrecognized)
	}
	if p.BundleDigest != "" {
		t.Fatalf("bundleDigest = %q，期望被折成空串（它是原样落盘的自由串）", p.BundleDigest)
	}
}

// TestGraphSaveFailuresUseTheirOwnWhitelist：图落盘失败走**自己那份**白名单。
//
// 回归的形状：这份值曾经会被塞进 CleanupFailures 那条路——而它只放行
// agent/sandbox/scenario，于是 "write"/"marshal" 被**静默丢弃**，公开面上什么都
// 看不到。那正是「账记了但看不见」，比不记更糟：它让「图没留下来」无从判断。
func TestGraphSaveFailuresUseTheirOwnWhitelist(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	run := harness.RunResult{RunID: "run-1", Challenges: []harness.ChallengeResult{{
		Challenge: harness.Challenge{Code: "fake"},
		Outcome: harness.OutcomeView{
			Reason: harness.ReasonSolved,
			// 三个合法枚举 + 一个白名单外的值 + 一个明文 canary。
			GraphSaveFailures: []string{"write", "marshal", "export", "boom", canaryFlag},
		},
	}}}
	if err := rs.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, resultsDirName, "run-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if containsBytes(b, []byte(canaryFlag)) {
		t.Fatalf("结果文件泄漏了候选明文: %s", b)
	}
	var p publicResult
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	got := p.Challenges[0].GraphSaveFailures
	if len(got) != 3 || got[0] != "write" || got[1] != "marshal" || got[2] != "export" {
		t.Fatalf("graphSaveFailures = %v，期望恰好 [write marshal export]（白名单外的值必须被丢弃）", got)
	}
	// 走一轮往返：公开结构里的值要能读回 OutcomeView（否则 stats 与后续分析看不见）。
	back, err := rs.Get(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Challenges) != 1 || len(back.Challenges[0].Outcome.GraphSaveFailures) != 3 {
		t.Fatalf("往返后 graphSaveFailures 丢了: %+v", back.Challenges)
	}
}

// TestNewResultStoreCreatesPrivateDir：private/ 必须在**构造时**就存在。
//
// 回归：它原先只由第一次写 trace 惰性创建，而 bridge 在写 trace **之前**启动，
// 且 `bridge.privateStderrPath` 要求目录已存在——否则返回空串、桥的 stderr 被
// 静默丢弃。桥的 stderr 是平台侧异常（例如提交被平台以未识别状态码拒绝）唯一
// 能看到响应体的地方，于是出故障时想要的证据正好没了。
func TestNewResultStoreCreatesPrivateDir(t *testing.T) {
	root := t.TempDir()
	if _, err := NewResultStore(root); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(root, privateDirName))
	if err != nil {
		t.Fatalf("private/ 没有被建出来: %v", err)
	}
	if !st.IsDir() {
		t.Fatal("private/ 不是目录")
	}
	// 权限与 §3.3 一致：目录 0700、文件 0600。
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Errorf("private/ 权限 = %o，期望 700", perm)
	}
}

func TestResultRoundTripKeepsMetricsAndDropsPlaintext(t *testing.T) {
	rs := newResultStore(t)
	ended := baseTime.Add(90 * time.Second)
	// ProfileDigest 用**真实形态**（digestJSON 的 sha256 hex[:8]，16 个十六进制
	// 字符）。此前这里填的是 "pd" 这类占位串，而公开面现在与 BundleDigest 走
	// 同一份字符集校验——占位串会被折成空串，于是「往返一致」这条断言测的就不再
	// 是它要测的东西。
	run := harness.RunResult{RunID: "run-rt", Scenario: "sc", ProfileDigest: profileDigestSample, Model: "m",
		BundleDigest: bundleDigestSample,
		StartedAt:    baseTime, EndedAt: ended, Completed: true, Reason: harness.ReasonCompleted,
		State: harness.RunFinished,
		Err:   "*harness.Error",
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "c1", Category: "cat"},
			Outcome: harness.OutcomeView{Code: "c1", Reason: harness.ReasonSolved,
				Flags: []string{canaryFlag}, Candidates: []harness.Candidate{{Flag: canaryFlag}},
				Submitted: 2, ProgressConfirmed: 7, ProgressTotal: 10, RemainingAtStart: 5,
				Score: 42, Stats: harness.Stats{CostUSD: 1.25}, Rounds: 3, HintUsed: 1,
				BranchesAbandoned: 3,
				StartedAt:         baseTime, EndedAt: ended},
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
	// 运行终态与扩展包摘要也必须往返——它们是 v0.4 的新公开面：报告要能回答
	// 「怎么结束的」与「挂的是哪份扩展包内容」，这两件事事后都无法重算。
	if got.State != run.State {
		t.Fatalf("state 往返不一致: %q / %q", got.State, run.State)
	}
	if got.BundleDigest != run.BundleDigest {
		t.Fatalf("bundleDigest 往返不一致: %q / %q", got.BundleDigest, run.BundleDigest)
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
		c.Outcome.HintUsed != want.Outcome.HintUsed ||
		c.Outcome.BranchesAbandoned != want.Outcome.BranchesAbandoned {
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

// ── 运行终态与扩展包摘要 ──

// TestStateSanitized 钉住 State 的折叠规则。
//
// State 与 Reason 同类风险：类型上是契约枚举，底层却是 string，而它会原样落盘。
func TestStateSanitized(t *testing.T) {
	cases := []struct {
		name string
		in   harness.RunState
		want string
	}{
		{"空串保持空（未记录）", "", ""},
		{"正常结束放行", harness.RunFinished, "finished"},
		{"失败放行", harness.RunFailed, "failed"},
		{"取消放行", harness.RunCancelled, "cancelled"},
		{"v0.3 旧终态放行", harness.RunCompleted, "completed"},
		{"明文折叠", canaryFlag, stateUnrecognized},
		{"大小写不符折叠", "Finished", stateUnrecognized},
		{"带空格折叠", "finished ", stateUnrecognized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeState(tc.in); got != tc.want {
				t.Fatalf("sanitizeState(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
	// 全部终态常量都必须原样通过（新增终态时这条会立刻暴露折叠规则太严）。
	terminals := []harness.RunState{harness.RunFinished, harness.RunFailed,
		harness.RunCancelled, harness.RunCompleted}
	for _, s := range terminals {
		if got := sanitizeState(s); got != string(s) {
			t.Fatalf("终态 %q 被折成了 %q——折叠不能吃掉合法的运行终态", s, got)
		}
	}
	// 非终态一律折叠：结果文件里的这个字段回答的是「运行怎么结束的」，写着一个
	// 「还在跑」的值本身就是坏数据，读成 unrecognized 比读成 running 安全。
	for _, s := range []harness.RunState{harness.RunCreated, harness.RunPreparing,
		harness.RunRunning, harness.RunPaused} {
		if got := sanitizeState(s); got != stateUnrecognized {
			t.Fatalf("非终态 %q 折成了 %q，期望 %q", s, got, stateUnrecognized)
		}
	}
}

func TestBundleDigestSanitized(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空串保持空（未核验）", "", ""},
		{"十六进制摘要放行", bundleDigestSample, bundleDigestSample},
		{"完整 sha256 放行", strings.Repeat("abcdef0123456789", maxDigestLen/16), strings.Repeat("abcdef0123456789", maxDigestLen/16)},
		{"明文折叠成空", canaryFlag, ""},
		{"大写 hex 折叠", "0123456789ABCDEF", ""},
		{"非 hex 折叠", "0123456789abcdez", ""},
		{"带空格折叠", " 0123456789abcdef", ""},
		{"太短折叠", "abc", ""},
		{"太长折叠", strings.Repeat("a", maxDigestLen+1), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeHexDigest(tc.in); got != tc.want {
				t.Fatalf("sanitizeHexDigest(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNewResultFieldsSanitizedOnDisk：坏值落盘后必须是折叠后的值，且文件里
// 不得留下明文。走的是 Save → 读原始字节 → Get 这条真实路径，而不是直接调
// 脱敏函数——toPublic 漏接一个字段是这里唯一能抓到的方式。
func TestNewResultFieldsSanitizedOnDisk(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	saveRun(t, rs, harness.RunResult{RunID: "run-bad", StartedAt: baseTime,
		State: canaryFlag, BundleDigest: canaryFlag, ProfileDigest: canaryFlag,
		// 清单里的字段同样走白名单：镜像与版本是**自由串**（来源是配置与 docker
		// 输出），明文顺着它们漏进公开面与顺着 Reason 漏进去是同一条通道。
		Manifest: harness.RunManifest{
			HintPolicy: canaryFlag, RequestedImage: canaryFlag, Image: canaryFlag,
			PiVersion: canaryFlag, PlannerDryRounds: -1,
		},
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "c1"},
			Outcome:   harness.OutcomeView{BranchesAbandoned: -3},
		}}})

	b, err := os.ReadFile(filepath.Join(root, resultsDirName, "run-bad.json"))
	if err != nil {
		t.Fatal(err)
	}
	if containsBytes(b, []byte(canaryFlag)) {
		t.Fatalf("结果文件泄漏了明文: %s", b)
	}
	var p publicResult
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.State != stateUnrecognized {
		t.Fatalf("落盘 state = %q，期望 %q", p.State, stateUnrecognized)
	}
	if p.BundleDigest != "" {
		t.Fatalf("落盘 bundleDigest = %q，期望空串", p.BundleDigest)
	}
	// ProfileDigest 与 BundleDigest 是公开文件里并列的两个摘要字段，走同一份
	// 字符集校验——此前只有后者有闸，同性质的两个字段待遇不同是漏了一处。
	if p.ProfileDigest != "" {
		t.Fatalf("落盘 profileDigest = %q，期望空串", p.ProfileDigest)
	}
	if len(p.Challenges) != 1 || p.Challenges[0].BranchesAbandoned != 0 {
		t.Fatalf("落盘 branchesAbandoned = %+v，期望被钳到 0", p.Challenges)
	}
	bad := p.Manifest
	if bad.HintPolicy != "" || bad.RequestedImage != "" || bad.Image != "" || bad.PiVersion != "" {
		t.Fatalf("清单里的坏值没有被折叠: %+v", bad)
	}
	if bad.PlannerDryRounds != 0 {
		t.Fatalf("清单里的负数没有被钳到 0: %d", bad.PlannerDryRounds)
	}
	got, err := rs.Get(context.Background(), "run-bad")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != stateUnrecognized || got.BundleDigest != "" || got.ProfileDigest != "" ||
		got.Challenges[0].Outcome.BranchesAbandoned != 0 {
		t.Fatalf("读回的值不是折叠后的值: state=%q digest=%q profile=%q abandoned=%d",
			got.State, got.BundleDigest, got.ProfileDigest, got.Challenges[0].Outcome.BranchesAbandoned)
	}
	if got.Manifest.Image != "" || got.Manifest.PiVersion != "" || got.Manifest.HintPolicy != "" {
		t.Fatalf("读回的清单不是折叠后的值: %+v", got.Manifest)
	}
}

// TestManifestRoundTripAndSanitizers 钉住运行清单的两件事：
//
//  1. 合法值能完整往返（清单的全部意义是事后可读，往返丢了就等于没记）；
//  2. 每个自由串字段都过自己的字符集闸——镜像引用与版本号的值域不同，各自一份
//     规则，混用会让其中一个悄悄接受不属于它的东西。
func TestManifestRoundTripAndSanitizers(t *testing.T) {
	m := harness.RunManifest{
		PlannerDryRounds: 3, PromptMaxFacts: 8, PromptMaxNegative: 4,
		HintPolicy: harness.HintAuto,
		// 镜像引用要能吃下 digest 与 registry 路径两种形态。
		RequestedImage: "red-harness-runner:v0.3.0",
		Image:          "sha256:4e08f9133cd2f36d30fe26011d3a4308d6f7eef5ebbcdc203867608c37b66e98",
		PiVersion:      "0.85.1", ImageMismatch: true,
	}
	got := fromPublicManifest(toPublicManifest(m))
	if got != m {
		t.Fatalf("清单往返不一致:\n got %+v\nwant %+v", got, m)
	}

	// 逐字段的值域边界。每一条都是「这个字段不该接受的东西」。
	cases := []struct {
		name string
		in   string
		got  string
	}{
		{"镜像引用接受 digest", m.Image, m.Image},
		{"镜像引用接受 registry 路径", "registry.example.com:5000/a/b@sha256:abcd", "registry.example.com:5000/a/b@sha256:abcd"},
		{"镜像引用拒绝空格", "evil image", ""},
		{"镜像引用拒绝明文", canaryFlag, ""},
		{"镜像引用拒绝换行", "a\nb", ""},
		{"版本接受 semver", "0.85.1", "0.85.1"},
		{"版本接受预发布标记", "1.2.3-rc.1+build", "1.2.3-rc.1+build"},
		{"版本拒绝斜杠", "0.85.1/x", ""},
		{"版本拒绝明文", canaryFlag, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			if strings.Contains(tc.name, "版本") {
				got = sanitizePiVersion(tc.in)
			} else {
				got = sanitizeImageRef(tc.in)
			}
			if got != tc.got {
				t.Fatalf("sanitize(%q) = %q，期望 %q", tc.in, got, tc.got)
			}
		})
	}

	// 提示策略是**穷举**的三个值，不是「小写标识符形态」——一个拼错的策略必须
	// 折掉，否则它在公开面里看起来合法，而 RunSpec.Validate 恰恰要拒掉它。
	for _, ok := range []string{harness.HintOff, harness.HintAuto, harness.HintAlways} {
		if got := sanitizeHintPolicy(ok); got != ok {
			t.Errorf("合法策略 %q 被折成 %q", ok, got)
		}
	}
	if got := sanitizeHintPolicy("alwayss"); got != "" {
		t.Errorf("拼错的策略应折成空串，得到 %q", got)
	}
}

// TestNewResultFieldsEmptyStayEmpty：空串是「未记录 / 未核验」，**不得**被折成
// 兜底值——那会把「这次运行没记终态」谎报成「记了个不认识的终态」。
func TestNewResultFieldsEmptyStayEmpty(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	saveRun(t, rs, harness.RunResult{RunID: "run-empty", StartedAt: baseTime})

	b, err := os.ReadFile(filepath.Join(root, resultsDirName, "run-empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	// omitempty/omitzero：旧形状的文件里根本不该出现这几个键（读旧文件靠 json
	// 零值兜底）。清单用 omitzero：没有清单的运行不该留下一片零值字段——那会让
	// 「没记录」与「记了一堆 0」看起来一样。
	for _, key := range []string{`"state"`, `"bundleDigest"`, `"manifest"`} {
		if strings.Contains(string(b), key) {
			t.Fatalf("空值不该落盘成 %s: %s", key, b)
		}
	}
	got, err := rs.Get(context.Background(), "run-empty")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "" || got.BundleDigest != "" {
		t.Fatalf("空值往返后变成了 state=%q digest=%q，期望都保持空串", got.State, got.BundleDigest)
	}
}

// TestResultFilePinsNewFieldNames：公开文件的**键名**是持久化契约（下游报告、
// 看板、stats 都按名字读），改名等于静默破坏所有现存消费方。
func TestResultFilePinsNewFieldNames(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	saveRun(t, rs, harness.RunResult{RunID: "run-keys", StartedAt: baseTime,
		State: harness.RunFinished, BundleDigest: bundleDigestSample,
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "c1"},
			Outcome:   harness.OutcomeView{BranchesAbandoned: 2},
		}}})
	b, err := os.ReadFile(filepath.Join(root, resultsDirName, "run-keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"state":"finished"`, `"bundleDigest":"` + bundleDigestSample + `"`,
		`"branchesAbandoned":2`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("公开结果里缺少 %s: %s", want, b)
		}
	}
}

// TestPublicCountsAndGraphStatePersist：三个此前只活在内存/stdout 里的量现在
// **落盘且能读回**，而读不回来的那两个（Attempts/AuditIncomplete）必须真的不落盘。
//
// 为什么要按字段分开测而不是合并成一条「往返一致」：这一组字段的处境并不相同
// ——Duplicates/Rejected 有根包来源（往返必须成立），GraphState 是从
// GraphSaveFailures **折算**出来的（只能单向），Attempts/AuditIncomplete 根本
// 还没有来源。一条笼统的往返断言会把第三种情况（没来源）也测成「一致」，而那
// 正是它要防的谎报。
func TestPublicCountsAndGraphStatePersist(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	saveRun(t, rs, harness.RunResult{RunID: "run-counts", StartedAt: baseTime,
		State: harness.RunFinished, Completed: true,
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "c1"},
			Outcome: harness.OutcomeView{
				Code: "c1", Reason: harness.ReasonSolved,
				// 平台幂等命中 3 次、判错 4 条；图落盘失败在 export 阶段。
				Duplicates: 3, Rejected: 4,
				GraphSaveFailures: []string{"export"},
				// 负数在计数上没有含义（见 sanitizeCount）——公开面必须是 0 而不是 -5。
				BranchesAbandoned: -5,
			},
		}}})
	b, err := os.ReadFile(filepath.Join(root, resultsDirName, "run-counts.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 键名是持久化契约：下游报告与 stats 按名字读，改名等于静默破坏所有现存文件。
	for _, want := range []string{`"duplicates":3`, `"rejected":4`, `"graphState":"failed"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("公开结果里缺少 %s: %s", want, b)
		}
	}
	// 还没有来源的两个字段**不得**出现在文件里：omitempty + 恒零值意味着「没记录」，
	// 而写一个 0 出去会被读成「一次都没提交过 / 审计完整」——两者都是谎报。
	for _, never := range []string{`"attempts"`, `"auditIncomplete"`} {
		if strings.Contains(string(b), never) {
			t.Fatalf("没有来源的字段 %s 不该落盘（omitempty 应让它整个消失）: %s", never, b)
		}
	}

	got, err := rs.Get(context.Background(), "run-counts")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Challenges) != 1 {
		t.Fatalf("挑战数 = %d，期望 1", len(got.Challenges))
	}
	oc := got.Challenges[0].Outcome
	if oc.Duplicates != 3 || oc.Rejected != 4 {
		t.Fatalf("duplicates/rejected 往返不一致: %+v", oc)
	}
	if oc.BranchesAbandoned != 0 {
		t.Fatalf("负计数应被折成 0，实际 %d", oc.BranchesAbandoned)
	}
	// 折算字段读不回来是有意的（OutcomeView 没有接收它的字段），但**不得**被
	// 折成别的值——「读回来是空的」与「读回来是 saved」是两回事。
	if len(oc.GraphSaveFailures) != 1 || oc.GraphSaveFailures[0] != "export" {
		t.Fatalf("graphSaveFailures 往返不一致: %v", oc.GraphSaveFailures)
	}
}

// TestGraphStateAbsentUnlessProvablyFailed：graphState 只在**能证明图没落下来**
// 时出现。
//
// 留空是「未记录」，谎报一个 saved 是「让人以为图留下来了，而那份图可能压根没写」。
// 同一个文件里的两个字段也不许互相矛盾：白名单外的失败值既不会出现在
// graphSaveFailures 里，就不该凭空让 graphState 变成 failed。
func TestGraphStateAbsentUnlessProvablyFailed(t *testing.T) {
	cases := []struct {
		name     string
		failures []string
		want     bool
	}{
		{"没有失败阶段 ⇒ 未记录", nil, false},
		{"白名单外的值不算失败", []string{canaryFlag, "boom"}, false},
		{"合法阶段 ⇒ failed", []string{"write"}, true},
		{"合法与非法混在一起仍算 failed", []string{"boom", "marshal"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			rs, err := NewResultStore(root)
			if err != nil {
				t.Fatal(err)
			}
			saveRun(t, rs, harness.RunResult{RunID: "run-gs", StartedAt: baseTime,
				Challenges: []harness.ChallengeResult{{
					Challenge: harness.Challenge{Code: "c1"},
					Outcome:   harness.OutcomeView{Code: "c1", GraphSaveFailures: tc.failures},
				}}})
			b, err := os.ReadFile(filepath.Join(root, resultsDirName, "run-gs.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), canaryFlag) {
				t.Fatalf("公开结果泄漏了明文: %s", b)
			}
			has := strings.Contains(string(b), `"graphState"`)
			if has != tc.want {
				t.Fatalf("graphState 存在性 = %v，期望 %v: %s", has, tc.want, b)
			}
			if tc.want && !strings.Contains(string(b), `"graphState":"failed"`) {
				t.Fatalf("graphState 只可能是 failed（四态里其余三种是装配与产物事实）: %s", b)
			}
		})
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

// 过滤维度用的四份摘要。全部是**合法形态**（16 个十六进制字符）：
// profile/bundle 两个维度现在走同一份字符集校验，占位串（"pd-a" 之类）会被折成
// 空串，于是「按 profile 过滤」这条断言会因为**格式**而不是**语义**失败——
// 那种失败指向不了任何真实缺陷。
const (
	profileA = "aaaa000011112222"
	profileB = "bbbb000011112222"
	bundleA  = "0123456789abcdef"
	bundleB  = "fedcba9876543210"
)

// seedFilterRuns 建两份结果，覆盖全部过滤维度。
func seedFilterRuns(t *testing.T, rs *ResultFileStore) {
	t.Helper()
	saveRun(t, rs, harness.RunResult{RunID: "run-a", Scenario: "sc-a", ProfileDigest: profileA,
		BundleDigest: bundleA, Model: "m-a", StartedAt: baseTime, Completed: true,
		Challenges: []harness.ChallengeResult{{
			Challenge: harness.Challenge{Code: "c-a", Category: "cat-a"},
			Outcome: harness.OutcomeView{ProgressTotal: 10, ProgressConfirmed: 7,
				RemainingAtStart: 5, Score: 10, Stats: harness.Stats{CostUSD: 0.5}, HintUsed: 1},
		}}})
	saveRun(t, rs, harness.RunResult{RunID: "run-b", Scenario: "sc-b", ProfileDigest: profileB,
		BundleDigest: bundleB, Model: "m-b", StartedAt: baseTime.Add(2 * time.Hour),
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
		{"ProfileDigest 命中", harness.StatsQuery{ProfileDigest: profileA}, 1, 2, 5},
		{"ProfileDigest 不命中", harness.StatsQuery{ProfileDigest: "cccc000011112222"}, 0, 0, 0},
		{"BundleDigest 命中", harness.StatsQuery{BundleDigest: bundleA}, 1, 2, 5},
		{"BundleDigest 不命中", harness.StatsQuery{BundleDigest: "ffffffffffffffff"}, 0, 0, 0},
		// 这两个维度是**独立**的：ProfileDigest 里存的只是 bundle 的**路径**，
		// 同一个路径换了内容它不会变。所以「profile 命中但 bundle 不命中」必须
		// 过滤掉——不这么做，两次不同的扩展包会被算作同一次实验，而输出上
		// 完全看不出来。
		{"Profile+Bundle 同时命中", harness.StatsQuery{ProfileDigest: profileA, BundleDigest: bundleA}, 1, 2, 5},
		{"Profile 命中但 Bundle 不命中", harness.StatsQuery{ProfileDigest: profileA, BundleDigest: bundleB}, 0, 0, 0},
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
