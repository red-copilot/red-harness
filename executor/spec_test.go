package executor

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// 本文件是**无 build tag** 的单元测试：它只断言「渲染出来的 docker argv 长什么样」，
// 不碰 Docker daemon。这样隔离约束在每台机器、每次 `go test ./...` 上都被钉住，
// 而不是只在装了 Docker 的机器上。
//
// 为什么把断言放在 argv 上：argv 是**唯一**决定容器隔离属性的地方。把约束写成
// 注释或者写在 README 里都会漂移；写成 argv 断言后，任何人删掉 `--cap-drop=ALL`
// 都会立刻红。

// ── 测试脚手架 ──

const testWorkdirHost = "/var/lib/red-harness/runs/run-1/work/web-01"

func testSpec() harness.ExecSpec {
	return harness.ExecSpec{
		RunID: "run-1",
		Target: harness.Target{
			Code:  "web-01",
			Addrs: []string{"10.0.0.5:8080"},
		},
		Executor: harness.ExecutorSpec{
			Image:      "red-harness-runner:v0.3.0",
			Workdir:    "/work",
			CPUs:       1.5,
			MemoryMB:   768,
			PidsLimit:  256,
			ReadOnly:   true,
			AllowHosts: []string{"10.0.0.5:8080"},
			Env:        map[string]string{"AGENT_MODE": "pentest"},
		},
		Network: "rh-net-run-1",
		Workdir: testWorkdirHost,
	}
}

func mustPlan(t *testing.T, spec harness.ExecSpec, cfg DockerConfig) runPlan {
	t.Helper()
	p, err := planRun(spec, cfg)
	if err != nil {
		t.Fatalf("planRun 失败: %v", err)
	}
	return p
}

// valueOf 返回 argv 里 `--flag value` 或 `--flag=value` 两种写法的值。
// 第二种返回 true；第一种返回紧随其后的元素。找不到返回 ("", false)。
func valueOf(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag {
			if i+1 < len(argv) {
				return argv[i+1], true
			}
			return "", false
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"="), true
		}
	}
	return "", false
}

