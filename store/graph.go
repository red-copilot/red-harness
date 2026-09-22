package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/legacy"
)

// 本文件是**每题产物**的布局与索引。它修的是一个具体的缺陷：图的落点过去是
// `<runDir>/graph.json`（见 `FileStore.graphPath`），而 `SaveGraph` 是**每题
// 调用一次**——一次跑两道题，第一题的图会被第二题原样盖掉，于是「跑完之后
// 还有几道题的图可看」这个问题的答案是「一道」。
//
// 修法不是「再加一个文件名」，而是让产物带上**题目身份**：
//
//	<runDir>/
//	├── graph.json                                        0600  ← 旧路径，仍可读（ReadGraph 回退）
//	├── artifacts.json                                    0600  产物索引（本文件新增）
//	└── challenges/<challengeId>/attempts/<n>/
//	    ├── graph.json                                    0600
//	    └── graph.mmd                                     0600
//
// ⚠️ **路径由 store 拼，不由装配层拼。** 如果装配层自己 `filepath.Join` 出这棵树，
// 布局就定义在两处：store 的测试再也证明不了布局（它测的是自己拼的那个），而
// 「图写到 A、索引指向 B」这类错配从此没有一条断言能挡住。与 `RunDir` 同一条
// 纪律：**布局只有一处定义**。
//
// ⚠️ **`attempts/<n>` 这一层必须保留。** `harness.FirstAttempt` 是一个带名字的
// 常量而**不是计数器**（N0 没有重试，每题一次）。今天省掉它，将来真加重试时
// reader 就要同时认「两层」与「三层」两种目录形态——而旧路径兼容已经要处理
// 一种了。代价是一个空目录层，收益是零迁移。

const (
	// challengesDirName 是 run 目录下按题目分树的入口。
	challengesDirName = "challenges"
	// attemptsDirName 是一题的第几次尝试（见 harness.FirstAttempt 的注释：
	// 它不是计数器，保留这一层只是为了将来不需要迁移已存在的 run 目录）。
	attemptsDirName = "attempts"
	// artifactsFileName 是产物索引。它是**给人看的摘要**，不是真源：
	// 「某份产物在不在」的判据永远是文件本身在不在（见 ReadGraph）。
	artifactsFileName = "artifacts.json"
)

// 产物索引的 schema 与布局标识。
//
// 两个常量都写进文件：`layout` 是布局的**版本**（`challenges/attempts/v1`），
// 而不是路径前缀——将来布局变了，读的人拿这个串就知道该按哪一版解释 `path`
// 字段，而不是靠「路径长这样」去猜。
const (
	artifactSchema = 1
	artifactLayout = "challenges/attempts/v1"
)

// GraphSaver 的装配状态，落进索引的 `graphSaver` 字段。
//
// ⚠️ 这两个值是**装配事实**（`HarnessOptions.Graphs` 在不在位），store 无从
// 自己知道——它只能由调用方（唯一同时看得见端口与 store 的装配层）声明进来。
// 与 `harness.GraphDisabled` 的关系：那个枚举值说的是「某一题图产物的实际结果」，
// 而根包明令**实现方不得返回它**（见 ports.go），所以本题级状态里不接受
// disabled，它由这里的 run 级字段承担。
const (
	ArtifactSaverWired    = "wired"
	ArtifactSaverDisabled = "disabled"
)

// attemptDir 是「一题的某次尝试」的目录路径。**布局的唯一定义点。**
//
// 不在这里做校验：调用方（ForAttempt / ReadGraph / RecordChallengeArtifacts）
// 各自先过 ValidChallengeID 与 attempt 下界，校验与拼接分开是为了让
// 「先校验、后落盘」这条顺序在代码里看得见。
func attemptDir(runDir string, cid harness.ChallengeID, n harness.AttemptID) string {
	return filepath.Join(runDir, challengesDirName, string(cid), attemptsDirName, strconv.Itoa(int(n)))
}

// artifactRelPath 返回一份产物**相对 run 目录**的路径。
//
// 索引里只放相对路径：绝对路径会把宿主上的用户名、挂载点、临时目录名写进一份
// 会被拷来拷去的产物清单里，而那份清单没有任何地方需要知道「在哪台机器上」。
//
// 用 `path.Join` 而不是 `filepath.Join`：这是**文件里**的串（json 字段），
// 分隔符必须是固定的 `/`，否则同一份索引在不同平台上形态不同。
func artifactRelPath(cid harness.ChallengeID, n harness.AttemptID, name string) string {
	return path.Join(challengesDirName, string(cid), attemptsDirName, strconv.Itoa(int(n)), name)
}

