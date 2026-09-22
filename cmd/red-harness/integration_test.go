//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/executor"
	"github.com/red-copilot/red-harness/internal/cli"
	"github.com/red-copilot/red-harness/piai"
	"github.com/red-copilot/red-harness/store"
)

type integrationSink struct {
	mu     sync.Mutex
	events []harness.Event
}

func (s *integrationSink) Emit(e harness.Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func (s *integrationSink) toolOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Kind == harness.EventToolEnd {
			return e.Output
		}
	}
	return ""
}

// TestIntegrationCLIFakeDocker exercises the v0.4 entrypoint with a real
// sandbox. The existing executor integration suite exercises the legacy port.
func TestIntegrationCLIFakeDocker(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("iptables integration requires root")
	}
	if err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").Run(); err != nil {
		t.Skipf("Docker unavailable: %v", err)
	}
	if err := exec.Command("docker", "image", "inspect", "red-harness-runner:v0.3.0").Run(); err != nil {
		t.Skipf("runner image unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	buildDir := t.TempDir()
	stub := filepath.Join(buildDir, "stubpi")
	build := exec.CommandContext(ctx, "go", "build", "-o", stub, "./piai/testdata/stubpi")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build stub pi: %v: %s", err, out)
	}
	dockerfile := []byte("FROM red-harness-runner:v0.3.0\nCOPY stubpi /opt/stubpi\nRUN chmod 0755 /opt/stubpi && ln -sf /opt/stubpi /usr/local/bin/pi\nENV STUBPI_SCENARIO=solve\n")
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), dockerfile, 0o600); err != nil {
		t.Fatal(err)
	}
	image := "red-harness-stubpi-it:" + strings.ReplaceAll(t.Name(), "/", "-")
	dockerBuild := exec.CommandContext(ctx, "docker", "build", "--quiet", "-t", image, buildDir)
	if out, err := dockerBuild.CombinedOutput(); err != nil {
		t.Fatalf("build test image: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if os.Getenv("RH_TEST_KEEP") != "1" {
			_ = exec.Command("docker", "image", "rm", "--force", image).Run()
		}
	})
	// First prove the stub process and its child tool see the same Docker
	// hostname, and that hostname identifies the container returned by Launch.
	argvLog := filepath.Join(buildDir, "docker-argv.log")
	wrapper := filepath.Join(buildDir, "docker-wrapper")
	wrapperBody := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> '%s'\nexec docker \"$@\"\n", argvLog)
	if err := os.WriteFile(wrapper, []byte(wrapperBody), 0o700); err != nil {
		t.Fatal(err)
	}
	canary := "test-provider-key-never-in-argv"
	credentialFile := filepath.Join(buildDir, "provider.env")
	if err := os.WriteFile(credentialFile, []byte("FAKE_PROVIDER_API_KEY="+canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := executor.NewDocker(executor.DockerConfig{Binary: wrapper, ProviderAllowHosts: []string{"opencode.ai"}})
	if err != nil {
		t.Fatal(err)
	}
	identityRun := harness.RunID("it-identity-" + strings.ReplaceAll(t.Name(), "/", "-"))
	ss, err := d.NewSession(ctx, harness.SandboxSpec{RunID: identityRun,
		Target: harness.Target{Code: "demo-1", Addrs: []string{"127.0.0.1:1"}}, Image: image, Workdir: "/work"})
	if err != nil {
		t.Fatalf("create identity sandbox: %v", err)
	}
	defer func() { _ = ss.Close(context.Background()) }()
	sink := &integrationSink{}
	ag, err := (&piai.Factory{EnvFile: credentialFile}).New(harness.AgentSpec{Provider: "fake-provider"}, ss, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ag.Close(context.Background()) }()
	if err := ag.Start(ctx, harness.AgentStart{Workdir: "/work"}); err != nil {
		t.Fatalf("start stub pi in sandbox: %v", err)
	}
	loggedArgv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(loggedArgv), canary) {
		t.Fatal("provider key appeared in Docker process arguments")
	}
	if _, err := ag.Round(ctx, harness.RoundRequest{Prompt: "offline identity", Round: 1}); err != nil {
		t.Fatalf("sandbox stub round: %v", err)
	}
	probe, err := ss.Probe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := sink.toolOutput()
	wantHost := probe.ContainerID
	if len(wantHost) > 12 {
		wantHost = wantHost[:12]
	}
	if !strings.Contains(output, "agent-host="+wantHost+" tool-host="+wantHost) {
		t.Fatalf("agent/tool container identity mismatch: id=%s output=%q", probe.ContainerID, output)
	}
	if err := ag.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ss.Close(ctx); err != nil {
		t.Fatal(err)
	}

	resultDir := t.TempDir()
	// `--bundle` 走生产路径：它同时决定只读挂进容器的目录与公开结果里的
	// BundleDigest。这条断言针对的是一个**真实存在过的口径差**——CLI 装配层
	// 此前既不传 Profile 也不传 Sandbox.ProfileDir，于是 CLI 跑出来的
	// BundleDigest **恒为空串**（未核验），`stats --bundle` 在 CLI 路径上永远
	// 筛不出东西，而同一个能力对直接调 SDK 的调用方是有的。
	bundleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundleDir, "ext.js"), []byte("// bundle v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := cli.Main([]string{"run", "--scenario", "fake", "--store", resultDir,
		"--image", image, "--submit", "--budget-rounds", "2",
		"--bundle", bundleDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("CLI exit %d: stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	rs, err := store.NewResultStore(resultDir)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := rs.List(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("result list: runs=%d err=%v", len(runs), err)
	}
	run := runs[0]
	if !run.Completed || len(run.Challenges) != 1 || run.Challenges[0].Outcome.Submitted != 1 {
		t.Fatalf("Fake platform did not confirm the sandbox answer: %+v", run)
	}
	// `--bundle` 必须真的走到内容摘要。空串在这个字段上的语义是「未核验」，
	// 所以它非空就证明 CLI 路径与直接 SDK 调用在这一项上同口径了。
	if run.BundleDigest == "" {
		t.Fatal("--bundle 没有产生 BundleDigest：CLI 路径与 SDK 路径仍然不同口径")
	}
	// 候选审计对账（R1 出口条件）：生产装配路径上必须真的落出审计行，且行数与
	// 公开面的提交数一致。这条**只能在真实装配里验**——离线用例用的是假件，
	// 证明不了「装起来之后这个端口是接上的」。
	auditPath := filepath.Join(resultDir, "private", string(run.RunID), "submissions.jsonl")
	auditBytes, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("生产路径没有落出候选审计 %s: %v", auditPath, err)
	}
	auditLines := strings.Split(strings.TrimSpace(string(auditBytes)), "\n")
	if len(auditLines) != run.Challenges[0].Outcome.Submitted {
		t.Fatalf("审计行数 = %d，公开的 Submitted = %d——两者必须对得上",
			len(auditLines), run.Challenges[0].Outcome.Submitted)
	}
	var rec struct {
		Flag        string `json:"flag"`
		Fingerprint string `json:"fingerprint"`
		Correct     bool   `json:"correct"`
		SubmittedAt string `json:"submittedAt"`
	}
	if err := json.Unmarshal([]byte(auditLines[0]), &rec); err != nil {
		t.Fatalf("审计行不是合法 JSON: %v", err)
	}
	if !rec.Correct || rec.Fingerprint == "" || rec.SubmittedAt == "" || rec.Flag == "" {
		t.Fatalf("审计行缺少「提交了什么、平台怎么判的」所需的字段: %+v", rec)
	}
	// 公开面不得出现这条明文。用审计行自己的 flag 当 canary，所以不需要在用例里
	// 写死任何真实答案。
	publicBytes, err := os.ReadFile(filepath.Join(resultDir, "results", string(run.RunID)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicBytes), rec.Flag) {
		t.Fatalf("公开结果泄漏了候选明文: %s", publicBytes)
	}
	for _, kind := range []string{"ps", "network ls"} {
		args := []string{"ps", "--all", "--quiet"}
		if kind == "network ls" {
			args = []string{"network", "ls", "--quiet"}
		}
		args = append(args, "--filter", "label=red-harness.run="+string(run.RunID))
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "" {
			t.Errorf("leftover %s for %s: %s (%v)", kind, run.RunID, out, err)
		}
	}
	for _, mode := range []string{"cancel", "startup-failure"} {
		t.Run(mode, func(t *testing.T) {
			runID := harness.RunID("it-" + mode + "-" + strings.ReplaceAll(t.Name(), "/", "-"))
			caseCtx, stop := context.WithCancel(ctx)
			defer stop()
			ss, err := d.NewSession(caseCtx, harness.SandboxSpec{RunID: runID,
				Target: harness.Target{Code: "demo-1", Addrs: []string{"127.0.0.1:1"}}, Image: image, Workdir: "/work"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ss.Close(context.Background()) }()
			if _, err := ss.Probe(caseCtx); err != nil {
				t.Fatal(err)
			}
			binary := "pi"
			if mode == "startup-failure" {
				binary = "/missing-pi"
			}
			agent, err := (&piai.Factory{BinPath: binary, EnvFile: credentialFile}).New(harness.AgentSpec{Provider: "fake-provider"}, ss, &integrationSink{})
			if err != nil {
				t.Fatal(err)
			}
			startErr := agent.Start(caseCtx, harness.AgentStart{Workdir: "/work"})
			if mode == "startup-failure" && startErr == nil {
				t.Fatal("missing pi unexpectedly started")
			}
			if mode == "cancel" {
				if startErr != nil {
					t.Fatal(startErr)
				}
				stop()
			}
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer closeCancel()
			if err := agent.Close(closeCtx); err != nil && err != io.EOF {
				t.Logf("agent close after %s: %v", mode, err)
			}
			if err := ss.Close(closeCtx); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"ps", "--all", "--quiet"}, {"network", "ls", "--quiet"}} {
				args = append(args, "--filter", "label=red-harness.run="+string(runID))
				out, err := exec.CommandContext(closeCtx, "docker", args...).CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "" {
					t.Fatalf("leftover after %s: args=%v output=%s err=%v", mode, args, out, err)
				}
			}
		})
	}
}
