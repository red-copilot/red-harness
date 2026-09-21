package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
}

var _ harness.SandboxSession = (*dockerSession)(nil)

func (s *dockerSession) Probe(ctx context.Context) (harness.ProbeResult, error) {
	s.mu.Lock()
	closed, id := s.closed, s.container
	s.mu.Unlock()
	if closed {
		return harness.ProbeResult{}, harness.Ef(harness.KindExecutor, "sandbox.probe", "sandbox 已关闭", nil)
	}
	if id != "" {
		return harness.ProbeResult{ContainerID: id, Image: s.plan.Image, Workdir: s.plan.WorkdirCtr}, nil
	}
	// Probe the image without starting an agent process. This is a management
	// check only; Launch remains the sole path that starts pi/tools.
	if _, err := s.docker.run(ctx, nil, "image", "inspect", s.plan.Image); err != nil {
		return harness.ProbeResult{}, harness.Ef(harness.KindExecutor, "sandbox.probe", "runner 镜像不可用", err)
	}
	return harness.ProbeResult{Image: s.plan.Image, Workdir: s.plan.WorkdirCtr}, nil
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
	s.launched = true
	s.mu.Unlock()

	p := s.plan
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
	argv := runArgv(p)
	argv[0] = s.docker.cfg.Binary
	out, err := s.docker.run(ctx, nil, argv[1:]...)
	if err != nil {
		return nil, harness.Ef(harness.KindExecutor, "sandbox.launch", "启动 sandbox 主进程失败", wrapOut(err, out))
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return nil, harness.Ef(harness.KindExecutor, "sandbox.launch", "docker 未返回容器 ID", nil)
	}
	attach := exec.CommandContext(ctx, s.docker.cfg.Binary, "attach", "--sig-proxy=false", "-i", id)
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
		return nil, harness.Ef(harness.KindExecutor, "sandbox.launch", "attach 主进程失败", err)
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
