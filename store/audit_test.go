package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/answer"
)

// auditCanary 是一条**明显的假 flag**，用来验证私密面与公开面的分离。
const auditCanary = "flag{audit-canary-not-real}"

func newAudit(t *testing.T, runID harness.RunID, flag string, v harness.SubmissionVerdict) (*ResultFileStore, string) {
	t.Helper()
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.AppendAudit(context.Background(), runID, "c1", harness.CandidateAudit{
		Source: "call-1", Provenance: "observed", IntentID: "i1", Round: 2,
		SubmittedAt: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
		Verdict:     v, Score: 10, Message: "平台原话", Flag: flag,
	}); err != nil {
		t.Fatal(err)
	}
	return rs, filepath.Join(root, privateDirName, string(runID), submissionsFileName)
}

// TestAuditFileIsPrivateAndCarriesPlaintext：审计落在 0700/0600 的私密面，
// 且**确实写了明文与指纹**——这是它相对于「只有聚合计数」的全部价值。
func TestAuditFileIsPrivateAndCarriesPlaintext(t *testing.T) {
	_, path := newAudit(t, "run-1", auditCanary, harness.SubmissionVerdict{Correct: true})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("审计文件不存在: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("审计文件权限 = %v，期望 0600（里面有候选明文）", info.Mode().Perm())
	}
	for _, dir := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		di, err := os.Stat(dir)
		if err != nil || di.Mode().Perm() != 0o700 {
			t.Errorf("审计目录 %s 权限 = %v err=%v，期望 0700", dir, di.Mode().Perm(), err)
		}
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), auditCanary) {
		t.Fatal("审计里没有候选明文——「提交了什么」仍然答不出来")
	}
	var rec auditRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("审计行不是合法 JSON: %v", err)
	}
	// 指纹必须与唯一真源逐字一致：它会经 Fingerprints() 之类回灌 prompt，
	// 两份实现一旦漂移，「同一条候选」在两个地方就是两个串。
	if want := answer.Fingerprint(auditCanary); rec.Fingerprint != want {
		t.Errorf("指纹 = %q，期望 answer.Fingerprint 的 %q", rec.Fingerprint, want)
	}
	if rec.Correct != true || rec.Challenge != "c1" || rec.IntentID != "i1" || rec.Round != 2 {
		t.Errorf("审计字段不全: %+v", rec)
	}
	if rec.SubmittedAt != "2026-09-22T10:00:00.000Z" {
		t.Errorf("提交时间 = %q，期望 RFC3339 UTC", rec.SubmittedAt)
	}
}

// TestAuditAppendsOneLinePerSubmission：追加语义——一次提交一行，崩溃后已写出
// 的行仍然可读。这正是审计相对于「内存账本」的全部价值。
func TestAuditAppendsOneLinePerSubmission(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	verdicts := []harness.SubmissionVerdict{
		{Correct: true, Duplicate: true},
		{Rejected: true},
		{Uncertain: true},
	}
	for i, v := range verdicts {
		rec := harness.CandidateAudit{SubmittedAt: time.Unix(int64(i), 0).UTC(),
			Verdict: v, Flag: "flag{line" + string(rune('a'+i)) + "}"}
		if err := rs.AppendAudit(context.Background(), "run-1", "c1", rec); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(root, privateDirName, "run-1", submissionsFileName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("行数 = %d，期望 3（一次提交一行）", len(lines))
	}
	// 三种判定必须各自可辨——「幂等命中」「判错」「不知道结果」混成一种，
	// 审计就回答不了它该回答的问题。
	var got [3]auditRecord
	for i, l := range lines {
		if err := json.Unmarshal([]byte(l), &got[i]); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v", i, err)
		}
	}
	if !got[0].Correct || !got[0].Duplicate {
		t.Errorf("第 1 行应记幂等命中: %+v", got[0])
	}
	if !got[1].Rejected || got[1].Correct {
		t.Errorf("第 2 行应记判错: %+v", got[1])
	}
	if !got[2].Uncertain {
		t.Errorf("第 3 行应记不确定: %+v", got[2])
	}
}

