package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	harness "github.com/red-copilot/red-harness"
)

const maxPrivateTraceBytes = 64 << 20

var _ harness.TraceStore = (*ResultFileStore)(nil)

// AppendTrace stores raw events only below <ResultDir>/private. The challenge
// name is hashed so a platform-provided code cannot become a filesystem path.
func (s *ResultFileStore) AppendTrace(ctx context.Context, runID harness.RunID, challenge string, event harness.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validRunID(runID); err != nil {
		return err
	}
	if challenge == "" {
		return harness.Ef(harness.KindConfig, "resultstore.trace", "题目编号为空", nil)
	}
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if len(line) > eventMaxTraceBytes {
		return harness.Ef(harness.KindPersistence, "resultstore.trace", "单条 trace 超限", nil)
	}
	sum := sha256.Sum256([]byte(challenge))
	dir := filepath.Join(filepath.Dir(s.root), "private", string(runID))
	if err := mkdirAllPrivate(dir, dirPerm); err != nil {
		return err
	}
	path := filepath.Join(dir, hex.EncodeToString(sum[:])+".jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, privatePerm)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(privatePerm); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(line)) > maxPrivateTraceBytes {
		return harness.Ef(harness.KindPersistence, "resultstore.trace", "题目 trace 总量超限", nil)
	}
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("append private trace: %w", err)
	}
	return f.Sync()
}

const eventMaxTraceBytes = 1 << 20