// AttemptStore 是「某次运行里某一道题的某一次尝试」的句柄。
//
// 形状与 `ForRun` 返回的 run 视图对称：**它只拥有路径**，并且它拥有的路径
// 保证是校验过的（构造者是 ForAttempt，不合法时根本不返回句柄）。
type AttemptStore struct {
	runID       harness.RunID
	challengeID harness.ChallengeID
	attempt     harness.AttemptID
	dir         string
}

// Dir 返回该次尝试的目录（已建好、0700）。
func (a *AttemptStore) Dir() string { return a.dir }

// PutGraph 原子写本题的 DAG 载荷（`graph.json`，0600）。
//
// 与 `FileStore.PutGraph` 同一条纪律：store **不理解** blob 的内容（dag 的
// schema 1 + migrate 是前向兼容的唯一实现），它只负责路径、权限与原子性。
func (a *AttemptStore) PutGraph(blob legacy.GraphBlob) error {
	p := filepath.Join(a.dir, graphFileName)
	if err := writeFileAtomic(p, blob, privatePerm); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.attempt.put_graph", RunID: a.runID,
			Msg: "写题目图失败", Err: err,
		}
	}
	return nil
}

// PutGraphExport 写本题图的人可读导出（`graph.mmd`，0600）。
//
// 0600 而不是 0644：导出里有凭证事实与目标地址，只是**形状**更适合人读
// （与 `dag.Graph.Save`、`FileStore.PutGraphExport` 同一条理由）。
func (a *AttemptStore) PutGraphExport(blob []byte) error {
	p := filepath.Join(a.dir, graphExportFileName)
	if err := writeFileAtomic(p, blob, privatePerm); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.attempt.put_graph_export", RunID: a.runID,
			Msg: "写题目图导出失败", Err: err,
		}
	}
	return nil
}

// GetGraph 读回本题的图。文件不存在 ⇒ 包装了 os.ErrNotExist 的错误。
//
// 它**不**回退到旧路径：这里问的是「这道题的图在不在」，回退是
// `FileStore.ReadGraph` 的职责（那个入口的名字就写着「按题目读、读不到再认旧
// 布局」）。让每个读入口各带一套回退逻辑，等于让「旧路径何时算数」有多个答案。
func (a *AttemptStore) GetGraph() (legacy.GraphBlob, error) {
	p := filepath.Join(a.dir, graphFileName)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读图 %s 失败: %w", p, err)
	}
	return b, nil
}

// ForAttempt 取「某次运行里某一道题的某一次尝试」的句柄，并建好目录（0700）。
//
// 校验**全部发生在 mkdirAllPrivate 之前**：任何一个参数不合法都必须
// 返回 KindConfig 且**不落到存储根上**。先建目录再校验会在存储根下留下一串
// 由非法 id 拼出来的目录——而「路径逃逸」这件事的可观测后果正是这种东西。
//
// ⚠️ `id` 是显式参数（与 `RunDir` 同），因此它在**根句柄**上调用是正常的。
// 在一个 run 视图上传另一个 run 的 id 是**写错**，不是特性：那会让句柄的
// `RunID()` 与实际落点分家，所以那种组合直接拒绝。
func (s *FileStore) ForAttempt(id harness.RunID, cid harness.ChallengeID, n harness.AttemptID) (*AttemptStore, error) {
	if err := validRunID(id); err != nil {
		return nil, err
	}
	if s.runID != "" && s.runID != id {
		return nil, &harness.Error{
			Kind: harness.KindConfig, Op: "store.for_attempt", RunID: s.runID,
			Msg: fmt.Sprintf("在 run %s 的视图上取 run %s 的尝试句柄：句柄身份与落点分家", s.runID, id),
		}
	}
	if err := validChallengeID(cid); err != nil {
		return nil, err
	}
	if err := validAttemptID(n); err != nil {
		return nil, err
	}
	dir := attemptDir(s.RunDir(id), cid, n)
	if err := mkdirAllPrivate(dir, dirPerm); err != nil {
		return nil, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.for_attempt", RunID: id,
			Msg: "建题目产物目录失败", Err: err,
		}
	}
	return &AttemptStore{runID: id, challengeID: cid, attempt: n, dir: dir}, nil
}

