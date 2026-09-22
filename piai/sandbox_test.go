package piai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// ─────────────────────── 手写假 sandbox ───────────────────────
//
// 这一组 fake 存在的理由：v0.4 的验收门之一是「契约测试证明生产 piai 无宿主
// 进程启动路径」。证明它的唯一办法是给 Agent 一个**记录型**的 SandboxSession，
// 然后断言：Launch 被调用了几次、ProcessSpec 的每个字段是什么、宿主上有没有
// 任何东西被执行。假进程用 os.Pipe 做 stdio（真管道，不是内存 buffer），
// 因为 piai 的 reader/writer 协程是按真管道的阻塞语义写的。

// fakePi 是一个**进程内**的极简 pi 协议对端：够跑通 Start 握手（new_session +
// get_state 断言）与一轮 prompt（事件流 + agent_settled）。
//
// 它刻意不复用 testdata/stubpi：stubpi 是**独立进程**，把它放进容器路径需要
// Docker（属于集成测试）。这里要证的是「谁启动进程」这一层的契约，不是协议
// 解析——协议解析已经由 agent_test.go 里的真 stubpi 钉住了。
type fakePi struct {
	mu      sync.Mutex
	w       *os.File // 写回 agent（agent 从另一头读）
	gen     int
	session string
	// commands 是 get_commands 的返回。exts 非空时给一条 source=="extension"，
	// 让 Start 的 extension 自检能过。
	exts []string
	// prompts 收到的 prompt 条数。
	prompts int
	// gotCommands 是收到的命令类型序列（断言用）。
	got []string
}

func newFakePi(w *os.File) *fakePi {
	return &fakePi{w: w, session: "sess-0001"}
}

func (p *fakePi) send(obj map[string]any) {
	b, _ := json.Marshal(obj)
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.w.Write(append(b, '\n'))
}

func (p *fakePi) respond(id, command string, data any) {
	obj := map[string]any{"type": "response", "command": command, "success": true, "id": id}
	if data != nil {
		obj["data"] = data
	}
	p.send(obj)
}

// serve 是读循环。r 是 agent 写进来的那一头；EOF 时退出。
func (p *fakePi) serve(r *os.File) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var cmd map[string]any
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			continue
		}
		typ, _ := cmd["type"].(string)
		id, _ := cmd["id"].(string)
		p.mu.Lock()
		p.got = append(p.got, typ)
		p.mu.Unlock()
		switch typ {
		case "new_session":
			p.mu.Lock()
			p.gen++
			p.session = fmt.Sprintf("sess-%04d", p.gen+1)
			p.mu.Unlock()
			p.respond(id, typ, map[string]any{"cancelled": false})
		case "get_state":
			p.mu.Lock()
			sid := p.session
			p.mu.Unlock()
			p.respond(id, typ, map[string]any{
				"sessionId": sid, "sessionFile": "/work/.pi-sessions/" + sid + ".jsonl",
				"isStreaming": false, "isCompacting": false, "messageCount": 0,
				"pendingMessageCount": 0, "thinkingLevel": "medium",
			})
		case "get_commands":
			cmds := []any{}
			for _, e := range p.exts {
				cmds = append(cmds, map[string]any{"name": "ext", "description": "d",
					"source": "extension", "sourceInfo": map[string]any{"path": e}})
			}
			p.respond(id, typ, map[string]any{"commands": cmds})
		case "prompt":
			p.mu.Lock()
			p.prompts++
			p.mu.Unlock()
			p.respond(id, typ, nil)
			go p.turn()
		case "steer", "abort":
			p.respond(id, typ, nil)
		default:
			p.send(map[string]any{"type": "response", "command": typ, "success": false,
				"id": id, "error": "Unknown command: " + typ})
		}
	}
}

// turn 演一轮：文本增量 → 工具调用 → settled。
func (p *fakePi) turn() {
	p.send(map[string]any{"type": "agent_start"})
	p.send(map[string]any{"type": "turn_start"})
	p.send(map[string]any{"type": "message_update",
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "沙箱内文本。"}})
	p.send(map[string]any{"type": "tool_execution_start", "toolCallId": "call_s1", "toolName": "bash",
		"args": map[string]any{"command": "id"}})
	p.send(map[string]any{"type": "tool_execution_end", "toolCallId": "call_s1", "toolName": "bash",
		"isError": false, "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "uid=65534(nobody)\n"}},
			"details": map[string]any{"report_fact": map[string]any{"next": "试试 /admin"}}}})
	p.send(map[string]any{"type": "turn_end", "message": map[string]any{"role": "assistant", "stopReason": "stop"}})
	p.send(map[string]any{"type": "agent_settled"})
}

