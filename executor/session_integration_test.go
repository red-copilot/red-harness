//go:build integration

package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// v0.4 M1 出口门的真实容器验收：stub pi 明确证明自身运行在目标容器而非宿主，
// 且一次 fake 题目全生命周期成功。
//
// 与 docker_test.go 的分工：那个文件测的是 v0.3 的 Executor.Prepare/Exec 路径
// （起一个长睡容器再 docker exec）；本文件测 v0.4 的 Sandbox/SandboxSession 路径
// （NewSession → Probe → Launch 一个 attached 主进程 → Kill → Close）。
//
// 运行方式（**必须显式带 tag**，因为这些测试会改宿主的 iptables 与网络）：
//
//	go test -tags integration ./executor/... -count=1
//
// 前置条件：Docker daemon 可用、red-harness-runner:v0.3.0 已构建、以 root 运行
// （iptables 需要 CAP_NET_ADMIN）。测试镜像 red-harness-runner:teststub 由本文件
// 自己构建（见 buildStubImage）；**构建不出来时跳过**而不是失败——集成测试失败
// 应该意味着「隔离真的坏了」，而不是「这台机器没编出测试镜像」。
//
// 环境变量（沿用 docker_test.go 的约定）：
//   RH_TEST_KEEP=1      失败后保留现场（容器/网络/iptables 规则），方便手工排查
//   RH_TEST_STUB_IMAGE  v0.4 路径用的测试镜像 tag 覆盖
//
// # 本文件钉住的两条「只能实测、不能推理」的结论
//
//  1. Probe 的核验容器是**一次性**的：`docker run --rm` 跑完即消失，且不带 run
//     label、不挂 per-run 网络（TestV04ProbeVerifiesPiInImage 的容器计数断言、
//     TestV04ProbeUsesEphemeralContainer 的网络容器数断言）。
//  2. `attachedProcess.Wait()` 在**主进程自己退出**与**容器被 stop** 两条路径上
//     都可靠返回，且主进程自己退出时 attach 的退出码**就是容器主进程的退出码**
//     （TestV04WaitReturnsWhenMainProcessExits 断言 ExitCode()==7，
//     TestV04KillTerminatesProcessTree 断言 stop 之后 Wait 在超时内返回）。
//
// # 从分支 v04-container-lifecycle（48b132f）抢救回来的适配说明
//
// 本文件原样取自那个分支（**不整分支合并、不 cherry-pick 它的 session.go**——
// 那个分支落后 master 32 个提交，会退回 master 已有的凭据 env-file 等修正）。
// 它对 master 的 `executor/session.go` 做了三处适配，都是**契约差异**而非放宽断言：
//
//  1. 起主进程之前一律先 `Probe`：master 要求「未核验镜像内 pi 版本就不得 Launch」，
//     生产路径（v04.go 的 runChallenge）也是这个顺序。少这一步的话 Launch 会以
//     KindConfig 被拒，而那个错误**恰好**能让若干断言假绿（尤其
//     TestV04LaunchRejectsUnsafeWorkdir 与 TestV04FailurePathLeavesNoLeftovers）。
//  2. 断言「主进程在跑」用 `waitContainerRunning` 的有界轮询，不是 Launch 返回后
//     立刻 inspect：master 的 Launch 是 `docker create` + `docker start --attach`
//     的 attach 客户端 `Start()`，后者**只把 docker CLI 拉起来**，真正的 start
//     请求由它异步发出，所以立刻 inspect 大概率读到 "created"（本机实测 4/5）。
//     轮询超时仍返回非 running ⇒ 失败，所以「start 根本没发出去」那类事故照样会被
//     抓住。⚠️ 这是一处**真实的行为差异**（分支用的是同步的 `docker run --detach`），
//     已在汇报里单独列出。
//  3. `ProbeResult.Image` 只断言「解析成同一个 image ID」，不断言字符串等于 tag：
//     master 的 Probe 会把 tag 解析成 ID 再回报（更精确——ID 把那次核验钉死在一个
//     具体镜像上，tag 会漂移）。两个值都能通过 `docker image inspect` 解析。
//
// ⚠️ 已知红：`TestV04LaunchRejectsUnsafeWorkdir`。它钉的是分支的另一处修正
// （Launch 用 `ProcessSpec.Workdir` 直接覆盖 `p.WorkdirCtr`，**绕过** planRun 对
// `ExecutorSpec.Workdir` 的那两条校验），而 master 的 Launch 里没有这两条校验。
// 本文件按原样保留该用例及其断言，**没有**改动 master 的实现去迁就它——红线是
// 「先报告、不擅自扩大改动面」。

// defaultTestStubImage 是「runner + stub pi」的测试镜像 tag。
const defaultTestStubImage = "red-harness-runner:teststub"

func testStubImage() string {
	if v := os.Getenv("RH_TEST_STUB_IMAGE"); v != "" {
		return v
	}
	return defaultTestStubImage
}

// ── 测试镜像 ──

// repoRoot 返回仓库根目录（本测试文件所在包目录的上一级）。
//
// 用相对路径而不是 runtime.Caller：`go test` 的工作目录**就是**包目录
// （executor/），这是 go test 的既定语义。
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("..")
	if err != nil {
		t.Skipf("跳过：无法确定仓库根目录（%v）", err)
	}
	return abs
}

// buildStubImage 把 piai 的 stub pi 编成静态二进制，再用 runner/Dockerfile.teststub
// 打一个测试镜像，返回可用的 tag。
//
// 为什么构建失败要跳过而不是失败：这台机器可能编不出 Go 二进制、或者生产 runner
// 镜像还没构建。那两种情况都不说明「隔离坏了」。
//
// 为什么 stubpi 是**预先编好**再 COPY：构建上下文因此是一个临时目录，而不是
// runner/——runner/ 是生产镜像的目录，不该被塞进一个测试二进制。也因为这个，
// piai/testdata/stubpi 的源码一行都不用改（它归 piai 包所有）。
func buildStubImage(t *testing.T) string {
	t.Helper()
	tag := testStubImage()

	// 已经建好就直接复用（重复跑不重复构建，2 核机器上这一步要几十秒）。
	// 环境用与执行器同源的那一份（见 Docker.subprocessEnv）：这里是测试自己调
	// docker，但「连哪个 daemon」的口径不该与生产路径不同，否则一个设了
	// DOCKER_CONTEXT 的 shell 会让测试检查 A 端点、跑到 B 端点。
	if out, err := runCmd(context.Background(), "docker",
		subprocessEnvFor(harness.DefaultDockerEndpoint()), "image", "inspect", tag); err == nil && strings.TrimSpace(out) != "" {
		return tag
	}

	dir := t.TempDir()
	stubBin := filepath.Join(dir, "stubpi")
	build := exec.Command("go", "build", "-o", stubBin, "./piai/testdata/stubpi")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("跳过：无法编译 stubpi（%v）: %s", err, out)
	}

	dockerfile := filepath.Join(repoRoot(t), "runner", "Dockerfile.teststub")
	buildCmd := exec.Command("docker", "build", "-f", dockerfile, "-t", tag, dir)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Skipf("跳过：无法构建测试镜像 %s（%v）: %s", tag, err, out)
	}
	return tag
}

