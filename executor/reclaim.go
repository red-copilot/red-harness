package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// 本文件负责**回收**：容器、网络、iptables 规则、宿主侧代理，四样东西都要能被
// 精确地按 run 找回来并清掉。
//
// # 为什么回收必须按 label 而不是按名字
//
// 名字是可以被复用与抢注的：两个 run 的 RunID 净化后可能撞名，用户也可能手动
// 起一个同名容器。按名字删的后果是「删掉了别人的容器」——而这类错误现场完全
// 无法归因（被删的那个 run 只会表现为「容器莫名其妙不见了」）。
// label 是 run 身份的直接投影，删错的可能性为零。
//
// # 为什么回收必须幂等
//
// 三条路径都会调它：正常结束、用户取消、宿主重启恢复。任何一条路径上重复调用
// （比如引擎在 defer 里调一次、ReclaimStale 又扫到一次）都必须无害。所以这里
// 全部用「找不到就跳过」的语义，而不是「找不到就报错」。
//
// # 宿主重启恢复为什么必须有
//
// 重启后内存里的 run 列表没了，只有磁盘上的 run 目录还在。重启前起的容器里跑的
// 是**进攻性工具**——没有扫描把它们收掉，它们会继续占着网段与内存，而没有任何
// 人在看它们的输出。ReclaimStale 就是为这个场景写的。

// reclaimContainerArgv 渲染「按 run label 找出本 run 的全部容器」的 argv。
//
// 用 `--all`（含已停止的）而不是默认的只列运行中：崩溃恢复时容器往往已经是
// Exited 状态，只列运行中的会把它漏掉，留下一个永远不会被回收的容器。
func reclaimContainerArgv(cfg DockerConfig, runID harness.RunID) []string {
	return []string{
		binaryOr(cfg), "ps", "--all", "--quiet",
		"--filter", "label=" + LabelRun + "=" + string(runID),
	}
}

// reclaimNetworkArgv 渲染「按 run label 找出本 run 的网络」的 argv。
func reclaimNetworkArgv(cfg DockerConfig, runID harness.RunID) []string {
	return []string{
		binaryOr(cfg), "network", "ls", "--quiet",
		"--filter", "label=" + LabelRun + "=" + string(runID),
	}
}

func binaryOr(cfg DockerConfig) string {
	if cfg.Binary != "" {
		return cfg.Binary
	}
	return "docker"
}

// ── Docker 对象回收 ──

// removeContainersByRun 按 label 找出并删除本 run 的全部容器。
func (d *Docker) removeContainersByRun(ctx context.Context, runID harness.RunID) error {
	out, err := d.run(ctx, nil, reclaimContainerArgv(d.cfg, runID)[1:]...)
	if err != nil {
		return harness.Ef(harness.KindExecutor, "executor.reclaim", "列出容器失败", err)
	}
	ids := fields(out)
	if len(ids) == 0 {
		return nil // 已经回收过了：幂等路径。
	}
	// --force：崩溃恢复时容器可能仍在跑，不加 --force 会删不掉；
	// --volumes：容器自己创建的匿名卷也一并清掉（否则宿主上攒孤儿卷）。
	args := append([]string{"rm", "--force", "--volumes"}, ids...)
	if out, err := d.run(ctx, nil, args...); err != nil {
		return harness.Ef(harness.KindExecutor, "executor.reclaim", "删除容器失败", wrapOut(err, out))
	}
	return nil
}

// removeNetwork 按名字删除网络（名字由 run 决定，且上面已按 label 校验过归属）。
func (d *Docker) removeNetwork(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	if out, err := d.run(ctx, nil, "network", "rm", name); err != nil {
		// 网络不存在 = 已经回收过了，不是错误。
		if strings.Contains(strings.ToLower(err.Error()+out), "no such network") {
			return nil
		}
		return harness.Ef(harness.KindExecutor, "executor.reclaim", "删除网络失败", wrapOut(err, out))
	}
	return nil
}

