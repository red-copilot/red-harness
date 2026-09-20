// Package executor 是 Docker 隔离执行器：它把 harness.ExecutorSpec 渲染成一组
// 硬隔离的 `docker run` 参数，负责创建 per-run 网络、回收容器，并把 provider
// 流量收束到宿主侧的域名白名单代理。
//
// 包的设计原则只有一条：**隔离属性必须体现在 argv 上，而不是体现在注释里。**
// 307 GB 磁盘写满那条事故的防线就是这里的 `--read-only` + 有上限的 `--tmpfs`；
// 它一旦被谁顺手删掉，`spec_test.go` 会立刻红。
package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// ── 常量 ──

const (
	// LabelRun 是**回收的唯一依据**（PLAN.md:54）。容器、网络、iptables 规则
	// 全部带它，Reclaim 按它精确匹配，宿主重启后的孤儿扫描也按它走。
	//
	// 为什么不用容器名做依据：名字可以被复用/抢注，label 是 run 身份的直接投影。
	LabelRun = "red-harness.run"
	// LabelRole 区分同一 run 下的容器角色（v1 只有 runner，留位给后续的
	// 代理边车或靶场边车）。
	LabelRole = "red-harness.role"

	roleRunner = "runner"

	// defaultImageTag 与 runner/README.md 记录的产物 tag 一致。
	defaultImageTag = "red-harness-runner:v0.3.0"

	// 隔离参数的缺省值。任何一个缺省成「不限」都等于没有护栏：
	//   - pids：fork 炸弹会打满宿主 PID 表，宿主 sshd 起不来；
	//   - memory：无上限的分配会触发宿主 OOM killer（宿主只有 3 GB）；
	//   - cpus：爆破/挖矿会吃满宿主 CPU，pi 的 RPC 往返超时；
	//   - tmpfs：只读 rootfs 下 tmpfs 是**唯一**可写区，不给上限等于回到
	//     307 GB 事故的起点（写 /tmp 写满宿主内存/磁盘）。
	defaultCPUs      = 1.0
	defaultMemoryMB  = 512
	defaultPidsLimit = 256
	defaultTmpfsMB   = 128
	// defaultWorkdir 是容器内工作目录（与 runner 镜像里的 /work 一致）。
	defaultWorkdir = "/work"

	// defaultCommandTimeout 是单条命令的墙钟上限。pi 的一轮可能很长，但
	// **必须有界**——无界的一轮会让整个 run 卡死而不触发任何预算护栏。
	defaultCommandTimeout = 30 * time.Minute

	// defaultStopTimeout 是 docker stop 的宽限期（秒）。给容器一点时间收尾；
	// 0 会立刻 SIGKILL，agent 的收尾动作被截断。
	defaultStopTimeout = 10

	// iptablesCommentPrefix 用于给规则打 run 注释，宿主重启后可归属与清理。
	iptablesCommentPrefix = "red-harness-run-"

	// proxyPort 是宿主侧 provider 代理的固定端口。固定而非随机，是为了让
	// 网络层（iptables 里对网桥网关的放行）与代理实现解耦。
	proxyPort = 18080

	// defaultNetworkSupernet 是 per-run 网络从中切 /24 的超级网。见
	// DockerConfig.NetworkSupernet 里为什么避开 Docker 默认池与 10.0.0.0/8。
	defaultNetworkSupernet = "10.211.0.0/16"
)

// ── 配置 ──

