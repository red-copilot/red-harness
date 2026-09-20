package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

// 本文件是**候选明文账本**。它是整个仓库里唯一允许出现候选明文的地方
// （设计文档 §3.3：事实层与答案层严格分离）。
//
// 为什么把它单列一个包内文件而不是混进 store.go：这个文件的每一行都值得
// 被审一遍——它是明文唯一的落点，也是「账本缺失」这类 fail-closed 判定的
// 所在（Review Focus #2）。

// ledgerRecord 是账本里的一行。
//
// 为什么用**追加日志**而不是覆写 JSON：账本要回答的问题是「这个候选提交过
// 吗、平台怎么判的」，而这个问题在崩溃后必须仍然可答。追加日志 + 一次
// write 写完整行的性质与 events.jsonl 相同：要么整行在、要么整行不在。
// 覆写式账本在写盘中途被杀会同时丢掉「已提交」与「已判错」两组事实，而
// 那正是最需要它们的时候。
type ledgerRecord struct {
	Flag       string             `json:"flag"`
	Source     string             `json:"source,omitempty"`
	Output     string             `json:"output,omitempty"`
	Confidence float64            `json:"confidence,omitempty"`
	Provenance harness.Provenance `json:"provenance,omitempty"`
	ToolCallID string             `json:"toolCallId,omitempty"`
	IntentID   string             `json:"intentId,omitempty"`
	Round      int                `json:"round,omitempty"`
	Submitted  bool               `json:"submitted,omitempty"`
	Correct    bool               `json:"correct,omitempty"`
	Duplicate  bool               `json:"duplicate,omitempty"`
	// RejectReason 是族别归因（gate 说它为什么可疑）。
	RejectReason string `json:"rejectReason,omitempty"`
	// SubmitError 是平台侧判错/出错信息。与上一条分开：报告要能同时回答
	// 「gate 为什么认为它可疑」和「平台为什么拒它」两个问题。
	SubmitError string `json:"submitError,omitempty"`
}

// evidenceStore 实现 harness.EvidenceStore。
type evidenceStore struct {
	runDir string
}

// PutCandidate 追加一条候选明文记录。
//
// **账本缺失时返回 KindPersistence，而不是「顺手建一个」**——见 Rejected
// 的注释：恢复路径上「账本不在」必须 fail closed，所以这里也不能悄悄补一个
// 空账本出来。首次写入（run 刚创建）时账本由 New/ForRun 建好，不走这条路径。
func (e *evidenceStore) PutCandidate(c harness.Candidate) error {
	if err := e.requireLedger(); err != nil {
		return err
	}
	rec := ledgerRecord{
		Flag: c.Flag, Source: c.Source, Output: c.Output,
		Confidence: c.Confidence, Provenance: c.Provenance,
		ToolCallID: c.ToolCallID, IntentID: c.IntentID, Round: c.Round,
		Submitted: c.Submitted, Correct: c.Correct, Duplicate: c.Duplicate,
		RejectReason: c.RejectReason, SubmitError: c.SubmitError,
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.put_candidate",
			Msg: "候选记录序列化失败", Err: err,
		}
	}
	if err := appendLine(e.ledgerPath(), line, 0o600); err != nil {
		return err
	}
	return nil
}

// PutEvidence 写一份原始工具输出到 private/evidence/<ref>。
//
// ref 由 gate 的取证串给出（工具调用 id 之类），**必须挡住路径逃逸**：
// ref 一旦含 `../` 就能写到 private/ 之外，而 private/ 之外的目录是公开面
// （report.*、run.json 就在那里）。这里用 filepath.IsLocal 判据 + 显式
// 拒绝绝对路径，两者缺一不可（IsLocal 对绝对路径返回 false，但把两件事分开
// 写能让后人一眼看出防的是哪一类）。
func (e *evidenceStore) PutEvidence(ref string, data []byte) error {
	full, err := e.evidencePath(ref)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.put_evidence",
			Msg: "建证据目录失败", Err: err,
		}
	}
	if err := os.Chmod(filepath.Dir(full), 0o700); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.put_evidence",
			Msg: "chmod 证据目录失败", Err: err,
		}
	}
	// 证据是原始工具输出，**含候选明文**（命中处附近的原文就是它）⇒ 0600。
	if err := writeFileAtomic(full, data, 0o600); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.put_evidence",
			Msg: "写证据失败", Err: err,
		}
	}
	return nil
}

