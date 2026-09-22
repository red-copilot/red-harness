package dag

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// mermaidLabelMax 是节点/边标签的 rune 上限。
//
// 按 **rune** 切，不按 byte：图里几乎全是中文，按 byte 切会切出半个字。
const mermaidLabelMax = 60

// Mermaid 把一张图渲染成 mermaid flowchart 文本。
//
// **它是只读渲染，不引入任何调度语义**：`dag` 的「不做拓扑排序式调度、不做攻击
// 路径规划」是明令，这里只把已有的事实/意图/边/状态画出来，不改图、不排序、不
// 推导下一步。输出是给人复盘的，不是给程序读的。
//
// **它从 `document()` 渲染**——也就是 `Save` 与 `MarshalJSON` 落盘用的那份结构，
// 而不是活的 `g.nodes`。这不是实现上的顺手：它让「画的图」与「落盘的图」**在
// 构造上就是同一份**。两个后果都重要：
//
//   - 明文纪律：`document()` 已经过擦洗（`Node.Raw`/`Content`/`Goal` 里的答案串
//     换成指纹），所以这里不可能画出一份没擦洗的图。直接从活图渲染就等于开一条
//     绕过 `scrub` 的新出口——`MarshalJSON` 已经这样漏过一次。
//   - 可复盘性：事后看 `.mmd` 的人看到的，就是他打开 `graph.json` 会看到的东西。
//
// g 为 nil 时返回一个空但可解析的文档（与 `Render` 的 nil 图退化同一态度）。
func Mermaid(g *Graph) string {
	if g == nil {
		return "flowchart TD\n"
	}
	doc := g.document()
	st := g.Stats()

	var b strings.Builder
	b.WriteString("flowchart TD\n")
	fmt.Fprintf(&b, "  %%%% red-harness DAG · %s (%s)\n", oneLine(doc.Code, mermaidLabelMax), oneLine(doc.Category, mermaidLabelMax))
	fmt.Fprintf(&b, "  %%%% facts=%d intents=%d negative=%d rejected=%d\n",
		st.Facts, st.Intents, st.Negative, len(doc.Rejected))

	// 标识符**自生成**，不用 Node.ID：图是允许人工编辑的（schema 注释里写明
	// 「真实运维里一定会有人去改 dag.json」），而人工写进去的 id 可以是
	// `x-->y`、`end`、`a b` 这些 mermaid 语法。原始 id 放进标签文本里。
	ids := make(map[string]string, len(doc.Nodes))
	for i, n := range doc.Nodes {
		if n == nil {
			continue
		}
		id := "n" + strconv.Itoa(i+1)
		ids[n.ID] = id
	}
	// 悬空边：requires 允许指向不存在的事实（见 Link 的注释），端点缺失就整条
	// 跳过——画成占位节点会让图与事实不符。
	abandonCause := abandonedCauses(doc.Edges)

	for i, n := range doc.Nodes {
		if n == nil {
			continue
		}
		id := "n" + strconv.Itoa(i+1)
		open, close := mermaidShape(n)
		fmt.Fprintf(&b, "  %s%s\"%s\"%s\n", id, open, mermaidLabel(n, abandonCause[n.ID]), close)
	}
	for _, e := range doc.Edges {
		from, okFrom := ids[e.From]
		to, okTo := ids[e.To]
		if !okFrom || !okTo {
			continue
		}
		arrow, label := mermaidEdge(e)
		round := ""
		if e.Round > 0 {
			round = " ·r" + strconv.Itoa(e.Round)
		}
		fmt.Fprintf(&b, "  %s %s|%s%s| %s\n", from, arrow, label, round, to)
	}
	writeMermaidClasses(&b, doc.Nodes)
	return b.String()
}

// mermaidShape 用形状区分两大类节点：事实是方框、意图是胶囊。
//
// 返回**成对的**定界符，不是只有左括号：只返回左括号就得在调用处硬编右括号，而
// 胶囊的右括号是 `])` 不是 `]`——两处一旦对不上就是语法错误（`n1[["x"]`），
// 而本仓库没有 mermaid 解析器（零第三方依赖），golden 是唯一防线。
func mermaidShape(n *Node) (open, close string) {
	if n.Kind == NodeIntent {
		return "([", "])"
	}
	return "[", "]"
}

