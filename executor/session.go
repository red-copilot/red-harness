package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	harness "github.com/red-copilot/red-harness"
)

// NewSession implements the v0.4 Sandbox port. It creates network policy and
// the provider proxy, but deliberately does not start a placeholder process.
// The first and only Launch call becomes the container's main process.
func (d *Docker) NewSession(ctx context.Context, spec harness.SandboxSpec) (harness.SandboxSession, error) {
	if strings.TrimSpace(string(spec.RunID)) == "" {
		return nil, harness.Ef(harness.KindConfig, "sandbox.session", "RunID 不能为空", nil)
	}
	if strings.TrimSpace(spec.Workdir) == "" {
		return nil, harness.Ef(harness.KindConfig, "sandbox.session", "Workdir 不能为空", nil)
	}
	if spec.ProfileDir != "" {
		if !filepath.IsAbs(spec.ProfileDir) || strings.Contains(spec.ProfileDir, ":") {
			return nil, harness.Ef(harness.KindConfig, "sandbox.session", "ProfileDir 必须是绝对路径且不得含冒号", nil)
		}
		if _, err := os.Stat(spec.ProfileDir); err != nil {
			return nil, harness.Ef(harness.KindConfig, "sandbox.session", "profile bundle 不存在", err)
		}
	}
	execSpec := harness.ExecSpec{
		RunID:  spec.RunID,
		Target: spec.Target,
		Executor: harness.ExecutorSpec{
			Image: spec.Image, Workdir: "/work", CPUs: spec.CPUs,
			MemoryMB: spec.MemoryMB, PidsLimit: spec.PidsLimit,
			// The allowlist comes from the authorized platform's Target.Addrs
			// (resolved by the harness in Run), never from the caller.
			ReadOnly: true, AllowHosts: append([]string(nil), spec.Target.Addrs...), Env: cloneEnv(spec.Env),
		},
		Workdir: spec.Workdir,
	}
	p, err := planRun(execSpec, d.cfg)
	if err != nil {
		return nil, err
	}
	// v0.4 never bind-mounts a writable host work directory. /work is a
	// bounded tmpfs owned by this container; only the optional solver profile is
	// mounted from the trusted host and it is read-only.
	p.Mounts = nil
	p.ExtraTmpfs = []string{"/work:rw,nosuid,nodev,size=64m,mode=1777"}
	if spec.ProfileDir != "" {
		p.ReadonlyMounts = map[string]string{spec.ProfileDir: "/profile"}
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.PrepareTimeout)
	defer cancel()

	netw := networkFor(p, d.cfg)
	if out, err := d.run(ctx, nil, netw.createArgv()[1:]...); err != nil {
		return nil, harness.Ef(harness.KindExecutor, "sandbox.session", "创建 per-run 网络失败", wrapOut(err, out))
	}
	if netw.ManageIptables {
		netw.Bridge = d.detectBridge(ctx, p.NetworkName)
		if err := d.installNetworkRules(ctx, netw); err != nil {
			_ = d.removeNetwork(ctx, p.NetworkName)
			return nil, err
		}
	}
	if p.ProxyURL != "" {
		bind := fmt.Sprintf("%s:%d", p.NetworkGateway, proxyPort)
		pr, err := startProviderProxy(bind, proxyConfig{AllowHosts: d.cfg.ProviderAllowHosts}, nil)
		if err != nil {
			if netw.ManageIptables {
				d.uninstallNetworkRules(ctx, netw)
			}
			_ = d.removeNetwork(ctx, p.NetworkName)
			return nil, err
		}
		d.mu.Lock()
		d.proxies[spec.RunID] = pr
		d.mu.Unlock()
	}
	return &dockerSession{docker: d, spec: spec, execSpec: execSpec, plan: p}, nil
}

type dockerSession struct {
	docker   *Docker
	spec     harness.SandboxSpec
	execSpec harness.ExecSpec
	plan     runPlan

	mu        sync.Mutex
	launched  bool
	closed    bool
	container string
	piVersion string
}

