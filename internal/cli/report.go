package cli

import (
	"flag"
)

// report 执行 `report` 子命令：从 run 目录生成报告（PLAN.md:61）。
//
// 完整实现在 T16 的 `report/` 包里。本波次固定参数面：
//
//   - 默认输出到 `<runDir>/report.md`（与 `report.json` 同目录，见设计文档 §6），
//     用 `--out` 可改写。
//   - `--json` 给机器读；默认 markdown 给人读。
//
// ⚠️ 报告是**公开产物**（0644，可能被拷走、被贴进工单），所以生成时必须保持
// 脱敏：flag 明文、凭据、原始工具输出都不出现在报告里（PLAN.md:61）。
// 报告生成器读 private/ 账本时必须只取指纹与计数。
func (a *app) report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	_ = fs.String("store", "runs", "运行目录根")
	_ = fs.String("run", "", "目标运行 ID")
	_ = fs.String("out", "", "输出路径（默认写到 run 目录下）")
	_ = fs.Bool("json", false, "输出 JSON（默认 markdown）")
	help, err := a.parseFlags("report", fs, args)
	if help || err != nil {
		return err
	}
	// T16 会在这里调 report.Build。
	return notImplemented("report")
}
