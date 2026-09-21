package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// Docker 是 harness.Executor 的 Docker 实现。
//
// 它的职责边界刻意划得很窄：**只管容器与网络的生命周期**。它不知道 pi、不知道
// 题目、不知道候选答案——那些是 agent 与场景层的事。这样做的理由是可测性：
// 整个包的正确性可以只用 argv 与 Docker 的实际行为来验证。
type Docker struct {
	cfg DockerConfig

	mu      sync.Mutex
	proxies map[harness.RunID]*providerProxy
}

// 编译期断言：Docker 必须满足 harness.Executor。
//
// 为什么写在这里而不是留给使用方发现：契约漂移（有人在 Executor 接口上加了
// 方法）会在**装配层**才炸，而那里通常离改动最远。断言把它拉回改动现场。
var _ harness.Executor = (*Docker)(nil)
var _ harness.Sandbox = (*Docker)(nil)

// NewDocker 构造 Docker 执行器。
//
// 构造期只做**配置校验**，不碰 Docker daemon：把「这台机器的 Docker 能不能用」
// 留给 Available（doctor 阶段显式调用）。理由是构造失败的语义应该是「配置错了」，
// 而「Docker 没装/没起」是一个可以单独报告、单独重试的运行时状态。
func NewDocker(cfg DockerConfig) (*Docker, error) {
	ncfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	if ncfg.ProviderProxy {
		// 白名单在构造期就校验：非法条目（比如裸 `*.`）如果等到第一次
		// Prepare 才发现，那时 run 已经进入 preparing 状态了。
		if _, err := newAllowlistProxy(proxyConfig{AllowHosts: ncfg.ProviderAllowHosts}); err != nil {
			return nil, harness.Ef(harness.KindConfig, "executor.config",
				"ProviderAllowHosts 非法", err)
		}
	}
	if ncfg.NetworkSupernet != "" {
		if _, _, err := allocateSubnet("probe", ncfg.NetworkSupernet); err != nil {
			return nil, err
		}
	}
	return &Docker{cfg: ncfg, proxies: map[harness.RunID]*providerProxy{}}, nil
}

// Config 返回生效后的配置（已补缺省）。doctor 与测试用它。
func (d *Docker) Config() DockerConfig { return d.cfg }

// Available 检查 Docker daemon 是否可用。
//
// 用 `docker info` 而不是 `docker version`：后者在 daemon 挂掉时**仍然成功**
// （它只读客户端信息），于是「Docker 不可用」会被静默当成可用，直到第一次
// Prepare 才以别的形式失败。
func (d *Docker) Available(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.PrepareTimeout)
	defer cancel()
	out, err := d.run(ctx, nil, "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		return harness.Ef(harness.KindExecutor, "executor.available",
			"Docker daemon 不可用（`docker info` 失败）", err)
	}
	if strings.TrimSpace(out) == "" {
		return harness.Ef(harness.KindExecutor, "executor.available",
			"Docker daemon 返回空版本号", nil)
	}
	return nil
}

