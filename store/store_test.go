package store_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/store"
)

// 假的候选明文。**故意带上信封形状**，这样「明文是否泄漏到公开文件」这条
// 断言才有意义 —— 一个不含 flag{...} 形状的串即使漏出去也测不出来。
const fakeFlag = "flag{t7_priv4te_0nly_d0nt_leak}"

func newStore(t *testing.T) *store.FileStore {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// newRun 造一个 run 目录并返回它的 store 视图。
func newRun(t *testing.T, s *store.FileStore, id harness.RunID, seq int64) *store.FileStore {
	t.Helper()
	r, err := s.ForRun(id)
	if err != nil {
		t.Fatalf("ForRun: %v", err)
	}
	if err := r.Append(ev(seq, harness.EvRunCreated, id)); err != nil {
		t.Fatalf("Append 首事件: %v", err)
	}
	return r
}

func ev(seq int64, typ harness.DomainEventType, id harness.RunID) harness.DomainEvent {
	return harness.DomainEvent{
		Seq: seq, At: time.Unix(1700000000+seq, 0).UTC(),
		Type: typ, RunID: id,
		Payload: json.RawMessage(`{"note":"no plaintext here"}`),
	}
}

func snap(id harness.RunID, lastSeq int64) harness.Snapshot {
	spec := harness.RunSpec{Scenario: "fake", Targets: []string{"demo-1"}, StoreDir: "/tmp/x"}
	return harness.Snapshot{
		SchemaVersion:  harness.SchemaVersion,
		RunID:          id,
		State:          harness.RunRunning,
		Spec:           spec,
		SpecDigest:     spec.Digest(),
		LastAppliedSeq: lastSeq,
		StartedAt:      time.Unix(1700000000, 0).UTC(),
	}
}

// TestNewLayout 钉目录布局与权限（设计文档 §6）。
func TestNewLayout(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-layout", 1)

	want := map[string]os.FileMode{
		r.Dir():                                               0o700,
		filepath.Join(r.Dir(), "run.json"):                    0o600,
		filepath.Join(r.Dir(), "events.jsonl"):                0o600,
		filepath.Join(r.Dir(), "private"):                     0o700,
		filepath.Join(r.Dir(), "private", "candidates.jsonl"): 0o600,
		filepath.Join(r.Dir(), "private", "evidence"):         0o700,
	}
	for p, mode := range want {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("期望存在 %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != mode {
			t.Errorf("%s 权限 = %04o，期望 %04o", p, got, mode)
		}
	}
	// 根目录本身也必须是 0700：run 目录在它下面，根目录宽松等于把 run 目录名
	// 暴露给同机其他用户。
	if fi, err := os.Stat(s.Dir()); err != nil {
		t.Fatalf("store 根目录不存在: %v", err)
	} else if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("store 根目录权限 = %04o，期望 0700", got)
	}
	// 报告是公开产物，必须是 0644（其它用户可以读，用于交付/贴工单）。
	// 走 store 自己的入口写（报告名与权限都是布局的一部分，不该由调用方
	// 自己 chmod）。
	for _, name := range []string{"report.json", "report.md"} {
		if err := r.PutReport(name, []byte("# 报告\n无明文\n")); err != nil {
			t.Fatalf("PutReport(%s): %v", name, err)
		}
		p := filepath.Join(r.Dir(), name)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o644 {
			t.Errorf("%s 权限 = %04o，期望 0644", name, got)
		}
	}
	// 报告名是布局的一部分：其它名字必须被拒（否则它成为又一个路径逃逸面）。
	if err := r.PutReport("../escape.md", []byte("x")); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("非法报告名应 KindConfig，实际 %v", err)
	}
}

