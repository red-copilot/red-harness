package bridge

// 进程监管的测试：常驻复用、请求关联、崩溃重启与重做预检、stderr 落点。
//
// 这一层测的是「Go 侧能不能可靠地驱动一个会死的 Python 进程」，全部用真实
// 子进程（bridge.py + mock_sdk）跑，不 mock 进程。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBridgeIsResident 钉住「整个跑分期间一个 Python 进程」。
//
// 失效模式（前身设计的既有结论）：每次调用起一个新进程，意味着每次都重新
// 建 httpx 连接池、重新做 VPN 预检。VPN 预检本身有 10 秒超时（client.py:46），
// 每题做一次会把整个 run 拖垮。
func TestBridgeIsResident(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	c := newMockClient(t, map[string]string{"TSEC_MOCK_LOG": logPath})

	for i := 0; i < 3; i++ {
		if _, err := c.List(context.Background()); err != nil {
			t.Fatalf("第 %d 次 List 失败: %v", i, err)
		}
	}
	if c.Restarts() != 0 {
		t.Errorf("常驻复用不该有重启，实际 %d 次", c.Restarts())
	}
	lines := readLines(t, logPath)
	// 三次 list：每次都真的走到了 Python 侧（说明桥一直活着在服务）。
	if n := countLines(lines, "list"); n != 3 {
		t.Errorf("应有 3 次 list 调用，实际 %d（日志: %v）", n, lines)
	}
}

// TestRequestIDCorrelation 钉住 request ID 关联。
//
// 失效模式：两条请求的响应对调（例如并发时按到达顺序而不是 id 匹配），
// 会让 `submit` 拿到 `list` 的结果——表现为「平台判对了但进度是错的」，
// 而且日志上看不出来。
//
// 这里用并发调用制造「响应可能乱序」的局面。Go 侧按 id 投递，所以结果必须
// 与各自的请求对应。注意桥是串行处理的，但 Go 侧的 pending 表必须在并发下
// 也正确。
func TestRequestIDCorrelation(t *testing.T) {
	c := newMockClient(t, nil)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	codes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 交替两种调用，让「响应与请求错配」在结果里可见。
			if i%2 == 0 {
				_, errs[i] = c.List(context.Background())
				return
			}
			res, err := c.Start(context.Background(), "web-01")
			errs[i] = err
			codes[i] = res.Code
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发调用失败: %v", i, err)
		}
	}
	for i := 1; i < n; i += 2 {
		if codes[i] != "web-01" {
			t.Errorf("第 %d 个 start 的 Code 应为 web-01，实际 %q（响应错配）", i, codes[i])
		}
	}
}

// TestChildDeathRestartsAndRedoesPrecheck 钉住 **崩溃重启 + 重做预检**。
//
// 这是 PLAN.md:68 的硬要求，也是本任务里最容易被做漏的一条。两个断言：
//
//  1. 杀掉子进程后，下一次调用自动重启并成功（而不是永久失败）；
//  2. 重启后**重做 VPN 预检**——新进程没有任何「VPN 通」的证据，跳过预检会
//     让「在一个不通的 VPN 上继续跑」变成静默状态（ports.go:95-96 记的前身
//     事故正是这个形状）。
func TestChildDeathRestartsAndRedoesPrecheck(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	c := newMockClient(t, map[string]string{"TSEC_MOCK_LOG": logPath})

	// 先做一次预检并确认通过（这样 vpnOK 为真，重启后才应该重做）。
	if err := c.CheckVPN(context.Background()); err != nil {
		t.Fatalf("首次预检失败: %v", err)
	}
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("首次 List 失败: %v", err)
	}
	before := countLines(readLines(t, logPath), "check_vpn")

	// **杀掉子进程**（不是杀整个进程组——模拟「桥自己崩了」）。
	c.mu.Lock()
	pid := c.p.cmd.Process.Pid
	c.mu.Unlock()
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("杀子进程失败: %v", err)
	}
	// 等 Go 侧的 reader 发现 EOF。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		dead := c.p.isDead()
		c.mu.Unlock()
		if dead {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 下一次调用应自动重启并成功。
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("崩溃后 List 应自动重启并成功: %v", err)
	}
	if c.Restarts() != 1 {
		t.Errorf("应恰好重启 1 次，实际 %d 次", c.Restarts())
	}
	after := countLines(readLines(t, logPath), "check_vpn")
	if after <= before {
		t.Errorf("重启后必须重做 VPN 预检（前 %d 次，后 %d 次）", before, after)
	}
}

// TestRestartWithoutPrecheckDoesNotInjectOne 钉住「用户没做过预检时，重启不替它注入」。
//
// 失效模式：重启时无条件做一次预检，会让「预检失败」与「重启失败」再次混成
// 一个错误——而这正是本任务要消除的混淆（plan 里明令不要用上下文管理器）。
// 用户没调过 CheckVPN 时，桥不该自己决定去连 VPN。
func TestRestartWithoutPrecheckDoesNotInjectOne(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	c := newMockClient(t, map[string]string{"TSEC_MOCK_LOG": logPath})

	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	// 杀进程。
	c.mu.Lock()
	pid := c.p.cmd.Process.Pid
	c.mu.Unlock()
	proc, _ := os.FindProcess(pid)
	_ = proc.Kill()
	time.Sleep(200 * time.Millisecond)

	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("崩溃后 List 应自动重启并成功: %v", err)
	}
	if n := countLines(readLines(t, logPath), "check_vpn"); n != 0 {
		t.Errorf("没做过预检时重启不该注入预检，实际 %d 次", n)
	}
}