// validChallengeID 挡住一切不是 `harness.ChallengeIDFor` 产物的串。
//
// 判据用根包的 `ValidChallengeID`（严格：只收 64 位小写十六进制），**不在这里
// 再实现一遍**：ChallengeID 的唯一真源在根包，而它存在的理由恰恰是「第二个
// 消费者必然抄一份，抄出来的两份是同一个名字下的两种值」。这里的角色只是
// **拒绝**，不是定义。
func validChallengeID(cid harness.ChallengeID) error {
	if !harness.ValidChallengeID(string(cid)) {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.challenge_id",
			Msg: fmt.Sprintf("题目 ID %q 非法：必须是 harness.ChallengeIDFor 产出的 64 位小写十六进制", string(cid)),
		}
	}
	return nil
}

// validAttemptID 挡住 `attempts/0`、`attempts/-1` 这类路径段。
//
// 下界取 `harness.FirstAttempt` 而不是字面量 1：这个常量是「本轮唯一的 attempt
// 号」的唯一定义点，而它的值将来若变，路径段的合法下界必须跟着变。
func validAttemptID(n harness.AttemptID) error {
	if n < harness.FirstAttempt {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.attempt_id",
			Msg: fmt.Sprintf("attempt=%d 非法：必须 ≥ %d", int(n), int(harness.FirstAttempt)),
		}
	}
	return nil
}

// ── 读图：新布局优先，旧布局回退 ──

// GraphSource 说明 `ReadGraph` **实际**是从哪一处读到图的。
//
// 它存在的理由与 `harness.GraphState` 相同：一个只返回 (blob, error) 的读入口
// 无法回答「这份图是新布局的，还是旧 run 目录根上那一份」——而两者的可信度
// 不同（旧布局只有一份图，且它可能是被后一题盖过的那份）。调用方要能自己判断
// 「我读到的是不是我要的那道题」。
type GraphSource string

const (
	// GraphSourceChallenge 表示读自 `challenges/<id>/attempts/<n>/graph.json`。
	GraphSourceChallenge GraphSource = "challenge"
	// GraphSourceLegacyRunRoot 表示读自旧布局的 `<runDir>/graph.json`。
	//
	// ⚠️ 这一档的语义是「**这个 run 只有一份图**」：旧布局按 run 存，一题一图，
	// 所以一道题读到了它、另一道题也会读到同一份。它不是错误，但它**不是**
	// 「这道题的图」。
	GraphSourceLegacyRunRoot GraphSource = "legacy_run_root"
)

// ReadGraph 按题目身份读图，并**如实报告读到的是哪一种**。
//
// 顺序：先 `challenges/<cid>/attempts/<n>/graph.json`，读不到再回退
// `<runDir>/graph.json`。回退只在**文件不存在**时发生：新布局的图读不动
// （权限、IO 错误）是一个必须报出来的故障，把它折成「那就去读旧路径吧」会让
// 一次损坏表现为「这份图还是旧的那份」，静默且难查。
//
// ⚠️ **为什么不改 `GetGraph`**：它被 `_ legacy.GraphStore = (*FileStore)(nil)`
// 编译期钉死（见 store.go 末尾），改它的返回类型会把「读旧图」与「换掉 v0.3
// 端口」两件无关的事捆在一起。旧路径可读由本函数的回退负责。
//
// ⚠️ **不尝试补造已覆盖的历史图**：旧 run 里只存在一份图（后一题盖掉前一题），
// 谁也无法从它反推出「另一道题的那份」。所以一道题在旧 run 上读到的可能正是
// 别人的图——这正是 `GraphSource` 必须如实返回的原因，也是 N0.3 要修的东西。
func (s *FileStore) ReadGraph(cid harness.ChallengeID, n harness.AttemptID) (legacy.GraphBlob, GraphSource, error) {
	if s.runID == "" {
		return nil, "", &harness.Error{
			Kind: harness.KindConfig, Op: "store.read_graph",
			Msg: "根句柄不能按题目读图：先用 ForRun 取运行视图",
		}
	}
	if err := validChallengeID(cid); err != nil {
		return nil, "", err
	}
	if err := validAttemptID(n); err != nil {
		return nil, "", err
	}
	if b, err := os.ReadFile(filepath.Join(attemptDir(s.dir, cid, n), graphFileName)); err == nil {
		return b, GraphSourceChallenge, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", &harness.Error{
			Kind: harness.KindPersistence, Op: "store.read_graph", RunID: s.runID,
			Msg: "读题目图失败（文件在，但读不出来）", Err: err,
		}
	}
	legacyPath := s.graphPath()
	b, err := os.ReadFile(legacyPath)
	if err != nil {
		// 「两处都没有」必须仍然是 os.ErrNotExist：调用方靠它区分「首跑」
		// （用题目新建一张图）与「损坏」（不能悄悄新建空图）。
		return nil, "", fmt.Errorf("读图 %s 失败: %w", legacyPath, err)
	}
	return b, GraphSourceLegacyRunRoot, nil
}

