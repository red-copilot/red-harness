package piai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// Agent 是持久 pi 会话客户端，实现 harness.Agent。
//
// 生命周期：Start 起进程 + 握手 + new_session；Round 一轮一个 prompt；Close 整组
// kill。pi 不会自己退出（M0 实测），所以 Close 不能省。
type Agent struct {
	// Session, when set, is the only supported process-launch path for v0.4.
	// A nil Session keeps the legacy local test adapter available.
	Session harness.SandboxSession
	// BinPath 覆盖二进制路径（stub 测试与显式部署用）。为空时按
	// DiscoverBin 的顺序发现。
	BinPath string
	// Workdir 是 pi 的工作目录。AGENTS.md 从这里加载（M0 确认非交互模式照常加载）。
	Workdir string
	// SessionDir 是 --session-dir。为空时落在 <Workdir>/.pi-sessions。
	// 不用 --no-session：get_entries 游标与 sessionFile 需要会话文件。
	SessionDir string
	// HomeDir 指向一个每题独立的 HOME。pi 从 $HOME/.pi/agent/* 发现扩展/角色/
	// 技能，共享 HOME 会跨题污染（前身教训）。为空时不动 HOME。
	HomeDir string
	// EnvFile 显式指定 .env 路径。为空时从 Workdir 向上逐级查找。
	EnvFile string

	Provider string
	Model    string
	Thinking string
	// SystemPrompt 走 --append-system-prompt（保留 pi 默认编码能力）。
	SystemPrompt string
	// Extensions 是 extension 文件的路径（自动转绝对路径）。非空时 Start 会
	// 断言 get_commands 里确实出现了 source=="extension" 的命令。
	Extensions []string
	// Approve 对应 --approve。不开的话项目资源被**静默**忽略。
	Approve bool

	// VersionRange 覆盖支持区间。零值用 DefaultVersionRange。
	VersionRange VersionRange

	// StallTimeout 是无事件多久后触发 get_state 探活。默认 90s。
	StallTimeout time.Duration
	// ProbeTimeout 是单次探活的往返上限。默认 15s（M0 最坏 414ms，余量充足）。
	ProbeTimeout time.Duration
	// AbortGrace 是 ctx 到期后等 agent_settled 的宽限期。默认 10s。
	// 10s **是拍的**（M10 实测后再定）：太短会把正在落盘的内容丢掉（可解释性
	// 没了），太长会白等。真 pi 收到 abort 后落盘要多久目前没有数据。
	AbortGrace time.Duration
	// RestartBackoff 是进程死亡后的重启退避基数（指数增长）。默认 2s。
	RestartBackoff time.Duration
	// MaxRestarts 是单次 Start..Close 之间允许的重启次数，超过则 Round 直接失败。
	// 默认 5。没有上限的话「pi 起不来」会变成无限重启循环。
	MaxRestarts int

	// OnRawEvent 是原始帧回调（取证/调试用，可空）。**不要在这里做 IO**。
	OnRawEvent func(env Envelope, raw []byte)

	mu        sync.Mutex
	cur       *proc
	started   bool
	closed    bool
	restarts  int
	nextTry   time.Time
	sessionID string
	// needReset 表示下次 Round 前必须重做 new_session。进程重启、或上一轮
	// 以失败收场时置位——「会话未复位」绝不能用假设糊过去（设计 §二）。
	needReset bool
	lastErr   string

	// 当前在飞的轮次。reader 只往它推事件。
	rs *roundState

	version string
	env     []string
	envPath string
}

// roundState 是「reader 协程 → Round 协程」的事件通道。
//
// 为什么不用无缓冲 channel：Round 可能在探活里阻塞几百毫秒，而 reader 绝不能
// 因此停下来（它一停，pi 的 stdout 管道就会写满 ⇒ 死锁）。所以用一个受 mutex
// 保护的切片 + 单槽通知：reader 永不阻塞，Round 醒来后整批取走。
type roundState struct {
	mu     sync.Mutex
	frames []frame
	notify chan struct{}
}

type frame struct {
	env Envelope
	raw []byte
}

func newRoundState() *roundState {
	return &roundState{notify: make(chan struct{}, 1)}
}

func (r *roundState) push(env Envelope, raw []byte) {
	r.mu.Lock()
	r.frames = append(r.frames, frame{env: env, raw: append([]byte(nil), raw...)})
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *roundState) drain() []frame {
	r.mu.Lock()
	f := r.frames
	r.frames = nil
	r.mu.Unlock()
	return f
}

func (a *Agent) versionRange() VersionRange {
	if a.VersionRange.Min == "" && a.VersionRange.Max == "" {
		return DefaultVersionRange
	}
	return a.VersionRange
}

func (a *Agent) stallTimeout() time.Duration {
	if a.StallTimeout > 0 {
		return a.StallTimeout
	}
	return 90 * time.Second
}

func (a *Agent) probeTimeout() time.Duration {
	if a.ProbeTimeout > 0 {
		return a.ProbeTimeout
	}
	return 15 * time.Second
}

