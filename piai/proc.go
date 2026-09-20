package piai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// 失效模式常量。它们会经 RoundResult.Err 透到 harness 的护栏里，
// 所以是**协议的一部分**，不要随手改字符串。
const (
	// ErrProcessDied：reader 读到 EOF / 进程退出。必须置非空——否则
	// 「0 回合 + 空错误」会与 provider 静默失败混淆，前身 280 run / 0 flag
	// 那次事故就是护栏失效导致的。
	ErrProcessDied = "pi_process_died"
	// ErrWedged：看门狗判定死锁（探活连续失败/超时），已 kill 重启。
	ErrWedged = "pi_wedged"
	// ErrSessionReset：new_session 后 get_state 断言失败（会话没复位）。
	ErrSessionReset = "pi_session_not_reset"
	// ErrOverflow：出站队列溢出——说明对端根本不读 stdin，属于 wedged 前兆。
	ErrOverflow = "pi_stdin_queue_overflow"
)

// queueMaxFrames 是出站队列上限。队列**无界**会让「对端不读 stdin」变成内存
// 无界增长；有界又要求入队方阻塞——而入队方常常是 reader 协程，阻塞它正是
// 我们要避免的死锁。折中：允许到 8192 帧（远超正常用量），溢出即判对端已死。
//
// 这个数**是拍的**（M10 实测后再定）：它只在「对端完全不读 stdin」时才会撞到。
// 注意 steer 的语义是「中途纠正方向」，不是数据通道；若将来有人拿它做高频注入，
// 该改的是设计而不是这个常量。
const queueMaxFrames = 8192

// DefaultBinPath 是 M0 实测的 bundled 路径。pi 不在非交互 shell 的 PATH 上，
// 所以必须有个已知兜底。
const DefaultBinPath = "/root/.local/share/pi-node/node-v22.23.2-linux-x64/bin/pi"

// EnvPiBin 是显式指定二进制路径的环境变量（优先级最高）。
const EnvPiBin = "REDCOPILOT_PI_BIN"

// VersionRange 是支持的 pi 版本区间（闭区间）。pi 的发布节奏是 1–2 周/版，
// RPC 是版本敏感面（前身被 0.74.2 咬过），所以启动前必须校验：不符就报配置错，
// 而不是在跑分中途以「未知命令」的形式炸掉。
type VersionRange struct {
	Min string
	Max string
}

// DefaultVersionRange：0.85.1 是前身的生产版本，0.86.0 是 M0 全量实测的版本。
//
// 上界**刻意只放到 0.86.99**：pi 的发布节奏是 1–2 周/版，而 0.87/0.88/0.89
// 一个都没跑过——把上界放到 0.89.x 的实质是「承认三个未验证的 minor 可以跑」，
// 那正是前身被 0.74.2 咬过的形状。跨 minor 必须重新验证后再放宽。
var DefaultVersionRange = VersionRange{Min: "0.85.0", Max: "0.86.99"}

// DiscoverBin 按 M0 确认的顺序发现 pi 二进制：
// REDCOPILOT_PI_BIN → exec.LookPath("pi") → bundled 已知路径。
func DiscoverBin(explicit string) (string, error) {
	if explicit != "" {
		if !fileExists(explicit) {
			return "", fmt.Errorf("piai: 指定的 pi 二进制不存在: %s", explicit)
		}
		return explicit, nil
	}
	if v := os.Getenv(EnvPiBin); v != "" {
		if !fileExists(v) {
			return "", fmt.Errorf("piai: %s=%s 指向的文件不存在", EnvPiBin, v)
		}
		return v, nil
	}
	if p, err := exec.LookPath("pi"); err == nil {
		return p, nil
	}
	if fileExists(DefaultBinPath) {
		return DefaultBinPath, nil
	}
	return "", fmt.Errorf("piai: 找不到 pi 二进制。请设置 %s 或把 pi 放进 PATH（已知 bundled 路径 %s 也不存在）",
		EnvPiBin, DefaultBinPath)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// parseVersion 只认 `X.Y.Z` 前缀（`pi --version` 输出恰好是 `0.86.0`）。
func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		// 容忍 `0.86.0-beta.1` 这类后缀。
		if j := strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }); j >= 0 {
			p = p[:j]
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func cmpVersion(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// checkVersion 跑 `pi --version` 并校验区间。它**必须**在子进程环境（PATH 已
// 前置 bundled node）下跑：pi 是 `#!/usr/bin/env node` 脚本，系统 node 是 v18，
// 直接用当前进程的 PATH 去跑会得到一个 node 语法错误而不是版本号。
func checkVersion(ctx context.Context, bin string, env []string, rng VersionRange) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "--version")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("piai: `%s --version` 失败（退出码 %d）: %s", bin, ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("piai: 无法执行 `%s --version`: %w", bin, err)
	}
	got := strings.TrimSpace(string(out))
	cur, ok := parseVersion(got)
	if !ok {
		return got, fmt.Errorf("piai: 无法解析 `%s --version` 的输出 %q", bin, got)
	}
	if rng.Min != "" {
		if min, ok := parseVersion(rng.Min); ok && cmpVersion(cur, min) < 0 {
			return got, versionMismatch(got, rng)
		}
	}
	if rng.Max != "" {
		if max, ok := parseVersion(rng.Max); ok && cmpVersion(cur, max) > 0 {
			return got, versionMismatch(got, rng)
		}
	}
	return got, nil
}

