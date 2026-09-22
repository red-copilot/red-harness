//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// 本文件是「同一个宿主、同一个 daemon 上，两个**不同 `--store`** 的真进程争同一把
// 跨进程单运行锁」的出口门（N0.2）。
//
// 为什么必须是两个真进程：单运行锁是 flock 上的文件锁，它的全部意义就是「进程 A
// 没退，进程 B 进不来」。在同一个测试进程里调两次 `cli.Main` 当然也会撞上同一把锁，
// 但那只说明「两个 fd 互斥」，与「宿主上跑着两个 red-harness，只有一个能开工」不是
// 一回事。所以这里**构建真实的 CLI 二进制**再起两个——顺带把 `cmd/red-harness` 的
// 装配入口（`main` 里那一行 `cli.Main(..., newPorts)`）也真的跑了一遍。
//
// ⚠️ **这条用例存在的唯一理由是最后那组「第一个进程的容器与网络还在」的断言。**
// `Harness.Run` 的顺序是：取锁 → `reclaimStale`（回收宿主上遗留的容器/网络）→
// `Discover` → 每题一个 sandbox。锁在回收**之前**，所以「第二个进程进不去」直接
// 等价于「它没有机会回收掉第一个进程正在用的资源」——那正是本波次要证明的事：
// 资源有了 owner 归属判据之后，别人的东西不会被删。其余断言（非 0 退出、失败在
// 配置类、stdout 为空）都是这条的**前置**：它们共同保证第二个进程真的没跑起来，
// 否则「资源还在」可能只是因为「它还没来得及删」。
//
// ⚠️ **两个进程显式指向同一条锁路径（`--lock`）**，而不是靠默认值。理由：默认锁的
// 位置当前是 `<StoreDir>/run.lock`（每个 store 一把），把它改成「按 daemon 端点
// 一把」是另一条在途的工作。这里验的是**锁机制本身**（flock 跨进程互斥 + 失败发生在
// 回收之前），不依赖默认路径那件事；等默认路径按端点收敛之后，这条用例只要去掉
// `--lock` 两个参数就变成了「默认策略也是全宿主互斥」，断言一行都不用改。
//
// ⚠️ 造资源只用 `--scenario fake` + stub 镜像：不碰真平台、不用任何真凭据。
// 子进程的环境变量也是**最小集**（PATH/HOME/夹具路径），刻意不继承 os.Environ()。

// CLI 退出码是公开 API。这里写**字面量**而不是 import `internal/cli` 的常量：
// 本用例刻意从**进程**这一侧观察行为（真二进制、真 stdout、真退出码），把断言绑到
// 包内符号上就等于绕过了「用户看到的到底是什么」。
const (
	// exitUnsolvedIT：跑完了但有题没解出来。第二个进程一旦给出这个码，说明它**
	// 已经跑进题目循环**，锁根本没拦住它。
	exitUnsolvedIT = 3
)