// ── v0.4 脚手架 ──

// newTestSession 构造一个测试用 Docker 并建一个 session（= 一个 run）。
//
// 每个用例一个独立 session，用后必须 Close（Close 会 Reclaim）。
func newTestSession(t *testing.T, runID harness.RunID) (*Docker, harness.SandboxSession, context.Context) {
	t.Helper()
	cfg := DefaultDockerConfig()
	// 集成测试用较短的管理超时：卡住时要快速失败，而不是把整轮预算耗掉。
	cfg.PrepareTimeout = 90 * time.Second
	cfg.CommandTimeout = 2 * time.Minute
	cfg.StopTimeout = 3
	d, err := NewDocker(cfg)
	if err != nil {
		t.Fatalf("NewDocker: %v", err)
	}
	ctx := context.Background()
	if err := d.Available(ctx); err != nil {
		t.Skipf("跳过：Docker 不可用（%v）", err)
	}
	img := buildStubImage(t)
	if _, err := d.run(ctx, nil, "image", "inspect", img); err != nil {
		t.Skipf("跳过：测试镜像 %s 不存在（%v）", img, err)
	}

	spec := harness.SandboxSpec{
		RunID:     runID,
		Target:    harness.Target{Code: "it-v04-target"},
		Image:     img,
		Workdir:   "/work",
		CPUs:      1.0,
		MemoryMB:  512,
		PidsLimit: 128,
	}
	ss, err := d.NewSession(ctx, spec)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { closeSession(t, d, ss) })
	return d, ss, ctx
}

// closeSession 关闭 session，并把「关闭失败」报成测试失败——一个删不掉的容器会
// 污染后续用例（网段被占、标签被扫到）。
func closeSession(t *testing.T, d *Docker, ss harness.SandboxSession) {
	t.Helper()
	if os.Getenv("RH_TEST_KEEP") == "1" && t.Failed() {
		t.Logf("RH_TEST_KEEP=1 且用例已失败：保留现场供排查")
		return
	}
	if err := ss.Close(context.Background()); err != nil {
		t.Errorf("SandboxSession.Close 失败: %v", err)
	}
}

// launchStubPi 起一个 stubpi 主进程。
//
// 先 Probe 再 Launch：这是契约要求的顺序（见 v04.go 的 runChallenge——它也是在
// 起 agent 之前先 Probe 拿容器工作目录），Launch 会在未核验时直接拒绝。Probe 的
// 核验容器是 `--rm` 的一次性容器，不带 run label，所以不改变本 run 的容器计数。
func launchStubPi(t *testing.T, ss harness.SandboxSession, ctx context.Context, extraEnv map[string]string) harness.ManagedProcess {
	t.Helper()
	if _, err := ss.Probe(ctx); err != nil {
		t.Fatalf("Launch 前 Probe: %v", err)
	}
	env := map[string]string{"STUBPI_SCENARIO": "normal"}
	for k, v := range extraEnv {
		env[k] = v
	}
	mp, err := ss.Launch(ctx, harness.ProcessSpec{
		Command: []string{"stubpi"},
		Env:     env,
		Workdir: "/work",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	return mp
}

// ── RPC 往返 ──

// rpcRoundTrip 往主进程写一帧 RPC 并读回一帧响应，返回整帧。
//
// 读用 bufio.Scanner 按 LF 切：stubpi 的输出恰好一行一个 JSON（与 piai 的
// frames.go 同款纪律）。超时用 goroutine + channel 实现——ManagedProcess 的
// Read 没有 ctx 版本，卡住时必须能报出来而不是把整个测试挂死。
func rpcRoundTrip(t *testing.T, mp harness.ManagedProcess, frame map[string]any, timeout time.Duration) map[string]any {
	t.Helper()
	b, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("序列化 RPC 帧: %v", err)
	}
	if _, err := mp.Write(append(b, '\n')); err != nil {
		t.Fatalf("写 RPC 帧失败: %v", err)
	}
	line, err := readFrame(mp, timeout)
	if err != nil {
		t.Fatalf("读 RPC 响应失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("响应不是 JSON（%q）: %v", line, err)
	}
	if got, _ := out["type"].(string); got != "response" {
		t.Fatalf("期望 response 帧，got %q（原帧 %q）", got, line)
	}
	return out
}

// readFrame 读一行并返回；超时即失败。
func readFrame(mp harness.ManagedProcess, timeout time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(mp)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		if !sc.Scan() {
			ch <- result{err: fmt.Errorf("stdout 读到 EOF（%v）", sc.Err())}
			return
		}
		ch <- result{line: sc.Text()}
	}()
	select {
	case r := <-ch:
		return r.line, r.err
	case <-time.After(timeout):
		return "", fmt.Errorf("读帧超时（%v）：stdio 通道没通", timeout)
	}
}

// assertRPCOK 断言一帧响应是成功的。
func assertRPCOK(t *testing.T, resp map[string]any, what string) {
	t.Helper()
	if ok, _ := resp["success"].(bool); !ok {
		t.Fatalf("%s 未成功: %v", what, resp)
	}
}

// ── 容器内事实 ──

// containerFactsScript 一次取回容器内的全部事实。
//
// 用**一次性脚本**而不是每条命令一次 docker exec：2 核的机器上进程启动本身
// 比这些命令贵得多。
//
// ⚠️ 语法纪律：这段脚本由 `sh`（dash）解释，**不能用 `$(( ... && ... ))`**——
// 那是 bash 的算术命令，dash 会直接报 `Syntax error: Missing '))'`，而后面
// 所有行都不会被执行（本机实测踩过：ROOTWRITE 之后的输出整段消失）。所以写
// 成功/失败判定一律用 `VAR=a; cmd && VAR=b` 的形式。
const containerFactsScript = `
echo "HOSTNAME=$(hostname)"
echo "DOCKERENV=$(test -f /.dockerenv && echo yes || echo no)"
echo "CGROUP=$(head -1 /proc/1/cgroup 2>/dev/null)"
echo "UID=$(id -u)"
echo "GID=$(id -g)"
ROOTWRITE=denied; touch /rh-rootfs-probe 2>/dev/null && ROOTWRITE=ok
echo "ROOTWRITE=$ROOTWRITE"
echo "WORKDIR=$(pwd)"
echo "WORK_TMPFS_KB=$(df -k --output=size /work 2>/dev/null | tail -1 | tr -d ' ')"
echo "TMP_TMPFS_KB=$(df -k --output=size /tmp 2>/dev/null | tail -1 | tr -d ' ')"
echo "DOCKERSOCK=$(test -e /var/run/docker.sock && echo present || echo absent)"
echo "PI_PATH=$(command -v pi)"
echo "PI_VERSION=$(pi --version 2>/dev/null)"
echo "STUBPI_PATH=$(command -v stubpi)"
echo "STUBPI_VERSION=$(stubpi --version 2>/dev/null)"
echo "HOSTREPO_VISIBLE=$(test -e /root/red-harness && echo yes || echo no)"
echo "HOSTPI_VISIBLE=$(test -e /root/.local/share/pi-node && echo yes || echo no)"
`

// containerFacts 取容器内的两组事实并合并：
//   - 容器内**另一个**进程的视角（docker exec，用同一个容器 ID）；
//   - 容器自身的权威属性（docker inspect，宿主侧读，不是容器内进程的自述）。
//
// 为什么要两者：exec 视角证明「这个容器里的事实对任何进程都成立」，inspect
// 视角证明「宿主上看到的就是这些属性」。两者都取自**同一个容器 ID**，所以
// 主进程（stubpi）跑在里面这件事是被这个 ID 锚定的。
func containerFacts(t *testing.T, d *Docker, ss harness.SandboxSession) map[string]string {
	t.Helper()
	id := containerIDOf(t, ss)
	facts := inspectFacts(t, d, id)

	out, err := d.run(context.Background(), nil, "exec", "--user", d.cfg.RunUser, "--workdir", "/work", id, "sh", "-c", containerFactsScript)
	if err != nil {
		t.Fatalf("容器内取事实失败: %v", err)
	}
	for k, v := range parseKV(out) {
		facts[k] = v
	}
	return facts
}

// inspectFacts 用 `docker inspect` 取容器自身的权威属性。
func inspectFacts(t *testing.T, d *Docker, id string) map[string]string {
	t.Helper()
	format := strings.Join([]string{
		"{{.Id}}",
		"{{.Config.Hostname}}",
		"{{.State.Status}}",
		"{{.HostConfig.ReadonlyRootfs}}",
		"{{.HostConfig.NetworkMode}}",
		"{{.Config.User}}",
	}, "\t")
	out, err := d.run(context.Background(), nil, "inspect", "--format", format, id)
	if err != nil {
		t.Fatalf("inspect %s 失败: %v", id, err)
	}
	parts := strings.Split(strings.TrimSpace(out), "\t")
	if len(parts) < 6 {
		t.Fatalf("inspect 输出字段不足: %q", out)
	}
	return map[string]string{
		"INSPECT_ID":         parts[0],
		"INSPECT_HOSTNAME":   parts[1],
		"INSPECT_STATUS":     parts[2],
		"INSPECT_READONLY":   parts[3],
		"INSPECT_NETWORK":    parts[4],
		"INSPECT_CONFIGUSER": parts[5],
	}
}

// containerIDOf 从 session 取本次主进程的容器 ID（走契约面 Probe）。
func containerIDOf(t *testing.T, ss harness.SandboxSession) string {
	t.Helper()
	pr, err := ss.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if pr.ContainerID == "" {
		t.Fatal("Launch 之后 Probe 必须回报 ContainerID")
	}
	return pr.ContainerID
}

// parseKV 解析 `KEY=value` 行。
func parseKV(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m
}

// hostCgroup 返回宿主 PID 1 的 cgroup 行，用于与容器内对比。
func hostCgroup(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}

// ── 零遗留断言 ──

// assertNoLeftovers 断言本次 run 在宿主上**零遗留**：容器、网络、iptables 规则。
//
// 三条都要查，因为它们是三条独立的回收路径（removeContainersByRun /
// removeNetworksByRun / uninstallRulesByRun），任何一条漏掉都会攒下孤儿：
// 孤儿容器占内存、孤儿 bridge 占网段（攒够之后新 run 连网络都建不出来）、
// 孤儿规则会掐掉别的 run 的流量。
func assertNoLeftovers(t *testing.T, d *Docker, runID harness.RunID) {
	t.Helper()
	ctx := context.Background()

	if out, err := d.run(ctx, nil, "ps", "--all", "--filter", "label="+LabelRun+"="+string(runID), "--format", "{{.ID}} {{.Names}} {{.Status}}"); err != nil {
		t.Fatalf("列容器失败: %v", err)
	} else if s := strings.TrimSpace(out); s != "" {
		t.Errorf("遗留容器（run=%s）: %q", runID, s)
	}

	if out, err := d.run(ctx, nil, "network", "ls", "--filter", "label="+LabelRun+"="+string(runID), "--format", "{{.ID}} {{.Name}}"); err != nil {
		t.Fatalf("列网络失败: %v", err)
	} else if s := strings.TrimSpace(out); s != "" {
		t.Errorf("遗留网络（run=%s）: %q", runID, s)
	}

	if !d.cfg.ManageIptables {
		return
	}
	// 注释里带 owner（见 commentFor）：残留检查要按**本部署**的注释查，
	// 否则两个部署各有一个同名 run 时，这里会把别人的规则报成「我的残留」。
	comment := commentFor(harness.OwnerID(d.cfg.Owner), runID)
	for _, chain := range []string{chainForward, chainInput} {
		out, err := d.iptablesOut(ctx, "-S", chain)
		if err != nil {
			// 链不存在 = 没有规则。iptables 整个不可用时也走这里，下面记一行日志
			// 让「没查成」可见，而不是静默通过。
			t.Logf("iptables -S %s 不可用（跳过该链的残留检查）: %v", chain, err)
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, comment) {
				t.Errorf("遗留 iptables 规则（%s, run=%s）: %s", chain, runID, line)
			}
		}
	}
}

