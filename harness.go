package harness

import (
	"fmt"
	"time"
)

// Version 是 SDK 版本。它被写进 dag 的 schema 文档（`dag/store.go` 消费它做
// 前向兼容判断），所以改版本号会影响旧图的读取路径——不要随手改。
const Version = "0.3.0"

const (
	// HintOff 从不请求提示。
	HintOff = "off"
	// HintAuto 连续 dry 轮次达阈值且本题未用过提示时请求一次。
	HintAuto = "auto"
	// HintAlways 每道题开始就请求一次提示。
	//
	// v0.2 的文档这么写，但代码里**没有守卫**（`maybeHint` 只对 HintAuto 判
	// `HintUsed > 0`），于是它每轮都请求、每轮都扣分。v0.3 修正为实现与文档一致：
	// HintAlways 同样受「每题一次」守卫。
	HintAlways = "always"
)

// IntentRef 是 Planner 眼里一个意图的最小视图。
//
// 定义在 harness 而不是 dag，是为了让根包不依赖 dag（避免循环依赖：
// dag 需要引用 harness 的 Challenge/OutcomeView）。
type IntentRef struct {
	ID   string
	Kind string
	Goal string
	// Round 是这个意图被创建时的轮号。
	Round int
}

// DefaultPrompt 渲染「本题」段。
//
// 答案格式说明以题面为准——前身 `_INTRANET_ORCHESTRATION` 规则 2 的原话：
// 题目要求 flag{...} 就写 flag{...}，要求密码/hash/密钥就写原始值，
// 不要自己加外壳。
func DefaultPrompt(c Challenge, s StartResult) string {
	p := fmt.Sprintf("目标：%s", c.Code)
	if c.Description != "" {
		p += "\n题面：" + c.Description
	}
	if len(s.Addrs) > 0 {
		p += fmt.Sprintf("\n地址：%v", s.Addrs)
	}
	if c.Category != "" {
		p += "\n类别：" + c.Category
	}
	if c.FlagCount > 0 {
		p += fmt.Sprintf("\nflag 数：%d（已确认 %d）", c.FlagCount, c.Solved)
	}
	p += "\n答案格式：" + AnswerFormatHint(c)
	return p
}

// AnswerFormatHint 生成答案格式说明。绝不硬塞 flag{...}——题目没说的形态
// 就不能替它假设。
//
// v0.2 里这个函数是未导出的 `answerFormatHint`，而 `DefaultFlagFormat`
// （`solver.go:44` 的 `"flag{...}"`）从未被引用。v0.3 删掉那个死常量，
// 并把渲染入口导出——`Renderer` 的实现（dag 包）需要它。
func AnswerFormatHint(c Challenge) string {
	if c.FlagFormat != "" {
		return c.FlagFormat
	}
	return "以题面为准（可能是 flag{...}，也可能是密码 / hash / 密钥的原始值，不要自己加外壳）"
}

// DefaultRoundTimeout 是单轮的墙钟上限。0 表示不设。
//
// 为什么单列一个常量：轮级超时与运行级预算（Budget.MaxWall）是两件事。
// 轮级超时保护的是「一轮卡死」，运行级预算保护的是「整体花超」。
const DefaultRoundTimeout = 20 * time.Minute