// TestIntegrationTwoStoresContendForTheRunLock 起两个不同 `--store` 的 CLI 进程，
// 用同一条显式锁路径逼它们互斥。
func TestIntegrationTwoStoresContendForTheRunLock(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("集成门需要 root（iptables 与 docker 资源归属都按 root 判定）")
	}
	if err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").Run(); err != nil {
		t.Skipf("Docker 不可用：%v", err)
	}
	if err := exec.Command("docker", "image", "inspect", "red-harness-runner:v0.3.0").Run(); err != nil {
		t.Skipf("runner 镜像不在位：%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	buildDir := t.TempDir()
	bin := filepath.Join(buildDir, "red-harness")
	goBuild(t, ctx, filepath.Join("..", ".."), "-o", bin, "./cmd/red-harness")
	// stallforever：stub pi 应答完第一个 prompt 就不再读 stdin，容器会一直 Up 到本轮
	// 墙钟超时（数分钟）。第一个进程因此**稳定地持有锁**，而不是两秒就跑完、把锁
	// 和容器一起还回来——那样「第二个进程进不去」根本来不及观察。
	image := buildStubImage(t, ctx, buildDir, "stallforever")

	// 夹具用**本题独有**的题目编号：容器上的 `red-harness.challenge` 标签是它的
	// sha256，于是本用例能按标签精确认出**自己的**容器。同一宿主上可能并发跑着别的
	// worktree 的集成门（它们用的是内置的 demo-1），按共享题号去数容器会把别人的
	// 资源算进来。
	code := "lock-contend-" + strconv.Itoa(os.Getpid())
	cid, err := harness.ChallengeIDFor(code)
	if err != nil {
		t.Fatal(err)
	}
	fixture := writeFixture(t, buildDir, code)
	const answer = "flag{lock-contend-offline-canary}"
	_ = answer // 答案写进夹具（见 writeFixture），本题不需要用到它

	// 两个进程：不同的 store，**同一条显式锁路径**。
	lockPath := filepath.Join(buildDir, "shared.lock")
	storeA := filepath.Join(buildDir, "store-a")
	storeB := filepath.Join(buildDir, "store-b")
	// 最小环境：只要 PATH（executor 自己拼 docker 的调用环境）、一个可写的 HOME
	// （piai 的会话目录）与夹具路径。**不继承宿主环境**——这条用例里没有任何一步
	// 需要凭据，而继承等于把宿主上那些真的 key 无故带进一个新进程。
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + buildDir,
		"RED_HARNESS_FAKE_CHALLENGES=" + fixture,
	}

	// 收尾注册在**进程之前**：`t.Cleanup` 是 LIFO，所以它会最后跑——那时两个
	// 进程已经被杀干净，容器与网络才删得掉（见 rhCleanup 的注释）。
	rc := &rhCleanup{t: t, cid: cid}
	t.Cleanup(rc.cleanup)

	// ── 第一个进程：真的跑起来并占住锁 ──
	p1 := startCLI(t, buildDir, bin, env,
		"run", "--scenario", "fake", "--store", storeA, "--image", image, "--lock", lockPath)

	// 它越过锁的第一个可观察证据是 sandbox 容器。等它出现再继续——否则第二个进程
	// 可能先于第一个进程去抢锁（那就成了「谁先谁赢」，与本用例要证的事无关）。
	before := waitForContainers(t, ctx, p1, cid, 90*time.Second)
	runID := before[0].Run
	if runID == "" {
		t.Fatalf("容器上没有 run 标签，无法定位它属于哪次运行：%+v", before)
	}
	rc.remember(runID)
	netsBefore := networksForRun(t, ctx, runID)
	if len(netsBefore) == 0 {
		// 空集合会让下面「网络一个不少」的断言变成空转——那正是本用例要防的形态。
		t.Fatalf("第一个进程没有创建 per-run 网络（run=%s），「没有被回收」将无从断言", runID)
	}
	// 「同一个宿主」在这里是一条**断言**而不是假设：容器的 owner 标签必须就是
	// 本机 hostname/uid 折出来的 owner（harness.ResolveOwner 是它的真源）。
	if want := wantOwner(t); before[0].Owner != string(want) {
		t.Fatalf("容器的 owner 标签 = %q，期望 %q——两个进程不在同一个归属域里",
			before[0].Owner, want)
	}

	// ── 第二个进程：同一个 daemon、同一条锁路径、不同的 store ──
	p2 := startCLI(t, buildDir, bin, env,
		"run", "--scenario", "fake", "--store", storeB, "--image", image, "--lock", lockPath)

	// 先看它有没有**动手**：集合的任何变化都是最直接的证据（同一个夹具、同一个题目，
	// 容器标签一模一样）。
	//
	// ⚠️ **变化有两个方向，都要报**（实测两种都出现过，见本函数末尾那段）：
	//   - 集合**变大**：第二个进程起了自己的 sandbox（它抢到了锁）；
	//   - 集合**变空**：第二个进程在启动时把第一个进程正在跑的容器**回收掉了**。
	// 后者是 executor 的扫描修好之后才出现的形态，而它比前者严重得多——那条路径是
	// `docker rm --force`。只报「多了一个」会让这种红看起来像「起了两个容器」，
	// 而实际发生的是「一个正在跑的进攻性工具进程被连容器删掉了」。
	// 这段轮询也是非空性实验的落点：把第二个进程的锁路径改掉，它就会在这里红。
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if now := containersForChallenge(t, ctx, cid); len(now) != len(before) || !sameObjects(before, now) {
			p2.kill()
			what := "起了自己的 sandbox（它抢到了锁或根本没抢）"
			if len(now) < len(before) {
				what = "**删掉了**第一个进程正在跑的容器（它把对方当成了崩溃遗留）"
			}
			t.Fatalf("第二个进程动手了——它%s：容器集合从 %v 变成 %v（stderr=%q）",
				what, before, now, p2.stderr())
		}
		if p2.exited() {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !p1.alive() {
		t.Fatalf("第一个进程在断言窗口内退出了，这条用例的前提（它占着锁）不成立："+
			"exit=%d stdout=%q stderr=%q", p1.code(), p1.stdout(), p1.stderr())
	}

	// ── 第二个进程必须失败在锁上 ──
	//
	// 它此刻阻塞在 `locker.Lock(ctx)` 里（CLI 的 ctx 只由信号触发，没有超时），
	// 所以给它一点时间自己退；不退就 SIGINT——取消是用户主动终止的正常路径，
	// 锁会因此返回错误，而**失败点仍然在 Prepare/Reclaim 之前**。
	if !p2.wait(5 * time.Second) {
		if err := p2.cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Fatalf("向第二个进程发 SIGINT 失败：%v", err)
		}
	}
	if !p2.wait(30 * time.Second) {
		p2.kill()
		t.Fatalf("第二个进程在 SIGINT 之后 30s 仍未退出：stderr=%q", p2.stderr())
	}
	switch code := p2.code(); code {
	case 0:
		t.Fatalf("第二个进程成功了（退出码 0）——单运行锁没有挡住它：stdout=%q stderr=%q",
			p2.stdout(), p2.stderr())
	case exitUnsolvedIT:
		t.Fatalf("第二个进程的退出码是 3（跑完了但没解出来）——它分明跑到了题目循环里，" +
			"说明锁没有在 Discover/Prepare 之前拦住它")
	}
	// 失败必须是**配置类**：消息链上要同时出现两层 op（`harness.lock` / `wire.lock`）
	// 与它们的 Kind（`config`）。它回答的是「这台机器上已经有一个 run 在跑」，与
	// 「跑到一半坏了」不是一类——处置也不同（等它跑完 vs 去查日志）。
	//
	// 为什么这里只能按消息判：Kind 是包内类型，跨进程只能从这条折叠过的消息链上读。
	// 消息本身是**公开面**（根包 errors.go 的折叠格式），不含路径与凭据。
	stderr := p2.stderr()
	for _, want := range []string{"harness.lock: config", "wire.lock: config", "单运行锁"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("第二个进程的失败不是「锁上的配置类错误」（消息里缺 %q）：stderr=%q", want, stderr)
		}
	}
	// stdout 是**公开面**：一行进度都没打出来，等于它连 Discover 都没走到。
	if out := p2.stdout(); out != "" {
		t.Fatalf("第二个进程往 stdout 写了东西（它进到了题目循环）：%q", out)
	}

	// ── 唯一理由：第一个进程的资源**一个不少、原样在跑** ──
	//
	// 这条断言是**真正的防线**，而且它管的方向比上面几条都要紧。
	//
	// ⚠️ 写这条用例时（基线 `d5497f4`）它还不是：那时 `ReclaimStale` 在生产路径上
	// 是**空转**的——`staleScanArgv` 取 `{{json .Labels}}`，而 `ps` / `network ls`
	// 的模板上下文里 Labels 是逗号拼接的**字符串**（`"k=v,k=v"`），解析却按 JSON
	// **对象**走，于是每个对象都落进 Pending(unparsable)、一个都不删。扫描格式在
	// `3d3d299` 修好之后，这条断言的前提才成立。
	//
	// **失效形态随之变了，而且是变严重**（两次非空性实验实测，破坏方式是给第二个
	// 进程换一条 `--lock` 路径）：
	//   - 修好之前：容器集合 1 → 2。第二个进程起了自己的 sandbox，第一个的照样活着。
	//     讨厌，但**不破坏任何东西**——因为那时回收根本删不掉东西。
	//   - 修好之后：容器集合 1 → **空**。第二个进程在启动时按「owner 匹配 且 runID
	//     不在本次 live 集合里」判定，把第一个进程**正在跑**的容器 `docker rm --force`
	//     掉了。这正是 N0.2 那一类事故：两个 run 互相删资源。
	//
	// 所以「资源还在」现在**确实**证明了锁保护了它们：第二个进程已经拿到了它的
	// live 集合（只装它自己的 runID），如果锁没拦住它，它会立刻把别人的资源收掉。
	// 上面几条钉住「它没走到 Prepare/Reclaim」，这一条钉住「即使走到了，别人的东西
	// 也没有被它动」——两条合起来才排除掉「锁只是让它晚了几秒失败」这种解释。
	//
	// ⚠️ 这条断言**不能**单独读：它在本机以外的 owner 域（别的用户、别的 daemon）上
	// 不成立——跨用户互斥是明令不做的（见 `local/lock.go`），那时两个进程的 owner
	// 不同，回收本来就够不到对方的资源。
	after := containersForChallenge(t, ctx, cid)
	if !sameObjects(before, after) {
		t.Fatalf("第二个进程动了第一个进程的容器：before=%v after=%v", before, after)
	}
	for _, o := range after {
		if !o.Running {
			t.Errorf("容器 %s 还在但已经不在跑了（State.Running=false）", o.ID[:12])
		}
	}
	netsAfter := networksForRun(t, ctx, runID)
	if !sameStrings(netsBefore, netsAfter) {
		t.Fatalf("第二个进程回收掉了第一个进程的网络：before=%v after=%v", netsBefore, netsAfter)
	}
	t.Logf("第二个进程在锁上失败（exit=%d，stderr=%q）；第一个进程（run=%s）的 %d 个容器与 %d 个网络原样在跑",
		p2.code(), strings.TrimSpace(p2.stderr()), runID, len(after), len(netsAfter))
}

