package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	harness "github.com/red-copilot/red-harness"
)

func TestResultFileStoreNeverPersistsCandidatePlaintext(t *testing.T) {
	root := t.TempDir()
	rs, err := NewResultStore(root)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "flag{candidate_must_stay_private}"
	run := harness.RunResult{RunID: "run-1", Err: secret, Challenges: []harness.ChallengeResult{{
		Challenge: harness.Challenge{Code: "fake"},
		Outcome:   harness.OutcomeView{Flags: []string{secret}, Candidates: []harness.Candidate{{Flag: secret}}},
	}}}
	if err := rs.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, resultsDirName, "run-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "" || containsBytes(b, []byte(secret)) {
		t.Fatalf("result file leaked candidate plaintext: %s", b)
	}
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
