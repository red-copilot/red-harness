package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

// SchemaVersion 是落盘格式的版本号。**改字段语义时必须 +1**，并在 migrate 里
// 补一条迁移路径——图是断点续跑的载体，一次不兼容的格式变更会让所有历史 run
// 无法恢复（前身靠 `_blackboard.json` + `progress.py` 打补丁，正是因为没有版本号）。
const SchemaVersion = 1

// document 是落盘结构。字段全部导出且带 json tag，便于人工查看与修数据
// （真实运维里一定会有人去改 dag.json 修一道卡住的题）。
type document struct {
	Schema  int    `json:"schema"`
	Harness string `json:"harnessVersion,omitempty"`
	SavedAt string `json:"savedAt,omitempty"`

	Code     string       `json:"code"`
	Category string       `json:"category"`
	Shape    answer.Shape `json:"shape"`

	Round    int         `json:"round"`
	Seq      int         `json:"seq"`
	Nodes    []*Node     `json:"nodes"`
	Edges    []Edge      `json:"edges"`
	Rejected []Rejection `json:"rejected,omitempty"`
}

// Save 把图落盘（JSON + schema 版本）。
//
// 两个实现细节各有对应的事故：
//
//  1. **先写临时文件再 rename**。设计文档要求「每轮落盘 ⇒ 断点续跑」，而落盘
//     发生在每轮结束——进程很可能正是在这里被杀（预算到点、Ctrl-C、OOM）。
//     直接覆写会让 dag.json 停在半截，下一次 resume 读到的是半个 JSON。
//     rename 在同一文件系统上是原子的，所以读者要么看到旧版、要么看到新版。
//  2. **flag 明文不落盘**。前身事故：`MEMORY.md` / `_blackboard.json` 会带上 flag
//     原文（agent 在散文里写过、或在 negative 事实里描述过「试了 flag{guess} 失败」
//     这类内容都会漏进来）。落盘的候选/答案只留指纹：sha256[:8] + 长度 + 首尾字符
//     （前身 `_scrub_flag_plaintext` 的做法）。图里本来不该有答案（不变量 2），
//     但**描述性文本里可能出现**，所以落盘口必须再洗一遍。
//
// 洗的范围是**所有会进 prompt 或报告的自由文本**：事实内容、意图目标、证据、被拒
// 审计、以及抽取器留下的取证片段（Raw）。Raw 最容易漏——它是工具输出的原文摘录，
// 「agent 把 flag 写进文件再 cat 出来」时，flag 就在 Raw 里，而它不参与任何判定，
// 很容易被当成「无所谓的一小段」放过。
func (g *Graph) Save(path string) error {
	if path == "" {
		return errors.New("dag: Save 路径为空")
	}
	doc := document{
		Schema:  SchemaVersion,
		Harness: harness.Version,
		SavedAt: g.now().UTC().Format(time.RFC3339),
		Code:    g.Code, Category: g.Category, Shape: g.Shape,
		Round: g.Round, Seq: g.seq,
		Edges: g.edges,
	}
	for _, id := range g.order {
		doc.Nodes = append(doc.Nodes, g.scrubNode(g.nodes[id]))
	}
	for _, r := range g.rejected {
		rc := r
		rc.Content = g.scrub(rc.Content)
		doc.Rejected = append(doc.Rejected, rc)
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("dag: 序列化失败: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("dag: 建目录失败: %w", err)
		}
	}
	tmp := path + ".tmp"
	// 0600：图里虽无答案明文，但含凭证事实与目标地址，不该让同机其他用户读。
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("dag: 写临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("dag: 落盘失败: %w", err)
	}
	return nil
}

// scrubNode 返回落盘用的节点副本：洗掉答案明文、丢弃只对内存有意义的字段。
func (g *Graph) scrubNode(n *Node) *Node {
	c := cloneNode(n)
	c.Content = g.scrub(c.Content)
	c.Raw = g.scrub(c.Raw)
	c.Goal = g.scrub(c.Goal)
	c.Evidence = g.scrub(c.Evidence)
	return c
}

