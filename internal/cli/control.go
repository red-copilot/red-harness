package cli

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net"
	"path/filepath"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// 控制面：`pause` / `cancel` 作用的对象是**另一个进程里**正在跑的运行，所以它们
// 不走本地引擎对象，而是连到那个进程创建的 Unix control socket（PLAN.md:40，
// 权限 0600）。为什么必须是 socket 而不是「读一个暂停文件」：暂停要 abort 当前
// round 并把被中断的 intent 标记为 interrupted 落盘，那只有 run 进程自己能做到。
//
// 本波次只交付**客户端**这一半。服务端（监听、权限 0600、命令分发）在 T11 的
// `engine/control.go` 里；两边共享的唯一约定是下面的 socket 路径与 wire 格式。

const (
	// controlSocketName 是 control socket 的文件名。
	controlSocketName = "control.sock"
	// controlTimeout 是单条控制命令的墙钟上限。
	//
	// 有超时是必要的：socket 对端可能是一个已经卡死的 run 进程（暂停要 abort
	// 当前 round，可能正在等 pi 的响应）。没有超时的话 CLI 会一直挂着，用户
	// 唯一的出路是 Ctrl-C。
	controlTimeout = 10 * time.Second
)

// controlSocketPath 返回一次运行的 control socket 路径：`<storeDir>/runs/<runID>/`。
//
// ⚠️ **这是与 engine 侧各写一份的共享约定**（engine 不能导入本包）。改这里必须
// 同时改 T11 的服务端，否则 pause/cancel 会静默连不上——测试
// `TestControlSocketPathIsUnderRunDir` 钉住的就是这个字面形状。
func controlSocketPath(storeDir string, runID harness.RunID) string {
	return filepath.Join(storeDir, "runs", string(runID), controlSocketName)
}

// controlCommand 是控制命令的 wire 形状。
//
// 用 JSON 而不是裸字符串：T14 之后 pause/cancel 可能还要带理由（写进报告），
// 而且 JSON 让服务端能对未知命令回结构化错误而不是「看不懂就忽略」。
type controlCommand struct {
	Cmd string `json:"cmd"`
}

// controlClient 是控制面客户端。三条命令与 harness.RunHandle 的 Pause/Resume/
// Cancel 一一对应——socket 存在的意义就是让**另一个进程**能调它们。
type controlClient interface {
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Cancel(ctx context.Context) error
}

// controlDialer 建一个连到指定 socket 路径的客户端。
//
// **零值为 nil ⇒ pause/cancel 返回「未实现」**：本波次没有服务端可连，而且
// T14 之前也没有真正的运行进程。生产实现走 net.Dial("unix", path)。
type controlDialer func(socketPath string) (controlClient, error)

// pause 执行 `pause` 子命令。
func (a *app) pause(args []string) error { return a.controlCmd(args, "pause") }

// cancel 执行 `cancel` 子命令。
func (a *app) cancel(args []string) error { return a.controlCmd(args, "cancel") }

// controlCmd 是 pause 与 cancel 的公共实现（两者只差一条命令名）。
func (a *app) controlCmd(args []string, cmd string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	var (
		store = fs.String("store", "runs", "运行目录根")
		runID = fs.String("run", "", "目标运行 ID")
	)
	help, err := a.parseFlags(cmd, fs, args)
	if help || err != nil {
		return err
	}
	if err := requireRunID(*runID); err != nil {
		return err
	}

	ports, err := a.ports(*store)
	if err != nil {
		return err
	}
	if ports.Control == nil {
		return notImplemented(cmd)
	}
	client, err := ports.Control(controlSocketPath(*store, harness.RunID(*runID)))
	if err != nil {
		// 连不上通常意味着「这个 run 没有在跑」——把原样错误上抛，不要改写成
		// 「未实现」或「运行不存在」：前者会让人以为功能没做，后者会掩盖真实的
		// 路径/权限问题。
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	switch cmd {
	case "pause":
		return client.Pause(ctx)
	case "cancel":
		return client.Cancel(ctx)
	}
	return harness.Ef(harness.KindConfig, "cli.control", "未知控制命令: "+cmd, nil)
}

// socketControl 是 controlClient 的生产实现：一条命令一次连接，写完即关。
//
// 为什么不做长连接：控制命令是低频的人手操作，而长连接会带来「连接还开着、
// 但 run 已经结束」的模糊状态。一问一答的 socket 语义最不容易出错。
type socketControl struct{ path string }

// newSocketControl 拨到 path。失败在**拨号时**就返回，而不是等到发命令——
// 「run 不在跑」应当在用户按下 pause 的瞬间报出来。
func newSocketControl(path string) (controlClient, error) {
	conn, err := net.DialTimeout("unix", path, controlTimeout)
	if err != nil {
		return nil, harness.Ef(harness.KindConfig, "cli.control",
			"连不上运行进程的 control socket（该 run 可能没在跑）: "+path, err)
	}
	_ = conn.Close()
	return &socketControl{path: path}, nil
}

func (c *socketControl) Pause(ctx context.Context) error  { return c.send(ctx, "pause") }
func (c *socketControl) Resume(ctx context.Context) error { return c.send(ctx, "resume") }
func (c *socketControl) Cancel(ctx context.Context) error { return c.send(ctx, "cancel") }

// send 发一条命令并读回一行应答。空应答视为成功（服务端在 T11 里定具体形状）。
func (c *socketControl) send(ctx context.Context, cmd string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return harness.Ef(harness.KindConfig, "cli.control", "控制命令发送失败: "+cmd, err)
	}
	defer func() { _ = conn.Close() }()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(controlCommand{Cmd: cmd}); err != nil {
		return harness.Ef(harness.KindConfig, "cli.control", "控制命令写入失败: "+cmd, err)
	}
	// 读回一行应答。EOF 也算成功：服务端收到命令后直接关闭是合法应答。
	reply, err := io.ReadAll(io.LimitReader(conn, 4096))
	if err != nil {
		return harness.Ef(harness.KindConfig, "cli.control", "控制命令应答读取失败: "+cmd, err)
	}
	if len(reply) > 0 {
		var out struct {
			OK  bool   `json:"ok"`
			Err string `json:"error,omitempty"`
		}
		if json.Unmarshal(reply, &out) == nil && !out.OK && out.Err != "" {
			return harness.Ef(harness.KindConfig, "cli.control", out.Err, nil)
		}
	}
	return nil
}
