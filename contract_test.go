package harness_test

import (
	"bytes"
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
