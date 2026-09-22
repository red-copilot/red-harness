package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// 本文件是 N0.3「每题产物可寻址」的出口门：同一 run 下两道题各自留下自己的图，
// 而这件事过去做不到——落点是 `<runDir>/graph.json`，一题一份，后写的盖前写的。

// cidFor 把题目编号映射成 ChallengeID。**走根包**，不在这里自己算 sha256：
// 测试里再实现一遍哈希，正是「同一个名字下的两个值」的起点。
func cidFor(t *testing.T, code string) harness.ChallengeID {
	t.Helper()
	cid, err := harness.ChallengeIDFor(code)
	if err != nil {
		t.Fatalf("ChallengeIDFor(%q): %v", code, err)
	}
	return cid
}

// TestForAttemptLayoutIsPinnedHere 钉住**布局字面量**。
//
// 这是唯一一处写出 `challenges/<challengeId>/attempts/<n>` 的地方。布局由 store
// 拼（而不是让装配层拼）的全部意义就在这里：装配层自己 Join 出这棵树的话，
// store 的测试测的是自己拼的那份，而生产走的是另一份——「图写到 A、索引指向 B」
// 从此没有一条断言挡得住。
func TestForAttemptLayoutIsPinnedHere(t *testing.T) {
	s := newStore(t)
	const code = "demo-1"
	cid := cidFor(t, code)

	a, err := s.ForAttempt("run-attempt", cid, harness.FirstAttempt)
	if err != nil {
		t.Fatalf("ForAttempt: %v", err)
	}
	// 布局：<StoreDir>/runs/<runID>/challenges/<64hex>/attempts/<n>
	want := filepath.Join(s.Dir(), "runs", "run-attempt",
		"challenges", string(cid), "attempts", "1")
	if a.Dir() != want {
		t.Fatalf("attempt dir = %s\n             期望 %s", a.Dir(), want)
	}
	// challengeId 是 64 位小写十六进制（根包 ValidChallengeID 的严格判据）。
	if len(cid) != 64 || !harness.ValidChallengeID(string(cid)) {
		t.Fatalf("challengeId = %q，期望 64 位小写十六进制", string(cid))
	}
	// 逐级目录都必须是 0700：中间层宽松等于把「这个 run 跑过哪些题」暴露给同机
	// 其他用户（题库目录名本身不是秘密，但纪律是纪律）。
	for _, dir := range []string{
		filepath.Join(s.Dir(), "runs", "run-attempt", "challenges"),
		filepath.Join(s.Dir(), "runs", "run-attempt", "challenges", string(cid)),
		filepath.Join(s.Dir(), "runs", "run-attempt", "challenges", string(cid), "attempts"),
		a.Dir(),
	} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("目录不存在 %s: %v", dir, err)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Errorf("%s 权限 = %04o，期望 0700", dir, got)
		}
	}

	blob := []byte(`{"schema":1,"code":"demo-1","nodes":[]}`)
	export := []byte("graph LR\n  n0[\"demo-1\"]\n")
	if err := a.PutGraph(blob); err != nil {
		t.Fatalf("PutGraph: %v", err)
	}
	if err := a.PutGraphExport(export); err != nil {
		t.Fatalf("PutGraphExport: %v", err)
	}
	for name, want := range map[string][]byte{"graph.json": blob, "graph.mmd": export} {
		p := filepath.Join(a.Dir(), name)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s 权限 = %04o，期望 0600（含凭证事实与目标地址）", name, got)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, want) {
			t.Errorf("%s 内容 = %s，期望 %s", name, b, want)
		}
	}
	// 句柄自己的 GetGraph 读回来的就是刚写的那份。
	got, err := a.GetGraph()
	if err != nil {
		t.Fatalf("GetGraph: %v", err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("GetGraph = %s，期望 %s", got, blob)
	}
}