func (p *fakePi) commandTypes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got...)
}

func (p *fakePi) promptCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prompts
}

// fakeManaged 是附着式假进程。它**没有**宿主子进程：两端都是 os.Pipe，
// Wait 在 Kill 之前一直阻塞——这正是 sandbox 路径下 Agent.Close 必须走 Kill
// 的证据（v0.4：停止容器即可靠终止 pi 及工具子进程）。
type fakeManaged struct {
	r *os.File
	w *os.File

	mu       sync.Mutex
	kills    int
	closed   bool
	waitDone chan struct{}
}

func newFakeManaged() (*fakeManaged, *os.File, *os.File) {
	// agent 读 rOut；peer 写 wOut。
	rOut, wOut, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	// peer 读 rIn；agent 写 wIn。
	rIn, wIn, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	m := &fakeManaged{r: rOut, w: wIn, waitDone: make(chan struct{})}
	return m, wOut, rIn
}

func (m *fakeManaged) Read(b []byte) (int, error)  { return m.r.Read(b) }
func (m *fakeManaged) Write(b []byte) (int, error) { return m.w.Write(b) }
func (m *fakeManaged) Close() error                { return m.Kill() }

func (m *fakeManaged) Wait() error {
	<-m.waitDone
	return nil
}

func (m *fakeManaged) Kill() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.waitDone)
	}
	m.kills++
	m.mu.Unlock()
	// 关管道让 reader 协程也醒来（真实实现里是 docker stop，容器一停
	// attach 的 stdout 就到 EOF）。
	_ = m.r.Close()
	_ = m.w.Close()
	return nil
}

func (m *fakeManaged) killCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kills
}

// fakeSession 是记录型 SandboxSession。
type fakeSession struct {
	mu sync.Mutex
	// probeResult 是 Probe 的返回。Workdir 是**容器内**工作目录，Agent 拿它当
	// --session-dir / HOME / ProcessSpec.Workdir 的基准。
	probeResult harness.ProbeResult
	probeErr    error
	probeCalls  int

	launchErr  error
	launches   []harness.ProcessSpec
	lastProc   *fakeManaged
	lastPeer   *fakePi
	closeCalls int
	// peerHook 在 Launch 造出假对端、开始 serve **之前**调用一次，用来预置
	// 场景（例如 get_commands 要返回的扩展列表）。
	peerHook func(*fakePi)
}

var _ harness.SandboxSession = (*fakeSession)(nil)

func (s *fakeSession) Probe(ctx context.Context) (harness.ProbeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeCalls++
	if s.probeErr != nil {
		return harness.ProbeResult{}, s.probeErr
	}
	return s.probeResult, nil
}

func (s *fakeSession) Launch(ctx context.Context, ps harness.ProcessSpec) (harness.ManagedProcess, error) {
	s.mu.Lock()
	s.launches = append(s.launches, ps)
	err := s.launchErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	m, wOut, rIn := newFakeManaged()
	peer := newFakePi(wOut)
	if s.peerHook != nil {
		s.peerHook(peer)
	}
	go peer.serve(rIn)
	s.mu.Lock()
	s.lastProc, s.lastPeer = m, peer
	s.mu.Unlock()
	return m, nil
}

func (s *fakeSession) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	return nil
}

func (s *fakeSession) launchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.launches)
}

func (s *fakeSession) spec(t *testing.T, i int) harness.ProcessSpec {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.launches) {
		t.Fatalf("Launch 只被调用了 %d 次，取不到第 %d 个 ProcessSpec", len(s.launches), i)
	}
	return s.launches[i]
}

// recSink 是记录型 EventSink。
type recSink struct {
	mu     sync.Mutex
	events []harness.Event
}

func (s *recSink) Emit(e harness.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *recSink) kinds() []harness.EventKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]harness.EventKind, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Kind)
	}
	return out
}

func (s *recSink) count(k harness.EventKind) int {
	n := 0
	for _, got := range s.kinds() {
		if got == k {
			n++
		}
	}
	return n
}

