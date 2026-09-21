package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// statsFlags 是 `stats` 的聚合过滤条件。
//
// 它是 `harness.StatsQuery` 的**唯一**翻译层：过滤语义（哪些字段是 run 级、
// 哪些是题级、时间窗是闭区间还是开区间）由 `store.ResultFileStore` 决定，
// CLI 只负责把字符串折成值。在这里再实现一遍过滤等于让两处口径漂移。
type statsFlags struct {
	store     string
	profile   string
	model     string
	scenario  string
	challenge string
	category  string
	since     string
	until     string
}

func (f *statsFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.store, "store", "runs", "运行目录根")
	fs.StringVar(&f.profile, "profile", "", "按 profile 摘要过滤（SolverProfile.Digest()）")
	fs.StringVar(&f.model, "model", "", "按模型名过滤")
	fs.StringVar(&f.scenario, "scenario", "", "按场景名过滤（fake / tsecbench）")
	fs.StringVar(&f.challenge, "challenge", "", "按题目 code 过滤")
	fs.StringVar(&f.category, "category", "", "按题目类别过滤")
	fs.StringVar(&f.since, "since", "", "起始时间（含），RFC3339 或 2006-01-02")
	fs.StringVar(&f.until, "until", "", "结束时间（含），RFC3339 或 2006-01-02")
}

// query 把 flag 折成 harness.StatsQuery。
//
// 空串一律表示「该维度不过滤」——这与 StatsQuery 的零值语义一致（`""` 与
// `time.Time{}` 都是「不设限」），所以不需要额外的「有没有给」标记。
func (f *statsFlags) query() (harness.StatsQuery, error) {
	since, err := parseWhen(f.since)
	if err != nil {
		return harness.StatsQuery{}, usagef("cli.stats", "--since 不是合法时间: %q（用 RFC3339 或 2006-01-02）", f.since)
	}
	until, err := parseWhen(f.until)
	if err != nil {
		return harness.StatsQuery{}, usagef("cli.stats", "--until 不是合法时间: %q（用 RFC3339 或 2006-01-02）", f.until)
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		// 反过来的时间窗永远匹配不到东西，而「0 条结果」与「时间写反了」在输出上
		// 完全同形。这里直接拒绝，别让用户去猜。
		return harness.StatsQuery{}, usagef("cli.stats", "--until (%s) 早于 --since (%s)：时间窗为空",
			f.until, f.since)
	}
	return harness.StatsQuery{
		ProfileDigest: f.profile,
		Model:         f.model,
		Scenario:      f.scenario,
		Challenge:     f.challenge,
		Category:      f.category,
		Since:         since,
		Until:         until,
	}, nil
}

// parseWhen 解析时间参数。空串 ⇒ 零值（不过滤）。
//
// 接受两种写法：完整 RFC3339（带时区，推荐）与 `2006-01-02`（按本地时区，
// 日界对齐到当天 00:00:00）。**只给日期时按本地时区解释**是刻意的：用户敲
// `--since 2026-09-20` 想的是「我本地那天之后」，把它按 UTC 解释会让东八区的
// 用户丢掉当天早上 8 小时的数据。
func parseWhen(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02", s, time.Local)
}

// stats 执行 `stats` 子命令：按维度聚合公开指标。
//
// ⚠️ **召回率的分母纪律（v0.4 硬要求）**：`RemainingAtStart` 是「起跑时还差
// 几个」之和。它可能为 0——那不是「召回率 0%」，而是**分母未知**（这些题目的
// FlagCount 没报出来，`store.recallDelta` 会把它们整体排除在分子分母之外）。
// 把分母未知印成 0% 会把「不知道」说成「一道都没解出来」，是报告层最严重的
// 一类谎话。所以见 printStats：分母为 0 时明确打出「分母未知，不得宣称召回率」。
func (a *app) stats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	f := &statsFlags{}
	f.register(fs)
	help, err := a.parseFlags("stats", fs, args)
	if help || err != nil {
		return err
	}
	q, err := f.query()
	if err != nil {
		return err
	}

	// 与 list 走同一个 store 根（absStoreDir）：run 在哪儿写、stats 去哪儿读，
	// 必须是同一个字符串。
	ports, err := a.ports(absStoreDir(f.store), harness.RunSpec{})
	if err != nil {
		return err
	}
	if ports.Results == nil {
		return notImplemented("stats")
	}
	rep, err := ports.Results.Stats(context.Background(), q)
	if err != nil {
		return err
	}
	printStats(a.out(), rep, q)
	return nil
}

// printStats 打印聚合结果。
//
// 打印顺序刻意是「样本量 → 解题 → 召回率」：任何比率在样本量为 0 时都没有意义，
// 所以 Runs 必须先出现，读者才能自己判断后面的数字该不该信。
func printStats(w io.Writer, rep harness.StatsReport, q harness.StatsQuery) {
	fmt.Fprintf(w, "运行数：%d（已完成 %d，完成率 %s）\n", rep.Runs, rep.Completed, percent(rep.CompletionRate, rep.Runs))
	fmt.Fprintf(w, "确认 flag：%d / 起跑剩余 %d\n", rep.ConfirmedFlags, rep.RemainingAtStart)
	if rep.RemainingAtStart > 0 {
		fmt.Fprintf(w, "召回率：%.1f%%（%d/%d）\n",
			rep.RecallRate*100, rep.ConfirmedFlags, rep.RemainingAtStart)
	} else {
		// ⚠️ 这一行是**结论的一部分**，不是提示语。`StatsReport.RecallRate` 在
		// 分母为 0 时是 0（store 的注释写明了「0/0 不得是 NaN」），如果这里顺手
		// 打一个 "0.0%"，读者拿到的就是「一道都没解出来」——而事实是这些题目
		// 的 FlagCount 未知，本次统计**根本算不出召回率**。
		fmt.Fprintf(w, "召回率：分母未知，不得宣称召回率（起跑剩余为 0：命中的题目均未报告 FlagCount，已整体排除在分子分母之外）\n")
	}
	fmt.Fprintf(w, "得分：%d\t成本：%.4f USD\t时长：%.0fs\n", rep.Score, rep.CostUSD, rep.DurationSeconds)
	fmt.Fprintf(w, "用过提示的运行：%d\tprovider 故障运行：%d\n", rep.HintedRuns, rep.ProviderFailures)
	if d := filterText(q); d != "" {
		fmt.Fprintf(w, "过滤条件：%s\n", d)
	}
}

// percent 打印比率。
//
// ⚠️ 样本量为 0 时**不打印 0.0%**，而是打印「无样本」：完成率的分母是 Runs，
// 与召回率是同一个坑——`0/0` 在 store 里被规约成 0，直接印出来就成了「完成率
// 0%」这个不存在的结论。
func percent(rate float64, n int) string {
	if n == 0 {
		return "无样本（分母为 0，不作结论）"
	}
	return fmt.Sprintf("%.1f%%", rate*100)
}

// filterText 回显生效的过滤条件。
//
// 为什么必须回显：一份聚合数字脱离了过滤条件就没有意义，而终端 scrollback 里
// 只看得到数字。这不是「好看」而是可复核性——报告要能证明自己算的是哪一批运行。
func filterText(q harness.StatsQuery) string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("profile", q.ProfileDigest)
	add("model", q.Model)
	add("scenario", q.Scenario)
	add("challenge", q.Challenge)
	add("category", q.Category)
	if !q.Since.IsZero() {
		parts = append(parts, "since="+q.Since.Format(time.RFC3339))
	}
	if !q.Until.IsZero() {
		parts = append(parts, "until="+q.Until.Format(time.RFC3339))
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}