// scrub 把文本里出现的答案串换成指纹形式。
//
// 判据与入库口一致（同一个 Shape），所以「能被拒入图的东西」与「能被洗掉的东西」
// 是同一批——两处用不同判据会让某些串从缝隙里漏过去。
func (g *Graph) scrub(s string) string {
	if s == "" {
		return s
	}
	for _, e := range g.Shape.Envelopes {
		if e.Prefix == "" || e.Suffix == "" {
			continue
		}
		var b strings.Builder
		for i := 0; i < len(s); {
			j := strings.Index(s[i:], e.Prefix)
			if j < 0 {
				b.WriteString(s[i:])
				break
			}
			j += i
			rest := s[j+len(e.Prefix):]
			k := strings.Index(rest, e.Suffix)
			if k < 0 {
				b.WriteString(s[i:])
				break
			}
			end := j + len(e.Prefix) + k + len(e.Suffix)
			full := s[j:end]
			// 只洗「像答案」的（长度合规、内部无空白），否则会误洗正文里
			// 顺带提到的 `flag{...}` 语法示例（例如题面里的格式说明）。
			if k >= 1 && k <= answer.MaxEnvelopeLen && !strings.ContainsAny(rest[:k], " \t\r\n") {
				b.WriteString(s[i:j])
				b.WriteString(FlagFingerprint(full))
			} else {
				b.WriteString(s[i:end])
			}
			i = end
		}
		s = b.String()
	}
	return s
}

// FlagFingerprint 返回一个答案的指纹：sha256[:8] + 长度 + 首尾字符。
//
// **不给明文**是刻意的（前身 `_scrub_flag_plaintext`）：判错的 flag 要回灌进下一轮
// prompt 告诉 agent「这个不要再试」，但把明文写回去等于把答案又塞进上下文——
// 一旦上下文被压缩/落盘/上报，明文就泄漏了。指纹足够让 agent 判断「我是不是又想到
// 同一个」，又不足以让它直接抄。
//
// 应改为调用 answer.Fingerprint（唯一真源）；格式 fp:<hex8>/len=N/<首>…<尾>。
// 现在这份实现与 answer.Fingerprint **逐字同构**（同一套格式、同一套 rune 处理），
// 但它是**第二份实现**：核验发现 gate 侧的 Fingerprint 用了另一套格式
// （`<hex8>:<字符数>:<首><尾>`），两套格式并存会让「gate 说平台判错的」与
// 「dag 记下的死胡同」无法按指纹 join——同一份判错回灌在两条链路上各记一套。
// 真源已落地在 answer 包（answer/fingerprint.go），这里**先不动**（仓库所有者
// 要求统一改，且改签名会波及 gate 的调用点），届时本函数应直接转发：
//
//	func FlagFingerprint(flag string) string { return answer.Fingerprint(flag) }
//
// 或直接删除、让调用点改调 answer.Fingerprint（连同 extract_test.go 里对格式的
// 断言一起改）。保留这段说明是因为：一个「看起来一样」的副本比一个明显不同的
// 副本更危险——它会在某次单边修改后静默分叉。
func FlagFingerprint(flag string) string {
	flag = strings.TrimSpace(flag)
	sum := sha256.Sum256([]byte(flag))
	fp := hex.EncodeToString(sum[:])[:8]
	if flag == "" {
		return fmt.Sprintf("fp:%s/len=0", fp)
	}
	r := []rune(flag)
	first, last := string(r[0]), string(r[len(r)-1])
	if len(r) == 1 {
		last = first
	}
	return fmt.Sprintf("fp:%s/len=%d/%s…%s", fp, len(r), first, last)
}