// rhCleanup 收尾：只删**本用例自己造的** docker 资源。
//
// ⚠️ **判据必须精确**，因为它跑在一个可能同时跑着别的 worktree 集成门的宿主上：
// 容器按那道**独有题号**的标签找（本进程 pid 生成，别的门用的是内置 demo-1，不会
// 撞），网络按**从这些容器里读出来的 runID** 标签找。绝不做宽泛标签批量删，更不碰
// `docker system prune`——一次误删就是别人的红，而且是那种「看起来像随机失败」的红。
//
// 为什么这条兜底是**必需**的：本用例在断言完成后会杀掉第一个进程，而引擎自己的
// 回收挂在 ctx 取消上——SIGKILL 不给它这个机会。少了这一段，每跑一次这条用例就会
// 在宿主上留一个容器与一个网络。
type rhCleanup struct {
	t     *testing.T
	cid   harness.ChallengeID
	mu    sync.Mutex
	runID string
}

func (c *rhCleanup) remember(runID string) {
	c.mu.Lock()
	c.runID = runID
	c.mu.Unlock()
}

func (c *rhCleanup) runIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runID == "" {
		return nil
	}
	return []string{c.runID}
}

// cleanup 删掉本次运行留下的容器与网络。清理路径**刻意不 Fatal**：它的失败不该
// 掩盖用例本身的结果，所以只 Logf（漏掉的资源会以 `red-harness.run=` 标签留在宿主
// 上，用 docker 直接查得到）。
func (c *rhCleanup) cleanup() {
	ctx := context.Background()
	// 1) 本用例的容器：按独有题号找，读完它的 run 标签再删。
	ids := c.dockerLines(ctx, "ps", "--all", "--quiet",
		"--filter", "label=red-harness.challenge="+string(c.cid))
	runs := c.runIDs()
	for _, id := range ids {
		if run := c.dockerOne(ctx, "inspect", "--format",
			`{{index .Config.Labels "red-harness.run"}}`, id); run != "" {
			runs = append(runs, run)
		}
		if out, err := exec.CommandContext(ctx, "docker", "rm", "--force", id).CombinedOutput(); err != nil {
			c.t.Logf("清理容器 %s 失败：%v（%s）", id, err, out)
		} else {
			c.t.Logf("已清理容器 %s", id[:12])
		}
	}
	// 2) 这些运行对应的网络。只按读出来的 runID 删：网络没有题目标签，所以它的
	//    精确性来自上一步——集合是从**我自己的**容器反查出来的。
	for _, run := range runs {
		for _, net := range c.dockerLines(ctx, "network", "ls", "--quiet",
			"--filter", "label=red-harness.run="+run) {
			if out, err := exec.CommandContext(ctx, "docker", "network", "rm", net).CombinedOutput(); err != nil {
				c.t.Logf("清理网络 %s 失败：%v（%s）", net, err, out)
			} else {
				c.t.Logf("已清理网络 %s", net[:12])
			}
		}
	}
}

