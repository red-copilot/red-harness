package bridge

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
	"syscall"
	"time"
)

// stderrTailMax 是内存里保留的 stderr 尾部上限。
//
// 保留尾部而不是全部：启动期报错（`ModuleNotFoundError: No module named
// 'httpx'`、容器不存在）足够诊断，而长 stderr 会把真正的死因淹掉。完整记录
// 在 private/ 的日志文件里，内存这份只用于错误消息。
const stderrTailMax = 8192

// proc 是一个 bridge.py 子进程 + 它的 reader/writer 协程。
//
// 它不认识平台语义（那是 client.go 的事），只负责三件事：
//  1. 把请求行写进 stdin；
//  2. 按 `id` 把响应行投递给等待的调用方；
//  3. 死的时候说清楚，并唤醒所有等待者。
type proc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Scanner

	// stderrLog 是子进程 stderr 的落盘目标。**它绝不能是宿主 stderr**：
	// 桥的 stderr 里会有 Python traceback，而 traceback 里可能有 base_url、
	// 请求 URL 与 token 片段。按 §3.3 的明文纪律，这些只允许进 private/。
	stderrLog *os.File

	waitOnce sync.Once
	waitErr  error
	waitDone chan struct{}

	mu         sync.Mutex
	dead       bool
	deathMsg   string
	stderrTail string
	pending    map[string]chan response

	onDeath func(msg string)

	// firstCh 在读到**第一行**时关闭，firstRaw 是那一行的原文。
	//
	// 为什么需要它：桥启动时先打一行握手（`{"event":"hello",...}`），Go 侧
	// 的启动探活必须等它。握手行没有 `id`，与普通响应共用一个读协程（协议
	// 只有一条读路径，握手不可能因为某次调用而漏读），所以由读协程在解析前
	// 顺手记下第一行。
	firstOnce sync.Once
	firstRaw  []byte
	firstCh   chan struct{}
}

// procConfig 是启动一个桥进程所需的一切。
type procConfig struct {
	// argv 是完整命令行（argv[0] 是程序本身）。**由 Client 组装**，proc 不
	// 关心它是 `python3 /path/bridge.py` 还是 `docker exec x python3 -m bridge`。
	argv []string
	// dir 是子进程的工作目录。空表示继承。
	dir string
	// env 是子进程环境（已含 token 等凭据）。**proc 从不打印它。**
	env []string
	// stderrPath 是子进程 stderr 的落盘文件（private/ 下）。空表示丢弃。
	stderrPath string
	onDeath    func(msg string)
}

// startProc 起进程并装上 reader/writer。
func startProc(cfg procConfig) (*proc, error) {
	if len(cfg.argv) == 0 {
		return nil, errors.New("bridge: 空的桥命令")
	}
	cmd := exec.Command(cfg.argv[0], cfg.argv[1:]...)
	cmd.Dir = cfg.dir
	cmd.Env = cfg.env
	// 独立进程组：桥命令可能是 `docker exec ...` 或 shell 包装，只 kill 直接
	// 子进程会留下孤儿（前身 `_stop_process_tree` 的教训）。整组 kill 靠 Setpgid。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("bridge: 取 stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("bridge: 取 stdout 管道失败: %w", err)
	}

	// stderr 落盘。打不开就丢弃——**绝不回退到宿主 stderr**，那正是明文
	// 泄漏到普通日志的那条路径。
	var stderrLog *os.File
	if cfg.stderrPath != "" {
		if f, err := os.OpenFile(cfg.stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			stderrLog = f
		} else {
			return nil, fmt.Errorf("bridge: 打不开桥 stderr 日志 %s: %w", cfg.stderrPath, err)
		}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("bridge: 取 stderr 管道失败: %w", err)
	}

	if err := cmd.Start(); err != nil {
		if stderrLog != nil {
			_ = stderrLog.Close()
		}
		return nil, fmt.Errorf("bridge: 启动桥进程失败 (%s): %w", cfg.argv[0], err)
	}

	p := &proc{
		cmd:       cmd,
		stdin:     stdin,
		out:       bufio.NewScanner(stdout),
		stderrLog: stderrLog,
		waitDone:  make(chan struct{}),
		firstCh:   make(chan struct{}),
		pending:   map[string]chan response{},
		onDeath:   cfg.onDeath,
	}
	// 协议是 JSONL，一行一个对象。Go 的 Scanner 默认 64KiB 上限——`list` 在
	// 题目多的时候会超，那会让 Scanner 报 ErrTooLong 并**静默停止读行**，
	// 表现为「桥卡住不回复」。抬到 16MiB 并显式处理 ErrTooLong（见 readLoop）。
	p.out.Buffer(make([]byte, 0, 64<<10), 16<<20)

	go p.readStderr(stderrPipe)
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
		line := sc.Text()
		p.mu.Lock()
		if len(p.stderrTail) > stderrTailMax {
			p.stderrTail = p.stderrTail[len(p.stderrTail)-stderrTailMax/2:]
		}
		p.stderrTail += line + "\n"
		p.mu.Unlock()
		if p.stderrLog != nil {
			_, _ = p.stderrLog.WriteString(line + "\n")
		}
	}
}

