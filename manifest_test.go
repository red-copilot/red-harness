package harness

import (
	"context"
	"testing"
)

// ── 运行清单：冻结「实际生效的配置与产物身份」 ──
//
// 这组用例针对的是一类具体的事后问题：跑完一次 run，公开结果里只有 16 个十六
// 进制字符的 ProfileDigest，而摘要**不可逆**——它能证明两次运行不一样，却回答
// 不了「差在哪」。「这次跑的是 2 轮还是 5 轮阈值」「用的是哪个镜像」在落盘之后
// 一律无解，而这两样恰恰是复现一次实验所必需的。

// TestResolveDryRoundsPrecedence 钉住回落链的顺序。
//
// 顺序是契约：PolicySpec > profile.Planner > 内置默认。任一级改动都会让「我配了
// 5 轮」在报告里变成别的数，而报告看起来仍然正常。
func TestResolveDryRoundsPrecedence(t *testing.T) {
	cases := []struct {
		name string
		spec RunSpec
		want int
	}{
		{"都没有 ⇒ 内置默认", RunSpec{}, DefaultDryRoundsBeforeHint},
		{"只有 profile", RunSpec{Profile: SolverProfile{Planner: PlannerConfig{DryRoundsBeforeHint: 5}}}, 5},
		{"只有 policy", RunSpec{Policy: PolicySpec{DryRoundsBeforeHint: 7}}, 7},
		{"两者都有 ⇒ policy 胜",
			RunSpec{Policy: PolicySpec{DryRoundsBeforeHint: 7},
				Profile: SolverProfile{Planner: PlannerConfig{DryRoundsBeforeHint: 5}}}, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveDryRounds(c.spec); got != c.want {
				t.Fatalf("resolveDryRounds = %d，期望 %d", got, c.want)
			}
			if got := resolveManifest(c.spec).PlannerDryRounds; got != c.want {
				t.Fatalf("清单记的 %d 与实际生效的 %d 不一致——清单的全部意义就是它记生效值", got, c.want)
			}
		})
	}
}

// TestObserveProbeFreezesImageIdentity：镜像身份在第一次成功 Probe 时冻结，
// 后续题只做核对，**不覆盖**。
//
// 为什么不覆盖：覆盖会让「这次 run 用的是哪个镜像」变成**最后一道题**的答案。
// 一道题中途换了镜像的运行，前面那些题的结论就不再属于同一个实验条件了。
func TestObserveProbeFreezesImageIdentity(t *testing.T) {
	var m RunManifest
	m.observeProbe(ProbeResult{Image: "sha256:aaa", PiVersion: "0.85.1"})
	if m.Image != "sha256:aaa" || m.PiVersion != "0.85.1" {
		t.Fatalf("首次 Probe 应冻结身份，得到 %+v", m)
	}
	if m.ImageMismatch {
		t.Fatal("首次 Probe 不该报不一致")
	}

	// 同一镜像再报一次：无事发生。
	m.observeProbe(ProbeResult{Image: "sha256:aaa", PiVersion: "0.85.1"})
	if m.ImageMismatch {
		t.Fatal("同一镜像不该报不一致")
	}

	// 换了镜像：记账，但**不覆盖**已冻结的身份。
	m.observeProbe(ProbeResult{Image: "sha256:bbb", PiVersion: "0.86.0"})
	if !m.ImageMismatch {
		t.Fatal("镜像变了却没记账")
	}
	if m.Image != "sha256:aaa" || m.PiVersion != "0.85.1" {
		t.Fatalf("已冻结的身份被覆盖了: %+v", m)
	}

	// 空 Image 表示「未核验」，不是「换了个空镜像」——不得据此报不一致。
	var empty RunManifest
	empty.observeProbe(ProbeResult{Image: "sha256:aaa"})
	empty.observeProbe(ProbeResult{})
	if empty.ImageMismatch {
		t.Fatal("空 Image 表示未核验，不该被读成镜像不一致")
	}
}

// TestRunRecordsManifest 是清单的接线验收：一次真实编排跑完之后，公开产物里
// 能读到生效的阈值、实际用的镜像与 pi 版本。
func TestRunRecordsManifest(t *testing.T) {
	sc := &stubScenario{
		challenges: []Challenge{{Code: "c1", FlagCount: 1}},
		answers:    map[string]string{"c1": "flag{manifest}"},
	}
	sb := &fakeSandbox{}
	h := newTestHarness(t, sc, sb, &scriptedAgentFactory{agent: &fakeAgent{}},
		func(Challenge) CandidateGate { return newStubGate() })

	spec := testRunSpec()
	// 请求 3 轮；profile 里放一个不同的值，用来区分「请求值」与「生效值」。
	spec.Policy = PolicySpec{DryRoundsBeforeHint: 3}
	spec.Profile = SolverProfile{Planner: PlannerConfig{DryRoundsBeforeHint: 9}}
	res, err := h.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Manifest.PlannerDryRounds != 3 {
		t.Errorf("清单记的阈值 = %d，期望 3（PolicySpec 胜出，而不是 profile 里的 9）",
			res.Manifest.PlannerDryRounds)
	}
	if res.Manifest.HintPolicy != HintAuto {
		t.Errorf("HintPolicy = %q，期望折成 %q", res.Manifest.HintPolicy, HintAuto)
	}
	// testRunSpec 的 SandboxSpec.Image 是 "test-image"，假 session 的 Probe 把它
	// 原样回报成「解析后的镜像」。
	if res.Manifest.RequestedImage != "test-image" {
		t.Errorf("RequestedImage = %q，期望 %q", res.Manifest.RequestedImage, "test-image")
	}
	if res.Manifest.Image != "test-image" {
		t.Errorf("Image = %q，期望来自 Probe 的 %q——Probe 的返回值此前被 `_` 丢弃",
			res.Manifest.Image, "test-image")
	}
}

// TestManifestRequestedImageFallsBackToExecutor：镜像的回落与 runChallenge 里
// sb.Image 的回落同源（Sandbox 为空则取 Executor）。
func TestManifestRequestedImageFallsBackToExecutor(t *testing.T) {
	got := resolveManifest(RunSpec{Executor: ExecutorSpec{Image: "from-executor:v1"}})
	if got.RequestedImage != "from-executor:v1" {
		t.Fatalf("RequestedImage = %q，期望回落到 Executor 的镜像", got.RequestedImage)
	}
}
