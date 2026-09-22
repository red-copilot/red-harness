package executor

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"

	harness "github.com/red-copilot/red-harness"
)

// 本文件负责「网络边界」这一半隔离：per-run bridge 网络 + 目标白名单 + 默认拒绝。
//
// # 实现方式：per-run bridge + iptables（`--internal` 作为可选加强）
//
// PLAN.md:52 给了两条路：`--internal` + 显式端口映射，或 iptables 规则。这里选
// **iptables 为主、`--internal` 为可选**，依据是本机实测：
//
//   - `--internal` 确实给出结构性的默认拒绝。实测：容器内 `ip route` 只剩一条
//     `172.x.0.0/16 dev eth0`（连默认网关都没有），对 1.1.1.1 直接
//     "Network is unreachable"。
//
//   - **但 `--internal` 同时掐掉了到宿主的路由**，而 TSecBench 返回的靶场地址
//     就是「宿主机可达的地址」（经 VPN 直连），容器必须经宿主转发才能到。
//     实测：`--internal` 网络里容器连宿主的出网地址直接 "Network is
//     unreachable"，连 INPUT 链都到不了——白名单里的目标一样不可达。
//     即「默认拒绝做到了，但连该放行的也一起拒了」。
//
// 所以默认（DockerConfig.InternalNetwork=false）走 iptables：per-run bridge 给出
// 网络隔离（每个 run 一张自己的 bridge，互相不可达），iptables 给出「只放行
// 白名单目标 + 宿主代理端口，其余 DROP」。`--internal` 保留为配置项，给
// 「宿主上没有 iptables」或「靶场就在本机容器里」的部署形态用。
//
// 为什么不选「显式端口映射（-p）到宿主，再让容器连宿主端口」：每个目标端口都要
// 在宿主的所有接口上开一个监听，等于把靶场暴露到宿主全网；而且端口映射只适用于
// 「靶场在宿主上」这一种形态，对经 VPN 直连的远端靶场无能为力。
//
// # 规则分两条链：这是实测出来的，不是设计洁癖
//
// iptables 的两条路径**不是同一回事**，本机实测（每条都带计数器验证）：
//
//   - 容器 → 别的网络里的地址（或公网）：走 FORWARD 链。DOCKER-USER 挂在
//     FORWARD 上，规则生效（实测：`-s <网段> -j DROP` 让连接失败，且规则计数器
//     在涨）。这一条承载「远端靶场白名单」。
//
//   - 容器 → 宿主自己（网桥网关、宿主任何本地地址）：**根本不到 FORWARD**。
//     它进的是 INPUT 链。实测：只装 DOCKER-USER 的规则时，容器连宿主上的监听
//     照样成功；在 INPUT 上装 `-i <网桥> -s <网段> -j DROP` 之后才被拒，加回
//     `--dport <代理端口> -j ACCEPT` 后又通。所以宿主代理端口的放行与「到宿主
//     其他端口」的拒绝必须在 INPUT 上写。
//
// 漏掉 INPUT 那一条的后果是静默的：容器可以扫宿主的所有监听端口（sshd、看板、
// 别的服务），而 DOCKER-USER 里看起来「规则都写了」。
//
// # 一个诚实的边界（写在这里以免被当成漏洞）
//
// 与容器**同属本 run 网络**的地址（比如靶场容器被 docker 起来时与 runner 落在
// 同一个 bridge 上）之间的流量走二层交换，**不经过 iptables**。本机实测：同一
// bridge 内的 ACCEPT/DROP 规则计数器恒为 0，连接照常成功。这是 bridge 网络的
// 固有行为（除非加载 br_netfilter 并打开 bridge-nf-call-iptables，而那会改变
// 宿主上所有 bridge 的行为，代价远超收益）。
//
// 在 v1 的真实形态下这不构成缺口：靶场是 TSecBench 侧经 VPN 直连的地址，与
// runner 不在同一个 bridge 上，所以目标白名单对它是**真实生效**的（走 FORWARD）。
// 但「同网段可达」这件事必须在文档里写明，而不是假装规则覆盖了它。

// chainForward / chainInput 是规则落地的两条链。
const (
	chainForward = "DOCKER-USER"
	chainInput   = "INPUT"
)