// GetEvidence 读回一份证据。缺失时返回 os.ErrNotExist 包装的错误。
func (e *evidenceStore) GetEvidence(ref string) ([]byte, error) {
	full, err := e.evidencePath(ref)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("读证据 %s 失败: %w", full, err)
	}
	return b, nil
}

// Rejected 返回已判错答案的**指纹**集（明文不出 private/）。
//
// 三条语义，每条都对应一类真实事故：
//
//  1. **账本缺失 ⇒ KindPersistence，绝不返回空集。** 「账本为空 ⇒ 没有任何
//     候选被提交过 ⇒ 把全部候选重提一遍」是 Review Focus #2 点名的失败模式：
//     用户把 run 目录拷到别处、或进程在写账本中途被杀时，空集会被当成
//     「全新开始」，于是重复向平台提交、重复消耗提交额度。fail closed 的
//     代价是用户要显式处理一次错误，猜错的代价是重复提交——不对称，所以选前者。
//
//  2. **只给指纹**。这个列表会被回灌进下一轮 prompt 告诉 agent「这些不要再
//     试了」。给明文等于把答案又塞回上下文（前身 `_scrub_flag_plaintext` 的
//     事故：flag 明文进了 MEMORY.md / _blackboard.json）。指纹足够让 agent
//     判断「我是不是又想到同一个」，又不足以直接抄。
//
//  3. **去重**。同一答案判错两次只记一条——否则一条被反复试的错答案会把
//     prompt 预算占满。
func (e *evidenceStore) Rejected() ([]string, error) {
	if err := e.requireLedger(); err != nil {
		return nil, err
	}
	recs, err := e.readLedger()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(recs))
	var out []string
	for _, r := range recs {
		// 判据是「提交过、且没被判对」。Duplicate 等价于已确认（平台幂等命中），
		// 所以它**不算判错**——把它记进判错集会误导下一轮 prompt。
		if !r.Submitted || r.Correct || r.Duplicate || r.Flag == "" {
			continue
		}
		fp := answer.Fingerprint(r.Flag)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, fp)
	}
	return out, nil
}

// requireLedger 断言账本存在。不存在 ⇒ KindPersistence（fail closed）。
func (e *evidenceStore) requireLedger() error {
	if _, err := os.Stat(e.ledgerPath()); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.private_ledger",
			Msg: "私密账本 private/candidates.jsonl 不存在：拒绝按「从未提交过」继续" +
				"（那会导致重复提交已确认的答案）",
			Err: err,
		}
	}
	return nil
}

