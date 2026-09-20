package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	harness "github.com/red-copilot/red-harness"
)

// doctor 执行 `doctor` 子命令：在**任何平台写操作之前**体检环境。
//
// 为什么这一条必须在最前面：体检失败要阻止的是「带着坏配置去平台上跑题」。
// 前身事故里 VPN 预检失败被当成「题目不存在」继续跑，结果白烧预算；M0 也记过
// 凭据失效（401）会以**静默空会话**的形式出现——两者都不是「跑起来才发现」，
// 而是「跑起来也发现不了」。所以 doctor 的失败必须是退出码，不是日志。
//
// ⚠️ **凭据只报告「是否设置」，绝不打印值**（model.go 的 DoctorCheck 注释）。
// 值打印出来就会进终端 scrollback、进工单、进 CI 日志——这正是本计划里
// 「凭据绝不入库」那条规则要防的扩散路径。
//
// 本波次只交付 flag 解析与注入点：真正的检查项（Python SDK / 凭据 / VPN /
// Docker / runner 镜像 / pi 版本 / provider 配置）在 T14 里由装配层接上。
func (a *app) doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	// json 输出给机器读（CI 门禁），默认的人读格式给终端。
	_ = fs.Bool("json", false, "以 JSON 打印体检结果")
	_ = fs.String("store", "runs", "运行目录根")
	help, err := a.parseFlags("doctor", fs, args)
	if help || err != nil {
		return err
	}

	// doctor 不需要 store：体检的是宿主环境，与某次运行无关。所以装配点只用
	// 包级 wired 拿端口，不传 storeDir（传了反而暗示「体检结果与 store 有关」）。
	ports, err := a.ports("")
	if err != nil {
		return err
	}
	if ports.Doctor == nil {
		return notImplemented("doctor")
	}
	rep, err := ports.Doctor.Doctor(context.Background())
	if err != nil {
		return err
	}
	printDoctor(a.out(), rep)
	if !rep.OK {
		// 任何 Fatal 检查失败 ⇒ 退出码非 0（model.go 的 DoctorReport 契约）。
		// 这里返回的是 KindConfig 而不是 notImplementedError：doctor 的**骨架**
		// 已经接上了，失败的是环境本身。
		return harness.Ef(harness.KindConfig, "cli.doctor", "体检未通过", nil)
	}
	return nil
}

// printDoctor 打印体检结果。Detail 由各检查项自己保证不含凭据明文。
func printDoctor(w io.Writer, rep harness.DoctorReport) {
	for _, c := range rep.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		line := mark + " " + c.Name
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		if c.Fatal {
			line += "（Fatal）"
		}
		fmt.Fprintln(w, line)
	}
	if rep.OK {
		fmt.Fprintln(w, "体检通过")
	}
}