func hasBare(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

func argvString(argv []string) string { return strings.Join(argv, " ") }

// ── 六条硬隔离约束（PLAN.md:51-54）──
//
// 每条一个子测试。约束清单来自 PLAN.md:51：
// 非 root、只读 rootfs + tmpfs、capability 全移除、no-new-privileges、
// CPU/内存/PID 上限、独立 PID/IPC namespace。

func TestSpecIsolationNonRootUser(t *testing.T) {
	argv := runArgv(mustPlan(t, testSpec(), defaultDockerConfig()))

	user, ok := valueOf(argv, "--user")
	if !ok {
		t.Fatalf("argv 缺少 --user（容器会以 root 跑）: %s", argvString(argv))
	}
	uid, gid, found := strings.Cut(user, ":")
	if !found {
		t.Fatalf("--user 必须同时给 uid 与 gid（只给 uid 时 gid 会落到 0=root）: %q", user)
	}
	if uid == "0" || uid == "root" || uid == "" {
		t.Errorf("--user 的 uid 不得是 root: %q", user)
	}
	if gid == "0" || gid == "root" || gid == "" {
		t.Errorf("--user 的 gid 不得是 root: %q", user)
	}
}

func TestSpecIsolationReadOnlyRootfsAndTmpfs(t *testing.T) {
	argv := runArgv(mustPlan(t, testSpec(), defaultDockerConfig()))

	if !hasBare(argv, "--read-only") {
		t.Fatalf("argv 缺少 --read-only（根文件系统可写 ⇒ 307 GB 事故的防线失效）: %s", argvString(argv))
	}

	// /tmp 必须是一个**有大小上限**的 tmpfs。只写 `--tmpfs /tmp` 而不给 size=
	// 等于把上限交给宿主内存，仍然可能把机器写死。
	var tmp string
	for i, a := range argv {
		if a == "--tmpfs" && i+1 < len(argv) {
			if strings.HasPrefix(argv[i+1], "/tmp:") || argv[i+1] == "/tmp" {
				tmp = argv[i+1]
			}
		}
	}
	if tmp == "" {
		t.Fatalf("argv 缺少 --tmpfs /tmp（只读 rootfs 下 agent 将无处可写，且没有大小上限）: %s", argvString(argv))
	}
	if !strings.Contains(tmp, "size=") {
		t.Errorf("--tmpfs /tmp 必须带 size= 上限，got %q", tmp)
	}
}

func TestSpecIsolationCapDropAll(t *testing.T) {
	argv := runArgv(mustPlan(t, testSpec(), defaultDockerConfig()))

	if !hasBare(argv, "--cap-drop") {
		t.Fatalf("argv 缺少 --cap-drop（默认保留 NET_RAW/SETUID 等能力）: %s", argvString(argv))
	}
	v, _ := valueOf(argv, "--cap-drop")
	if v != "ALL" {
		t.Errorf("--cap-drop 必须是 ALL（逐项列举会随 Docker 默认集变化而漂移），got %q", v)
	}
	// 不得出现把能力加回来的写法。
	for _, a := range argv {
		if a == "--cap-add" || strings.HasPrefix(a, "--cap-add=") || a == "--privileged" {
			t.Errorf("不得重新加回能力或开特权模式: %q", a)
		}
	}
}

func TestSpecIsolationNoNewPrivileges(t *testing.T) {
	argv := runArgv(mustPlan(t, testSpec(), defaultDockerConfig()))

	v, ok := valueOf(argv, "--security-opt")
	if !ok {
		t.Fatalf("argv 缺少 --security-opt=no-new-privileges: %s", argvString(argv))
	}
	if v != "no-new-privileges" {
		t.Errorf("--security-opt 必须是 no-new-privileges（setuid 二进制提权路径），got %q", v)
	}
}

func TestSpecIsolationResourceLimits(t *testing.T) {
	p := mustPlan(t, testSpec(), defaultDockerConfig())
	argv := runArgv(p)

	pids, ok := valueOf(argv, "--pids-limit")
	if !ok {
		t.Fatalf("argv 缺少 --pids-limit（fork 炸弹会打满宿主 PID 表）: %s", argvString(argv))
	}
	if pids != "256" {
		t.Errorf("--pids-limit 应取 ExecutorSpec.PidsLimit=256, got %q", pids)
	}

	mem, ok := valueOf(argv, "--memory")
	if !ok {
		t.Fatalf("argv 缺少 --memory（无上限的内存分配会触发宿主 OOM killer）: %s", argvString(argv))
	}
	if mem != "768m" {
		t.Errorf("--memory 应取 ExecutorSpec.MemoryMB=768, got %q", mem)
	}
	// --memory-swap 必须等于 --memory：否则容器可以拿 swap 绕过内存上限。
	swap, ok := valueOf(argv, "--memory-swap")
	if !ok {
		t.Fatalf("argv 缺少 --memory-swap（不设时容器可用 swap 绕过 --memory）: %s", argvString(argv))
	}
	if swap != mem {
		t.Errorf("--memory-swap 必须等于 --memory（禁用 swap），got %q vs %q", swap, mem)
	}

	cpus, ok := valueOf(argv, "--cpus")
	if !ok {
		t.Fatalf("argv 缺少 --cpus（挖矿/爆破会吃满宿主 CPU）: %s", argvString(argv))
	}
	if cpus != "1.5" {
		t.Errorf("--cpus 应取 ExecutorSpec.CPUs=1.5, got %q", cpus)
	}
}

func TestSpecIsolationSeparatePIDAndIPCNamespaces(t *testing.T) {
	p := mustPlan(t, testSpec(), defaultDockerConfig())
	argv := runArgv(p)

	// IPC namespace：显式 private。共享 IPC 会暴露宿主的 SysV 信号量与共享内存段。
	ipc, ok := valueOf(argv, "--ipc")
	if !ok {
		t.Fatalf("argv 缺少 --ipc=private: %s", argvString(argv))
	}
	if ipc != "private" {
		t.Errorf("--ipc 必须是 private，got %q", ipc)
	}

	// PID namespace：**不得**出现 --pid=host。
	//
	// 这里刻意**不**渲染 `--pid=private`：本机实测 Docker 29.8.0 的 CLI 直接
	// 拒绝该写法（`docker: --pid: invalid PID mode`），`--pid` 只接受
	// host/container:<name>/<ns path> 三种取值，留空即默认的私有 PID namespace。
	// 所以「独立 PID namespace」在这里表现为「不把默认关掉」，断言也据此写成
	// 「不得出现任何指向宿主 PID namespace 的取值」。
	if v, ok := valueOf(argv, "--pid"); ok {
		t.Errorf("不得渲染 --pid（Docker 默认即私有 PID namespace；本机实测无 --pid=private 写法），got --pid=%s", v)
	}
	for _, a := range argv {
		if a == "--pid=host" || strings.Contains(a, "container:") || strings.Contains(a, "/proc/") {
			t.Errorf("不得把宿主或别的容器的 PID namespace 接进来: %q", a)
		}
	}
}

// 墙钟超时：Exec 侧由 context 强制（见 docker.go），容器侧由 --stop-timeout 兜底，
// 免得 Reclaim 时 docker stop 把 SIGTERM 的宽限期拖成无限。
func TestSpecWallClockTimeout(t *testing.T) {
	cfg := defaultDockerConfig()
	cfg.CommandTimeout = 90 * time.Second
	p := mustPlan(t, testSpec(), cfg)
	argv := runArgv(p)

	if p.Timeout != 90*time.Second {
		t.Fatalf("plan 必须携带墙钟超时, got %v", p.Timeout)
	}
	st, ok := valueOf(argv, "--stop-timeout")
	if !ok {
		t.Fatalf("argv 缺少 --stop-timeout: %s", argvString(argv))
	}
	if st == "0" {
		t.Errorf("--stop-timeout 不得为 0（docker stop 会立刻 SIGKILL，agent 的收尾动作被截断）")
	}
}

// ── 挂载面（PLAN.md:53）──
//
// 「只挂本题工作目录；运行状态、token 和私有证据不挂载进容器」。
// 这是**真断言**：把 argv 里所有 -v 收集起来逐个检查，而不是写条注释了事。

func TestSpecMountsOnlyChallengeWorkdir(t *testing.T) {
	spec := testSpec()
	argv := runArgv(mustPlan(t, spec, defaultDockerConfig()))

	var mounts []string
	for i, a := range argv {
		if a == "-v" || a == "--volume" || a == "--mount" {
			if i+1 < len(argv) {
				mounts = append(mounts, argv[i+1])
			}
		}
	}
	if len(mounts) != 1 {
		t.Fatalf("必须恰好挂载 1 个宿主目录（本题工作目录），got %d: %v", len(mounts), mounts)
	}
	want := testWorkdirHost + ":/work"
	if mounts[0] != want && mounts[0] != want+":rw" && mounts[0] != want+":ro" {
		t.Errorf("唯一允许的挂载是本题工作目录 %s, got %q", want, mounts[0])
	}
}

// 宿主状态目录的**逐条**否定断言。任何一条被挂进容器，都意味着容器里的 agent
// （被约束方）拿到了宿主的运行状态、凭据或私有证据。
func TestSpecNeverMountsHostState(t *testing.T) {
	// 这些路径是引擎/存储层的真实资产位置（见设计文档 §6 存储布局与 §7 敏感文件）。
	forbidden := []string{
		"/var/lib/red-harness/runs/run-1",         // 整个运行目录
		"/var/lib/red-harness/runs/run-1/private", // 私有证据与候选明文账本
		"private/",
		"run.json",
		"events.jsonl",
		"graph.json",
		".agent.env", // 平台 token
		".env",
		"token",
		"credentials",
		"/root/.claude",
		"/root/.local/share/pi-node", // 宿主 pi 运行时（容器内必须用自己的副本）
		"/var/run/docker.sock",       // 挂进去等于把宿主 root 交出去
		"/proc",
		"/sys",
	}

	// 用几个不同的 Workdir 取值都跑一遍：断言必须对任意工作目录成立，
	// 而不是只对测试里那一个成立。
	for _, wd := range []string{
		testWorkdirHost,
		"/tmp/rh-work",
		"/var/lib/red-harness/runs/run-1/work",
	} {
		spec := testSpec()
		spec.Workdir = wd
		argv := runArgv(mustPlan(t, spec, defaultDockerConfig()))

		for _, a := range argv {
			lower := strings.ToLower(a)
			for _, f := range forbidden {
				if !strings.Contains(lower, strings.ToLower(f)) {
					continue
				}
				// 工作目录本身是允许的宿主路径——但它必须是**被挂载的那一个**，
				// 且只有当 forbidden 项恰好是它自己时才算合法命中。
				if strings.Contains(wd, f) && strings.HasPrefix(a, wd+":") {
					continue
				}
				t.Errorf("argv 出现了宿主状态路径 %q（元素 %q, workdir=%s）", f, a, wd)
			}
		}

		// 挂载项本身再查一遍：挂载的宿主侧必须**等于**本题工作目录。
		for i, a := range argv {
			if a != "-v" && a != "--volume" {
				continue
			}
			src, _, _ := strings.Cut(argv[i+1], ":")
			if src != wd {
				t.Errorf("挂载源必须是本题工作目录 %q, got %q", wd, src)
			}
		}
	}
}

// ── 网络（PLAN.md:52）──

func TestSpecNetworkIsPerRunAndLabeled(t *testing.T) {
	spec := testSpec()
	p := mustPlan(t, spec, defaultDockerConfig())
	argv := runArgv(p)

	netw, ok := valueOf(argv, "--network")
	if !ok {
		t.Fatalf("argv 缺少 --network: %s", argvString(argv))
	}
	if netw != spec.Network {
		t.Errorf("--network 应取 ExecSpec.Network=%q, got %q", spec.Network, netw)
	}
	// 绝不能落在默认 bridge / host 上：默认 bridge 上所有容器互相可达，
	// host 网络则完全没有网络边界。
	switch netw {
	case "bridge", "host", "none", "default":
		t.Errorf("不得使用内置网络 %q（%s 上没有任何 per-run 边界）", netw, netw)
	}

	// run label 是回收的唯一依据（PLAN.md:54）。
	runLabel := false
	for i, a := range argv {
		if a == "--label" && i+1 < len(argv) && argv[i+1] == LabelRun+"="+string(spec.RunID) {
			runLabel = true
		}
	}
	if !runLabel {
		t.Errorf("argv 缺少 %s=%s 标签（回收会失效，宿主重启后容器变孤儿）: %s", LabelRun, spec.RunID, argvString(argv))
	}
}

// TestSpecContainerCarriesOwnershipLabels 钉住容器的三个归属标签。
//
// 为什么三个都要断言「值」而不只是「标签在」：这三条正是资源归属的**全部**信息
// （见 reclaim.go 的判据表）。owner 缺失/写错会让孤儿永远回收不掉（只报告不删）；
// challenge 与 attempt 是把「宿主上这一堆容器」对回某次运行的唯一线索——它们的
// 值错了不会让任何东西报错，只会让现场无法解释。
func TestSpecContainerCarriesOwnershipLabels(t *testing.T) {
	spec := testSpec()
	cfg := defaultDockerConfig()
	p := mustPlan(t, spec, cfg)
	argv := runArgv(p)

	// challenge 值的唯一真源是 harness.ChallengeIDFor——这里再算一遍是在断言
	// 「argv 里的那一串确实是这个函数的输出」，而不是把实现抄第二遍。
	wantChallenge, err := harness.ChallengeIDFor(spec.Target.Code)
	if err != nil {
		t.Fatalf("ChallengeIDFor(%q): %v", spec.Target.Code, err)
	}
	want := map[string]string{
		LabelRun + "=" + string(spec.RunID):                  "run 标签（回收的定位依据）",
		LabelOwner + "=" + string(p.Owner):                   "owner 标签（回收的归属依据）",
		LabelChallenge + "=" + string(wantChallenge):         "challenge 标签（题目身份的派生值）",
		LabelAttempt + "=" + itoa(int(harness.FirstAttempt)): "attempt 标签",
	}
	for i, a := range argv {
		if a != "--label" || i+1 >= len(argv) {
			continue
		}
		delete(want, argv[i+1])
	}
	for kv, why := range want {
		t.Errorf("容器 argv 缺少 %s（%s）: %s", kv, why, argvString(argv))
	}
	// owner 必须来自配置（normalize 之后），不得是空串：空值时判据会退化成
	// 「无主 ⇒ 只报告」，孤儿从此回收不掉。
	if p.Owner == harness.OwnerUnknown {
		t.Errorf("plan.Owner 不得为空（空 = 无主 = 永不回收）")
	}
}

// TestSpecNetworkCarriesOwnerNotChallenge 钉住网络的标签集合**刻意更小**。
//
// 网络是 per-run 的，没有题目维度：给它贴 challenge/attempt 会造出一个
// 「看起来参与判据、实际没人读」的字段，而下一个人会照它推断归属逻辑。
func TestSpecNetworkCarriesOwnerNotChallenge(t *testing.T) {
	spec := testSpec()
	cfg := defaultDockerConfig()
	p := mustPlan(t, spec, cfg)
	argv := networkFor(p, cfg).createArgv()
	joined := argvString(argv)

	if !strings.Contains(joined, "--label "+LabelOwner+"="+string(p.Owner)) {
		t.Errorf("网络必须带 owner 标签（只按 run 删会拆掉别的部署的同名 run）: %s", joined)
	}
	for _, l := range []string{LabelChallenge, LabelAttempt} {
		if strings.Contains(joined, l+"=") {
			t.Errorf("网络不该带 %s（网络是 per-run 的，没有题目维度）: %s", l, joined)
		}
	}
}

// TestPlanRejectsMissingTargetCode 钉住 challenge 的 fail closed。
//
// 空编号会让 challenge 标签退化成空串——「有标签但值不可用」比「没有标签」更糟：
// 看标签的人会以为归属已知。所以宁可这道题起不来。
func TestPlanRejectsMissingTargetCode(t *testing.T) {
	for _, code := range []string{"", "   ", "\t"} {
		spec := testSpec()
		spec.Target.Code = code
		if _, err := planRun(spec, defaultDockerConfig()); err == nil {
			t.Errorf("Target.Code=%q 必须被拒绝（否则 challenge 标签是空串）", code)
		} else if !harness.IsKind(err, harness.KindConfig) {
			t.Errorf("Target.Code=%q 应报 KindConfig, got %v", code, err)
		}
	}
}

// 目标白名单：只有 TSecBench 返回的 IP:port 被放行，其余出站默认拒绝。
//
// 规则分两条链落地（本机实测结论，见 network.go 文件头）：
//   - 目标走 FORWARD（DOCKER-USER）：靶场不在本 bridge 上时走这条。
//   - 目标**同时**要写 INPUT：靶场地址可能就在宿主自己身上（本地靶场、端口
//     转发），那种流量根本不到 FORWARD。只写 FORWARD 的失败是静默的——
//     「白名单里的目标全部超时」，而规则看起来「明明写了」。
//   - 宿主代理端口的放行与「到宿主其他端口」的拒绝走 INPUT。
func TestSpecAllowHostsDriveIptablesRules(t *testing.T) {
	spec := testSpec()
	spec.Executor.AllowHosts = []string{"10.0.0.5:8080", "10.0.0.7:443", "10.0.0.5:8080"} // 含一条重复
	cfg := defaultDockerConfig()

	p := mustPlan(t, spec, cfg)
	netc := networkFor(p, cfg)
	netc.Bridge = "br-test123456"

	var forwardTargets, inputTargets []ruleSpec
	for _, r := range netc.permitRules() {
		if !strings.Contains(r.String(), "-d 10.0.0.") {
			continue
		}
		if r.chain == chainForward {
			forwardTargets = append(forwardTargets, r)
		} else {
			inputTargets = append(inputTargets, r)
		}
	}
	// 去重后 2 个目标 × 2 条链 = 4 条。
	if len(forwardTargets) != 2 {
		t.Fatalf("允许目标去重后应有 2 条 FORWARD 放行规则, got %d: %v", len(forwardTargets), forwardTargets)
	}
	if len(inputTargets) != 2 {
		t.Fatalf("目标必须在 INPUT 链上也有放行规则（靶场在宿主上时走这条）, got %d: %v", len(inputTargets), inputTargets)
	}

	joined := ruleStrings(netc.permitRules())
	for _, want := range []string{"10.0.0.5", "8080", "10.0.0.7", "443"} {
		if !strings.Contains(joined, want) {
			t.Errorf("放行规则缺少 %s:\n%s", want, joined)
		}
	}

	// 代理端口必须放行，且**必须落在 INPUT 链**上：代理跑在宿主自己身上，
	// 容器到宿主的流量不到 FORWARD（实测），写在 DOCKER-USER 里永远匹配不到。
	if netc.ProxyPort <= 0 {
		t.Fatal("默认配置必须启用 provider 代理端口")
	}
	foundProxyInput := false
	for _, r := range netc.permitRules() {
		if r.chain == chainInput && strings.Contains(r.String(), itoa(netc.ProxyPort)) {
			foundProxyInput = true
			if !strings.Contains(r.String(), "-d "+p.NetworkGateway) {
				t.Errorf("代理放行规则应限定到网关 %s: %q", p.NetworkGateway, r)
			}
		}
	}
	if !foundProxyInput {
		t.Errorf("缺少 INPUT 链上的代理端口放行（容器连不上 provider 代理）: %v", netc.permitRules())
	}

	// 默认拒绝必须是**两条**：FORWARD 与 INPUT 各一条。
	denies := netc.denyRules()
	if len(denies) != 2 {
		t.Fatalf("默认拒绝规则必须覆盖 FORWARD 与 INPUT 两条链, got %d: %v", len(denies), denies)
	}
	chains := map[string]bool{}
	for _, r := range denies {
		chains[r.chain] = true
		// 不带目的地限定（带了就拦不住白名单之外的目标）。
		if strings.Contains(r.String(), "-d ") {
			t.Errorf("默认拒绝规则不得带目的地限定: %q", r)
		}
		// 必须带源限定：不带会掐死宿主上所有别的容器与所有别的入站流量。
		if !strings.Contains(r.String(), "-s "+p.NetworkSubnet) {
			t.Errorf("默认拒绝规则缺少源限定 -s %s: %q", p.NetworkSubnet, r)
		}
		// INPUT 上的规则还要按入接口限定（宿主上有很多接口）。
		if r.chain == chainInput && !strings.Contains(r.String(), "-i br-test123456") {
			t.Errorf("INPUT 拒绝规则应按网桥接口限定: %q", r)
		}
	}
	if !chains[chainForward] || !chains[chainInput] {
		t.Errorf("默认拒绝必须同时覆盖 FORWARD 与 INPUT: %v", chains)
	}

	// 放行规则必须排在拒绝规则**之前**（iptables 是首个匹配生效）。
	if netc.insertAccepts != true {
		t.Error("放行规则必须插入到拒绝规则之前")
	}
}

// ruleStrings 把规则列表渲染成可搜索的文本（测试断言用）。
func ruleStrings(rs []ruleSpec) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(r.chain)
		b.WriteString(" ")
		b.WriteString(r.String())
		b.WriteString("\n")
	}
	return b.String()
}