// nodeClass 返回节点该挂的样式类。
//
// 只取**最显著的那一个轴**：目标是「一眼看出图里哪些是死胡同、哪些是被放弃的
// 方向」，不是把四个轴编码成四种样式组合——组合出来的图没人读得动。
func nodeClass(n *Node) string {
	if n.Kind == NodeIntent {
		return "intent_" + string(n.State)
	}
	if n.FactKind == FactNegative {
		return "fact_dead"
	}
	switch n.Trust {
	case TrustAgent:
		return "fact_agent"
	case TrustInferred:
		return "fact_inferred"
	default:
		return "fact_host"
	}
}

// mermaidLabel 组装节点标签。
//
// 意图的标签带上「为什么它不在了」——这是这张图最值得回答的问题：一个 abandoned
// 的意图是被负面事实剪掉的、被别的方向取代的，还是编排层因为停滞把它扔掉的？
// 三者的改法完全不同（前者是题目事实，中者是策略正常运转，后者要调阈值）。
func mermaidLabel(n *Node, abandonCause string) string {
	var parts []string
	// 原始 id 放进标签：人拿 `.mmd` 与 `graph.json` 对照时，靠的就是这些 id
	// （`f2`/`i3`）——而标识符本身是自生成的 `n1..nk`，两者对不上就没法对照。
	// 它必须**转义后**进标签：id 是图里唯一允许人工随意填的字段。
	if n.ID != "" {
		parts = append(parts, n.ID)
	}
	if n.Kind == NodeIntent {
		parts = append(parts, "["+string(n.IntentKind)+"] "+n.Goal)
		// 只在**真的尝试过**之后才显示次数：MaxAttempts 有缺省值 3，于是每个
		// 没跑过的意图都会挂一个 `0/3`——一屏噪音，而它不携带任何信息。
		if n.Attempts > 0 {
			parts = append(parts, strconv.Itoa(n.Attempts)+"/"+strconv.Itoa(n.MaxAttempts))
		}
		if abandonCause != "" {
			parts = append(parts, abandonCause)
		} else if n.State == IntentAbandoned {
			// 被放弃但**没有任何入边**说明原因：那是编排层自己判的（连续停滞
			// 后 Abandon），不是事实层发现的死胡同。三种成因必须能分辨，所以
			// 这一档要显式说出来，而不是留空。
			parts = append(parts, "因停滞放弃")
		}
	} else {
		parts = append(parts, "["+string(n.FactKind)+"] "+n.Content)
		if n.FactKind == FactNegative {
			parts = append(parts, "已证伪")
		}
		if n.Confidence > 0 && n.Confidence < 1 {
			parts = append(parts, "c"+strconv.FormatFloat(n.Confidence, 'g', 2, 64))
		}
	}
	return escapeMermaidLabel(strings.Join(parts, " · "))
}

// abandonedCauses 扫边得出每个 abandoned 意图的成因。
//
// 为什么必须扫边：`Scheduler` 没有 supersedes 的读访问器，而「被证伪剪枝」与
// 「被换方向取代」在这张图上恰恰是两件不同的事（前者说明事实层发现了死胡同，
// 后者说明编排层主动换了方向）。两者都没有入边的 abandoned，才是「停滞放弃」。
func abandonedCauses(edges []Edge) map[string]string {
	out := make(map[string]string)
	for _, e := range edges {
		switch e.Kind {
		case EdgeRefutes:
			out[e.To] = "被证伪剪枝"
		case EdgeSupersedes:
			if out[e.To] == "" {
				out[e.To] = "被换方向取代"
			}
		}
	}
	return out
}

// mermaidEdge 给出箭头的形状与标签。
func mermaidEdge(e Edge) (string, string) {
	switch e.Kind {
	case EdgeRefutes:
		return "-.->", "refutes"
	case EdgeDerivedFrom:
		return "-.->", "derived"
	case EdgeSupersedes:
		return "==>", "supersedes"
	case EdgeProduces:
		return "-->", "produces"
	case EdgeEnables:
		return "-->", "enables"
	default:
		return "-->", string(e.Kind)
	}
}

