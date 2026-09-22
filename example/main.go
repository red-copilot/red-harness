// Command example 是**直接调用 SDK**（而不是经 CLI）的最小示例。
//
// 它存在的理由有两个，都不是「教学」：
//
//  1. R1 的出口条件之一是「CLI 与直接 SDK 调用同口径」。这句话需要一个可运行的
//     反例来证明——单靠断言「两者都走 internal/wire」不足为凭，因为 CLI 曾经
//     在 profile / bundle 上**绕过了**装配层（`cmd/red-harness` 不传 Profile 也
//     不传 Sandbox.ProfileDir，于是 CLI 跑出来的 BundleDigest 恒为空串，而直接
//     调 SDK 的调用方拿得到它）。本文件用同一份配置跑一次，打印出的
//     ProfileDigest / BundleDigest 必须与 CLI 那次逐字相同。
//  2. 它同时演示了**公开面与私密面的分界**：能打印什么、不能打印什么。
//
// ⚠️ **它需要 Docker 与 `red-harness-runner:v0.3.0` 镜像在位**——那是 SDK 唯一
// 真实的执行路径（题目在容器里跑，agent 与工具同一个容器）。没有 Docker 时它会
// 在装配阶段就明确报错，而不是静默退化。
//
// 用法：
//
//	go run ./example -store /tmp/rh-example -bundle /path/to/extensions
//
// 注意 -bundle 是可选的；不传时 BundleDigest 为空串——那个空串的语义是
// **未核验**，不是「内容没问题」。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/internal/wire"
)