// 网络形态：默认**不带** --internal（实测结论，见 DockerConfig.InternalNetwork），
// 但必须确定性分配网段（规则要在建网络之前渲染）并带 run label（孤儿网络可扫）。
//
// 打开 InternalNetwork 时 --internal 必须真的出现在 argv 上：宿主上没有 iptables
// 的部署形态要靠它拿回结构性隔离。
func TestSpecInternalNetworkAndGateway(t *testing.T) {
	cfg := defaultDockerConfig()
	p := mustPlan(t, testSpec(), cfg)
	netc := networkFor(p, cfg)

	argv := netc.createArgv()
	if hasBare(argv, "--internal") {
		t.Fatalf("默认不得带 --internal：它同时掐掉到宿主的路由，白名单目标（经宿主转发的靶场地址）会一起不可达: %s", argvString(argv))
	}
	// 网段必须是**创建之前**就确定好的：DROP 规则要带源限定，源限定要求网段已知。
	if p.NetworkSubnet == "" || p.NetworkGateway == "" {
		t.Fatalf("网段必须由 run 确定（否则规则无法在创建网络前渲染）: subnet=%q gw=%q", p.NetworkSubnet, p.NetworkGateway)
	}
	if !strings.Contains(argvString(argv), p.NetworkSubnet) {
		t.Errorf("createArgv 必须显式指定 --subnet %s: %s", p.NetworkSubnet, argvString(argv))
	}
	// 网络也要带 run label，宿主重启后的孤儿网络才能被扫出来。
	labeled := false
	for i, a := range argv {
		if a == "--label" && i+1 < len(argv) && strings.HasPrefix(argv[i+1], LabelRun+"=") {
			labeled = true
		}
	}
	if !labeled {
		t.Errorf("网络缺少 %s 标签: %s", LabelRun, argvString(argv))
	}

	// 显式打开时必须生效。
	cfg.InternalNetwork = true
	if got := networkFor(p, cfg).createArgv(); !hasBare(got, "--internal") {
		t.Errorf("InternalNetwork=true 时网络必须带 --internal: %s", argvString(got))
	}
}