var _ harness.SandboxSession = (*dockerSession)(nil)

func (s *dockerSession) Probe(ctx context.Context) (harness.ProbeResult, error) {
	s.mu.Lock()
	closed, id, cachedVersion := s.closed, s.container, s.piVersion
	// plan.Image 会被下面的成功分支改写（tag → image ID），所以与 piVersion
	// 一起在锁内取快照：两个字段的读写遵循同一把锁，不混用锁内锁外。
	image, workdir := s.plan.Image, s.plan.WorkdirCtr
	s.mu.Unlock()
	if closed {
		return harness.ProbeResult{}, harness.Ef(harness.KindExecutor, "sandbox.probe", "sandbox 已关闭", nil)
	}
	if id != "" {
		return harness.ProbeResult{ContainerID: id, Image: image, Workdir: workdir, PiVersion: cachedVersion}, nil
	}
	if cachedVersion != "" {
		return harness.ProbeResult{Image: image, Workdir: workdir, PiVersion: cachedVersion}, nil
	}
	// Probe uses a short-lived, network-isolated container from the exact image
	// Launch will use. The host's pi version says nothing about this image.
	imageID, err := s.docker.run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil || strings.TrimSpace(imageID) == "" {
		return harness.ProbeResult{}, harness.Ef(harness.KindExecutor, "sandbox.probe", "runner 镜像不可用", err)
	}
	imageID = strings.TrimSpace(imageID)
	version, err := s.docker.run(ctx, nil, "run", "--rm", "--network", "none", "--read-only",
		"--cap-drop", "ALL", "--user", s.docker.cfg.RunUser, "--entrypoint", "pi", imageID, "--version")
	if err != nil || strings.TrimSpace(version) == "" {
		return harness.ProbeResult{}, harness.Ef(harness.KindExecutor, "sandbox.probe", "无法核验镜像内 pi 版本", err)
	}
	version = strings.TrimSpace(version)
	s.mu.Lock()
	s.piVersion = version
	s.plan.Image = imageID
	s.mu.Unlock()
	return harness.ProbeResult{Image: imageID, Workdir: workdir, PiVersion: version}, nil
}