func main() {
	store := flag.String("store", "runs", "运行目录根（<store>/runs/<runID>/）")
	image := flag.String("image", "", "runner 镜像（空则用装配层缺省）")
	bundle := flag.String("bundle", "", "只读挂进容器的 extension bundle 目录（可选）")
	maxSubs := flag.Int("max-submissions", 0, "每题提交次数上限（0 表示用默认）")
	provider := flag.String("provider", "", "pi 的 --provider（必填：agent 靠它决定去哪要凭据）")
	model := flag.String("model", "", "pi 的 --model")
	flag.Parse()

	if err := run(*store, *image, *bundle, *maxSubs, *provider, *model); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(storeDir, image, bundleDir string, maxSubs int, provider, model string) error {
	// ── 装配 ──
	//
	// **只 import internal/wire，不 import 任何实现包。** 装配层是「谁把 X 交给 Y」
	// 唯一发生的地方；直接 import dag/gate/executor 会把调用方与具体实现锁在一起，
	// 而 SDK 的公开面就是根包的端口契约。
	r, err := wire.New(wire.Options{
		StoreDir: storeDir,
		Scenario: wire.ScenarioFake, // 离线演示题：不碰网络、不碰平台
		Sandbox:  wire.SandboxOptions{Image: image, ProfileDir: bundleDir},
		// 凭据只经**子进程环境变量**传递：这里放的是 provider 名，不是 key。
		// 平台 token 与 provider key 都不进 argv（argv 在 ps 里可见）。
		Agent: wire.AgentOptions{Provider: provider, Model: model},
	})
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// ── 体检 ──
	//
	// 先体检再跑：doctor 回答的是「这台机器上的装配齐不齐」（Docker 在不在、镜像
	// 在不在、跨进程锁接没接、可选端口接没接）。它在**起任何容器之前**跑，
	// 失败时给的是一条明确的配置结论，而不是一次跑到一半的运行。
	rep := r.Doctor(ctx)
	for _, c := range rep.Checks {
		mark := "ok"
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Printf("doctor %-18s %-4s %s\n", c.Name, mark, c.Detail)
	}
	if !rep.OK {
		return fmt.Errorf("doctor 未通过，按「失败要报失败」的原则不继续")
	}

	// ── 一次运行 ──
	//
	// DefaultSpec 给出「什么都不填时装配层会用什么」。下面这行**刻意显式覆盖**
	// 两项，演示「运行意图」归 RunSpec、其余归装配配置：
	spec := r.DefaultSpec()
	spec.Policy.MaxSubmissionsPerChallenge = maxSubs
	// Submit 为假时只记账不提交（干跑）。这里开着，因为演示题的平台是内存里的
	// Fake——它不需要任何凭据。
	spec.Submit = true

	res, err := r.Run(ctx, spec)
	// ⚠️ **先打印再判错**：失败路径上也需要看到进度与终态，否则一次「跑了 30 轮
	// 然后出错」的运行在终端上只剩一行错误。
	printRunResult(res)
	if err != nil {
		return err
	}

	// ── 公开结果：只有计数与指纹 ──
	if err := printPublicResult(r, res.RunID); err != nil {
		return err
	}
	// ── 私密审计：提交了什么、平台怎么判的 ──
	printPrivateAudit(storeDir, res.RunID)
	return nil
}

// printRunResult 打印**公开**的运行摘要。
//
// ⚠️ 这里刻意不碰 `res.Challenges[].Outcome.Flags` / `.Candidates`：它们是候选
// 明文，而标准输出会进终端 scrollback、工单与 CI 日志。CLI 的
// printRunResult 守着同一条纪律（且有一条专门的回归用例）。
func printRunResult(res harness.RunResult) {
	fmt.Printf("\nrun %s：终态 %s，解出=%v\n", res.RunID, res.State, res.Completed)
	for _, c := range res.Challenges {
		fmt.Printf("  %s\t%s\t进度 %d/%d\t提交 %d\t重复 %d\t判错 %d\t轮次 %d\t耗时 %s\n",
			c.Challenge.Code, c.Outcome.Reason,
			c.Outcome.ProgressConfirmed, c.Outcome.ProgressTotal,
			c.Outcome.Submitted, c.Outcome.Duplicates, c.Outcome.Rejected,
			c.Outcome.Rounds, c.Outcome.Duration().Round(time.Second))
	}
	if res.Reason != "" {
		fmt.Printf("  运行原因：%s\n", res.Reason)
	}
}

// printPublicResult 读公开结果文件并打印运行清单。
//
// 走 `ResultStore.Get`（而不是自己读文件）：公开结果的 schema 由 store 拥有，
// 调用方按结构体读不需要知道文件名与目录布局。
func printPublicResult(r *wire.Runner, runID harness.RunID) error {
	got, err := r.Results().Get(context.Background(), runID)
	if err != nil {
		return err
	}
	fmt.Printf("\n公开面（可分享）：profile=%s bundle=%s model=%s\n",
		got.ProfileDigest, orUnverified(got.BundleDigest), got.Model)
	m := got.Manifest
	fmt.Printf("运行清单：停滞阈值=%d 提交上限=%d 提示策略=%s\n",
		m.PlannerDryRounds, m.MaxSubmissions, m.HintPolicy)
	fmt.Printf("镜像：请求=%s 解析=%s pi=%s\n",
		m.RequestedImage, orUnverified(m.Image), orUnverified(m.PiVersion))
	fmt.Println("  ↑ 空串表示**未核验**，不是「没问题」——见 harness.ProbeResult.PiVersion 的注释")
	return nil
}

// printPrivateAudit 读**私密**审计：这次提交了什么、平台怎么判的。
//
// 它在 0700/0600 的 private/ 下，明文只允许出现在这一层。此处只打印指纹与前几个
// 字段——**示例不该把明文打进终端**，那正是 CLI 与 SDK 公开面纪律禁止的事。
func printPrivateAudit(storeDir string, runID harness.RunID) {
	path := filepath.Join(abs(storeDir), "private", string(runID), "submissions.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("\n私密审计：读不到（%v）——没接审计端口时这是正常的\n", err)
		return
	}
	fmt.Printf("\n私密面（不可分享）：%s\n", path)
	for i, line := range splitLines(b) {
		var rec struct {
			Fingerprint string `json:"fingerprint"`
			Correct     bool   `json:"correct"`
			Duplicate   bool   `json:"duplicate"`
			Rejected    bool   `json:"rejected"`
			Uncertain   bool   `json:"uncertain"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		fmt.Printf("  [%d] %s correct=%v duplicate=%v rejected=%v uncertain=%v\n",
			i, rec.Fingerprint, rec.Correct, rec.Duplicate, rec.Rejected, rec.Uncertain)
	}
	fmt.Println("  ↑ 只打印指纹；明文在同一个文件里，本示例刻意不把它打到终端")
}

func orUnverified(s string) string {
	if s == "" {
		return "（未核验）"
	}
	return s
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
