package harness

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// ── profile schema：解析、范围、以及在副作用之前失败 ──
//
// 这组用例对应 R1 的出口条件「配置拒绝用例」。它们要证明的不是「校验函数会
// 返回错误」，而是**错误发生在任何副作用之前**——所以每条用例都带一个记账型
// locker 与假 Sandbox/Scenario，断言它们的调用次数是 0。
//
// 为什么这条性质值得单独钉：配置错误是纯调用方错误，而它在旧实现里的表现恰恰
// 相反——拼错的键不报错、越界的值静默回落默认值，于是配置写错与配置没生效
// 在报告里长得一模一样（见 SolverProfile.Planner 的注释）。

// recordingLocker 记录锁是否真的被取过。
//
// 用它而不是 fakeRunLocker：后者不记账，于是「校验是否排在取锁之前」这条
// 断言会变成空转。
type recordingLocker struct {
	mu    sync.Mutex
	locks int
}

func (l *recordingLocker) Lock(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.locks++
	return nil
}

func (l *recordingLocker) Unlock() error { return nil }

func (l *recordingLocker) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.locks
}

func TestLoadProfileRejectsUnknownKey(t *testing.T) {
	// 拼错的键。旧实现里它静默无效、却仍然改变 ProfileDigest——两次本该可比的
	// 运行会被报告拆成两组。现在它在唯一的文本入口上被拒。
	_, err := LoadProfile(strings.NewReader(`{"planner":{"dryRoundBeforeHint":5}}`))
	if err == nil {
		t.Fatal("未知键应被拒绝")
	}
	if !IsKind(err, KindConfig) {
		t.Errorf("错误类别 = %v，期望 KindConfig", err)
	}
}

func TestLoadProfileRejectsWrongType(t *testing.T) {
	// 类型错：旧实现里 "5" 取不到 int，同样静默回落默认值。
	if _, err := LoadProfile(strings.NewReader(`{"planner":{"dryRoundsBeforeHint":"5"}}`)); err == nil {
		t.Fatal("字符串型数值应被拒绝")
	} else if !IsKind(err, KindConfig) {
		t.Errorf("错误类别 = %v，期望 KindConfig", err)
	}
	// 小数会被 encoding/json 读进 int 字段时报错，同样要落到 KindConfig。
	if _, err := LoadProfile(strings.NewReader(`{"promptPolicy":{"maxFacts":1.5}}`)); err == nil {
		t.Fatal("小数应被拒绝")
	} else if !IsKind(err, KindConfig) {
		t.Errorf("错误类别 = %v，期望 KindConfig", err)
	}
}

func TestLoadProfileRejectsTrailingContent(t *testing.T) {
	// 只读第一个 JSON 值会让一份拼接文件的前半段静默生效——那正是「看起来在、
	// 实际只生效了一半」的形状。
	_, err := LoadProfile(strings.NewReader(`{"name":"a"}{"name":"b"}`))
	if err == nil {
		t.Fatal("profile 之后的多余内容应被拒绝")
	}
	if !IsKind(err, KindConfig) {
		t.Errorf("错误类别 = %v，期望 KindConfig", err)
	}
}

func TestLoadProfileAcceptsValidAndValidates(t *testing.T) {
	p, err := LoadProfile(strings.NewReader(`{"name":"p1","planner":{"dryRoundsBeforeHint":5}}`))
	if err != nil {
		t.Fatalf("合法 profile 被拒: %v", err)
	}
	if p.Planner.DryRoundsBeforeHint != 5 {
		t.Errorf("dryRoundsBeforeHint = %d，期望 5", p.Planner.DryRoundsBeforeHint)
	}
	// 解析成功之后仍要过范围检查：文本进来的一份越界配置不能因为「JSON 合法」
	// 就放行。
	if _, err := LoadProfile(strings.NewReader(`{"planner":{"dryRoundsBeforeHint":20000}}`)); err == nil {
		t.Fatal("越界值应被拒绝")
	} else if !IsKind(err, KindConfig) {
		t.Errorf("错误类别 = %v，期望 KindConfig", err)
	}
}

// TestProfileValidateRanges 逐条钉住取值范围与「0 = 没配」这个约定。
func TestProfileValidateRanges(t *testing.T) {
	cases := []struct {
		name string
		p    SolverProfile
		ok   bool
	}{
		{"零值可用（= 没配，交给回落链）", SolverProfile{}, true},
		{"下界 1 可用", SolverProfile{Planner: PlannerConfig{DryRoundsBeforeHint: 1}}, true},
		{"上界 10000 可用", SolverProfile{PromptPolicy: PromptConfig{MaxFacts: 10000}}, true},
		{"越上界被拒", SolverProfile{PromptPolicy: PromptConfig{MaxFacts: 10001}}, false},
		{"负数被拒", SolverProfile{Planner: PlannerConfig{DryRoundsBeforeHint: -1}}, false},
		{"promptPolicy.maxNegative 同样受检", SolverProfile{PromptPolicy: PromptConfig{MaxNegative: 99999}}, false},
		// 两个渲染上限各自管不同的段（dag/render.go），所以
		// MaxNegative > MaxFacts **不是**错误——这里显式钉住，免得后来者
		// 顺手加一条不成立的跨字段约束。
		{"两段上限互不比较", SolverProfile{PromptPolicy: PromptConfig{MaxFacts: 4, MaxNegative: 12}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.p.Validate()
			if c.ok && err != nil {
				t.Errorf("应通过，却报 %v", err)
			}
			if !c.ok {
				if err == nil {
					t.Fatal("应被拒绝")
				}
				if !IsKind(err, KindConfig) {
					t.Errorf("错误类别 = %v，期望 KindConfig", err)
				}
			}
		})
	}
}

