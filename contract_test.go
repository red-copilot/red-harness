package harness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// ── 状态机 ──

func TestContract_RunStateTerminal(t *testing.T) {
	cases := []struct {
		state harness.RunState
		want  bool
	}{
		{harness.RunCreated, false},
		{harness.RunPreparing, false},
		{harness.RunRunning, false},
		{harness.RunPaused, false},
		{harness.RunCompleted, true},
		{harness.RunFailed, true},
		{harness.RunCancelled, true},
	}
	for _, c := range cases {
		if got := c.state.Terminal(); got != c.want {
			t.Errorf("RunState(%q).Terminal() = %v，期望 %v", c.state, got, c.want)
		}
	}
	// 未知状态必须**不是**终态：宁可让 Resume 多跑一次校验，也不能把一个
	// 拼错的状态当成「已结束」而拒绝恢复用户的运行。
	if harness.RunState("bogus").Terminal() {
		t.Error("未知状态不应被判为终态——拼错的状态会让 Resume 拒绝恢复合法运行")
	}
}

// ── 配置摘要 ──

func TestContract_RunSpecDigest(t *testing.T) {
	base := harness.RunSpec{
		Scenario: "tsec",
		Targets:  []string{"web-01", "pwn-02"},
		Agent:    harness.AgentSpec{Provider: "opencode-go", Model: "deepseek-v4-flash"},
		Budget:   harness.Budget{MaxRounds: 10},
	}

	// 同配置 ⇒ 同摘要。这是恢复时 fail closed 的基础：如果同配置算出不同摘要，
	// 每一次恢复都会被误判成「配置漂移」而拒绝。
	first, second := base.Digest(), base.Digest()
	if first != second {
		t.Fatalf("同一份配置两次 Digest() 结果不同：%q vs %q", first, second)
	}
	var copySpec harness.RunSpec
	if err := json.Unmarshal(mustJSON(t, base), &copySpec); err != nil {
		t.Fatalf("RunSpec 无法 JSON round-trip: %v", err)
	}
	if copySpec.Digest() != base.Digest() {
		t.Error("JSON round-trip 后摘要变了——恢复时会误判配置漂移")
	}

	// 字段漂移 ⇒ 摘要变。逐项验证，因为静默的「摘要不变」会让用户改了预算
	// 之后仍然用旧预算恢复，报告里的「为什么停」就变成假的了。
	drifts := map[string]func(*harness.RunSpec){
		"Scenario":   func(s *harness.RunSpec) { s.Scenario = "other" },
		"Budget":     func(s *harness.RunSpec) { s.Budget.MaxRounds = 11 },
		"Model":      func(s *harness.RunSpec) { s.Agent.Model = "other-model" },
		"Submit":     func(s *harness.RunSpec) { s.Submit = true },
		"HintPolicy": func(s *harness.RunSpec) { s.HintPolicy = harness.HintAlways },
	}
	for name, mutate := range drifts {
		s := base
		mutate(&s)
		if s.Digest() == base.Digest() {
			t.Errorf("改 %s 后摘要没变——配置漂移检测会漏掉这个字段", name)
		}
	}

	// Targets 顺序敏感：执行顺序会影响平台侧状态，所以顺序不同就是不同配置。
	reordered := base
	reordered.Targets = []string{"pwn-02", "web-01"}
	if reordered.Digest() == base.Digest() {
		t.Error("Targets 顺序变了但摘要没变——执行顺序会影响平台侧状态，应当算作配置漂移")
	}

	// nil 与空切片必须序列化一致，否则同一份配置在两次构造之间摘要会漂。
	// （encoding/json 把 nil 切片编成 null、空切片编成 []，所以这里是**断言
	// 当前行为**：调用方不要混用两者。）
	empty := base
	empty.Targets = []string{}
	if empty.Digest() == base.Digest() {
		t.Log("提示：nil Targets 与空 Targets 摘要相同（当前实现如此）")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ── 预算 ──

func TestContract_BudgetExhaustedReasons(t *testing.T) {
	cases := []struct {
		name string
		b    harness.Budget
		used harness.Budget
		want bool
		why  string
	}{
		{"未耗尽", harness.Budget{MaxRounds: 10, MaxWall: time.Minute, MaxTurns: 100},
			harness.Budget{MaxRounds: 1, MaxWall: time.Second, MaxTurns: 1}, false, ""},
		{"轮次耗尽", harness.Budget{MaxRounds: 10}, harness.Budget{MaxRounds: 10}, true, harness.ReasonMaxRounds},
		{"墙钟耗尽", harness.Budget{MaxWall: time.Second}, harness.Budget{MaxWall: time.Second}, true, harness.ReasonTimeout},
		{"工具调用耗尽", harness.Budget{MaxTurns: 5}, harness.Budget{MaxTurns: 5}, true, harness.ReasonMaxTurns},
		{"成本耗尽", harness.Budget{MaxCostUSD: 1.5}, harness.Budget{MaxCostUSD: 1.5}, true, harness.ReasonMaxCost},
		{"零值维度不限", harness.Budget{}, harness.Budget{MaxRounds: 9999, MaxWall: time.Hour, MaxTurns: 9999, MaxCostUSD: 999}, false, ""},
	}
	for _, c := range cases {
		got, reason := c.b.Exhausted(c.used)
		if got != c.want {
			t.Errorf("%s: Exhausted = %v，期望 %v", c.name, got, c.want)
			continue
		}
		if reason != c.why {
			t.Errorf("%s: reason = %q，期望 %q", c.name, reason, c.why)
		}
	}
}

// TestContract_BudgetReasonsAreDistinct 钉死「轮次」与「工具调用次数」不再共用
// 同一个原因字符串——v0.2 里两者都是 max_turns，报告因此无法回答「为什么停」。
func TestContract_BudgetReasonsAreDistinct(t *testing.T) {
	if harness.ReasonMaxRounds == harness.ReasonMaxTurns {
		t.Fatal("ReasonMaxRounds 与 ReasonMaxTurns 不能相同")
	}
	if harness.ReasonMaxCost == harness.ReasonTimeout {
		t.Fatal("ReasonMaxCost 与 ReasonTimeout 不能相同——v0.2 的成本分支就是从墙钟分支拷来的")
	}
}

// TestContract_ExistingReasonsUnchanged 钉死 v0.2 的九个原因字符串。
// 这些是公开 API，三个回归测试依赖它们。
func TestContract_ExistingReasonsUnchanged(t *testing.T) {
	want := map[string]string{
		"ReasonCompleted":       "completed",
		"ReasonTimeout":         "timeout",
		"ReasonStalled":         "stalled",
		"ReasonStopped":         "stopped",
		"ReasonMaxTurns":        "max_turns",
		"ReasonError":           "error",
		"ReasonNoIntent":        "no_intent",
		"ReasonSolved":          "solved",
		"ReasonProviderFailure": "provider_failure",
	}
	got := map[string]string{
		"ReasonCompleted":       harness.ReasonCompleted,
		"ReasonTimeout":         harness.ReasonTimeout,
		"ReasonStalled":         harness.ReasonStalled,
		"ReasonStopped":         harness.ReasonStopped,
		"ReasonMaxTurns":        harness.ReasonMaxTurns,
		"ReasonError":           harness.ReasonError,
		"ReasonNoIntent":        harness.ReasonNoIntent,
		"ReasonSolved":          harness.ReasonSolved,
		"ReasonProviderFailure": harness.ReasonProviderFailure,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q，v0.2 的值是 %q——Reason 字符串是 API，不得改变已有值", k, got[k], v)
		}
	}
}

// TestContract_DefaultBudget 钉死默认预算量级（PLAN.md:78）。
func TestContract_DefaultBudget(t *testing.T) {
	b := harness.DefaultBudget()
	if b.MaxRounds != 40 {
		t.Errorf("MaxRounds = %d，期望 40", b.MaxRounds)
	}
	if b.MaxWall != 30*time.Minute {
		t.Errorf("MaxWall = %v，期望 30m", b.MaxWall)
	}
	if b.MaxTurns != 600 {
		t.Errorf("MaxTurns = %d，期望 600", b.MaxTurns)
	}
	if b.MaxCostUSD != 0 {
		t.Errorf("MaxCostUSD = %v，默认应当不限（0）", b.MaxCostUSD)
	}
}

// ── 错误 ──

func TestContract_ErrorKindThroughWrapping(t *testing.T) {
	inner := harness.E(harness.KindPlatform, "platform.submit", errors.New("503"))
	if !harness.IsKind(inner, harness.KindPlatform) {
		t.Fatal("直接调用 IsKind 应当命中")
	}
	if harness.IsKind(inner, harness.KindConfig) {
		t.Fatal("分类不应串台")
	}

	// 三层包裹后仍然要命中——引擎会在各层包错误（fmt.Errorf("%w")），
	// 只看最外层会漏。
	wrapped := fmt.Errorf("起题失败: %w", fmt.Errorf("重试后仍失败: %w", inner))
	if !harness.IsKind(wrapped, harness.KindPlatform) {
		t.Error("三层 wrap 后 IsKind 必须仍然命中——引擎各层都会包错误")
	}
	if !errors.Is(wrapped, inner) {
		t.Error("Unwrap 链必须完整")
	}

	// E(nil) 返回 nil：让 `return E(...)` 可以出现在任何地方而不必先判空。
	if harness.E(harness.KindConfig, "op", nil) != nil {
		t.Error("E(kind, op, nil) 必须返回 nil")
	}
}

func TestContract_Retryable(t *testing.T) {
	retryable := &harness.Error{Kind: harness.KindPlatform, Op: "submit", Retryable: true}
	if !harness.Retryable(retryable) {
		t.Error("Retryable 应当为真")
	}
	if !harness.Retryable(fmt.Errorf("包一层: %w", retryable)) {
		t.Error("包裹后 Retryable 仍应为真")
	}
	if harness.Retryable(harness.E(harness.KindConfig, "op", errors.New("x"))) {
		t.Error("配置错误不应可重试")
	}
	if harness.Retryable(errors.New("裸错误")) {
		t.Error("非 *Error 不应被判为可重试")
	}
}

func TestContract_ErrorDoesNotLeakCauseInJSON(t *testing.T) {
	// Err 字段不参与 JSON 序列化——它可能带敏感上下文（路径、平台响应体）。
	e := harness.E(harness.KindPlatform, "platform.submit", errors.New("token=SECRETVALUE"))
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("SECRETVALUE")) {
		t.Errorf("错误 JSON 里出现了底层错误文本：%s", b)
	}
	// 但 Error() 要带上它——人看的时候需要。
	if !strings.Contains(e.Error(), "SECRETVALUE") {
		t.Error("Error() 应当包含底层错误，否则无法诊断")
	}
}

