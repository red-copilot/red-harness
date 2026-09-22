package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/legacy"
)

// 本文件负责 events.jsonl：追加、读取、以及「撕裂末行」的处理。

// appendEventLine 把一整行事件追加到日志末尾。
//
// 三个要点，各防一类事故：
//
//  1. **一次 write 系统调用写完「整行 + \n」**。分成多次写（或先写内容再写
//     换行）会在崩溃时留下半行——而半行让**整个 run 无法恢复**（Review Focus
//     #4）。一次写完 ⇒ 崩溃时要么整行在、要么整行不在。所以这里检查
//     `n == len(line)`：短写（磁盘满、配额）必须报错，绝不能当成写成功。
//
//  2. **先补撕裂的尾巴**（repairTornTail）。上一次崩溃留下的半行若不清掉，
//     本次追加会紧接在它后面，于是「半行 + 完整行」拼成**一条**物理行——它
//     不再是末行，下一次 LoadEvents 会把它当成非末行损坏而整份报错，run 就
//     永久不可恢复了。修法是把半行截掉（日志是 append-only 的，单写者前提下
//     截掉自己的半行是安全的）。
//
//  3. **fsync**。rename 的原子性只保证「快照不会半截」，不保证「快照引用的
//     事件已经落盘」。断电时快照可能比事件先持久化，于是快照指向一条不存在
//     的事件——正是「先事件后快照」这条顺序想防的事。每条事件一次 fsync 的
//     代价按轮计（一轮几分钟），换来顺序在断电后仍然成立。
//
// 前提：**单写者**（见 FileStore 的注释）。两个进程同时追加会互相截断。
func (s *FileStore) appendEventLine(line []byte) error {
	path := s.eventsPath()
	if err := repairTornTail(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开事件日志 %s 失败: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	// umask 会吃掉 OpenFile 的 mode：事件日志必须 0600（它含目标地址、事实
	// 摘要、指纹——不该让同机其他用户读）。
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod 事件日志失败: %w", err)
	}
	n, err := f.Write(line)
	if err != nil {
		return fmt.Errorf("写事件日志 %s 失败: %w", path, err)
	}
	if n != len(line) {
		return fmt.Errorf("写事件日志 %s 短写：%d/%d 字节（磁盘满或配额？）", path, n, len(line))
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync 事件日志 %s 失败: %w", path, err)
	}
	return nil
}

// repairTornTail 把日志末尾不完整的半行截掉。末尾以 \n 结尾（或文件为空）
// 时什么都不做。
func repairTornTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 首次追加，由 O_CREATE 建
		}
		return fmt.Errorf("打开事件日志 %s 失败: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("定位事件日志 %s 末尾失败: %w", path, err)
	}
	if size == 0 {
		return nil
	}
	// 从末尾往前找最后一个 \n 的偏移。按块回读而不是整份读入：日志随轮次
	// 增长，而这里每次追加都要跑一次。
	off, err := lastNewlineOffset(f, size)
	if err != nil {
		return err
	}
	if off == size-1 {
		return nil // 已经以 \n 结尾，没有半行
	}
	cut := off + 1 // off < 0 时 cut == 0 ⇒ 整份截掉（文件里一行完整事件都没有）
	if err := f.Truncate(cut); err != nil {
		return fmt.Errorf("截掉事件日志 %s 的半行失败: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync 事件日志 %s 失败: %w", path, err)
	}
	return nil
}

// lastNewlineOffset 返回 [0, size) 内最后一个 \n 的偏移；没有则返回 -1。
func lastNewlineOffset(f *os.File, size int64) (int64, error) {
	const chunk = 4096
	buf := make([]byte, chunk)
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && !errors.Is(err, io.EOF) {
			return -1, fmt.Errorf("回读事件日志失败: %w", err)
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i), nil
		}
		end = start
	}
	return -1, nil
}

// LoadEvents 返回序号严格大于 afterSeq 的事件，按日志顺序。
//
// **撕裂末行必须被跳过**（Review Focus #4）：进程被杀会留下半行，而「整份
// 日志解析失败」意味着 run 永久不可恢复——用户重启后什么都没了。代价对比是
// 不对称的：跳过残缺末行只丢一轮（那一轮重做），报错丢的是整次运行。
//
// 反过来，**非末行损坏是硬错误**：中间断一行说明日志真的坏了（人工编辑、
// 部分恢复、磁盘损坏），静默跳过等于把状态机的历史剪掉一段，恢复会以错误的
// 基线继续跑而无人知道。
//
// 「末行」的定义是**最后一个物理行**（无论它是否以 \n 结尾）。完整但内容损坏
// 的末行同样按撕裂处理：单写者 + 一次 write 写完一整行的前提下，完整行不可能
// 损坏，所以「完整且损坏」只可能是外部改动，而那种情况下「丢一轮」仍比
// 「整次运行不可恢复」便宜。
func (s *FileStore) LoadEvents(afterSeq int64) ([]legacy.DomainEvent, error) {
	all, err := scanEvents(s.eventsPath())
	if err != nil {
		return nil, err
	}
	if afterSeq <= 0 {
		return all, nil
	}
	out := make([]legacy.DomainEvent, 0, len(all))
	for _, ev := range all {
		if ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	return out, nil
}

// scanEvents 逐行解析事件日志。文件不存在 ⇒ (nil, nil)（首跑）。
func scanEvents(path string) ([]legacy.DomainEvent, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("读事件日志 %s 失败: %w", path, err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	// 先切行、再逐行判断，这样才能知道哪一行是**最后一个物理行**（撕裂判定
	// 需要它）。逐行流式读时「这一行是不是末行」要等到下一次读取才知道。
	lines := splitLines(b)
	out := make([]legacy.DomainEvent, 0, len(lines))
	for i, ln := range lines {
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		var ev legacy.DomainEvent
		if err := json.Unmarshal(ln, &ev); err != nil {
			if i == len(lines)-1 {
				// 撕裂的末行：丢掉它，返回已解析部分。
				return out, nil
			}
			return nil, &harness.Error{
				Kind: harness.KindPersistence,
				Op:   "store.load_events",
				Msg:  fmt.Sprintf("事件日志 %s 第 %d 行损坏（非末行，拒绝静默跳过）", path, i+1),
				Err:  err,
			}
		}
		out = append(out, ev)
	}
	return out, nil
}

// splitLines 按 \n 切行，**保留**最后一段没有换行符的尾巴（撕裂行正是它）。
// 末尾的换行符不产生额外的空行。
func splitLines(b []byte) [][]byte {
	var lines [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			lines = append(lines, b)
			break
		}
		lines = append(lines, b[:i])
		b = b[i+1:]
	}
	return lines
}

// lastSeqOnDisk 返回磁盘上「已落盘的最后一条事件」的序号（没有事件时 0）。
//
// 撕裂的末行**不**计入：它的序号只写了一半，算进去会让下一次 Append 跳到
// 一个空洞序号上。快照里可能记着那个序号（事件写完后才写快照，正常不会），
// 所以调用方取两者较大值。
func lastSeqOnDisk(path string) (int64, error) {
	evs, err := scanEvents(path)
	if err != nil {
		return 0, err
	}
	if len(evs) == 0 {
		return 0, nil
	}
	return evs[len(evs)-1].Seq, nil
}

// eventsPath 返回本次运行的领域事件日志路径。
func (s *FileStore) eventsPath() string { return filepath.Join(s.dir, eventsFileName) }
