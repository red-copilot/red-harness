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

// TestRunBundleFlagGoesThroughDeployOptions 钉住 `--bundle` 的**去向**。
//
// 它走部署级选项（`DeployOptions.BundleDir` → `wire.Options.Sandbox.ProfileDir`），
// **不写进 spec.Profile**。改前的做法是写进 Profile.ExtensionBundle，代价是 CLI
// 造出一份非空 profile、顶掉装配层的默认 profile，于是同一个 bundle 经 CLI 与经
// SDK 跑出两个不同的 ProfileDigest——实测：
//
//	SDK：profileDigest=d96751574f823ad8  bundleDigest=ab83809fda521ec9
//	CLI：profileDigest=445d6dc940f6960e  bundleDigest=ab83809fda521ec9
//
// bundle 摘要逐字相同而 profile 摘要不同，说明差在 profile 的其它字段（默认
// profile 的 Name）。「同一次实验」因此在报告里被拆成两组。
func TestRunBundleFlagGoesThroughDeployOptions(t *testing.T) {
	eng := &fakeRunner{res: harness.RunResult{RunID: "r-1", Scenario: "fake",
		Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}}}}}
	var gotDeploy DeployOptions
	a := newTestApp()
	a.Wire = func(_ string, _ harness.RunSpec, deploy DeployOptions) (Ports, error) {
		gotDeploy = deploy
		return Ports{Harness: eng}, nil
	}
	// 显式用相对路径调用：绝对化必须在 CLI 这一层做，因为下一站（executor 的
	// 只读挂载校验）要求绝对路径，而那时报错离用户输入太远。
	if code := dispatch([]string{"run", "--store", "/tmp/rh", "--bundle", "some/bundle"}, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d，期望 0", code)
	}
	if !filepath.IsAbs(gotDeploy.BundleDir) {
		t.Fatalf("DeployOptions.BundleDir = %q，期望绝对路径", gotDeploy.BundleDir)
	}
	if !strings.HasSuffix(gotDeploy.BundleDir, filepath.Join("some", "bundle")) {
		t.Errorf("DeployOptions.BundleDir = %q，期望以 some/bundle 结尾", gotDeploy.BundleDir)
	}
	if got := eng.specs[0].Profile.ExtensionBundle; got != "" {
		t.Errorf("spec.Profile.ExtensionBundle = %q，期望为空——由装配层用部署值补上，"+
			"否则 CLI 会顶掉默认 profile 并产生另一个 ProfileDigest", got)
	}
}

// TestRunBundleFlagClearsProfileBundle：同时给了 profile 文件与 --bundle 时，
// **flag 胜出**——与 `--image`/`--model` 覆盖装配默认值是同一条规矩。
//
// 实现方式是把文件里那个值清掉，让部署级的值去填（wire.resolve 只在运行级为空时
// 补）。这样既保住了优先级，又不让 CLI 自己造 profile。
func TestRunBundleFlagClearsProfileBundle(t *testing.T) {
	eng := &fakeRunner{res: harness.RunResult{RunID: "r-1", Scenario: "fake",
		Challenges: []harness.ChallengeResult{{Outcome: harness.OutcomeView{Reason: harness.ReasonSolved}}}}}
	var gotDeploy DeployOptions
	a := newTestApp()
	a.Wire = func(_ string, _ harness.RunSpec, deploy DeployOptions) (Ports, error) {
		gotDeploy = deploy
		return Ports{Harness: eng}, nil
	}
	path := writeProfile(t, `{"name":"p1","extensionBundle":"/from/profile"}`)
	if code := dispatch([]string{"run", "--store", "/tmp/rh",
		"--profile", path, "--bundle", "/from/flag"}, *a); code != 0 {
		t.Fatalf("dispatch(run) = %d，期望 0", code)
	}
	if gotDeploy.BundleDir != "/from/flag" {
		t.Fatalf("DeployOptions.BundleDir = %q，期望 /from/flag", gotDeploy.BundleDir)
	}
	if got := eng.specs[0].Profile.ExtensionBundle; got != "" {
		t.Errorf("文件里的 extensionBundle = %q，应被 --bundle 清掉（flag 更明确）", got)
	}
	// profile 的**其它**字段必须原样保留：清掉的只是 bundle 那一个入口。
	if got := eng.specs[0].Profile.Name; got != "p1" {
		t.Errorf("profile.Name = %q，期望 p1——--bundle 只该影响 bundle 那一个字段", got)
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