// newSandboxAgent 造一个走 sandbox 路径的 Agent。宿主 Workdir 是临时目录，
// 容器工作目录由假 Probe 报出来（缺省 /work）。
func newSandboxAgent(t *testing.T, containerWorkdir string) (*Agent, *fakeSession) {
	t.Helper()
	hostDir := t.TempDir()
	envFile := filepath.Join(hostDir, ".env")
	// 明显的假值：凭据纪律要求测试夹具里不能出现任何真实 key。
	body := "OPENCODE_API_KEY=test-key-not-real\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// 宿主进程环境里若真有同名变量，先清掉，避免测试断言依赖宿主状态。
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv(EnvPiBin, "")

	if containerWorkdir == "" {
		containerWorkdir = defaultContainerWorkdir
	}
	sess := &fakeSession{probeResult: harness.ProbeResult{
		ContainerID: "fake-container-id", Image: "red-harness-runner:v0.3.0", Workdir: containerWorkdir, PiVersion: "0.86.0"}}
	a := &Agent{
		Session:      sess,
		EnvFile:      envFile,
		Workdir:      hostDir,
		Provider:     "opencode-go",
		Model:        "deepseek-v4-flash",
		StallTimeout: 5 * time.Second,
		ProbeTimeout: 2 * time.Second,
		AbortGrace:   500 * time.Millisecond,
	}
	return a, sess
}

