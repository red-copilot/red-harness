package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// EnvBridgeCmd 是桥命令的覆盖点。**本机是硬需求**：SDK 装在容器层、面向
// python3.14，而宿主 python3.12 没有 httpx，`import tsec_benchmark` 在宿主上
// 必然失败——所以本机真跑的唯一路径是 `docker exec <容器> python3 -m bridge`。
//
// 值按空白切分（`strings.Fields`），所以带空格的路径需要自己包一层 shell。
// 刻意不做 shell 解析：桥命令来自环境变量，而 shell 解析会让一个配置错误变成
// 一条命令注入面。
const EnvBridgeCmd = "REDCOPILOT_BRIDGE_CMD"

// EnvToken / EnvBaseURL 是凭据来源。**只从环境读，绝不进 ClientConfig 的
// 公开字段之外的地方**，也绝不被打印。
const (
	EnvToken   = "BENCHMARK_TOKEN"
	EnvBaseURL = "BENCHMARK_BASE_URL"
)

// DefaultTimeout 与 SDK 的 `_DEFAULT_TIMEOUT`（client.py:38）一致。
//
// 为什么不直接沿用 SDK 默认：桥要在 Go 侧做 deadline 判定，两边默认值不一致
// 时，Go 侧会先超时并报「桥没响应」，而桥其实还在正常等平台——那会把一次
// 慢平台误诊成桥故障。
const DefaultTimeout = 30 * time.Second

// startupTimeout 是等 bridge.py 打出 hello 行的上限。
//
// 它覆盖的是「起进程 + import tsec_benchmark + 建 httpx client」这一段。
// 容器冷启动（`docker exec` 首次拉起）可能到秒级，所以给 20 秒；超时说明
// 桥命令本身有问题，而不是平台慢——这个区分必须在 NewClient 就做出来。
const startupTimeout = 20 * time.Second

// ClientConfig 是造一个桥客户端所需的一切。
type ClientConfig struct {
	// Command 是完整 argv（argv[0] 是程序）。空表示用 DefaultCommand()。
	Command []string
	// BaseURL / Token 是平台凭据。空则分别从 BENCHMARK_BASE_URL /
	// BENCHMARK_TOKEN 读。
	BaseURL string
	Token   string
	// Timeout 是单次平台调用的墙钟上限。0 表示 DefaultTimeout。
	Timeout time.Duration
	// WorkDir 是桥子进程的工作目录。空表示继承。
	WorkDir string
	// PrivateDir 是桥 stderr 的落盘目录（private/，0700/0600）。
	//
	// 为什么必须有：桥的 Python traceback 里可能有 base_url、请求 URL 与
	// token 片段。按 §3.3 的明文纪律，这些只允许进 private/，**绝不能继承
	// 宿主 stderr**。空表示丢弃 stderr（内存尾部仍保留用于诊断）。
	PrivateDir string
	// Env 是附加给桥子进程的环境变量（合并进 os.Environ()）。
	Env map[string]string
	// VPNDisabled 为真时 CheckVPN 直接成功（离线测试用）。
	// **生产路径不要设它**——前身事故正是「预检失败被当成题目不存在继续跑」。
	VPNDisabled bool
}

// DefaultCommand 按优先级解析桥命令：
//
//  1. ClientConfig.Command（调用方显式指定）；
//  2. 环境变量 REDCOPILOT_BRIDGE_CMD；
//  3. `python3 <找到的 bridge/bridge.py>`。
//
// 第 3 步的搜索顺序刻意是「cwd 相对 → 可执行文件相对」：CLI 通常从仓库根跑，
// 而 `go test` 从包目录跑，两者都要能找到。
func DefaultCommand() ([]string, error) {
	if v := strings.TrimSpace(os.Getenv(EnvBridgeCmd)); v != "" {
		parts := strings.Fields(v)
		if len(parts) == 0 {
			return nil, fmt.Errorf("bridge: %s 是空白，无法解析成命令", EnvBridgeCmd)
		}
		return parts, nil
	}
	script, err := findScript()
	if err != nil {
		return nil, err
	}
	return []string{"python3", script}, nil
}