// Prepare 创建 per-run 网络并启动容器，返回句柄。
//
// 失败时的清理是**必须**的：网络建好了但容器起不来，如果不回收网络，宿主上会
// 攒下一堆孤儿 bridge（每个都占一个网段）。所以这里在失败路径上按逆序回收。
func (d *Docker) Prepare(ctx context.Context, spec harness.ExecSpec) (harness.ExecHandle, error) {
	p, err := planRun(spec, d.cfg)
	if err != nil {
		return harness.ExecHandle{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.PrepareTimeout)
	defer cancel()

	netw := networkFor(p, d.cfg)

	// 1) 网络。先建网络后起容器，顺序不可颠倒：容器必须在创建时就挂到
	// 正确网络上，否则它会先落在默认 bridge 上（一个短暂的「网络无边界」窗口）。
	if out, err := d.run(ctx, nil, netw.createArgv()[1:]...); err != nil {
		return harness.ExecHandle{}, harness.Ef(harness.KindExecutor, "executor.prepare",
			fmt.Sprintf("创建 per-run 网络 %s 失败", p.NetworkName), wrapOut(err, out))
	}

	// 2) iptables 规则。装在容器起来**之前**：先放行、后起容器，
	// 保证容器存在的那一刻规则就已生效（反过来会有一个敞开的窗口）。
	//
	// 网桥名要等网络创建后才知道，而 INPUT 链上的规则按入接口限定最精确，
	// 所以在装规则前先探测一次（探测不到就退回只按源网段限定）。
	if netw.ManageIptables {
		netw.Bridge = d.detectBridge(ctx, p.NetworkName)
		if err := d.installNetworkRules(ctx, netw); err != nil {
			_ = d.removeNetwork(ctx, p.NetworkName)
			return harness.ExecHandle{}, err
		}
	}

	// 3) provider 代理。绑在网桥网关上——那是容器唯一能到的宿主地址。
	if p.ProxyURL != "" {
		bind := fmt.Sprintf("%s:%d", p.NetworkGateway, proxyPort)
		pr, err := startProviderProxy(bind, proxyConfig{AllowHosts: d.cfg.ProviderAllowHosts}, nil)
		if err != nil {
			d.uninstallNetworkRules(ctx, netw)
			_ = d.removeNetwork(ctx, p.NetworkName)
			return harness.ExecHandle{}, err
		}
		d.mu.Lock()
		d.proxies[spec.RunID] = pr
		d.mu.Unlock()
	}

	// 4) 容器。
	out, err := d.run(ctx, nil, runArgv(p)[1:]...)
	if err != nil {
		d.release(spec.RunID)
		if netw.ManageIptables {
			d.uninstallNetworkRules(ctx, netw)
		}
		_ = d.removeNetwork(ctx, p.NetworkName)
		return harness.ExecHandle{}, harness.Ef(harness.KindExecutor, "executor.prepare",
			fmt.Sprintf("启动容器失败（镜像 %s 是否存在？）", p.Image), wrapOut(err, out))
	}

	return harness.ExecHandle{
		ContainerID: strings.TrimSpace(out),
		Network:     p.NetworkName,
		// Addrs 是**容器内视角**的目标地址。
		//
		// 为什么直接沿用宿主视角的地址：本实现刻意**不做 NAT**——容器通过
		// iptables 放行后直连目标的真实 IP:port（靶场地址是 VPN 内的直连地址，
		// 宿主与容器走的是同一条路由）。若将来引入 NAT（比如目标只在宿主
		// loopback 上），这里必须换成映射后的地址，否则容器里的 agent 会
		// 拿着一个不存在的地址去扫，表现为「目标全部超时」。
		Addrs: append([]string(nil), p.AllowHosts...),
	}, nil
}

// Exec 在容器里跑一条命令。
//
// 三个必须做对的地方：
//
//  1. **墙钟超时**：ctx 到期时不仅 `docker exec` 要退出，容器里那条命令也要
//     被杀。docker exec 被杀不会杀掉容器内的进程，所以超时路径上补一刀
//     `pkill -f`（见 killInContainer）。不补的话，agent 起的 `nmap` 会在容器里
//     继续跑，下一轮的命令与上一轮的残留进程抢资源，现场无法解释。
//  2. **stdin**：只有调用方给了 Stdin 才开 `-i`。pi 的 RPC 走 stdin，但别的
//     命令（`pi --version`）开了 `-i` 又没有输入源时会挂住。
//  3. **输出不截断**：pi 的 tool 输出可能很大（实测 details 262 KB 不截断），
//     所以这里用 bytes.Buffer 全收，截断留给上层的证据层决定。
func (d *Docker) Exec(ctx context.Context, h harness.ExecHandle, cmd []string, opts harness.ExecOptions) (harness.ExecResult, error) {
	if len(cmd) == 0 {
		return harness.ExecResult{}, harness.Ef(harness.KindConfig, "executor.exec", "命令为空", nil)
	}
	if h.ContainerID == "" {
		return harness.ExecResult{}, harness.Ef(harness.KindConfig, "executor.exec",
			"ExecHandle.ContainerID 为空：句柄不是 Prepare 返回的", nil)
	}

	// 用 plan 的默认值补齐 exec 渲染需要的字段（user/workdir/容器名）。
	// 句柄里没有这些，所以从 cfg 与 opts 复原；这是有意的取舍——
	// ExecHandle 是冻结契约（model.go），不能为了渲染方便往里加字段。
	p := runPlan{ContainerName: h.ContainerID, User: d.cfg.RunUser, WorkdirCtr: defaultWorkdir}
	timeout := effectiveTimeout(opts, d.cfg)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := execArgv(h, cmd, opts, p, d.cfg)
	start := time.Now()
	out, err := d.run(runCtx, opts.Stdin, argv[1:]...)
	elapsed := time.Since(start)

	res := harness.ExecResult{
		Stdout:   out,
		Duration: elapsed,
	}
	if err == nil {
		return res, nil
	}

	// 区分「命令自己非零退出」与「执行器故障」。
	//
	// 这是本包最容易搞错的一处：`docker exec` 会把容器内命令的退出码**原样**
	// 透出，所以一个正常的 `nmap` 非零退出与「docker 找不到容器」在 exec 层
	// 看起来都是 error。把前者当执行器故障上报，会让一次普通的工具失败变成
	// run 级异常。
	var ee *exec.ExitError
	if errors.As(err, &ee) && runCtx.Err() == nil {
		// 容器内命令自己退出的码：docker exec 透传的退出码就是它。
		res.ExitCode = ee.ExitCode()
		res.Stderr = stderrOf(ee)
		return res, nil
	}
	if runCtx.Err() != nil && ctx.Err() == nil {
		// 命令超时（不是外层取消）。杀掉容器里的残留进程，再把超时作为
		// 一个**带退出码**的结果返回，而不是 error：调用方（引擎）需要
		// 区分「这轮工具超时」与「执行器坏了」，前者是可解释的轮级事件。
		res.ExitCode = -1
		res.Stderr = fmt.Sprintf("命令超时（%s）: %s", timeout, strings.Join(cmd, " "))
		d.killInContainer(h.ContainerID, cmd)
		return res, nil
	}
	// 外层取消：交给调用方按 KindCancelled 处理。
	if ctx.Err() != nil {
		return res, harness.Ef(harness.KindCancelled, "executor.exec", "上下文已取消", ctx.Err())
	}
	return res, harness.Ef(harness.KindExecutor, "executor.exec",
		fmt.Sprintf("docker exec 失败（容器 %s）", h.ContainerID), err)
}

// Reclaim 按 run label 精确回收本次运行创建的全部容器、网络、规则与代理。
//
// 它是**幂等**的：正常结束、取消、宿主重启恢复三条路径都会调它，重复调用必须
// 无害（第二次调用时对象已经不存在了）。
func (d *Docker) Reclaim(ctx context.Context, runID harness.RunID) error {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.PrepareTimeout)
	defer cancel()

	// 代理与规则先关：容器还在时先停规则，会让容器内的连接变成黑洞；
	// 先停容器再停规则才是正确顺序（下面按容器→规则→网络→代理走）。
	//
	// 收集错误而不是提前返回：一个对象删不掉不该阻止别的对象被删，
	// 否则一次失败会让孤儿永远留着。
	var errs []error

	// 1) 容器（按 label，覆盖本 run 的全部容器，不只是句柄里那一个）。
	if err := d.removeContainersByRun(ctx, runID); err != nil {
		errs = append(errs, err)
	}

	// 2) iptables 规则（按注释归属）。
	if d.cfg.ManageIptables {
		if err := d.uninstallRulesByRun(ctx, runID); err != nil {
			errs = append(errs, err)
		}
	}

	// 3) 网络。
	if err := d.removeNetworksByRun(ctx, runID); err != nil {
		errs = append(errs, err)
	}

	// 4) 代理（本进程内的资源，不走 CLI）。
	d.release(runID)

	return joinErrors(errs)
}