// TestForAttemptRejectsBadInput 钉住入口校验：**任何一个参数不合法都必须在
// mkdirAllPrivate 之前返回**，且不得在存储根下落下一个目录。
//
// 先建目录再校验的可观测后果正是「由非法 id 拼出来的目录」——路径逃逸的现场
// 就长这样。
func TestForAttemptRejectsBadInput(t *testing.T) {
	s := newStore(t)
	if _, err := s.ForRun("run-badinput"); err != nil {
		t.Fatal(err)
	}
	good := cidFor(t, "demo-1")

	badIDs := []harness.ChallengeID{
		"",    // 空
		"abc", // 太短
		harness.ChallengeID(strings.Repeat("A", 64)),       // 大写
		harness.ChallengeID(strings.Repeat("g", 64)),       // 非十六进制
		harness.ChallengeID(strings.Repeat("a", 63)),       // 63 位
		harness.ChallengeID(strings.Repeat("a", 63) + "/"), // 长度对，但含路径分隔符
		"..",               // 路径逃逸
		"../../etc/passwd", // 路径逃逸
	}
	for _, cid := range badIDs {
		if _, err := s.ForAttempt("run-badinput", cid, harness.FirstAttempt); !harness.IsKind(err, harness.KindConfig) {
			t.Errorf("ForAttempt(cid=%q) 的 Kind = %v，期望 KindConfig", string(cid), err)
		}
	}
	for _, n := range []harness.AttemptID{0, -1, -100} {
		if _, err := s.ForAttempt("run-badinput", good, n); !harness.IsKind(err, harness.KindConfig) {
			t.Errorf("ForAttempt(attempt=%d) 的 Kind = %v，期望 KindConfig", int(n), err)
		}
	}
	// 非法 RunID 同样在校验之列（它是目录名）。
	if _, err := s.ForAttempt("../escape", good, harness.FirstAttempt); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("ForAttempt(runID=../escape) 的 Kind = %v，期望 KindConfig", err)
	}
	// 存储根下只剩 runs/<id>：没有任何一层是由非法参数拼出来的。
	entries, err := os.ReadDir(filepath.Join(s.Dir(), "runs", "run-badinput"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "challenges" {
			t.Fatalf("非法参数建出了 challenges/：%v", entries)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Error("非法 RunID 建出了目录")
	}
}

// TestForAttemptRefusesCrossRunHandle 钉住「句柄身份与落点不得分家」：
// `id` 是显式参数，所以根句柄上调用是正常的；在一个 run 视图上传**另一个**
// run 的 id 是写错，不是特性。
func TestForAttemptRefusesCrossRunHandle(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-a")
	if err != nil {
		t.Fatal(err)
	}
	cid := cidFor(t, "demo-1")
	// 同一个 run：允许。
	if _, err := r.ForAttempt("run-a", cid, harness.FirstAttempt); err != nil {
		t.Fatalf("同 run 的 ForAttempt 应被接受: %v", err)
	}
	if _, err := r.ForAttempt("run-b", cid, harness.FirstAttempt); !harness.IsKind(err, harness.KindConfig) {
		t.Fatalf("跨 run 的 ForAttempt 的 Kind = %v，期望 KindConfig", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "runs", "run-b")); !errors.Is(err, os.ErrNotExist) {
		t.Error("跨 run 调用仍然建出了目录")
	}
}

// TestRootHandleRejectsAttemptOps 钉住根句柄边界：RunID 上下文缺失时，读写都
// 必须明确报错，而不是落到存储根目录上（与 PutGraph/GetGraph 同一条纪律）。
func TestRootHandleRejectsAttemptOps(t *testing.T) {
	s := newStore(t)
	cid := cidFor(t, "demo-1")
	if _, _, err := s.ReadGraph(cid, harness.FirstAttempt); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("根句柄 ReadGraph 应 KindConfig，实际 %v", err)
	}
	if err := s.RecordChallengeArtifacts(store.ArtifactEntry{ChallengeID: cid,
		Attempt: harness.FirstAttempt, GraphSaver: store.ArtifactSaverWired,
	}); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("根句柄 RecordChallengeArtifacts 应 KindConfig，实际 %v", err)
	}
	if _, err := s.ArtifactIndex(); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("根句柄 ArtifactIndex 应 KindConfig，实际 %v", err)
	}
	// 根部不得留下索引文件。
	if _, err := os.Stat(filepath.Join(s.Dir(), "artifacts.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("根句柄在存储根上写出了 artifacts.json")
	}
}

