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
	"github.com/red-copilot/red-harness/legacy"
)

// Docker 是 legacy.Executor 与 harness.Sandbox 的 Docker 实现。
//
// 它的职责边界刻意划得很窄：**只管容器与网络的生命周期**。它不知道 pi、不知道
// 题目、不知道候选答案——那些是 agent 与场景层的事。这样做的理由是可测性：
// 整个包的正确性可以只用 argv 与 Docker 的实际行为来验证。
type Docker struct {
	cfg DockerConfig

	mu      sync.Mutex
	proxies map[harness.RunID]*providerProxy
}

// 编译期断言：Docker 必须同时满足 legacy.Executor（v0.3 端口）与 harness.Sandbox
// （v0.4 生产端口）——同一份实现服务两代端口，这一行是那件事的显式记录。
//
// 为什么写在这里而不是留给使用方发现：契约漂移（有人在 Executor 接口上加了
// 方法）会在**装配层**才炸，而那里通常离改动最远。断言把它拉回改动现场。
var _ legacy.Executor = (*Docker)(nil)
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
	// 只支持本机 daemon：宿主级锁无法跨主机互斥，而**归属判据**里的 owner 也只在
	// 一台机器上有意义。normalize 已经把零值回落成本机端点，所以这条断言针对的是
	// 一个显式指定的非本机端点——fail closed，而不是「尽力而为」。
	//
	// 为什么放在构造期而不是第一次 Prepare：Sandbox 是公开端口，直接调 SDK 的
	// 调用方可能手工拼一个 DockerConfig。构造时炸出来的是一条配置错误，等到起
	// 容器时才炸则是一条执行器故障（现场已经不在手里了）。
	if !ncfg.Endpoint.IsLocal() {
		return nil, harness.Ef(harness.KindConfig, "executor.config",
			"只支持本机 docker daemon（unix socket）：远程 daemon 下宿主级锁与资源归属判据都不成立", nil)
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
	// ⚠️ **必须用分开返回 stdout/stderr 的那个变体**：容器内命令的 stderr 是
	// 排障与报告的第一手材料（「为什么写 / 失败」只有 stderr 说得清），而
	// `res.Stderr = stderrOf(ee)` 这条路**恒为空**——只要显式设过 cmd.Stderr，
	// Go 就不会再填 ExitError.Stderr。之前这里拿到的永远是空串。
	out, errOut, err := runCmdStdinSplit(runCtx, d.cfg.Binary, opts.Stdin, d.subprocessEnv(), argv[1:]...)
	elapsed := time.Since(start)

	res := harness.ExecResult{
		Stdout:   out,
		Stderr:   errOut,
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
		// stderr 已经在 res 里了（见上面的 split 变体），这里不再赋值。
		res.ExitCode = ee.ExitCode()
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
	// owner 必须在**任何副作用之前**解析出来：这条路径会删容器、网络与 iptables
	// 规则，而 run 标签在宿主上不唯一（两个部署可以各有一个 run-1）。
	// 拿不到 owner 就不删（fail closed），否则「只删自己的」会退化成「谁的都删」。
	self, err := d.selfOwner()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.PrepareTimeout)
	defer cancel()

	// 代理与规则先关：容器还在时先停规则，会让容器内的连接变成黑洞；
	// 先停容器再停规则才是正确顺序（下面按容器→规则→网络→代理走）。
	//
	// 收集错误而不是提前返回：一个对象删不掉不该阻止别的对象被删，
	// 否则一次失败会让孤儿永远留着。
	var errs []error

	// 1) 容器（按 label，覆盖本 run 的全部容器，不只是句柄里那一个）。
	//    定位参数里带 owner（见 reclaim.go 的 ownerFilter）——那不是「多一层
	//    保险」，而是让外来的同名 run 根本不出现在结果里。
	if err := d.removeContainersByRun(ctx, runID); err != nil {
		errs = append(errs, err)
	}

	// 2) iptables 规则（按注释归属，注释里带 owner）。
	if d.cfg.ManageIptables {
		if err := d.uninstallRulesByRun(ctx, self, runID); err != nil {
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

// ReclaimStale 扫描带 run label 的容器与网络，把**属于本部署、且不属于任何活跃
// run** 的对象回收掉（PLAN.md:54 的宿主重启恢复路径）。
//
// 判据是 **run + owner 两条**（见 reclaim.go 的 judgeStale，那里的表是权威）：
// 名字可以被抢注，而 run 标签在宿主上**不唯一**——两个用不同 `--store` 的进程
// 各自开一个 run-1 是合法的。所以「label 存在」本身不足以成为删除依据。
//
// live 是当前仍应存在的 runID 集合（由引擎从 store 里读出来）。
//
// ⚠️ **Pending 在本轮被丢弃**：ReclaimStale 的签名（harness.Sandbox 端口）还没
// 切到 harness.ReclaimReport，所以「发现了但不该删」这件事暂时没有通道。这是
// 有意的中间态，且严格比切之前安全——切之前那些对象会被直接删掉。
//
// 为什么必须有它：宿主重启后内存里的 run 列表没了，只有磁盘上的 run 目录还在。
// 没有这个扫描，重启前起的容器会永久占着网段与内存，而它们跑的 agent 已经
// 没有任何人在看了——那是一个无人监督的进攻性工具进程。
func (d *Docker) ReclaimStale(ctx context.Context, live map[harness.RunID]bool) ([]harness.RunID, error) {
	// owner 在任何副作用之前解析（判据的输入，缺了它一律不删）。
	self, err := d.selfOwner()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.PrepareTimeout)
	defer cancel()

	// 扫描面：容器 + 网络。两种对象一次扫完再判定，而不是「边扫边删」——
	// 判据要先看全（比如同一个 run 的容器与网络必须得到同一个结论）。
	var objs []scanObject
	for _, kind := range []string{"container", "network"} {
		part, err := d.scanLabeled(ctx, kind)
		if err != nil {
			return nil, err
		}
		objs = append(objs, part...)
	}

	rep := judgeStale(objs, self, live)
	// 删除逐个走 Reclaim（它自己有 owner 闸门），失败时返回**已删的那批**加错误——
	// 与切之前同形：调用方拿到的是「这次实际删掉了什么」，而不是一个空集合。
	reclaimed := make([]harness.RunID, 0, rep.ReclaimedTotal())
	for _, id := range rep.Reclaimed {
		if err := d.Reclaim(ctx, id); err != nil {
			return reclaimed, err
		}
		reclaimed = append(reclaimed, id)
	}
	return reclaimed, nil
}

// scanLabeled 扫描宿主上带 LabelRun 标签的对象，返回**逐个对象**的归属信息
// （run 标签 + owner 标签 + 种类）。解析交给纯函数 parseScan。
//
// 为什么输出形态固定成 `{{json .Labels}}`：`{{.Label "x"}}` 只会打印一个标签的
// **值**，某些 Docker 版本/对象类型下会退化成打印 `key=value`（甚至多个标签用逗号
// 拼成 `k=v,k=v`）——而「只认其中一种」的后果是静默的：扫不到任何 run，
// ReclaimStale 报「无事可做」，宿主上却躺着重启前的孤儿容器。JSON 没有这个歧义，
// 而且一次调用就带回 owner。见 parseScan 里为什么不用「模板 + 分隔符」。
func (d *Docker) scanLabeled(ctx context.Context, kind string) ([]scanObject, error) {
	out, err := d.run(ctx, nil, d.staleScanArgv(kind)[1:]...)
	if err != nil {
		return nil, harness.Ef(harness.KindExecutor, "executor.reclaim",
			fmt.Sprintf("扫描 %s 标签失败", kind), err)
	}
	return parseScan(kind, out), nil
}

// staleScanArgv 渲染「按 label 列出对象」的 argv。
//
// `--filter label=<LabelRun>`（只给键、不给值）是**存在性**过滤：任何带 run 标签的
// 对象都要被看见，因为「看见了但不该删」要能进 Pending，而看不见就无法报告。
func (d *Docker) staleScanArgv(kind string) []string {
	const format = "{{json .Labels}}"
	switch kind {
	case "network":
		return []string{d.cfg.Binary, "network", "ls", "--filter", "label=" + LabelRun, "--format", format}
	default:
		return []string{d.cfg.Binary, "ps", "--all", "--filter", "label=" + LabelRun, "--format", format}
	}
}

// ── 内部：进程调用 ──

// run 执行一条 docker 子命令，返回 stdout。
//
// 统一入口的理由：每条 docker 调用都必须带 ctx（否则宿主网络卡住时永久挂起）、
// 都必须把 stderr 收进错误消息（否则失败只有一句 "exit status 1"）。
func (d *Docker) run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	return runCmdStdin(ctx, d.cfg.Binary, stdin, d.subprocessEnv(), args...)
}

// subprocessEnv 是本执行器交给**每一个**子进程的环境变量。
//
// ⚠️ 它是「我操作的是本机 daemon」这句话的**结构属性**，而不只是一句注释：
// docker CLI 会读 DOCKER_HOST / DOCKER_CONTEXT / DOCKER_CONFIG，而 `$HOME` 缺失
// 时它还会退回 /etc/passwd 里的家目录去读 `~/.docker/config.json` 的
// currentContext —— 于是「连哪个 daemon」会随调用方的 shell 与宿主用户变化，
// 而锁身份与资源归属判据都建立在「本机 daemon」这个假设上。
//
// 三项都显式钉死（空值是**有意义**的：它表示「不解析 context 文件 / 不读用户
// 配置」，而不是「继承」）：
//
//	DOCKER_HOST    生效端点，不是硬编码的默认值（默认端点只是回落值）
//	DOCKER_CONTEXT 空 ⇒ 不解析 context 文件
//	DOCKER_CONFIG  空 ⇒ 不读 ~/.docker 配置
//
// 对 iptables 这类子进程多出来的 DOCKER_* 是惰性的（它们只认自己的环境变量），
// 所以只保留一条环境构造路径，不再为「谁需要哪个变量」分叉。
func (d *Docker) subprocessEnv() []string {
	return subprocessEnvFor(d.cfg.Endpoint)
}

// subprocessEnvFor 是 subprocessEnv 的纯函数形态（同样的三个变量，同样的理由）。
// 拆出来是为了让**不属于本执行器**的子进程调用（测试直接调 docker、将来别处
// 复用）也走同一份口径，而不是各写一份各自漂移。
func subprocessEnvFor(ep harness.DockerEndpoint) []string {
	return []string{
		"PATH=" + subprocessPath,
		"DOCKER_HOST=" + ep.ID(),
		"DOCKER_CONTEXT=",
		"DOCKER_CONFIG=",
	}
}

// subprocessPath 是子进程的 PATH。docker CLI 与 iptables 都靠它找到自己。
const subprocessPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// runCmd 执行一条宿主命令（env 由调用方给出，理由见 subprocessEnv）。
func runCmd(ctx context.Context, bin string, env []string, args ...string) (string, error) {
	return runCmdStdin(ctx, bin, nil, env, args...)
}

func runCmdStdin(ctx context.Context, bin string, stdin io.Reader, env []string, args ...string) (string, error) {
	out, errOut, err := runCmdStdinSplit(ctx, bin, stdin, env, args...)
	if err != nil {
		msg := strings.TrimSpace(errOut)
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%w: %s", err, msg)
	}
	return out, nil
}

// runCmdStdinSplit 与 runCmdStdin 相同，但把 stdout 与 stderr **分开**返回。
//
// 为什么必须有这个变体：`Exec` 要把容器内命令的 stderr 原样交给调用方——写只读
// 文件系统为什么失败、工具为什么报错，只有 stderr 说得清。而 `runCmdStdin` 把它
// 折进了 error 的文本里，且只在**失败**路径上；成功路径上那条 stderr 会被直接
// 丢掉（例如工具退出 0 但打了警告）。折进 error 还有个更硬的问题：调用方要拿到
// 它就得去解析错误字符串，而 errors.go 明令禁止按消息文本做判断。
//
// 也**不要**改用 `ExitError.Stderr`：它只在走 `cmd.Output()` 时才会被填充，而这里
// 显式设了 cmd.Stderr（为了与 stdout 分开），于是它恒为 nil——那正是本函数要修的
// 那个缺陷。
func runCmdStdinSplit(ctx context.Context, bin string, stdin io.Reader, env []string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = stdin
	}
	// 环境变量刻意**不继承**（env 的内容与理由见 Docker.subprocessEnv）：
	// 继承会让「本机 Docker」的语义随调用方的 shell 变化而变。
	cmd.Env = env
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func wrapOut(err error, out string) error {
	out = strings.TrimSpace(out)
	if out == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, out)
}