func (a *Agent) abortGrace() time.Duration {
	if a.AbortGrace > 0 {
		return a.AbortGrace
	}
	return 10 * time.Second
}

func (a *Agent) restartBackoff() time.Duration {
	if a.RestartBackoff > 0 {
		return a.RestartBackoff
	}
	return 2 * time.Second
}

func (a *Agent) maxRestarts() int {
	if a.MaxRestarts > 0 {
		return a.MaxRestarts
	}
	return 5
}

func (a *Agent) sessionDir() string {
	if a.SessionDir != "" {
		return a.SessionDir
	}
	return filepath.Join(a.Workdir, ".pi-sessions")
}

// Start 起进程、握手、校验版本、断言 extension 已加载、new_session。
//
// 顺序是有讲究的：版本校验在起进程**之前**（版本不符是配置错，不该先花掉一次
// Node 冷启动），extension 自检在 new_session **之前**（扩展没加载上，整轮会
// 跑空——M0 结论 #6 明确要求用 get_commands 而不是等第一次工具调用）。
func (a *Agent) Start(ctx context.Context, req harness.AgentStart) error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return errors.New("piai: Agent 已启动")
	}
	a.started = true
	a.mu.Unlock()

	// harness.AgentStart 是权威输入，覆盖 Agent 上的同名字段。
	if req.Workdir != "" {
		a.Workdir = req.Workdir
	}
	if req.SessionDir != "" {
		a.SessionDir = req.SessionDir
	}
	if req.Provider != "" {
		a.Provider = req.Provider
	}
	if req.Model != "" {
		a.Model = req.Model
	}
	if req.SystemPrompt != "" {
		a.SystemPrompt = req.SystemPrompt
	}
	if len(req.Extensions) > 0 {
		a.Extensions = req.Extensions
	}
	a.Approve = a.Approve || req.Approve
	if req.Thinking != "" {
		a.Thinking = req.Thinking
	}
	if a.Workdir == "" {
		a.Workdir = "."
	}
	if abs, err := filepath.Abs(a.Workdir); err == nil {
		a.Workdir = abs
	}

	var bin string
	if a.Session != nil {
		if _, err := a.Session.Probe(ctx); err != nil {
			return err
		}
		bin = a.BinPath
		if bin == "" {
			bin = "pi"
		}
	} else {
		var err error
		bin, err = DiscoverBin(a.BinPath)
		if err != nil {
			return err
		}
	}
	a.BinPath = bin

	envMap, envPath, err := resolveEnvFile(a.EnvFile, a.Workdir)
	if err != nil {
		return err
	}
	var env []string
	if a.Session != nil {
		env = sandboxEnv(envMap, a.Provider, a.HomeDir)
	} else {
		env = childEnv(envMap, filepath.Dir(bin))
		if a.HomeDir != "" {
			env = append(env, "HOME="+a.HomeDir)
		}
	}
	a.env = env
	a.envPath = envPath

	// provider 凭据预检：pi 把 401 呈现成一次**静默的空会话**（M0 实测
	// stopReason=error + 空 content + 照样 agent_settled）。在这里显式拦下，
	// 好过以「跑完了但什么都没发生」的形式烧掉整个题库。
	if a.Provider != "" && !envHasKey(env, a.Provider) {
		return fmt.Errorf("piai: provider %q 没有可用的 API key（.env=%s）。pi 会把缺凭据呈现成静默的空会话，所以这里直接拒绝启动。期望的环境变量名：%s",
			a.Provider, envPath, providerKeyName(a.Provider))
	}

	if a.Session != nil {
		// The version probe must run in the same image and namespace as pi. The
		// attached session is the sole process slot, so the image-level Probe is
		// the management check and protocol handshake is the runtime check.
		a.version = "sandbox"
	} else {
		if v, err := checkVersion(ctx, bin, env, a.versionRange()); err != nil {
			return err
		} else {
			a.version = v
		}
	}

	if err := a.spawn(ctx); err != nil {
		return err
	}
	return a.handshake(ctx)
}

// providerKeyName 按 pi 的官方惯例推导：<PROVIDER 大写，连字符换下划线>_API_KEY。
func providerKeyName(provider string) string {
	return strings.ToUpper(strings.ReplaceAll(provider, "-", "_")) + "_API_KEY"
}

// envHasKey 检查子进程环境里该 provider 的 key 是否存在且非空。
// 已知 provider 有别名（opencode-go 走 OPENCODE_API_KEY），所以按惯例名 + 别名
// 一起查，避免把合法配置误判成缺凭据。
func envHasKey(env []string, provider string) bool {
	names := []string{providerKeyName(provider)}
	switch provider {
	case "opencode-go", "opencode":
		names = append(names, "OPENCODE_API_KEY", "OPENCODE_GO_API_KEY")
	case "anthropic":
		names = append(names, "ANTHROPIC_API_KEY")
	}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for _, n := range names {
			if k == n && strings.TrimSpace(v) != "" {
				return true
			}
		}
	}
	return false
}