// stderr 返回 stderr 尾部（用于拼错误消息）。**只含 Python 侧的输出**，
// 桥自己从不往 stderr 写凭据。
func (p *proc) stderr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.TrimSpace(p.stderrTail)
}

// readLoop 是唯一的读协程：解析一行、按 id 投递。
//
// **握手行与响应行走同一条路**：桥先打一行 `{"event":"hello",...}`，之后
// 才是一行一个响应。让它们共用一条读路径是刻意的——协议只有一个读者，
// 握手不可能因为某次调用而漏读。
func (p *proc) readLoop() {
	for p.out.Scan() {
		raw := p.out.Bytes()
		if len(raw) == 0 {
			continue
		}
		// 第一行可能是握手行（没有 id，只有 event）。**只在第一行做这个判定**：
		// 之后的行若带 event 就是协议污染（例如有人误调了
		// `tsec_benchmark._cli.main`，它往 stdout 打表格），跳过即可。
		p.noteFirst(raw)
		var resp response
		if err := json.Unmarshal(raw, &resp); err != nil {
			// 单行解析失败不能杀死 reader：桥侧可能因为版本漂移回了非协议行。
			// 跳过并继续——把整条桥判死会让一次格式抖动变成一次崩溃重启。
			continue
		}
		if resp.ID == "" && resp.Result == nil && resp.Error == nil {
			continue // 握手行
		}
		p.deliver(resp)
	}
	// Scan 返回 false：要么 EOF（进程退出），要么行超长。
	msg := CodeSubprocessExit
	if err := p.out.Err(); err != nil {
		// ErrTooLong 是**我们的**问题（上限太小），不是对端死了。分开报，
		// 否则会把「响应太大」误诊成「桥崩了」并触发无意义的重启。
		msg = CodeProtocolError + ": " + err.Error()
	}
	p.markDead(msg)
}

// noteFirst 记下第一行原文并放行等待启动握手的调用方。幂等。
func (p *proc) noteFirst(raw []byte) {
	p.firstOnce.Do(func() {
		p.mu.Lock()
		p.firstRaw = append([]byte(nil), raw...)
		p.mu.Unlock()
		close(p.firstCh)
	})
}

// firstLine 返回第一行原文（未读到或进程已死时返回空）。
func (p *proc) firstLine() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstRaw
}

func (p *proc) deliver(resp response) {
	p.mu.Lock()
	ch := p.pending[resp.ID]
	delete(p.pending, resp.ID)
	p.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
}

// markDead 幂等：reader 与 writer 都可能先发现对端死亡。
func (p *proc) markDead(msg string) {
	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		return
	}
	p.dead = true
	p.deathMsg = msg
	pend := p.pending
	p.pending = map[string]chan response{}
	p.mu.Unlock()

	// 唤醒所有等待者（关闭 channel，调用方据此判死）。
	for _, ch := range pend {
		close(ch)
	}
	if p.onDeath != nil {
		p.onDeath(msg)
	}
}