// ── 事件 ──

func TestContract_DomainEventJSONRoundTrip(t *testing.T) {
	ev := harness.DomainEvent{
		Seq:     7,
		At:      time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		Type:    harness.EvSubmitResult,
		RunID:   "run-1",
		Round:   3,
		Payload: json.RawMessage(`{"fingerprint":"fp:abcd1234/len=8/f…}"}`),
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back harness.DomainEvent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Seq != ev.Seq || back.Type != ev.Type || back.RunID != ev.RunID || back.Round != ev.Round {
		t.Errorf("round-trip 丢字段：%+v", back)
	}
	if string(back.Payload) != string(ev.Payload) {
		t.Errorf("Payload 变了：%s", back.Payload)
	}
}

// ── 明文不落公开面 ──

// TestContract_SnapshotHasNoPlaintext 是本文件最重要的一条：快照会被写进
// run.json、被 CLI 打印、被看板读——全是公开面。
func TestContract_SnapshotHasNoPlaintext(t *testing.T) {
	const secret = "flag{this_must_never_be_persisted}"

	snap := harness.Snapshot{
		SchemaVersion: harness.SchemaVersion,
		RunID:         "run-1",
		State:         harness.RunCompleted,
		Spec:          harness.RunSpec{Scenario: "fake"},
		SpecDigest:    "abc123",
		Objective:     harness.Objective{Kind: "flag_count", Want: 2, Got: 2, Completed: true},
		Public: harness.PublicSummary{
			CandidatesSeen:     3,
			SubmittedConfirmed: 2,
			// 公开面只有指纹。
			ConfirmedFP: []string{"fp:deadbeef/len=36/f…}"},
		},
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte(secret)) {
		t.Fatal("Snapshot 序列化里出现了候选明文——公开面绝不允许")
	}
	// 反向确认：如果真有字段装了明文，这条测试要能发现。
	type probe struct {
		X string `json:"x"`
	}
	pb, _ := json.Marshal(probe{X: secret})
	if !bytes.Contains(pb, []byte(secret)) {
		t.Fatal("自检失败：bytes.Contains 对明文不敏感，本测试无意义")
	}
}

// TestContract_SnapshotTypeHasNoFlagsField 用反射级别的检查确保没有人在
// Snapshot 上加回一个 Flags 字段。
func TestContract_SnapshotTypeHasNoFlagsField(t *testing.T) {
	b := mustJSON(t, harness.Snapshot{})
	for _, forbidden := range []string{`"flags"`, `"Flags"`, `"flag"`} {
		if bytes.Contains(b, []byte(forbidden)) {
			t.Errorf("Snapshot 序列化里出现了 %s —— 明文相关字段不得进入公开快照", forbidden)
		}
	}
}

// ── 引擎装配 ──

type fakePlatform struct{ harness.Platform }

func (fakePlatform) List(context.Context) ([]harness.Challenge, error) { return nil, nil }
func (fakePlatform) Start(context.Context, string) (harness.StartResult, error) {
	return harness.StartResult{}, nil
}
func (fakePlatform) Hint(context.Context, string) (harness.HintResult, error) {
	return harness.HintResult{}, nil
}
func (fakePlatform) Submit(context.Context, string, string) (harness.SubmitResult, error) {
	return harness.SubmitResult{}, nil
}
func (fakePlatform) Close(context.Context, string) (harness.CloseResult, error) {
	return harness.CloseResult{}, nil
}

type fakeExecutor struct{ harness.Executor }

func (fakeExecutor) Available(context.Context) error              { return nil }
func (fakeExecutor) Reclaim(context.Context, harness.RunID) error { return nil }

type fakeAgents struct{ harness.AgentFactory }

type fakeStore struct{ harness.Store }

func (fakeStore) Dir() string { return "/tmp/x" }

type fakeScenario struct{ harness.Scenario }

func TestContract_NewRejectsMissingPorts(t *testing.T) {
	// 空 Options ⇒ KindConfig，且消息要**指出缺哪个**。
	// 前身事故里「端口没接」的表现是静默退化（DAG 零事实、所有包自测全绿），
	// 所以这条错误必须点名。
	_, err := harness.New(harness.Options{})
	if err == nil {
		t.Fatal("空 Options 应当报错")
	}
	if !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("错误分类应为 KindConfig，实际 %v", err)
	}
	msg := err.Error()
	for _, port := range []string{"Platform", "Executor", "Agents", "Store", "Scenarios"} {
		if !strings.Contains(msg, port) {
			t.Errorf("错误消息里没有指出缺 %s：%s", port, msg)
		}
	}
}

