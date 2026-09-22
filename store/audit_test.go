package store

import (
	"context"
	"encoding/json"
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
func TestAuditRejectsBadInput(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 题目编号为空：它会变成文件里的一个字段，空值说明调用方漏了东西。
	if err := rs.AppendAudit(ctx, "run-1", "", harness.CandidateAudit{Flag: "x"}); err == nil {
		t.Error("空题目编号应被拒绝")
	}
	// 路径逃逸的 runID：审计文件名是固定的，但目录名来自 runID。
	if err := rs.AppendAudit(ctx, "../../hostile", "c1", harness.CandidateAudit{Flag: "x"}); err == nil {
		t.Error("路径逃逸的 runID 应被拒绝")
	}
	// 已取消的 ctx：写入前就该返回，而不是把半条记录落盘。
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := rs.AppendAudit(cancelled, "run-1", "c1", harness.CandidateAudit{Flag: "x"}); err == nil {
		t.Error("已取消的 ctx 应被拒绝")
	}
}
