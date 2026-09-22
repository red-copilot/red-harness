package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

func TestPrivateTracePermissionsAndPublicSeparation(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "flag{trace-only-canary}"
	if err := rs.AppendTrace(context.Background(), "run-1", "../../hostile", harness.Event{Kind: harness.EventToolEnd, Output: secret}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "private", "run-1"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("private trace entries=%d err=%v", len(entries), err)
	}
	path := filepath.Join(root, "private", "run-1", entries[0].Name())
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("trace file mode=%v err=%v", info.Mode(), err)
	}
	for _, dir := range []string{filepath.Join(root, "private"), filepath.Join(root, "private", "run-1")} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("trace directory %s mode=%v err=%v", dir, info.Mode(), err)
		}
	}
	trace, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(trace), secret) {
		t.Fatalf("private trace missing canary: %v", err)
	}
	if err := rs.Save(context.Background(), harness.RunResult{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(filepath.Join(root, "results", "run-1.json"))
	if err != nil || strings.Contains(string(public), secret) {
		t.Fatalf("private trace leaked to result: %v", err)
	}
}

// TestTraceFileNameComesFromChallengeIDFor：trace 的文件名**必须**是
// `harness.ChallengeIDFor(code)` 的产物，而不是本包自己算的哈希。
//
// 这条测试守的是一次真实的漂移风险：这里过去是一行内联的 `sha256.Sum256`。
// 题目身份现在有两个消费者（这份 trace 的文件名、`challenges/<id>/attempts/<n>/`
// 的产物路径），两处各算一次的话，它们是「同一个名字下的两种值」——错配是静默的，
// 因为写的时候两边各自看着都对，只有拿一处的名字去找另一处写下的东西时才暴露。
// 所以断言不看「是不是 sha256」，只看**与唯一真源逐字相等**。
func TestTraceFileNameComesFromChallengeIDFor(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	// 刻意用一个**会被路径带走**的编号：平台下发的 code 不得成为路径段。
	const code = "../../hostile/../code"
	if err := rs.AppendTrace(context.Background(), "run-hash", code, harness.Event{Kind: harness.EventToolEnd}); err != nil {
		t.Fatal(err)
	}
	cid, err := harness.ChallengeIDFor(code)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, privateDirName, "run-hash", string(cid)+".jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("trace 文件名不是 ChallengeIDFor 的产物（期望 %s）: %v", want, err)
	}
	// 目录里**只能有**这一个文件：多出来的那个就是「另一份哈希实现」留下的。
	entries, err := os.ReadDir(filepath.Dir(want))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("trace 目录里有 %d 个文件 %v，期望恰好 1 个", len(entries), names)
	}
}

// TestTraceRejectsBlankChallengeCode：空/全空白编号 ⇒ KindConfig，且**不落盘**。
//
// 判空只有一处（`ChallengeIDFor`）：这里再判一次 `code == ""` 就是第二个「什么算
// 合法编号」的答案，而两者迟早有一个先漂移——空白编号正是它们分歧的那一类。
func TestTraceRejectsBlankChallengeCode(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"", "   ", "\t\n"} {
		err := rs.AppendTrace(context.Background(), "run-blank", code, harness.Event{Kind: harness.EventToolEnd})
		if !harness.IsKind(err, harness.KindConfig) {
			t.Errorf("编号 %q 的错误 = %v，期望 KindConfig", code, err)
		}
	}
	// 被拒的输入不得留下任何目录：`runs/<id>//graph.json` 这类空路径段会把布局
	// 悄悄改掉，而「改掉了」与「没写」在目录列表上看起来一样。
	if entries, err := os.ReadDir(filepath.Join(root, privateDirName)); err == nil && len(entries) != 0 {
		t.Fatalf("被拒的编号不该建出目录: %v", entries)
	}
}