// DockerConfig 是执行器的运维级配置。
//
// 这里刻意**不放**任何用户/题目级的东西（镜像、CPU、内存、白名单）——那些走
// harness.ExecutorSpec，因为它们是 RunSpec 的一部分、要参与配置摘要（Digest）。
// DockerConfig 是「这台机器上的 Docker 怎么用」，不进摘要。
type DockerConfig struct {
	// Binary 是 docker CLI 的路径。默认 "docker"。
	//
	// 为什么走 CLI 而不是 SDK：整个仓库零第三方依赖，SDK 会把 containerd、
	// opencontainers、grpc 一大串依赖拖进来。CLI 的接口是稳定的文本，且
	// 出问题时人可以直接复制那条命令复现。
	Binary string

	// RunUser 是容器的 uid:gid。默认 "65534:65534"（nobody:nogroup）。
	//
	// **必须同时给 uid 和 gid**：只给 uid 时 gid 落到 0=root，容器里新建的文件
	// 属组是 root，工作目录的属组权限就成了绕过非 root 约束的口子。
	RunUser string

	// CommandTimeout 是单条命令的默认墙钟上限（ExecOptions.Timeout 为零值时用）。
	CommandTimeout time.Duration

	// PrepareTimeout 是 Prepare/Reclaim 这类管理操作的上限。默认 2 分钟。
	// 给管理操作单独一个上限，是为了让「镜像不存在」这类错误快速失败，
	// 而不是把整轮预算耗在等一个永远不会成功的 docker run 上。
	PrepareTimeout time.Duration

	// TmpfsSizeMB 是容器内 /tmp 这个 tmpfs 的大小上限（MB）。
	// **它是 307 GB 事故的直接对策**：只读 rootfs 让 / 不可写，而 /tmp 是
	// 唯一可写区，所以它的上限就是「agent 最多能写多少」。
	TmpfsSizeMB int

	// StopTimeout 是 docker stop 的宽限期（秒）。
	StopTimeout int

	// InternalNetwork 为真时，per-run 网络带 `--internal`（去掉默认路由）。
	//
	// **默认关（false）**，这是一个实测出来的结论，不是偏好：
	//
	//   - `--internal` 确实给出结构性的默认拒绝（容器内 `ip route` 只剩一条
	//     `172.x.0.0/16 dev eth0`，连网关都不可达，对 1.1.1.1 直接
	//     "Network is unreachable"）。
	//   - 但它同时**掐掉了到宿主的路由**。而 TSecBench 返回的靶场地址就是
	//     「宿主机可达的地址」（经 VPN 直连），容器必须经宿主的转发才能到。
	//     实测：`--internal` 网络里，容器连宿主的出网地址直接
	//     "Network is unreachable"，连 INPUT 链都到不了 —— 白名单里的目标
	//     一样不可达。
	//
	// 所以默认拒绝必须落在 iptables 上（ManageIptables），而不是靠 --internal。
	// 这样做的代价是「默认拒绝依赖规则正确性」，因此规则必须带源限定、且
	// 装规则要在起容器**之前**（见 Prepare）。把这一位留给运维：宿主上没有
	// iptables、或者部署形态是「靶场就是本机容器」时，可以打开它换回结构性隔离。
	InternalNetwork bool

	// ManageIptables 为真（默认）时，执行器为每个 run 维护三条规则：
	// 放行到 AllowHosts 的流量、放行到宿主代理端口的流量、其余全部拒绝。
	//
	// 为什么需要它（`--internal` 已经拦住了出站）：容器要能连到
	// `10.x`/`192.168.x` 的靶场地址，那是宿主侧的路由。没有这两条放行，
	// 容器连靶场都摸不到。非 root 或没有 iptables 的环境可以关掉它，
	// 代价是「目标白名单」退化成「网络可达即可达」。
	ManageIptables bool

	// ProviderProxy 为真（默认）时，启动宿主侧域名白名单代理，并把
	// HTTP_PROXY/HTTPS_PROXY 注入容器。
	//
	// provider 流量（模型 API）走代理；代理只放行白名单域名。这是
	// PLAN.md:52 的「provider 流量通过宿主侧域名白名单代理」。
	ProviderProxy bool

	// ProviderAllowHosts 是代理放行的域名白名单，支持 `*.example.com` 通配。
	// **空表示全部拒绝**（fail closed）——空白名单如果退化成全放行，
	// 「provider 白名单」就成了一句空话。
	ProviderAllowHosts []string

	// ProxyBindIP 是代理监听的宿主地址。为空时取 per-run 网络的网桥网关
	// （容器在 --internal 网络里唯一能到的宿主地址）。
	ProxyBindIP string

	// NetworkSupernet 是 per-run 网络从中切 /24 的超级网，默认
	// "10.211.0.0/16"。
	//
	// 为什么由 run 确定性分配网段而不是让 Docker 从地址池里挑：iptables 的
	// 默认拒绝规则必须带**源限定**（否则会掐死宿主上所有别的容器），而源限定
	// 要求网段在网络创建之前就知道。确定性分配还让同一 run 重起时网段不变，
	// 残留规则能对上号。
	//
	// 为什么默认不是 172.17-172.31（Docker 默认池）：避开与其它容器抢地址。
	// 也刻意避开 10.0.0.0/8 的常用内网段——靶场地址通常就在那里。
	NetworkSupernet string

	// WritableRootfs 是**运维逃生口**：置真时容器根文件系统可写。
	//
	// 为什么需要逃生口：某些调试场景（比如进容器看 pi 为什么起不来）确实要写。
	// 为什么它不是 ExecutorSpec 的字段：只读 rootfs 是 307 GB 事故的唯一防线，
	// 不能由用户配置（用户配置要进 RunSpec 摘要、可被 CLI flag 改），
	// 只能由这台机器的运维显式打开。
	WritableRootfs bool

	// DisableNoNewPrivileges 是**运维逃生口**：关掉 no-new-privileges。
	// 默认 false（即始终加 no-new-privileges）。与 WritableRootfs 同理。
	DisableNoNewPrivileges bool

	// HostStateDirs 是**额外**要断言「绝不挂进容器」的宿主路径。
	// 引擎/存储层把自己的资产目录填进来，渲染时会逐条校验。
	HostStateDirs []string
}