// TestAuditNeverTouchesPublicResult：审计**绝不**进公开结果。
//
// 这是本仓库最硬的一条纪律（公开面只有计数与指纹），而审计是本轮**新引入**的
// 一处明文落盘——新开的通道必须自己带一条 canary。
func TestAuditNeverTouchesPublicResult(t *testing.T) {
	rs, _ := newAudit(t, "run-1", auditCanary, harness.SubmissionVerdict{Correct: true})
	if err := rs.Save(context.Background(), harness.RunResult{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(filepath.Join(filepath.Dir(rs.root), resultsDirName, "run-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), auditCanary) {
		t.Fatalf("公开结果泄漏了候选明文: %s", public)
	}
	// 平台原话同样只在私密面——它是自由文本，与 Message 漏进公开面是同一条通道。
	if strings.Contains(string(public), "平台原话") {
		t.Fatalf("公开结果泄漏了平台消息: %s", public)
	}
}

// TestAuditRejectsBadInput：与 AppendTrace 同形的入口校验。
//
// 三种拒绝必须**分得清类别**，否则调用方没法决定怎么处置：入口错（配置错了，
// 重试无意义）与落盘故障（该重试/该报磁盘问题）是两回事，而取消既不是前者也
// 不是后者。
func TestAuditRejectsBadInput(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 题目编号为空：它会变成文件里的一个字段，空值说明调用方漏了东西。
	if err := rs.AppendAudit(ctx, "run-1", "", harness.CandidateAudit{Flag: "x"}); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("空题目编号的错误 = %v，期望 KindConfig", err)
	}
	// 路径逃逸的 runID：审计文件名是固定的，但目录名来自 runID。
	if err := rs.AppendAudit(ctx, "../../hostile", "c1", harness.CandidateAudit{Flag: "x"}); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("路径逃逸的 runID 错误 = %v，期望 KindConfig", err)
	}
	// 已取消的 ctx：写入前就该返回，而不是把半条记录落盘。它**不得**被折成
	// KindPersistence——「用户按了 Ctrl-C」与「磁盘坏了」必须能分开。
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err = rs.AppendAudit(cancelled, "run-1", "c1", harness.CandidateAudit{Flag: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("已取消的 ctx 错误 = %v，期望 context.Canceled", err)
	}
	if harness.IsKind(err, harness.KindPersistence) {
		t.Errorf("ctx 取消被误报成落盘故障: %v", err)
	}
}

// auditFilePath 返回某个 run 的审计文件路径。
func auditFilePath(root string, runID harness.RunID) string {
	return filepath.Join(root, privateDirName, string(runID), submissionsFileName)
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// TestAuditRejectsOversizedLine：**单行上限**（auditMaxLineBytes）必须真的挡住。
//
// 一条审计记录只有指纹、几个枚举与一行明文；超出这个数只可能是某处把整段工具
// 输出塞了进来。挡住它而不是截断：被截断的 JSON 行会让**整份审计**读不出来，
// 而它是事后唯一能回答「提交了什么、平台怎么判的」的东西。
//
// 这条不是落盘故障（磁盘没问题），但它同样不是入口配置错——它的归属是
// KindPersistence：调用方据此把这一条**丢掉并继续**，而不是把整次运行判失败。
func TestAuditRejectsOversizedLine(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	// 明文字段本身超限：编码后的整行必然更长。
	huge := strings.Repeat("A", auditMaxLineBytes)
	err = rs.AppendAudit(context.Background(), "run-1", "c1", harness.CandidateAudit{Flag: huge})
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("超长单条的错误 = %v，期望 KindPersistence", err)
	}
	// 被拒的记录**不得**留下任何痕迹：一条写了一半的记录会让这份审计从「缺一条」
	// 变成「整个文件读不出来」（多行 JSONL 里有个坏行）。
	path := auditFilePath(root, "run-1")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		b, _ := os.ReadFile(path)
		t.Fatalf("被拒的超长记录不该建出审计文件（size=%d）: %s", len(b), b)
	}
}

// TestAuditRejectsOversizedTotal：**总量上限**（maxAuditBytes）必须真的挡住，
// 且挡住时文件**一个字节都不长**。
//
// 上限的判据是「加上这一条会不会超」（`info.Size()+len(line) > max`），所以上限
// 生效后文件应当停在**恰好不超**的位置，而不是先写进去再回滚——后者在崩溃窗口
// 里会留下一条超出上限的记录，且没有任何东西会来修它。
func TestAuditRejectsOversizedTotal(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	// 每行尽量大但合法：60000 字节明文 + 编码开销仍远小于 auditMaxLineBytes，
	// 这样撞到的一定是**总量**那条闸，而不是上一行那条。用 'A' 是因为它不产生
	// JSON 转义，行长可精确预估。
	big := strings.Repeat("A", 60000)
	ctx := context.Background()

	var failErr error
	var sizeAtFailure int64
	path := auditFilePath(root, "run-1")
	for i := 0; i < 400; i++ {
		rec := harness.CandidateAudit{SubmittedAt: time.Unix(int64(i), 0).UTC(), Flag: big}
		if err := rs.AppendAudit(ctx, "run-1", "c1", rec); err != nil {
			failErr = err
			break
		}
		sizeAtFailure = fileSize(t, path)
	}
	if failErr == nil {
		t.Fatalf("写满 %d 字节仍然全部成功——总量上限没生效", maxAuditBytes)
	}
	if !harness.IsKind(failErr, harness.KindPersistence) {
		t.Fatalf("超总量的错误 = %v，期望 KindPersistence", failErr)
	}
	if sizeAtFailure > maxAuditBytes {
		t.Fatalf("上限之前文件就已经 %d 字节 > %d", sizeAtFailure, maxAuditBytes)
	}
	if got := fileSize(t, path); got != sizeAtFailure {
		t.Fatalf("被拒的那一条仍然写进了文件：%d → %d 字节", sizeAtFailure, got)
	}
	// 挡住之后仍然得能读：不能因为撞了一次闸就把已有审计弄坏。
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	// 填满这件事本身要成立：一条都没写出去的话，上面那条「被拒后文件不长」就是
	// 恒真的（0 → 0），看着在守着，其实什么都没守。
	if len(lines) < 100 {
		t.Fatalf("只写出了 %d 行（%d 字节），没到总量上限附近", len(lines), sizeAtFailure)
	}
	for i, l := range lines {
		var rec auditRecord
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON（上限失效时留下了坏行）: %v", i, err)
		}
	}
}

