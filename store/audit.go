package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

// submissionsFileName 是私密候选审计的文件名。
//
// ⚠️ **刻意不叫 `candidates.jsonl`**：那个名字已经被 v0.3 的候选账本占了
// （`candidatesFileName`，落点是 `<StoreDir>/runs/<id>/private/`）。两者虽然都是
// 私密面、都记候选，但回答的问题不同——账本记的是「见过哪些候选」，审计记的是
// 「提交了哪些、平台怎么判的」。同名不同树会让「我去看审计」的人打开另一份文件，
// 而那份文件恰好是可行的（它只是没有他要的内容），于是结论会是「审计里没有」。
//
// 与 trace 不同，这里**不按题目哈希分文件**：审计要回答的是一个跨题目的问题
// ——「这次运行一共提交了多少条、平台怎么判的」，而那个答案只有在同一个文件里
// 才数得出来。题目编号作为字段落在每一行上。
const submissionsFileName = "submissions.jsonl"

const (
	// maxAuditBytes 是单个 run 的候选审计总量上限。
	maxAuditBytes = 16 << 20
	// auditMaxLineBytes 是单行上限：一条审计记录只有指纹、几个枚举与一行明文，
	// 超出这个数只可能是某处把整段工具输出塞了进来。
	auditMaxLineBytes = 64 << 10
)

var _ harness.AuditStore = (*ResultFileStore)(nil)

// auditRecord 是候选审计的落盘 schema（**私密面**，允许明文）。
//
// 它回答的是那个在 v0.5 之前**无法回答**的问题：这次提交了什么，平台怎么判的。
// 在此之前，判定只活在内存里的 Candidate 上、随本题结束消失，公开结果只剩聚合
// 计数——「147 次提交、146 条判错」这类事实事后只能靠猜。
type auditRecord struct {
	// Challenge 是题目编号（平台给的 code，只在这里作为**字段**出现，不进路径）。
	Challenge string `json:"challenge"`
	// Fingerprint 由 answer.Fingerprint 算出——那是唯一真源。
	//
	// 为什么要在这里算而不是让根包传进来：根包不许 import 任何子包（`answer` 是
	// 子包），所以指纹只能由实现方代算。这不是「再实现一遍」——本文件调用的正是
	// 那个唯一实现。
	Fingerprint string `json:"fingerprint"`
	// Flag 是候选**明文**。它只允许出现在这一层（0700 目录、0600 文件）。
	Flag string `json:"flag"`
	// Source / Provenance / IntentID / Round 是出处与推导链锚点。
	Source     string `json:"source,omitempty"`
	Provenance string `json:"provenance,omitempty"`
	IntentID   string `json:"intentId,omitempty"`
	Round      int    `json:"round,omitempty"`
	// SubmittedAt 是发起这次提交的宿主时间（RFC3339，UTC）。
	SubmittedAt string `json:"submittedAt"`
	// Verdict 的四个布尔字段与根包 SubmissionVerdict 一一对应。
	Correct   bool `json:"correct"`
	Duplicate bool `json:"duplicate,omitempty"`
	Rejected  bool `json:"rejected,omitempty"`
	Uncertain bool `json:"uncertain,omitempty"`
	Score     int  `json:"score,omitempty"`
	// Message 是平台原样返回的说明；SubmitError 是传输/平台异常文本。
	Message     string `json:"message,omitempty"`
	SubmitError string `json:"submitError,omitempty"`
}

// AppendAudit 追加一条候选审计到 <ResultDir>/private/<runID>/submissions.jsonl。
//
// 权限与目录树与 trace 完全一致（0700 / 0600）：明文纪律不变，变的只是「哪条
// 路径承载它」。写入方式同样是 O_APPEND 一行一条——崩溃后已写出的行仍然可读，
// 这正是审计相对于「内存账本」的全部价值。
// ⚠️ **每一条失败分支都必须带 Kind**（ctx 取消除外）。`harness.IsKind(err,
// KindPersistence)` 是调用方识别「落盘故障」的唯一手段，而一个裸 error 会被
// 折成 `unclassified`：那意味着「审计写失败了」在调用方眼里与「某个说不上来
// 的错误」同形，而审计是否完整恰恰是公开结果里要如实回答的一件事
// （`auditIncomplete`）。
//
// ctx 取消**故意例外**：它是取消，不是落盘故障——调用方靠
// `errors.Is(err, context.Canceled)` 判它，包成 KindPersistence 会让「用户按了
// Ctrl-C」变成「磁盘坏了」。
func (s *ResultFileStore) AppendAudit(ctx context.Context, runID harness.RunID, challenge string, rec harness.CandidateAudit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validRunID(runID); err != nil {
		return err
	}
	if challenge == "" {
		// 这一条是**入口校验**，不是落盘故障：编号为空说明调用方漏了东西，
		// 重试同样的输入毫无意义。
		return harness.Ef(harness.KindConfig, "resultstore.audit", "题目编号为空", nil)
	}
	line, err := json.Marshal(auditRecord{
		Challenge:   challenge,
		Fingerprint: answer.Fingerprint(rec.Flag),
		Flag:        rec.Flag,
		Source:      rec.Source,
		Provenance:  rec.Provenance,
		IntentID:    rec.IntentID,
		Round:       rec.Round,
		SubmittedAt: rec.SubmittedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Correct:     rec.Verdict.Correct,
		Duplicate:   rec.Verdict.Duplicate,
		Rejected:    rec.Verdict.Rejected,
		Uncertain:   rec.Verdict.Uncertain,
		Score:       rec.Score,
		Message:     rec.Message,
		SubmitError: rec.SubmitError,
	})
	if err != nil {
		return harness.Ef(harness.KindPersistence, "resultstore.audit", "审计记录序列化失败", err)
	}
	line = append(line, '\n')
	if len(line) > auditMaxLineBytes {
		return harness.Ef(harness.KindPersistence, "resultstore.audit", "单条审计超限", nil)
	}
	// 与 AppendTrace 同源：privateDirName 是唯一真源，写字面量迟早与 NewResultStore
	// 建出来的那个、以及 bridge 的 PrivateDir 漂移。
	dir := filepath.Join(filepath.Dir(s.root), privateDirName, string(runID))
	if err := mkdirAllPrivate(dir, dirPerm); err != nil {
		return harness.Ef(harness.KindPersistence, "resultstore.audit", "建私密审计目录失败", err)
	}
	path := filepath.Join(dir, submissionsFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, privatePerm)
	if err != nil {
		return harness.Ef(harness.KindPersistence, "resultstore.audit", "打开审计文件失败", err)
	}
	defer f.Close()
	// 显式 Chmod：进程 umask 可能让 O_CREATE 建出比 0600 更宽的文件，而这个文件
	// 里装着候选明文。与 AppendTrace 同样的理由。
	if err := f.Chmod(privatePerm); err != nil {
		return harness.Ef(harness.KindPersistence, "resultstore.audit", "chmod 审计文件失败", err)
	}
	info, err := f.Stat()
	if err != nil {
		return harness.Ef(harness.KindPersistence, "resultstore.audit", "读审计文件大小失败", err)
	}
	if info.Size()+int64(len(line)) > maxAuditBytes {
		return harness.Ef(harness.KindPersistence, "resultstore.audit", "候选审计总量超限", nil)
	}
	if _, err := f.Write(line); err != nil {
		return harness.Ef(harness.KindPersistence, "resultstore.audit", "写审计行失败", err)
	}
	if err := f.Sync(); err != nil {
		return harness.Ef(harness.KindPersistence, "resultstore.audit", "fsync 审计文件失败", err)
	}
	return nil
}