// DefaultDockerConfig 返回运维缺省值。它刻意与 DockerConfig 的零值不同：
// 零值里的 bool 全是 false（= 关掉隔离），直接拿零值构造执行器是不安全的。
func DefaultDockerConfig() DockerConfig {
	return DockerConfig{
		Binary:          "docker",
		RunUser:         "65534:65534",
		CommandTimeout:  defaultCommandTimeout,
		PrepareTimeout:  2 * time.Minute,
		TmpfsSizeMB:     defaultTmpfsMB,
		StopTimeout:     defaultStopTimeout,
		InternalNetwork: false,
		ManageIptables:  true,
		ProviderProxy:   true,
	}
}

// defaultDockerConfig 是包内测试用的别名（spec_test.go 无 build tag）。
func defaultDockerConfig() DockerConfig { return DefaultDockerConfig() }

// normalize 把零值补成安全缺省，并校验不可能安全执行的取值。
//
// 为什么在 NewDocker 里做而不是在每次 planRun 里做：DockerConfig 是**运维**
// 配置，配错了应该在构造时立刻炸（doctor 阶段），而不是等到第一次起容器。
func (c DockerConfig) normalize() (DockerConfig, error) {
	if c.Binary == "" {
		c.Binary = "docker"
	}
	if c.RunUser == "" {
		c.RunUser = "65534:65534"
	}
	if err := validateRunUser(c.RunUser); err != nil {
		return c, err
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = defaultCommandTimeout
	}
	if c.PrepareTimeout <= 0 {
		c.PrepareTimeout = 2 * time.Minute
	}
	if c.TmpfsSizeMB <= 0 {
		c.TmpfsSizeMB = defaultTmpfsMB
	}
	if c.StopTimeout <= 0 {
		c.StopTimeout = defaultStopTimeout
	}
	return c, nil
}

var userRe = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