// TestReadGraphPrefersChallengeLayoutThenFallsBackToLegacyRunRoot 钉住两件事：
//
//  1. **旧路径仍可读**——N0.3 之前的 run 只有 `<runDir>/graph.json` 一份，升级
//     之后它必须还能被读出来，否则「读旧 run」这件事在下一次改动里就悄悄坏了。
//  2. 读到的是哪一种必须**如实返回**：旧 run 的那一份不是「这道题的图」（一题
//     一份，后写的盖前写的），调用方要能自己判断。
func TestReadGraphPrefersChallengeLayoutThenFallsBackToLegacyRunRoot(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-readgraph")
	if err != nil {
		t.Fatal(err)
	}
	cid := cidFor(t, "demo-1")
	other := cidFor(t, "demo-2")

	// 两处都没有 ⇒ os.ErrNotExist（调用方据此区分首跑与损坏）。
	if _, _, err := r.ReadGraph(cid, harness.FirstAttempt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("两处缺图应满足 os.ErrNotExist，实际 %v", err)
	}

	// 手工造一个**旧布局**的 run（N0.3 之前的落点）。
	legacyBlob := []byte(`{"schema":1,"code":"demo-1","nodes":[]}`)
	if err := os.WriteFile(filepath.Join(r.Dir(), "graph.json"), legacyBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	got, src, err := r.ReadGraph(cid, harness.FirstAttempt)
	if err != nil {
		t.Fatalf("旧路径应仍可读: %v", err)
	}
	if src != store.GraphSourceLegacyRunRoot {
		t.Fatalf("来源 = %q，期望 %q", src, store.GraphSourceLegacyRunRoot)
	}
	if !bytes.Equal(got, legacyBlob) {
		t.Fatalf("旧图内容 = %s，期望 %s", got, legacyBlob)
	}

	// 新布局出现之后，旧路径不再被优先读（本题读新那份）。
	a, err := s.ForAttempt("run-readgraph", cid, harness.FirstAttempt)
	if err != nil {
		t.Fatal(err)
	}
	newBlob := []byte(`{"schema":1,"code":"demo-1","nodes":[{"id":"n1"}]}`)
	if err := a.PutGraph(newBlob); err != nil {
		t.Fatal(err)
	}
	got, src, err = r.ReadGraph(cid, harness.FirstAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if src != store.GraphSourceChallenge {
		t.Fatalf("来源 = %q，期望 %q", src, store.GraphSourceChallenge)
	}
	if !bytes.Equal(got, newBlob) {
		t.Fatalf("新布局的图 = %s，期望 %s", got, newBlob)
	}

	// ⚠️ 另一道题仍然读到**旧路径的那一份**：旧 run 里只有一份图，谁也补造不出
	// 第二道题的图。这正是 N0.3 要修的行为，而它在旧 run 上必须保持可读
	// ——这就是 GraphSource 必须如实返回的理由。
	if _, src, err = r.ReadGraph(other, harness.FirstAttempt); err != nil || src != store.GraphSourceLegacyRunRoot {
		t.Fatalf("不同题目读旧路径 = %q, %v；期望 %q（旧 run 只有一份图）",
			src, err, store.GraphSourceLegacyRunRoot)
	}

	// 参数校验与 GetGraph 同形。
	if _, _, err := r.ReadGraph("not-a-cid", harness.FirstAttempt); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("非法 challengeId 应 KindConfig，实际 %v", err)
	}
	if _, _, err := r.ReadGraph(cid, 0); !harness.IsKind(err, harness.KindConfig) {
		t.Errorf("attempt=0 应 KindConfig，实际 %v", err)
	}
}

// TestTwoChallengesKeepDistinctArtifacts 是 **N0.3 的出口门**：
// 同一 run 两道题，各自的图与导出都留在自己的目录里，互不覆盖。
//
// 回归的形状：过去两次 SaveGraph 都写 `<runDir>/graph.json`，跑完只剩最后一题
// 的图，而前一道题的图**没有任何痕迹**说明它存在过。
func TestTwoChallengesKeepDistinctArtifacts(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-two")
	if err != nil {
		t.Fatal(err)
	}
	cid1, cid2 := cidFor(t, "demo-1"), cidFor(t, "demo-2")
	a1, err := s.ForAttempt("run-two", cid1, harness.FirstAttempt)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.ForAttempt("run-two", cid2, harness.FirstAttempt)
	if err != nil {
		t.Fatal(err)
	}
	blob1 := []byte(`{"schema":1,"code":"demo-1","nodes":[{"id":"a"}]}`)
	blob2 := []byte(`{"schema":1,"code":"demo-2","nodes":[{"id":"b"}]}`)
	if err := a1.PutGraph(blob1); err != nil {
		t.Fatal(err)
	}
	if err := a1.PutGraphExport([]byte("graph LR\n  a\n")); err != nil {
		t.Fatal(err)
	}
	if err := a2.PutGraph(blob2); err != nil {
		t.Fatal(err)
	}
	if err := a2.PutGraphExport([]byte("graph LR\n  b\n")); err != nil {
		t.Fatal(err)
	}

	for i, want := range []struct {
		cid  harness.ChallengeID
		blob []byte
		exp  string
	}{{cid1, blob1, "graph LR\n  a\n"}, {cid2, blob2, "graph LR\n  b\n"}} {
		got, src, err := r.ReadGraph(want.cid, harness.FirstAttempt)
		if err != nil {
			t.Fatalf("第 %d 题读图: %v", i+1, err)
		}
		if src != store.GraphSourceChallenge || !bytes.Equal(got, want.blob) {
			t.Fatalf("第 %d 题 = %q / %s，期望 %q / %s", i+1, src, got,
				store.GraphSourceChallenge, want.blob)
		}
		// 人可读导出同样按题目分家（它含凭证事实与目标地址，与图同权限）。
		mmd := filepath.Join(s.Dir(), "runs", "run-two", "challenges", string(want.cid),
			"attempts", "1", "graph.mmd")
		b, err := os.ReadFile(mmd)
		if err != nil {
			t.Fatalf("第 %d 题的导出读不到: %v", i+1, err)
		}
		if string(b) != want.exp {
			t.Fatalf("第 %d 题导出 = %q，期望 %q", i+1, b, want.exp)
		}
	}
	// 两份文件确实在不同目录下（不是同一个文件被写了两次）。
	if a1.Dir() == a2.Dir() {
		t.Fatal("两道题的产物目录相同")
	}
	// ⚠️ 旧落点此时**不存在**：图不再写到 run 根上。这条断言的意义是「新布局是
	// 唯一写入方」——将来若有人「顺手」把图又写回 run 根，它会红。
	if _, err := r.GetGraph(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run 根上的 graph.json 不该存在，实际 %v", err)
	}
}

// TestArtifactIndexMergesAndStaysPrivate 钉住索引的三件事：合并语义（两次登记
// 后两题都在）、权限（0600）、以及「路径一律相对 run 目录」。
func TestArtifactIndexMergesAndStaysPrivate(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-idx")
	if err != nil {
		t.Fatal(err)
	}
	// 还没登记过 ⇒ os.ErrNotExist（调用方据此区分首跑与损坏）。
	if _, err := r.ArtifactIndex(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺索引应满足 os.ErrNotExist，实际 %v", err)
	}

	type want struct {
		code string
		blob []byte
	}
	wants := []want{{"demo-1", []byte(`{"schema":1,"code":"demo-1","nodes":[]}`)},
		{"demo-2", []byte(`{"schema":1,"code":"demo-2","nodes":[{"id":"b"}]}`)}}
	for _, w := range wants {
		cid := cidFor(t, w.code)
		a, err := s.ForAttempt("run-idx", cid, harness.FirstAttempt)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.PutGraph(w.blob); err != nil {
			t.Fatal(err)
		}
		if err := a.PutGraphExport([]byte("graph LR\n  x\n")); err != nil {
			t.Fatal(err)
		}
		if err := r.RecordChallengeArtifacts(store.ArtifactEntry{
			ChallengeID: cid, Code: w.code, Attempt: harness.FirstAttempt,
			GraphSaver:  store.ArtifactSaverWired,
			Graph:       store.ArtifactDeclaration{State: harness.GraphSaved},
			GraphExport: store.ArtifactDeclaration{State: harness.GraphSaved},
		}); err != nil {
			t.Fatalf("RecordChallengeArtifacts(%s): %v", w.code, err)
		}
	}

	idx, err := r.ArtifactIndex()
	if err != nil {
		t.Fatalf("ArtifactIndex: %v", err)
	}
	if idx.Schema != 1 || idx.Layout != "challenges/attempts/v1" ||
		idx.RunID != "run-idx" || idx.GraphSaver != store.ArtifactSaverWired {
		t.Fatalf("索引头不对: %+v", idx)
	}
	if idx.UpdatedAt.IsZero() || time.Since(idx.UpdatedAt) > time.Hour {
		t.Fatalf("updatedAt = %v，期望是刚写的时间", idx.UpdatedAt)
	}
	// **两次登记后两题都在**（合并而不是覆盖）。
	if len(idx.Challenges) != 2 {
		t.Fatalf("索引里 %d 条，期望 2 条（后者不得覆盖前者）", len(idx.Challenges))
	}
	for i, w := range wants {
		cid := string(cidFor(t, w.code))
		row := idx.Challenges[i]
		if row.ChallengeID != cid || row.Code != w.code || row.Attempt != 1 {
			t.Fatalf("第 %d 条 = %+v，期望 challengeId=%s code=%s attempt=1", i+1, row, cid, w.code)
		}
		if row.Graph.State != harness.GraphSaved || row.GraphExport.State != harness.GraphSaved {
			t.Fatalf("第 %d 条状态 = %+v", i+1, row)
		}
		// 路径相对 run 目录，且 sha256/字节数与**磁盘上的字节**一致——索引的
		// 全部用处就是事后能拿它核对「图有没有被拷坏」。
		wantRel := filepath.ToSlash(filepath.Join("challenges", cid, "attempts", "1", "graph.json"))
		if row.Graph.Path != wantRel {
			t.Fatalf("第 %d 条 graph.path = %q，期望 %q", i+1, row.Graph.Path, wantRel)
		}
		sum := sha256.Sum256(w.blob)
		if row.Graph.SHA256 != hex.EncodeToString(sum[:]) || row.Graph.Bytes != int64(len(w.blob)) {
			t.Fatalf("第 %d 条摘要/字节 = %s/%d，期望 %s/%d", i+1,
				row.Graph.SHA256, row.Graph.Bytes, hex.EncodeToString(sum[:]), len(w.blob))
		}
	}

	// 文件权限 0600、内容里**绝不出现宿主绝对路径**。
	idxPath := filepath.Join(r.Dir(), "artifacts.json")
	fi, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("artifacts.json 权限 = %04o，期望 0600", got)
	}
	b, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(s.Dir())) || bytes.Contains(b, []byte(r.Dir())) {
		t.Fatalf("索引里出现了宿主绝对路径: %s", b)
	}
	// 同一 (challengeId, attempt) 再登记是**覆盖**，不是追加。
	if err := r.RecordChallengeArtifacts(store.ArtifactEntry{
		ChallengeID: cidFor(t, "demo-1"), Code: "demo-1", Attempt: harness.FirstAttempt,
		GraphSaver: store.ArtifactSaverWired,
		Graph:      store.ArtifactDeclaration{State: harness.GraphSaved},
		GraphExport: store.ArtifactDeclaration{
			State: harness.GraphFailed, Stage: "export"},
	}); err != nil {
		t.Fatal(err)
	}
	idx, err = r.ArtifactIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Challenges) != 2 {
		t.Fatalf("覆盖后 %d 条，期望仍是 2 条", len(idx.Challenges))
	}
	var first store.ArtifactIndexEntry
	for _, e := range idx.Challenges {
		if e.Code == "demo-1" {
			first = e
		}
	}
	if first.GraphExport.State != harness.GraphFailed || first.GraphExport.Stage != "export" {
		t.Fatalf("覆盖没有生效: %+v", first.GraphExport)
	}
	// failed 但导出文件其实写出来了：登记实际存在的那一份（图也一样）。
	if first.GraphExport.Path == "" || first.GraphExport.SHA256 == "" {
		t.Fatalf("failed 但文件存在时应登记它: %+v", first.GraphExport)
	}
}