// TestSeqMustBeMonotonic 钉住「跳号即拒绝」，且错误分类必须是 KindPersistence
// ——调用方靠 Kind 决定停机而不是靠解析消息。
func TestSeqMustBeMonotonic(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-seq", 1)

	for _, bad := range []int64{1, 3, 0, -1, 99} {
		err := r.Append(ev(bad, harness.EvRoundStarted, "run-seq"))
		if err == nil {
			t.Fatalf("Seq=%d 应被拒", bad)
		}
		if !harness.IsKind(err, harness.KindPersistence) {
			t.Errorf("Seq=%d 的错误 Kind = %v，期望 KindPersistence", bad, err)
		}
		if got := len(mustLoad(t, r, 0)); got != 1 {
			t.Fatalf("被拒的事件不得落盘，现有 %d 条", got)
		}
	}
	if err := r.Append(ev(2, harness.EvRoundStarted, "run-seq")); err != nil {
		t.Fatalf("Seq=2 应被接受: %v", err)
	}
}

func mustLoad(t *testing.T, r *store.FileStore, after int64) []harness.DomainEvent {
	t.Helper()
	got, err := r.LoadEvents(after)
	if err != nil {
		t.Fatalf("LoadEvents(%d): %v", after, err)
	}
	return got
}

// TestAppendOrderEventBeforeSnapshot 钉住顺序：**事件先落盘，快照后落盘**。
//
// 注入一次快照写失败，断言事件仍在 events.jsonl 里。反过来（先快照后事件）
// 在崩溃时会产生「快照指向不存在的事件」，恢复时无法重放。
func TestAppendOrderEventBeforeSnapshot(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-order", 1)

	boom := errors.New("注入的快照写失败")
	r.SetSnapshotHook(func() error { return boom })

	err := r.Append(ev(2, harness.EvRoundStarted, "run-order"))
	if err == nil {
		t.Fatal("快照写失败必须透出")
	}
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Errorf("Kind = %v，期望 KindPersistence", err)
	}

	got := mustLoad(t, r, 0)
	if len(got) != 2 {
		t.Fatalf("事件日志应有 2 条（含失败那次），实际 %d 条 —— 顺序反了", len(got))
	}
	if got[1].Seq != 2 {
		t.Errorf("末条 Seq = %d，期望 2", got[1].Seq)
	}
	// 快照没写成功 ⇒ run.json 仍停在上一次成功的位置。恢复基线因此**不会**
	// 超过日志末尾，重放不会指向不存在的事件。
	snap, err := r.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.LastAppliedSeq != 1 {
		t.Errorf("快照 LastAppliedSeq = %d，期望 1（失败那次不该被记进快照）", snap.LastAppliedSeq)
	}
}

// TestAppendThenSnapshotConsistent 钉住「快照的 lastAppliedSeq 与日志末尾一致」。
func TestAppendThenSnapshotConsistent(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-consist", 1)
	for seq := int64(2); seq <= 5; seq++ {
		if err := r.Append(ev(seq, harness.EvRoundStarted, "run-consist")); err != nil {
			t.Fatalf("Append(%d): %v", seq, err)
		}
		snap, err := r.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if snap.LastAppliedSeq != seq {
			t.Fatalf("Seq=%d 后快照 LastAppliedSeq = %d", seq, snap.LastAppliedSeq)
		}
	}
	got := mustLoad(t, r, 0)
	if len(got) != 5 || got[4].Seq != 5 {
		t.Fatalf("日志 = %d 条，末条 Seq=%d", len(got), got[len(got)-1].Seq)
	}
}

// TestLoadEventsAfterSeq 钉住 afterSeq 是**排他**下界（看板 SSE 的
// Last-Event-ID 语义：客户端说「我已经有 3 了」，就只补发 4 起）。
func TestLoadEventsAfterSeq(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-after", 1)
	for seq := int64(2); seq <= 4; seq++ {
		if err := r.Append(ev(seq, harness.EvRoundStarted, "run-after")); err != nil {
			t.Fatal(err)
		}
	}
	got := mustLoad(t, r, 2)
	if len(got) != 2 || got[0].Seq != 3 || got[1].Seq != 4 {
		t.Fatalf("afterSeq=2 应返回 [3 4]，实际 %v", seqs(got))
	}
	if n := len(mustLoad(t, r, 4)); n != 0 {
		t.Fatalf("afterSeq=末尾应返回空，实际 %d 条", n)
	}
	if n := len(mustLoad(t, r, 0)); n != 4 {
		t.Fatalf("afterSeq=0 应返回全部 4 条，实际 %d", n)
	}
}