// TestAuditWriteFailureIsPersistence：**落盘失败必须报 KindPersistence**。
//
// 这是本轮补上的一处归类：这些分支过去返回裸 error，于是 `harness.IsKind(err,
// KindPersistence)` 判不出来，调用方看到的是「某个说不上来的错误」——而
// 「审计写失败了」恰恰是公开结果里要如实回答的一件事（auditIncomplete）。把它
// 折成 unclassified 等于让「审计缺了」有与「别的什么出错了」同一种外观。
//
// 手法是让目录**建不出来**：`<ResultDir>/private/<runID>` 位置先放一个普通文件，
// mkdirAllPrivate 必然失败。这比注入一个失败钩子更接近真实故障（磁盘满、权限被
// 改、有人手工动过目录），而且不需要为测试在生产代码里开口子。
func TestAuditWriteFailureIsPersistence(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(root, privateDirName, "run-blocked")
	if err := os.WriteFile(blocker, []byte("占位"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = rs.AppendAudit(context.Background(), "run-blocked", "c1",
		harness.CandidateAudit{SubmittedAt: time.Unix(0, 0).UTC(), Flag: "flag{x}"})
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("建目录失败的错误 = %v，期望 KindPersistence", err)
	}
	// 占位文件必须原样还在：失败的写入不得把它改掉或删掉。
	if b, err := os.ReadFile(blocker); err != nil || string(b) != "占位" {
		t.Fatalf("失败的写入动了占位文件: %q err=%v", b, err)
	}
}