// readLedger 逐行解析账本。**末行撕裂按残缺处理并跳过**（与事件日志同一
// 判据）：进程在写账本中途被杀时，丢掉那一条记录只会让一个候选被当成「未
// 提交」——比整份账本不可读（run 完全无法恢复）便宜得多。
func (e *evidenceStore) readLedger() ([]ledgerRecord, error) {
	b, err := os.ReadFile(e.ledgerPath())
	if err != nil {
		return nil, &harness.Error{
			Kind: harness.KindPersistence, Op: "store.private_ledger",
			Msg: "读私密账本失败", Err: err,
		}
	}
	if len(b) == 0 {
		return nil, nil
	}
	lines := splitLines(b)
	out := make([]ledgerRecord, 0, len(lines))
	for i, ln := range lines {
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		var r ledgerRecord
		if err := json.Unmarshal(ln, &r); err != nil {
			if i == len(lines)-1 {
				break // 撕裂的末行：丢掉
			}
			return nil, &harness.Error{
				Kind: harness.KindPersistence, Op: "store.private_ledger",
				Msg: fmt.Sprintf("私密账本第 %d 行损坏（非末行，拒绝静默跳过）", i+1),
				Err: err,
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// ledgerPath 返回账本路径。
func (e *evidenceStore) ledgerPath() string {
	return filepath.Join(e.runDir, privateDirName, candidatesFileName)
}

// evidencePath 把 ref 解析成 private/evidence/ 下的绝对路径，并挡住逃逸。
func (e *evidenceStore) evidencePath(ref string) (string, error) {
	bad := &harness.Error{
		Kind: harness.KindPersistence, Op: "store.evidence_path",
		Msg: fmt.Sprintf("证据引用 %q 非法（不得为绝对路径，也不得越出 private/evidence/）", ref),
	}
	if ref == "" {
		return "", &harness.Error{
			Kind: harness.KindPersistence, Op: "store.evidence_path",
			Msg: "证据引用为空",
		}
	}
	if filepath.IsAbs(ref) || strings.HasPrefix(ref, "/") {
		return "", bad
	}
	clean := filepath.Clean(ref)
	if clean == "." || !filepath.IsLocal(clean) {
		return "", bad
	}
	base := filepath.Join(e.runDir, privateDirName, evidenceDirName)
	full := filepath.Join(base, clean)
	// 双保险：Join 之后再做一次前缀检查。Clean 已经处理了 `..`，但如果有人
	// 之后放宽了上面的判据，这一层仍然能挡住逃逸。
	if rel, err := filepath.Rel(base, full); err != nil || !filepath.IsLocal(rel) {
		return "", bad
	}
	return full, nil
}

// appendLine 追加「整行 + \n」，一次 write 写完。
//
// 与 appendEventLine 的区别：这里不修撕裂尾巴，也不 fsync。原因是账本
// **不参与恢复基线**——它只回答「某个候选提交过吗」，撕裂末行按残缺跳过即可
// （readLedger 已处理），丢掉一条记录的代价是那个候选可能被重提一次；而
// 每次记账都 fsync 会把每轮几十条候选记录的写盘成本叠起来。事件的顺序
// 约束（快照指向的事件必须已落盘）只对 events.jsonl 成立。
func appendLine(path string, line []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, perm)
	if err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.append_line",
			Msg: "打开账本失败", Err: err,
		}
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(perm); err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.append_line",
			Msg: "chmod 账本失败", Err: err,
		}
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	n, err := f.Write(buf)
	if err != nil {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.append_line",
			Msg: "写账本失败", Err: err,
		}
	}
	if n != len(buf) {
		return &harness.Error{
			Kind: harness.KindPersistence, Op: "store.append_line",
			Msg: fmt.Sprintf("写账本短写：%d/%d 字节（磁盘满或配额？）", n, len(buf)),
		}
	}
	return nil
}

// ensureLedger 建出账本文件（幂等）。
//
// 为什么**首次**写入要自己创建，而恢复路径上缺失要 fail closed：两者回答的
// 是两个不同的问题。「run 目录刚被建出来」⇒ 建一个空账本是对的；「run 目录
// 存在、快照存在、账本却不在」⇒ 说明账本被删/被拷漏了，此时补一个空账本
// 等于宣称「什么都没提交过」。判据是**快照在不在**：快照在而账本不在就是
// 后一种情形，由调用方（engine 的恢复路径）用 Rejected() 的报错挡住。
func ensureLedger(runDir string) error {
	dir := filepath.Join(runDir, privateDirName)
	if err := mkdirAllPrivate(dir, 0o700); err != nil {
		return fmt.Errorf("建 private 目录失败: %w", err)
	}
	evDir := filepath.Join(dir, evidenceDirName)
	if err := mkdirAllPrivate(evDir, 0o700); err != nil {
		return fmt.Errorf("建 evidence 目录失败: %w", err)
	}
	path := filepath.Join(dir, candidatesFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("建账本 %s 失败: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod 账本失败: %w", err)
	}
	return nil
}

var _ harness.EvidenceStore = (*evidenceStore)(nil)