// TestProfileEmptyFollowsSchema：Empty 是「该不该回落到装配 profile」的唯一判据，
// 换成结构体之后它的零值判断必须跟着走。
func TestProfileEmptyFollowsSchema(t *testing.T) {
	if !(SolverProfile{}).Empty() {
		t.Error("零值 profile 应判为 Empty")
	}
	if (SolverProfile{Planner: PlannerConfig{DryRoundsBeforeHint: 2}}).Empty() {
		t.Error("配了 planner 的 profile 不应判为 Empty")
	}
	if (SolverProfile{PromptPolicy: PromptConfig{MaxFacts: 10}}).Empty() {
		t.Error("配了 promptPolicy 的 profile 不应判为 Empty")
	}
}

// TestRunRejectsInvalidConfigBeforeSideEffects 是这组用例的核心。
//
// 它同时钉三件事：
//  1. 非法配置以 KindConfig 失败（不是静默回落）；
//  2. **跨进程锁没有被取过**——校验排在 `locker.Lock` 之前，而 Lock 会写
//     `/run/lock/red-harness/run-<daemon 端点指纹>.lock`，那是一处真实副作用；
//  3. Sandbox 与 Scenario 一次都没被调用。
func TestRunRejectsInvalidConfigBeforeSideEffects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*RunSpec)
	}{
		{"planner 越界", func(s *RunSpec) { s.Profile.Planner.DryRoundsBeforeHint = 20000 }},
		{"promptPolicy 越界", func(s *RunSpec) { s.Profile.PromptPolicy.MaxFacts = -3 }},
		{"hintPolicy 非法值", func(s *RunSpec) { s.HintPolicy = "alwayss" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := &stubScenario{
				challenges: []Challenge{{Code: "c1", FlagCount: 1}},
				answers:    map[string]string{"c1": "flag{x}"},
			}
			sb := &fakeSandbox{}
			locker := &recordingLocker{}
			h, err := NewHarness(HarnessOptions{
				Scenario: sc, Sandbox: sb, Agents: &scriptedAgentFactory{agent: &fakeAgent{}},
				Gate:    func(Challenge) CandidateGate { return newStubGate() },
				Results: &auditResults{}, Locker: locker,
				Planner:  func(Challenge) Planner { return &stubPlanner{} },
				Renderer: func(Challenge) Renderer { return stubRenderer{} },
			})
			if err != nil {
				t.Fatalf("NewHarness: %v", err)
			}

			spec := testRunSpec()
			c.mut(&spec)
			res, err := h.Run(context.Background(), spec)
			if err == nil {
				t.Fatal("非法配置应被拒绝")
			}
			if !IsKind(err, KindConfig) {
				t.Errorf("错误类别 = %v，期望 KindConfig", err)
			}
			// State 的契约：Run 返回了错误，State 就必是 failed 或 cancelled。
			if res.State != RunFailed {
				t.Errorf("State = %q，期望 %q", res.State, RunFailed)
			}
			if n := locker.count(); n != 0 {
				t.Errorf("取锁 %d 次，期望 0（校验必须排在副作用之前）", n)
			}
			if sb.staleCalls != 0 || sb.sessions != 0 {
				t.Errorf("Sandbox 被调用：stale=%d sessions=%d，期望均为 0", sb.staleCalls, sb.sessions)
			}
			if sc.prepared != 0 {
				t.Errorf("Scenario.Prepare 被调用 %d 次，期望 0", sc.prepared)
			}
		})
	}
}

// TestRunAcceptsValidProfileLimits 是上面那条的反面对照：合法配置必须放行到
// 真的起 session——否则「零调用」可能是因为整条路径根本没通。
func TestRunAcceptsValidProfileLimits(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{val}"},
	}
	sb := &fakeSandbox{}
	locker := &recordingLocker{}
	h, err := NewHarness(HarnessOptions{
		Scenario: sc, Sandbox: sb, Agents: &scriptedAgentFactory{agent: &fakeAgent{}},
		Gate:    func(Challenge) CandidateGate { return newStubGate() },
		Results: &auditResults{}, Locker: locker,
		Planner:  func(Challenge) Planner { return &stubPlanner{} },
		Renderer: func(Challenge) Renderer { return stubRenderer{} },
	})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	spec := testRunSpec()
	spec.Profile = SolverProfile{Planner: PlannerConfig{DryRoundsBeforeHint: 3},
		PromptPolicy: PromptConfig{MaxFacts: 8, MaxNegative: 4}}
	spec.HintPolicy = HintAuto
	if _, err := h.Run(context.Background(), spec); err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}
	if locker.count() != 1 {
		t.Errorf("取锁 %d 次，期望 1", locker.count())
	}
	if sb.sessions != 1 {
		t.Errorf("session 数 = %d，期望 1", sb.sessions)
	}
}

// TestProfileSchemaIsStrictOnJSON：schema 化的一个直接收益是「没配」在 JSON 里
// 真的消失（omitzero），于是没用这两个键的配置保持旧的 RunSpec.Digest。
//
// 如果换成 omitempty，零值会序列化成 `"planner":{}`——摘要随之漂移，而摘要漂移
// 在报告里表现为「换了一次实验分组」。这条用例就是那个选择的防回归。
func TestProfileSchemaIsStrictOnJSON(t *testing.T) {
	b, err := json.Marshal(RunSpec{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "planner") || strings.Contains(string(b), "promptPolicy") {
		t.Errorf("未配置的 profile 不应出现在 JSON 里: %s", b)
	}
	// 配了就必须出现，且值可读。
	p, err := json.Marshal(RunSpec{Profile: SolverProfile{PromptPolicy: PromptConfig{MaxFacts: 7}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(p), `"maxFacts":7`) {
		t.Errorf("配置值应出现在 JSON 里: %s", p)
	}
}