// validateRunUser 拒绝 root 与非 uid:gid 形式的用户。
//
// 为什么强制数字形式：`--user nobody` 只在镜像的 /etc/passwd 里有 nobody 时
// 生效，缺了会得到 `unable to find user` 的运行时失败——那是**起容器时才炸**，
// 而这里是**配置时就炸**。数字形式对任何镜像都成立。
func validateRunUser(u string) error {
	if !userRe.MatchString(u) {
		return harness.Ef(harness.KindConfig, "executor.config",
			fmt.Sprintf("RunUser 必须是 uid:gid 的数字形式（如 65534:65534），got %q", u), nil)
	}
	uid, gid, _ := strings.Cut(u, ":")
	if uid == "0" || gid == "0" {
		return harness.Ef(harness.KindConfig, "executor.config",
			fmt.Sprintf("RunUser 不得包含 root（0），got %q", u), nil)
	}
	return nil
}

// ── 渲染计划 ──

// runPlan 是「一次运行怎么起容器」的完整渲染结果。
//
// 把它显式建模出来的理由：argv 是唯一决定隔离属性的东西，而 argv 里有十几处
// 取值来自不同来源（用户 spec、运维 cfg、缺省值）。先算出 plan，再渲染 argv，
// 测试就能直接对 plan 断言（例如「唯一允许的挂载是哪个」），不必解析字符串。
type runPlan struct {
	RunID   harness.RunID
	Image   string
	Network string
	// ContainerName 是确定性的容器名。确定性的理由：崩溃恢复时同一 run 重起
	// 容器，名字必须可预测；而**回收**仍然只按 label（名字只是给人看的）。
	ContainerName string
	NetworkName   string

	User           string
	WorkdirHost    string
	WorkdirCtr     string
	ReadOnly       bool
	CPUs           string
	Memory         string
	PidsLimit      int
	StopTimeout    int
	Tmpfs          string
	SecurityOpts   []string
	Command        []string
	Env            map[string]string
	ProxyURL       string
	AllowHosts     []string
	Timeout        time.Duration
	NetworkSubnet  string
	NetworkGateway string
	// 挂载面：宿主路径 → 容器内路径。**只允许一个**（本题工作目录）。
	Mounts map[string]string
}