// countContainersByRun 返回该 run 的容器数（含已停止的）。
func countContainersByRun(t *testing.T, d *Docker, runID harness.RunID) int {
	t.Helper()
	out, err := d.run(context.Background(), nil, "ps", "--all", "--filter", "label="+LabelRun+"="+string(runID), "--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("列容器失败: %v", err)
	}
	return len(fields(out))
}

// countContainersOnNetwork 返回挂在指定网络上的容器数（宿主侧权威读数）。
func countContainersOnNetwork(t *testing.T, d *Docker, name string) int {
	t.Helper()
	out, err := d.run(context.Background(), nil, "network", "inspect", name, "--format", "{{len .Containers}}")
	if err != nil {
		t.Fatalf("inspect 网络 %s 失败: %v", name, err)
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		t.Fatalf("解析网络容器数失败（%q）: %v", out, err)
	}
	return n
}

// planOf 取 session 内部渲染出来的 plan（测试与实现同包，但**只读**使用）。
func planOf(t *testing.T, ss harness.SandboxSession) runPlan {
	t.Helper()
	ds, ok := ss.(*dockerSession)
	if !ok {
		t.Fatalf("session 不是 *dockerSession: %T", ss)
	}
	return ds.plan
}

// ── 出口门 1：stub pi 证明自己在容器内 ──

