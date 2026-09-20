package harness

import "context"

// Challenge 是平台视角的一道题。
type Challenge struct {
	Code string
	// Description 是题面。它决定答案形态（flag{...} 还是密码/hash/密钥），
	// 也是 DefaultPrompt 与 dag.Render 的输入之一。
	Description string
	// Category 决定目标链（recon→exploit→… 还是 analyze→solve）。
	// 平台不下发时由 tsec 包从题面关键词推断。
	Category   string
	Difficulty string
	FlagCount  int
	Solved     int
	// Addrs 在 list 时若容器已 available 就有值；否则为空，等 Start 之后填。
	Addrs []string
	// FlagFormat 是平台明确下发的答案格式（可能为空 ⇒ 由题面推断）。
	FlagFormat string
}

func (c Challenge) Remaining() int {
	if c.FlagCount <= c.Solved {
		return 0
	}
	return c.FlagCount - c.Solved
}

// Done 报告这道题是否已全部完成。
func (c Challenge) Done() bool { return c.FlagCount > 0 && c.Solved >= c.FlagCount }

type StartResult struct {
	Code string
	// Addrs 是容器直连地址（IP:端口）。可能为空——平台启动是异步的，
	// 调用方需要轮询（tsec 包负责）。
	Addrs []string
	// Description 在这里补上，修掉旧契约里「Challenge.Description 是死路径」
	// 的缺陷：启动后平台可能给出比 list 时更完整的题面。
	Description string
}

type SubmitResult struct {
	Correct   bool
	Awarded   int
	Duplicate bool
	Message   string
	// 进度回填，便于调用方不必再 list 一次。
	CorrectFlagCount int
	TotalFlagCount   int
	MatchedIndex     int
}

type HintResult struct {
	Code string
	Hint string
}

type CloseResult struct {
	Code   string
	Closed bool
}

// Platform 是平台适配器接口。实现见 platform 包（走 tsec_bridge.py）。
type Platform interface {
	List(ctx context.Context) ([]Challenge, error)
	Start(ctx context.Context, code string) (StartResult, error)
	Hint(ctx context.Context, code string) (HintResult, error)
	Submit(ctx context.Context, code, flag string) (SubmitResult, error)
	Close(ctx context.Context, code string) (CloseResult, error)
}

// HealthChecker 是可选接口：平台适配器若需要在任何真实调用之前做连通性预检
// （tsecbench 的 VPN 预检就是这个语义），实现它。Session.Run 在起题之前调用。
type HealthChecker interface {
	Health(ctx context.Context) error
}