// versionMismatch 是一条**可操作**的配置错：跑分中途才炸的版本问题会烧掉整个
// 题库，所以要在启动前拦住，并告诉人怎么改。
func versionMismatch(got string, rng VersionRange) error {
	return fmt.Errorf("piai: pi 版本 %s 不在支持区间 [%s, %s]；RPC 协议是版本敏感面，请安装受支持的版本或显式设置 piai.Agent.VersionRange", got, rng.Min, rng.Max)
}

// ─────────────────────────── 出站队列 ───────────────────────────

// frameQueue 是**唯一**的通向 pi stdin 的入口。reader 协程只入队，绝不直接
// 写 stdin —— 这正是 writer 死锁的根治点：stdin 管道满时写会阻塞，若阻塞发生
// 在 reader 协程里，reader 就停止消费 stdout，pi 的 stdout 管道随之写满，双方
// 互等 ⇒ 永久挂死（M0 前身记录在案的事故）。
//
// 队列里存的是**已经序列化好的帧**，只有一条 FIFO：Command 与 UI 应答（带
// requestId 等结构体里没有的键）必须共用同一个顺序，abort 排在 prompt 之后才
// 有效。曾经把 Command 与 raw 分成两条切片，取的时候先取 Command 队列，于是
// UI 应答会被后面的 abort 插队——顺序一乱，协议就错了。
type frameQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	frames   [][]byte
	closed   bool
	overflow bool
	pushed   int
}

func newFrameQueue() *frameQueue {
	fq := &frameQueue{}
	fq.cond = sync.NewCond(&fq.mu)
	return fq
}

// push 永不阻塞（除非队列已满到上限，那时返回 false 让调用方把进程判死）。
// 序列化在**调用方**的栈上做，不占 writer 的时间。
func (q *frameQueue) push(c Command) bool {
	b, err := json.Marshal(c)
	if err != nil {
		return false
	}
	return q.pushRaw(b)
}

// pop 返回一条待写出的原始帧（已带结尾换行）。队列空且未关闭时阻塞——
// 它是 writer 协程的主循环，阻塞在这里是正常的。
func (q *frameQueue) pop() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.frames) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.frames) == 0 {
		return nil, false
	}
	b := q.frames[0]
	q.frames = q.frames[1:]
	return b, true
}

// pushRaw 接受一条**已经序列化**的帧（UI 应答走这条：它带 requestId 等
// 结构体里没有的键）。它同时是 push 的底层实现，保证两条路径共用同一个顺序。
func (q *frameQueue) pushRaw(b []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	if len(q.frames) >= queueMaxFrames {
		q.overflow = true
		return false
	}
	q.frames = append(q.frames, append(append([]byte(nil), b...), '\n'))
	q.pushed++
	q.cond.Signal()
	return true
}

func (q *frameQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

func (q *frameQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.frames)
}

func (q *frameQueue) didOverflow() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.overflow
}

// ─────────────────────────── 进程 ───────────────────────────

type procConfig struct {
	bin  string
	args []string
	env  []string
	dir  string
	// 回调在**启动 goroutine 之前**装好。曾经的写法是 startProc 返回后再赋值，
	// 那留下一个窗口：reader 若在这个窗口里读到一帧，onFrame 还是 nil，帧被静默
	// 丢掉。丢一条 extension_ui_request 就是 pi 侧永久挂死（它阻塞等应答），
	// 所以这个窗口必须消掉。
	onFrame func(env Envelope, raw []byte)
	onDeath func(msg string)
}

