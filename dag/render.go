package dag

import (
	"fmt"
	"sort"
	"strings"

	harness "github.com/red-copilot/red-harness"
)

// RenderInput 是渲染一轮 prompt 所需的全部输入。
//
// 为什么把「已确认 flag 数」与「判错指纹」单独传，而不是从图里读：
// **图里根本没有它们**。答案不是事实（不变量 2），已确认数与判错账本属于 gate
// 与 Session。渲染器读图只能读到「已知事实/死胡同/前沿」，其余必须由调用方注入。
type RenderInput struct {
	// Challenge 是题目（编号/类别/难度/题面）。
	Challenge harness.Challenge
	// Confirmed 是已被平台确认的 flag 数。
	Confirmed int
	// Rejected 是判错答案的**指纹**（绝不传明文）。见 FlagFingerprint。
	Rejected []string
	// Intent 是本轮要渲染的意图。为 nil 时渲染「收尾」段（前沿耗尽）。
	Intent *Node
	// MaxFacts 限制「已知事实」段的条目数（<=0 时用 DefaultMaxFacts）。
	// 前身教训：噪音事实会把真信号挤没，所以这一段必须有硬上限。
	MaxFacts int
	// MaxNegative 限制「已证伪」段的条目数（<=0 时用 DefaultMaxNegative）。
	MaxNegative int
}

// DefaultMaxFacts / DefaultMaxNegative 是渲染段的条目上限。
//
// 为什么要有上限（前身 B14 的直接教训）：前身 `actionable_assets()` 取 top-5 凭证
// 是因为不设限会撑爆 prompt，但它取的是**前 5 条**——而垃圾事实恰好排在最前面，
// 真信号被挤掉。本实现按 (类别优先级, 置信度, 轮次) 排序后再截断，让「最可能有用
// 的事实」排在前面，而不是「最早出现的」。
const (
	DefaultMaxFacts    = 24
	DefaultMaxNegative = 12
)

// factPriority 决定事实在「已知事实」段里的排序优先级（小的靠前）。
//
// 顺序的依据是「agent 拿到它之后能立刻行动」：
// 凭证和立足点直接可用，漏洞次之（要写 exploit），目标/服务/路径是背景，
// negative 单独成段（不属于「可直接使用」）。
func factPriority(k FactKind) int {
	switch k {
	case FactFoothold:
		return 0
	case FactCredential:
		return 1
	case FactVuln:
		return 2
	case FactService:
		return 3
	case FactTarget:
		return 4
	case FactArtifact:
		return 5
	case FactNegative:
		return 9
	}
	return 8
}

// Render 是**纯函数**：输入 Graph + Challenge + 已确认数 + 判错指纹，输出本轮
// prompt 片段。它不读时钟、不读环境、不写任何东西——所以能做 golden 测试，
// 而 golden 测试是这类「拼字符串」的代码唯一可靠的回归手段。
//
// 段的形状与设计文档 §一「每轮 prompt 的形状」一一对应：
//
//	## 本题 / ## 已知事实（有来源，可直接使用）/ ## 已证伪，不要再试 /
//	## 本轮意图（只做这一件事）/ ## 交付
//
// 顺序刻意是「先环境、再已知、再禁区、最后任务」：把「本轮意图」放在最后，
// 是因为长上下文里模型对末尾的指令依从度最高，而「只做这一件事」正是最需要
// 被遵守的约束（前身最贵的浪费就是 agent 在同一轮里同时开好几条线，每条都做一半）。
func Render(g *Graph, in RenderInput) string {
	var b strings.Builder

	if g == nil {
		// 图还没建（例如平台 Start 失败就进入渲染）不该让整个驱动进程崩掉：
		// 渲染是纯函数，退化成「只有题面与交付约定」比 panic 有用得多。
		writeHeader(&b, g, in)
		writeIntent(&b, in)
		writeDelivery(&b, g, in)
		return b.String()
	}

	writeHeader(&b, g, in)
	writeFacts(&b, g, in)
	writeNegative(&b, g, in)
	writeIntent(&b, in)
	writeDelivery(&b, g, in)

	return b.String()
}

