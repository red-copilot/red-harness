package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

// 本文件是 `NewSession` 的**离线**用例（不带 integration tag，不碰 Docker）。
//
// 为什么需要它：`session_integration_test.go` 里所有用例的工作目录都是 `/work`，
// 而 `/work` 恰好就是修复前硬编的那个值——所以那批用例**无论修复在不在都能通过**。
// 真正要钉住的是「调用方指定的容器内工作目录一路传到 --workdir 与 tmpfs，且绝不
// 落进宿主字段」，那件事只有换一个非 /work 的值才看得见。这里用一个桩 `docker`
// 可执行文件替掉真实 CLI：NewSession 在 ManageIptables=false 且 ProviderProxy=false
// 时只发一次外部调用（`docker network create`），桩把它记下来即可。

// fakeDocker 造一个桩 `docker`：把收到的 argv 逐行追加进日志，然后回一行假 ID。
//
// 用 argv 日志而不是「回放整条 docker run」：NewSession 这一步只做网络创建，
// 主进程的 argv 由 Launch 渲染——那部分在离线用例里直接调 runArgv 检查（见下）。
func fakeDocker(t *testing.T) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "docker-argv.log")
	bin = filepath.Join(dir, "docker")
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> '%s'\necho fake-network-id\n", argvLog)
	if err := os.WriteFile(bin, []byte(body), 0o700); err != nil {
		t.Fatalf("写桩 docker: %v", err)
	}
	return bin, argvLog
}

// newFakeDocker 用桩二进制构造执行器：关掉 iptables 与 provider 代理，
// 让 NewSession 的外部调用面收缩到只剩 `docker network create`。
func newFakeDocker(t *testing.T) (*Docker, string) {
	t.Helper()
	bin, argvLog := fakeDocker(t)
	d, err := NewDocker(DockerConfig{Binary: bin, ManageIptables: false, ProviderProxy: false})
	if err != nil {
		t.Fatalf("NewDocker: %v", err)
	}
	return d, argvLog
}

// TestNewSessionRejectsUnsafeContainerWorkdir 钉住 SandboxSpec.Workdir 的校验。
//
// 为什么这几条必须在**渲染之前**就拒掉（而不是留给 Docker）：
//   - 相对路径：Docker 会把它当**具名卷**静默挂成一个空卷——不是报错，而是
//     「工作目录里什么都没有」，agent 的表现会诡异到无法归因；
//   - 冒号：`-v src:dst` 与 `--tmpfs <path>:<opts>` 都靠冒号分隔，Linux 上
//     `/tmp/a:b` 是**合法目录名**，Docker 不会报错，语法却被拆坏。
func TestNewSessionRejectsUnsafeContainerWorkdir(t *testing.T) {
	d, argvLog := newFakeDocker(t)
	for _, wd := range []string{"", "   ", "relative/path", "./work", "/tmp/a:b"} {
		_, err := d.NewSession(context.Background(), harness.SandboxSpec{
			RunID: "it-workdir-validation", Image: "test-image", Workdir: wd,
		})
		if err == nil {
			t.Errorf("SandboxSpec.Workdir=%q 必须被拒", wd)
			continue
		}
		if !harness.IsKind(err, harness.KindConfig) {
			t.Errorf("Workdir=%q 的错误应是 KindConfig, got %v", wd, err)
		}
	}
	// 被拒的调用不得碰到 Docker：校验必须在渲染之前（留下半截状态最糟）。
	if b, err := os.ReadFile(argvLog); err == nil && strings.TrimSpace(string(b)) != "" {
		t.Errorf("被拒的 NewSession 不得调用 docker, got argv: %q", b)
	}
}