// TestRecordChallengeArtifactsRejectsInconsistentDeclarations 钉住入口校验：
// 一份**自相矛盾**的索引比没有索引更糟——它会让「图留下来了没有」这个唯一的
// 问题得到一个错的答案。
func TestRecordChallengeArtifactsRejectsInconsistentDeclarations(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-idxbad")
	if err != nil {
		t.Fatal(err)
	}
	cid := cidFor(t, "demo-1")
	ok := store.ArtifactDeclaration{State: harness.GraphAbsent}
	base := func() store.ArtifactEntry {
		return store.ArtifactEntry{ChallengeID: cid, Code: "demo-1", Attempt: harness.FirstAttempt,
			GraphSaver: store.ArtifactSaverWired, Graph: ok, GraphExport: ok}
	}

	cases := []struct {
		name string
		mut  func(e *store.ArtifactEntry)
		kind harness.Kind
	}{
		{"challengeId 非法", func(e *store.ArtifactEntry) { e.ChallengeID = "abc" }, harness.KindConfig},
		{"attempt 为 0", func(e *store.ArtifactEntry) { e.Attempt = 0 }, harness.KindConfig},
		{"graphSaver 空", func(e *store.ArtifactEntry) { e.GraphSaver = "" }, harness.KindConfig},
		{"graphSaver 非法值", func(e *store.ArtifactEntry) { e.GraphSaver = "maybe" }, harness.KindConfig},
		{"题目级 disabled", func(e *store.ArtifactEntry) {
			e.Graph.State = harness.GraphDisabled
		}, harness.KindConfig},
		{"状态为空串", func(e *store.ArtifactEntry) { e.Graph.State = "" }, harness.KindConfig},
		{"absent 带失败阶段", func(e *store.ArtifactEntry) {
			e.Graph.Stage = "write"
		}, harness.KindConfig},
		{"failed 没给阶段", func(e *store.ArtifactEntry) {
			e.Graph.State = harness.GraphFailed
		}, harness.KindConfig},
		{"saved 带失败阶段", func(e *store.ArtifactEntry) {
			e.Graph.State = harness.GraphSaved
			e.Graph.Stage = "write"
		}, harness.KindConfig},
		{"阶段不在白名单", func(e *store.ArtifactEntry) {
			e.Graph.State = harness.GraphFailed
			e.Graph.Stage = "boom"
		}, harness.KindConfig},
		// saved 但文件不在：这是本索引最不能容忍的状态（它会让人以为图留下来了）。
		{"saved 但文件不存在", func(e *store.ArtifactEntry) {
			e.Graph.State = harness.GraphSaved
		}, harness.KindPersistence},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := base()
			tc.mut(&e)
			err := r.RecordChallengeArtifacts(e)
			if !harness.IsKind(err, tc.kind) {
				t.Fatalf("Kind = %v，期望 %v", err, tc.kind)
			}
		})
	}

	// 端口没接上（disabled）却声明了产物：索引会描述一个不存在的部署。
	disabled := base()
	disabled.GraphSaver = store.ArtifactSaverDisabled
	disabled.Graph.State = harness.GraphSaved
	if err := r.RecordChallengeArtifacts(disabled); !harness.IsKind(err, harness.KindConfig) {
		t.Fatalf("disabled + saved 的 Kind = %v，期望 KindConfig", err)
	}
	// disabled + absent 是自洽的（装配层没接端口，本题也就没有产物）。
	disabledOK := base()
	disabledOK.GraphSaver = store.ArtifactSaverDisabled
	if err := r.RecordChallengeArtifacts(disabledOK); err != nil {
		t.Fatalf("disabled + absent 应被接受: %v", err)
	}
	// 装配身份在同一个 run 内不得改变。
	flip := base()
	flip.Code = "demo-2"
	flip.ChallengeID = cidFor(t, "demo-2")
	flip.GraphSaver = store.ArtifactSaverWired
	if err := r.RecordChallengeArtifacts(flip); !harness.IsKind(err, harness.KindConfig) {
		t.Fatalf("graphSaver 变化后 Kind = %v，期望 KindConfig", err)
	}

	// 上面所有被拒的登记都不该留下索引（第一次成功的那条除外）。
	idx, err := r.ArtifactIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Challenges) != 1 || idx.GraphSaver != store.ArtifactSaverDisabled {
		t.Fatalf("被拒的登记不该落盘: %+v", idx)
	}
}