func findScript() (string, error) {
	var cands []string
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands,
			// CLI 通常从仓库根跑。
			filepath.Join(wd, "bridge", "bridge.py"),
			// 从 bridge/ 目录里跑（`go test ./bridge/...` 的 cwd 就是它）。
			filepath.Join(wd, "bridge.py"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(d, "bridge", "bridge.py"),
			filepath.Join(filepath.Dir(d), "bridge", "bridge.py"),
		)
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("bridge: 找不到 bridge/bridge.py（已试 %s）。"+
		"请用 ClientConfig.Command 或 %s 显式指定桥命令——本机宿主 python3 缺 httpx，"+
		"通常需要指向容器：%s='docker exec <容器> python3 -m bridge'",
		strings.Join(cands, ", "), EnvBridgeCmd, EnvBridgeCmd)
}

// helloInfo 是 bridge.py 启动时打出的第一行（`{"event":"hello",...}`）。
//
// **为什么要有它**：启动探活必须是「命令能跑 + SDK 能导入」两件事的合成判据。
// 单独跑 `python3 -c "import tsec_benchmark"` 在宿主上必然失败（无 httpx），
// 而桥命令可能指向容器——那样这个探测会得出「缺 SDK」的错误结论，把
// 「宿主失败但容器可用」这种**正常配置**误报成故障。hello 行经由**实际要用的
// 那条命令**回来，天然覆盖两种配置。
type helloInfo struct {
	SDK    string
	Python string
}

// ProbeResult 是 doctor 需要的那两级 SDK 可达性结论。
//
// 「宿主导入失败 + 容器可用」是正常配置，必须报成通过（附说明）——这是
// doctor 的核心要求，所以桥把两个判据都暴露出来，让 doctor 自己合成结论，
// 而不是让桥替它下「缺 SDK」的笼统判断。
type ProbeResult struct {
	// HostImportOK 是宿主 `python3 -c "import tsec_benchmark"` 的结果。
	HostImportOK bool
	// HostImportErr 是宿主导入失败的原因（成功时为空）。
	HostImportErr string
	// BridgeOK 为真表示桥命令真的起来了并回了 hello（⇒ SDK 在**桥侧**
	// 可导入，且桥的 stdio 协议是通的）。
	BridgeOK bool
	// SDKVersion / PythonVersion 来自 hello 行（BridgeOK 为假时为空）。
	SDKVersion    string
	PythonVersion string
	// Detail 是一句给人看的结论，含「宿主导入失败但容器可用」这种组合。
	Detail string
}

// Probe 做两级 SDK 可达性探测，供 doctor 使用。
//
// 它**不**做 VPN 预检（那是 CheckVPN 的事），也不做任何平台写操作。
func Probe(ctx context.Context, cfg ClientConfig) ProbeResult {
	res := ProbeResult{}
	// 一级：宿主的 python3。用 cfg.Command 里的 argv[0] 之外固定 `python3`：
	// 覆盖命令可能指向 docker，那不是「宿主 python」。
	hostPython := "python3"
	if len(cfg.Command) > 0 && filepath.Base(cfg.Command[0]) != "docker" {
		hostPython = cfg.Command[0]
	}
	if err := probeHostImport(ctx, hostPython); err != nil {
		res.HostImportOK = false
		res.HostImportErr = err.Error()
	} else {
		res.HostImportOK = true
	}

	// 二级：走真正要用的那条桥命令。**这里必须用同一个 ClientConfig**，
	// 否则 doctor 验的路径与 run 走的路径不是同一条。
	c, err := NewClient(cfg)
	if err != nil {
		res.BridgeOK = false
		res.Detail = describeProbe(res, "桥命令起不来: "+err.Error())
		return res
	}
	defer c.Shutdown()
	h := c.Handshake()
	// helloInfo 由 parseHello 填充；SDK 版本为空说明桥侧没导入成功——
	// 但那不该发生，因为握手行是**桥进程活着**的证据，而 SDK 导入失败时
	// 桥会打一行 sdk_missing 的错误行而不是握手行。所以这里只作记录。
	res.BridgeOK = true
	res.SDKVersion = h.SDK
	res.PythonVersion = h.Python
	res.Detail = describeProbe(res, "")
	return res
}

