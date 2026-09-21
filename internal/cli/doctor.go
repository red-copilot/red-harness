package cli

import (
	"context"
	"encoding/json"
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
	jsonOut := fs.Bool("json", false, "以 JSON 打印体检结果")
	help, err := a.parseFlags("doctor", fs, args)
	if help || err != nil {
		return err
	}

	// doctor 不需要 store：体检的是宿主环境，与某次运行无关。所以装配点只传
	// storeDir=""——**装配层必须容忍它**（store 根与体检无关）。传一个假目录
	// 反而会让装配层真的去建目录、并把「体检」和「某个 store」绑在一起。
	//
	// ⚠️ **不要再注册 `--store`**：它会被解析、然后被丢掉，用户看到 --help 里
	// 有这个 flag 就会以为体检是针对某个 store 做的。
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
	if *jsonOut {
		if err := printDoctorJSON(a.out(), rep); err != nil {
			return err
		}
	} else {
		printDoctor(a.out(), rep)
	}
	// **Fatal 项失败必拦，OK 为假也必拦**：model.go 的契约是「任何 Fatal 检查失败
	// ⇒ 退出码非 0」。只看 rep.OK 等于把「该不该拦」整个委托给填报告的一方——一旦
	// OK 的语义被实现方改成「非致命汇总」，Fatal 守卫就失效了。所以这里取两者的
	// **并集**：宁可多拦（纯告警也红），不可漏拦（Fatal 失败却绿）。
	if !rep.OK || hasFatalFailure(rep) {
		// 这里返回的是 KindConfig 而不是 notImplementedError：doctor 的**骨架**
		// 已经接上了，失败的是环境本身。
		return harness.Ef(harness.KindConfig, "cli.doctor", "体检未通过", nil)
	}
	return nil
}

// hasFatalFailure 报告是否有 Fatal 检查项失败。
//
// 与 `!rep.OK` 是**或**关系：它额外覆盖「OK 为真但某项 Fatal 检查失败」这种
// 自相矛盾的报告——那是实现方的 bug，但不能让它变成一次绿色的 CI。
func hasFatalFailure(rep harness.DoctorReport) bool {
	for _, c := range rep.Checks {
		if c.Fatal && !c.OK {
			return true
		}
	}
	return false
}

// printDoctorJSON 打印机器可读的体检结果。
//
// ⚠️ **必须与 printDoctor 走同一份 DoctorReport**：Detail 由各检查项自己保证
// 不含凭据明文（model.go 的 DoctorCheck 注释）。CI 门禁靠 `.ok` 判成败。
func printDoctorJSON(w io.Writer, rep harness.DoctorReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return harness.Ef(harness.KindConfig, "cli.doctor", "打印 JSON 体检结果失败", err)
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