// ── 产物索引 ──

// ArtifactDeclaration 是调用方对**一份产物**的声明（输入侧）。
//
// 为什么输入与登记分成两个类型：登记里的 `Path` / `SHA256` / `Bytes` 由 store
// 从布局与磁盘推导（见 RecordChallengeArtifacts），而声明里允许的只有
// 「状态」与「卡在哪一步」。合成一个类型会让调用方以为自己能指定路径——那条
// 路通向「索引指向 A、文件在 B」，而那种错配不会有任何一条断言挡得住。
type ArtifactDeclaration struct {
	// State 是产物的实际结果，取值域是根包的四态之一。⚠️ `disabled` **不接受**：
	// 它是装配事实，由 run 级的 GraphSaver 字段承担（见 ports.go 对
	// GraphDisabled 的「实现方不得返回」）。
	State harness.GraphState
	// Stage 是失败阶段（`marshal` / `write` / `export` / `unknown`），只与
	// State == failed 搭配。它是 root 包 GraphSaveFailures 的同一个值域
	// （白名单见 sanitizeGraphSaveFailures，本文件不另立一份）。
	Stage string
}

// ArtifactEntry 是**一道题一次尝试**的产物登记（输入侧）。
type ArtifactEntry struct {
	ChallengeID harness.ChallengeID
	// Code 是平台下发的题目编号。允许为空（「这次没记下编号」），它在这里只是
	// 一个便于人读的字段：路径与索引的键都是 ChallengeID。
	//
	// 为什么明文编号可以出现在这里：这份索引在 0700 的 run 目录下、0600，
	// 而 `results/<runID>.json` 本来就含 code。真正不许出现的是**候选明文**。
	Code    string
	Attempt harness.AttemptID
	// GraphSaver 是这次运行装配层有没有接 GraphSaver 端口（wired / disabled）。
	//
	// 它是 **run 级**事实，却随每次登记带进来：`HarnessOptions.Graphs` 只有
	// 装配层看得见，store 无从知道。同一个 run 内它必须恒定——两次登记给出
	// 不同的值 ⇒ KindConfig，因为那意味着调用方在描述一个不存在的部署。
	GraphSaver  string
	Graph       ArtifactDeclaration
	GraphExport ArtifactDeclaration
}

// ArtifactFile 是索引里登记的**一份产物**（输出侧）。
type ArtifactFile struct {
	State harness.GraphState `json:"state"`
	// Path 相对 run 目录（绝不出现宿主绝对路径）。
	Path string `json:"path,omitempty"`
	// SHA256 是产物字节的 sha256（小写十六进制）。
	//
	// 为什么由 store 算而不是让调用方传：调用方传的话，「索引里的摘要」与
	// 「磁盘上的字节」就成了两个来源，而它们的校验关系（摘要能否对上）正是
	// 这份索引唯一的用处——事后想确认「图有没有被拷坏」只能靠它。
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	// Stage 只在 State == failed 时出现。
	Stage string `json:"stage,omitempty"`
}

// ArtifactIndexEntry 是索引里的一道题一次尝试。
type ArtifactIndexEntry struct {
	ChallengeID string       `json:"challengeId"`
	Code        string       `json:"code,omitempty"`
	Attempt     int          `json:"attempt"`
	Graph       ArtifactFile `json:"graph"`
	GraphExport ArtifactFile `json:"graphExport,omitzero"`
}