func (c *rhCleanup) dockerLines(ctx context.Context, args ...string) []string {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		c.t.Logf("清理时执行 docker %v 失败：%v", args, err)
		return nil
	}
	return strings.Fields(strings.TrimSpace(string(out)))
}

func (c *rhCleanup) dockerOne(ctx context.Context, args ...string) string {
	if lines := c.dockerLines(ctx, args...); len(lines) > 0 {
		return lines[0]
	}
	return ""
}

// ── 进程与 docker 的脚手架 ──

// cliProc 是一个真在跑的 CLI 进程。
//
// 输出必须是并发安全的：父进程要在子进程**还活着**的时候读 stderr/stdout（不然
// 拿不到「它此刻在报什么」），而裸 bytes.Buffer 会与子进程的写入构成数据竞争。
type cliProc struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  bytes.Buffer
	errb bytes.Buffer
	done chan struct{}
	werr error
	ec   int
}

type lockedWriter struct {
	p     *cliProc
	isErr bool
}

func (w lockedWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	if w.isErr {
		return w.p.errb.Write(b)
	}
	return w.p.out.Write(b)
}

// startCLI 起一个 CLI 进程。它**不经 ctx**：取消必须走信号（SIGINT），因为这条
// 用例要的正是「用户按了 Ctrl-C 之后锁有没有把它放进来」——CommandContext 会在
// ctx 到期时直接 SIGKILL，那样就看不到锁的失败消息了。
func startCLI(t *testing.T, dir, bin string, env []string, args ...string) *cliProc {
	t.Helper()
	p := &cliProc{done: make(chan struct{})}
	p.cmd = exec.Command(bin, args...)
	p.cmd.Dir = dir
	p.cmd.Env = env
	p.cmd.Stdout = lockedWriter{p: p}
	p.cmd.Stderr = lockedWriter{p: p, isErr: true}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("启动 CLI 进程失败（%s %v）：%v", bin, args, err)
	}
	go func() {
		err := p.cmd.Wait()
		p.mu.Lock()
		p.werr = err
		// ⚠️ 先判类型再判 err==nil 的顺序反过来都行，但**不能**把类型断言塞进
		// switch 的 case 条件里顺带判空：`case ee != nil && errors.As(...)` 里
		// `ee` 在 As 跑之前恒为 nil，于是每个非 0 退出都被记成 -1。
		switch {
		case err == nil:
			p.ec = 0
		default:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				p.ec = ee.ExitCode()
			} else {
				// 非 ExitError（等待本身失败等）：码取 -1。断言按「非 0 且非 3」
				// 处理，所以它仍然会被判成「失败了」。
				p.ec = -1
			}
		}
		p.mu.Unlock()
		close(p.done)
	}()
	t.Cleanup(func() { p.kill() })
	return p
}

