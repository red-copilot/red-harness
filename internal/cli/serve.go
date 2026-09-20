package cli

import (
	"flag"
)

// serve 执行 `serve` 子命令：本地看板（PLAN.md:60）。
//
// 完整实现在 T15 的 `web/` 包里。本波次只固定**公开契约**——参数名就是契约，
// 后面对齐时不该再改：
//
//   - 默认只监听 127.0.0.1：看板会展示运行状态与 DAG，绑到 0.0.0.0 等于把它
//     暴露到网络上。要用随机 bearer token 才允许其他主机访问（PLAN.md:60）。
//   - 只允许 pause/resume/cancel 三个动作，不允许注入命令或编辑事实。
func (a *app) serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	_ = fs.String("store", "runs", "运行目录根")
	_ = fs.String("addr", "127.0.0.1:0", "监听地址；默认仅本机，端口 0 表示随机端口")
	_ = fs.Bool("open", false, "启动后打印访问 URL（含随机 token）")
	help, err := a.parseFlags("serve", fs, args)
	if help || err != nil {
		return err
	}
	// T15 会在这里起 web.Server 并阻塞到信号退出。
	return notImplemented("serve")
}