// TestV04StubPiProvesItRunsInContainer 是 M1 出口门的核心断言：
// **stub pi 自己起的那个容器**与宿主是两个世界。
//
// 锚定方式：先用 RPC 握手证明主进程活着且 stdio 通了，再取**同一个容器 ID** 的
// 事实。这样任何一条隔离失效（容器实际是宿主 PID namespace、宿主工作目录被挂
// 进去了）都会被直接读出来，而不是靠「容器起来了」推断。
func TestV04StubPiProvesItRunsInContainer(t *testing.T) {
	d, ss, ctx := newTestSession(t, harness.RunID("it-v04-incontainer"))
	mp := launchStubPi(t, ss, ctx, nil)
	defer func() { _ = mp.Kill() }()

	resp := rpcRoundTrip(t, mp, map[string]any{"type": "get_state", "id": "1"}, 30*time.Second)
	assertRPCOK(t, resp, "get_state")
	if data, _ := resp["data"].(map[string]any); data == nil || fmt.Sprint(data["sessionId"]) == "" {
		t.Fatalf("get_state 响应缺少 sessionId: %v", resp)
	}

	facts := containerFacts(t, d, ss)

	// 1) hostname 必须与宿主不同：同 UTS namespace 的最直接表现就是同名。
	hostName, err := os.Hostname()
	if err != nil {
		t.Fatalf("取宿主 hostname: %v", err)
	}
	if facts["HOSTNAME"] == "" {
		t.Fatal("容器内 hostname 为空")
	}
	if facts["HOSTNAME"] == hostName {
		t.Errorf("容器 hostname 与宿主相同（%q）：UTS namespace 没有隔离", hostName)
	}
	// inspect 的 Hostname 必须与容器内 `hostname` 命令一致——证明我们读的确实是
	// 主进程所在的那个容器，而不是别的对象。
	if facts["INSPECT_HOSTNAME"] != facts["HOSTNAME"] {
		t.Errorf("inspect 的 Hostname=%q 与容器内 hostname=%q 不一致（读的可能不是同一个容器）",
			facts["INSPECT_HOSTNAME"], facts["HOSTNAME"])
	}

	// 2) 容器标记：/.dockerenv 存在，或 /proc/1/cgroup 与宿主不同。
	//    用「或」而不是「与」：cgroup v2 下容器内 /proc/1/cgroup 只有 `0::/`，
	//    /.dockerenv 是更稳的信号；两个都不成立才是问题。
	dockerEnv := facts["DOCKERENV"] == "yes"
	cgroupIsolated := facts["CGROUP"] != hostCgroup(t)
	if !dockerEnv && !cgroupIsolated {
		t.Errorf("容器内看不到任何容器化标记：/.dockerenv=%q cgroup=%q（宿主 cgroup=%q）",
			facts["DOCKERENV"], facts["CGROUP"], hostCgroup(t))
	}

	// 3) 宿主的工作目录、宿主 pi 运行时、docker socket 都不可见。
	if facts["HOSTREPO_VISIBLE"] == "yes" {
		t.Errorf("宿主仓库目录在容器里可见（挂载面失控）")
	}
	if facts["HOSTPI_VISIBLE"] == "yes" {
		t.Errorf("宿主 pi 运行时在容器里可见（容器必须用镜像自带那份）")
	}
	if facts["DOCKERSOCK"] != "absent" {
		t.Errorf("容器里出现了 docker socket（挂进去等于把宿主 root 交出去）: %q", facts["DOCKERSOCK"])
	}

	// 4) pi 与 stubpi 都来自镜像（绝对路径在 /usr/local/bin 下）。
	if facts["PI_PATH"] != "/usr/local/bin/pi" {
		t.Errorf("镜像里的 pi 应在 /usr/local/bin/pi, got %q", facts["PI_PATH"])
	}
	if facts["STUBPI_PATH"] != "/usr/local/bin/stubpi" {
		t.Errorf("镜像里的 stubpi 应在 /usr/local/bin/stubpi, got %q", facts["STUBPI_PATH"])
	}
}

// ── 出口门 2：Probe 在同一镜像内核验 pi 版本与位置 ──

// TestV04ProbeVerifiesPiInImage 钉住「Probe 在同一镜像中核验 pi 版本与运行位置」，
// 并钉住**核验容器零残留**。
//
// 零残留的判据是宿主侧计数，不是「代码里写了 --rm」：
//   - 本 run 的带 label 容器数在 Probe 前后不变（核验容器不带 run label，
//     因此既不该出现在这里，也不该被 Reclaim 误删）；
//   - 宿主上不存在任何以本 run 镜像为 image 的、正在运行的临时容器。
func TestV04ProbeVerifiesPiInImage(t *testing.T) {
	runID := harness.RunID("it-v04-probe")
	d, ss, ctx := newTestSession(t, runID)

	before := countContainersByRun(t, d, runID)

	pr, err := ss.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if pr.PiVersion == "" {
		t.Fatal("ProbeResult.PiVersion 为空：调用方只能读成「未核验」，而 v0.4 要求同一镜像内核验")
	}
	// 判据是「Probe 报的镜像 == 本次用的镜像」，而不是「字符串恰好等于 tag」：
	// 实现可以在 Probe 期间把 tag 解析成 image ID 并回报后者（更精确——ID 把
	// 那次核验钉死在一个具体镜像上，tag 会漂移）。所以两边都解析成 ID 再比。
	if got, want := imageIDOf(t, d, pr.Image), imageIDOf(t, d, testStubImage()); got != want {
		t.Errorf("ProbeResult.Image=%q（解析为 %s）不是本次镜像 %q（解析为 %s）", pr.Image, got, testStubImage(), want)
	}
	if pr.Workdir != "/work" {
		t.Errorf("ProbeResult.Workdir 应是容器内工作目录 /work, got %q", pr.Workdir)
	}
	// 镜像里的 pi 版本直接问镜像，免得把版本号写死进测试（写死会在基础镜像升级
	// 时变成假红）。Probe 报的必须与它一致——否则核验的不是同一个镜像。
	want := imagePiVersion(t, d)
	if pr.PiVersion != want {
		t.Errorf("Probe 报的 pi 版本 %q 与镜像里的 %q 不一致（核验的可能不是同一个镜像）", pr.PiVersion, want)
	}

	// ── 核验容器零残留（实测断言）──
	if after := countContainersByRun(t, d, runID); after != before {
		t.Errorf("Probe 之后本 run 的容器数从 %d 变成 %d：核验容器没有零残留", before, after)
	}
	// 直接查「有没有用本镜像起的容器」：核验容器是 docker run --rm，跑完就该
	// 从 `docker ps -a` 里消失。用祖先过滤而不是名字，避免依赖实现细节。
	out, err := d.run(ctx, nil, "ps", "--all", "--filter", "ancestor="+testStubImage(), "--format", "{{.ID}} {{.Names}} {{.Status}}")
	if err != nil {
		t.Fatalf("按镜像列容器失败: %v", err)
	}
	if s := strings.TrimSpace(out); s != "" {
		t.Errorf("宿主上仍有以 %s 为镜像的容器（Probe 的核验容器必须随 --rm 消失）: %q", testStubImage(), s)
	}

	// Launch 之后再 Probe 一次：必须仍回报同一个版本，且**不再**起核验容器。
	mp := launchStubPi(t, ss, ctx, nil)
	defer func() { _ = mp.Kill() }()
	pr2, err := ss.Probe(ctx)
	if err != nil {
		t.Fatalf("Launch 后 Probe: %v", err)
	}
	if pr2.PiVersion != pr.PiVersion {
		t.Errorf("Launch 前后 Probe 报的版本不一致: %q vs %q", pr.PiVersion, pr2.PiVersion)
	}
	if pr2.ContainerID == "" {
		t.Error("Launch 之后 Probe 必须回报 ContainerID")
	}
}

// imageIDOf 把一个镜像引用（tag 或 ID，两种形式都接受）解析成 image ID。
//
// 为什么需要它：Probe 可以在核验过程中把 tag 解析成 ID 并回报 ID（更精确），
// 而测试手里的那个是 tag。直接比字符串会把一个**实现选择**误判成缺陷。
func imageIDOf(t *testing.T, d *Docker, ref string) string {
	t.Helper()
	out, err := d.run(context.Background(), nil, "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		t.Fatalf("解析镜像 %q 失败: %v", ref, err)
	}
	return strings.TrimSpace(out)
}