// ReclaimStale 扫描带 run label 的容器与网络，把**不属于任何活跃 run** 的对象
// 回收掉（PLAN.md:54 的宿主重启恢复路径）。
//
// 判据是「label 存在」而不是「名字像我们的」：名字可以被抢注，label 是 run
// 身份的直接投影。live 是当前仍应存在的 runID 集合（由引擎从 store 里读出来）。
//
// 为什么必须有它：宿主重启后内存里的 run 列表没了，只有磁盘上的 run 目录还在。
// 没有这个扫描，重启前起的容器会永久占着网段与内存，而它们跑的 agent 已经
// 没有任何人在看了——那是一个无人监督的进攻性工具进程。
func (d *Docker) ReclaimStale(ctx context.Context, live map[harness.RunID]bool) ([]harness.RunID, error) {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.PrepareTimeout)
	defer cancel()

	ids, err := d.scanLabeled(ctx, "container")
	if err != nil {
		return nil, err
	}
	nids, err := d.scanLabeled(ctx, "network")
	if err != nil {
		return nil, err
	}
	all := append(ids, nids...)

	seen := map[harness.RunID]bool{}
	var reclaimed []harness.RunID
	for _, id := range all {
		if id == "" || seen[id] || live[id] {
			continue
		}
		seen[id] = true
		if err := d.Reclaim(ctx, id); err != nil {
			return reclaimed, err
		}
		reclaimed = append(reclaimed, id)
	}
	return reclaimed, nil
}