// ── provider 代理（PLAN.md:52）──

func TestSpecProxyEnvPointsAtHostSideProxy(t *testing.T) {
	cfg := defaultDockerConfig()
	cfg.ProviderAllowHosts = []string{"api.example.com", "*.example.org"}
	spec := testSpec()
	spec.Executor.Env = nil

	p := mustPlan(t, spec, cfg)
	argv := runArgv(p)

	if p.ProxyURL == "" {
		t.Fatal("启用代理时 plan 必须带 ProxyURL")
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(p.ProxyURL, "http://"))
	if err != nil {
		t.Fatalf("ProxyURL 必须是 http://host:port: %v", err)
	}
	if net.ParseIP(host) == nil {
		t.Errorf("代理必须用 IP 直连（容器在 --internal 网络里没有 DNS），got %q", host)
	}
	// 代理必须绑在本 run 的网桥网关上：那是容器唯一能到达的宿主地址。
	if host != p.NetworkGateway {
		t.Errorf("代理应绑在本 run 的网桥网关 %s 上, got %q", p.NetworkGateway, host)
	}
	if port == "0" || port == "" {
		t.Errorf("代理端口必须是真实端口，got %q", port)
	}

	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		found := ""
		for i, a := range argv {
			if (a == "-e" || a == "--env") && i+1 < len(argv) && strings.HasPrefix(argv[i+1], k+"=") {
				found = strings.TrimPrefix(argv[i+1], k+"=")
			}
		}
		if found == "" {
			t.Errorf("argv 缺少 %s（容器不会走白名单代理）: %s", k, argvString(argv))
		} else if !strings.Contains(found, port) {
			t.Errorf("%s 必须指向代理端口 %s, got %q", k, port, found)
		}
	}
	// NO_PROXY 必须存在：否则容器访问目标端点时也会被塞进代理，目标流量被
	// 代理转发（代理只放行 provider 域名 ⇒ 目标不可达）。这是最容易漏的一条。
	noproxy := ""
	for i, a := range argv {
		if (a == "-e" || a == "--env") && i+1 < len(argv) && strings.HasPrefix(argv[i+1], "NO_PROXY=") {
			noproxy = strings.TrimPrefix(argv[i+1], "NO_PROXY=")
		}
	}
	if noproxy == "" {
		t.Fatal("argv 缺少 NO_PROXY：目标流量会被错误地塞进 provider 代理")
	}
	for _, want := range []string{"10.0.0.5"} {
		if !strings.Contains(noproxy, want) {
			t.Errorf("NO_PROXY 必须含目标地址 %s, got %q", want, noproxy)
		}
	}
}

