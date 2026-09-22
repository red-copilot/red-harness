package store

import (
	"context"
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
//
// 题目哈希**只能有一处实现**：它是 `harness.ChallengeIDFor`（根包）。这里过去
// 是一行内联的 `sha256.Sum256([]byte(challenge))`，而题目身份现在有两个消费者
// ——这份 trace 的文件名与 `challenges/<id>/attempts/<n>/` 的产物路径——两处各
// 算一次的话，它们是「同一个名字下的两个值」，错配是静默的（read 时找不到写时
// 写下的那个文件，而两边各自看着都对）。
//
// 编号为空/全空白 ⇒ KindConfig，由 ChallengeIDFor 判定（不再在这里另判一次
// `challenge == ""`：两处判空就是两个「什么算合法编号」的答案，而其中一个会
// 先漂移）。
func (s *ResultFileStore) AppendTrace(ctx context.Context, runID harness.RunID, challenge string, event harness.Event) error {
	if err := ctx.Err(); err != nil {
		// ctx 取消是**取消**，不是落盘故障：调用方靠 errors.Is(err, context.Canceled)
		// 判它，包成 KindPersistence 会让「用户按了 Ctrl-C」变成「磁盘坏了」。
		return err
	}
	if err := validRunID(runID); err != nil {
		return err
	}
	cid, err := harness.ChallengeIDFor(challenge)
	if err != nil {
		return err
	}
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if len(line) > eventMaxTraceBytes {
		return harness.Ef(harness.KindPersistence, "resultstore.trace", "单条 trace 超限", nil)
	}
	// 用 privateDirName 而不是字面量：这条路径必须与 NewResultStore 在构造时
	// 建出来的那个、以及 bridge 的 PrivateDir 是**同一个**——三处写字面量迟早漂移，
	// 而漂移的表现是「trace 写到 A、桥的 stderr 找 B」，两边都看着正常。
	dir := filepath.Join(filepath.Dir(s.root), privateDirName, string(runID))
	if err := mkdirAllPrivate(dir, dirPerm); err != nil {
		return err
	}
	path := filepath.Join(dir, string(cid)+".jsonl")
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