// TestNewSessionContainerWorkdirReachesArgv 钉住 v0.4 的工作目录整条通路：
// `SandboxSpec.Workdir`（容器内路径）→ `ExecutorSpec.Workdir` → `--workdir` 与
// 有界 tmpfs，且**不**落进宿主字段（`ExecSpec.Workdir` / `runPlan.WorkdirHost` /
// `Mounts`）。
//
// 用 `/work2` 而不是 `/work`：`/work` 就是修复前硬编的值，用它等于什么都没验。
func TestNewSessionContainerWorkdirReachesArgv(t *testing.T) {
	d, argvLog := newFakeDocker(t)
	ss, err := d.NewSession(context.Background(), harness.SandboxSpec{
		RunID: "it-session-workdir", Target: harness.Target{Code: "demo"},
		Image: "test-image", Workdir: "/work2",
		CPUs: 1, MemoryMB: 256, PidsLimit: 64,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ds, ok := ss.(*dockerSession)
	if !ok {
		t.Fatalf("session 不是 *dockerSession: %T", ss)
	}
	p := ds.plan

	// 1) 容器内工作目录必须跟着调用方走。
	if p.WorkdirCtr != "/work2" {
		t.Errorf("容器内工作目录应为 /work2, got %q（硬编 /work 会让 agent 的 cwd 落在只读 rootfs 上）", p.WorkdirCtr)
	}
	// 2) 有界 tmpfs 必须落在同一个目录上：写死在 /work 时它挂在一个不存在的
	//    目录上，而 agent 真正的工作目录在只读 rootfs 下没有任何可写区。
	wantTmpfs := "/work2:rw,nosuid,nodev,size=64m,mode=1777"
	if len(p.ExtraTmpfs) != 1 || p.ExtraTmpfs[0] != wantTmpfs {
		t.Errorf("工作目录 tmpfs 应为 %q, got %v", wantTmpfs, p.ExtraTmpfs)
	}
	// 3) 宿主侧：v0.4 一个宿主目录都不挂，且容器内路径不得出现在宿主字段里。
	if len(p.Mounts) != 0 {
		t.Errorf("v0.4 不得挂任何宿主目录, got %v", p.Mounts)
	}
	if p.WorkdirHost != "" {
		t.Errorf("runPlan.WorkdirHost 应被清空（宿主路径字段在 v0.4 无消费者）, got %q", p.WorkdirHost)
	}

	// 4) 渲染出来的 argv：--workdir 与 tmpfs 都是 /work2，且没有任何 -v。
	argv := runArgv(p)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--workdir /work2") {
		t.Errorf("docker run argv 里应出现 `--workdir /work2`, got %q", joined)
	}
	if !strings.Contains(joined, "--tmpfs "+wantTmpfs) {
		t.Errorf("docker run argv 里应出现 `--tmpfs %s`, got %q", wantTmpfs, joined)
	}
	if !strings.Contains(joined, "HOME=/work2/.home") {
		// HOME 必须落在**可写**的工作目录之下：只读 rootfs 下 pi 写不了会话目录，
		// 会以「静默失去扩展加载能力」的形式失败。
		t.Errorf("HOME 应落在工作目录下（/work2/.home）, got %q", joined)
	}
	if strings.Contains(joined, " -v ") {
		t.Errorf("v0.4 的 docker run 不得带任何 -v 挂载, got %q", joined)
	}
	if strings.Contains(joined, sessionPlaceholderHostDir) {
		t.Errorf("宿主工作目录占位值不得进 argv, got %q", joined)
	}

	// 5) 已知的外部调用面：这一次 NewSession 只发一条 `docker network create`
	//    （桩把每个参数写成一行，所以前两行就是子命令），且**不**起任何容器——
	//    主进程的启动只发生在 Launch 里。
	log, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("读桩 docker 的 argv 日志: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) < 2 || lines[0] != "network" || lines[1] != "create" {
		t.Errorf("NewSession 应只调用一次 `docker network create`, got %q", log)
	}
	for _, l := range lines {
		if l == "run" {
			t.Errorf("NewSession 不得起容器（那是 Launch 的事）, got %q", log)
		}
	}
}
