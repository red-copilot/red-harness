//go:build integration

package executor

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// 集成测试：真的起容器、真的装 iptables 规则、真的回收。
//
// 运行方式（**必须显式带 tag**，因为这些测试会改宿主的 iptables 与网络）：
//
//	go test -tags integration ./executor/... -count=1
//
// 前置条件：Docker daemon 可用、red-harness-runner:v0.3.0 已构建、以 root 运行
// （iptables 需要 CAP_NET_ADMIN）。任一不满足时**跳过**而不是失败——集成测试
// 失败应该意味着「隔离真的坏了」，而不是「这台机器没装 Docker」。
//
// 环境变量：
//   RH_TEST_IMAGE   覆盖镜像（默认 red-harness-runner:v0.3.0）
//   RH_TEST_KEEP    置 1 时失败后不回收，方便手工进容器看现场

const defaultTestImage = "red-harness-runner:v0.3.0"

func testImage() string {
	if v := os.Getenv("RH_TEST_IMAGE"); v != "" {
		return v
	}
	return defaultTestImage
}

// newTestDocker 构造一个测试用执行器，并断言前置条件。
func newTestDocker(t *testing.T) (*Docker, context.Context) {
	t.Helper()
	cfg := DefaultDockerConfig()
	// 集成测试用较短的管理超时：卡住时要快速失败，而不是把 30 分钟预算耗掉。
	cfg.PrepareTimeout = 90 * time.Second
	cfg.CommandTimeout = 2 * time.Minute

	d, err := NewDocker(cfg)
	if err != nil {
		t.Fatalf("NewDocker: %v", err)
	}
	ctx := context.Background()
	if err := d.Available(ctx); err != nil {
		t.Skipf("跳过：Docker 不可用（%v）", err)
	}
	if _, err := d.run(ctx, nil, "image", "inspect", testImage()); err != nil {
		t.Skipf("跳过：测试镜像 %s 不存在（先跑 docker build -t %s runner/）", testImage(), testImage())
	}
	return d, ctx
}

// makeWorkdir 造一个宿主工作目录。**所有集成用例都挂它**——这是唯一允许
// 挂进容器的宿主路径。
func makeWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod 工作目录: %v", err)
	}
	return dir
}

// runSpec 造一份用于集成测试的 ExecSpec。
func runSpec(t *testing.T, runID harness.RunID, workdir string, hosts []string) harness.ExecSpec {
	t.Helper()
	return harness.ExecSpec{
		RunID: runID,
		Target: harness.Target{
			Code:  "it-target",
			Addrs: hosts,
		},
		Executor: harness.ExecutorSpec{
			Image:      testImage(),
			Workdir:    "/work",
			CPUs:       1.0,
			MemoryMB:   512,
			PidsLimit:  128,
			AllowHosts: hosts,
		},
		Workdir: workdir,
	}
}

// cleanup 回收，并把「回收失败」报成测试失败——一个删不掉的容器会污染
// 后续用例（网段被占、标签被扫到）。
func cleanup(t *testing.T, d *Docker, runID harness.RunID) {
	t.Helper()
	if os.Getenv("RH_TEST_KEEP") == "1" && t.Failed() {
		t.Logf("RH_TEST_KEEP=1 且用例已失败：保留 run=%s 的对象供排查", runID)
		return
	}
	if err := d.Reclaim(context.Background(), runID); err != nil {
		t.Errorf("Reclaim(%s) 失败: %v", runID, err)
	}
}

// ── 只读文件系统 ──

