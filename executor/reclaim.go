package executor

import (
	"context"
	"encoding/json"
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
	return append([]string{
		binaryOr(cfg), "ps", "--all", "--quiet",
		"--filter", "label=" + LabelRun + "=" + string(runID),
	}, ownerFilter(cfg)...)
}

// reclaimNetworkArgv 渲染「按 run label 找出本 run 的网络」的 argv。
func reclaimNetworkArgv(cfg DockerConfig, runID harness.RunID) []string {
	return append([]string{
		binaryOr(cfg), "network", "ls", "--quiet",
		"--filter", "label=" + LabelRun + "=" + string(runID),
	}, ownerFilter(cfg)...)
}

// ownerFilter 返回「只要我自己那批」的查询参数。
//
// **为什么是第二道闸而不是可选的收紧**：run 标签在宿主上**不唯一**——两个用不同
// `--store` 的进程各自开一个 run-1 是合法的。只按 run 定位的失效模式是一次已核实
// 的真实事故：两边各自拿到锁、各自把对方认成孤儿，然后 `docker rm --force` 掉对方
// **正在用**的容器、网络与 iptables 规则。
//
// ⚠️ owner 为空时**仍然渲染这一条**，不省略：`label=<key>=` 在 docker 里是
// 「带这个标签且值为空」，而本执行器写下的 owner 永远非空，所以它匹配不到任何
// 东西（fail closed）。省略它才会退化成「谁的都删」。
func ownerFilter(cfg DockerConfig) []string {
	return []string{"--filter", "label=" + LabelOwner + "=" + cfg.Owner}
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
func (d *Docker) uninstallRulesByRun(ctx context.Context, owner harness.OwnerID, runID harness.RunID) error {
	if !d.cfg.ManageIptables {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// 注释里带 owner（见 commentFor）：netfilter 是宿主全局的，「按注释删规则」
	// 必须能区分「我的 run-1」与「别人的 run-1」。
	comment := commentFor(owner, runID)
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
//
// 用的是同一份被钉死的环境（见 subprocessEnv）：iptables 对多出来的 DOCKER_*
// 是惰性的，但**不继承环境**这条性质对所有子进程都成立，不按二进制分叉。
func (d *Docker) runBinary(ctx context.Context, bin string, args ...string) (string, error) {
	return runCmd(ctx, bin, d.subprocessEnv(), args...)
}

// ── 回收判定（纯函数） ──
//
// 这一节是**纯函数**：不碰 daemon、不读全局状态。理由不是洁癖——回收是本包里
// 唯一会**删除别人资源**的代码路径，它的判据必须能逐条离线钉住。上一版的判据
// （`runID ∉ live`）只写在 ReclaimStale 的循环里，于是「哪些情况不该删」既不可读、
// 也无法单独测试，而漏掉的那几条恰恰是删错对象的成因。
//
// 分层：parseInspect 只负责「把某种输出形态翻译成对象」，classifyScanned 只负责
// 「标签表 → 归属」，judgeStale 只负责「该不该删」，执行删除留在 ReclaimStale。
// 分开之后，「解析歧义」与「归属不匹配」这两种完全不同的失败不会被混成同一件事。
//
// ⚠️ 归类（classifyScanned）与形态（parseInspect）**必须**是两层：它们曾经是一层，
// 于是「归类逻辑对不对」从未被真正执行过——夹具编的是我们**以为** docker 会打印的
// 形态，而 docker 打印的是另一种，每一行都解析失败，全部归到 unparsable。测试是绿的。

// scanObject 是一次 label 扫描读到的**单个**对象。
type scanObject struct {
	// Kind 是资源种类（"container" / "network"），由扫描命令决定，解析失败时
	// 仍然有效——「有个网络解析不出来」比「有个东西解析不出来」可操作得多。
	Kind string
	// Parsed 为假表示这一行**无法归属**（扫描输出里读不出 run 标签）。此时
	// RunID / Owner 都无意义，判定函数必须把它归到 Pending(unparsable)。
	//
	// 为什么解析歧义要「只报告」而不是当作空：当作空 = 扫不到东西，而扫不到的
	// 表现是「ReclaimStale 报无事可做，宿主上却躺着孤儿容器」——里面的进攻性工具
	// 还在跑，没有任何人在看它的输出。
	Parsed bool
	RunID  harness.RunID
	Owner  harness.OwnerID
}

// inspectObject 是 `docker inspect` 数组元素里我们关心的那部分。
//
// 两个标签位是**实测**出来的（本机 Docker 29.8），不是照着文档猜的：
//   - 容器把用户标签放在 `.Config.Labels`
//   - 网络把用户标签放在顶层 `.Labels`
//
// 只声明需要的字段，不做全量映射：`docker inspect` 的输出有几十个字段，全量映射
// 会在 Docker 升级改掉某个字段类型时整体解析失败——而失败的方向是「解析不出来
// ⇒ 不删」，那会把一次无害的升级变成回收**永久静默失效**。
type inspectObject struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Labels map[string]string `json:"Labels"`
}

// labelsOf 按对象类型取标签表。容器与网络的标签位不同是 Docker 的输出形态，
// 不是我们的选择（见 inspectObject 上那段实测记录）。
func (o inspectObject) labelsOf(kind string) map[string]string {
	if kind == "network" {
		return o.Labels
	}
	return o.Config.Labels
}

// parseInspect 把一次 `docker inspect <ids...>` 的输出解析成对象列表。
//
// **为什么不是 `ls --format '{{json .Labels}}'`**——这是本文件最贵的一条教训：
// 那个模板函数在 `docker ps --all` 与 `docker network ls` 上打印的是一个 **JSON
// 字符串**（`"k=v,k=v"`，逗号拼接），**不是** JSON 对象。照 JSON 对象去
// `json.Unmarshal` 会**每一行都失败**，于是每个对象都变成 Parsed=false
// （unparsable），而 `unparsable ⇒ 只报告` 这条安全性质让整件事**完全静默**：
// 回收永远什么都不删，宿主上的孤儿容器永远躺着。
//
// 注意「不删」本身是安全方向，但**「因为解析器坏了所以不删」不是**——它把 N0.2
// 的出口门「不属于该 owner 的资源不可删除」变成了**空转成立**（什么都不删，所以
// 「不删别人的」平凡为真）。这就是为什么这一层的测试夹具不能手写：手写的夹具编出
// 来的正是我们**以为**docker 会打印的东西。
//
// inspect 没有这个歧义：它的输出是 docker 的**规范结构**（数组 + 类型化字段），
// 我们拿到的是真 JSON 对象，而不是某个人对表格输出的重新渲染。
//
// ⚠️ 整体解析失败**返回 error 而不是一堆 Parsed=false**：调用方（scanLabeled）
// 对「docker 真的坏了」与「解析不出某个对象」的处置不同（见那里的退出码处理）。
// 把前者降级成后者，就等于给一个坏掉的 docker 编一份「都很可疑」的报告。
//
// ⚠️ owner 的值**不做任何规范化**（不 trim、不大小写折叠）：规范化只会把两个
// 不同的 owner 合并成一个，而合并的方向恰好是「外来的看起来像我的」。
func parseInspect(kind, out string) ([]scanObject, error) {
	var raw []inspectObject
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &raw); err != nil {
		return nil, fmt.Errorf("解析 docker inspect 输出：%w", err)
	}
	labels := make([]map[string]string, 0, len(raw))
	for _, o := range raw {
		labels = append(labels, o.labelsOf(kind))
	}
	return classifyScanned(kind, labels), nil
}

// classifyScanned 把「逐个对象的标签表」判成对象列表。它是扫描方式无关的纯函数，
// 所以能被表驱动地测，而不必造 Docker——parseInspect 只负责把某种输出形态翻译成
// 这一层的输入。
//
// ⚠️ 解析不出来（标签表里没有 run）的**绝不能被丢掉**（见 scanObject.Parsed）：
// 它变成一条 Parsed=false 的对象，最终进 Pending(unparsable)。`nil` 标签表与
// 「标签表里 run 为空」在这里是同一档，都归 unparsable。
func classifyScanned(kind string, labels []map[string]string) []scanObject {
	objs := make([]scanObject, 0, len(labels))
	for _, l := range labels {
		runID := strings.TrimSpace(l[LabelRun])
		if runID == "" {
			// 扫描命令带了 `--filter label=red-harness.run`（ls 那一步），所以
			// 「有标签」是前置条件；这里为空只可能是标签值真是空串，或者是
			// inspect 的字段位变了。两种情况都无法归属，归到 unparsable，
			// 而不是「一个没有 run 的资源」。
			objs = append(objs, scanObject{Kind: kind})
			continue
		}
		objs = append(objs, scanObject{
			Kind:   kind,
			Parsed: true,
			RunID:  harness.RunID(runID),
			Owner:  harness.OwnerID(l[LabelOwner]),
		})
	}
	return objs
}

// judgeStale 把扫描到的对象按回收判据分类：该删的进 Reclaimed，不该删的进 Pending。
//
// 判据逐条照 harness.ReclaimReport 的注释（那里是权威）：
//
//	owner 匹配 且 runID ∉ live ⇒ 删除，进 Reclaimed
//	owner 匹配 且 runID ∈  live ⇒ 保留，两条都不进
//	owner 不匹配                ⇒ 保留，进 Pending(owner_mismatch)
//	无 owner 标签               ⇒ 保留，进 Pending(owner_unknown)
//	解析不出 run 标签           ⇒ 保留，进 Pending(unparsable)
//
// 它**只回答「该不该删」**，不执行删除——那是 ReclaimStale 的事。「保留」的含义
// 是**绝不删除**，不是「稍后重试」。
//
// self 必须是已解析的 owner（DockerConfig.normalize 保证非空）。若它是
// OwnerUnknown，判定会退化成「全部 Pending」：这是有意的 fail closed——「我是谁」
// 不可判定时，「谁都不像我的」不能当成「可以删」，否则这次修复就被原样取消了。
func judgeStale(objs []scanObject, self harness.OwnerID, live map[harness.RunID]bool) harness.ReclaimReport {
	var rep harness.ReclaimReport
	seen := map[harness.RunID]bool{}
	for _, o := range objs {
		switch {
		case !o.Parsed:
			rep.Pending = append(rep.Pending, harness.StaleObject{
				Kind: o.Kind, Reason: harness.StaleUnparsable,
			})
		case o.Owner == harness.OwnerUnknown:
			rep.Pending = append(rep.Pending, harness.StaleObject{
				Kind: o.Kind, RunID: o.RunID, Owner: o.Owner, Reason: harness.StaleOwnerUnknown,
			})
		case o.Owner != self:
			rep.Pending = append(rep.Pending, harness.StaleObject{
				Kind: o.Kind, RunID: o.RunID, Owner: o.Owner, Reason: harness.StaleOwnerMismatch,
			})
		case live[o.RunID]:
			// 活跃 run：正常的资源，不是遗留。两条都不进。
		default:
			if !seen[o.RunID] {
				seen[o.RunID] = true
				rep.Reclaimed = append(rep.Reclaimed, o.RunID)
			}
		}
	}
	return rep
}

// selfOwner 返回本执行器**已解析**的 owner，未解析时报 KindConfig。
//
// **为什么是错误而不是「空 owner 就少删一点」**：回收是删除闸门，闸门少一道时正确
// 的行为是**不删**并说出来，而不是删得少一点——「删得少一点」在现场看起来与「今天
// 没有孤儿」完全一样，而孤儿是宿主上无人监督的进攻性工具进程。
func (d *Docker) selfOwner() (harness.OwnerID, error) {
	if d.cfg.Owner == "" {
		return harness.OwnerUnknown, harness.Ef(harness.KindConfig, "executor.reclaim",
			"DockerConfig.Owner 未解析（所有者未知）：无法判定资源归属，拒绝回收", nil)
	}
	return harness.OwnerID(d.cfg.Owner), nil
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