// handshake 做 extension 自检 + 会话复位断言。
func (a *Agent) handshake(ctx context.Context) error {
	if len(a.Extensions) > 0 {
		cmds, err := a.commands(ctx)
		if err != nil {
			return fmt.Errorf("piai: 启动自检失败（get_commands）: %w", err)
		}
		if !hasExtensionCommand(cmds) {
			return fmt.Errorf("piai: 启动自检失败：传了 %d 个 extension（%v）但 get_commands 里没有任何 source==extension 的命令。"+
				"非交互模式下项目资源默认被忽略，请检查路径是否为绝对路径且 --approve 已开启（当前 Approve=%v）",
				len(a.Extensions), a.Extensions, a.Approve)
		}
	}
	if err := a.NewSession(ctx); err != nil {
		return err
	}
	return nil
}

func hasExtensionCommand(cmds []SlashCommand) bool {
	for _, c := range cmds {
		if c.Source == "extension" {
			return true
		}
	}
	return false
}

// spawn 起一个 pi 进程并装上回调。调用方负责 already-dead 检查。
func (a *Agent) spawn(ctx context.Context) error {
	args := buildArgs(a.sessionDir(), a.Extensions, a.Provider, a.Model, a.Thinking, a.SystemPrompt, a.Approve)
	if a.Session == nil {
		if err := os.MkdirAll(a.sessionDir(), 0o755); err != nil {
			return fmt.Errorf("piai: 创建 session-dir 失败: %w", err)
		}
	}
	// 回调随 startProc 一起传进去：进程一起来 reader 就在跑，任何「返回后再赋值」
	// 的写法都会留下丢帧窗口（丢一条 extension_ui_request 就是永久挂死）。
	cfg := procConfig{
		bin:  a.BinPath,
		args: args,
		env:  a.env,
		dir:  a.Workdir,
		onFrame: func(env Envelope, raw []byte) {
			if a.OnRawEvent != nil {
				a.OnRawEvent(env, raw)
			}
			a.mu.Lock()
			rs := a.rs
			a.mu.Unlock()
			if rs != nil {
				rs.push(env, raw)
			}
		},
		onDeath: func(msg string) {
			a.mu.Lock()
			a.lastErr = msg
			// 这里**不**置 needReset：新起的进程身上没有任何会话，复位由
			// resetIfNeeded 里那次无条件的 new_session 负责。置了反而会让
			// resetIfNeeded 多杀一个刚起好的进程，白白烧掉一次重启预算
			// （死亡-重启循环里很容易因此提前撞上 MaxRestarts）。
			a.mu.Unlock()
		},
	}
	var p *proc
	var err error
	if a.Session != nil {
		cmd := append([]string{a.BinPath}, args...)
		managed, launchErr := a.Session.Launch(ctx, harness.ProcessSpec{Command: cmd, Env: envMapToMap(a.env), Workdir: a.Workdir})
		if launchErr != nil {
			return launchErr
		}
		p, err = startManagedProc(managed, cfg)
	} else {
		p, err = startProc(cfg)
	}
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.cur = p
	// 新进程没有会话，needReset 的语义是「当前进程的会话不可信」，所以清掉。
	a.needReset = false
	a.mu.Unlock()
	return nil
}

func envMapToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// ensureProc 保证有一个活进程，必要时按退避重启。
//
// 「进程中途死亡 ⇒ 监督器重启（带退避）⇒ 下次 Round 前重新 new_session」是
// 设计 §二 的要求。重启有上限：pi 起不来（二进制被删/端口被占/凭据坏）时，
// 无限重启会把整道题的墙钟预算耗光，还会让日志看不出真正的死因。
func (a *Agent) ensureProc(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return errors.New("piai: Agent 已关闭")
	}
	p := a.cur
	needSpawn := p == nil || p.isDead()
	wait := time.Until(a.nextTry)
	if needSpawn && wait > 0 {
		a.mu.Unlock()
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		return a.ensureProc(ctx)
	}
	if !needSpawn {
		a.mu.Unlock()
		return nil
	}
	if a.restarts >= a.maxRestarts() {
		n := a.restarts
		a.mu.Unlock()
		return fmt.Errorf("piai: 进程已重启 %d 次（上限 %d），放弃。最后错误: %s", n, a.maxRestarts(), a.lastErr)
	}
	a.restarts++
	n := a.restarts
	// 指数退避：2s, 4s, 8s... 上限 30s。退避**同时**是下次尝试的闸门：进程刚
	// 死就要重启时，nextTry 在未来，上面的等待分支会先把这段时间睡掉。
	back := a.restartBackoff() * time.Duration(1<<uint(min(n-1, 4)))
	if back > 30*time.Second {
		back = 30 * time.Second
	}
	a.nextTry = time.Now().Add(back)
	a.mu.Unlock()

	if err := a.spawn(ctx); err != nil {
		return err
	}
	if len(a.Extensions) > 0 {
		// 重启后也要重新自检：扩展加载失败是「静默跑空」的典型入口。
		if cmds, err := a.commands(ctx); err != nil {
			return fmt.Errorf("piai: 重启后自检失败: %w", err)
		} else if !hasExtensionCommand(cmds) {
			return errors.New("piai: 重启后 get_commands 里仍无 extension 命令")
		}
	}
	return nil
}