// 代理的域名判定：白名单内放行、白名单外拒绝、非 443 端口拒绝（CONNECT 隧道
// 一旦允许任意端口，白名单就退化成「任意 TCP 转发」）。
func TestProxyDomainAllowlist(t *testing.T) {
	p, err := newAllowlistProxy(proxyConfig{
		AllowHosts: []string{"api.example.com", "*.example.org", "exact.test"},
	})
	if err != nil {
		t.Fatalf("构造代理失败: %v", err)
	}
	cases := []struct {
		host string
		port string
		want bool
	}{
		{"api.example.com", "443", true},
		{"API.EXAMPLE.COM", "443", true},  // 大小写不敏感
		{"api.example.com:443", "", true}, // 带端口的主机名
		{"evil.example.com", "443", false},
		{"api.example.com.evil.test", "443", false}, // 后缀伪装
		{"a.example.org", "443", true},              // 通配符命中
		{"a.b.example.org", "443", true},
		{"example.org", "443", false}, // 通配符不匹配裸域
		{"exact.test", "443", true},
		{"exact.test", "80", false}, // 只允许 443
		{"api.example.com", "8443", false},
		{"10.0.0.5", "443", false}, // 裸 IP 不在白名单
		{"", "443", false},
	}
	for _, c := range cases {
		host := c.host
		if c.port != "" {
			host = net.JoinHostPort(c.host, c.port)
		}
		if got := p.allows(host); got != c.want {
			t.Errorf("allows(%q) = %v, want %v", host, got, c.want)
		}
	}
	// 一条白名单都不配时必须全拒（fail closed），不能退化成「全放行」。
	empty, err := newAllowlistProxy(proxyConfig{})
	if err != nil {
		t.Fatalf("构造空代理失败: %v", err)
	}
	if empty.allows("api.example.com:443") {
		t.Error("空白名单必须全拒（fail closed）")
	}
}

// ── docker exec 渲染 ──

func TestSpecExecArgvAppliesUserWorkdirAndEnv(t *testing.T) {
	cfg := defaultDockerConfig()
	spec := testSpec()
	p := mustPlan(t, spec, cfg)
	h := harness.ExecHandle{ContainerID: "abc123", Network: spec.Network}

	argv := execArgv(h, []string{"pi", "--version"}, harness.ExecOptions{
		Env:     map[string]string{"ROUND": "3"},
		Workdir: "/work/sub",
	}, p, cfg)

	if argv[0] != cfg.Binary {
		t.Errorf("argv[0] 应是 docker 二进制, got %q", argv[0])
	}
	if len(argv) < 3 || argv[1] != "exec" {
		t.Fatalf("必须是 docker exec: %v", argv)
	}
	if argv[len(argv)-2] != "pi" || argv[len(argv)-1] != "--version" {
		t.Errorf("命令必须原样落在尾部: %v", argv)
	}
	if !hasBare(argv, "--user") {
		t.Errorf("docker exec 也必须降权到非 root（否则一次 exec 就把 --user 绕过了）: %v", argv)
	}
	u, _ := valueOf(argv, "--user")
	if u != p.User {
		t.Errorf("exec 的 --user 必须与容器一致: got %q want %q", u, p.User)
	}
	if w, _ := valueOf(argv, "--workdir"); w != "/work/sub" {
		t.Errorf("--workdir 应取 ExecOptions.Workdir, got %q", w)
	}
	envFound := false
	for i, a := range argv {
		if (a == "-e" || a == "--env") && i+1 < len(argv) && argv[i+1] == "ROUND=3" {
			envFound = true
		}
	}
	if !envFound {
		t.Errorf("ExecOptions.Env 必须透传: %v", argv)
	}
}

// 传给 docker exec 的容器标识必须是**句柄里的 ID**，不能是名字：名字可以被
// 另一个 run 抢注，ID 不会。
func TestSpecExecUsesContainerID(t *testing.T) {
	cfg := defaultDockerConfig()
	p := mustPlan(t, testSpec(), cfg)
	argv := execArgv(harness.ExecHandle{ContainerID: "deadbeefcafe"}, []string{"true"}, harness.ExecOptions{}, p, cfg)
	if !hasBare(argv, "deadbeefcafe") {
		t.Errorf("必须用 ContainerID 定位容器: %v", argv)
	}
}

// ── 命名与校验 ──

func TestSpecContainerAndNetworkNamesAreDerivedAndSafe(t *testing.T) {
	spec := testSpec()
	spec.RunID = harness.RunID("2026-09-20T12:00:00Z/web 01") // 含斜杠与空格
	spec.Network = ""
	p := mustPlan(t, spec, defaultDockerConfig())

	for _, n := range []string{p.ContainerName, p.NetworkName} {
		if strings.ContainsAny(n, "/ :") {
			t.Errorf("名字里不得出现 / 空格 冒号（Docker 与 bridge 接口名都不接受）: %q", n)
		}
		if n == "" {
			t.Error("名字不得为空")
		}
	}
	// 不同 run 必须得到不同的网络名，否则两次运行的网络边界会互相污染。
	other := spec
	other.RunID = harness.RunID("run-2")
	if mustPlan(t, other, defaultDockerConfig()).NetworkName == p.NetworkName {
		t.Error("不同 run 的网络名必须不同")
	}
}