func seqs(evs []harness.DomainEvent) []int64 {
	out := make([]int64, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Seq)
	}
	return out
}

// TestLoadEventsSkipsTornLastLine 钉住 Review Focus #4：进程被杀留下的半行
// **不得**让整个 run 永久无法恢复。
func TestLoadEventsSkipsTornLastLine(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-torn", 1)
	if err := r.Append(ev(2, harness.EvRoundStarted, "run-torn")); err != nil {
		t.Fatal(err)
	}
	// 手工追加半行（模拟「写事件日志写了一半就被 kill」）。
	f, err := os.OpenFile(filepath.Join(r.Dir(), "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":3,"at":"2026-09-20T00:00:00Z","type":"round_st`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	got := mustLoad(t, r, 0)
	if len(got) != 2 || got[1].Seq != 2 {
		t.Fatalf("应跳过残缺末行返回 [1 2]，实际 %v", seqs(got))
	}
	// 从最后一个完整事件继续：Seq=3 必须仍被接受（lastSeq 取自完整行）。
	if err := r.Append(ev(3, harness.EvRoundStarted, "run-torn")); err != nil {
		t.Fatalf("残缺末行之后的 Seq=3 应被接受: %v", err)
	}
}

// TestLoadEventsRejectsCorruptMiddleLine 钉住反面：**非末行**损坏是硬错误。
//
// 中间断一行说明日志真的坏了（人工编辑、部分恢复、磁盘损坏），静默跳过等于
// 把状态机的历史剪掉一段，而恢复会以错误的基线继续跑。
func TestLoadEventsRejectsCorruptMiddleLine(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-corrupt", 1)
	for seq := int64(2); seq <= 4; seq++ {
		if err := r.Append(ev(seq, harness.EvRoundStarted, "run-corrupt")); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(r.Dir(), "events.jsonl")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(b), "\n")
	lines[1] = "{这不是 JSON}\n" // 第 2 行（非末行）损坏
	if err := os.WriteFile(p, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = r.LoadEvents(0)
	if err == nil {
		t.Fatal("非末行损坏必须报错")
	}
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Errorf("Kind = %v，期望 KindPersistence", err)
	}
}

// TestSnapshotMissingIsNotExist 钉住「首跑」与「损坏」可区分：调用方靠
// errors.Is(err, os.ErrNotExist) 决定要不要新建 run。
func TestSnapshotMissingIsNotExist(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-missing")
	if err != nil {
		t.Fatal(err)
	}
	// ForRun 只建目录，不造快照。
	if _, err := r.Snapshot(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺 run.json 应满足 os.ErrNotExist，实际 %v", err)
	}
	// 快照损坏必须是**另一个**错误，不能伪装成「不存在」——伪装会让调用方
	// 把一次损坏当成首跑，把整个 run 从头再跑一遍。
	if err := os.WriteFile(filepath.Join(r.Dir(), "run.json"), []byte("{半截"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = r.Snapshot(t.Context())
	if err == nil {
		t.Fatal("损坏的 run.json 必须报错")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("损坏不得报告成 os.ErrNotExist")
	}
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Errorf("Kind = %v，期望 KindPersistence", err)
	}
}

// TestLoadEventsMissingIsEmpty 钉住首跑：没有 events.jsonl ⇒ 空切片且不报错。
func TestLoadEventsMissingIsEmpty(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-noevents")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.LoadEvents(0)
	if err != nil {
		t.Fatalf("缺 events.jsonl 不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("期望空，实际 %v", seqs(got))
	}
}

// TestGraphRoundTrip 钉住 graph.json 是**不透明载荷**：store 只负责路径与
// 原子性，不理解内容（store 不导入 dag）。
func TestGraphRoundTrip(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-graph")
	if err != nil {
		t.Fatal(err)
	}
	// 缺失时必须是 os.ErrNotExist，调用方据此区分首跑与损坏。
	if _, err := r.GetGraph(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺 graph.json 应满足 os.ErrNotExist，实际 %v", err)
	}
	blob := []byte(`{"schema":1,"code":"demo-1","nodes":[]}`)
	if err := r.PutGraph(blob); err != nil {
		t.Fatalf("PutGraph: %v", err)
	}
	got, err := r.GetGraph()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("graph 往返不一致:\n got %s\nwant %s", got, blob)
	}
	fi, err := os.Stat(filepath.Join(r.Dir(), "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("graph.json 权限 = %04o，期望 0600", fi.Mode().Perm())
	}
}

// TestPrivateNeverLeaksPlaintext 钉住事实层/答案层分离（设计文档 §3.3）：
// 候选明文只进 private/，公开文件一个字节都不能有。
func TestPrivateNeverLeaksPlaintext(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-leak", 1)
	if err := r.Append(ev(2, harness.EvCandidateSeen, "run-leak")); err != nil {
		t.Fatal(err)
	}
	if err := r.PutGraph([]byte(`{"schema":1,"nodes":[{"content":"无明文"}]}`)); err != nil {
		t.Fatal(err)
	}
	// 公开报告也写一份（权限 0644，最容易被拷走的那类文件）。
	for _, name := range []string{"report.json", "report.md"} {
		if err := r.PutReport(name, []byte("# 报告\n无明文\n")); err != nil {
			t.Fatal(err)
		}
	}

	priv := r.Private()
	if err := priv.PutCandidate(harness.Candidate{
		Flag: fakeFlag, Source: "tool-1", Output: "命中处附近的原文 " + fakeFlag,
		Confidence: 0.9, Provenance: harness.ProvenanceObserved,
		ToolCallID: "tc-1", IntentID: "intent-A", Round: 1,
	}); err != nil {
		t.Fatalf("PutCandidate: %v", err)
	}
	if err := priv.PutEvidence("evidence/tc-1.txt", []byte("原始输出 "+fakeFlag)); err != nil {
		t.Fatalf("PutEvidence: %v", err)
	}
	// 判错账本也写一条（它是被回灌进 prompt 的那条路径，最危险）。
	if err := priv.PutCandidate(harness.Candidate{
		Flag: fakeFlag, Provenance: harness.ProvenanceObserved,
		Submitted: true, Correct: false, RejectReason: "platform_rejected",
	}); err != nil {
		t.Fatal(err)
	}

	// 公开文件必须**零命中**明文。
	public := []string{"run.json", "events.jsonl", "graph.json", "report.json", "report.md"}
	for _, name := range public {
		b, err := os.ReadFile(filepath.Join(r.Dir(), name))
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		if bytes.Contains(b, []byte(fakeFlag)) {
			t.Errorf("候选明文泄漏进公开文件 %s", name)
		}
	}
	// private/ 里必须**有**明文——否则「没泄漏」只是因为根本没记。
	privDir := filepath.Join(r.Dir(), "private")
	found := false
	err := filepath.WalkDir(privDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(fakeFlag)) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("private/ 里没有候选明文 —— 账本没真的记下来")
	}
}

// TestPrivateLedgerMissingFailsClosed 钉住 Review Focus #2：
// 「账本缺失 ⇒ 没有任何候选被提交过 ⇒ 把全部候选重提一遍」是**必须被阻止**的
// 推理。账本缺失只能 fail closed。
func TestPrivateLedgerMissingFailsClosed(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-noledger", 1)
	priv := r.Private()
	// 首次写入必须**自己创建**账本（否则第一次跑就没有账本，一恢复就 fail）。
	if err := priv.PutCandidate(harness.Candidate{Flag: fakeFlag, Provenance: harness.ProvenanceObserved}); err != nil {
		t.Fatalf("首次 PutCandidate 应创建账本: %v", err)
	}
	if _, err := priv.Rejected(); err != nil {
		t.Fatalf("账本存在时 Rejected 不该报错: %v", err)
	}

	// 把账本删掉（用户在别处拷了 run 目录、或进程写账本中途被杀）。
	if err := os.Remove(filepath.Join(r.Dir(), "private", "candidates.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := priv.Rejected(); !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("账本缺失时 Rejected 必须 KindPersistence，实际 %v", err)
	}
	err := priv.PutCandidate(harness.Candidate{Flag: fakeFlag, Provenance: harness.ProvenanceObserved})
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("账本缺失时 PutCandidate 必须 KindPersistence，实际 %v", err)
	}
}

// TestRejectedReturnsFingerprintsOnly 钉住「判错账本只给指纹」（明文不出
// private/，因为它会被回灌进 prompt）。
func TestRejectedReturnsFingerprintsOnly(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-rej", 1)
	priv := r.Private()
	if err := priv.PutCandidate(harness.Candidate{
		Flag: fakeFlag, Provenance: harness.ProvenanceObserved,
		Submitted: true, Correct: false, SubmitError: "平台判错",
	}); err != nil {
		t.Fatal(err)
	}
	// 未判错的候选不进判错集。
	if err := priv.PutCandidate(harness.Candidate{
		Flag: "flag{pending_one}", Provenance: harness.ProvenanceObserved,
	}); err != nil {
		t.Fatal(err)
	}
	// 平台幂等命中等价于已确认，不算判错。
	if err := priv.PutCandidate(harness.Candidate{
		Flag: "flag{dup_one}", Provenance: harness.ProvenanceObserved,
		Submitted: true, Correct: true, Duplicate: true,
	}); err != nil {
		t.Fatal(err)
	}

	fps, err := priv.Rejected()
	if err != nil {
		t.Fatal(err)
	}
	if len(fps) != 1 {
		t.Fatalf("判错集 = %v，期望恰好 1 条", fps)
	}
	if strings.Contains(fps[0], fakeFlag) || strings.Contains(fps[0], "flag{") {
		t.Fatalf("判错集不得含明文: %q", fps[0])
	}
	// 判错集是**追加**的：同一答案判错两次只记一条（去重），否则回灌 prompt
	// 会被同一串占满。
	if err := priv.PutCandidate(harness.Candidate{
		Flag: fakeFlag, Provenance: harness.ProvenanceObserved,
		Submitted: true, Correct: false, SubmitError: "又判错",
	}); err != nil {
		t.Fatal(err)
	}
	fps, err = priv.Rejected()
	if err != nil {
		t.Fatal(err)
	}
	if len(fps) != 1 {
		t.Fatalf("判错集去重后应为 1 条，实际 %v", fps)
	}
}

// TestEvidenceRoundTrip 钉住证据存取与路径逃逸防护。
func TestEvidenceRoundTrip(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-evid", 1)
	priv := r.Private()
	ref := "evidence/tc-9.txt"
	if err := priv.PutEvidence(ref, []byte("原始工具输出")); err != nil {
		t.Fatal(err)
	}
	got, err := priv.GetEvidence(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "原始工具输出" {
		t.Fatalf("证据往返 = %q", got)
	}
	if _, err := priv.GetEvidence("evidence/不存在.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺证据应满足 os.ErrNotExist，实际 %v", err)
	}
	// 路径逃逸：ref 来自 gate 的取证串，必须挡住 `../` 写到 private/ 之外。
	for _, bad := range []string{"../escape.txt", "evidence/../../escape.txt", "/etc/passwd", ".."} {
		if err := priv.PutEvidence(bad, []byte("x")); err == nil {
			t.Errorf("PutEvidence(%q) 应被拒", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "escape.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("路径逃逸成功写到了 store 根目录")
	}
}

// TestNoTmpLeftBehind 钉住原子写不留残渣：每次 Append/PutGraph 都写临时文件
// 再 rename，失败路径也必须清掉。
func TestNoTmpLeftBehind(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-tmp", 1)
	if err := r.Append(ev(2, harness.EvRoundStarted, "run-tmp")); err != nil {
		t.Fatal(err)
	}
	if err := r.PutGraph([]byte(`{"schema":1}`)); err != nil {
		t.Fatal(err)
	}
	r.SetSnapshotHook(func() error { return errors.New("注入失败") })
	_ = r.Append(ev(3, harness.EvRoundStarted, "run-tmp"))

	err := filepath.WalkDir(s.Dir(), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(d.Name(), ".tmp") {
			t.Errorf("残留临时文件: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestUniqueTmpName 钉住临时文件名**唯一**：老的固定 `path + ".tmp"` 在两个
// 写者下会撞车（单写者是 store 的前提，但唯一名是廉价的第二道保险，也让
// 残留文件可归因到具体进程）。
func TestUniqueTmpName(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-unique")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		name := store.TmpName(filepath.Join(r.Dir(), "run.json"))
		if seen[name] {
			t.Fatalf("临时文件名重复: %s", name)
		}
		seen[name] = true
		if !strings.HasPrefix(filepath.Base(name), "run.json.tmp.") {
			t.Errorf("临时文件名 %s 不含 .tmp. 前缀", name)
		}
	}
}

// TestSingleWriterAssumption 钉住「store 假定单写者」这条前提的**可检测性**。
//
// 单写者一旦被破坏（两个引擎进程指同一个 run 目录），事件序号会互相打架。
// store 不做跨进程加锁（那是文件锁的活，超出本包范围），但冲突**不会静默
// 通过**：后到的写者拿着过期的序号，会被 checkSeq 拒绝并报 KindPersistence。
// 这里的时序是确定性的（先让 b 读到旧基线，再让 a 落地），不依赖竞态。
func TestSingleWriterAssumption(t *testing.T) {
	s := newStore(t)
	a := newRun(t, s, "run-sw", 1)
	b, err := s.ForRun("run-sw")
	if err != nil {
		t.Fatal(err)
	}
	// b 拿到「下一号是 2」的基线（它以为自己是唯一写者）。
	if last, err := b.LastSeq(); err != nil || last != 1 {
		t.Fatalf("b 的基线 = %d, %v，期望 1", last, err)
	}
	// a 先写。
	if err := a.Append(ev(2, harness.EvRoundStarted, "run-sw")); err != nil {
		t.Fatal(err)
	}
	// b 用同一个序号写：必须被拒（而不是写出交错日志）。
	err = b.Append(ev(2, harness.EvRoundStarted, "run-sw"))
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("过期写者必须被拒，实际 %v", err)
	}
	got := mustLoad(t, a, 0)
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("日志 = %v，期望 [1 2]（冲突的那个不落盘）", seqs(got))
	}
}

// TestRunIDMustBeSafe 钉住 RunID 当目录名用的安全边界：它来自调用方
// （CLI 的 --run），含 `..` 的 id 会让私密账本落到存储根之外（公开面）。
func TestRunIDMustBeSafe(t *testing.T) {
	s := newStore(t)
	for _, bad := range []harness.RunID{"", "../escape", "a/b", "/abs", ".", "..", ".hidden"} {
		if _, err := s.ForRun(bad); err == nil {
			t.Errorf("ForRun(%q) 应被拒", bad)
		} else if !harness.IsKind(err, harness.KindConfig) {
			t.Errorf("ForRun(%q) 的 Kind = %v，期望 KindConfig", bad, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("非法 RunID 建出了目录")
	}
}

// TestListRuns 钉住 `Engine.List` 需要的枚举面：只认合法 run 目录。
func TestListRuns(t *testing.T) {
	s := newStore(t)
	for _, id := range []harness.RunID{"run-a", "run-b"} {
		if _, err := s.ForRun(id); err != nil {
			t.Fatal(err)
		}
	}
	// 一个普通文件不该被当成 run。
	if err := os.WriteFile(filepath.Join(s.Dir(), "runs", "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids, err := s.ListRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "run-a" || ids[1] != "run-b" {
		t.Fatalf("ListRuns = %v，期望 [run-a run-b]", ids)
	}
}

// TestRootHandleRejectsRunOps 钉住根句柄与 run 视图的边界：根句柄没有 run
// 上下文，任何读写都必须明确报错，而不是落到存储根目录上。
func TestRootHandleRejectsRunOps(t *testing.T) {
	s := newStore(t)
	if err := s.Append(ev(1, harness.EvRunCreated, "x")); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("根句柄 Append 应 KindConfig，实际 %v", err)
	}
	if _, err := s.Snapshot(t.Context()); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("根句柄 Snapshot 应 KindConfig，实际 %v", err)
	}
	if err := s.PutGraph([]byte("{}")); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("根句柄 PutGraph 应 KindConfig，实际 %v", err)
	}
	if _, err := s.GetGraph(); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("根句柄 GetGraph 应 KindConfig，实际 %v", err)
	}
}

// TestDirAndPrivate 是接口层面的自检：store 必须满足两个契约接口。
func TestDirAndPrivate(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-iface", 1)
	var _ harness.Store = r
	var _ harness.GraphStore = r
	if !strings.HasSuffix(r.Dir(), filepath.Join("runs", "run-iface")) {
		t.Errorf("Dir() = %s，期望以 runs/run-iface 结尾", r.Dir())
	}
	if r.Private() == nil {
		t.Fatal("Private() 不得为 nil")
	}
}

// TestAtomicWriteFailureLeavesNoTrace 钉住原子写的**失败路径**：报错、清理
// 临时文件、绝不破坏目标。
//
// 注入手法是确定性的（不依赖非 root）：在目标路径上先放一个**目录**，于是
// `rename(tmp, path)` 必然失败（Linux 上把文件 rename 到目录上会 EISDIR）。
// 这条路径覆盖的是「临时文件已经写完并 fsync 了、最后一步才失败」——正是
// 最需要清理逻辑的那一种。真实崩溃窗口（写到一半被杀）由 rename 的原子性
// 覆盖：读者要么看到旧版、要么看到新版。
func TestAtomicWriteFailureLeavesNoTrace(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-atomic")
	if err != nil {
		t.Fatal(err)
	}
	// 目标路径上放一个非空目录，让 rename 必失败。
	blocked := filepath.Join(r.Dir(), "graph.json")
	if err := os.MkdirAll(filepath.Join(blocked, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	err = r.PutGraph([]byte(`{"schema":1}`))
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("rename 失败必须报 KindPersistence，实际 %v", err)
	}
	// 目标没被破坏（它还是那个目录）。
	fi, statErr := os.Stat(blocked)
	if statErr != nil || !fi.IsDir() {
		t.Fatalf("失败路径破坏了目标: %v %v", fi, statErr)
	}
	// 临时文件必须被清掉——残留的 `.tmp.*` 会让人误以为「有一次写没完成」，
	// 而下次写用的是另一个唯一名，永远不会被回收。
	assertNoTmp(t, s.Dir())
}

// TestSnapshotWriteFailureKeepsRunJSONReadable 钉住「快照写失败不会让 run.json
// 变成半截」：写失败的路径必须报错并清理临时文件，而不是留下一个可能被
// 恢复路径读到的残片。
func TestSnapshotWriteFailureKeepsRunJSONReadable(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-snapfail", 1)
	// 把 run.json 变成一个目录 ⇒ 原子写的 rename 必失败。
	if err := os.Remove(filepath.Join(r.Dir(), "run.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.Dir(), "run.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.PutSnapshot(snap("run-snapfail", 9)); !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("快照写失败必须报 KindPersistence，实际 %v", err)
	}
	assertNoTmp(t, s.Dir())
}

// assertNoTmp 断言目录树里没有残留的临时文件。
func assertNoTmp(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(d.Name(), ".tmp.") {
			t.Errorf("残留临时文件: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTmpFileIsInTargetDir 钉住临时文件与目标**同目录**。
//
// 为什么这条值得单列：rename 只在同一个文件系统内是原子的，跨设备会直接
// 失败（EXDEV）。把临时文件放进 os.TempDir() 是这类实现最常见的写法，而它
// 在「存储根挂在另一个挂载点上」时会**静默降级**成非原子写——正是本包最想
// 避免的形态。
func TestTmpFileIsInTargetDir(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-tmpdir")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		filepath.Join(r.Dir(), "run.json"),
		filepath.Join(r.Dir(), "private", "evidence", "x.txt"),
	} {
		if got := filepath.Dir(store.TmpName(target)); got != filepath.Dir(target) {
			t.Errorf("TmpName(%s) 的目录 = %s，必须与目标同目录", target, got)
		}
	}
}

// TestPutReportRejectsUnknownName 钉住报告名白名单（布局的一部分）。
func TestPutReportRejectsUnknownName(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, "run-report", 1)
	for _, bad := range []string{"", "report.txt", "../report.md", "sub/report.md"} {
		if err := r.PutReport(bad, []byte("x")); !harness.IsKind(err, harness.KindConfig) {
			t.Errorf("PutReport(%q) 应 KindConfig，实际 %v", bad, err)
		}
	}
}

// TestCrashThenResumeIsRecoverable 是整包的端到端故事：**一次真实的崩溃之后
// 用一个全新的 store 实例恢复**。
//
// 崩法刻意选最坏的一种组合（都是真实发生过的）：
//   - 事件写完了、快照没写成（进程在两次写之间被杀）；
//   - 事件日志末尾留了半行；
//   - 账本末尾留了半行。
//
// 恢复的要求（T11 的恢复路径据此实现）：
//   - 快照落后于日志 ⇒ 基线取快照的 lastAppliedSeq，重放能补齐；
//   - 半行不阻断恢复；
//   - 已提交的候选仍被认成「已提交」（不能重提）。
func TestCrashThenResumeIsRecoverable(t *testing.T) {
	root := t.TempDir()
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ForRun("run-crash")
	if err != nil {
		t.Fatal(err)
	}
	priv := r.Private()
	if err := priv.PutCandidate(harness.Candidate{
		Flag: fakeFlag, Provenance: harness.ProvenanceObserved,
		Submitted: true, Correct: true, IntentID: "intent-A", Round: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Append(ev(1, harness.EvRunCreated, "run-crash")); err != nil {
		t.Fatal(err)
	}
	// 第 2 条事件写完之后快照写失败 ⇒ 崩溃点。
	r.SetSnapshotHook(func() error { return errors.New("崩溃") })
	if err := r.Append(ev(2, harness.EvRoundStarted, "run-crash")); err == nil {
		t.Fatal("注入的崩溃必须透出")
	}
	// 手工制造两处半行（进程被杀时最常见的形态）。
	appendRaw(t, filepath.Join(r.Dir(), "events.jsonl"), `{"seq":3,"type":"cand`)
	appendRaw(t, filepath.Join(r.Dir(), "private", "candidates.jsonl"), `{"flag":"flag{half`)

	// 全新实例（模拟重启后的进程）。
	s2, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s2.ForRun("run-crash")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := r2.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("崩溃后快照必须仍可读: %v", err)
	}
	if snap.LastAppliedSeq != 1 {
		t.Fatalf("快照基线 = %d，期望 1（落后于日志是合法的）", snap.LastAppliedSeq)
	}
	// 重放：从基线之后取事件，末行半行被跳过。
	replay := mustLoad(t, r2, snap.LastAppliedSeq)
	if len(replay) != 1 || replay[0].Seq != 2 {
		t.Fatalf("重放 = %v，期望 [2]", seqs(replay))
	}
	// 下一条事件的序号必须是 3（半行的 3 不算，日志末尾完整的是 2）。
	if err := r2.Append(ev(3, harness.EvRoundStarted, "run-crash")); err != nil {
		t.Fatalf("崩溃后必须能继续追加: %v", err)
	}
	// 已确认的候选不得被当成「未提交」（否则恢复后会重复提交）。
	fps, err := r2.Private().Rejected()
	if err != nil {
		t.Fatalf("崩溃后账本必须仍可读: %v", err)
	}
	if len(fps) != 0 {
		t.Fatalf("已确认的候选不该进判错集: %v", fps)
	}
}

// appendRaw 往文件末尾追加一段原始字节（模拟被 kill 的进程留下的残片）。
func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