func (p *cliProc) wait(d time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (p *cliProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *cliProc) alive() bool { return !p.exited() }

// kill 是幂等的：正常退出后再调是空操作（`Process.Kill` 会返回「已退出」，
// 这里忽略它——清理路径不该因为「已经没了」而失败）。
func (p *cliProc) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if p.exited() {
		return
	}
	_ = p.cmd.Process.Kill()
	p.wait(10 * time.Second)
}

func (p *cliProc) code() int      { p.wait(0); p.mu.Lock(); defer p.mu.Unlock(); return p.ec }
func (p *cliProc) stdout() string { p.mu.Lock(); defer p.mu.Unlock(); return p.out.String() }
func (p *cliProc) stderr() string { p.mu.Lock(); defer p.mu.Unlock(); return p.errb.String() }

// rhObject 是宿主上一个带 red-harness 标签的 docker 对象。
type rhObject struct {
	ID      string
	Run     string
	Owner   string
	Running bool
}

// containersForChallenge 按**题目哈希标签**列出容器。
//
// 为什么按题目标签而不是「按 run 标签」：run 标签是本次运行才生成的，而题目编号
// 是本用例独有的——用它过滤，并发的其他集成门（跑内置 demo-1）不会被算进来。
//
// ⚠️ 标签值用 `docker inspect --format '{{index .Config.Labels "x"}}'` 取，不用
// `{{json .Labels}}`：后者在本机的 Docker 29.8 上打出的是**逗号拼接的字符串**
// （`"k=v,k=v"`），不是 JSON 对象——executor 的 parseScan 正是被它咬住的
// （见报告与 executor/reclaim.go 的注释）。
func containersForChallenge(t *testing.T, ctx context.Context, cid harness.ChallengeID) []rhObject {
	t.Helper()
	ids := dockerLines(t, ctx, "ps", "--all", "--quiet", "--filter", "label=red-harness.challenge="+string(cid))
	out := make([]rhObject, 0, len(ids))
	for _, id := range ids {
		out = append(out, rhObject{
			ID:      id,
			Run:     dockerOne(t, ctx, "inspect", "--format", `{{index .Config.Labels "red-harness.run"}}`, id),
			Owner:   dockerOne(t, ctx, "inspect", "--format", `{{index .Config.Labels "red-harness.owner"}}`, id),
			Running: dockerOne(t, ctx, "inspect", "--format", "{{.State.Running}}", id) == "true",
		})
	}
	return out
}