func TestSpecRejectsUnsafeInput(t *testing.T) {
	cfg := defaultDockerConfig()

	// 空 RunID：回收靠 label，没有 RunID 就没有回收依据。
	s := testSpec()
	s.RunID = ""
	if _, err := planRun(s, cfg); err == nil {
		t.Error("空 RunID 必须被拒（否则容器无法按 label 回收）")
	}

	// 空 Workdir：没有工作目录就没有可挂载的东西，agent 无处落盘。
	s = testSpec()
	s.Workdir = ""
	if _, err := planRun(s, cfg); err == nil {
		t.Error("空 Workdir 必须被拒")
	}

	// 相对路径：docker -v 的宿主侧必须绝对，相对路径会被 Docker 当成具名卷，
	// 静默变成「一个空卷」而不是本题工作目录。
	s = testSpec()
	s.Workdir = "relative/path"
	if _, err := planRun(s, cfg); err == nil {
		t.Error("相对 Workdir 必须被拒（会被 Docker 当具名卷，静默挂成空卷）")
	}

	// 含冒号：会把 -v 语法拆坏，可能挂到别的宿主路径上。
	s = testSpec()
	s.Workdir = "/tmp/a:b"
	if _, err := planRun(s, cfg); err == nil {
		t.Error("含冒号的 Workdir 必须被拒（-v 语法会被拆坏）")
	}

	// 宿主根目录：等于把整台宿主交出去。
	s = testSpec()
	s.Workdir = "/"
	if _, err := planRun(s, cfg); err == nil {
		t.Error("Workdir 为 / 必须被拒")
	}

	// 非法白名单：无法解析成 IP:port 的条目必须被拒，而不是静默丢弃
	// （静默丢弃 = 用户以为放行了、实际没放行）。
	s = testSpec()
	s.Executor.AllowHosts = []string{"10.0.0.5"} // 缺端口
	if _, err := planRun(s, cfg); err == nil {
		t.Error("缺端口的 AllowHosts 条目必须被拒")
	}
}

// 只读 rootfs 是不可协商的：ExecutorSpec.ReadOnly=false 不生效。
// 唯一的逃生口是运维级的 DockerConfig.WritableRootfs（默认 false）。
func TestSpecReadOnlyIsNonNegotiable(t *testing.T) {
	spec := testSpec()
	spec.Executor.ReadOnly = false // 用户显式要求可写
	p := mustPlan(t, spec, defaultDockerConfig())
	if !p.ReadOnly {
		t.Fatal("ExecutorSpec.ReadOnly=false 不得关掉只读 rootfs（PLAN.md:51）")
	}
	if !hasBare(runArgv(p), "--read-only") {
		t.Fatal("只读 rootfs 必须落到 argv 上")
	}

	// 逃生口只对运维开放。
	cfg := defaultDockerConfig()
	cfg.WritableRootfs = true
	if mustPlan(t, spec, cfg).ReadOnly {
		t.Error("DockerConfig.WritableRootfs=true 应能显式打开可写 rootfs")
	}
}

// 默认值：零值 ExecutorSpec 也必须得到一组合法的隔离参数（不能出现
// --cpus 0 / --memory 0m 这种会被 Docker 拒绝或等于不限的取值）。
func TestSpecDefaultsAreSafe(t *testing.T) {
	cfg := defaultDockerConfig()
	p := mustPlan(t, harness.ExecSpec{
		RunID: "run-1",
		// Target.Code 是**必须**给的：它是容器上 challenge 标签的来源，而空编号
		// 在渲染期就被拒（fail closed，见 planRun）。这里用一个最小合法值，
		// 因为本用例验的是「零值 ExecutorSpec 的缺省是否安全」。
		Target:   harness.Target{Code: "web-01"},
		Executor: harness.ExecutorSpec{Image: "img"},
		Workdir:  "/tmp/w",
	}, cfg)
	argv := runArgv(p)

	for _, f := range []string{"--cpus", "--memory", "--pids-limit"} {
		v, ok := valueOf(argv, f)
		if !ok {
			t.Fatalf("缺省值下也必须渲染 %s: %v", f, argv)
		}
		if v == "0" || v == "0m" || v == "" {
			t.Errorf("%s 的缺省值不得是「不限」: %q", f, v)
		}
	}
	if p.User == "" || p.User == "0:0" {
		t.Errorf("缺省用户必须是非 root: %q", p.User)
	}
	if p.WorkdirCtr == "" {
		t.Error("容器内工作目录缺省值不得为空")
	}
	if len(p.Command) == 0 {
		t.Error("缺省命令不得为空（否则容器起不来，Prepare 后立刻退出）")
	}
}

// 默认 env 必须把 HOME 指到可写区（工作目录）内。pi 从 $HOME/.pi/agent/*
// 发现扩展/角色/技能；HOME 指向只读 rootfs 会让 pi 静默失去扩展加载能力
// （M0 实测的坑），而 HOME 指向宿主路径则根本不存在。
func TestSpecHomeIsInsideWritableWorkdir(t *testing.T) {
	spec := testSpec()
	spec.Executor.Env = nil
	p := mustPlan(t, spec, defaultDockerConfig())

	home := p.Env["HOME"]
	if home == "" {
		t.Fatal("必须默认注入 HOME")
	}
	if !strings.HasPrefix(home, p.WorkdirCtr) {
		t.Errorf("HOME 必须落在容器内工作目录 %q 之下（只读 rootfs 下 pi 无处写会话），got %q", p.WorkdirCtr, home)
	}
	if p.Env["TMPDIR"] == "" {
		t.Error("必须默认注入 TMPDIR=/tmp，否则工具会往只读 rootfs 的 /var/tmp 写")
	}
}

// ExecOptions 的零值 Timeout 必须落到配置的默认值，不能是「不限」。
func TestExecTimeoutDefaults(t *testing.T) {
	cfg := defaultDockerConfig()
	if cfg.CommandTimeout <= 0 {
		t.Fatal("defaultDockerConfig 必须给出正的 CommandTimeout")
	}
	if got := effectiveTimeout(harness.ExecOptions{}, cfg); got != cfg.CommandTimeout {
		t.Errorf("零值 Timeout 应回落到 %v, got %v", cfg.CommandTimeout, got)
	}
	if got := effectiveTimeout(harness.ExecOptions{Timeout: time.Second}, cfg); got != time.Second {
		t.Errorf("显式 Timeout 应被尊重, got %v", got)
	}
}