// Load 从磁盘载入图。文件不存在时返回 os.ErrNotExist 包装的错误。
//
// 载入后做三件事，顺序不能换：
//  1. **迁移**（按 schema 版本）；
//  2. **重建索引**（去重索引不落盘——它是派生的，重建才能保证与节点一致；
//     落盘索引一旦与节点不同步，去重会静默失效）；
//  3. **复查不变量**（Validate）。图可能被人工编辑过、被旧版本写过、或写盘时被杀。
//
// Validate 发现问题时返回**图 + error**：调用方（Session.Run 的 resume 路径）
// 可以选择带着问题继续跑，也可以放弃重来，但必须知道这件事——静默继续跑一张
// 有脏数据的图，正是前身「61 条垃圾凭证」能一直留在库里的原因。
func Load(path string) (*Graph, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dag: 读 %s 失败: %w", path, err)
	}
	var doc document
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("dag: 解析 %s 失败: %w", path, err)
	}
	if err := migrate(&doc); err != nil {
		return nil, fmt.Errorf("dag: 迁移 %s 失败: %w", path, err)
	}

	g := &Graph{
		Code: doc.Code, Category: doc.Category, Shape: doc.Shape,
		Round: doc.Round, seq: doc.Seq, rejected: doc.Rejected,
	}
	g.init()
	if g.Shape.Empty() {
		// 老文档（或被人手改没了 shape）⇒ 用兜底形态。空 Shape 会让
		// 「答案形状内容拒入图」这条不变量彻底失效（LookLike 恒假）。
		g.Shape = answer.Infer("")
	}
	for _, n := range doc.Nodes {
		if n == nil || n.ID == "" {
			continue
		}
		c := *n
		g.nodes[c.ID] = &c
		g.order = append(g.order, c.ID)
		// 重建去重索引。同时挡住文档里的重复节点（人工合并过两份 dag.json
		// 就会发生）：后出现的同键节点被丢弃，而不是让索引指向其中一个。
		if c.IsFact() {
			k := factKey(c.FactKind, c.Content)
			if _, dup := g.factIdx[k]; !dup {
				g.factIdx[k] = c.ID
			}
		} else if c.IsIntent() {
			k := intentKey(c.IntentKind, c.Goal)
			if _, dup := g.intentIdx[k]; !dup {
				g.intentIdx[k] = c.ID
			}
		}
		// seq 必须大于所有节点的 Seq，否则新节点的 Seq 会与旧节点撞车，
		// 排序就不再是全序（平局判定会退化）。
		if c.Seq > g.seq {
			g.seq = c.Seq
		}
	}
	for _, e := range doc.Edges {
		k := edgeKey(e)
		if g.edgeSeen[k] {
			continue
		}
		g.edgeSeen[k] = true
		g.edges = append(g.edges, e)
	}

	if errs := g.Validate(); len(errs) > 0 {
		return g, fmt.Errorf("dag: 载入 %s 后发现 %d 处不变量违规（首个: %w）",
			path, len(errs), errs[0])
	}
	return g, nil
}

// LoadOrNew 是 resume 路径的入口：文件在就载入，不在就用题目新建。
//
// 它把「载入失败」分成两类：文件不存在（正常首跑 ⇒ 新建）与其余错误（必须让
// 调用方知道，不能悄悄新建一张空图——那等于把已积累的事实全部丢掉，而且
// 表现为「这道题从头开始跑」，极难察觉）。
func LoadOrNew(path string, ch harness.Challenge) (*Graph, error) {
	g, err := Load(path)
	if err == nil {
		return g, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return New(ch), nil
	}
	return nil, err
}