// writeMermaidClasses 只写这张图**真的用到的**样式类。
//
// 全量写死会让每份 .mmd 都带一屏用不上的 classDef（十四种状态里通常只有三四种
// 出现），把真正要看的东西淹掉。顺序按类名排序，保证输出稳定。
func writeMermaidClasses(b *strings.Builder, nodes []*Node) {
	used := map[string]bool{}
	for _, n := range nodes {
		if n != nil {
			used[nodeClass(n)] = true
		}
	}
	names := make([]string, 0, len(used))
	for name := range used {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(b, "  classDef %s %s\n", name, mermaidClassStyle(name))
	}
	if len(names) == 0 {
		return
	}
	// 按类聚合节点，减少 class 行数。
	byClass := map[string][]string{}
	for i, n := range nodes {
		if n == nil {
			continue
		}
		id := "n" + strconv.Itoa(i+1)
		c := nodeClass(n)
		byClass[c] = append(byClass[c], id)
	}
	for _, name := range names {
		fmt.Fprintf(b, "  class %s %s\n", strings.Join(byClass[name], ","), name)
	}
}

// mermaidClassStyle 是样式表。**只表达「能不能信」与「还活着吗」两件事。**
func mermaidClassStyle(name string) string {
	switch name {
	case "fact_host":
		return "fill:#e8f5e9,stroke:#2e7d32" // 宿主指纹匹配，最可信
	case "fact_agent":
		return "fill:#fff8e1,stroke:#f9a825,stroke-dasharray: 5 5" // agent 自述
	case "fact_inferred":
		return "fill:#f5f5f5,stroke:#9e9e9e,stroke-dasharray: 2 3" // 推导而来
	case "fact_dead":
		return "fill:#ffebee,stroke:#c62828" // 死胡同
	case "intent_done":
		return "fill:#c8e6c9,stroke:#2e7d32"
	case "intent_active":
		return "fill:#bbdefb,stroke:#1565c0,stroke-width:3px"
	case "intent_failed":
		return "fill:#ffe0b2,stroke:#ef6c00"
	case "intent_blocked":
		return "fill:#f5f5f5,stroke:#9e9e9e,stroke-dasharray: 5 5"
	case "intent_abandoned":
		return "fill:#eeeeee,stroke:#616161,stroke-dasharray: 3 3"
	case "intent_interrupted":
		return "fill:#ffe0b2,stroke:#8d6e63,stroke-dasharray: 2 2"
	case "intent_pending":
		return "fill:#ffffff,stroke:#424242"
	default:
		// 未知状态（未来版本写的、人工编辑的）也要有样式，否则 mermaid 会因为
		// 引用未定义的类而报错——图宁可比实际信息少，也不能渲染不出来。
		return "fill:#ffffff,stroke:#424242"
	}
}

// escapeMermaidLabel 把任意文本折成一行、可安全放进 `["…"]` 的标签。
//
// 三条规则，每条都对应一种真的会把图弄坏或弄错的输入：
//
//  1. **控制字符与换行折成空格**。mermaid 逐行解析，标签里一个换行会把整个节点
//     声明截断——而 agent 的工具输出里换行是常态。
//  2. **单趟替换 `#` 与 `"`**。`"` 是标签终止符；`#` 是 mermaid 的实体起始符，
//     不转义它，正文里原本的 `#` 会和后面的字符凑成一个实体、渲染成别的字
//     （`flag` 后面跟 `{` 之类的内容在安全题里到处都是）。必须在**同一趟**里做：
//     先转 `"` 再转 `#` 会把刚生成的 `#quot;` 再毁成 `#35;quot;`。
//  3. **截断按 rune**，超出加省略号。
//
// 中文、全角标点原样输出：mermaid 认，转成 \uXXXX 反而不认。
func escapeMermaidLabel(s string) string {
	s = oneLine(s, mermaidLabelMax)
	if s == "" {
		return "(无内容)"
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '#':
			b.WriteString("#35;")
		case '"':
			b.WriteString("#quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// oneLine 把文本折成一行并按 rune 截断。
func oneLine(s string, max int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	runes := []rune(out)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return out
}