// imagePiVersion 从镜像里读出 pi 的版本（在容器里跑，不是宿主上的 pi）。
func imagePiVersion(t *testing.T, d *Docker) string {
	t.Helper()
	out, err := d.run(context.Background(), nil, "run", "--rm", "--network", "none", "--read-only",
		"--user", d.cfg.RunUser, "--entrypoint", "sh", testStubImage(), "-c", "pi --version")
	if err != nil {
		t.Fatalf("读镜像内 pi 版本失败: %v", err)
	}
	return strings.TrimSpace(out)
}

// TestV04ProbeUsesEphemeralContainer 证明 Probe 的核验容器是**一次性**的，
// 且不挂在 per-run 网络上。
//
// 这条针对 Probe 的实现约束：它**不能**成为「起第二个长活进程」的口子。
// 核验容器挂到 per-run 网络上会在 Reclaim 之后留下一张被占用的网络（Docker 拒绝
// 删除仍有容器连接的网络），所以「网络上容器数为 0」是硬断言。
func TestV04ProbeUsesEphemeralContainer(t *testing.T) {
	runID := harness.RunID("it-v04-ephemeral")
	d, ss, ctx := newTestSession(t, runID)

	if _, err := ss.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if n := countContainersByRun(t, d, runID); n != 0 {
		t.Errorf("Probe 之后本 run 的容器数应为 0（核验容器必须随 --rm 消失）, got %d", n)
	}
	netName := planOf(t, ss).NetworkName
	if n := countContainersOnNetwork(t, d, netName); n != 0 {
		t.Errorf("per-run 网络 %s 上应没有任何容器（Probe 的核验容器不得挂上去）, got %d 个", netName, n)
	}
}

// ── 出口门 3：一次完整的 fake 题目全生命周期 ──

// TestV04SessionFullLifecycle 是 M1 出口门里「一次 fake 题目全生命周期成功」的
// 最小真实版本：NewSession → Probe → Launch（stubpi）→ RPC 握手 → Kill → Close，
// 然后断言零遗留。
func TestV04SessionFullLifecycle(t *testing.T) {
	runID := harness.RunID("it-v04-lifecycle")
	d, ss, ctx := newTestSession(t, runID)

	pr, err := ss.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if pr.PiVersion == "" {
		t.Fatal("ProbeResult.PiVersion 为空")
	}

	mp := launchStubPi(t, ss, ctx, nil)
	id := containerIDOf(t, ss)

	// 主进程必须真的在跑（不是「docker run 成功但进程立刻退了」）。
	if st := waitContainerRunning(t, d, id, 15*time.Second); st != "running" {
		t.Fatalf("Launch 之后容器状态应在超时内变成 running, got %q", st)
	}

	// 极简 RPC 握手：get_state。这一步证明 stdio 双向通道真的通了。
	resp := rpcRoundTrip(t, mp, map[string]any{"type": "get_state", "id": "1"}, 30*time.Second)
	assertRPCOK(t, resp, "get_state")
	if data, _ := resp["data"].(map[string]any); data == nil || fmt.Sprint(data["sessionId"]) == "" {
		t.Fatalf("get_state 响应缺少 sessionId: %v", resp)
	}

	// 再走一条真正的状态变更命令，证明不是「只能应答一次」的假通道。
	resp = rpcRoundTrip(t, mp, map[string]any{"type": "new_session", "id": "2"}, 30*time.Second)
	assertRPCOK(t, resp, "new_session")
	resp = rpcRoundTrip(t, mp, map[string]any{"type": "get_commands", "id": "3"}, 30*time.Second)
	assertRPCOK(t, resp, "get_commands")

	// Kill：停止容器 ⇒ 主进程与它起的工具子进程一起结束。
	if err := mp.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	assertContainerStopped(t, d, id)

	// Close：回收本次 run 的容器、规则、网络、代理。
	if err := ss.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertNoLeftovers(t, d, runID)
}

