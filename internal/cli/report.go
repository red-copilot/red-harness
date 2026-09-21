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
//
// ⚠️ **`--run` 是必需的，且必须现在就开始校验**：报告是针对某一次运行的，
// 缺它时退化成「随便挑一个 run」会生成一份看起来正常、实际属于别人的报告。
// `--out`/`--json` 是 T16 对齐时不该再改的公开面，其值读不到是因为实现还没
// 落地——T16 落地时必须连读取一起补上。
func (a *app) report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	_ = fs.String("store", "runs", "运行目录根")
	runID := fs.String("run", "", "目标运行 ID")
	_ = fs.String("out", "", "输出路径（默认写到 run 目录下）")
	_ = fs.Bool("json", false, "输出 JSON（默认 markdown）")
	help, err := a.parseFlags("report", fs, args)
	if help || err != nil {
		return err
	}
	if err := requireRunID(*runID); err != nil {
		return err
	}
	// T16 会在这里调 report.Build。
	return notImplemented("report")
}