func (p *proc) isDead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dead
}

// call 发一条请求并等响应。ctx 到期返回可重试的 KindPlatform 错误。
//
// 注意返回值里的 error **不是**平台业务错误——平台错误在 response.Error 里。
// 这里的 error 只表示「这条桥本身没能给出答案」（进程死了、超时、协议坏了）。
func (p *proc) call(ctx context.Context, req request) (response, error) {
	p.mu.Lock()
	if p.dead {
		msg := p.deathMsg
		p.mu.Unlock()
		return response{}, fmt.Errorf("bridge: 桥进程已死 (%s): %s", msg, p.stderr())
	}
	ch := make(chan response, 1)
	p.pending[req.ID] = ch
	p.mu.Unlock()

	line, err := json.Marshal(req)
	if err != nil {
		p.mu.Lock()
		delete(p.pending, req.ID)
		p.mu.Unlock()
		return response{}, fmt.Errorf("bridge: 序列化请求失败: %w", err)
	}
	line = append(line, '\n')

	// 写与等分开：写失败只说明对端已死，仍然要等 markDead 唤醒我们，
	// 否则会在写失败路径上泄漏一个 pending 表项。
	if _, err := p.stdin.Write(line); err != nil {
		p.markDead(CodeSubprocessExit + ": " + err.Error())
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return response{}, fmt.Errorf("bridge: 桥进程在应答前退出 (%s): %s", p.deathMsgLocked(), p.stderr())
		}
		return resp, nil
	case <-ctx.Done():
		// 超时/取消：把这条 pending 摘掉（迟到的响应会被丢弃，不能投给下一个
		// 复用同一个 id 的调用方），并按 ctx 的类别报错。
		p.mu.Lock()
		delete(p.pending, req.ID)
		p.mu.Unlock()
		return response{}, ctx.Err()
	}
}

func (p *proc) deathMsgLocked() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deathMsg
}

// kill 杀掉整个进程组并等它退干净。
//
// 为什么必须等：`close` 之后立刻重启会在端口上短暂出现两个桥进程，两者都
// 连着同一个平台 token，平台侧会看到并发的 `start`/`submit`。
func (p *proc) kill() {
	if p.cmd.Process != nil {
		// 负号 = 整个进程组。
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	_ = p.stdin.Close()
	p.markDead(CodeSubprocessExit + ": killed by caller")
	select {
	case <-p.waitDone:
	case <-time.After(5 * time.Second):
	}
	if p.stderrLog != nil {
		_ = p.stderrLog.Close()
	}
}

// restartBackoff 是重启前的等待。首次立刻重试（可能是启动竞态），之后指数
// 退避到上限——桥起不来通常是环境问题（容器没了、依赖缺），密集重试只会
// 把日志刷满并掩盖真因。
func restartBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	d := time.Duration(1<<uint(attempt-2)) * 250 * time.Millisecond
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

// privateStderrPath 在 private 目录下给桥的 stderr 找一个落点。
//
// 目录权限 0700、文件 0600（§3.3）。目录不存在时**不创建**——private/ 的
// 生命周期属于 store，桥只是往里写；盲目创建会在错误的路径上留垃圾。
func privateStderrPath(dir string) string {
	if dir == "" {
		return ""
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Join(dir, "bridge-stderr.log")
}

// waitExit 等进程退出（带超时），供 doctor 的探活使用。
func (p *proc) waitExit(d time.Duration) bool {
	select {
	case <-p.waitDone:
		return true
	case <-time.After(d):
		return false
	}
}

// exitCode 返回进程退出码（进程未退出时返回 -1）。
func (p *proc) exitCode() int {
	select {
	case <-p.waitDone:
	default:
		return -1
	}
	if p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}

// nextID 生成请求 id。用「前缀 + 递增序号」而不是 UUID：序号在日志里可读，
// 而且**同一个 Client 实例内的 id 绝不重复**——迟到响应被丢弃的前提。
func nextID(prefix string, n int) string {
	return prefix + strconv.Itoa(n)
}