// migrate 把旧版本文档升级到当前 schema。
//
// 迁移函数**只前进不后退**，且每条都必须能重复执行（幂等）——因为迁移后的文档
// 会在下一次 Save 时被覆盖，而如果中途失败，下次 Load 会再迁一次。
func migrate(doc *document) error {
	if doc.Schema > SchemaVersion {
		return fmt.Errorf("schema %d 比本版本支持的 %d 更新（请升级程序，不要降级读）",
			doc.Schema, SchemaVersion)
	}
	for doc.Schema < SchemaVersion {
		switch doc.Schema {
		case 0:
			// v0 ⇒ v1：无版本号的老文档（等价于前身 `_blackboard.json` 的形态）。
			// 三件事：
			//   a) 补 shape（否则「答案形状拒入图」失效）；
			//   b) 补 trust（老节点没有这个字段，默认 host-verified——老文档里的
			//      事实全部来自宿主抽取通道）；
			//   c) **净化存量噪音凭证**（前身 `_load` 里那段 `_cred_quality_ok`
			//      过滤的直接移植）。老正则攒下的表单字段/payload 会一直留在
			//      图里影响调度，加载时净化则随重载自然生效，无竞态。
			if doc.Shape.Empty() {
				doc.Shape = answer.Infer("")
			}
			kept := doc.Nodes[:0]
			for _, n := range doc.Nodes {
				if n == nil {
					continue
				}
				if n.IsFact() {
					if n.Trust == "" {
						n.Trust = TrustHost
					}
					if n.FactKind == FactCredential && !CredQualityOK(n.Content) {
						continue // 噪音凭证：剔除，不迁移
					}
					if n.FactKind == FactNegative && n.Refutes == "" {
						// 老文档里的 negative 没有 refutes（老模型里没这个概念）。
						// 它无法把任何意图移出前沿，所以保留它只会污染「已证伪」
						// 段——但直接删又丢掉信息。这里保留内容、不当作 negative
						// 处理（降级为 artifact），让它至少可见。
						n.FactKind = FactArtifact
					}
				}
				kept = append(kept, n)
			}
			doc.Nodes = kept
			doc.Schema = 1
		default:
			return fmt.Errorf("没有从 schema %d 出发的迁移路径", doc.Schema)
		}
	}
	return nil
}

// MarshalJSON 不导出内部索引（它们是派生数据，落盘没有意义且会不同步）。
// 这里只是把 Graph 的直接序列化指向 document，防止有人直接 json.Marshal(g)
// 得到一个缺一半字段的对象。
func (g *Graph) MarshalJSON() ([]byte, error) {
	doc := document{
		Schema: SchemaVersion, Harness: harness.Version,
		SavedAt: g.now().UTC().Format(time.RFC3339),
		Code:    g.Code, Category: g.Category, Shape: g.Shape,
		Round: g.Round, Seq: g.seq, Edges: g.edges, Rejected: g.rejected,
	}
	for _, id := range g.order {
		doc.Nodes = append(doc.Nodes, g.scrubNode(g.nodes[id]))
	}
	return json.Marshal(doc)
}

// UnmarshalJSON 让 json.Unmarshal 走与 Load 相同的迁移 + 索引重建路径，
// 避免出现「用 json.Unmarshal 得到一张索引为空的图」这种静默失效。
func (g *Graph) UnmarshalJSON(b []byte) error {
	var doc document
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	if err := migrate(&doc); err != nil {
		return err
	}
	*g = Graph{Code: doc.Code, Category: doc.Category, Shape: doc.Shape,
		Round: doc.Round, seq: doc.Seq, rejected: doc.Rejected}
	g.init()
	if g.Shape.Empty() {
		g.Shape = answer.Infer("")
	}
	for _, n := range doc.Nodes {
		if n == nil || n.ID == "" {
			continue
		}
		c := *n
		g.nodes[c.ID] = &c
		g.order = append(g.order, c.ID)
		if c.IsFact() {
			g.factIdx[factKey(c.FactKind, c.Content)] = c.ID
		} else if c.IsIntent() {
			g.intentIdx[intentKey(c.IntentKind, c.Goal)] = c.ID
		}
		if c.Seq > g.seq {
			g.seq = c.Seq
		}
	}
	for _, e := range doc.Edges {
		k := edgeKey(e)
		if g.edgeSeen[k] {
			continue
		}
		g.edgeSeen[k] = true
		g.edges = append(g.edges, e)
	}
	return nil
}