// proc 是一个 pi 进程 + 它的 reader/writer 协程。它不认识协议语义（那是
// Agent 的事），只负责：把帧送出去、把帧收进来、死的时候说清楚。
type proc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	q     *frameQueue
	out   *FrameReader

	waitOnce sync.Once
	waitErr  error
	waitDone chan struct{}

	mu         sync.Mutex
	dead       bool
	deathMsg   string
	stderrTail string
	pending    map[string]chan Response
	seq        int

	// 回调，均由 reader 协程调用；实现方自己保证不阻塞。
	onFrame func(env Envelope, raw []byte)
	onDeath func(msg string)

	// 诊断用：writer 最后一次成功写出字节的时间。
	lastWrite atomic.Int64
}

func startProc(cfg procConfig) (*proc, error) {
	cmd := exec.Command(cfg.bin, cfg.args...)
	cmd.Dir = cfg.dir
	cmd.Env = cfg.env
	// 独立进程组：pi 会 spawn bundled node 子进程，只 kill 父进程会留下孤儿
	// （前身 _stop_process_tree 的教训）。整组 kill 靠 Setpgid。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// stderr 单独收：pi 的启动期错误（版本、凭据、extension 加载失败）几乎都
	// 只出现在 stderr，丢掉它会让「起不来」变成无从下手。
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("piai: 启动 %s 失败: %w", cfg.bin, err)
	}
	p := &proc{
		cmd:      cmd,
		stdin:    stdin,
		q:        newFrameQueue(),
		out:      NewFrameReader(stdout),
		waitDone: make(chan struct{}),
		pending:  map[string]chan Response{},
		onFrame:  cfg.onFrame,
		onDeath:  cfg.onDeath,
	}
	p.lastWrite.Store(time.Now().UnixNano())
	go p.readStderr(stderr)
	go p.writeLoop()
	go p.readLoop()
	go func() {
		p.waitErr = cmd.Wait()
		close(p.waitDone)
	}()
	return p, nil
}

func (p *proc) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8<<10), 1<<20)
	for sc.Scan() {
		// 只保留最后一小段：pi 的启动报错足够，长 stderr 会淹掉真正的死因。
		p.mu.Lock()
		line := sc.Text()
		if len(p.stderrTail) > 8192 {
			p.stderrTail = p.stderrTail[len(p.stderrTail)-4096:]
		}
		p.stderrTail += line + "\n"
		p.mu.Unlock()
	}
}

func (p *proc) stderr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.TrimSpace(p.stderrTail)
}

// writeLoop 是**唯一**写 stdin 的协程。
func (p *proc) writeLoop() {
	for {
		b, ok := p.q.pop()
		if !ok {
			return
		}
		if _, err := p.stdin.Write(b); err != nil {
			// 写失败 = 对端已死（EPIPE）。reader 那边也会看到 EOF，两边都走
			// markDead，由 markDead 保证只触发一次。
			p.markDead(ErrProcessDied, err.Error())
			return
		}
		p.lastWrite.Store(time.Now().UnixNano())
	}
}

// readLoop 是 reader 协程：只解析与分发，**绝不**内联写 stdin。
func (p *proc) readLoop() {
	for {
		raw, ok, err := p.out.Next()
		if !ok {
			msg := ErrProcessDied
			if err != nil {
				msg = ErrProcessDied + ": " + err.Error()
			}
			p.markDead(ErrProcessDied, msg)
			return
		}
		if len(raw) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			// 单帧解析失败不能杀死 reader：pi 版本漂移时会有我们没见过的帧，
			// 而且 stderr/日志混入 stdout 也偶有发生。跳过并继续。
			continue
		}
		if env.Type == "response" {
			p.deliver(raw, env)
			continue
		}
		if p.onFrame != nil {
			p.onFrame(env, raw)
		}
	}
}