func startSandboxAgent(t *testing.T, a *Agent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.Start(ctx, harness.AgentStart{Workdir: a.Workdir}); err != nil {
		t.Fatalf("sandbox 路径 Start 失败: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
}

func envOf(env map[string]string, k string) string { return env[k] }

// ─────────────────────── 生产路径：无宿主进程 ───────────────────────

// TestSandboxLaunchNeverStartsHostProcess 是 v0.4 验收门「契约测试证明生产
// piai 无宿主进程启动路径」的直接证据。
//
// 做法：把一个**可执行脚本**放在宿主上，任何一次宿主 exec 都会留下标记文件；
// 同时把 REDCOPILOT_PI_BIN 指向一个不存在的路径（DiscoverBin 被调用就会报错）。
// Start 与 Round 都必须成功，而标记文件必须不存在。
func TestSandboxLaunchNeverStartsHostProcess(t *testing.T) {
	hostDir := t.TempDir()
	marker := filepath.Join(hostDir, "host-exec-happened")
	hostBin := filepath.Join(hostDir, "host-pi-must-not-run")
	script := "#!/bin/sh\ntouch " + marker + "\necho 0.86.0\n"
	if err := os.WriteFile(hostBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPiBin, filepath.Join(hostDir, "pi-via-env-does-not-exist"))

	a, sess := newSandboxAgent(t, "")
	a.BinPath = hostBin
	startSandboxAgent(t, a)

	if _, err := a.Round(context.Background(), "侦察", nil); err != nil {
		t.Fatalf("Round 失败: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("宿主上的 pi 被启动了：sandbox 路径绝不允许宿主 exec.Command(pi)")
	}
	if sess.launchCount() != 1 {
		t.Fatalf("Launch 调用次数 = %d，期望 1", sess.launchCount())
	}
	// DiscoverBin 若被调用，BinPath 不存在会直接报错；走到这里说明它没被调用。
	if a.BinPath != hostBin {
		t.Errorf("BinPath 被改写成 %q：sandbox 路径不该做宿主二进制发现", a.BinPath)
	}
}

// TestSandboxProcessSpecFields 逐字段断言 ProcessSpec：Command / Env / Workdir
// 三者都必须是**容器内**语义。
func TestSandboxProcessSpecFields(t *testing.T) {
	a, sess := newSandboxAgent(t, "")
	// BinPath 留空：容器里应当用 PATH 上的 "pi"。
	a.BinPath = ""
	a.Extensions = nil
	a.Approve = true
	startSandboxAgent(t, a)

	spec := sess.spec(t, 0)

	// ── Command ──
	if len(spec.Command) == 0 {
		t.Fatal("Command 为空")
	}
	if spec.Command[0] != "pi" {
		t.Errorf("argv[0] = %q，期望容器 PATH 上的 \"pi\"（宿主绝对路径在容器里不存在）", spec.Command[0])
	}
	wantArgs := buildArgs(a.sessionDir(), nil, a.Provider, a.Model, a.Thinking, a.SystemPrompt, a.Approve)
	if len(spec.Command) != len(wantArgs)+1 {
		t.Fatalf("argv 长度 = %d，期望 %d: %v", len(spec.Command), len(wantArgs)+1, spec.Command)
	}
	for i := range wantArgs {
		if spec.Command[i+1] != wantArgs[i] {
			t.Errorf("argv[%d] = %q，期望 %q", i+1, spec.Command[i+1], wantArgs[i])
		}
	}
	// --session-dir 必须是容器内路径：宿主路径在容器里不存在，pi 会静默落到
	// 别处（或直接失败），而 get_entries 游标与 sessionFile 都指着它。
	sd := argValue(spec.Command, "--session-dir")
	if sd != "/work/.pi-sessions" {
		t.Errorf("--session-dir = %q，期望容器内路径 /work/.pi-sessions", sd)
	}
	if filepath.IsAbs(sd) && strings.HasPrefix(sd, a.Workdir) {
		t.Errorf("--session-dir 落到了宿主工作目录里: %q", sd)
	}
	// 宿主上**不能**出现这个目录：容器里的 tmpfs 路径不该被宿主代码创建。
	if _, err := os.Stat(filepath.Join(a.Workdir, ".pi-sessions")); err == nil {
		t.Error("宿主上出现了 .pi-sessions：sandbox 路径不该用宿主路径")
	}

	// ── Env ──
	if got := envOf(spec.Env, "PATH"); got != "/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("PATH = %q，期望镜像内固定 PATH（宿主 PATH 在容器里没有意义）", got)
	}
	if got := envOf(spec.Env, "HOME"); got != "/work/.home" {
		t.Errorf("HOME = %q，期望容器内可写 HOME /work/.home（只读 rootfs 下宿主 HOME 写不了）", got)
	}
	if got := envOf(spec.Env, "OPENCODE_API_KEY"); got != "test-key-not-real" {
		t.Errorf("provider 凭据没有经环境变量传递: %q", got)
	}

	// ── 凭据绝不进命令行（ps 可见 argv 是硬规矩）──
	for _, arg := range spec.Command {
		if strings.Contains(arg, "test-key-not-real") {
			t.Fatalf("凭据出现在 argv 里: %q", arg)
		}
	}

	// ── Workdir ──
	if spec.Workdir != "/work" {
		t.Errorf("Workdir = %q，期望容器内路径 /work（宿主路径在容器里不存在）", spec.Workdir)
	}
	if spec.Workdir == a.Workdir {
		t.Error("Workdir 传的是宿主路径：docker run --workdir 会指向容器里不存在的目录")
	}
}

// TestSandboxUsesProbeWorkdir：容器工作目录以 Probe 的结果为准（只有 sandbox
// 实现知道镜像里的工作目录是什么），session-dir / HOME / ProcessSpec.Workdir
// 三者必须跟着它走。
func TestSandboxUsesProbeWorkdir(t *testing.T) {
	a, sess := newSandboxAgent(t, "/srv/agent-home")
	startSandboxAgent(t, a)

	spec := sess.spec(t, 0)
	if spec.Workdir != "/srv/agent-home" {
		t.Errorf("Workdir = %q，期望 Probe 报出来的容器工作目录", spec.Workdir)
	}
	if got := envOf(spec.Env, "HOME"); got != "/srv/agent-home/.home" {
		t.Errorf("HOME = %q，期望落在容器工作目录之下", got)
	}
	if got := argValue(spec.Command, "--session-dir"); got != "/srv/agent-home/.pi-sessions" {
		t.Errorf("--session-dir = %q，期望落在容器工作目录之下", got)
	}
}

func TestSandboxVersionValidatedBeforeLaunch(t *testing.T) {
	a, sess := newSandboxAgent(t, "")
	a.VersionRange = VersionRange{Min: "9.0.0", Max: "9.9.9"}
	if err := a.Start(context.Background(), harness.AgentStart{Workdir: a.Workdir}); err == nil {
		t.Fatal("镜像内 pi 版本超出支持区间时必须拒绝启动")
	}
	if sess.launchCount() != 0 {
		t.Fatal("版本不支持后仍启动了容器主进程")
	}
	a, sess = newSandboxAgent(t, "")
	sess.probeResult.PiVersion = ""
	if err := a.Start(context.Background(), harness.AgentStart{Workdir: a.Workdir}); err == nil {
		t.Fatal("镜像内 pi 版本为空时必须拒绝启动")
	}
	a, _ = newSandboxAgent(t, "")
	startSandboxAgent(t, a)
	if got := a.Version(); got != "0.86.0" {
		t.Errorf("Version() = %q，期望镜像内版本", got)
	}
}

// TestSandboxProbeFailureFailsStart：Probe 失败时 Start 直接失败，且**不**去
// 起进程再报错（「起进程再报错」会留下一个没人管的容器/进程）。
func TestSandboxProbeFailureFailsStart(t *testing.T) {
	a, sess := newSandboxAgent(t, "")
	sess.probeErr = errors.New("runner 镜像不可用")

	err := a.Start(context.Background(), harness.AgentStart{Workdir: a.Workdir})
	if err == nil {
		t.Fatal("Probe 失败必须让 Start 失败")
	}
	if !strings.Contains(err.Error(), "sandbox 探测失败") {
		t.Errorf("错误信息要能定位到 Probe: %v", err)
	}
	if sess.launchCount() != 0 {
		t.Errorf("Probe 失败后仍调用了 Launch %d 次", sess.launchCount())
	}
}

// TestSandboxLaunchFailureFailsStart：Launch 失败要带上上下文（哪一步失败的），
// 而不是把 sandbox 实现的错误原样抛出——原样抛出会让人以为是协议问题。
func TestSandboxLaunchFailureFailsStart(t *testing.T) {
	a, sess := newSandboxAgent(t, "")
	sess.launchErr = errors.New("docker run 失败")

	err := a.Start(context.Background(), harness.AgentStart{Workdir: a.Workdir})
	if err == nil {
		t.Fatal("Launch 失败必须让 Start 失败")
	}
	if !strings.Contains(err.Error(), "sandbox 启动 pi 失败") {
		t.Errorf("错误信息要能定位到 Launch: %v", err)
	}
}

// ─────────────────────── 附着式假进程上的一整轮 ───────────────────────

// TestSandboxRoundOverManagedProcess：在假 session 的附着式假进程上跑通
// Start → Round，证明 sandbox 路径是一条能用的完整链路（握手 + 会话复位断言
// + 事件流），而不是只有「字段看起来对」。
func TestSandboxRoundOverManagedProcess(t *testing.T) {
	a, sess := newSandboxAgent(t, "")
	a.Extensions = []string{"/profile/ext/main.ts"}
	a.Approve = true
	// 假对端要在 Start 的握手**之前**知道扩展列表：get_commands 是启动自检的
	// 唯一依据，返回空列表会被正确判成「项目资源被静默忽略」。
	sess.peerHook = func(p *fakePi) { p.exts = a.Extensions }
	startSandboxAgent(t, a)

	sess.mu.Lock()
	peer := sess.lastPeer
	sess.mu.Unlock()
	// 假对端在 Launch 里被创建、peerHook 已经把扩展列表预置进去了；这里再断言
	// 一次，免得 peerHook 哪天被删掉后自检变成「静默通过空列表」。
	if got := peer.exts; len(got) != 1 || got[0] != a.Extensions[0] {
		t.Fatalf("假对端的扩展列表没就位: %v", got)
	}

	var kinds []harness.EventKind
	res, err := a.Round(context.Background(), "侦察目标", func(ev harness.Event) {
		kinds = append(kinds, ev.Kind)
	})
	if err != nil {
		t.Fatalf("Round 失败: %v", err)
	}
	if res.Err != "" {
		t.Fatalf("RoundResult.Err 应为空: %s", res.Err)
	}
	if res.Reason != harness.ReasonCompleted {
		t.Errorf("Reason = %q，期望 completed", res.Reason)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d，期望 1（数 tool_execution_start）", res.Turns)
	}
	if res.Text != "沙箱内文本。" {
		t.Errorf("Text = %q", res.Text)
	}
	if !contains(kinds, harness.EventToolEnd) || !contains(kinds, harness.EventSettled) {
		t.Errorf("事件种类不全: %v", kinds)
	}
	// 每轮一次 new_session（Start 的握手 1 次 + 本轮 1 次）。
	cmds := peer.commandTypes()
	n := 0
	for _, c := range cmds {
		if c == "new_session" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("new_session 次数 = %d，期望 2（Start 1 + 本轮 1）: %v", n, cmds)
	}
	if sess.launchCount() != 1 {
		t.Errorf("Launch 次数 = %d，期望 1（一个 sandbox 只允许一个主进程）", sess.launchCount())
	}
}

// ─────────────────────── Close 必须终止附着进程 ───────────────────────

// TestSandboxCloseKillsManagedProcess：v0.4 要求「停止容器即可靠终止 pi 及工具
// 子进程」。假进程的 Wait 在 Kill 之前一直阻塞，所以「Close 之后 Wait 返回」
// 就是这条要求的直接证据。
func TestSandboxCloseKillsManagedProcess(t *testing.T) {
	a, sess := newSandboxAgent(t, "")
	startSandboxAgent(t, a)

	sess.mu.Lock()
	proc := sess.lastProc
	sess.mu.Unlock()
	if proc == nil {
		t.Fatal("没有附着进程")
	}
	waited := make(chan struct{})
	go func() { _ = proc.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("Kill 之前 Wait 就返回了，假进程没有模拟「不会自己退出」")
	case <-time.After(100 * time.Millisecond):
	}

	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Close 之后附着进程的 Wait 仍未返回：sandbox 路径没有终止主进程")
	}
	if proc.killCount() == 0 {
		t.Error("Close 没有调用 ManagedProcess.Kill")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("Close 必须幂等: %v", err)
	}
}

// ─────────────────────── Factory ───────────────────────

// TestFactoryRequiresSessionAndSink：v0.4 的工厂**必须**拿到 sandbox session
// （没有 session 就只能回落宿主进程，而那条路径是禁止的），事件出口也不能缺。
func TestFactoryRequiresSessionAndSink(t *testing.T) {
	f := &Factory{}
	sess := &fakeSession{probeResult: harness.ProbeResult{Workdir: "/work", PiVersion: "0.86.0"}}
	sink := &recSink{}

	ag, err := f.New(harness.AgentSpec{}, nil, sink)
	if err == nil {
		t.Fatal("session 为 nil 时必须构造失败（v0.4 不允许宿主进程启动路径）")
	}
	if ag != nil {
		t.Error("构造失败时必须返回 nil Agent")
	}
	if !strings.Contains(err.Error(), "sandbox session") {
		t.Errorf("错误消息要可读且点明原因: %v", err)
	}

	ag, err = f.New(harness.AgentSpec{}, sess, nil)
	if err == nil {
		t.Fatal("sink 为 nil 时必须构造失败（事件出口缺失会让轮次结果不可解释）")
	}
	if ag != nil {
		t.Error("构造失败时必须返回 nil Agent")
	}
	if !strings.Contains(err.Error(), "event sink") {
		t.Errorf("错误消息要可读且点明原因: %v", err)
	}
}

// TestFactoryDoesNotDoubleEmit：事件只走**一条**出口（Round 的 emit 回调 =
// sink.Emit）。这里同时挂一个取证用的 OnRawEvent 回调，断言：
//   - 规范化事件到达 sink，且 settled 只到了一次；
//   - sink 收到的每一条都带 Kind（原始帧路径会产出零值事件）；
//   - Factory.New 自己**不**去挂 OnRawEvent（曾经那版挂了一个什么都不做的
//     emitNormalized，看起来在工作、实际什么都不做）。
func TestFactoryDoesNotDoubleEmit(t *testing.T) {
	hostDir := t.TempDir()
	envFile := filepath.Join(hostDir, ".env")
	if err := os.WriteFile(envFile, []byte("OPENCODE_API_KEY=test-key-not-real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")

	sess := &fakeSession{probeResult: harness.ProbeResult{Workdir: "/work", PiVersion: "0.86.0"}}
	sink := &recSink{}
	f := &Factory{EnvFile: envFile}
	ag, err := f.New(harness.AgentSpec{Provider: "opencode-go", Model: "deepseek-v4-flash"}, sess, sink)
	if err != nil {
		t.Fatal(err)
	}
	ad, ok := ag.(*adapter)
	if !ok {
		t.Fatalf("Factory.New 返回了 %T，期望 *adapter", ag)
	}
	if ad.agent.OnRawEvent != nil {
		t.Error("Factory.New 不该挂 OnRawEvent：事件出口只有 sink 一条，第二条出口会重复推送")
	}
	// 取证钩子由调用方自己挂（Agent 的文档语义）。
	var rawFrames int
	ad.agent.OnRawEvent = func(Envelope, []byte) { rawFrames++ }
	ad.agent.StallTimeout = 5 * time.Second
	ad.agent.ProbeTimeout = 2 * time.Second
	ad.agent.AbortGrace = 500 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := ag.Start(ctx, harness.AgentStart{Workdir: hostDir}); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = ag.Close(context.Background()) }()

	if _, err := ag.Round(ctx, harness.RoundRequest{Prompt: "侦察", Round: 1}); err != nil {
		t.Fatalf("Round 失败: %v", err)
	}
	if rawFrames == 0 {
		t.Error("OnRawEvent 没收到任何原始帧：取证钩子失效了")
	}
	if n := sink.count(harness.EventSettled); n != 1 {
		t.Errorf("sink 收到 %d 条 settled，期望恰好 1 条（重复推送说明有第二条出口）", n)
	}
	if n := sink.count(harness.EventToolStart); n < 1 {
		t.Errorf("sink 没收到 tool_start，事件链路断了: %v", sink.kinds())
	}
	if n := sink.count(harness.EventToolEnd); n != 1 {
		t.Errorf("sink 收到 %d 条 tool_end，期望 1", n)
	}
	for i, k := range sink.kinds() {
		if k == "" {
			t.Fatalf("sink 第 %d 条事件的 Kind 为空：有原始帧被当成规范化事件推了出去", i)
		}
	}
}

// ─────────────────────── sandboxEnv 的凭据纪律 ───────────────────────

// TestSandboxEnvNeverInheritsHostEnv：容器只拿到 provider 凭据与最小运行环境。
// 平台 token 与宿主控制变量必须留在可信一侧。
func TestSandboxEnvNeverInheritsHostEnv(t *testing.T) {
	t.Setenv("BENCHMARK_TOKEN", "host-platform-token-must-not-leak")
	t.Setenv("OPENCODE_API_KEY", "")
	env := sandboxEnv(map[string]string{"A": "1"}, "opencode-go", "")
	got := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			got[k] = v
		}
	}
	if _, ok := got["BENCHMARK_TOKEN"]; ok {
		t.Error("宿主的平台 token 被继承进 sandbox 环境了")
	}
	if _, ok := got["A"]; ok {
		t.Error("非 provider 的 .env 项被注入 sandbox")
	}
	if got["HOME"] != "/work/.home" {
		t.Errorf("HOME 缺省 = %q，期望容器内可写路径 /work/.home", got["HOME"])
	}
	if got["PATH"] != "/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("PATH = %q，期望镜像内固定 PATH", got["PATH"])
	}
	// 显式 home 优先。
	env = sandboxEnv(nil, "", "/custom/home")
	if !contains(env, "HOME=/custom/home") {
		t.Errorf("显式 HOME 没有生效: %v", env)
	}
}