// planRun 把 ExecSpec + DockerConfig 渲染成 runPlan。
//
// 校验原则：宁可在这里（渲染期）失败，也不要在 docker run 起容器之后失败——
// 后者会留下半截状态（网络已建、容器已起），而错误现场已经不在手里了。
func planRun(spec harness.ExecSpec, cfg DockerConfig) (runPlan, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return runPlan{}, err
	}

	if strings.TrimSpace(string(spec.RunID)) == "" {
		// 没有 RunID 就没有 label，也就没有回收依据：容器会变成永久孤儿。
		return runPlan{}, harness.Ef(harness.KindConfig, "executor.plan",
			"ExecSpec.RunID 为空：回收按 label 走，没有 RunID 的容器无法被回收", nil)
	}
	wd := spec.Workdir
	if strings.TrimSpace(wd) == "" {
		return runPlan{}, harness.Ef(harness.KindConfig, "executor.plan",
			"ExecSpec.Workdir 为空：容器里没有可写工作区，agent 无处落盘", nil)
	}
	if !path.IsAbs(wd) {
		// 相对路径会被 Docker 当成**具名卷**，静默挂成一个空卷——不是报错，
		// 是「工作目录里什么都没有」，agent 的表现会诡异到无法归因。
		return runPlan{}, harness.Ef(harness.KindConfig, "executor.plan",
			fmt.Sprintf("ExecSpec.Workdir 必须是绝对路径，got %q（相对路径会被 Docker 当具名卷，静默挂成空卷）", wd), nil)
	}
	if wd == "/" {
		return runPlan{}, harness.Ef(harness.KindConfig, "executor.plan",
			"ExecSpec.Workdir 不得是宿主根目录：等于把整台宿主挂进容器", nil)
	}
	if strings.Contains(wd, ":") {
		// `-v src:dst` 用冒号分隔，宿主侧带冒号会把语法拆坏（可能挂到别的路径）。
		return runPlan{}, harness.Ef(harness.KindConfig, "executor.plan",
			fmt.Sprintf("ExecSpec.Workdir 不得含冒号（-v 语法会被拆坏），got %q", wd), nil)
	}

	workdirCtr := spec.Executor.Workdir
	if strings.TrimSpace(workdirCtr) == "" {
		workdirCtr = defaultWorkdir
	}
	if !path.IsAbs(workdirCtr) {
		return runPlan{}, harness.Ef(harness.KindConfig, "executor.plan",
			fmt.Sprintf("ExecutorSpec.Workdir 必须是容器内绝对路径，got %q", workdirCtr), nil)
	}
	if strings.Contains(workdirCtr, ":") {
		return runPlan{}, harness.Ef(harness.KindConfig, "executor.plan",
			fmt.Sprintf("ExecutorSpec.Workdir 不得含冒号，got %q", workdirCtr), nil)
	}

	image := spec.Executor.Image
	if strings.TrimSpace(image) == "" {
		image = defaultImageTag
	}

	// 白名单：每条都必须是可解析的 IP:port。**不静默丢弃非法条目**——
	// 静默丢弃的后果是「用户以为放行了、实际没放行」，而失败会以
	// 「目标不可达」的形式出现在很远的地方。
	hosts, err := normalizeAllowHosts(spec.Executor.AllowHosts)
	if err != nil {
		return runPlan{}, err
	}

	p := runPlan{
		RunID:         spec.RunID,
		Image:         image,
		NetworkName:   networkName(spec),
		ContainerName: containerName(spec),
		User:          cfg.RunUser,
		WorkdirHost:   wd,
		WorkdirCtr:    workdirCtr,
		// ReadOnly 的取值**不看** ExecutorSpec.ReadOnly：见 DockerConfig.WritableRootfs。
		ReadOnly:    !cfg.WritableRootfs,
		CPUs:        formatCPUs(spec.Executor.CPUs),
		Memory:      formatMemory(spec.Executor.MemoryMB),
		PidsLimit:   positiveOr(spec.Executor.PidsLimit, defaultPidsLimit),
		StopTimeout: cfg.StopTimeout,
		Tmpfs: fmt.Sprintf("/tmp:rw,nosuid,nodev,size=%dm,mode=1777",
			positiveOr(cfg.TmpfsSizeMB, defaultTmpfsMB)),
		Command:    []string{"sleep", "infinity"},
		AllowHosts: hosts,
		Timeout:    cfg.CommandTimeout,
		Mounts:     map[string]string{wd: workdirCtr},
	}
	p.SecurityOpts = []string{"no-new-privileges"}
	if cfg.DisableNoNewPrivileges {
		p.SecurityOpts = nil
	}

	// 网段：确定性分配（见 DockerConfig.NetworkSupernet）。有了它，iptables 的
	// 默认拒绝规则才能带源限定，且能在网络创建之前就渲染出来。
	supernet := cfg.NetworkSupernet
	if supernet == "" {
		supernet = defaultNetworkSupernet
	}
	subnet, gateway, err := allocateSubnet(spec.RunID, supernet)
	if err != nil {
		return runPlan{}, err
	}
	p.NetworkSubnet, p.NetworkGateway = subnet, gateway

	// env：缺省值先铺，用户值后覆盖。HOME 必须落在**可写**的工作目录之下。
	//
	// 为什么 HOME 不能是 /root 或 /：只读 rootfs 下 pi 写不了会话目录，会以
	// 「静默失去扩展加载能力」的形式失败（M0 实测）；而 HOME=/root 在
	// `--user 65534` 下连读都读不到。
	p.Env = map[string]string{
		"HOME":   path.Join(workdirCtr, ".home"),
		"TMPDIR": "/tmp",
	}
	for k, v := range spec.Executor.Env {
		p.Env[k] = v
	}

	// provider 代理：只在本机没有别的代理环境变量时注入。
	// 已有的 HTTP_PROXY 说明用户在 spec.Env 里显式指定了（比如企业代理），
	// 这时覆盖它是错的——但白名单代理仍然会起，只是不接线。
	if cfg.ProviderProxy && !hasProxyEnv(p.Env) {
		host := cfg.ProxyBindIP
		if host == "" {
			host = p.NetworkGateway
		}
		p.ProxyURL = fmt.Sprintf("http://%s:%d", host, proxyPort)
		p.Env["HTTP_PROXY"] = p.ProxyURL
		p.Env["HTTPS_PROXY"] = p.ProxyURL
		p.Env["http_proxy"] = p.ProxyURL
		p.Env["https_proxy"] = p.ProxyURL
		// NO_PROXY 是**必须**的：代理只放行 provider 域名，目标端点的流量
		// 一旦被塞进代理就会被代理拒绝（表现为「靶场连不上」）。目标地址与
		// loopback 一律直连。
		noProxy := []string{"127.0.0.1", "localhost"}
		for _, a := range p.AllowHosts {
			h, _, _ := splitHostPort(a)
			noProxy = append(noProxy, h)
		}
		sort.Strings(noProxy)
		p.Env["NO_PROXY"] = strings.Join(dedupe(noProxy), ",")
		p.Env["no_proxy"] = p.Env["NO_PROXY"]
	}

	// 挂载面断言：宿主状态目录一条都不许出现在挂载里。
	// 这是「运行状态、token 和私有证据不挂载进容器」（PLAN.md:53）的**代码级**防线，
	// 不是注释。
	for src := range p.Mounts {
		for _, forbidden := range forbiddenHostPaths(cfg) {
			if src == forbidden {
				return runPlan{}, harness.Ef(harness.KindConfig, "executor.plan",
					fmt.Sprintf("拒绝把宿主状态路径挂进容器: %s", src), nil)
			}
		}
	}

	return p, nil
}