// Reclaim 必须按 label 精确回收（PLAN.md:54），而不是按名字前缀猜。
func TestReclaimUsesRunLabel(t *testing.T) {
	argv := reclaimContainerArgv(DockerConfig{}, harness.RunID("run-1"))
	joined := argvString(argv)
	if !strings.Contains(joined, LabelRun+"=run-1") {
		t.Errorf("容器回收必须按 label: %v", argv)
	}
	nargv := reclaimNetworkArgv(DockerConfig{}, harness.RunID("run-1"))
	if !strings.Contains(argvString(nargv), LabelRun+"=run-1") {
		t.Errorf("网络回收必须按 label: %v", nargv)
	}
	// 只删自己那一批：回收的**定位**必须完全靠 run label，不得出现任何
	// 广谱定位（`--all` 在 network ls 上会列出宿主上全部网络；`prune` 会删掉
	// 别的 run 的对象）。
	all := append(append([]string{}, argv...), nargv...)
	allJoined := argvString(all)
	if !strings.Contains(allJoined, "--filter label="+LabelRun+"=run-1") {
		t.Fatalf("回收必须按 run label 定位: %v", all)
	}
	for _, bad := range []string{"prune", "system df"} {
		if strings.Contains(allJoined, bad) {
			t.Errorf("回收不得用广谱操作 %q: %v", bad, all)
		}
	}
	// 网络回收不得带 --all（那会列出宿主上全部网络）。
	if strings.Contains(argvString(nargv), "--all") {
		t.Errorf("网络回收不得带 --all: %v", nargv)
	}
	// 容器回收**必须**含已停止的容器：崩溃恢复时容器常常已经是 Exited，
	// 只列运行中的会漏掉它，留下一个永不回收的容器。
	if !strings.Contains(argvString(argv), "--all") {
		t.Errorf("容器回收必须含已停止的容器: %v", argv)
	}
}

// ReclaimStale 的扫描分两段，两段的 argv 形态都是**冻结的**。
//
// ⚠️ 这条用例上一版把 `{{json .Labels}}` 当契约钉死了——而那个模板在 `docker ps`
// 与 `docker network ls` 上打印的是 **JSON 字符串**（逗号拼接的 `k=v`）而不是 JSON
// 对象，解析**每一行都失败**，于是回收永远什么都不删。**测试把缺陷钉成了契约，
// 是这次事故里最值得记住的一环**：一条关于外部命令输出形态的断言，如果不来自对
// 那个命令的实测，它就在保护 bug。
//
// 现在钉的是「哪一段用哪种形态」：
//   - 第一段只取 ID（`--quiet`，不带任何模板）——ID 是 docker 自己的十六进制串，
//     没有 `key=value` 那种歧义，也就不需要模板。
//   - 第二段 `docker inspect` 取结构（不带 `--format`）——它输出的是 docker 的
//     规范 JSON 数组，是唯一一个**结构**源。
func TestReclaimStaleScanArgvIsFrozen(t *testing.T) {
	c := &Docker{cfg: defaultDockerConfig()}

	for _, kind := range []string{"container", "network"} {
		joined := argvString(c.staleListArgv(kind))

		// 过滤只给键、不给值：owner 不匹配的资源也要被**看见**（它们进 Pending，
		// 而看不见就无法报告）。
		if !strings.Contains(joined, "label="+LabelRun) {
			t.Errorf("%s 的列表必须按 label 过滤: %s", kind, joined)
		}
		if !strings.Contains(joined, "--quiet") {
			t.Errorf("%s 的列表必须只取 ID（--quiet）: %s", kind, joined)
		}
		// 这一条是那次事故的直接防线：任何回到模板的改动都必须在这里变红，
		// 因为 `{{json .Labels}}` 打印的是字符串，`{{.Labels}}` 是逗号拼接的
		// `k=v`，两者都不是 JSON 对象。
		if strings.Contains(joined, "{{") {
			t.Errorf("%s 的列表不得带模板（模板输出的是表格的重新渲染，不是结构）: %s", kind, joined)
		}
	}

	// 容器扫描必须含已停止的容器：崩溃恢复时容器常常已经是 Exited，
	// 只列运行中的会漏掉它，留下一个永不回收的容器。
	if joined := argvString(c.staleListArgv("container")); !strings.Contains(joined, "--all") {
		t.Errorf("容器扫描必须含已停止的容器: %s", joined)
	}

	// 第二段：inspect 的 ID 必须逐个出现，且**不得带 --format**（带了就不再是
	// 规范结构，退回「某个人对输出的重新渲染」）。
	iargv := c.staleInspectArgv([]string{"aaa111", "bbb222"})
	joined := argvString(iargv)
	if !strings.Contains(joined, "inspect") {
		t.Errorf("第二段必须是 inspect: %s", joined)
	}
	if strings.Contains(joined, "--format") {
		t.Errorf("inspect 不得带 --format（要的是规范 JSON 数组）: %s", joined)
	}
	for _, id := range []string{"aaa111", "bbb222"} {
		if !strings.Contains(joined, id) {
			t.Errorf("inspect 必须带上每个 ID（%s）: %s", id, joined)
		}
	}
}

// iptables 规则必须带 run 注释：否则宿主重启后残留规则无法被归属与清理。
//
// 注释同时是**两条链**上的唯一归属依据——uninstallRulesByRun 扫的是
// `iptables -S <chain>` 输出里的注释，漏标一条链，那条链上的规则就永远删不掉。
func TestIptablesRulesAreCommentTagged(t *testing.T) {
	cfg := defaultDockerConfig()
	p := mustPlan(t, testSpec(), cfg)
	netc := networkFor(p, cfg)
	netc.Bridge = "br-test123456"

	all := append(append([]ruleSpec{}, netc.permitRules()...), netc.denyRules()...)
	if len(all) == 0 {
		t.Fatal("默认配置必须产生规则")
	}
	// 注释的期望值：owner 指纹 + runID。**指纹必须在里面**——netfilter 是宿主
	// 全局的，只带 runID 的注释让「按注释删规则」无法区分「我的 run-1」与
	// 「别人的 run-1」，而正是这条路径会拆掉对方正在用的默认拒绝规则。
	wantComment := commentFor(p.Owner, p.RunID)
	seenChain := map[string]bool{}
	for _, r := range all {
		seenChain[r.chain] = true
		if !strings.Contains(r.String(), wantComment) {
			t.Errorf("规则缺少 run 注释 %q: %q", wantComment, r)
		}
		// 注释必须挂在 -m comment --comment 上，否则 iptables 不认。
		if !strings.Contains(r.String(), "-m comment --comment") {
			t.Errorf("注释没有通过 -m comment 传递: %q", r)
		}
	}
	if !seenChain[chainForward] || !seenChain[chainInput] {
		t.Errorf("规则必须同时覆盖 FORWARD 与 INPUT 两条链（漏一条的失败是静默的）: %v", seenChain)
	}
	if !strings.HasPrefix(commentFor(p.Owner, "run-1"), "red-harness-run-") {
		t.Errorf("注释前缀必须是可扫描的常量, got %q", commentFor(p.Owner, "run-1"))
	}
	// 不同 owner 的注释必须不同：否则「按注释删规则」会误伤。
	other, err := harness.ResolveOwner("other-box", 0)
	if err != nil {
		t.Fatalf("ResolveOwner: %v", err)
	}
	if commentFor(other, p.RunID) == wantComment {
		t.Errorf("两个不同 owner 的 run-1 得到同一条注释 %q（按注释删规则会误伤）", wantComment)
	}

	// 规则参数里不得出现「本该是独立参数、却被拼进上一个值里」的空格。
	//
	// 这条断言来自一个真实缺陷：回收路径曾经把整条规则拼成一个字符串传给
	// iptables，于是 `-s` 的值被解析成 `10.211.x.0/24 -m comment ... -j DROP`，
	// 删除报 "invalid mask" ——规则永久残留，而错误看起来像「网段写错了」。
	// 每个参数独立成参是 iptables 的硬要求，所以在**规则渲染层**就钉住它。
	for _, r := range all {
		for _, a := range r.args {
			if strings.Contains(a, " ") {
				t.Errorf("规则参数含空格（iptables 会把它当成一个值，删除/安装都会失败）: %q in %q", a, r)
			}
		}
	}
}