// waitContainerRunning 在有界超时内等到容器进入运行态，返回最终读到的状态。
//
// 为什么不「Launch 返回后立刻 inspect 并断言 running」：master 的 Launch 是
// `docker create`（同步，容器的初始状态就是 created）+ `docker start --attach`
// 的 attach 客户端 `Start()`（**只把 docker CLI 这个宿主进程拉起来**，真正的
// start 请求由它异步发出去）。所以 Launch 返回与容器转 running 之间有一个窗口，
// 立刻 inspect 大概率读到 "created"（本机实测 4/5）。
//
// 断言的内容不变——主进程最终**必须真的跑起来**，停住就是缺陷（「start 根本没
// 发出去」正是 attach 带了不存在的 flag 那类事故的形状）。变的只是不再断言一个
// 外部不可控的时序。轮询用尽仍未 running 时把状态原样返回，由调用方失败。
func waitContainerRunning(t *testing.T, d *Docker, id string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	status := ""
	for {
		status = inspectFacts(t, d, id)["INSPECT_STATUS"]
		if status == "running" || time.Now().After(deadline) {
			return status
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// assertContainerStopped 断言容器已经不在运行（stopped/exited/dead 都算）。
func assertContainerStopped(t *testing.T, d *Docker, id string) {
	t.Helper()
	status := inspectFacts(t, d, id)["INSPECT_STATUS"]
	if status == "running" {
		t.Errorf("Kill 之后容器仍在运行（停止不可靠）: %s", id)
	}
}

// ── 出口门 4：Wait 可靠返回（两条路径）+ 停止即可靠终止进程树 ──

// TestV04WaitReturnsWhenMainProcessExits 证明**主进程自己退出**时 Wait 可靠返回，
// 且 attach 的退出码**就是容器主进程的退出码**。
//
// 这条为什么重要：piai 的 startManagedProc 会起一个 goroutine 调 `m.Wait()`，
// 而「进程死亡」的判定就建立在它返回上。Wait 挂住 = 进程死亡永远检测不到 =
// 「0 回合 + 空错误」那种静默失败（前身被它咬过）。
//
// 用 `sh -c 'sleep 1; exit 7'` 而不是 stubpi 的 `--version-fail`（退出码 3）：
// 7 这个值在别处不出现，能排除「恰好撞上某个默认退出码」的可能。
func TestV04WaitReturnsWhenMainProcessExits(t *testing.T) {
	runID := harness.RunID("it-v04-waitexit")
	d, ss, ctx := newTestSession(t, runID)

	// 先 Probe：契约要求 Launch 之前必须核验过镜像内的 pi（v04.go 的 runChallenge
	// 也是这个顺序），未核验时 Launch 会直接拒绝。
	if _, err := ss.Probe(ctx); err != nil {
		t.Fatalf("Launch 前 Probe: %v", err)
	}
	mp, err := ss.Launch(ctx, harness.ProcessSpec{Command: []string{"sh", "-c", "sleep 1; exit 7"}, Workdir: "/work"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	id := containerIDOf(t, ss)

	waitErr := waitWithTimeout(t, mp, 30*time.Second)

	// 1) 容器侧权威退出码。
	code := containerExitCode(t, d, id)
	if code != 7 {
		t.Errorf("容器主进程的退出码应为 7, got %d", code)
	}
	// 2) attach 客户端的退出码必须等于它——这是「Wait 返回的是主进程退出状态」
	//    的实测判据，也是 piai 能区分「正常结束」与「异常结束」的依据。
	if waitErr == nil {
		t.Errorf("Wait 返回 nil，但主进程以 7 退出（attach 的退出码没有传递过来）")
	} else {
		var ee *exec.ExitError
		if !errors.As(waitErr, &ee) {
			t.Errorf("Wait 应返回 *exec.ExitError, got %T: %v", waitErr, waitErr)
		} else if ee.ExitCode() != 7 {
			t.Errorf("attach 的退出码应为 7（= 容器主进程退出码）, got %d", ee.ExitCode())
		}
	}

	// 3) Wait 必须可重复调用（契约要求幂等；piai 与调用方都会调）。
	if second := mp.Wait(); second == nil {
		t.Errorf("Wait 第二次调用应返回同样的非 nil 错误")
	}

	assertContainerStopped(t, d, id)
	if err := ss.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertNoLeftovers(t, d, runID)
}

// containerExitCode 读容器主进程的退出码（宿主侧权威读数）。
func containerExitCode(t *testing.T, d *Docker, id string) int {
	t.Helper()
	out, err := d.run(context.Background(), nil, "inspect", "--format", "{{.State.ExitCode}}", id)
	if err != nil {
		t.Fatalf("读容器退出码失败: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		t.Fatalf("解析退出码失败（%q）: %v", out, err)
	}
	return n
}

// TestV04KillTerminatesProcessTree 证明「停止容器即可靠终止 pi 及工具子进程」，
// 并且**被 stop 之后 Wait 也可靠返回**。
//
// 判据不是「容器没了」，而是**容器里的工具子进程真的死了**。做法：stubpi 支持
// STUBPI_SPAWN_CHILD，会起一个 `sleep 300` 子进程——那正是「pi 起的工具子进程」
// 的模型。Kill 之后用 `docker top` 确认整棵树都没了。
//
// 子进程 PID 文件写在 /work（有界 tmpfs）而不是宿主挂载：v0.4 不挂宿主可写目录，
// 这正是要一起验的。
func TestV04KillTerminatesProcessTree(t *testing.T) {
	runID := harness.RunID("it-v04-killtree")
	d, ss, ctx := newTestSession(t, runID)

	mp := launchStubPi(t, ss, ctx, map[string]string{"STUBPI_SPAWN_CHILD": "/work/child.pid"})
	id := containerIDOf(t, ss)

	// 等到子进程出现（stubpi 起来就 spawn，通常毫秒级）。
	var topBefore string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, err := d.run(ctx, nil, "top", id, "-eo", "pid,comm")
		if err == nil {
			topBefore = out
			if strings.Contains(out, "sleep") {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(topBefore, "sleep") {
		t.Fatalf("容器里没有看到 stubpi 起的工具子进程（docker top 输出: %q）", topBefore)
	}
	if !strings.Contains(topBefore, "stubpi") {
		t.Errorf("容器里没有看到主进程 stubpi（docker top 输出: %q）", topBefore)
	}

	if err := mp.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// 停止之后：容器不再是 running，且 docker top 不再能列出进程。
	// 两条都要查——只看状态会漏掉「容器标成 exited 但进程还在」这种不可靠停止。
	assertContainerStopped(t, d, id)
	if out, err := d.run(ctx, nil, "top", id, "-eo", "pid,comm"); err == nil {
		if strings.Contains(out, "sleep") || strings.Contains(out, "stubpi") {
			t.Errorf("Kill 之后容器里仍有进程在跑（停止不可靠）: %q", out)
		}
	}

	// 被 stop 之后 Wait 也必须返回（这是与上一条不同的路径：那条是自己退出）。
	_ = waitWithTimeout(t, mp, 30*time.Second)

	if err := ss.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertNoLeftovers(t, d, runID)
}

// waitWithTimeout 在超时内等 Wait 返回；超时即测试失败（挂住就是不可靠）。
func waitWithTimeout(t *testing.T, mp harness.ManagedProcess, timeout time.Duration) error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- mp.Wait() }()
	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		t.Fatalf("Wait 在 %v 内没有返回（停止不可靠）", timeout)
		return nil
	}
}

// ── 出口门 5：只允许一个主进程 + Workdir 校验 ──

// TestV04SecondLaunchRejected 钉住「Launch 只允许一个 attached 主进程」。
//
// 这条不是洁癖：第二个主进程意味着容器里有两份 pi，它们会各自开会话、各自跑工具，
// 而 RPC 通道只有一条——现场表现为「agent 的行为随机地像两个人」。
func TestV04SecondLaunchRejected(t *testing.T) {
	runID := harness.RunID("it-v04-oneproc")
	d, ss, ctx := newTestSession(t, runID)

	mp := launchStubPi(t, ss, ctx, nil)
	defer func() { _ = mp.Kill() }()

	_, err := ss.Launch(ctx, harness.ProcessSpec{Command: []string{"stubpi"}, Workdir: "/work"})
	if err == nil {
		t.Fatal("第二次 Launch 必须失败（一个 sandbox 只允许一个主进程）")
	}
	if !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("第二次 Launch 应是 KindConfig, got %v", err)
	}
	// 失败之后第一次那个主进程必须还活着（失败的 Launch 不该破坏已有的进程）。
	if st := waitContainerRunning(t, d, containerIDOf(t, ss), 15*time.Second); st != "running" {
		t.Errorf("第二次 Launch 失败后，第一个主进程的容器状态应为 running, got %q", st)
	}
	// 而且不得因此多出容器。
	if n := countContainersByRun(t, d, runID); n != 1 {
		t.Errorf("第二次 Launch 之后本 run 的容器数应为 1, got %d", n)
	}
}

// TestV04LaunchRejectsUnsafeWorkdir 钉住 Launch 覆盖 Workdir 时的校验。
//
// 根因（v0.4 最容易漏的一处）：planRun 校验的是 ExecutorSpec.Workdir，而 Launch
// 用 ProcessSpec.Workdir **直接覆盖** p.WorkdirCtr，绕过了那次校验。实测
// （Docker 29.8.0，只读 rootfs）`--workdir /does/not/exist` 与 `--workdir /tmp/a:b`
// 都会**静默成功**，于是失败推迟到 pi 落盘时才以别的形式出现。
func TestV04LaunchRejectsUnsafeWorkdir(t *testing.T) {
	runID := harness.RunID("it-v04-badworkdir")
	d, ss, ctx := newTestSession(t, runID)

	// 先 Probe：不 Probe 的话下面两次 Launch 会因为「未核验」被拒，而那个错误**恰好
	// 也是 KindConfig**——用例会全绿地通过，却一个字符的 Workdir 校验都没验到。
	if _, err := ss.Probe(ctx); err != nil {
		t.Fatalf("Launch 前 Probe: %v", err)
	}

	for _, wd := range []string{"relative/path", "/tmp/a:b"} {
		_, err := ss.Launch(ctx, harness.ProcessSpec{Command: []string{"stubpi"}, Workdir: wd})
		if err == nil {
			t.Errorf("Launch 的 Workdir=%q 必须被拒（它绕过了 planRun 的校验）", wd)
			continue
		}
		if !harness.IsKind(err, harness.KindConfig) {
			t.Errorf("Workdir=%q 的错误应是 KindConfig, got %v", wd, err)
		}
	}
	// 被拒的 Launch 不该起容器。
	if n := countContainersByRun(t, d, runID); n != 0 {
		t.Errorf("被拒的 Launch 不得起容器，got %d 个", n)
	}
	// 被拒之后 session 仍可用（失败不该污染状态）。
	mp := launchStubPi(t, ss, ctx, nil)
	defer func() { _ = mp.Kill() }()
	if st := waitContainerRunning(t, d, containerIDOf(t, ss), 15*time.Second); st != "running" {
		t.Errorf("被拒的 Launch 之后 session 应仍可用, got %q", st)
	}
}

// ── 出口门 6：ReclaimStale ──

// TestV04ReclaimStaleRemovesCrashLeftover 造一个「上次崩溃的遗留」，断言
// ReclaimStale 收掉它、且**不碰** live 里的 run。
//
// 为什么这条不能省：宿主重启后内存里的 run 列表没了，只有磁盘上的 run 目录还在。
// 没有这个扫描，重启前起的容器会永久占着网段与内存，而它们跑的 agent 已经没有任何
// 人在看了——那是一个无人监督的进攻性工具进程。
func TestV04ReclaimStaleRemovesCrashLeftover(t *testing.T) {
	crashID := harness.RunID("it-v04-crash-leftover")
	liveID := harness.RunID("it-v04-live-run")

	d, crashSS, ctx := newTestSession(t, crashID)
	// 起一个带 label 的遗留容器 + 网络，然后**不回收**（模拟宿主在 Reclaim 前崩溃）。
	crashMP := launchStubPi(t, crashSS, ctx, nil)
	crashContainer := containerIDOf(t, crashSS)

	// live 的那一个：同样起容器，但它必须被 ReclaimStale 放过。
	_, liveSS, _ := newTestSession(t, liveID)
	liveMP := launchStubPi(t, liveSS, ctx, nil)
	liveContainer := containerIDOf(t, liveSS)
	defer func() {
		_ = liveMP.Kill()
		_ = crashMP.Kill()
	}()

	if countContainersByRun(t, d, crashID) == 0 {
		t.Fatal("前置条件不成立：遗留 run 应有容器")
	}

	// live 里只有 liveID ⇒ crashID 是「重启后的孤儿」。
	out, err := d.ReclaimStale(ctx, map[harness.RunID]bool{liveID: true})
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	found := false
	for _, id := range out.Reclaimed {
		if id == crashID {
			found = true
		}
	}
	if !found {
		t.Errorf("ReclaimStale 应回收 %s, got %v", crashID, out.Reclaimed)
	}
	// 这一条是「别把别人报成自己人」的入口：crashID 是本部署的孤儿，所以它必须
	// 出现在**回收**清单里，而不是待定清单里。两者互换的表现是「孤儿永远不被收，
	// 而每次运行都报一堆待定」——没人会注意到，因为运行本身是成功的。
	for _, o := range out.Pending {
		if o.RunID == crashID {
			t.Errorf("本部署的孤儿被报成了待定（%+v）：它会被永久留在宿主上", o)
		}
	}

	// 遗留必须没了（容器与网络）。
	if out, _ := d.run(ctx, nil, "ps", "--all", "--filter", "id="+crashContainer, "--format", "{{.ID}}"); strings.TrimSpace(out) != "" {
		t.Errorf("ReclaimStale 之后遗留容器仍在: %q", out)
	}
	if out, _ := d.run(ctx, nil, "network", "ls", "--filter", "label="+LabelRun+"="+string(crashID), "--format", "{{.Name}}"); strings.TrimSpace(out) != "" {
		t.Errorf("ReclaimStale 之后遗留网络仍在: %q", out)
	}

	// 活跃 run 必须**一个都没少**：这是这个函数最危险的一处——收错对象等于把一个
	// 正在跑的进攻性工具进程连容器一起删掉。
	if out, _ := d.run(ctx, nil, "ps", "--all", "--filter", "id="+liveContainer, "--format", "{{.ID}}"); strings.TrimSpace(out) == "" {
		t.Errorf("活跃 run 的容器被 ReclaimStale 误删了（%s）", liveID)
	}
	if out, _ := d.run(ctx, nil, "network", "ls", "--filter", "label="+LabelRun+"="+string(liveID), "--format", "{{.Name}}"); strings.TrimSpace(out) == "" {
		t.Errorf("活跃 run 的网络被 ReclaimStale 误删了（%s）", liveID)
	}
}

// ── 出口门 7：三条结束路径都零遗留 ──

// TestV04CloseReclaimsContainerAndNetwork 正常路径：Close 必须回收容器与网络。
//
// 直接断言「Close 之后本 run 的容器与网络都没了」，而不是只看 Close 返回 nil。
func TestV04CloseReclaimsContainerAndNetwork(t *testing.T) {
	runID := harness.RunID("it-v04-close")
	d, ss, ctx := newTestSession(t, runID)

	mp := launchStubPi(t, ss, ctx, nil)
	id := containerIDOf(t, ss)
	netName := planOf(t, ss).NetworkName

	if n := countContainersByRun(t, d, runID); n != 1 {
		t.Fatalf("Close 前应有 1 个容器, got %d", n)
	}
	if out, _ := d.run(ctx, nil, "network", "ls", "--filter", "name=^"+netName+"$", "--format", "{{.Name}}"); strings.TrimSpace(out) == "" {
		t.Fatalf("Close 前网络 %s 应存在", netName)
	}

	if err := mp.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := ss.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if out, _ := d.run(ctx, nil, "ps", "--all", "--filter", "id="+id, "--format", "{{.ID}}"); strings.TrimSpace(out) != "" {
		t.Errorf("Close 之后容器仍在: %q", out)
	}
	if out, _ := d.run(ctx, nil, "network", "ls", "--filter", "name=^"+netName+"$", "--format", "{{.Name}}"); strings.TrimSpace(out) != "" {
		t.Errorf("Close 之后网络 %s 仍在: %q", netName, out)
	}
	assertNoLeftovers(t, d, runID)

	// Close 必须幂等（引擎的 defer 路径与显式路径都会调它）。
	if err := ss.Close(ctx); err != nil {
		t.Errorf("Close 必须幂等，第二次失败: %v", err)
	}
	// 关闭之后 Probe / Launch 都必须明确失败，而不是静默返回一个空结果。
	if _, err := ss.Probe(ctx); err == nil {
		t.Error("关闭之后 Probe 必须失败")
	}
	if _, err := ss.Launch(ctx, harness.ProcessSpec{Command: []string{"stubpi"}}); err == nil {
		t.Error("关闭之后 Launch 必须失败")
	}
}

// TestV04CancelPathLeavesNoLeftovers 取消路径：Launch 之后立刻取消 ctx，
// 资源仍必须被回收。
//
// 取消是最容易漏的一条：agent 的 Round 在 ctx 取消时会一路返回错误，而资源回收
// 必须在**独立**的、有界的 context 里做（不能拿已经取消的 ctx 去 Reclaim——那样
// 会立刻失败，而现场看起来「回收过了」）。
func TestV04CancelPathLeavesNoLeftovers(t *testing.T) {
	runID := harness.RunID("it-v04-cancel")
	d, ss, _ := newTestSession(t, runID)

	cancelCtx, cancel := context.WithCancel(context.Background())
	mp := launchStubPi(t, ss, cancelCtx, nil)
	id := containerIDOf(t, ss)
	if st := waitContainerRunning(t, d, id, 15*time.Second); st != "running" {
		t.Fatalf("取消前容器应在跑, got %q", st)
	}

	// 取消：模拟用户按 Ctrl-C / 墙钟到期。
	cancel()

	// 取消之后回收走**独立的** context。
	if err := mp.Kill(); err != nil {
		t.Errorf("取消路径 Kill: %v", err)
	}
	if err := ss.Close(context.Background()); err != nil {
		t.Fatalf("取消路径 Close: %v", err)
	}
	assertNoLeftovers(t, d, runID)
}

// TestV04FailurePathLeavesNoLeftovers 失败路径：主进程立刻死掉
// （stubpi --version-fail 往 stderr 写一行并以退出码 3 结束），资源仍必须被回收。
//
// 「失败」在这里是最短的那条：主进程没跑起来。它对应真实世界里「pi 的二进制被删了 /
// 依赖缺失」——那种情况下容器已经起来了，回收责任一样在。
func TestV04FailurePathLeavesNoLeftovers(t *testing.T) {
	runID := harness.RunID("it-v04-failure")
	d, ss, ctx := newTestSession(t, runID)

	// 先 Probe：不能因为「未核验」而被 Launch 拒掉——那样这条用例会走进
	// 「Launch 直接失败（可接受）」那个分支，于是**什么都没验**却显示通过。
	if _, err := ss.Probe(ctx); err != nil {
		t.Fatalf("Launch 前 Probe: %v", err)
	}
	mp, err := ss.Launch(ctx, harness.ProcessSpec{Command: []string{"stubpi", "--version-fail"}, Workdir: "/work"})
	if err != nil {
		// Launch 本身失败也可以接受——那样容器已经在 Launch 内部被清掉了。
		t.Logf("Launch 直接失败（可接受）: %v", err)
		if err := ss.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		assertNoLeftovers(t, d, runID)
		return
	}
	// 主进程很快以 3 退出；Wait 必须返回，Kill 仍然必须成功（幂等）。
	_ = waitWithTimeout(t, mp, 30*time.Second)
	if code := containerExitCode(t, d, containerIDOf(t, ss)); code != 3 {
		t.Errorf("stubpi --version-fail 的退出码应为 3, got %d", code)
	}
	if err := mp.Kill(); err != nil {
		t.Errorf("失败路径 Kill: %v", err)
	}
	if err := ss.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertNoLeftovers(t, d, runID)
}

// ── 出口门 8：隔离属性在真容器里成立 ──

// TestV04IsolationFactsInRealContainer 在真容器里断言 v0.4 的隔离属性：
// 非 root、只读 rootfs、/work 是有界 tmpfs、宿主状态不可见、挂在 per-run 网络上。
//
// 与 spec_test.go 的分工：那里断言**argv 上写了什么**（每台机器都能跑），这里
// 断言**容器里实际是什么**（只有装了 Docker 的机器能跑）。argv 写了但没生效的
// 情况（具名卷的坑、--user 被镜像 USER 覆盖）只有这一层能发现。
func TestV04IsolationFactsInRealContainer(t *testing.T) {
	runID := harness.RunID("it-v04-isolation")
	d, ss, ctx := newTestSession(t, runID)
	mp := launchStubPi(t, ss, ctx, nil)
	defer func() { _ = mp.Kill() }()

	facts := containerFacts(t, d, ss)

	// 1) 非 root。
	if facts["UID"] == "0" || facts["UID"] == "" {
		t.Errorf("容器内 uid 必须是数字且非 0, got %q", facts["UID"])
	}
	if facts["GID"] == "0" {
		t.Errorf("容器内 gid 不得是 0（只给 uid 时 gid 会落到 root）: %q", facts["GID"])
	}
	// docker inspect 的 Config.User 也必须是降权后的值。
	if u := facts["INSPECT_CONFIGUSER"]; !strings.HasPrefix(u, facts["UID"]+":") {
		t.Errorf("inspect 的 Config.User=%q 与容器内 uid=%q 不一致", u, facts["UID"])
	}

	// 2) 只读 rootfs：写 / 必须被拒。
	if facts["ROOTWRITE"] != "denied" {
		t.Errorf("只读 rootfs 下写 / 竟然成功了（307 GB 事故的防线失效）: %q", facts["ROOTWRITE"])
	}
	if facts["INSPECT_READONLY"] != "true" {
		t.Errorf("inspect 的 ReadonlyRootfs 应为 true, got %q", facts["INSPECT_READONLY"])
	}

	// 3) /work 是有界 tmpfs：容量必须等于 NewSession 配置的 64m（65536 KiB）。
	//    这条是「有界」的核心：无上限的 tmpfs 等于回到 307 GB 事故的起点。
	if got := facts["WORK_TMPFS_KB"]; got != "65536" {
		t.Errorf("/work 的 tmpfs 容量应为 65536 KiB (64m), got %q", got)
	}
	// /tmp 也是 tmpfs，且带 size= 上限（默认 128m = 131072 KiB）。
	if got := facts["TMP_TMPFS_KB"]; got != "131072" {
		t.Errorf("/tmp 的 tmpfs 容量应为 131072 KiB (128m), got %q", got)
	}
	// 工作目录必须就是它自己（不是别的地方）。
	if facts["WORKDIR"] != "/work" {
		t.Errorf("容器内工作目录应为 /work, got %q", facts["WORKDIR"])
	}

	// 4) 宿主状态不可见。
	if facts["DOCKERSOCK"] != "absent" {
		t.Errorf("容器里出现了 docker socket: %q", facts["DOCKERSOCK"])
	}
	if facts["HOSTPI_VISIBLE"] == "yes" {
		t.Errorf("宿主的 pi 运行时在容器里可见")
	}
	if facts["HOSTREPO_VISIBLE"] == "yes" {
		t.Errorf("宿主的仓库目录在容器里可见")
	}
	// 网络模式必须是本 run 的 per-run 网络（不是默认 bridge）。
	if nm := facts["INSPECT_NETWORK"]; !strings.HasPrefix(nm, "rh-net-") {
		t.Errorf("容器必须挂在 per-run 网络上（默认 bridge 等于没有网络边界）, got %q", nm)
	}
	// 镜像里的 pi 版本必须可读（M1 的版本核验在隔离层也要成立）。
	if facts["PI_VERSION"] != "0.85.1" {
		t.Errorf("镜像里的 pi 版本应为 0.85.1, got %q", facts["PI_VERSION"])
	}
	if facts["STUBPI_VERSION"] != "0.86.0" {
		t.Errorf("镜像里的 stubpi 版本应为 0.86.0, got %q", facts["STUBPI_VERSION"])
	}
}