// networksForRun 按 run 标签列出网络（网络没有题目标签，见 executor/network.go）。
func networksForRun(t *testing.T, ctx context.Context, runID string) []string {
	t.Helper()
	return dockerLines(t, ctx, "network", "ls", "--quiet", "--filter", "label=red-harness.run="+runID)
}

// waitForContainers 等第一个进程的 sandbox 出现，返回它那一批容器的快照。
func waitForContainers(t *testing.T, ctx context.Context, p1 *cliProc, cid harness.ChallengeID, d time.Duration) []rhObject {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		got := containersForChallenge(t, ctx, cid)
		if len(got) > 0 {
			return got
		}
		if p1.exited() {
			t.Fatalf("第一个进程还没起 sandbox 就退出了：exit=%d stdout=%q stderr=%q",
				p1.code(), p1.stdout(), p1.stderr())
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s 内没有看到第一个进程的 sandbox 容器：stdout=%q stderr=%q",
		d, p1.stdout(), p1.stderr())
	return nil
}

func dockerLines(t *testing.T, ctx context.Context, args ...string) []string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %v 失败：%v（输出 %s）", args, err, out)
	}
	return strings.Fields(strings.TrimSpace(string(out)))
}

func dockerOne(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	lines := dockerLines(t, ctx, args...)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// wantOwner 是本机的 owner 标识（hostname/uid）。用 harness.ResolveOwner 而不是
// 自己拼串：那是它的唯一真源。
func wantOwner(t *testing.T) harness.OwnerID {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("取 hostname 失败：%v", err)
	}
	owner, err := harness.ResolveOwner(host, os.Getuid())
	if err != nil {
		t.Fatalf("解析 owner 失败：%v", err)
	}
	return owner
}