// writeHeader 渲染「本题」段。
func writeHeader(b *strings.Builder, g *Graph, in RenderInput) {
	ch := in.Challenge
	b.WriteString("## 本题\n")
	fmt.Fprintf(b, "编号：%s", ch.Code)
	if ch.Category != "" {
		fmt.Fprintf(b, "   类别：%s", ch.Category)
	}
	if ch.Difficulty != "" {
		fmt.Fprintf(b, "   难度：%s", ch.Difficulty)
	}
	if ch.FlagCount > 0 {
		fmt.Fprintf(b, "   进度：flag %d/%d 已确认", in.Confirmed, ch.FlagCount)
	} else if in.Confirmed > 0 {
		fmt.Fprintf(b, "   进度：flag %d 已确认", in.Confirmed)
	}
	b.WriteString("\n")

	// 目标地址取 FactTarget 事实（而不是 Challenge.Addrs）——因为抽取器后来可能
	// 发现**新的**地址（内网跳板、其他端口），那些才是真正要打的。Challenge.Addrs
	// 只有平台起容器时给的那一个。
	var addrs []string
	if g != nil {
		for _, f := range g.factsIn(FactTarget) {
			addrs = append(addrs, f.Content)
		}
	}
	if len(addrs) == 0 && len(ch.Addrs) > 0 {
		addrs = ch.Addrs
	}
	if len(addrs) > 0 {
		fmt.Fprintf(b, "目标地址：%s\n", strings.Join(addrs, ", "))
	}
	if d := strings.TrimSpace(ch.Description); d != "" {
		fmt.Fprintf(b, "题面：%s\n", d)
	}
	b.WriteString("\n")
}

// writeFacts 渲染「已知事实」段。
//
// 每条事实都带来源（`← bash: nmap -sV …`）与信任层。**这不是装饰**：agent 需要
// 知道哪条事实是宿主从工具输出里读到的（可信）、哪条是它自己上一轮申报的
// （可能错）——前身把它们混在一起，于是 agent 会照着自己上轮的猜测继续往下推。
func writeFacts(b *strings.Builder, g *Graph, in RenderInput) {
	facts := g.factsIn("")
	var usable []*Node
	for _, f := range facts {
		if f.FactKind == FactNegative {
			continue // 单独成段
		}
		usable = append(usable, f)
	}
	if len(usable) == 0 {
		return
	}
	sort.SliceStable(usable, func(i, j int) bool {
		pi, pj := factPriority(usable[i].FactKind), factPriority(usable[j].FactKind)
		if pi != pj {
			return pi < pj
		}
		if usable[i].Confidence != usable[j].Confidence {
			return usable[i].Confidence > usable[j].Confidence
		}
		return usable[i].Seq < usable[j].Seq
	})
	limit := in.MaxFacts
	if limit <= 0 {
		limit = DefaultMaxFacts
	}
	shown := usable
	hidden := 0
	if len(shown) > limit {
		hidden = len(shown) - limit
		shown = shown[:limit]
	}

	b.WriteString("## 已知事实（有来源，可直接使用）\n")
	for _, f := range shown {
		fmt.Fprintf(b, "- %s\n", factLine(f))
	}
	if hidden > 0 {
		// 明说还有多少条被省略。不说的话 agent 会以为「就这些」，
		// 从而重复做已经做过的事（它没法知道自己漏看了什么）。
		fmt.Fprintf(b, "- …另有 %d 条低优先事实未列出（图里已记录，可按需查询）\n", hidden)
	}
	b.WriteString("\n")
}

// factLine 渲染单条事实。来源与信任层必须同时出现——只给来源不给信任层，
// agent 无法区分「宿主从输出里读到的」与「它自己申报的」。
func factLine(f *Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", f.FactKind, f.Content)
	if f.Source != "" {
		fmt.Fprintf(&b, "   ← %s", f.Source)
	}
	if f.Trust == TrustAgent {
		fmt.Fprintf(&b, "（agent 申报，confidence %.1f）", f.Confidence)
	} else if f.Trust == TrustInferred {
		b.WriteString("（推断，未经工具输出验证）")
	}
	if f.FactKind == FactCredential && f.Confidence < 0.7 {
		// 低置信凭证要显式标出来：前身把「可能的凭证」当「已发现凭证」注入，
		// agent 于是拿着一个错误的密码反复试（每一轮都在烧钱）。
		b.WriteString("（未验证）")
	}
	return b.String()
}