// ruleSpec 是一条待安装的 iptables 规则：链 + 参数。
//
// 把链也建模进来（而不是只渲染参数串）的原因：同一条语义（放行/拒绝）在两条
// 链上的写法不同，而且**漏写一条链的失败是静默的**（见文件头）。显式建模后，
// 测试可以断言「两条链上都有规则」。
type ruleSpec struct {
	chain string
	args  []string
}

// String 返回规则的参数部分（不含链名）。
func (r ruleSpec) String() string { return strings.Join(r.args, " ") }

// network 是一次运行的网络边界。
type network struct {
	Name string
	// RunID + Owner 是这条资源的**回收判据**（见 spec.go 的 LabelOwner）。网络
	// 也要带 owner：孤儿网络与孤儿容器一样占宿主资源（网段），而只按 run 删会让
	// 另一个部署的同名 run 的网络被拆掉——那会连带掐断正在跑的 agent 的全部流量。
	//
	// 网络**不带** challenge：网络是 per-run 的，没有题目维度。
	RunID   harness.RunID
	Owner   harness.OwnerID
	Subnet  string
	Gateway string
	// Bridge 是本网络的宿主网桥接口名（br-<网络 ID 前 12 位>）。只有在网络
	// 创建之后才拿得到，所以它是**可选**的：为空时 INPUT 规则不带 -i
	// （仍然带 -s 源限定，不会误伤别的容器）。
	Bridge string
	// Internal 为真时网络带 `--internal`（结构性默认拒绝）。
	Internal bool
	// ManageIptables 为真时由本执行器维护规则。
	ManageIptables bool
	// AllowHosts 是目标白名单（IP:port，已去重排序）。
	AllowHosts []string
	// ProxyPort 是宿主侧代理端口（0 表示不管理）。
	ProxyPort int
	// insertAccepts 为真表示放行规则必须插到拒绝规则**之前**
	// （iptables 是首个匹配生效；插在后面等于放行永不生效）。
	insertAccepts bool
}

// networkFor 由 runPlan + 配置构造网络描述。
func networkFor(p runPlan, cfg DockerConfig) network {
	n := network{
		Name:  p.NetworkName,
		RunID: p.RunID,
		// owner 取自 **plan** 而不是 cfg：plan 的 owner 一定是 normalize 之后的
		// 值，而 networkFor 的 cfg 参数可能是没走过 normalize 的原样配置。
		// 更重要的一点：容器与网络必须拿到**同一个** owner——两者判据不一致时，
		// 「同一个 run 的容器被删、网络留下」这类半截回收现场无法解释。
		Owner:          p.Owner,
		Subnet:         p.NetworkSubnet,
		Gateway:        p.NetworkGateway,
		Internal:       cfg.InternalNetwork,
		ManageIptables: cfg.ManageIptables,
		AllowHosts:     p.AllowHosts,
		insertAccepts:  true,
	}
	if cfg.ProviderProxy {
		n.ProxyPort = proxyPort
	}
	return n
}

// createArgv 渲染 `docker network create` 的 argv。
func (n network) createArgv() []string {
	argv := []string{
		"docker", "network", "create",
		"--label", LabelRun + "=" + string(n.RunID),
		"--label", LabelOwner + "=" + string(n.Owner),
		"--label", LabelRole + "=" + "network",
	}
	if n.Internal {
		// 去掉默认路由 ⇒ 出站默认拒绝是结构性的，不依赖规则顺序。
		argv = append(argv, "--internal")
	}
	if n.Subnet != "" {
		// 网段由 run 决定（见 allocateSubnet），而不是让 Docker 从地址池里挑：
		// iptables 规则要在**创建之前**就渲染出来（否则 DROP 规则得等到创建后
		// 才装，中间那段时间是敞开的），所以网段必须先知道。
		argv = append(argv, "--subnet", n.Subnet)
	}
	argv = append(argv, n.Name)
	return argv
}