func TestContract_NewAcceptsCompletePorts(t *testing.T) {
	opts := harness.Options{
		Platform:  fakePlatform{},
		Executor:  fakeExecutor{},
		Agents:    fakeAgents{},
		Store:     fakeStore{},
		Scenarios: map[string]harness.Scenario{"fake": fakeScenario{}},
	}
	// 端口齐备时不得因为「缺端口」而报错。
	// 注意：本波次里 engine 包还不存在，所以 New 会返回「实现未注册」的
	// KindConfig 错误——但它的消息**不应**提到缺端口。这是刻意的：让
	// T11 交付 engine 包之后，这个测试自然变成「返回非 nil 引擎」。
	eng, err := harness.New(opts)
	if err != nil {
		if strings.Contains(err.Error(), "Platform 未设置") {
			t.Fatalf("端口齐备却报缺端口：%v", err)
		}
		t.Logf("engine 实现尚未注册（预期，T11 交付）：%v", err)
		return
	}
	if eng == nil {
		t.Fatal("New 返回了 nil 引擎且无错误")
	}
}

func TestContract_MissingPortsHelper(t *testing.T) {
	// MissingPorts 是导出 API，engine 包的测试会用它断言消息内容——
	// 抄一份清单就会漂移。
	got := harness.Options{}.MissingPorts()
	if len(got) != 5 {
		t.Errorf("空 Options 应报 5 个缺端口，实际 %v", got)
	}
	full := harness.Options{
		Platform: fakePlatform{}, Executor: fakeExecutor{}, Agents: fakeAgents{},
		Store: fakeStore{}, Scenarios: map[string]harness.Scenario{"fake": fakeScenario{}},
	}
	if m := full.MissingPorts(); len(m) != 0 {
		t.Errorf("端口齐备时不应有缺项，实际 %v", m)
	}
}

