package dag

import "strconv"

// EdgeKind 是边的类别。六种边各自对应图「挣得」的一项能力，不是装饰：
//
//   - requires / produces 是**执行契约**：requires 是前置，produces 是事后连的
//     实际产出（它同时是 vuln/foothold 的证据引用目标）。
//   - enables 是**分支点**：一个事实派生多个候选意图。扁平列表只能线性追加，
//     图能保留分支并在一条失败后回到另一条。
//   - refutes 是**死胡同剪枝**：negative 事实把对应意图永久移出前沿，并且渲染进
//     prompt 时明确告知「以下方向已试过且失败，不要重复」。这是直接省钱的能力——
//     前身实测最贵的浪费就是反复重试同一路径。
//   - derived_from 是**推导链**（provenance）：每个 flag 候选能回溯到「哪条事实 →
//     哪个意图 → 哪次工具调用」。这是 gate 证据闸的信任基础。
//   - supersedes 是**换方向**：新意图取代旧意图（旧意图标 abandoned），保留
//     审计痕迹而不是删掉它。
type EdgeKind string

const (
	// EdgeRequires：intent → fact，前置条件。
	EdgeRequires EdgeKind = "requires"
	// EdgeProduces：intent → fact，实际产出（事后连）。
	EdgeProduces EdgeKind = "produces"
	// EdgeEnables：fact → intent，由事实派生的新意图。
	EdgeEnables EdgeKind = "enables"
	// EdgeRefutes：negative fact → intent，证伪。
	EdgeRefutes EdgeKind = "refutes"
	// EdgeDerivedFrom：fact → fact，推导链（provenance）。
	EdgeDerivedFrom EdgeKind = "derived_from"
	// EdgeSupersedes：intent → intent，取代（换方向）。
	EdgeSupersedes EdgeKind = "supersedes"
)

var edgeKinds = map[EdgeKind]bool{
	EdgeRequires: true, EdgeProduces: true, EdgeEnables: true,
	EdgeRefutes: true, EdgeDerivedFrom: true, EdgeSupersedes: true,
}

// Valid 报告这个边类别是否是已知类别。
func (k EdgeKind) Valid() bool { return edgeKinds[k] }

// Edge 是一条有向边。Round 是连边发生的轮次（取证用）。
type Edge struct {
	From  string   `json:"from"`
	To    string   `json:"to"`
	Kind  EdgeKind `json:"kind"`
	Round int      `json:"round,omitempty"`
}

// edgeKey 是边的去重键。同一对节点之间可以有多条不同类别的边（例如一个意图
// 既 requires 某事实、又 produces 它——虽然少见，但不是错误），但同类别重复连
// 是无意义的，去重掉。
//
// 键里**必须**带 Round：同一对端点之间同类别但不同轮次的边是**不同的事件**，
// 合并掉会丢掉「哪一轮发生了什么」——例如两条 requires 边分别是第 1 轮与第 3 轮
// 声明的两个前置，合并成一条等于少了一个前置条件（意图会在前置不全时被执行）。
// 同一轮内重复连才是无意义的重复。
func edgeKey(e Edge) string {
	return e.From + "\x00" + e.To + "\x00" + string(e.Kind) + "\x00" + strconv.Itoa(e.Round)
}
