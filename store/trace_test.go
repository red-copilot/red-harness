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