// NewSession 发 new_session 并用 get_state **断言**会话已复位。
//
// 断言而不是假设：`{"cancelled":true}`（被 extension 的 session_before_switch
// 取消）与「id 没变」都意味着会话没复位，此时发 prompt 会把上一题的上下文带进来
// ——评测公平性直接失效。断言失败按「会话未复位」处理：重启 pi 进程。
func (a *Agent) NewSession(ctx context.Context) error {
	a.mu.Lock()
	prevID := a.sessionID
	a.mu.Unlock()

	resp, err := a.call(ctx, Command{Type: CmdNewSess})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("piai: new_session 失败: %s", resp.Error)
	}
	var data NewSessionData
	if len(resp.Data) > 0 {
		_ = json.Unmarshal(resp.Data, &data)
	}
	if data.Cancelled {
		return fmt.Errorf("piai: new_session 被 extension 取消（cancelled=true），会话未复位")
	}
	st, err := a.probe(ctx)
	if err != nil {
		return fmt.Errorf("piai: new_session 后 get_state 断言失败: %w", err)
	}
	if st.IsStreaming {
		a.markNeedReset()
		return fmt.Errorf("piai: 会话未复位：new_session 后 isStreaming 仍为 true")
	}
	if prevID != "" && st.SessionID == prevID {
		a.markNeedReset()
		return fmt.Errorf("piai: 会话未复位：new_session 后 sessionId 未变（仍是 %s）", prevID)
	}
	if st.MessageCount != 0 {
		a.markNeedReset()
		return fmt.Errorf("piai: 会话未复位：new_session 后 messageCount=%d（期望 0），历史未清空", st.MessageCount)
	}
	a.mu.Lock()
	a.sessionID = st.SessionID
	a.needReset = false
	a.mu.Unlock()
	return nil
}

func (a *Agent) markNeedReset() {
	a.mu.Lock()
	a.needReset = true
	a.mu.Unlock()
}

// resetIfNeeded 在**每一轮**发 prompt 前复位会话（每题一次 new_session 是评测
// 公平性的前提：题目间的上下文必须隔离）。失败时**重启进程**再试一次——
// 「会话未复位」不是可以带着走的软错误。
//
// 这里刻意不缓存「上一轮刚复位过」的判断：只要一轮跑完，历史里就有上一题的内容，
// 复用会话等于把上一题的答案与死胡同带进下一题。
func (a *Agent) resetIfNeeded(ctx context.Context) error {
	a.mu.Lock()
	need := a.needReset
	cur := a.cur
	a.mu.Unlock()
	// 进程已经死了的话，换进程这件事由 ensureProc 做，不必在这里多杀一次
	// （多杀一次会让重启计数凭空 +1，撞上 MaxRestarts 上限）。
	if need && cur != nil && !cur.isDead() {
		a.restartProc()
	}
	if err := a.ensureProc(ctx); err != nil {
		return err
	}
	if err := a.NewSession(ctx); err == nil {
		return nil
	} else {
		a.lastErr = err.Error()
	}
	a.restartProc()
	if err := a.ensureProc(ctx); err != nil {
		return err
	}
	if err := a.NewSession(ctx); err != nil {
		return fmt.Errorf("piai: 会话未复位且重启后仍无法复位: %w", err)
	}
	return nil
}

// restartProc 整组 kill 当前进程（若还活着），并让 ensureProc 去重启。
func (a *Agent) restartProc() {
	a.mu.Lock()
	p := a.cur
	a.cur = nil
	a.needReset = true
	a.nextTry = time.Time{}
	a.mu.Unlock()
	if p != nil {
		p.kill()
	}
}

func (a *Agent) call(ctx context.Context, c Command) (Response, error) {
	if err := a.ensureProc(ctx); err != nil {
		return Response{}, err
	}
	a.mu.Lock()
	p := a.cur
	a.mu.Unlock()
	if p == nil {
		return Response{}, errors.New("piai: 无可用进程")
	}
	return p.call(ctx, c)
}