// ArtifactIndex 是产物索引的落盘形态。
type ArtifactIndex struct {
	Schema int    `json:"schema"`
	RunID  string `json:"runId"`
	Layout string `json:"layout"`
	// GraphSaver 见 ArtifactEntry.GraphSaver。
	GraphSaver string    `json:"graphSaver"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// Challenges 按 (challengeId, attempt) 升序——顺序固定，合并才能被断言。
	Challenges []ArtifactIndexEntry `json:"challenges"`
}

// RecordChallengeArtifacts 登记一道题一次尝试的产物，**整文档原子重写**。
//
// 合并语义：同一 (challengeId, attempt) 覆盖，其余保留。两次调用之后两题都在
// ——这是 N0.3 的出口门（过去一题一图，后者盖前者）。
//
// 路径、sha256 与字节数**一律由 store 从布局与磁盘推导**，调用方只声明状态：
//
//   - `saved`  ⇒ 两份产物都必须存在且可读；读不到 ⇒ KindPersistence。
//     「账上记了 saved 但文件不在」是这份索引最不能容忍的状态：它会让人以为
//     图留下来了。
//   - `absent` / `disabled` ⇒ 不附路径与摘要（omitempty）。不查磁盘：这两个
//     状态的意思正是「没有文件」。
//   - `failed` ⇒ 附 stage；只登记**实际存在**的那一份（图写成功、导出失败时，
//     图的摘要仍然有用），不存在的那份不附。
func (s *FileStore) RecordChallengeArtifacts(entry ArtifactEntry) error {
	if s.runID == "" {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.record_artifacts",
			Msg: "根句柄不能登记产物：先用 ForRun 取运行视图",
		}
	}
	if err := validChallengeID(entry.ChallengeID); err != nil {
		return err
	}
	if err := validAttemptID(entry.Attempt); err != nil {
		return err
	}
	if entry.GraphSaver != ArtifactSaverWired && entry.GraphSaver != ArtifactSaverDisabled {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.record_artifacts", RunID: s.runID,
			Msg: fmt.Sprintf("graphSaver=%q 非法（只允许 %s / %s）",
				entry.GraphSaver, ArtifactSaverWired, ArtifactSaverDisabled),
		}
	}

	idx, err := s.readArtifactIndex()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// 索引损坏时**不覆盖**：覆盖会把别的题目的登记一起抹掉，而那份
			// 登记恰恰是「这次运行留下过什么」的唯一线索。
			return err
		}
		idx = ArtifactIndex{Schema: artifactSchema, Layout: artifactLayout}
	}
	if idx.GraphSaver != "" && idx.GraphSaver != entry.GraphSaver {
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.record_artifacts", RunID: s.runID,
			Msg: fmt.Sprintf("graphSaver 从 %s 变成 %s：装配身份在同一个 run 内不得改变",
				idx.GraphSaver, entry.GraphSaver),
		}
	}
	if entry.GraphSaver == ArtifactSaverDisabled &&
		(entry.Graph.State != harness.GraphAbsent || entry.GraphExport.State != harness.GraphAbsent) {
		// 端口没接上就不可能写出别的东西。允许这个组合等于允许索引描述一个
		// 不存在的部署——而这份索引的全部用处就是事后回答「图留下来了没有」。
		return &harness.Error{
			Kind: harness.KindConfig, Op: "store.record_artifacts", RunID: s.runID,
			Msg: "graphSaver=disabled 却声明了产物：没接端口就不会有文件",
		}
	}

	graph, err := s.describeArtifact(entry.ChallengeID, entry.Attempt, graphFileName, entry.Graph)
	if err != nil {
		return err
	}
	export, err := s.describeArtifact(entry.ChallengeID, entry.Attempt, graphExportFileName, entry.GraphExport)
	if err != nil {
		return err
	}
	row := ArtifactIndexEntry{
		ChallengeID: string(entry.ChallengeID),
		Code:        entry.Code,
		Attempt:     int(entry.Attempt),
		Graph:       graph,
		GraphExport: export,
	}

	idx.Schema = artifactSchema
	idx.Layout = artifactLayout
	idx.RunID = string(s.runID)
	idx.GraphSaver = entry.GraphSaver
	idx.UpdatedAt = time.Now().UTC()
	replaced := false
	for i := range idx.Challenges {
		if idx.Challenges[i].ChallengeID == row.ChallengeID && idx.Challenges[i].Attempt == row.Attempt {
			idx.Challenges[i] = row
			replaced = true
			break
		}
	}
	if !replaced {
		idx.Challenges = append(idx.Challenges, row)
	}
	sort.Slice(idx.Challenges, func(i, j int) bool {
		if idx.Challenges[i].ChallengeID != idx.Challenges[j].ChallengeID {
			return idx.Challenges[i].ChallengeID < idx.Challenges[j].ChallengeID
		}
		return idx.Challenges[i].Attempt < idx.Challenges[j].Attempt
	})

	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.record_artifacts", RunID: s.runID,
			Msg: "产物索引序列化失败", Err: err,
		}
	}
	if err := writeFileAtomic(s.artifactsPath(), append(b, '\n'), privatePerm); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.record_artifacts", RunID: s.runID,
			Msg: "写产物索引失败", Err: err,
		}
	}
	return nil
}

// describeArtifact 把一条声明变成一条登记（路径 / 摘要 / 字节数由磁盘推导）。
func (s *FileStore) describeArtifact(cid harness.ChallengeID, n harness.AttemptID, name string, d ArtifactDeclaration) (ArtifactFile, error) {
	stage := d.Stage
	if stage != "" && !graphStageAllowed(stage) {
		return ArtifactFile{}, &harness.Error{
			Kind: harness.KindConfig, Op: "store.record_artifacts", RunID: s.runID,
			Msg: fmt.Sprintf("失败阶段 %q 不在白名单内（marshal / write / export / unknown）", stage),
		}
	}
	af := ArtifactFile{State: d.State, Stage: stage}
	switch d.State {
	case harness.GraphSaved, harness.GraphFailed:
	case harness.GraphAbsent:
		if stage != "" {
			return ArtifactFile{}, &harness.Error{
				Kind: harness.KindConfig, Op: "store.record_artifacts", RunID: s.runID,
				Msg: "声明为 absent 却带失败阶段：两者互相矛盾",
			}
		}
		return af, nil
	default:
		// ⚠️ 包括 harness.GraphDisabled：它是装配事实，由 run 级的 graphSaver
		// 字段承担（ports.go 明令实现方不得返回它）。空串同样落这里——空串与
		// 「没声明」同形，而这份索引里「没声明」不是合法输入。
		return ArtifactFile{}, &harness.Error{
			Kind: harness.KindConfig, Op: "store.record_artifacts", RunID: s.runID,
			Msg: fmt.Sprintf("题目级产物状态 %q 非法（只允许 %s / %s / %s；disabled 是"+
				"装配事实，不属于题目级）", string(d.State),
				string(harness.GraphAbsent), string(harness.GraphSaved), string(harness.GraphFailed)),
		}
	}
	if d.State == harness.GraphFailed && stage == "" {
		return ArtifactFile{}, &harness.Error{
			Kind: harness.KindConfig, Op: "store.record_artifacts", RunID: s.runID,
			Msg: "声明为 failed 但没给失败阶段：不动的是「已写一半」，卡在哪一步必须能回答",
		}
	}
	if d.State == harness.GraphSaved && stage != "" {
		return ArtifactFile{}, &harness.Error{
			Kind: harness.KindConfig, Op: "store.record_artifacts", RunID: s.runID,
			Msg: "声明为 saved 却带失败阶段：两者互相矛盾",
		}
	}

	full := filepath.Join(attemptDir(s.dir, cid, n), name)
	st, err := os.Stat(full)
	switch {
	case err != nil && errors.Is(err, os.ErrNotExist):
		if d.State == harness.GraphSaved {
			return ArtifactFile{}, &harness.Error{
				Kind: harness.KindPersistence, Op: "store.record_artifacts", RunID: s.runID,
				Msg: "声明为 saved 但产物不存在：索引不能记一份磁盘上没有的文件", Err: err,
			}
		}
		return af, nil // failed 且文件没写出来：只记阶段
	case err != nil:
		return ArtifactFile{}, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.record_artifacts", RunID: s.runID,
			Msg: "读产物信息失败", Err: err,
		}
	}
	// 目录不是产物：saved 声明撞上一个同名目录同样是「账实不符」。
	if st.IsDir() {
		return ArtifactFile{}, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.record_artifacts", RunID: s.runID,
			Msg: "产物路径被一个目录占着：索引不能登记它",
		}
	}
	sum, err := fileSHA256(full)
	if err != nil {
		return ArtifactFile{}, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.record_artifacts", RunID: s.runID,
			Msg: "算产物摘要失败", Err: err,
		}
	}
	af.Path = artifactRelPath(cid, n, name)
	af.SHA256 = sum
	af.Bytes = st.Size()
	return af, nil
}

// ArtifactIndex 读回产物索引。文件不存在 ⇒ 包装了 os.ErrNotExist 的错误。
//
// 读回来时**逐条复检**（ChallengeID 合法、attempt 下界、path 相对且本机内）：
// 索引句柄会被 CLI 与报告消费，一份被人工改坏的索引不该把 `../..` 之类的东西
// 送进下游，也不该静默地少几行。判据都取自本文件与根包里已有的那几处。
func (s *FileStore) ArtifactIndex() (ArtifactIndex, error) {
	if s.runID == "" {
		return ArtifactIndex{}, &harness.Error{
			Kind: harness.KindConfig, Op: "store.artifact_index",
			Msg: "根句柄不能读产物索引：先用 ForRun 取运行视图",
		}
	}
	idx, err := s.readArtifactIndex()
	if err != nil {
		return ArtifactIndex{}, err
	}
	for i, row := range idx.Challenges {
		if !harness.ValidChallengeID(row.ChallengeID) {
			return ArtifactIndex{}, &harness.Error{
				Kind: harness.KindPersistence, Op: "store.artifact_index", RunID: s.runID,
				Msg: fmt.Sprintf("索引第 %d 条的 challengeId 非法（索引被人工改坏）", i+1),
			}
		}
		if row.Attempt < int(harness.FirstAttempt) {
			return ArtifactIndex{}, &harness.Error{
				Kind: harness.KindPersistence, Op: "store.artifact_index", RunID: s.runID,
				Msg: fmt.Sprintf("索引第 %d 条的 attempt=%d 非法（索引被人工改坏）", i+1, row.Attempt),
			}
		}
		for _, af := range []ArtifactFile{row.Graph, row.GraphExport} {
			if af.Path != "" && !relPathIsSafe(af.Path) {
				return ArtifactIndex{}, &harness.Error{
					Kind: harness.KindPersistence, Op: "store.artifact_index", RunID: s.runID,
					Msg: fmt.Sprintf("索引第 %d 条的路径 %q 不是 run 目录内的相对路径", i+1, af.Path),
				}
			}
		}
	}
	return idx, nil
}

// readArtifactIndex 读并解析 artifacts.json（不做 run 校验，内部也用）。
//
// 「不存在」原样往上抛（不包装）：调用方之一（RecordChallengeArtifacts）要把它
// 与「损坏」分开——「还没有索引」是首跑，「索引解析不了」绝不能顺手覆盖。
func (s *FileStore) readArtifactIndex() (ArtifactIndex, error) {
	b, err := os.ReadFile(s.artifactsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 「还没有索引」**原样**往上传：调用方靠它区分首跑与损坏
			// （损坏绝不能顺手覆盖——那会抹掉别的题目的登记）。
			return ArtifactIndex{}, err
		}
		// 其余读失败（权限、目标被一个目录占着、IO 错）都是落盘故障：
		// 裸 error 会被调用方折成 unclassified，而「索引读不动」必须能被
		// harness.IsKind(err, KindPersistence) 认出来。
		return ArtifactIndex{}, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.artifact_index", RunID: s.runID,
			Msg: "读产物索引失败", Err: err,
		}
	}
	var idx ArtifactIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return ArtifactIndex{}, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.artifact_index", RunID: s.runID,
			Msg: "产物索引解析失败（被截断或被人工改坏）", Err: err,
		}
	}
	return idx, nil
}

// artifactsPath 返回产物索引的路径。
func (s *FileStore) artifactsPath() string { return filepath.Join(s.dir, artifactsFileName) }

// relPathIsSafe 判一个索引里的路径确实是「run 目录内的相对路径」。
//
// 与 PutEvidence 的判据同向（filepath.IsLocal + 显式拒绝对路径）：两处防的是
// 同一类东西，判据也要同形，否则宽的那一处就是逃逸面。
func relPathIsSafe(p string) bool {
	if p == "" || filepath.IsAbs(p) || path.IsAbs(p) {
		return false
	}
	return filepath.IsLocal(filepath.FromSlash(p))
}

// fileSHA256 算文件字节的 sha256（小写十六进制）。
func fileSHA256(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