// writeNegative 渲染「已证伪，不要再试」段。
//
// 这一段是图**直接省钱**的地方。前身只能靠 stoploss 的「连续 N 轮无新事实」
// 整体刹车，无法表达「这条具体路径已证伪」，于是 agent 反复重试同一路径——
// 前身实测最贵的浪费就是这个。措辞上必须明确「不要再试」而不是「仅供参考」：
// 模糊的提示会被模型当成「可以再确认一下」。
func writeNegative(b *strings.Builder, g *Graph, in RenderInput) {
	if g == nil {
		return
	}
	neg := g.factsIn(FactNegative)
	if len(neg) == 0 {
		return
	}
	limit := in.MaxNegative
	if limit <= 0 {
		limit = DefaultMaxNegative
	}
	shown := neg
	hidden := 0
	if len(shown) > limit {
		hidden = len(shown) - limit
		shown = shown[:limit]
	}
	b.WriteString("## 已证伪，不要再试\n")
	for _, f := range shown {
		src := ""
		if f.Refutes != "" {
			// 带上「源自哪个意图」：agent 看到「源自 intent:recon-2」才知道
			// 这是上一轮具体做过的哪件事失败了，而不是一句无主的断言。
			src = fmt.Sprintf("（源自 intent:%s）", f.Refutes)
		}
		fmt.Fprintf(b, "- %s %s\n", f.Content, src)
	}
	if hidden > 0 {
		fmt.Fprintf(b, "- …另有 %d 条已证伪方向未列出\n", hidden)
	}
	b.WriteString("\n")
}

// writeIntent 渲染「本轮意图」段。
func writeIntent(b *strings.Builder, in RenderInput) {
	b.WriteString("## 本轮意图（只做这一件事）\n")
	if in.Intent == nil {
		// 前沿耗尽：所有方向都试过或都已证伪。这时不该给 agent 一个模糊的
		// 「继续找找」，而要它把已知信息做一次收口——常见结果是它发现
		// 「其实我已经拿到答案了，只是没提交」。
		b.WriteString("（前沿已空：没有未尝试、未被证伪的方向了。）\n")
		b.WriteString("请基于已有事实做一次收口：复核所有已知线索，若已有答案候选，" +
			"按下面的交付约定写出；若确认无解，明确说明卡在哪里。\n\n")
		return
	}
	it := in.Intent
	fmt.Fprintf(b, "[%s] %s\n", it.IntentKind, it.Goal)
	if len(it.Expect) > 0 {
		ks := make([]string, 0, len(it.Expect))
		for _, k := range it.Expect {
			ks = append(ks, string(k))
		}
		fmt.Fprintf(b, "预期产出：%s 事实。\n", strings.Join(ks, " 或 "))
	} else {
		b.WriteString("预期产出：能推进本方向的新事实。\n")
	}
	if it.Attempts > 0 {
		// 告诉 agent 这是第几次尝试。前身没有这个信息，agent 每次都以
		// 「第一次做」的姿态重新开始，于是把同样的命令又跑一遍。
		fmt.Fprintf(b, "这是本方向的第 %d 次尝试（上限 %d）。\n", it.Attempts+1, it.attemptCap())
	}
	b.WriteString("若失败，请申报一条 negative 事实说明失败原因（这样下一轮不会再试同一条路）。\n\n")
}

// writeDelivery 渲染「交付」段。
//
// 措辞直接取前身 `_INTRANET_ORCHESTRATION` 规则 2 的原话——「答案格式以题目描述
// 为准……不要自己加外壳」。这句是前身少数被验证有效的提示词之一：模型默认会给
// 任何答案套上 `flag{...}`（因为它见过的 CTF 大多如此），而平台要求裸密码时
// 加了外壳的答案会被判错——判错会**扣分**，而且会被记进判错账本。
func writeDelivery(b *strings.Builder, g *Graph, in RenderInput) {
	b.WriteString("## 交付\n")
	b.WriteString("找到答案立即 `printf '%s\\n' '答案' >> FLAG`（追加，不要覆盖）。\n")
	b.WriteString("答案格式以题目描述为准——题目要求 `flag{...}` 就写 `flag{...}`，" +
		"要求密码/hash/密钥就写原始值，不要自己加外壳。\n")
	if len(in.Rejected) > 0 {
		// 判错账本回灌：**只给指纹不给明文**（前身 `_scrub_flag_plaintext`）。
		// 给明文等于把答案又写回上下文，一旦上下文被压缩/落盘/上报就泄漏了；
		// 指纹足以让 agent 判断「我是不是又想到同一个」。
		fmt.Fprintf(b, "以下候选已被平台判错，不要再提交（只给指纹）：%s\n",
			strings.Join(in.Rejected, ", "))
	}
	if in.Intent != nil {
		b.WriteString("完成后用 report_fact 申报本轮事实（含失败原因）。\n")
	}
}