// TestSandboxEnvProcessEnvOnlyFillsProviderKey：进程环境只**补位** provider 凭据
// 族，而且以 .env 为优先。
func TestSandboxEnvProcessEnvOnlyFillsProviderKey(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "from-process-env")
	env := sandboxEnv(map[string]string{"OPENCODE_API_KEY": "from-dotenv"}, "opencode-go", "")
	if !contains(env, "OPENCODE_API_KEY=from-dotenv") {
		t.Errorf(".env 里的凭据应当优先: %v", env)
	}
	// .env 没有时才用进程环境补。
	env = sandboxEnv(nil, "opencode-go", "")
	if !contains(env, "OPENCODE_API_KEY=from-process-env") {
		t.Errorf("进程环境的凭据没有补位: %v", env)
	}
}

// TestProviderCredentialNamesNeverEmptyProvider：空 provider **不能**推导出
// "_API_KEY" 这个名字。
//
// 曾经的守卫是 `if name != "_API_KEY"`——一个字符串比较，它把「空 provider 会
// 生成 _API_KEY」这个真实缺陷藏了起来：守卫与缺陷同源，删掉守卫（或改别名列表）
// 就会让容器去读宿主的 `_API_KEY` 环境变量。修法是不生成这个名字。
func TestProviderCredentialNamesNeverEmptyProvider(t *testing.T) {
	for _, p := range []string{"", "   "} {
		for _, n := range providerCredentialNames(p) {
			if n == "_API_KEY" {
				t.Errorf("provider=%q 推导出了 %q：空 provider 不得生成惯例名", p, n)
			}
		}
		if got := providerCredentialNames(p); len(got) != 0 {
			t.Errorf("provider=%q 不该产生任何凭据名: %v", p, got)
		}
	}
	if got := providerCredentialNames("deepseek"); !contains(got, "DEEPSEEK_API_KEY") {
		t.Errorf("惯例名推导错了: %v", got)
	}
	if got := providerCredentialNames("opencode-go"); !contains(got, "OPENCODE_API_KEY") {
		t.Errorf("已知别名丢了: %v", got)
	}
	// 宿主上的 _API_KEY 绝不能被注入容器。
	t.Setenv("_API_KEY", "host-underscore-key-must-not-leak")
	env := sandboxEnv(nil, "", "")
	for _, kv := range env {
		if strings.HasPrefix(kv, "_API_KEY=") {
			t.Fatalf("宿主 _API_KEY 被注入 sandbox 环境: %v", env)
		}
	}
}