// TestArtifactIndexAtomicWriteFailureLeavesNoTrace 钉住索引的**失败路径**：
// 报错、清理临时文件、绝不破坏目标、也不破坏别的题目的登记。
//
// 注入手法与 store_test.go 的 TestAtomicWriteFailureLeavesNoTrace 同形（确定性、
// 不依赖非 root）：在目标路径上放一个目录，`rename(tmp, path)` 必然失败。
func TestArtifactIndexAtomicWriteFailureLeavesNoTrace(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-idxfail")
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(r.Dir(), "artifacts.json")
	if err := os.MkdirAll(filepath.Join(blocked, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	entry := store.ArtifactEntry{ChallengeID: cidFor(t, "demo-1"), Code: "demo-1",
		Attempt: harness.FirstAttempt, GraphSaver: store.ArtifactSaverWired,
		Graph:       store.ArtifactDeclaration{State: harness.GraphAbsent},
		GraphExport: store.ArtifactDeclaration{State: harness.GraphAbsent}}
	err = r.RecordChallengeArtifacts(entry)
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("rename 失败必须报 KindPersistence，实际 %v", err)
	}
	fi, statErr := os.Stat(blocked)
	if statErr != nil || !fi.IsDir() {
		t.Fatalf("失败路径破坏了目标: %v %v", fi, statErr)
	}
	assertNoTmp(t, s.Dir())

	// 失败之后合并路径仍然可用：把障碍移开，同一条登记应当成功。
	if err := os.RemoveAll(blocked); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordChallengeArtifacts(entry); err != nil {
		t.Fatalf("障碍移开后登记应成功: %v", err)
	}
	idx, err := r.ArtifactIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Challenges) != 1 {
		t.Fatalf("索引 = %+v，期望 1 条", idx)
	}
}

