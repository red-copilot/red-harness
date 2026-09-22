package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

// ── `--profile` / `--bundle`：CLI 与直接 SDK 调用同口径 ──
//
// 在这两个 flag 之前，CLI 路径**无法**配置 solver profile：`cmd/red-harness`
// 构造 wire.Options 时既不传 Profile 也不传 Sandbox.ProfileDir，于是
//
//   - profile 恒为 {Name:"default"}，ProfileDigest 每次运行都一样；
//   - ExtensionBundle 恒为空 ⇒ BundleDigest 恒为 ""（未核验）⇒
//     `stats --bundle` 在 CLI 路径上永远筛不出东西。
//
// 也就是说「按 bundle 分组」这个能力只对直接调 SDK 的调用方存在。这组用例钉的
// 就是这条口径差被消掉了。

// writeProfile 落一份 profile 文件，返回路径。
func writeProfile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunProfileFlagReachesRunSpec(t *testing.T) {
	eng := &fakeRunner{res: harness.RunResult{RunID: "r-1", Scenario: "fake",
		Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}}}}}
	a := newTestApp()
	a.Wire = func(string, harness.RunSpec, DeployOptions) (Ports, error) {
		return Ports{Harness: eng}, nil
	}
	path := writeProfile(t, `{"name":"p1","systemPrompt":"你是授权的 CTF 助手",
		"planner":{"dryRoundsBeforeHint":5},"promptPolicy":{"maxFacts":8,"maxNegative":4}}`)

	if code := dispatch([]string{"run", "--store", "/tmp/rh", "--profile", path}, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d，期望 0", code)
	}
	if len(eng.specs) != 1 {
		t.Fatalf("Run 调用次数 = %d，期望 1", len(eng.specs))
	}
	p := eng.specs[0].Profile
	if p.Name != "p1" || p.SystemPrompt != "你是授权的 CTF 助手" {
		t.Errorf("profile 基本字段没进 RunSpec: %+v", p)
	}
	if p.Planner.DryRoundsBeforeHint != 5 {
		t.Errorf("planner.dryRoundsBeforeHint = %d，期望 5", p.Planner.DryRoundsBeforeHint)
	}
	if p.PromptPolicy.MaxFacts != 8 || p.PromptPolicy.MaxNegative != 4 {
		t.Errorf("promptPolicy = %+v，期望 8/4", p.PromptPolicy)
	}
}

// TestRunProfileFlagRejectsBadConfigBeforeWiring 是这组里最重要的一条。
//
// 「在副作用之前失败」在 CLI 这一层的含义是：**装配函数根本没被调用**。装配会建
// 目录、起 bridge 子进程、取跨进程锁——一份写错的 profile 不该走到那里。
func TestRunProfileFlagRejectsBadConfigBeforeWiring(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"未知键", `{"planner":{"dryRoundBeforeHint":5}}`},
		{"类型错", `{"planner":{"dryRoundsBeforeHint":"5"}}`},
		{"越界", `{"promptPolicy":{"maxFacts":99999}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wired := false
			a := newTestApp()
			a.Wire = func(string, harness.RunSpec, DeployOptions) (Ports, error) {
				wired = true
				return Ports{Harness: &fakeRunner{}}, nil
			}
			code := dispatch([]string{"run", "--store", "/tmp/rh", "--profile", writeProfile(t, c.body)}, *a)
			if code != exitFailure {
				t.Fatalf("退出码 = %d，期望 %d（用法/配置错）", code, exitFailure)
			}
			if wired {
				t.Fatal("装配函数被调用了——配置错误必须在装配（副作用）之前拒绝")
			}
		})
	}
}

// TestRunBundleFlagIsAbsolute 钉住 bundle 路径的绝对化。
//
// bundle 会经 SandboxSpec.ProfileDir 走到 executor 的只读挂载校验，而那里要求
// **绝对路径**。相对路径若原样传下去，会在起容器那一刻才被拒——报错点离用户
// 输入太远，而且那时的失败形态是容器起不来，不是「你的参数写错了」。
func TestRunBundleFlagIsAbsolute(t *testing.T) {
	eng := &fakeRunner{res: harness.RunResult{RunID: "r-1", Scenario: "fake",
		Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}}}}}
	a := newTestApp()
	a.Wire = func(string, harness.RunSpec, DeployOptions) (Ports, error) {
		return Ports{Harness: eng}, nil
	}
	// 显式用相对路径调用。
	if code := dispatch([]string{"run", "--store", "/tmp/rh", "--bundle", "some/bundle"}, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d，期望 0", code)
	}
	got := eng.specs[0].Profile.ExtensionBundle
	if !filepath.IsAbs(got) {
		t.Fatalf("ExtensionBundle = %q，期望绝对路径", got)
	}
	if !strings.HasSuffix(got, filepath.Join("some", "bundle")) {
		t.Errorf("ExtensionBundle = %q，期望以 some/bundle 结尾", got)
	}
}

// TestRunBundleFlagOverridesProfile：flag 是更明确的那一次输入，与
// `--image`/`--model` 覆盖装配默认值同一条规矩。
func TestRunBundleFlagOverridesProfile(t *testing.T) {
	eng := &fakeRunner{res: harness.RunResult{RunID: "r-1", Scenario: "fake",
		Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}}}}}
	a := newTestApp()
	a.Wire = func(string, harness.RunSpec, DeployOptions) (Ports, error) {
		return Ports{Harness: eng}, nil
	}
	path := writeProfile(t, `{"extensionBundle":"/from/profile"}`)
	if code := dispatch([]string{"run", "--store", "/tmp/rh",
		"--profile", path, "--bundle", "/from/flag"}, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d，期望 0", code)
	}
	if got := eng.specs[0].Profile.ExtensionBundle; got != "/from/flag" {
		t.Fatalf("ExtensionBundle = %q，期望被 --bundle 覆盖成 /from/flag", got)
	}
}

// TestRunProfileFlagMissingFile：文件不存在是配置错误，不是「静默用默认 profile」。
//
// 静默回落在这里格外危险：用户以为加载了自己的 profile，而实际跑的是内置默认值，
// 而两者的 ProfileDigest 不同——报告里表现为「换了次实验」，没人会想到是路径写错。
func TestRunProfileFlagMissingFile(t *testing.T) {
	a := newTestApp()
	a.Wire = func(string, harness.RunSpec, DeployOptions) (Ports, error) {
		t.Fatal("装配函数不该被调用")
		return Ports{}, nil
	}
	if code := dispatch([]string{"run", "--store", "/tmp/rh",
		"--profile", filepath.Join(t.TempDir(), "nope.json")}, *a); code != exitFailure {
		t.Fatalf("退出码 = %d，期望 %d", code, exitFailure)
	}
}