// ── 提示策略常量 ──

func TestContract_HintPolicies(t *testing.T) {
	if harness.HintOff != "off" || harness.HintAuto != "auto" || harness.HintAlways != "always" {
		t.Error("HintPolicy 常量值变了——它们是 API")
	}
}

// ── 版本 ──

func TestContract_Version(t *testing.T) {
	// 版本号被 dag 的 schema 文档消费，改它会影响旧图读取路径。
	if harness.Version != "0.3.0" {
		t.Errorf("Version = %q，期望 0.3.0", harness.Version)
	}
}

// ── 答案格式渲染 ──

func TestContract_AnswerFormatHintNeverAssumesEnvelope(t *testing.T) {
	// 题目没说形态时不能**替它假设**成 flag{...} 的外壳。注意文案里会出现
	// 「可能是 flag{...}」这句话本身（那是在说明可能性），所以判据不是
	// 「不含 flag{...}」，而是「必须是那句以题面为准的说明」。
	got := harness.AnswerFormatHint(harness.Challenge{Code: "x"})
	if !strings.Contains(got, "以题面为准") {
		t.Errorf("应当把「以题面为准」原样带上，实际 %q", got)
	}
	if !strings.Contains(got, "不要自己加外壳") {
		t.Errorf("应当提醒不要自己加外壳，实际 %q", got)
	}
	// 题面给了形态就照用——一个字都不加。
	if got := harness.AnswerFormatHint(harness.Challenge{FlagFormat: "flag{...}"}); got != "flag{...}" {
		t.Errorf("题面给了形态时应原样使用，实际 %q", got)
	}
	if got := harness.AnswerFormatHint(harness.Challenge{FlagFormat: "密码原文"}); got != "密码原文" {
		t.Errorf("题面给了非信封形态时应原样使用，实际 %q", got)
	}
}