// TestArtifactIndexFailsClosedOnCorruptFile 钉住「索引损坏」不是「没有索引」：
// 一次解析失败必须报错，绝不能顺手覆盖——覆盖会把别的题目的登记一起抹掉。
func TestArtifactIndexFailsClosedOnCorruptFile(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-idxcorrupt")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(r.Dir(), "artifacts.json")
	if err := os.WriteFile(p, []byte("{半截"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ArtifactIndex(); !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("损坏的索引应 KindPersistence，实际 %v", err)
	}
	err = r.RecordChallengeArtifacts(store.ArtifactEntry{ChallengeID: cidFor(t, "demo-1"),
		Attempt: harness.FirstAttempt, GraphSaver: store.ArtifactSaverWired,
		Graph:       store.ArtifactDeclaration{State: harness.GraphAbsent},
		GraphExport: store.ArtifactDeclaration{State: harness.GraphAbsent}})
	if !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("损坏的索引不得被静默覆盖，实际 %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, []byte("{半截")) {
		t.Fatalf("损坏的索引被改写了: %s", b)
	}
}

// TestArtifactIndexRejectsTamperedRows 钉住读侧的复检：索引会被 CLI 与报告消费，
// 一份被人工改坏的索引不该把 `../..` 之类的东西送进下游。
func TestArtifactIndexRejectsTamperedRows(t *testing.T) {
	s := newStore(t)
	r, err := s.ForRun("run-idxtamper")
	if err != nil {
		t.Fatal(err)
	}
	write := func(row map[string]any) {
		t.Helper()
		doc := map[string]any{"schema": 1, "runId": "run-idxtamper",
			"layout": "challenges/attempts/v1", "graphSaver": "wired",
			"updatedAt":  time.Now().UTC().Format(time.RFC3339),
			"challenges": []any{row}}
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.Dir(), "artifacts.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{"challengeId": "../../etc", "attempt": 1})
	if _, err := r.ArtifactIndex(); !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("非法 challengeId 应 KindPersistence，实际 %v", err)
	}
	write(map[string]any{"challengeId": strings.Repeat("a", 64), "attempt": 0})
	if _, err := r.ArtifactIndex(); !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("非法 attempt 应 KindPersistence，实际 %v", err)
	}
	write(map[string]any{"challengeId": strings.Repeat("a", 64), "attempt": 1,
		"graph": map[string]any{"state": "saved", "path": "/etc/passwd"}})
	if _, err := r.ArtifactIndex(); !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("绝对路径应 KindPersistence，实际 %v", err)
	}
	write(map[string]any{"challengeId": strings.Repeat("a", 64), "attempt": 1,
		"graph": map[string]any{"state": "saved", "path": "../../escape.json"}})
	if _, err := r.ArtifactIndex(); !harness.IsKind(err, harness.KindPersistence) {
		t.Fatalf("逃逸路径应 KindPersistence，实际 %v", err)
	}
	// 一份正常的索引读得回来（上面的拒绝不是因为函数总是报错）。
	write(map[string]any{"challengeId": strings.Repeat("a", 64), "attempt": 1,
		"graph": map[string]any{"state": "saved",
			"path": "challenges/" + strings.Repeat("a", 64) + "/attempts/1/graph.json"}})
	idx, err := r.ArtifactIndex()
	if err != nil {
		t.Fatalf("正常索引应可读: %v", err)
	}
	if len(idx.Challenges) != 1 || idx.Challenges[0].Graph.State != harness.GraphSaved {
		t.Fatalf("索引 = %+v", idx)
	}
}