// permitRules 返回放行规则：目标白名单 + 宿主代理端口。
//
// 为什么要精确到端口：只放行 IP 的话，目标机上任何一个开放端口都可达，
// 「只允许到 TSecBench 返回的 IP:port」就退化成了「只允许到那个 IP」。
//
// 为什么**每条目标规则都要写两条链**：目标地址的形态决定它走哪条链，而
// 执行器在装规则时**不知道**它是哪一种（TSecBench 返回的可能是经 VPN 直连的
// 远端地址，也可能是宿主自己身上的地址——本地靶场、端口转发、代理入口）。
//
//   - 远端地址 → 走 FORWARD（DOCKER-USER）。
//   - 宿主自己的地址 → 走 INPUT，**根本不到 FORWARD**（本机实测，见文件头）。
//
// 只写 FORWARD 的失败是静默且极难归因的：靶场在宿主上时，容器发出的 SYN 会被
// INPUT 上的默认拒绝规则吃掉，表现为「白名单里的目标全部超时」——而
// DOCKER-USER 里规则看起来「明明写了」。所以这里两条链都写，让两种形态都可达。
// 重复放行同一个地址不构成安全损失：两条链各自只覆盖自己那条路径。
func (n network) permitRules() []ruleSpec {
	if !n.ManageIptables {
		return nil
	}
	out := make([]ruleSpec, 0, 2*len(n.AllowHosts)+1)
	for _, a := range n.AllowHosts {
		host, port, ok := splitHostPort(a)
		if !ok {
			continue
		}
		out = append(out,
			n.rule(chainForward, "-d", host, port, "ACCEPT"),
			n.rule(chainInput, "-d", host, port, "ACCEPT"),
		)
	}
	if n.ProxyPort > 0 {
		// provider 代理在**宿主自己**身上，所以这条必须落在 INPUT 链上
		// （走 FORWARD 的写法在这里永远匹配不到，见文件头）。
		out = append(out, n.rule(chainInput, "-d", n.Gateway, itoa(n.ProxyPort), "ACCEPT"))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].chain != out[j].chain {
			return out[i].chain < out[j].chain
		}
		return out[i].String() < out[j].String()
	})
	return out
}

// denyRules 返回默认拒绝规则：FORWARD 与 INPUT 各一条。
//
// 两条都必须有，且都**不带目的地限定**——带了就拦不住白名单之外的目标，而拦住
// 那些正是它们存在的意义。两条都**必须带源限定**（-s 网段）：不带的话会掐死
// 宿主上所有别的容器与所有别的入站流量。
func (n network) denyRules() []ruleSpec {
	if !n.ManageIptables {
		return nil
	}
	return []ruleSpec{
		n.rule(chainForward, "", "", "", "DROP"),
		n.rule(chainInput, "", "", "", "DROP"),
	}
}

// denyRule 返回 FORWARD 上的那条默认拒绝规则（单元测试与文档用）。
func (n network) denyRule() string {
	rs := n.denyRules()
	if len(rs) == 0 {
		return ""
	}
	return rs[0].String()
}

// rule 拼一条规则。
func (n network) rule(chain, dstFlag, dstHost, dstPort, action string) ruleSpec {
	args := []string{}
	if chain == chainInput && n.Bridge != "" {
		// INPUT 链上的规则按入接口限定：宿主上有很多接口，只约束本 run 那个网桥。
		args = append(args, "-i", n.Bridge)
	}
	if n.Subnet != "" {
		// 只约束本 run 那个网络的流量。
		args = append(args, "-s", n.Subnet)
	}
	if dstHost != "" {
		args = append(args, "-d", dstHost)
	}
	if dstPort != "" {
		args = append(args, "-p", "tcp", "--dport", dstPort)
	}
	args = append(args, "-j", action)
	// 注释是必须的：宿主重启后残留规则只能靠它归属到某个 run，
	// 否则要么删不掉，要么得靠猜（而猜错的代价是掐掉别人的网络）。
	//
	// ⚠️ 注释里必须带 owner（见 commentFor）：netfilter 表是宿主全局的，只带
	// runID 的注释在「两个部署各有一个 run-1」时无法区分彼此。
	args = append(args, "-m", "comment", "--comment", commentFor(n.Owner, n.RunID))
	return ruleSpec{chain: chain, args: args}
}