func describeProbe(res ProbeResult, extra string) string {
	var b strings.Builder
	switch {
	case res.BridgeOK:
		b.WriteString(fmt.Sprintf("桥侧 SDK 可用 (tsec_benchmark %s, python %s)", res.SDKVersion, res.PythonVersion))
		if !res.HostImportOK {
			b.WriteString("；宿主导入失败是**正常配置**（SDK 装在容器层），宿主原因: " + res.HostImportErr)
		}
	default:
		b.WriteString("桥侧 SDK 不可用")
		if res.HostImportOK {
			b.WriteString("（宿主导入成功但桥命令失败——检查桥命令与容器）")
		} else {
			b.WriteString("，且宿主导入也失败: " + res.HostImportErr)
		}
	}
	if extra != "" {
		b.WriteString("；" + extra)
	}
	return b.String()
}

func probeHostImport(ctx context.Context, python string) error {
	if python == "" {
		python = "python3"
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, python, "-c", "import tsec_benchmark").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

// ── 客户端 ──

// Client 是 TSecBench 平台的 Go 侧门面。它满足 harness.Platform 与
// harness.HealthChecker。
//
// **命名提醒**：`Close(ctx, code)` 是**平台操作**（关题目容器，来自
// harness.Platform 的冻结方法集）；终止桥子进程是 `Shutdown()`。两个不同的
// 东西，刻意不叫同一个名字。
type Client struct {
	cfg   ClientConfig
	argv  []string
	env   []string
	priv  string
	token string

	// mu 串行化全部桥调用。桥是一行请求一行响应，且只有一个 Python 进程
	// （一个事件循环），并发调用不会更快，只会让「哪条请求对应哪个响应」变难查。
	// 它同时保护 proc 的重启。
	mu  sync.Mutex
	p   *proc
	seq int

	// handshake 是桥启动时打出的握手行解析结果（供 doctor 的 Probe 用）。
	// **它只在启动成功时有值**——启动失败时 NewClient 直接报错，所以不需要
	// 一个额外的「有没有握手」布尔。
	handshake helloInfo

	// vpnOK 记录「预检曾经通过」。它决定重启后是否重做预检——用户还没调过
	// CheckVPN 时不能替它注入一次预检（那会让「预检失败」与「构造失败」
	// 再次混在一起，正是本任务要消除的混淆）。
	vpnOK bool

	// restarts 是累计重启次数，供诊断。
	restarts int
}

// NewClient 起桥子进程并等到 hello 行——**启动探活在这里，不在第一次调用**。
//
// 缺依赖时给的是可操作的报错（含「宿主缺 httpx，本机需要容器」这条真实
// 路径），而不是等第一次 `list` 炸出一个无从下手的 ModuleNotFoundError。
//
// **它不做 VPN 预检**：桥用 `auto_check_vpn=False` 构造裸 client，预检由
// 调用方显式调 CheckVPN。这样「预检失败」与「构造失败」是两个可区分的错误。
func NewClient(cfg ClientConfig) (*Client, error) {
	argv := cfg.Command
	if len(argv) == 0 {
		var err error
		argv, err = DefaultCommand()
		if err != nil {
			return nil, err
		}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	token := cfg.Token
	if token == "" {
		token = os.Getenv(EnvToken)
	}
	base := cfg.BaseURL
	if base == "" {
		base = os.Getenv(EnvBaseURL)
	}
	// token/base URL 不校验非空：校验放在桥侧（它知道 base URL 是否真的必填，
	// 而 Go 侧凭空拒绝会让离线测试与 `check_vpn` 干跑都跑不起来）。
	// 真缺的时候桥会回 missing_credential，消息里不带值。

	c := &Client{
		cfg:   cfg,
		argv:  argv,
		env:   bridgeEnv(cfg.Env, token, base),
		priv:  cfg.PrivateDir,
		token: token,
	}
	if err := c.startLocked(); err != nil {
		return nil, err
	}
	return c, nil
}

// bridgeEnv 组装子进程环境。
//
// 它**只增不减**：桥需要继承 PATH、HOME、PYTHONPATH 等（`docker exec` 尤其
// 依赖 PATH）。凭据在这里注入，且只注入进子进程环境——不进命令行（命令行会
// 出现在 `ps` 里），不进日志。
func bridgeEnv(extra map[string]string, token, baseURL string) []string {
	env := os.Environ()
	if token != "" {
		env = append(env, EnvToken+"="+token)
	}
	if baseURL != "" {
		env = append(env, EnvBaseURL+"="+baseURL)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// startLocked 起进程并等 hello。调用方须持有 c.mu（或尚未发布 c）。
func (c *Client) startLocked() error {
	p, err := startProc(procConfig{
		argv:       c.argv,
		dir:        c.cfg.WorkDir,
		env:        c.env,
		stderrPath: privateStderrPath(c.priv),
	})
	if err != nil {
		return err
	}
	c.p = p
	select {
	case <-p.firstCh:
		h, herr := parseHandshake(p.firstLine())
		if herr != nil {
			// 第一行不是握手行 ⇒ 启动**失败**。桥在缺 SDK / 缺凭据时就是
			// 这个形状：它先写一行带原因的 error 行再退出。
			//
			// **必须在内容上判定，不能只看「有没有读到一行」**：桥先写行、
			// 后退出，reader 与 waitDone 谁先就绪是不确定的——只看「读到行
			// 就算启动成功」会让缺依赖变成「启动成功、第一次调用才炸」。
			p.kill()
			return fmt.Errorf("bridge: 桥进程 %v 启动失败: %w（stderr: %s）",
				c.argv, herr, p.stderr())
		}
		c.handshake = h
		return nil
	case <-p.waitDone:
		// 进程直接退了（连一行都没写）——把 stderr 尾部接进错误消息，否则
		// 「起不来」无从下手（这正是前身 B14 的形状）。
		return fmt.Errorf("bridge: 桥进程启动即退出 (exit=%d): %s", p.exitCode(), p.stderr())
	case <-time.After(startupTimeout):
		p.kill()
		return fmt.Errorf("bridge: 桥进程 %v 在 %s 内没有回握手行。"+
			"宿主 python3 没有 httpx，本机需要把桥命令指向带 SDK 的容器"+
			"（%s='docker exec <容器> python3 -m bridge'）。stderr: %s",
			c.argv, startupTimeout, EnvBridgeCmd, p.stderr())
	}
}

// parseHandshake 解析桥的第一行。
//
// 只有 `{"event":"hello",...}` 才算启动成功。第一行是 error 行时返回那个
// 错误（含 code 与可操作的 message）——这样「缺 SDK」「缺凭据」都能在
// NewClient 阶段以**原始可操作原因**报出来，而不是一个笼统的「启动失败」。
func parseHandshake(raw []byte) (helloInfo, error) {
	if len(raw) == 0 {
		return helloInfo{}, errors.New("桥没有输出任何握手信息")
	}
	var hello struct {
		Event  string `json:"event"`
		SDK    string `json:"sdk"`
		Python string `json:"python"`
	}
	if err := json.Unmarshal(raw, &hello); err == nil && hello.Event == "hello" {
		return helloInfo{SDK: hello.SDK, Python: hello.Python}, nil
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err == nil && resp.Error != nil {
		return helloInfo{}, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return helloInfo{}, fmt.Errorf("桥的第一行不是握手行: %s", clipForError(raw))
}

// clipForError 截断一行原始输出用于错误消息（防止一行病态输出撑爆错误文本）。
func clipForError(raw []byte) string {
	const max = 300
	s := string(raw)
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// Handshake 返回桥启动时的握手信息（供 doctor 的 Probe）。
//
// 它不返回 ok：**握手有没有发生，由 NewClient 是否成功表达**——握手失败时
// NewClient 直接报错（含可操作的原因），所以这里不必再有一个布尔。
func (c *Client) Handshake() helloInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handshake
}

// Shutdown 终止桥子进程并等它退干净。幂等。
func (c *Client) Shutdown() {
	c.mu.Lock()
	p := c.p
	c.p = nil
	c.mu.Unlock()
	if p != nil {
		p.kill()
	}
}

// Restarts 返回累计重启次数（诊断用）。
func (c *Client) Restarts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restarts
}

// ── 桥调用 ──

// do 发一条请求。桥进程死掉时**重启一次并重发**（PLAN.md:68）。
//
// 重发是安全的，因为六个命令里有四个是幂等读（check_vpn/list/hint 无副作用），
// 而两个写（start/submit/close）在平台侧都有幂等语义：重复 start 会撞
// invalid_state、重复 submit 会回 duplicate（等价于已确认）、重复 close 幂等。
// **超时与取消不重发**——那两种情况下请求可能已经到达平台，重发才是危险动作；
// 引擎侧有 Reconcile 对账来处理这一类。
func (c *Client) do(ctx context.Context, op, cmd string, req request) (response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.p == nil {
		return response{}, harness.Ef(harness.KindConfig, op, "桥客户端已关闭", nil)
	}
	req.ID = c.nextIDLocked()
	resp, err := c.p.call(ctx, req)
	if err == nil {
		return resp, nil
	}
	if ctx.Err() != nil {
		// 超时/取消：**不重启、不重发**。请求可能已经到达平台，重发才是危险
		// 动作——引擎侧要按 Reconcile 对账后再决定（设计 §5.3）。
		//
		// 两种 ctx 错误刻意分开：
		//   - DeadlineExceeded ⇒ KindPlatform + **可重试**（平台慢/网络抖动，
		//     同样输入稍后重试可能有不同结果）。
		//   - Canceled ⇒ KindCancelled + 不可重试（用户/上层主动停，重试是错的）。
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return response{}, &harness.Error{
				Kind:      harness.KindPlatform,
				Op:        op,
				Retryable: true,
				Msg:       fmt.Sprintf("桥调用超时（%s）", cmd),
				Err:       err,
			}
		}
		return response{}, &harness.Error{
			Kind: harness.KindCancelled,
			Op:   op,
			Msg:  fmt.Sprintf("桥调用被取消（%s）", cmd),
			Err:  err,
		}
	}
	// 桥本身出了问题：重启一次再重发。
	if rerr := c.restartLocked(ctx); rerr != nil {
		return response{}, rerr
	}
	req.ID = c.nextIDLocked()
	resp, err = c.p.call(ctx, req)
	if err != nil {
		return response{}, harness.Ef(harness.KindPlatform, op,
			fmt.Sprintf("桥重启后仍失败（%s）", cmd), err)
	}
	return resp, nil
}

func (c *Client) nextIDLocked() string {
	c.seq++
	return nextID("r", c.seq)
}

// restartLocked 按退避重启桥进程，并**重做 VPN 预检**。
//
// 为什么必须重做预检：新进程是新的一次构造，它没有任何「VPN 通」的证据。
// 跳过预检会让「重启后在一个不通的 VPN 上继续跑」变成静默状态——那正是
// HealthChecker 要防的事（ports.go:95-96 记的前身事故）。
func (c *Client) restartLocked(ctx context.Context) error {
	c.restarts++
	if d := restartBackoff(c.restarts); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return harness.Ef(harness.KindCancelled, "bridge.restart", "等待重启退避时被取消", ctx.Err())
		}
	}
	if c.p != nil {
		c.p.kill()
		c.p = nil
	}
	if err := c.startLocked(); err != nil {
		return harness.Ef(harness.KindConfig, "bridge.restart", "桥进程重启失败", err)
	}
	if !c.vpnOK {
		// 用户还没做过预检，不替它注入。
		return nil
	}
	resp, err := c.p.call(ctx, request{Cmd: CmdCheckVPN, Deadline: deadlineFor(deadline(ctx, c.cfg.Timeout))})
	if err != nil {
		c.vpnOK = false
		return harness.Ef(harness.KindPlatform, "bridge.restart",
			"桥重启后 VPN 预检未能完成，已按未预检处理", err)
	}
	if resp.Error != nil {
		c.vpnOK = false
		return wireErrorToHarness(resp.Error, "bridge.restart")
	}
	return nil
}

// deadline 取「ctx 的到期时刻」与「本客户端默认超时」中更早的那个。
func deadline(ctx context.Context, def time.Duration) time.Time {
	d := time.Now().Add(def)
	if dl, ok := ctx.Deadline(); ok && dl.Before(d) {
		return dl
	}
	return d
}

// ── harness.Platform + harness.HealthChecker ──

// Health 实现 harness.HealthChecker：引擎在任何平台写操作之前先调它。
func (c *Client) Health(ctx context.Context) error { return c.CheckVPN(ctx) }

// CheckVPN 做 VPN 联通预检。
//
// **它必须在任何平台调用之前**（对齐 SDK 的 `__enter__` 语义，ports.go:95-96）。
// 桥侧用的是 `auto_check_vpn=False` 构造的裸 client + 显式 `check_vpn()`，
// 而不是上下文管理器——后者的 `__enter__` 会把预检和构造混在一起。
func (c *Client) CheckVPN(ctx context.Context) error {
	if c.cfg.VPNDisabled {
		return nil
	}
	resp, err := c.do(ctx, "platform.check_vpn", CmdCheckVPN, request{
		Cmd:      CmdCheckVPN,
		Deadline: deadlineFor(deadline(ctx, c.cfg.Timeout)),
	})
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return wireErrorToHarness(resp.Error, "platform.check_vpn")
	}
	var v vpnWire
	if err := decodeResult(resp, &v); err != nil {
		return err
	}
	if !v.OK {
		return harness.Ef(harness.KindPlatform, "platform.check_vpn",
			"VPN 预检未通过（status="+v.Status+"）", nil)
	}
	c.mu.Lock()
	c.vpnOK = true
	c.mu.Unlock()
	return nil
}

// List 实现 harness.Platform。
//
// 字段映射见 challengeWire 的注释。**两个字段刻意丢弃**：SDK 的 `level` 与
// `total_score` 在冻结的 harness.Challenge 里没有对应物，而给它加字段会改
// `dag/store.go` schema 1 的持久化键（C3 事故的形状）。平台权威进度来自
// `submit` 的 `correct_flag_count/total_flag_count`，不依赖这两个。
//
// `container_addr` **仅当 `container_status == "available"` 时非空**，所以
// 地址为空是正常的 pending 态，不是错误——调用方（Scenario.Prepare）负责轮询。
func (c *Client) List(ctx context.Context) ([]harness.Challenge, error) {
	resp, err := c.do(ctx, "platform.list", CmdList, request{
		Cmd:      CmdList,
		Deadline: deadlineFor(deadline(ctx, c.cfg.Timeout)),
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, wireErrorToHarness(resp.Error, "platform.list")
	}
	var items []challengeWire
	if err := decodeResult(resp, &items); err != nil {
		return nil, err
	}
	out := make([]harness.Challenge, 0, len(items))
	for _, it := range items {
		out = append(out, harness.Challenge{
			Code:        it.UniqueCode,
			Description: it.Description,
			Difficulty:  it.Difficulty,
			FlagCount:   it.FlagCount,
			Solved:      it.CorrectFlagCount,
			Addrs:       it.ContainerAddr,
		})
	}
	return out, nil
}

// Start 实现 harness.Platform。
//
// **空 Addrs 不是错误**：起题是异步的，`container_status` 从 pending 到
// available 需要轮询（SDK_API.md:129-130），Scenario.Prepare 用 List 轮询。
func (c *Client) Start(ctx context.Context, code string) (harness.StartResult, error) {
	resp, err := c.do(ctx, "platform.start", CmdStart, request{
		Cmd:      CmdStart,
		Code:     code,
		Deadline: deadlineFor(deadline(ctx, c.cfg.Timeout)),
	})
	if err != nil {
		return harness.StartResult{}, err
	}
	if resp.Error != nil {
		return harness.StartResult{}, wireErrorToHarness(resp.Error, "platform.start")
	}
	var w startWire
	if err := decodeResult(resp, &w); err != nil {
		return harness.StartResult{}, err
	}
	// Code 用桥回的值，但它为空时回落到请求里的 code——SDK 的 StartResult
	// 一定有 unique_code，可一旦版本漂移回了个残缺对象，用请求值比丢空更好。
	if w.UniqueCode == "" {
		w.UniqueCode = code
	}
	return harness.StartResult{Code: w.UniqueCode, Addrs: w.ContainerAddr}, nil
}

// Hint 实现 harness.Platform。**没有提示时返回空串且不报错**（ports.go:71）。
//
// 查看提示会按比例扣分，所以调用方负责「每题最多一次」的守卫（HintAuto）。
func (c *Client) Hint(ctx context.Context, code string) (harness.HintResult, error) {
	resp, err := c.do(ctx, "platform.hint", CmdHint, request{
		Cmd:      CmdHint,
		Code:     code,
		Deadline: deadlineFor(deadline(ctx, c.cfg.Timeout)),
	})
	if err != nil {
		return harness.HintResult{}, err
	}
	if resp.Error != nil {
		return harness.HintResult{}, wireErrorToHarness(resp.Error, "platform.hint")
	}
	var w hintWire
	if err := decodeResult(resp, &w); err != nil {
		return harness.HintResult{}, err
	}
	h := harness.HintResult{Code: w.UniqueCode}
	if w.Hint != nil {
		h.Hint = *w.Hint
	}
	if h.Code == "" {
		h.Code = code
	}
	return h, nil
}

// Submit 实现 harness.Platform。
//
// **`DuplicateSubmit` 映射成 `Duplicate:true` 且 err == nil**——它是幂等命中，
// 等价于已确认，不是错误（ports.go:57、harness.go 的 harvest 语义）。
// 桥侧已经把 code=`duplicate` 折成成功；这里再挡一道是为了防「桥的映射被
// 改坏」或「某个 SDK 版本把 duplicate 直接当异常抛」时静默变成一次判错。
func (c *Client) Submit(ctx context.Context, code, flag string) (harness.SubmitResult, error) {
	resp, err := c.do(ctx, "platform.submit", CmdSubmit, request{
		Cmd:      CmdSubmit,
		Code:     code,
		Flag:     flag,
		Deadline: deadlineFor(deadline(ctx, c.cfg.Timeout)),
	})
	if err != nil {
		return harness.SubmitResult{}, err
	}
	if resp.Error != nil {
		if resp.Error.Code == CodeDuplicate {
			return harness.SubmitResult{Duplicate: true, Message: "duplicate"}, nil
		}
		return harness.SubmitResult{}, wireErrorToHarness(resp.Error, "platform.submit")
	}
	var w submitWire
	if err := decodeResult(resp, &w); err != nil {
		return harness.SubmitResult{}, err
	}
	out := harness.SubmitResult{
		Correct:          w.Correct,
		Awarded:          w.Awarded,
		Duplicate:        w.Duplicate,
		CorrectFlagCount: w.CorrectFlagCount,
		TotalFlagCount:   w.TotalFlagCount,
	}
	if w.MatchedFlagIndex != nil {
		out.MatchedIndex = *w.MatchedFlagIndex
	}
	// Message 由 Go 侧拼一句**不含明文**的说明。SDK 的 SubmitResult 没有
	// message 字段，而报告要能回答「平台为什么这么判」，所以这里给一个
	// 由结构字段合成、绝不含 flag 的短句。
	switch {
	case w.Duplicate:
		out.Message = "duplicate"
	case w.Correct:
		out.Message = "correct"
	default:
		out.Message = "rejected"
	}
	return out, nil
}

// Close 实现 harness.Platform：关题目容器，释放靶场资源与活跃名额。
//
// 它与 `Client.Shutdown`（终止桥子进程）是两件事，见 Client 的注释。
func (c *Client) Close(ctx context.Context, code string) (harness.CloseResult, error) {
	resp, err := c.do(ctx, "platform.close", CmdClose, request{
		Cmd:      CmdClose,
		Code:     code,
		Deadline: deadlineFor(deadline(ctx, c.cfg.Timeout)),
	})
	if err != nil {
		return harness.CloseResult{}, err
	}
	if resp.Error != nil {
		return harness.CloseResult{}, wireErrorToHarness(resp.Error, "platform.close")
	}
	var w closeWire
	if err := decodeResult(resp, &w); err != nil {
		return harness.CloseResult{}, err
	}
	if w.UniqueCode == "" {
		w.UniqueCode = code
	}
	return harness.CloseResult{Code: w.UniqueCode, Closed: w.Closed}, nil
}

// ── 错误与载荷 ──

// decodeResult 解析 `result` 载荷。
//
// 它把**所有**解析失败包成结构化的 KindPlatform 错误。这是 Go 侧的对应物：
// Python 侧那两个 SDK 缺陷（裸 KeyError、2xx 非 JSON 的 JSONDecodeError）
// 由 bridge.py 包住，而「桥回了合法 JSON 但 result 形状不对」（版本漂移）
// 必须在这里也包住，否则一个字段改名会变成调用方的一次 panic 或静默零值。
func decodeResult(resp response, v any) error {
	if len(resp.Result) == 0 || string(resp.Result) == "null" {
		return harness.Ef(harness.KindPlatform, "bridge.decode",
			"桥响应缺少 result 载荷", nil)
	}
	if err := json.Unmarshal(resp.Result, v); err != nil {
		return harness.Ef(harness.KindPlatform, "bridge.decode",
			"桥响应 result 形状不符（可能是 SDK 版本漂移）", err)
	}
	return nil
}

// wireErrorToHarness 把桥的结构化错误转成 harness.Error。
//
// **Msg 不照抄桥的 message**：Error 会被序列化进 JSON（看板、日志、报告），
// 而桥的 message 来自平台响应体，可能带 URL 与凭据片段。这里用 code +
// 一句固定的中文说明合成安全消息，原始 message 放进 Err（不参与序列化）。
func wireErrorToHarness(w *wireError, op string) error {
	if w == nil {
		return nil
	}
	kind := classToKind(w.Class)
	retry := w.Retryable
	// invalid_state 的可重试性由桥判定（它能看到异常原文）。这里只在桥
	// 漏判时用本地判据兜底——**不覆盖桥的判定**，因为桥那边有 detail。
	if w.Code == CodeInvalidState {
		if !retry && invalidStateRetryable(w.Message) {
			retry = true
		}
	}
	if w.Code == CodeResourceUnavail || w.Code == CodeInternalError {
		retry = true
	}
	msg := w.Code
	if w.StatusCode != 0 {
		msg = fmt.Sprintf("%s (http %d)", msg, w.StatusCode)
	}
	if h := codeHint(w.Code, w.Message); h != "" {
		msg += "：" + h
	}
	return &harness.Error{
		Kind:      kind,
		Op:        op,
		Retryable: retry,
		Msg:       msg,
		Err:       fmt.Errorf("bridge error %s: %s", w.Code, w.Message),
	}
}

// codeHint 给每个错误码一句可操作的说明。
//
// 这些说明是给**人**看的（报告、CLI 输出），所以刻意写成「下一步做什么」，
// 而不是复述平台消息。它们不含任何平台返回值。
func codeHint(code, platformMsg string) string {
	switch code {
	case CodeVPNCheckFailed:
		return "请先连接靶场 VPN 再重试"
	case CodeTaskNotFound:
		return "BENCHMARK_TOKEN 无效或缺失"
	case CodeChallengeNotFound:
		return "题目 code 不在本任务的题目集里"
	case CodeInvalidState:
		if invalidStateRetryable(platformMsg) {
			return "活跃容器数已达上限，关闭一个题目后重试"
		}
		return "任务已结束或题目状态不允许该操作"
	case CodeResourceUnavail:
		return "靶场实例未就绪或池子已空，稍后重试"
	case CodeInternalError:
		return "平台内部故障，稍后重试"
	case CodeValidationError:
		return "请求参数不合法"
	case CodeConnectionError:
		return "无法连接平台（网络或 VPN）"
	case CodeInvalidResponse:
		return "平台返回了无法解析的响应"
	case CodeSDKMissing:
		return "桥侧无法导入 tsec_benchmark"
	case CodeMissingCredential:
		return "缺少 BENCHMARK_TOKEN 或 BENCHMARK_BASE_URL"
	case CodeUnknownCommand:
		return "桥不认识该命令（Go 侧与 bridge.py 版本不一致）"
	case CodeProtocolError:
		return "桥的 stdio 协议被污染（检查是否有东西往 stdout 打印）"
	case CodeSubprocessExit:
		return "桥子进程退出"
	case CodeDeadlineExceeded:
		return "桥在 deadline 前未能开始该调用"
	default:
		return ""
	}
}

func classToKind(class string) harness.Kind {
	switch class {
	case ClassConfig:
		return harness.KindConfig
	case ClassCanceled:
		return harness.KindCancelled
	case ClassInternal:
		// 桥自身/平台的内部故障都按平台类上报：调用方能做的动作相同
		// （稍后重试或放弃），分成两个 Kind 只会让调用方多一个空分支。
		return harness.KindPlatform
	default:
		return harness.KindPlatform
	}
}