// killInContainer 在超时路径上补一刀，杀掉容器里可能残留的进程。
//
// 为什么需要它：`docker exec` 被 ctx 杀掉时，容器内那条命令**不会**跟着死——
// 它会变成孤儿继续跑（`--init` 只保证它被收割，不保证它被杀）。残留的 nmap /
// ffuf 会在下一轮与新的命令抢 CPU 与网络，让「这一轮为什么慢」变得无法解释。
//
// ⚠️ **PID 1 必须排除。** 这里曾经是 `pkill -f -- <cmd[0]>`，只按**第一个词**匹配
// 命令行：一条 `sleep 60` 超时后它会执行 `pkill -f sleep`，而容器的 PID 1 正是
// `sleep infinity`（见 spec.go 的主进程命令）——于是被杀掉的不是那条残留命令，
// 而是容器本身，后续所有 Exec 全部失败。这不是理论风险：集成用例
// `TestIntegrationWallClockTimeout` 就是被它咬住的。
//
// 所以改成两步且两条判据都收紧：
//   - 匹配**整条命令行**（不只是第一个词），`sleep 60` 不会再命中 `sleep infinity`；
//   - 逐个 PID 判断，显式跳过 PID 1（容器主进程）与自身（`sh -c` 的 argv 里也带着
//     那个待匹配的字符串，不排除就会把自己杀掉，循环提前中断）。
//
// 命令用 sh -c 传参而不是拼字符串：待匹配的行是**工具命令**（可能含引号、分号、
// 重定向），拼进脚本等于把一条工具命令当成 shell 代码执行。
func (d *Docker) killInContainer(containerID string, cmd []string) {
	if containerID == "" || len(cmd) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const script = `for p in $(pgrep -f -- "$1"); do
  [ "$p" = 1 ] && continue
  [ "$p" = "$$" ] && continue
  kill -9 "$p" 2>/dev/null
done`
	// 失败无所谓（进程可能已经退了），所以忽略错误。
	_, _ = d.run(ctx, nil, "exec", containerID, "sh", "-c", script, "sh", strings.Join(cmd, " "))
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