func TestIntegrationReadOnlyRootfs(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)
	runID := harness.RunID("it-readonly")

	h, err := d.Prepare(ctx, runSpec(t, runID, wd, nil))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d, runID)

	// 根文件系统的可写位置必须被拒。这条断言是 307 GB 事故的回归测试：
	// 没有它，一次 `dd if=/dev/zero of=/bigfile` 就能写满宿主。
	res, err := d.Exec(ctx, h, []string{"sh", "-c", "touch /should-fail"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("只读 rootfs 下写 / 竟然成功了（stdout=%q stderr=%q）", res.Stdout, res.Stderr)
	}
	if !strings.Contains(strings.ToLower(res.Stderr), "read-only") && !strings.Contains(strings.ToLower(res.Stderr), "permission") {
		t.Errorf("失败原因不是只读文件系统，可能是别的问题: %q", res.Stderr)
	}

	// 而 /tmp（tmpfs）与工作目录必须可写，否则 pi 起不来。
	res, err = d.Exec(ctx, h, []string{"sh", "-c", "touch /tmp/it-ok && echo TMP_OK"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "TMP_OK") {
		t.Errorf("tmpfs 应可写: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}

	res, err = d.Exec(ctx, h, []string{"sh", "-c", "touch /work/it-ok && echo WORK_OK"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "WORK_OK") {
		t.Errorf("工作目录应可写: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	// 工作目录里确实落到了**宿主**那个目录上（挂载真的生效，不是匿名卷）。
	if _, err := os.Stat(filepath.Join(wd, "it-ok")); err != nil {
		t.Errorf("容器写的文件没出现在宿主工作目录里（挂载没生效？）: %v", err)
	}
}

// 非 root：容器里的 uid 必须不是 0。这条断言必须在**真容器**里做——
// argv 上写了 --user 不等于容器里真的降权了。
func TestIntegrationRunsAsNonRoot(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)
	runID := harness.RunID("it-nonroot")

	h, err := d.Prepare(ctx, runSpec(t, runID, wd, nil))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d, runID)

	res, err := d.Exec(ctx, h, []string{"id", "-u"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) == "0" {
		t.Fatalf("容器以 root 运行（--user 没生效）: %q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("id 失败: %q", res.Stderr)
	}
}

// ── 资源限制 ──

func TestIntegrationResourceLimits(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)
	runID := harness.RunID("it-limits")

	spec := runSpec(t, runID, wd, nil)
	spec.Executor.MemoryMB = 256
	spec.Executor.PidsLimit = 64
	spec.Executor.CPUs = 1.0
	h, err := d.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d, runID)

	// 1) 内存上限真的落到了 cgroup 上。
	res, err := d.Exec(ctx, h, []string{"sh", "-c", "cat /sys/fs/cgroup/memory.max"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "268435456" {
		t.Errorf("memory.max 应为 256 MiB (268435456), got %q（stderr=%q）", got, res.Stderr)
	}

	// 2) PID 上限。
	res, err = d.Exec(ctx, h, []string{"sh", "-c", "cat /sys/fs/cgroup/pids.max"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "64" {
		t.Errorf("pids.max 应为 64, got %q", got)
	}

	// 3) CPU 上限：cpu.max 形如 "100000 100000"（一个核）。
	res, err = d.Exec(ctx, h, []string{"sh", "-c", "cat /sys/fs/cgroup/cpu.max"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); !strings.HasPrefix(got, "100000 ") {
		t.Errorf("cpu.max 应为 1 核（100000 <period>）, got %q", got)
	}

	// 4) 内存上限**真的会拒绝分配**：申请远超上限的内存必须失败。
	//    只看 cgroup 文件是「参数写对了」，看分配失败才是「限制生效了」。
	res, err = d.Exec(ctx, h, []string{"sh", "-c",
		`head -c 400m /dev/zero > /tmp/big 2>/dev/null && echo ALLOC_OK || echo ALLOC_DENIED`},
		harness.ExecOptions{Timeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(res.Stdout, "ALLOC_OK") {
		t.Errorf("256 MB 上限下写出 400 MB 竟然成功了（内存限制没生效）")
	}
}

// ── 墙钟超时 ──

func TestIntegrationWallClockTimeout(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)
	runID := harness.RunID("it-timeout")

	h, err := d.Prepare(ctx, runSpec(t, runID, wd, nil))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d, runID)

	start := time.Now()
	res, err := d.Exec(ctx, h, []string{"sleep", "60"}, harness.ExecOptions{Timeout: 3 * time.Second})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("超时应作为**结果**返回而不是执行器错误: %v", err)
	}
	if elapsed > 20*time.Second {
		t.Errorf("超时没有生效：跑了 %v", elapsed)
	}
	if res.ExitCode == 0 {
		t.Errorf("超时的命令不应报成功: %+v", res)
	}

	// 超时后容器必须还活着（超时杀的是那条命令，不是容器）。
	res, err = d.Exec(ctx, h, []string{"echo", "alive"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("超时后容器应仍可用: %v", err)
	}
	if !strings.Contains(res.Stdout, "alive") {
		t.Errorf("超时后容器不可用: %+v", res)
	}
}

// tcpProbe 返回一段在容器里探测 TCP 可达性的 bash 片段。
//
// **必须把 host:port 拆成 host/port**：bash 的 /dev/tcp 语法是
// `/dev/tcp/<host>/<port>`（斜杠），写成 `host:port` 会得到一个
// "No such file or directory"——而不是连接失败。这个区别很致命：
// 用冒号写法时「不可达」用例会**假通过**（它本来就期望失败），
// 而「可达」用例失败得莫名其妙（错误看起来像镜像里没有 bash 的 /dev/tcp 支持）。
//
// 用 /dev/tcp 而不是 nc：nc 的失败退出码在不同实现下不一致，/dev/tcp 更干净。
func tcpProbe(t *testing.T, hostport, okMarker, failMarker string) string {
	t.Helper()
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatalf("目标地址不是 host:port（%q）: %v", hostport, err)
	}
	return fmt.Sprintf(
		"timeout 8 bash -c 'exec 3<>/dev/tcp/%s/%s && head -1 <&3' && echo %s || echo %s",
		host, port, okMarker, failMarker)
}

// ── 网络边界 ──

// targetListener 在宿主上起一个 TCP 监听，模拟「TSecBench 返回的靶场端点」。
//
// 为什么绑在 0.0.0.0：容器到宿主走的是网桥，宿主的 127.0.0.1 在容器视角下是
// 容器自己的 loopback。真实靶场也是「宿主机可达的地址」，绑 0.0.0.0 更接近。
func targetListener(t *testing.T) (hostport string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("TARGET-OK\n"))
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	// 容器要连的宿主地址：默认路由的源地址（网桥能到的那个）。
	host := hostEgressIP(t)
	return net.JoinHostPort(host, port), func() { _ = ln.Close() }
}

// hostEgressIP 返回宿主在默认路由上的源地址（容器可达的那个）。
func hostEgressIP(t *testing.T) string {
	t.Helper()
	conn, err := net.Dial("udp", "1.1.1.1:80")
	if err != nil {
		t.Skipf("跳过：无法确定宿主出网地址（%v）", err)
	}
	defer conn.Close()
	host, _, _ := net.SplitHostPort(conn.LocalAddr().String())
	return host
}

// allowRuleReachesTarget：白名单里的目标端点必须可达。
func TestIntegrationAllowedTargetReachable(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)
	runID := harness.RunID("it-net-allow")

	target, stop := targetListener(t)
	defer stop()

	h, err := d.Prepare(ctx, runSpec(t, runID, wd, []string{target}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d, runID)

	// 用 bash 的 /dev/tcp（镜像里有 bash）；用 nc 也行，但 nc 的失败退出码
	// 在不同实现下不一致，/dev/tcp 更干净。
	// 白名单里的目标必须可达。
	res, err := d.Exec(ctx, h, []string{"bash", "-c", tcpProbe(t, target, "TARGET_OK", "TARGET_FAIL")},
		harness.ExecOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "TARGET_OK") {
		t.Errorf("白名单目标不可达（exit=%d stdout=%q stderr=%q）", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// 非授权端点必须不可达：这是「其他出站默认拒绝」的回归测试。
func TestIntegrationUnauthorizedEndpointUnreachable(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)
	runID := harness.RunID("it-net-deny")

	// 白名单里放一个**假的**地址，真目标故意不放进去。
	allowed, stopAllowed := targetListener(t)
	defer stopAllowed()
	secret, stopSecret := targetListener(t)
	defer stopSecret()

	h, err := d.Prepare(ctx, runSpec(t, runID, wd, []string{allowed}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d, runID)

	// 1) 非白名单的宿主端点：必须不可达。
	res, err := d.Exec(ctx, h, []string{"bash", "-c", tcpProbe(t, secret, "SECRET_REACHED", "SECRET_DENIED")},
		harness.ExecOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(res.Stdout, "SECRET_REACHED") {
		t.Errorf("非授权端点竟然可达（默认拒绝没生效）: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "SECRET_DENIED") {
		t.Errorf("非授权端点的探测没有给出明确结论: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}

	// 2) 公网直连：没有默认放行，必须不可达。
	res, err = d.Exec(ctx, h, []string{"bash", "-c",
		"timeout 8 bash -c 'exec 3<>/dev/tcp/1.1.1.1/80' && echo INET_REACHED || echo INET_DENIED"},
		harness.ExecOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(res.Stdout, "INET_REACHED") {
		t.Errorf("容器竟然能直连公网（默认拒绝没生效）: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "INET_DENIED") {
		t.Errorf("公网探测没有给出明确结论: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}

	// 3) 容器里不得有任何到宿主 Docker socket 的路。
	res, err = d.Exec(ctx, h, []string{"sh", "-c", "ls /var/run/docker.sock 2>&1 || echo NO_DOCKER_SOCK"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "NO_DOCKER_SOCK") {
		t.Errorf("容器里出现了 docker socket（隔离全线失效）: %q", res.Stdout)
	}
}

// ── provider 代理 ──

// 代理的两条路径都必须在**真容器**里验证：
//
//	白名单命中 → 代理去连上游（域名不存在时表现为 502，说明放行判定通过了）
//	白名单未命中 → 代理当场拒绝（403）
//
// 用 403 / 502 这对状态码做判据，而不是用「连不上」：连不上同时对应「代理没起」、
// 「容器没走代理」、「规则把代理端口拦了」三种完全不同的故障，现场无法归因。
// 而 502 只有在「代理收到 CONNECT、放行判定通过、开始拨号」之后才可能出现，
// 所以它是「容器确实把流量交给了代理」的强证据。
func TestIntegrationProviderProxyAllowsAllowlisted(t *testing.T) {
	_, ctx := newTestDocker(t) // 只做前置检查（Docker 可用、镜像存在）
	wd := makeWorkdir(t)

	cfg := DefaultDockerConfig()
	cfg.PrepareTimeout = 90 * time.Second
	cfg.ProviderAllowHosts = []string{"allowed.invalid"}
	d2, err := NewDocker(cfg)
	if err != nil {
		t.Fatalf("NewDocker: %v", err)
	}
	runID := harness.RunID("it-proxy")
	h, err := d2.Prepare(ctx, runSpec(t, runID, wd, nil))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d2, runID)

	// 1) 环境变量必须指向网关上的代理。
	res, err := d2.Exec(ctx, h, []string{"sh", "-c", "echo $HTTP_PROXY"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	proxy := strings.TrimSpace(res.Stdout)
	if proxy == "" {
		t.Fatalf("容器里没有 HTTP_PROXY")
	}
	ph, pp, err := net.SplitHostPort(strings.TrimPrefix(proxy, "http://"))
	if err != nil {
		t.Fatalf("HTTP_PROXY 不是合法的 host:port（%q）: %v", proxy, err)
	}

	// connect 在容器里对一个域名发 CONNECT 并取回第一行响应。
	// 刻意**不走** HTTP_PROXY：直接连代理端口，验证的是「代理按域名白名单
	// 判定」这条规则本身。走 HTTP_PROXY 的话失败会混进「客户端解析代理配置」
	// 这一层，现场就分不清是代理拒了还是客户端没走代理。
	connect := func(host string) string {
		t.Helper()
		cmd := fmt.Sprintf(
			`timeout 10 bash -c 'exec 3<>/dev/tcp/%s/%s; printf "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n" >&3; head -1 <&3'`,
			ph, pp, host, host)
		r, err := d2.Exec(ctx, h, []string{"bash", "-c", cmd}, harness.ExecOptions{Timeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("Exec(CONNECT %s): %v", host, err)
		}
		return r.Stdout
	}

	// 2) 白名单命中：allowed.invalid 不存在于任何 DNS，所以代理会走到拨号失败，
	//    响应必须是 502 —— 403 说明放行判定被误判，空输出说明代理根本没收到请求。
	if got := connect("allowed.invalid"); !strings.Contains(got, "502") {
		t.Errorf("白名单域名应通过放行判定并因上游不可达返回 502, got %q (proxy=%s)", got, proxy)
	}

	// 3) 白名单未命中：必须**明确拒绝**（403），而不是超时。
	if got := connect("evil.invalid"); !strings.Contains(got, "403") {
		t.Errorf("代理未按白名单拒绝（期望 403, got %q proxy=%s）", got, proxy)
	}

	// 4) NO_PROXY 必须让目标流量绕过代理（否则靶场全部不可达）。
	res, err = d2.Exec(ctx, h, []string{"sh", "-c", "echo $NO_PROXY"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) == "" {
		t.Errorf("容器里没有 NO_PROXY")
	}
}

// ── pi 可用 ──

func TestIntegrationPiRunsInsideContainer(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)
	runID := harness.RunID("it-pi")

	h, err := d.Prepare(ctx, runSpec(t, runID, wd, nil))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d, runID)

	res, err := d.Exec(ctx, h, []string{"pi", "--version"}, harness.ExecOptions{Timeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("pi --version 失败: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	// 版本闸（piai 的 DefaultVersionRange 是 {0.85.0, 0.86.99}）。容器里的 pi
	// 必须落在这个区间里，否则「镜像里的 pi 能不能被 piai 驱动」是个未知数。
	if !strings.Contains(res.Stdout, "0.85") && !strings.Contains(res.Stdout, "0.86") {
		t.Errorf("容器里的 pi 版本不在 piai 的版本闸内: %q", res.Stdout)
	}

	// node 也必须在：pi 的 shebang 是 `#!/usr/bin/env node`，node 不在 PATH
	// 时 pi 的失败形式是「找不到解释器」，离真正的原因很远。
	res, err = d.Exec(ctx, h, []string{"node", "--version"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("容器里没有可用的 node: %q / %q", res.Stdout, res.Stderr)
	}
}

// pi 必须在**只读 rootfs + 非 root + 工作目录可写**的组合下能启动。
//
// 这条是真正的价值所在：pi 需要写会话目录与 HOME，而只读 rootfs 会让
// HOME=/root 直接失败。executor 把 HOME 指到 /work/.home，这个用例验证
// 那个选择真的成立。
func TestIntegrationPiCanStartUnderIsolation(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)
	runID := harness.RunID("it-pi-start")

	h, err := d.Prepare(ctx, runSpec(t, runID, wd, nil))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d, runID)

	// HOME 必须在可写区，且 pi 能往里面写。
	res, err := d.Exec(ctx, h, []string{"sh", "-c", "echo HOME=$HOME; mkdir -p $HOME/.pi && echo HOME_WRITABLE"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "HOME_WRITABLE") {
		t.Errorf("HOME 不可写（pi 会静默失去扩展加载能力）: %q / %q", res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "/work/") {
		t.Errorf("HOME 应落在工作目录之下: %q", res.Stdout)
	}
}

// ── 回收 ──

func TestIntegrationReclaimRemovesContainerAndNetwork(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)
	runID := harness.RunID("it-reclaim")

	spec := runSpec(t, runID, wd, nil)
	h, err := d.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	containerID := h.ContainerID
	netName := h.Network

	// 回收前两者都在。
	if out, err := d.run(ctx, nil, "ps", "--all", "--filter", "id="+containerID, "--format", "{{.ID}}"); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("回收前容器应存在: out=%q err=%v", out, err)
	}
	if out, err := d.run(ctx, nil, "network", "ls", "--filter", "name=^"+netName+"$", "--format", "{{.Name}}"); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("回收前网络应存在: out=%q err=%v", out, err)
	}

	if err := d.Reclaim(ctx, runID); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	// 回收后两者都必须消失。
	if out, _ := d.run(ctx, nil, "ps", "--all", "--filter", "id="+containerID, "--format", "{{.ID}}"); strings.TrimSpace(out) != "" {
		t.Errorf("回收后容器仍在: %q", out)
	}
	if out, _ := d.run(ctx, nil, "network", "ls", "--filter", "name=^"+netName+"$", "--format", "{{.Name}}"); strings.TrimSpace(out) != "" {
		t.Errorf("回收后网络仍在: %q", out)
	}

	// 幂等：再回收一次必须无害（正常结束、取消、重启恢复三条路径都会调它）。
	if err := d.Reclaim(ctx, runID); err != nil {
		t.Errorf("Reclaim 必须幂等，第二次失败: %v", err)
	}
}

// ReclaimStale：宿主重启恢复路径——不带任何活跃 run 的扫描必须把对象收掉。
func TestIntegrationReclaimStale(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)
	runID := harness.RunID("it-stale")

	h, err := d.Prepare(ctx, runSpec(t, runID, wd, nil))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	containerID := h.ContainerID

	// live 为空 ⇒ 这次 run 是「重启后的孤儿」。
	rep, err := d.ReclaimStale(ctx, nil)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	found := false
	for _, id := range rep.Reclaimed {
		if id == runID {
			found = true
		}
	}
	if !found {
		t.Errorf("ReclaimStale 应回收 %s, got %v", runID, rep.Reclaimed)
	}
	// 回收清单是**已经删掉**的那些：报出来的每一个都必须真的不在宿主上了。
	// 带外核对（而不是只看返回值）是这条断言的要点——返回值与宿主状态分家时，
	// 「报了但没删」与「删了但没报」都只在这两处对不上时才看得见。
	if rep.ReclaimedTotal() != len(rep.Reclaimed) {
		t.Errorf("ReclaimedTotal=%d 与清单长度 %d 不一致", rep.ReclaimedTotal(), len(rep.Reclaimed))
	}
	if out, _ := d.run(ctx, nil, "ps", "--all", "--filter", "id="+containerID, "--format", "{{.ID}}"); strings.TrimSpace(out) != "" {
		t.Errorf("ReclaimStale 后容器仍在: %q", out)
	}

	// live 里标为活跃的 run **不得**被回收——这是这个函数最危险的一处：
	// 收错对象等于把一个正在跑的进攻性工具进程连容器一起删掉。
	runID2 := harness.RunID("it-stale-live")
	if _, err := d.Prepare(ctx, runSpec(t, runID2, makeWorkdir(t), nil)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d, runID2)

	if _, err := d.ReclaimStale(ctx, map[harness.RunID]bool{runID2: true}); err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	if out, _ := d.run(ctx, nil, "ps", "--all", "--filter", "label="+LabelRun+"="+string(runID2), "--format", "{{.ID}}"); strings.TrimSpace(out) == "" {
		t.Errorf("活跃 run 的容器被 ReclaimStale 误删了")
	}
}

// liveRunsOnHost 扫出宿主上当前带 run 标签的全部 runID。
//
// 负例需要一张「除了被测对象之外，别的都算活跃」的 live 表：这样即使判定有 bug、
// 把外来的对象判成了「本部署的孤儿」，也只有被测对象会被删——不会误伤同一台宿主
// 上另一个 worktree 里同时在跑的集成门留下的容器。
func liveRunsOnHost(t *testing.T, ctx context.Context, d *Docker) map[harness.RunID]bool {
	t.Helper()
	live := map[harness.RunID]bool{}
	for _, kind := range []string{"container", "network"} {
		objs, err := d.scanLabeled(ctx, kind)
		if err != nil {
			t.Fatalf("扫描 %s: %v", kind, err)
		}
		for _, o := range objs {
			if o.Parsed {
				live[o.RunID] = true
			}
		}
	}
	return live
}

// TestIntegrationReclaimStaleKeepsForeignObjects 是回收判定的**负例**。
//
// 它对着那个已被核实的事故：扫描面是宿主全局的，而 run 标签在宿主上**不唯一**
// （两个用不同 `--store` 的进程各有一个 run-1 是合法的）。所以「有 run 标签、
// 且不在 live 里」**不足以**成为删除依据——旧版本正是这样把另一个部署正在用的
// 容器、网络与 iptables 规则删掉的。
//
// 这里手工造出「别人的」两种对象（用 `docker create` / `docker network create`，
// 刻意不经过本执行器，所以它们带什么标签完全由本用例决定）：
//
//  1. 只有 run 标签、没有 owner 标签（升级前的旧资源）⇒ Pending(owner_unknown)
//  2. run 标签 + 另一个 owner 标签（同宿主上的另一个部署）⇒ Pending(owner_mismatch)
//
// 两条都必须**原样留着**。
func TestIntegrationReclaimStaleKeepsForeignObjects(t *testing.T) {
	d, ctx := newTestDocker(t)

	const (
		// 同一个 run ID 同时有容器与网络：真实遗留现场就是这样成对出现的。
		noOwnerRun = harness.RunID("it-neg-noowner")
		otherRun   = harness.RunID("it-neg-other")
		otherOwner = "another-host/0"
		noOwnerNet = "rh-neg-noowner-net"
	)

	// 1) 只有 run 标签的旧容器。
	if _, err := d.run(ctx, nil, "create",
		"--name", string(noOwnerRun),
		"--label", LabelRun+"="+string(noOwnerRun),
		"--label", LabelRole+"="+roleRunner,
		testImage(), "sleep", "infinity"); err != nil {
		t.Fatalf("造无 owner 的容器: %v", err)
	}
	// 2) 只有 run 标签的旧网络。
	if _, err := d.run(ctx, nil, "network", "create",
		"--label", LabelRun+"="+string(noOwnerRun),
		"--label", LabelRole+"="+"network",
		noOwnerNet); err != nil {
		t.Fatalf("造无 owner 的网络: %v", err)
	}
	// 3) 带另一个 owner 的容器。
	if _, err := d.run(ctx, nil, "create",
		"--name", string(otherRun),
		"--label", LabelRun+"="+string(otherRun),
		"--label", LabelOwner+"="+otherOwner,
		"--label", LabelRole+"="+roleRunner,
		testImage(), "sleep", "infinity"); err != nil {
		t.Fatalf("造他人的容器: %v", err)
	}

	// 清理走**原始 docker**，不走 Reclaim：Reclaim 按 owner 过滤，本来就够不到
	// 这三个对象——那正是本用例要断言的结论，用它来清理会把结论当前提。
	t.Cleanup(func() {
		if os.Getenv("RH_TEST_KEEP") == "1" && t.Failed() {
			t.Logf("RH_TEST_KEEP=1 且用例已失败：保留 %s / %s 供排查", noOwnerRun, otherRun)
			return
		}
		_, _ = d.run(context.Background(), nil, "rm", "-f", string(noOwnerRun), string(otherRun))
		_, _ = d.run(context.Background(), nil, "network", "rm", noOwnerNet)
	})

	exists := func(args ...string) bool {
		out, err := d.run(ctx, nil, args...)
		return err == nil && strings.TrimSpace(out) != ""
	}

	// live = 宿主上除这三个对象之外的全部 run（见 liveRunsOnHost 的理由）。
	live := liveRunsOnHost(t, ctx, d)
	delete(live, noOwnerRun)
	delete(live, otherRun)

	// ── 判定面 ──
	var objs []scanObject
	for _, kind := range []string{"container", "network"} {
		part, err := d.scanLabeled(ctx, kind)
		if err != nil {
			t.Fatalf("扫描 %s: %v", kind, err)
		}
		objs = append(objs, part...)
	}
	rep := judgeStale(objs, harness.OwnerID(d.cfg.Owner), live)

	for _, id := range rep.Reclaimed {
		if id == noOwnerRun || id == otherRun {
			t.Fatalf("外来的 run %s 被判成可回收——这就是那个删错对象的事故", id)
		}
	}
	// Pending 必须逐个对象地报告，而不是被同一个 runID 去重掉。
	pending := map[string]harness.StaleReason{}
	for _, o := range rep.Pending {
		pending[o.Kind+"/"+string(o.RunID)] = o.Reason
	}
	if got := pending["container/"+string(noOwnerRun)]; got != harness.StaleOwnerUnknown {
		t.Errorf("无 owner 的容器应是 %q, got %q（Pending=%+v）", harness.StaleOwnerUnknown, got, rep.Pending)
	}
	if got := pending["network/"+string(noOwnerRun)]; got != harness.StaleOwnerUnknown {
		t.Errorf("无 owner 的网络应是 %q, got %q（Pending=%+v）", harness.StaleOwnerUnknown, got, rep.Pending)
	}
	if got := pending["container/"+string(otherRun)]; got != harness.StaleOwnerMismatch {
		t.Errorf("他人 owner 的容器应是 %q, got %q（Pending=%+v）", harness.StaleOwnerMismatch, got, rep.Pending)
	}

	// ── 删除面 ──
	// 判定面算得对不等于执行面做得对，所以这里**再走一遍真路径**（ReclaimStale
	// 自己扫描 + 判定 + 删除），并同时断言两件事：外来的对象没被删，以及它们
	// **被如实报进了 Pending**。
	//
	// ⚠️ 「没删」与「没报」是两种不同的失败：只确认「对象还在」时，一个把外来的
	// 对象整个**看不见**的实现（扫描时按 owner 过滤掉了）也照样绿——而那意味着
	// 宿主上躺着一批谁也不认识的进攻性工具容器，且没有任何一处会说出来。
	rep2, err := d.ReclaimStale(ctx, live)
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	for _, id := range rep2.Reclaimed {
		if id == noOwnerRun || id == otherRun {
			t.Fatalf("ReclaimStale 返回了外来的 run %s: %v", id, rep2.Reclaimed)
		}
	}
	pending2 := map[string]harness.StaleReason{}
	for _, o := range rep2.Pending {
		pending2[o.Kind+"/"+string(o.RunID)] = o.Reason
	}
	if got := pending2["container/"+string(noOwnerRun)]; got != harness.StaleOwnerUnknown {
		t.Errorf("ReclaimStale 漏报/错报了无 owner 的容器: reason=%q（Pending=%+v）", got, rep2.Pending)
	}
	if got := pending2["network/"+string(noOwnerRun)]; got != harness.StaleOwnerUnknown {
		t.Errorf("ReclaimStale 漏报/错报了无 owner 的网络: reason=%q（Pending=%+v）", got, rep2.Pending)
	}
	if got := pending2["container/"+string(otherRun)]; got != harness.StaleOwnerMismatch {
		t.Errorf("ReclaimStale 漏报/错报了他人 owner 的容器: reason=%q（Pending=%+v）", got, rep2.Pending)
	}
	// Pending 必须**逐个对象**地报，而不是被同一个 runID 去重掉。这里不钉「总数
	// 恰好是 3」：`live` 是扫描那一刻的快照，同一台宿主上另一个 worktree 的集成
	// 门可能在下一瞬建出对象——那会让总数多一条，与「去重」这件事无关。
	// 钉「同一个 runID 下容器与网络各报一条」既精确又不受那种并发干扰。
	var sameRunPending []harness.StaleObject
	for _, o := range rep2.Pending {
		if o.RunID == noOwnerRun {
			sameRunPending = append(sameRunPending, o)
		}
	}
	if len(sameRunPending) != 2 {
		t.Errorf("同一个 run 的容器与网络应当各报一条 Pending，得到 %d 条: %+v",
			len(sameRunPending), rep2.Pending)
	}
	if rep2.PendingTotal() != len(rep2.Pending) {
		t.Errorf("PendingTotal=%d 与清单长度 %d 不一致", rep2.PendingTotal(), len(rep2.Pending))
	}
	if rep2.ReclaimedTotal() != len(rep2.Reclaimed) {
		t.Errorf("ReclaimedTotal=%d 与清单长度 %d 不一致", rep2.ReclaimedTotal(), len(rep2.Reclaimed))
	}
	// 报出来的每一条都必须带上「为什么留下它」的依据：一个只有 runID 的 Pending
	// 让人无从判断该不该人工介入。
	for _, o := range rep2.Pending {
		if o.Kind == "" || o.RunID == "" || o.Reason == "" {
			t.Errorf("Pending 条目缺字段: %+v", o)
		}
	}
	if !exists("ps", "--all", "--filter", "label="+LabelRun+"="+string(noOwnerRun), "--format", "{{.ID}}") {
		t.Errorf("无 owner 的容器被删了（它可能是升级前的孤儿，但也可能是别人的资源）")
	}
	if !exists("network", "ls", "--filter", "label="+LabelRun+"="+string(noOwnerRun), "--format", "{{.ID}}") {
		t.Errorf("无 owner 的网络被删了")
	}
	if !exists("ps", "--all", "--filter", "label="+LabelRun+"="+string(otherRun), "--format", "{{.ID}}") {
		t.Errorf("他人 owner 的容器被删了")
	}
}

// 挂载面：宿主状态目录**绝不**出现在容器里。这条断言在真容器上再验一遍——
// argv 断言保证「没写进去」，这里保证「即使写了也没生效」（比如具名卷的坑）。
func TestIntegrationNoHostStateVisibleInContainer(t *testing.T) {
	d, ctx := newTestDocker(t)
	wd := makeWorkdir(t)

	// 在宿主工作目录旁边造一个「私有证据」目录，确认它不会被挂进去。
	parent := filepath.Dir(wd)
	privateDir := filepath.Join(parent, "private")
	if err := os.MkdirAll(privateDir, 0o700); err != nil {
		t.Fatalf("造私有目录: %v", err)
	}
	secret := filepath.Join(privateDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("SHOULD-NOT-BE-VISIBLE\n"), 0o600); err != nil {
		t.Fatalf("写秘密文件: %v", err)
	}

	runID := harness.RunID("it-mounts")
	h, err := d.Prepare(ctx, runSpec(t, runID, wd, nil))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup(t, d, runID)

	res, err := d.Exec(ctx, h, []string{"sh", "-c",
		fmt.Sprintf("cat %s 2>&1 || echo NOT_VISIBLE; ls %s 2>&1 || echo DIR_NOT_VISIBLE", secret, privateDir)},
		harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(res.Stdout, "SHOULD-NOT-BE-VISIBLE") {
		t.Errorf("宿主的私有证据在容器里可见（挂载面失控）: %q", res.Stdout)
	}

	// 容器里也不得看见宿主的 docker socket 或宿主 HOME。
	res, err = d.Exec(ctx, h, []string{"sh", "-c", "ls /var/run/docker.sock /root/.local 2>&1 || echo HOST_STATE_ABSENT"}, harness.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "HOST_STATE_ABSENT") {
		t.Errorf("容器里出现了宿主状态: %q", res.Stdout)
	}
}