// ─────────────────────── 小工具 ───────────────────────

// argValue 取 `--flag value` 形式的值，取不到返回空串。
func argValue(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

// TestSandboxFindsEnvRelativeToProcessCwd 钉住沙箱路径的 `.env` 查找起点。
//
// 回归：查找起点曾经是 `Agent.Workdir`，而 v0.4 的生产路径（`runChallenge` →
// `AgentStart{Workdir: SandboxSpec.Workdir}`）在这里填的是**容器内**路径
// （缺省 `/work`）。拿容器路径去搜宿主文件系统必然落空 ⇒ `.env` 里配好的
// provider key 不生效，一路到 Start 的凭据预检才以「没有可用的 API key」拒绝。
//
// 这条缺陷长期看不见有两个原因，测试里都要堵掉：
//
//   - 既有的沙箱用例全都传 `AgentStart{Workdir: a.Workdir}`——**宿主**目录，
//     于是查找起点恰好是对的。这里必须按生产路径传容器路径。
//   - `doctor` 的凭据检查从 **cwd** 起找，`run` 从 Workdir 起找 ⇒ 体检报「已设置」
//     而运行拒绝启动，一次**假绿**。两者现在用同一个起点。
func TestSandboxFindsEnvRelativeToProcessCwd(t *testing.T) {
	hostDir := t.TempDir()
	// 明显的假值：凭据纪律要求测试夹具里不能出现任何真实 key。
	envFile := filepath.Join(hostDir, ".env")
	if err := os.WriteFile(envFile, []byte("OPENCODE_API_KEY=test-key-not-real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(hostDir)
	// 宿主进程环境里若真有同名变量，先清掉，避免测试断言依赖宿主状态。
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")

	const containerWorkdir = "/work"
	sess := &fakeSession{probeResult: harness.ProbeResult{
		ContainerID: "fake-container-id", Image: "red-harness-runner:v0.3.0",
		Workdir: containerWorkdir, PiVersion: "0.86.0"}}
	a := &Agent{
		Session: sess,
		// EnvFile 留空：这正是 CLI 的形态（`internal/cli` 不传 .env），
		// 所以查找必须自己走对。
		Workdir:      containerWorkdir,
		Provider:     "opencode-go",
		Model:        "deepseek-v4-flash",
		StallTimeout: 5 * time.Second,
		ProbeTimeout: 2 * time.Second,
		AbortGrace:   500 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.Start(ctx, harness.AgentStart{Workdir: containerWorkdir}); err != nil {
		t.Fatalf("沙箱路径必须能按进程 cwd 找到 .env（.env 是宿主文件，容器路径不是合法查找起点）: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	// 找到了还不够：它必须真的进了注入容器的环境（否则凭据只是「读到了」而没生效）。
	spec := sess.spec(t, 0)
	if got := envOf(spec.Env, "OPENCODE_API_KEY"); got != "test-key-not-real" {
		t.Errorf("容器环境里的 OPENCODE_API_KEY = %q，期望来自 .env 的值", got)
	}
}