// scanLabeled 返回宿主上带 LabelRun 标签的对象所归属的 runID 列表。
//
// 解析**两种**输出形态：`{{.Label "x"}}` 只会打印标签的**值**，而某些 Docker
// 版本/对象类型下会打印 `key=value`。只认其中一种的后果是静默的：扫不到任何
// run，ReclaimStale 报「无事可做」——而宿主上明明躺着重启前的孤儿容器，
// 里面的进攻性工具还在跑，没有任何人在看它们的输出。
func (d *Docker) scanLabeled(ctx context.Context, kind string) ([]harness.RunID, error) {
	out, err := d.run(ctx, nil, d.staleScanArgv(kind)[1:]...)
	if err != nil {
		return nil, harness.Ef(harness.KindExecutor, "executor.reclaim",
			fmt.Sprintf("扫描 %s 标签失败", kind), err)
	}
	var ids []harness.RunID
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 取最后一个 `=` 之后的片段：既兼容纯值，也兼容 `key=value`。
		if _, v, ok := strings.Cut(line, "="); ok {
			line = v
		}
		if line = strings.TrimSpace(line); line != "" {
			ids = append(ids, harness.RunID(line))
		}
	}
	return ids, nil
}

// staleScanArgv 渲染「按 label 列出对象」的 argv。
func (d *Docker) staleScanArgv(kind string) []string {
	switch kind {
	case "network":
		return []string{d.cfg.Binary, "network", "ls", "--filter", "label=" + LabelRun, "--format", "{{.Label \"" + LabelRun + "\"}}"}
	default:
		return []string{d.cfg.Binary, "ps", "--all", "--filter", "label=" + LabelRun, "--format", "{{.Label \"" + LabelRun + "\"}}"}
	}
}

// ── 内部：进程调用 ──

// run 执行一条 docker 子命令，返回 stdout。
//
// 统一入口的理由：每条 docker 调用都必须带 ctx（否则宿主网络卡住时永久挂起）、
// 都必须把 stderr 收进错误消息（否则失败只有一句 "exit status 1"）。
func (d *Docker) run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	return runCmdStdin(ctx, d.cfg.Binary, stdin, args...)
}

// runCmd 执行一条宿主命令。
//
// 环境变量刻意**不继承**：docker CLI 会读 DOCKER_HOST / DOCKER_CONFIG /
// DOCKER_CONTEXT，继承会让「本机 Docker」的语义随调用方的 shell 变化而变——
// 而这条执行器唯一的强假设就是「我操作的是本机 Docker」。
func runCmd(ctx context.Context, bin string, args ...string) (string, error) {
	return runCmdStdin(ctx, bin, nil, args...)
}

func runCmdStdin(ctx context.Context, bin string, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.String(), nil
}

func wrapOut(err error, out string) error {
	out = strings.TrimSpace(out)
	if out == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, out)
}

func stderrOf(ee *exec.ExitError) string {
	if ee == nil {
		return ""
	}
	return string(ee.Stderr)
}

// killInContainer 在超时路径上补一刀，杀掉容器里可能残留的进程。
//
// 为什么需要它：`docker exec` 被 ctx 杀掉时，容器内那条命令**不会**跟着死——
// 它会变成孤儿继续跑（`--init` 只保证它被收割，不保证它被杀）。残留的 nmap /
// ffuf 会在下一轮与新的命令抢 CPU 与网络，让「这一轮为什么慢」变得无法解释。
func (d *Docker) killInContainer(containerID string, cmd []string) {
	if containerID == "" || len(cmd) == 0 {
		return
	}
	// 用 pkill -f 匹配命令行。失败无所谓（进程可能已经退了），所以忽略错误。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = d.run(ctx, nil, "exec", containerID, "pkill", "-f", "--", cmd[0])
}

func (d *Docker) release(runID harness.RunID) {
	d.mu.Lock()
	pr := d.proxies[runID]
	delete(d.proxies, runID)
	d.mu.Unlock()
	if pr != nil {
		_ = pr.Close()
	}
}