func (p *proc) deliver(raw []byte, env Envelope) {
	var r Response
	if err := json.Unmarshal(raw, &r); err != nil {
		return
	}
	p.mu.Lock()
	ch := p.pending[r.ID]
	delete(p.pending, r.ID)
	p.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

// markDead 幂等：reader 与 writer 都可能先发现对端死亡。
func (p *proc) markDead(kind, msg string) {
	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		return
	}
	p.dead = true
	if p.deathMsg == "" {
		p.deathMsg = msg
	}
	p.deathMsg = kind + ": " + p.deathMsg
	pend := p.pending
	p.pending = map[string]chan Response{}
	p.mu.Unlock()

	// 唤醒所有等在应答上的调用方。
	for id, ch := range pend {
		close(ch)
		_ = id
	}
	p.q.close()
	if p.onDeath != nil {
		p.onDeath(p.deathMsg)
	}
}

func (p *proc) isDead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dead
}

// died 返回一个在进程死亡时关闭的 channel（惰性建，够用即可）。
func (p *proc) died() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitDone
}

// send 只入队，不等待应答。UI 应答与 steer 走这里。
func (p *proc) send(c Command) error {
	if p.isDead() {
		return errors.New(p.death())
	}
	if !p.q.push(c) {
		p.markDead(ErrOverflow, "出站队列溢出：对端未读取 stdin")
		return errors.New(ErrOverflow)
	}
	return nil
}

// call 入队并等应答。返回的 error 只会是 ctx / 进程死亡 / pi 报错。
func (p *proc) call(ctx context.Context, c Command) (Response, error) {
	if p.isDead() {
		return Response{}, errors.New(p.death())
	}
	p.mu.Lock()
	p.seq++
	c.ID = "piai-" + strconv.Itoa(p.seq)
	ch := make(chan Response, 1)
	p.pending[c.ID] = ch
	p.mu.Unlock()

	if !p.q.push(c) {
		p.mu.Lock()
		delete(p.pending, c.ID)
		p.mu.Unlock()
		p.markDead(ErrOverflow, "出站队列溢出：对端未读取 stdin")
		return Response{}, errors.New(ErrOverflow)
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return Response{}, errors.New(p.death())
		}
		return r, nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, c.ID)
		p.mu.Unlock()
		return Response{}, ctx.Err()
	case <-p.waitDone:
		p.mu.Lock()
		delete(p.pending, c.ID)
		p.mu.Unlock()
		return Response{}, errors.New(p.death())
	}
}

func (p *proc) death() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deathMsg != "" {
		return p.deathMsg
	}
	if p.waitErr != nil {
		return ErrProcessDied + ": " + p.waitErr.Error()
	}
	return ErrProcessDied
}

// kill 整组终止。pi 不会自己退出（M0 实测：三个 spike 全靠外部整组终止），
// 所以这是唯一可靠的收尾方式。
func (p *proc) kill() {
	if p.cmd.Process == nil {
		return
	}
	// 负 pid = 整个进程组。Setpgid 让 pi 及其 bundled node 子进程同组。
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	_ = p.cmd.Process.Kill()
	p.q.close()
	_ = p.stdin.Close()
	select {
	case <-p.waitDone:
	case <-time.After(5 * time.Second):
	}
}

// wait 等到进程真的退出（带超时）。
func (p *proc) wait(d time.Duration) bool {
	select {
	case <-p.waitDone:
		return true
	case <-time.After(d):
		return false
	}
}

// ─────────────────────────── 命令行 ───────────────────────────

// buildArgs 按 M0 确认的形状拼命令行。顺序有意固定，便于在日志里逐字比对
// （前身出问题时就是靠这一行定位的）。
func buildArgs(sessionDir string, exts []string, provider, model, thinking, sysPrompt string, approve bool) []string {
	args := []string{"--mode", "rpc"}
	if sessionDir != "" {
		// 不用 --no-session：get_entries 游标与 sessionFile 都需要会话文件。
		args = append(args, "--session-dir", sessionDir)
	}
	if provider != "" {
		args = append(args, "--provider", provider)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	for _, e := range exts {
		if e == "" {
			continue
		}
		// 必须是绝对路径：非交互模式下项目本地资源默认被忽略。
		if !filepath.IsAbs(e) {
			if abs, err := filepath.Abs(e); err == nil {
				e = abs
			}
		}
		args = append(args, "-e", e)
	}
	if approve {
		// 不开 --approve 时项目资源会被**静默**忽略——这是会悄悄失效的坑。
		args = append(args, "--approve")
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	if sysPrompt != "" {
		args = append(args, "--append-system-prompt", sysPrompt)
	}
	return args
}