// TestStartupFailureIsActionableNotFirstCallExplosion 钉住「启动探活」。
//
// 失效模式：桥命令本身有问题（例如容器不存在）时，如果不在 NewClient 阶段
// 发现，用户会在第一次 `list` 看到一个超时错误，完全不知道是桥命令错了。
func TestStartupFailureIsActionableNotFirstCallExplosion(t *testing.T) {
	cfg := mockConfig(t, nil)
	cfg.Command = []string{"/nonexistent/bridge-not-here"}
	cfg.Timeout = 2 * time.Second

	start := time.Now()
	_, err := NewClient(cfg)
	if err == nil {
		t.Fatal("桥命令不存在时 NewClient 应该失败")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("启动失败应在启动阶段快速报出，实际耗时 %s", elapsed)
	}
	if !strings.Contains(err.Error(), "启动桥进程失败") {
		t.Errorf("错误消息应指出启动失败: %v", err)
	}
}

// TestStartupTimeoutIncludesStderr 钉住「启动失败时把 stderr 接进错误消息」。
//
// 失效模式（前身 B14 的形状）：pi/bridge 的启动期错误几乎都只在 stderr，
// 不接出来这类故障无从下手。
func TestStartupTimeoutIncludesStderr(t *testing.T) {
	py := requirePython(t)
	// 一个「起来了但不说话」的脚本：只往 stderr 写一行然后睡。
	dir := t.TempDir()
	script := filepath.Join(dir, "silent.py")
	if err := os.WriteFile(script, []byte(
		"import sys, time\nsys.stderr.write('BOOM: mock silent bridge\\n')\nsys.stderr.flush()\ntime.sleep(60)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := ClientConfig{
		Command: []string{py, script},
		Timeout: 500 * time.Millisecond,
	}
	_, err := NewClient(cfg)
	if err == nil {
		t.Fatal("桥不握手时 NewClient 应该超时失败")
	}
	// 这条测试覆盖的是 startupTimeout（20 秒）那条分支——它是本包里唯一
	// 一条慢测试。默认跑它（它测的是真实存在的启动失败路径），但允许用
	// `-short` 跳过以加快迭代。
	if testing.Short() {
		t.Skip("short 模式跳过 20 秒的启动超时测试")
	}
	if !strings.Contains(err.Error(), "BOOM") {
		t.Errorf("错误消息应包含子进程 stderr 尾部: %v", err)
	}
	if !strings.Contains(err.Error(), EnvBridgeCmd) {
		t.Errorf("错误消息应指出桥命令可覆盖: %v", err)
	}
}

// TestShutdownIsIdempotentAndKills 钉住 Shutdown 幂等且真的杀进程。
func TestShutdownIsIdempotentAndKills(t *testing.T) {
	c := newMockClient(t, nil)
	c.mu.Lock()
	pid := c.p.cmd.Process.Pid
	c.mu.Unlock()

	c.Shutdown()
	c.Shutdown() // 幂等

	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	// 进程应已退出：signal 0 对它失败。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(nil); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Shutdown 后子进程 %d 仍活着", pid)
}

// TestCallAfterShutdownFailsClosed 钉住「关掉之后再调不会 panic」。
//
// 失效模式：Shutdown 后 c.p 为 nil，任何一次误调都会 nil 解引用——那是
// 一个 panic，而不是一个可归因的错误。
func TestCallAfterShutdownFailsClosed(t *testing.T) {
	c := newMockClient(t, nil)
	c.Shutdown()
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("Shutdown 之后调用应该报错")
	}
	if !strings.Contains(err.Error(), "已关闭") {
		t.Errorf("应报「已关闭」而不是 panic: %v", err)
	}
}

// TestConcurrentRestartIsSerialized 钉住重启不会被并发调用打乱。
//
// 失效模式：两个并发调用同时发现桥死了，各自起一个新进程——于是有两个桥进程
// 连着同一个 token，平台侧会看到并发的 start/submit。c.mu 必须把整条
// 「发现死亡 → 重启 → 重发」串行化。
func TestConcurrentRestartIsSerialized(t *testing.T) {
	c := newMockClient(t, nil)
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	pid := c.p.cmd.Process.Pid
	c.mu.Unlock()
	proc, _ := os.FindProcess(pid)
	_ = proc.Kill()
	time.Sleep(200 * time.Millisecond)

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.List(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发调用 %d 失败: %v", i, err)
		}
	}
	// 每个调用各自发现死亡时会各自重启——串行化之后，第一个调用重启成功，
	// 后续调用看到的是活的进程。所以重启次数必须是 1（不是 n）。
	if c.Restarts() != 1 {
		t.Errorf("并发崩溃恢复应只重启 1 次，实际 %d 次（多个桥进程会并发打平台）", c.Restarts())
	}
}

// ── 工具 ──

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func countLines(lines []string, want string) int {
	n := 0
	for _, l := range lines {
		if l == want {
			n++
		}
	}
	return n
}