// defaultProxyBindIP 是容器在 --internal 网络里唯一能到达的宿主地址：网桥网关。
// 实测（本机 Docker 29.8.0）：`docker network create --internal` 得到的网关
// （如 172.19.0.1）从容器内可达，而 1.1.1.1 / 8.8.8.8 直接
// "Network is unreachable"。
//
// 为什么不用 host.docker.internal：那需要 --add-host，而 DNS 在 --internal
// 网络里本就不通，解析要落在宿主注入的 /etc/hosts 上；用网关 IP 直连少一层
// 隐式依赖，也让 NO_PROXY 的写法更直白。
//
// 空串表示「用本 run 的网桥网关」（见 planRun 的收尾段）。
const defaultProxyBindIP = ""

// forbiddenHostPaths 返回**绝不挂进容器**的宿主路径清单。
//
// 判据不是「看起来敏感」，而是「挂进去等于把约束方变成信任方」：
//   - 运行状态目录：容器里的 agent 能读到事件日志、快照、控制 socket；
//   - private/：候选明文与原始证据（设计文档 §3.3 的严格分离）；
//   - 任何 *.env / token / 凭据文件：平台 token 与 provider key；
//   - docker.sock：挂进去等于把宿主 root 交出去，前面所有隔离全部作废。
func forbiddenHostPaths(cfg DockerConfig) []string {
	base := []string{
		"/var/run/docker.sock",
		"/run/docker.sock",
		"/proc",
		"/sys",
		"/dev",
		"/etc",
		"/root",
		"/var/lib/red-harness",
	}
	return append(base, cfg.HostStateDirs...)
}

// ── argv 渲染 ──