func sameObjects(a, b []rhObject) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── 构建 ──

// goBuild 在 dir 里跑一次 `go build`（args 是 build 的参数，子命令由这里补）。
func goBuild(t *testing.T, ctx context.Context, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "go", append([]string{"build"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %v 失败：%v\n%s", args, err, out)
	}
}

// buildStubImage 造一个「真 runner 镜像 + stub pi」的镜像。
//
// 它只在**本机**构建与删除（`RH_TEST_KEEP=1` 可以留住它），绝不触碰共享镜像。
func buildStubImage(t *testing.T, ctx context.Context, buildDir, scenario string) string {
	t.Helper()
	stub := filepath.Join(buildDir, "stubpi")
	goBuild(t, ctx, filepath.Join("..", ".."), "-o", stub, "./piai/testdata/stubpi")
	dockerfile := fmt.Sprintf(
		"FROM red-harness-runner:v0.3.0\nCOPY stubpi /opt/stubpi\n"+
			"RUN chmod 0755 /opt/stubpi && ln -sf /opt/stubpi /usr/local/bin/pi\n"+
			"ENV STUBPI_SCENARIO=%s\n", scenario)
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	image := "red-harness-lock-it:" + strings.ReplaceAll(t.Name(), "/", "-")
	cmd := exec.CommandContext(ctx, "docker", "build", "--quiet", "-t", image, buildDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建 stub 镜像失败：%v\n%s", err, out)
	}
	t.Cleanup(func() {
		if os.Getenv("RH_TEST_KEEP") != "1" {
			_ = exec.Command("docker", "image", "rm", "--force", image).Run()
		}
	})
	return image
}

// writeFixture 写一份只有本题一道题的离线夹具，返回路径。
//
// 题号是**本用例独有**的，于是容器上的 challenge 标签也是独有的：按它数容器不会
// 把并发跑着的其他集成门算进来（它们用内置的 demo-1）。
//
// 用 harness.Challenge 直接序列化而不是手拼 JSON：夹具的 schema 与根包结构体是
// 同一份，字段名漂移会在编译期/解析期暴露，而不是让用例静默地跑一道不存在的题。
func writeFixture(t *testing.T, dir, code string) string {
	t.Helper()
	const answer = "flag{lock-contend-offline-canary}"
	fx := struct {
		Challenges []harness.Challenge        `json:"challenges"`
		Answers    map[string]map[string]bool `json:"answers"`
		// ScorePerFlag 省略时装配层按 0 处理；显式给 1 与内置夹具同形。
		ScorePerFlag int `json:"scorePerFlag,omitempty"`
	}{
		Challenges: []harness.Challenge{{
			Code: code,
			// Description 里必须出现 flag{...}：gate 用题面推断答案形态。
			Description: "离线争锁用例（fake 场景）：容器里出现 " + answer + " 即算解出。不接任何真实平台。",
			Category:    "misc",
			FlagCount:   1,
			// 白名单非空即可（executor 读它渲染 iptables）；指回环地址，
			// 不真的打到任何东西上。
			Addrs: []string{"127.0.0.1:1"},
		}},
		Answers:      map[string]map[string]bool{code: {answer: true}},
		ScorePerFlag: 1,
	}
	body, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fake-challenges.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