// allocateSubnet 为一次运行确定性地分配一个 /24 网段。
//
// 为什么不让 Docker 从默认地址池里挑：iptables 的 DROP 规则必须带源限定，
// 而源限定要求网段在**创建网络之前**就知道。确定性分配（run → 网段）同时
// 带来一个好处：同一 run 重起容器时网段不变，残留规则能对上号。
//
// 地址池默认取 10.211.0.0/16（Docker 默认池是 172.17-172.31，避开它以免与
// 其它容器抢地址；也刻意避开 10.0.0.0/8 的常用内网段——靶场地址就在那里）。
// 部署环境若真用了 10.211.x，运维可以改 DockerConfig.NetworkSupernet。
func allocateSubnet(runID harness.RunID, supernet string) (subnet, gateway string, err error) {
	ip, ipnet, err := net.ParseCIDR(supernet)
	if err != nil {
		return "", "", harness.Ef(harness.KindConfig, "executor.network",
			fmt.Sprintf("NetworkSupernet 非法: %q", supernet), err)
	}
	ones, bits := ipnet.Mask.Size()
	if bits-ones < 8 {
		return "", "", harness.Ef(harness.KindConfig, "executor.network",
			fmt.Sprintf("NetworkSupernet %q 太小：至少要能切出 /24", supernet), nil)
	}
	// 用 run 的哈希取第三个八位组，得到 1..254（0 与 255 留给网络/广播）。
	sum := sha256.Sum256([]byte("subnet/" + string(runID)))
	third := 1 + int(binary.BigEndian.Uint16(sum[:2])%254)

	base := ip.To4()
	if base == nil {
		return "", "", harness.Ef(harness.KindConfig, "executor.network",
			"NetworkSupernet 必须是 IPv4", nil)
	}
	net4 := net.IPv4(base[0], base[1], byte(third), 0)
	gw4 := net.IPv4(base[0], base[1], byte(third), 1)
	return net4.String() + "/24", gw4.String(), nil
}

// ── provider 域名白名单代理 ──

// proxyConfig 是代理的配置。
type proxyConfig struct {
	// AllowHosts 是允许的域名，支持 `*.example.com` 通配。
	// **通配只匹配子域**，不匹配裸域（`*.example.com` 不该放行 example.com）。
	AllowHosts []string
	// Bind 是监听地址（host:port）。
	Bind string
}

// allowlistProxy 是宿主侧的 CONNECT 代理，按域名白名单放行。
//
// 为什么只做 CONNECT：provider 的 API 走 HTTPS，客户端先 CONNECT 建隧道。
// 这里**不解析 TLS**（不需要知道内容），只做「域名 + 端口」的准入判定——
// 代理一旦要解析内容就得持有证书、实现 MITM，那是另一个量级的复杂度与风险，
// 而且会把 provider 的凭据暴露在代理进程里。
type allowlistProxy struct {
	hosts map[string]bool
	// wildcards 是去掉 `*.` 前缀的通配后缀，形如 ".example.org"。
	wildcards []string
}

func newAllowlistProxy(cfg proxyConfig) (*allowlistProxy, error) {
	p := &allowlistProxy{hosts: map[string]bool{}}
	for _, raw := range cfg.AllowHosts {
		h := strings.ToLower(strings.TrimSpace(raw))
		if h == "" {
			continue
		}
		if strings.HasPrefix(h, "*.") {
			suffix := h[1:] // ".example.org"
			if suffix == "." {
				return nil, fmt.Errorf("非法的通配白名单条目: %q", raw)
			}
			p.wildcards = append(p.wildcards, suffix)
			continue
		}
		// 形如 `api.example.com:443` 的条目：去掉端口，端口由 allows 统一管。
		if host, _, ok := splitHostPort(h); ok {
			h = strings.ToLower(host)
		}
		p.hosts[h] = true
	}
	sort.Strings(p.wildcards)
	return p, nil
}

// allows 判定一次 CONNECT 目标是否放行。
//
// **fail closed**：白名单为空时全拒；端口不是 443 时全拒。
//
// 为什么锁 443：CONNECT 一旦允许任意端口，白名单就退化成「任意 TCP 转发」，
// 容器里的 agent 可以把代理当跳板去连白名单域名上的任意服务（比如某个
// 白名单域名恰好也提供 SOCKS/HTTP 代理，那就是一条完整的出站通道）。
func (p *allowlistProxy) allows(hostport string) bool {
	host := strings.ToLower(strings.TrimSpace(hostport))
	port := "443"
	if h, pt, ok := splitHostPort(host); ok {
		host, port = strings.ToLower(h), pt
	}
	if port != "443" {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	// 裸 IP 一律拒绝：白名单是**域名**白名单。放行 IP 等于绕开域名约束。
	if net.ParseIP(host) != nil {
		return false
	}
	if p.hosts[host] {
		return true
	}
	for _, w := range p.wildcards {
		// w 形如 ".example.org"：要求 host 以它结尾且前面还有标签，所以
		// `a.example.org` 命中，而 `example.org`（裸域）与
		// `api.example.org.evil.test`（后缀伪装）都不命中。
		if strings.HasSuffix(host, w) && len(host) > len(w) {
			return true
		}
	}
	return false
}