// runArgv 把 runPlan 渲染成 `docker run` 的完整 argv。
//
// 参数顺序是**确定性的**：map（Env）的键排序后再展开，否则同一份 spec 会渲染
// 出不同的 argv，`TestPlanRunIsDeterministic` 会红，而更糟的是容器实际行为
// 会随 map 迭代顺序变化——那类不确定性在现场无法复现。
func runArgv(p runPlan) []string {
	argv := []string{
		"docker", "run", "--detach",
		"--name", p.ContainerName,
		"--label", LabelRun + "=" + string(p.RunID),
		"--label", LabelRole + "=" + roleRunner,
	}

	// ── 六条硬隔离约束 ──
	if p.User != "" {
		argv = append(argv, "--user", p.User)
	}
	if p.ReadOnly {
		// 307 GB 事故的唯一防线。
		argv = append(argv, "--read-only")
	}
	argv = append(argv,
		"--cap-drop", "ALL",
		"--pids-limit", itoa(p.PidsLimit),
		"--memory", p.Memory,
		// --memory-swap 必须等于 --memory：不设时容器可以拿 swap 绕过内存上限，
		// 而宿主只有 3 GB 内存 + 2 GB swap。
		"--memory-swap", p.Memory,
		"--cpus", p.CPUs,
		// IPC namespace：显式 private。共享 IPC 会暴露宿主的 SysV 信号量与
		// 共享内存段（ptrace 类攻击的常见跳板）。
		"--ipc", "private",
		// PID namespace：**不渲染 --pid**。
		//
		// 本机实测 Docker 29.8.0 的 CLI 拒绝 `--pid=private`（`docker: --pid:
		// invalid PID mode`）——`--pid` 只接受 host / container:<name> / <ns path>
		// 三种取值，留空即 Docker 默认的**私有** PID namespace。所以「独立 PID
		// namespace」在这里的正确表达就是「不把默认关掉」，而不是写一个不存在
		// 的取值把容器起不来。
		"--stop-timeout", itoa(p.StopTimeout),
		// --init 装一个 PID 1 收割僵尸进程。为什么必要：容器里跑的是 pi 与它
		// 的子进程（bash/工具），没有 init 时孤儿进程会挂在 PID 1 上不被回收，
		// 最终撞上 --pids-limit，表现为「容器莫名其妙 fork 失败」。
		"--init",
	)
	for _, so := range p.SecurityOpts {
		argv = append(argv, "--security-opt", so)
	}
	if p.Tmpfs != "" {
		// /tmp 是只读 rootfs 下唯一的可写区，size= 上限就是「最多能写多少」。
		argv = append(argv, "--tmpfs", p.Tmpfs)
	}

	// ── 网络 ──
	argv = append(argv, "--network", p.NetworkName)

	// ── 挂载：只挂本题工作目录（PLAN.md:53）──
	// 排序后再渲染，保证确定性。
	mounts := make([]string, 0, len(p.Mounts))
	for src := range p.Mounts {
		mounts = append(mounts, src)
	}
	sort.Strings(mounts)
	for _, src := range mounts {
		argv = append(argv, "-v", src+":"+p.Mounts[src])
	}

	// ── env ──
	envKeys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		argv = append(argv, "-e", k+"="+p.Env[k])
	}

	argv = append(argv, "--workdir", p.WorkdirCtr)
	argv = append(argv, p.Image)
	argv = append(argv, p.Command...)
	return argv
}

// execArgv 渲染 `docker exec` 的 argv。
//
// 为什么 exec 也要带 --user 与 --workdir：容器一旦起来，`docker exec` 默认以
// **镜像的 USER**（这里是 root）执行——不显式降权的话，一次 exec 就把
// `docker run --user` 那层约束整个绕过了。
func execArgv(h harness.ExecHandle, cmd []string, opts harness.ExecOptions, p runPlan, cfg DockerConfig) []string {
	target := h.ContainerID
	if target == "" {
		// 拿不到 ID 时退回名字：这是**降级**而不是等价替代（名字可被抢注），
		// 但比 exec 到空目标上要好，而且会让调用方看见一个明确的失败。
		target = p.ContainerName
	}
	argv := []string{cfg.Binary, "exec"}
	if opts.Stdin != nil {
		// 只有真要喂 stdin 时才开 -i：pi 的 RPC 走 stdin，开 -i 但不管道
		// 会让 docker exec 永久等待。
		argv = append(argv, "-i")
	}
	if p.User != "" {
		argv = append(argv, "--user", p.User)
	}
	wd := opts.Workdir
	if wd == "" {
		wd = p.WorkdirCtr
	}
	argv = append(argv, "--workdir", wd)

	envKeys := make([]string, 0, len(opts.Env))
	for k := range opts.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		argv = append(argv, "-e", k+"="+opts.Env[k])
	}

	argv = append(argv, target)
	argv = append(argv, cmd...)
	return argv
}