func (a *Agent) commands(ctx context.Context) ([]SlashCommand, error) {
	resp, err := a.call(ctx, Command{Type: CmdGetCmds})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}
	var data struct {
		Commands []SlashCommand `json:"commands"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, err
	}
	return data.Commands, nil
}

// probe 是一次 get_state 往返。看门狗与复位断言都走它。
func (a *Agent) probe(ctx context.Context) (SessionState, error) {
	pctx, cancel := context.WithTimeout(ctx, a.probeTimeout())
	defer cancel()
	resp, err := a.call(pctx, Command{Type: CmdGetState})
	if err != nil {
		return SessionState{}, err
	}
	if !resp.Success {
		return SessionState{}, fmt.Errorf("get_state 失败: %s", resp.Error)
	}
	var st SessionState
	if err := json.Unmarshal(resp.Data, &st); err != nil {
		return SessionState{}, err
	}
	return st, nil
}

// ─────────────────────────── Round ───────────────────────────

// roundAcc 是本轮的累加器。初值刻意表达「未完成」：Reason 为空表示还没收尾，
// 前身「子进程 exitCode 初值 0 让运行中被算成已完成」的坑在 RoundResult 上同样
// 成立（设计 §十二），所以这里绝不给 Turns/Reason 任何「看起来完成」的默认值。
type roundAcc struct {
	turns       int
	text        strings.Builder
	cancelledUI int
	err         string
	reason      string
	settled     bool
	// providerErr 记录末条 assistant 消息的 stopReason=="error"。
	//
	// 它是**轮内**的暂存值：agent_settled 时被搬进 err（累积），同时把
	// providerErrThisRound 报给 RoundResult.ProviderError。
	providerErr string
	// providerErrThisRound 是**本轮独立**的 provider 故障，与 err 的累积语义
	// 分开。为什么必须分开：err 一旦被置上就跟着这个会话走到最后，而
	// agent_settled 每轮都来——第 1 轮的一次 401 会让之后每一轮的 Err 都带
	// pi_provider_error，调用方按 Err 判「模型服务挂了」就会在第 3 轮已经恢复
	// （甚至拿到 flag）时仍然判失败。判据必须是「这一轮自己有没有出错」。
	providerErrThisRound string
	// 节流时间戳。
	lastText, lastThink, lastToolUpdate time.Time
}

// setProviderErr 同时记录「累积」与「本轮」两份 provider 错误。
//
// turn_end 与 agent_end 会在同一次 provider 失败里各报一次（M0 实测两个事件都带
// stopReason=error），这里是幂等覆盖，重复调用无害。
func (acc *roundAcc) setProviderErr(msg string) {
	if msg == "" {
		msg = "provider 返回 stopReason=error（无错误文本）"
	}
	acc.providerErr = msg
	acc.providerErrThisRound = msg
}

func (a *Agent) Round(ctx context.Context, prompt string, emit func(harness.Event)) (harness.RoundResult, error) {
	if err := a.ensureProc(ctx); err != nil {
		return failRound(err), err
	}
	if err := a.resetIfNeeded(ctx); err != nil {
		return failRound(err), err
	}

	rs := newRoundState()
	a.mu.Lock()
	a.rs = rs
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.rs == rs {
			a.rs = nil
		}
		a.mu.Unlock()
	}()

	// 节流时间戳留零值：**第一个**增量必须立刻透出（否则短轮次会一条 text 事件
	// 都没有，态势台/transcript 直接看不到正文），之后同一秒内的才被压掉。
	acc := &roundAcc{}
	if emit == nil {
		emit = func(harness.Event) {}
	}

	// 正常路径：new_session 后空闲态发 prompt，**不传 streamingBehavior**。
	// 传了会静默改变语义（消息被排进 steering 队列而不是立即开跑），只有在
	// 「确实还在 streaming」时才该带它——那种情况在本实现里不会发生，因为每轮
	// 都以 agent_settled 收尾。
	if err := a.sendPrompt(ctx, prompt); err != nil {
		return failRound(err), err
	}

	a.mu.Lock()
	p := a.cur
	a.mu.Unlock()
	died := p.died()

	stall := time.NewTimer(a.stallTimeout())
	defer stall.Stop()
	probeFails := 0
	var prev SessionState
	havePrev := false

	for {
		if acc.settled {
			return a.finishRound(acc, nil)
		}
		select {
		case <-ctx.Done():
			// 优雅收尾：先 abort，宽限期内等 agent_settled，仍在 streaming 才 killpg。
			return a.finishRound(acc, a.gracefulStop(rs, acc, emit))
		case <-died:
			msg := a.deathMessage()
			acc.err = msg
			a.markNeedReset()
			// 必须置 error：0 回合 + 空错误的「跑完了但什么都没发生」正是前身
			// 280 run / 0 flag 的呈现方式。
			return a.finishRound(acc, errors.New(msg))
		case <-stall.C:
			prog, wedged := a.watchdog(ctx, emit, &probeFails, &prev, &havePrev)
			// 探活成功本身就是一次「有活动」的证据：pi 的 event loop 还能回话，
			// 说明它没死锁，只是可能在长时间生成没有 delta 的内容。所以重置
			// 计时器，而不是因为「状态没变」就判死（那正是前身「无输出即卡死」
			// 换个名字重犯）。
			stall.Reset(a.stallTimeout())
			if wedged {
				msg := ErrWedged + ": get_state 连续两次探活失败，判定死锁"
				acc.err = msg
				a.restartProc()
				return a.finishRound(acc, errors.New(msg))
			}
			if prog {
				probeFails = 0
			}
		case <-rs.notify:
		}
		// 醒来的第一件事：把攒下的事件全处理掉（探活期间可能积了一批）。
		for _, f := range rs.drain() {
			a.handleEvent(f.env, f.raw, acc, emit, rs)
		}
	}
}

func (a *Agent) deathMessage() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastErr != "" {
		return a.lastErr
	}
	return ErrProcessDied
}

// sendPrompt 入队 prompt 并等它的 response（success=true 表示已被接受）。
// 等 response 而不是发完就走：success=false 是「接受前被拒」（例如 compaction
// 进行中、无可用模型），这种拒绝必须当轮失败，不能等到看门狗超时。
func (a *Agent) sendPrompt(ctx context.Context, prompt string) error {
	resp, err := a.call(ctx, Command{Type: CmdPrompt, Msg: prompt})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("piai: prompt 被拒: %s", resp.Error)
	}
	return nil
}

// gracefulStop 是 ctx 到期/取消时的收尾：abort → 宽限等 settled → 仍在 streaming
// 才 killpg。宽限的价值是保住已产出的文本与 flag（设计 §二）。
func (a *Agent) gracefulStop(rs *roundState, acc *roundAcc, emit func(harness.Event)) error {
	a.mu.Lock()
	p := a.cur
	a.mu.Unlock()
	acc.reason = harness.ReasonStopped
	if p == nil {
		return context.Canceled
	}
	// abort 本身用独立超时：调用方的 ctx 已经死了，不能拿它当预算。
	actx, cancel := context.WithTimeout(context.Background(), a.probeTimeout())
	_, _ = p.call(actx, Command{Type: CmdAbort})
	cancel()

	deadline := time.After(a.abortGrace())
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			// 宽限期到仍在跑 ⇒ 只能整组 kill。pi 不会自己退出（M0）。
			p.kill()
			a.markNeedReset()
			return fmt.Errorf("%w: abort 宽限期（%s）内未收到 agent_settled，已 killpg", context.Canceled, a.abortGrace())
		case <-tick.C:
			for _, f := range rs.drain() {
				a.handleEvent(f.env, f.raw, acc, emit, rs)
			}
			if acc.settled {
				return context.Canceled
			}
		}
	}
}

func (a *Agent) finishRound(acc *roundAcc, err error) (harness.RoundResult, error) {
	res := harness.RoundResult{
		Turns:         acc.turns,
		Text:          acc.text.String(),
		Reason:        acc.reason,
		Err:           acc.err,
		ProviderError: acc.providerErrThisRound,
		CancelledUI:   acc.cancelledUI,
	}
	if res.Reason == "" {
		if res.Err == "" {
			res.Reason = harness.ReasonCompleted
		} else {
			res.Reason = harness.ReasonError
		}
	}
	if err == nil && res.Err != "" {
		// 有 Err 就必须让调用方也看到 error。harness 的 ctx 分支（先判
		// ctx.Err() 再判 err）依赖 err != nil 才进得去，所以这里不能只填
		// RoundResult.Err 而不返回 error——否则调用方会把一次真失败当成成功。
		err = errors.New(res.Err)
	}
	return res, err
}

func failRound(err error) harness.RoundResult {
	return harness.RoundResult{Reason: harness.ReasonError, Err: err.Error()}
}

// ─────────────────────────── 事件处理 ───────────────────────────

// handleEvent 是**帧解码表**的实现（设计 §二）。它只在 Round 协程里被调用，
// 所以 emit 天然是单线程的。
func (a *Agent) handleEvent(env Envelope, raw []byte, acc *roundAcc, emit func(harness.Event), rs *roundState) {
	switch env.Type {
	case "tool_execution_start":
		// Turns 语义 = 数 tool_execution_start（护栏依赖，不要改）。
		acc.turns++
		var e struct {
			ToolCallID string         `json:"toolCallId"`
			ToolName   string         `json:"toolName"`
			Args       map[string]any `json:"args"`
		}
		_ = json.Unmarshal(raw, &e)
		emit(harness.Event{
			Kind:       harness.EventToolStart,
			Tool:       e.ToolName,
			Args:       e.Args,
			ToolCallID: e.ToolCallID,
		})

	case "tool_execution_end":
		var e ToolEnd
		_ = json.Unmarshal(raw, &e)
		emit(harness.Event{
			Kind:       harness.EventToolEnd,
			Tool:       e.ToolName,
			ToolCallID: e.ToolCallID,
			Output:     e.Text(),
			Details:    e.Result.Details,
			IsError:    e.IsError,
		})

	case "tool_execution_update":
		// 节流 1/s：进度事件密度可达每秒几十条，不节流会把态势台与 JSONL 淹掉。
		now := time.Now()
		if now.Sub(acc.lastToolUpdate) < time.Second {
			return
		}
		acc.lastToolUpdate = now
		var e ToolProgress
		_ = json.Unmarshal(raw, &e)
		out := ""
		for _, c := range e.PartialResult.Content {
			out += c.Text
		}
		emit(harness.Event{Kind: harness.EventToolProgress, Tool: e.ToolName, ToolCallID: e.ToolCallID, Output: out})

	case "message_update":
		var d Delta
		_ = json.Unmarshal(raw, &d)
		now := time.Now()
		switch d.Assistent.Type {
		case "text_delta":
			// 文本累积**不受节流影响**：RoundResult.Text 是权威产物，节流只影响
			// 事件流的密度。
			acc.text.WriteString(d.Assistent.Delta)
			if now.Sub(acc.lastText) >= time.Second {
				acc.lastText = now
				emit(harness.Event{Kind: harness.EventText, Text: d.Assistent.Delta})
			}
		case "thinking_delta":
			if now.Sub(acc.lastThink) >= time.Second {
				acc.lastThink = now
				emit(harness.Event{Kind: harness.EventThinking, Text: d.Assistent.Delta})
			}
		case "toolcall_start":
			// 工具调用的开始也走 tool_start？不——Turns 只由 tool_execution_start
			// 计数（否则会和 tool_execution_start 重复计数）。这里只在事件流里
			// 留一条带 id 的痕迹，供 DAG 提前建立锚点。
			emit(harness.Event{Kind: harness.EventToolStart, Tool: d.Assistent.ToolName, ToolCallID: d.Assistent.ToolCallID})
		}

	case "turn_end":
		var e TurnEnd
		_ = json.Unmarshal(raw, &e)
		emit(harness.Event{Kind: harness.EventTurnDone, Text: e.Message.Role})
		if e.Message.StopReason == "error" {
			acc.setProviderErr(strings.TrimSpace(e.Message.ErrorMessage))
		}

	case "agent_end":
		var e AgentEnd
		_ = json.Unmarshal(raw, &e)
		emit(harness.Event{Kind: harness.EventTurnDone})
		for i := len(e.Messages) - 1; i >= 0; i-- {
			m := e.Messages[i]
			if m.Role != "assistant" {
				continue
			}
			if m.StopReason == "error" {
				acc.setProviderErr(strings.TrimSpace(m.ErrorMessage))
			}
			break
		}

	case "agent_settled":
		// 本轮完成信号。
		acc.settled = true
		// 不清零：finishRound 还要读它来填 RoundResult.ProviderError。轮级语义
		// 靠「每轮 Round 都新建一个 acc」天然成立——清零反而会把值在读取前抹掉
		// （第一版就是这么错的：agent_settled 里清、finishRound 里读，读到空）。
		roundErr := acc.providerErrThisRound
		if roundErr != "" && acc.err == "" {
			// pi 把 provider 错误呈现成一次静默的空会话（stopReason=error + 空
			// content + 照样 agent_settled）。不在这里置 error，harness 的
			// provider_failure 护栏就失效，题库会被静默烧掉。
			acc.err = "pi_provider_error: " + roundErr
		}
		emit(harness.Event{Kind: harness.EventSettled})

	case "compaction_start", "compaction_end":
		var e CompactionEnd
		_ = json.Unmarshal(raw, &e)
		emit(harness.Event{Kind: harness.EventCompaction, Text: e.Reason, Err: e.ErrorMessage})

	case "auto_retry_start":
		var e RetryStart
		_ = json.Unmarshal(raw, &e)
		emit(harness.Event{Kind: harness.EventRetry, Text: fmt.Sprintf("attempt %d/%d", e.Attempt, e.MaxAttempts), Err: e.Error})
	case "auto_retry_end":
		var e RetryEnd
		_ = json.Unmarshal(raw, &e)
		emit(harness.Event{Kind: harness.EventRetry, Text: fmt.Sprintf("attempt %d success=%v", e.Attempt, e.Success), Err: e.FinalError})

	case "summarization_retry_scheduled", "summarization_retry_attempt_start", "summarization_retry_finished":
		emit(harness.Event{Kind: harness.EventRetry, Text: env.Type})

	case "extension_error":
		var e ExtensionError
		_ = json.Unmarshal(raw, &e)
		msg := fmt.Sprintf("extension 错误（%s，事件 %s）: %s", e.ExtensionPath, e.Event, e.Error)
		if acc.err == "" {
			acc.err = msg
		}
		emit(harness.Event{Kind: harness.EventError, Err: msg})

	case "extension_ui_request":
		a.answerUI(env, raw, acc, emit)

	case "queue_update", "message_start", "message_end", "turn_start", "agent_start", "bash_execution_update":
		// 无需归一化动作：这些是内部生命周期，事件表里没有对应种类。
		// 显式列出而不是 default 吞掉，是为了让「新增事件」在 review 时可见。

	default:
		// 未知事件必须被忽略而不是让 reader 死掉：pi 的发布节奏是 1–2 周/版，
		// 版本漂移是已知风险。
	}
	_ = rs
}

// answerUI 应答 extension 的 UI 请求。
//
// 不答即挂死：dialog 类在 pi 侧 createDialogPromise 里阻塞等应答，而 pi 不会
// 自己退出（M0）。无头策略是「一律取消」——confirm 回 confirmed:false，
// select/input/editor 回 cancelled:true；fire-and-forget 类（notify/setStatus/
// setWidget/setTitle/set_editor_text）**不应答**（应答是多余的帧，且会污染
// pendingExtensionRequests 的语义）。
//
// 应答同时带 id 与 requestId 两个键：文档把 requestId 标为首选、id 标为 legacy
// 别名；pi 0.86.0 实际只认 id，两个都发零成本地对冲版本漂移。
func (a *Agent) answerUI(env Envelope, raw []byte, acc *roundAcc, emit func(harness.Event)) {
	var u UIRequest
	_ = json.Unmarshal(raw, &u)
	if u.ID == "" {
		u.ID = u.RequestID
	}
	if !u.IsDialog() {
		// fire-and-forget：只观测。
		emit(harness.Event{Kind: harness.EventUIRequest, Text: u.Method, Err: u.Title})
		return
	}
	body := map[string]any{
		"type":      CmdUIResp,
		"id":        u.ID,
		"requestId": u.ID,
	}
	if u.Method == "confirm" {
		body["confirmed"] = false
	} else {
		body["cancelled"] = true
	}
	b, err := json.Marshal(body)
	if err == nil {
		// 入队而不是直接写 stdin：这是 writer 死锁的唯一入口纪律。
		a.enqueueRaw(b)
	}
	acc.cancelledUI++
	// 被自动取消的对话框必须透出：它意味着 agent 索要输入却拿不到，该题结果的
	// 可解释性依赖这个事实（设计 §二）。
	emit(harness.Event{Kind: harness.EventUIRequest, Text: u.Method, Err: u.Title})
}

// enqueueRaw 把一条已序列化的帧交给唯一 writer。它只在 Round 协程里被调用，
// 所以这里可以安全地取 a.cur（进程不会在调用中途被换掉，除非 ensureProc 在
// 另一个 goroutine 里跑——那只有 Close/看门狗会做，而那时本轮已经在收尾）。
func (a *Agent) enqueueRaw(b []byte) {
	a.mu.Lock()
	p := a.cur
	a.mu.Unlock()
	if p == nil {
		return
	}
	p.q.pushRaw(b)
}

// ─────────────────────────── Steer / Stats / Close ───────────────────────────

// Steer 注入一条消息（hint / 剩余时间提醒）。
//
// 一律走 `steer` 命令：pi 的 steer 队列在下一轮 prompt 的开头被 drain（见
// agent-loop 的首次 getSteeringMessages 轮询），所以「空闲时发 steer」等价于
// 「下一条 prompt 之前插入这句话」——正是设计要的「hint 是方向线索，不替换当前
// 意图」。反过来，空闲时发 prompt 会立刻起一次 LLM 调用、脱离 DAG 渲染的意图，
// 既浪费一轮又污染上下文。
func (a *Agent) Steer(ctx context.Context, msg string) error {
	resp, err := a.call(ctx, Command{Type: CmdSteer, Msg: msg})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("piai: steer 失败: %s", resp.Error)
	}
	return nil
}

// Stats 取权威计数。Turns 用 toolCalls（与本地数 tool_execution_start 的语义
// 交叉校验；护栏仍依赖后者）。
func (a *Agent) Stats(ctx context.Context) (harness.Stats, error) {
	resp, err := a.call(ctx, Command{Type: CmdGetStats})
	if err != nil {
		return harness.Stats{}, err
	}
	if !resp.Success {
		return harness.Stats{}, errors.New(resp.Error)
	}
	var s SessionStats
	if err := json.Unmarshal(resp.Data, &s); err != nil {
		return harness.Stats{}, err
	}
	return harness.Stats{
		Turns:         s.ToolCalls,
		TokensIn:      s.Tokens.Input,
		TokensOut:     s.Tokens.Output,
		CacheRead:     s.Tokens.CacheRead,
		CacheWrite:    s.Tokens.CacheWrite,
		CostUSD:       s.Cost,
		SessionID:     s.SessionID,
		ContextTokens: s.ContextUsage.Tokens,
		ContextWindow: s.ContextUsage.ContextWindow,
	}, nil
}

// Close 整组终止。pi 不会自己退出（M0 实测：三个 spike 全靠外部整组终止），
// 所以这里必须显式 killpg，不能等它自己退。
func (a *Agent) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	p := a.cur
	a.cur = nil
	a.rs = nil
	a.mu.Unlock()
	if p == nil {
		return nil
	}
	// 先给一次 abort：还在 streaming 时它能让 pi 把已产出的内容落盘。
	actx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, _ = p.call(actx, Command{Type: CmdAbort})
	cancel()
	p.kill()
	return nil
}

// Version 返回 Start 时探测到的 pi 版本（未启动时为空）。
func (a *Agent) Version() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.version
}

// SessionID 返回当前 pi 会话 id（未启动时为空）。
func (a *Agent) SessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