func (s *dockerSession) Launch(ctx context.Context, ps harness.ProcessSpec) (harness.ManagedProcess, error) {
	if len(ps.Command) == 0 {
		return nil, harness.Ef(harness.KindConfig, "sandbox.launch", "主进程命令为空", nil)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, harness.Ef(harness.KindExecutor, "sandbox.launch", "sandbox 已关闭", nil)
	}
	if s.launched {
		s.mu.Unlock()
		return nil, harness.Ef(harness.KindConfig, "sandbox.launch", "一个 sandbox 只允许启动一个主进程", nil)
	}
	if s.piVersion == "" {
		s.mu.Unlock()
		return nil, harness.Ef(harness.KindConfig, "sandbox.launch", "必须先核验镜像内 pi 版本", nil)
	}
	s.launched = true
	// plan.Image 由 Probe 在锁内改写（tag → image ID），快照必须在同一个临界区
	// 里取，否则可能与那次改写交错，create/start 拿到两个不同的镜像引用。
	p := s.plan
	s.mu.Unlock()

	p.Command = append([]string(nil), ps.Command...)
	if ps.Workdir != "" {
		p.WorkdirCtr = ps.Workdir
	}
	for k, v := range ps.Env {
		if p.Env == nil {
			p.Env = map[string]string{}
		}
		p.Env[k] = v
	}
	// docker run -e KEY=value would expose provider keys in the host process
	// list. Docker reads this 0600 file during creation; remove it immediately.
	envFile, err := writeProcessEnvFile(p.Env)
	if err != nil {
		return nil, harness.Ef(harness.KindConfig, "sandbox.launch", "进程环境变量无效", err)
	}
	if envFile != "" {
		defer os.Remove(envFile)
	}
	p.Env = nil
	argv := runArgv(p)
	flags := []string{"--interactive"} // keep pi RPC stdin open
	if envFile != "" {
		flags = append(flags, "--env-file", envFile)
	}
	// Create first, then start with attachment in a single Docker command.
	// Starting detached and attaching later loses early RPC responses.
	//
	// docker create 不接受 --detach（容器创建后是 stopped，由 start --attach
	// 接管）。这里**先核对** runArgv 的固定前缀再切，而不是闷头切 argv[3:]：
	// 那句 argv 是隔离约束的唯一载体，前缀一变就该立刻响，而不是把新加的
	// flag 静默丢掉。
	if len(argv) < 3 || argv[1] != "run" || argv[2] != "--detach" {
		return nil, harness.Ef(harness.KindExecutor, "sandbox.launch", "docker run argv 前缀与预期不符", nil)
	}
	argv = append([]string{argv[0], "create"}, append(flags, argv[3:]...)...)
	argv[0] = s.docker.cfg.Binary
	out, err := s.docker.run(ctx, nil, argv[1:]...)
	if err != nil {
		return nil, harness.Ef(harness.KindExecutor, "sandbox.launch", "创建 sandbox 主进程失败", wrapOut(err, out))
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return nil, harness.Ef(harness.KindExecutor, "sandbox.launch", "docker 未返回容器 ID", nil)
	}
	attach := exec.CommandContext(ctx, s.docker.cfg.Binary, "start", "--attach", "--interactive", id)
	in, err := attach.StdinPipe()
	if err != nil {
		_, _ = s.docker.run(context.Background(), nil, "rm", "-f", id)
		return nil, harness.Ef(harness.KindExecutor, "sandbox.launch", "连接主进程 stdin 失败", err)
	}
	outR, err := attach.StdoutPipe()
	if err != nil {
		_ = in.Close()
		_, _ = s.docker.run(context.Background(), nil, "rm", "-f", id)
		return nil, harness.Ef(harness.KindExecutor, "sandbox.launch", "连接主进程 stdout 失败", err)
	}
	// stderr must not share the RPC stdout stream. It is retained in the
	// sandbox lifecycle, not printed to the host terminal.
	attach.Stderr = io.Discard
	if err := attach.Start(); err != nil {
		_ = in.Close()
		_, _ = s.docker.run(context.Background(), nil, "rm", "-f", id)
		return nil, harness.Ef(harness.KindExecutor, "sandbox.launch", "启动并连接主进程失败", err)
	}
	s.mu.Lock()
	s.container = id
	s.mu.Unlock()
	return &attachedProcess{session: s, id: id, cmd: attach, stdin: in, stdout: outR}, nil
}

func (s *dockerSession) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.docker.Reclaim(ctx, s.spec.RunID)
}

type attachedProcess struct {
	session *dockerSession
	id      string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	mu      sync.Mutex
	killed  bool
}

var _ harness.ManagedProcess = (*attachedProcess)(nil)

func (p *attachedProcess) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *attachedProcess) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *attachedProcess) Close() error                { _ = p.Kill(); _ = p.stdin.Close(); return p.stdout.Close() }
func (p *attachedProcess) Wait() error                 { return p.cmd.Wait() }
func (p *attachedProcess) Kill() error {
	p.mu.Lock()
	if p.killed {
		p.mu.Unlock()
		return nil
	}
	p.killed = true
	p.mu.Unlock()
	_ = p.stdin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), p.session.docker.cfg.CommandTimeout)
	defer cancel()
	_, err := p.session.docker.run(ctx, nil, "stop", "--time", itoa(p.session.docker.cfg.StopTimeout), p.id)
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func cloneEnv(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func writeProcessEnvFile(env map[string]string) (string, error) {
	if len(env) == 0 {
		return "", nil
	}
	f, err := os.CreateTemp("", "red-harness-env-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "" || strings.ContainsAny(key, "=\r\n") || strings.ContainsAny(env[key], "\r\n") {
			return "", fmt.Errorf("环境变量名称或值包含非法换行/分隔符")
		}
		if _, err = io.WriteString(f, key+"="+env[key]+"\n"); err != nil {
			return "", err
		}
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return path, nil
}