// removeNetworksByRun 按 label 删除本 run 的网络（宿主重启恢复路径用）。
func (d *Docker) removeNetworksByRun(ctx context.Context, runID harness.RunID) error {
	out, err := d.run(ctx, nil, reclaimNetworkArgv(d.cfg, runID)[1:]...)
	if err != nil {
		return harness.Ef(harness.KindExecutor, "executor.reclaim", "列出网络失败", err)
	}
	var errs []error
	for _, id := range fields(out) {
		if out, err := d.run(ctx, nil, "network", "rm", id); err != nil {
			if strings.Contains(strings.ToLower(err.Error()+out), "no such network") {
				continue
			}
			errs = append(errs, harness.Ef(harness.KindExecutor, "executor.reclaim", "删除网络失败", wrapOut(err, out)))
		}
	}
	return joinErrors(errs)
}

// ── iptables ──
//
// 规则分两条链落地（chainForward / chainInput，见 network.go 文件头的实测依据）：
// 目标白名单走 FORWARD，宿主代理端口的放行与「到宿主其他端口」的拒绝走 INPUT。
// 漏掉 INPUT 那一条的失败是**静默的**：容器能扫宿主的所有监听端口，而
// DOCKER-USER 里看起来「规则都写了」。

// installNetworkRules 装本 run 的网络规则（FORWARD 与 INPUT 两条链）。
//
// 顺序是契约：**放行在前、拒绝在后**。iptables 是首个匹配生效，拒绝规则插在
// 放行之前的话，白名单目标也会被一起 DROP——表现为「靶场全部不可达」，
// 而规则看起来「明明写了」。
func (d *Docker) installNetworkRules(ctx context.Context, n network) error {
	if !n.ManageIptables {
		return nil
	}
	for _, chain := range []string{chainForward, chainInput} {
		if err := d.ensureChain(ctx, chain); err != nil {
			return err
		}
	}
	permits := n.permitRules()
	denies := n.denyRules()
	for _, chain := range []string{chainForward, chainInput} {
		var chainPermits []ruleSpec
		for _, r := range permits {
			if r.chain == chain {
				chainPermits = append(chainPermits, r)
			}
		}
		// 逆序插到链首 ⇒ 最终顺序与 chainPermits 一致。
		for i := len(chainPermits) - 1; i >= 0; i-- {
			if err := d.iptables(ctx, append([]string{"-I", chain, "1"}, chainPermits[i].args...)...); err != nil {
				// 装到一半失败：把已装的撤掉，别留半套规则。
				_ = d.uninstallNetworkRules(ctx, n)
				return err
			}
		}
		for _, r := range denies {
			if r.chain != chain {
				continue
			}
			pos := fmt.Sprintf("%d", len(chainPermits)+1)
			if err := d.iptables(ctx, append([]string{"-I", chain, pos}, r.args...)...); err != nil {
				_ = d.uninstallNetworkRules(ctx, n)
				return err
			}
		}
	}
	return nil
}

// uninstallNetworkRules 撤掉本 run 的规则（幂等）。
//
// INPUT 规则可能带 `-i <网桥>`（网桥名只有在网络创建后才拿得到），所以先探测
// 一次再算规则；探测不到时退回「不带 -i」的形态再删一遍。
func (d *Docker) uninstallNetworkRules(ctx context.Context, n network) error {
	if !n.ManageIptables {
		return nil
	}
	bridges := []string{n.Bridge}
	if n.Bridge == "" {
		bridges = []string{d.detectBridge(ctx, n.Name), ""}
	}
	seen := map[string]bool{}
	for _, br := range bridges {
		nn := n
		nn.Bridge = br
		rules := append(nn.permitRules(), nn.denyRules()...)
		for _, r := range rules {
			key := r.chain + " " + r.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			// 重复删直到不存在：规则可能因为上一次回收失败而残留了多份。
			//
			// 注意参数里**不含链名**：链名单独作为 -D 的第一个参数传（下面），
			// 不能拼进参数串——拼进去 iptables 会把它当成一个 29 字符以上的
			// 链名，报 "chain name ... too long"，删除静默失败、规则永久残留。
			for i := 0; i < 8; i++ {
				if err := d.iptables(ctx, append([]string{"-D", r.chain}, r.args...)...); err != nil {
					break
				}
			}
		}
	}
	return nil
}