// 关掉 iptables 管理时不得产生任何规则（给无 root/无 iptables 的环境留路）。
func TestIptablesCanBeDisabled(t *testing.T) {
	cfg := defaultDockerConfig()
	cfg.ManageIptables = false
	p := mustPlan(t, testSpec(), cfg)
	netc := networkFor(p, cfg)
	if len(netc.permitRules()) != 0 || len(netc.denyRules()) != 0 || netc.denyRule() != "" {
		t.Errorf("关闭后不得有规则: %v / %v", netc.permitRules(), netc.denyRules())
	}
	// 但网络仍然必须存在且带 label：关掉 iptables 时默认拒绝只剩
	// 「网络可达即可达」，所以网络层不能跟着一起消失。
	if !hasBare(netc.createArgv(), "--subnet") {
		t.Error("即使不管理 iptables，网络也必须显式指定网段")
	}
}

// 代理未启用时不得注入任何 *_PROXY：一个指向不存在代理的 env 会让全部
// 出站流量静默失败，表现为「agent 什么工具都用不了」。
func TestProxyEnvAbsentWhenDisabled(t *testing.T) {
	cfg := defaultDockerConfig()
	cfg.ProviderProxy = false
	spec := testSpec()
	spec.Executor.Env = nil
	p := mustPlan(t, spec, cfg)
	argv := runArgv(p)
	for _, a := range argv {
		up := strings.ToUpper(a)
		if strings.HasPrefix(up, "HTTP_PROXY=") || strings.HasPrefix(up, "HTTPS_PROXY=") {
			t.Errorf("未启用代理时不得注入 %q", a)
		}
	}
	if p.ProxyURL != "" {
		t.Errorf("未启用代理时 ProxyURL 必须为空, got %q", p.ProxyURL)
	}
}

// docker CLI 调用必须带 context（否则宿主网络卡住时 Prepare 会永久挂住）。
func TestDockerCLIUsesContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := defaultDockerConfig()
	// owner 必须显式给：Reclaim 在碰任何东西之前先解析 owner（fail closed），
	// 不给的话它会在那个闸门上失败——同样是 error，但本用例的本意是「已取消的
	// ctx 让子进程立刻失败」，会在一个不相干的理由上绿掉。
	cfg.Owner = "ctx-test-host/0"
	c := &Docker{cfg: cfg}
	if err := c.Available(ctx); err == nil {
		t.Error("已取消的 ctx 必须让 Available 立刻失败")
	}
	if _, err := c.Prepare(ctx, testSpec()); err == nil {
		t.Error("已取消的 ctx 必须让 Prepare 立刻失败")
	}
	if err := c.Reclaim(ctx, "run-1"); err == nil {
		t.Error("已取消的 ctx 必须让 Reclaim 立刻失败")
	}
}

// 失败信息不得泄漏宿主细节到 KindConfig 的 Msg 之外，且必须是结构化错误。
func TestPlanErrorsAreTyped(t *testing.T) {
	s := testSpec()
	s.RunID = ""
	_, err := planRun(s, defaultDockerConfig())
	if err == nil {
		t.Fatal("应报错")
	}
	if !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("配置类错误必须是 KindConfig, got %v", err)
	}
}

func TestPlanRunIsDeterministic(t *testing.T) {
	spec := testSpec()
	spec.Executor.AllowHosts = []string{"10.0.0.7:443", "10.0.0.5:8080"}
	a := argvString(runArgv(mustPlan(t, spec, defaultDockerConfig())))
	b := argvString(runArgv(mustPlan(t, spec, defaultDockerConfig())))
	if a != b {
		t.Errorf("同一份 spec 必须渲染出同一个 argv（map 迭代顺序不得外泄）:\n%s\n%s", a, b)
	}
}

func TestCommentForIsIptablesSafe(t *testing.T) {
	// iptables 注释最长 256 字符且不能含引号/换行；runID 与 owner 都可能很长。
	//
	// owner 这一维是新增的：owner 的值来自 hostname（可以很长、可以含任意字符），
	// 而它在注释里只以 8 位指纹出现——同一条纪律见 harness.OwnerID.Fingerprint。
	longOwner, err := harness.ResolveOwner(strings.Repeat("host-", 200), 12345)
	if err != nil {
		t.Fatalf("ResolveOwner: %v", err)
	}
	long := harness.RunID(strings.Repeat("x", 400))
	for _, o := range []harness.OwnerID{longOwner, harness.OwnerUnknown} {
		c := commentFor(o, long)
		if len(c) > 256 {
			t.Errorf("注释超长会被 iptables 拒绝: %d (%q)", len(c), c)
		}
		if strings.ContainsAny(c, "\"' \n\t") {
			t.Errorf("注释不得含引号/空白/换行: %q", c)
		}
	}
	// owner 未知时也不得退化成 `red-harness-run--<runID>` 这种看不出少了什么的串：
	// 归属不明必须写在注释里，读规则的人才知道这条规则不该被自己删。
	if c := commentFor(harness.OwnerUnknown, "run-1"); !strings.Contains(c, unknownOwnerCommentToken) {
		t.Errorf("owner 未知时注释必须显式标明归属不明, got %q", c)
	}
}