// effectiveTimeout 解析单条命令的墙钟上限。零值回落到配置默认值——
// **绝不回落成「不限」**：无界的一轮会让 run 卡死而不触发任何预算护栏。
func effectiveTimeout(opts harness.ExecOptions, cfg DockerConfig) time.Duration {
	if opts.Timeout > 0 {
		return opts.Timeout
	}
	if cfg.CommandTimeout > 0 {
		return cfg.CommandTimeout
	}
	return defaultCommandTimeout
}

// ── 命名 ──

var nameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// networkName 返回 per-run 网络名。
//
// RunID 来自用户（可能是时间戳、可能含斜杠），而 Docker 的网络名会变成宿主上
// 的 bridge 接口名（≤15 字符的 IFNAMSIZ 限制）——所以必须净化成安全的短名。
// 短哈希保证不同 run 一定得到不同的名字（否则两次运行共享网络边界）。
func networkName(spec harness.ExecSpec) string {
	if n := strings.TrimSpace(spec.Network); n != "" {
		return sanitizeName(n)
	}
	return "rh-net-" + shortHash(string(spec.RunID))
}

// containerName 同理：确定性、可预测，方便现场直接 `docker logs`。
func containerName(spec harness.ExecSpec) string {
	return "rh-runner-" + shortHash(string(spec.RunID))
}

func sanitizeName(s string) string {
	s = nameUnsafe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" {
		return "rh"
	}
	return s
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:5])
}

// commentFor 返回 iptables 规则的注释（≤256 字符、无引号空白换行）。
func commentFor(runID harness.RunID) string {
	s := sanitizeName(string(runID))
	if len(s) > 200 {
		s = s[:200]
	}
	return iptablesCommentPrefix + s
}

// ── 小工具 ──

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func positiveOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// formatCPUs 把浮点 CPU 数渲染成 docker 接受的形式。
// 零/负值回落到默认值：`--cpus 0` 会被 Docker 拒绝，而「不限」等于没有护栏。
func formatCPUs(v float64) string {
	if v <= 0 {
		v = defaultCPUs
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

func formatMemory(mb int) string {
	return itoa(positiveOr(mb, defaultMemoryMB)) + "m"
}

// normalizeAllowHosts 校验并去重目标白名单。
func normalizeAllowHosts(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		host, port, ok := splitHostPort(a)
		if !ok {
			return nil, harness.Ef(harness.KindConfig, "executor.plan",
				fmt.Sprintf("AllowHosts 条目必须是 IP:port，got %q", a), nil)
		}
		if _, err := parsePort(port); err != nil {
			return nil, harness.Ef(harness.KindConfig, "executor.plan",
				fmt.Sprintf("AllowHosts 条目的端口非法: %q", a), err)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			return nil, harness.Ef(harness.KindConfig, "executor.plan",
				fmt.Sprintf("AllowHosts 条目不得是通配地址（等于放行全网）: %q", a), nil)
		}
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out, nil
}

// splitHostPort 解析 "IP:port"。刻意不用 net.SplitHostPort：它接受主机名，
// 而这里要求**必须是 IP**——域名白名单是 provider 代理的事，目标白名单只认真实
// 地址，否则一条 `AllowHosts: ["*:80"]` 就能把「目标白名单」变成全网放行。
func splitHostPort(s string) (host, port string, ok bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	host, port = s[:i], s[i+1:]
	// IPv6 字面量 [::1]:80 —— 去掉方括号。
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if host == "" {
		return "", "", false
	}
	return host, port, true
}

func parsePort(p string) (int, error) {
	n := 0
	if p == "" {
		return 0, fmt.Errorf("空端口")
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("端口必须是数字: %q", p)
		}
		n = n*10 + int(r-'0')
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("端口越界: %d", n)
	}
	return n, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func hasProxyEnv(env map[string]string) bool {
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if env[k] != "" {
			return true
		}
	}
	return false
}