// uninstallRulesByRun 按**注释**归属并删除规则（宿主重启恢复路径）。
//
// 为什么靠注释而不是靠网段：重启后网段是从磁盘上的 run 目录重算的，理论上能
// 算出来，但「算出来的网段」与「当时实际用的网段」一旦不一致（比如改了
// NetworkSupernet），规则就永远删不掉。注释是写在规则里的原文，不依赖任何重算。
func (d *Docker) uninstallRulesByRun(ctx context.Context, runID harness.RunID) error {
	if !d.cfg.ManageIptables {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	comment := commentFor(runID)
	var errs []error
	for _, chain := range []string{chainForward, chainInput} {
		out, err := d.iptablesOut(ctx, "-S", chain)
		if err != nil {
			// 链不存在 = 没有任何规则，幂等。
			if strings.Contains(strings.ToLower(err.Error()), "no chain") {
				continue
			}
			errs = append(errs, err)
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if !strings.Contains(line, comment) {
				continue
			}
			// `iptables -S` 输出形如 `-A INPUT -s ... -j DROP -m comment --comment X`。
			// 拆成 `-D <链>` + 参数列表：**每一个参数都必须单独成参**。
			// 把参数串拼成一整条传给 iptables 的后果是它把 `-s` 的值解析成
			// `10.211.x.0/24 -m comment ... -j DROP`，报 invalid mask ——
			// 删除失败、规则永久残留，而错误信息看起来像「网段写错了」。
			rest := strings.TrimPrefix(line, "-A ")
			if rest == line {
				continue // 不是可删除的规则行
			}
			chainName, spec, ok := strings.Cut(rest, " ")
			if !ok {
				continue
			}
			if err := d.iptables(ctx, append([]string{"-D", chainName}, strings.Fields(spec)...)...); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return joinErrors(errs)
}

// ensureChain 保证链存在。
//
// DOCKER-USER 由 Docker 创建，INPUT 由内核自带；两者理论上都在。这里仍然显式
// 检查一次，是为了把「iptables 根本不可用」（非 root、容器里跑、缺模块）变成
// 一条**可操作的**错误，而不是一堆 `no chain/target/match by that name`。
func (d *Docker) ensureChain(ctx context.Context, chain string) error {
	if _, err := d.iptablesOut(ctx, "-S", chain); err == nil {
		return nil
	}
	_, _ = d.iptablesOut(ctx, "-N", chain)
	if _, err := d.iptablesOut(ctx, "-S", chain); err != nil {
		return harness.Ef(harness.KindExecutor, "executor.network",
			fmt.Sprintf("iptables 链 %s 不可用（需要 root 或 CAP_NET_ADMIN；可设 ManageIptables=false 跳过）", chain), err)
	}
	return nil
}

// detectBridge 探测某个网络的宿主网桥接口名。
//
// Docker 的命名约定是 br-<网络 ID 前 12 位>，但网络 ID 只有在网络创建后才拿得到，
// 所以 INPUT 规则里的 -i 只能在创建之后补。探测失败时返回空串：规则仍然带
// -s 源限定（不会误伤别的容器），只是少了一层接口限定。
func (d *Docker) detectBridge(ctx context.Context, networkName string) string {
	if networkName == "" {
		return ""
	}
	out, err := d.run(ctx, nil, "network", "inspect", networkName, "--format", "{{.Id}}")
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(out)
	if len(id) < 12 {
		return ""
	}
	return "br-" + id[:12]
}

// iptables 执行一条 iptables 命令，失败时返回结构化错误。
//
// `-w` 是必须的：iptables 的 xtables 锁在并发时会让命令直接失败（"another
// app is currently holding the xtables lock"）。宿主上可能有别的容器管理进程
// 同时在改规则，没有 -w 就会随机失败。
func (d *Docker) iptables(ctx context.Context, args ...string) error {
	_, err := d.iptablesOut(ctx, args...)
	return err
}

func (d *Docker) iptablesOut(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-w", "5"}, args...)
	out, err := d.runBinary(ctx, "iptables", full...)
	if err != nil {
		return out, harness.Ef(harness.KindExecutor, "executor.iptables",
			fmt.Sprintf("iptables %s 失败", strings.Join(full, " ")), err)
	}
	return out, nil
}

// runBinary 执行任意宿主机二进制（iptables），与 run 的区别只是可换二进制。
func (d *Docker) runBinary(ctx context.Context, bin string, args ...string) (string, error) {
	return runCmd(ctx, bin, args...)
}

// ── 小工具 ──

func fields(s string) []string {
	return strings.Fields(strings.TrimSpace(s))
}

func joinErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	var parts []string
	for _, e := range errs {
		if e != nil {
			parts = append(parts, e.Error())
		}
	}
	return fmt.Errorf("%s", strings.Join(parts, "; "))
}
