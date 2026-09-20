package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
)

// tmpCounter 让同一进程内的临时文件名互不相同。
//
// 为什么不用固定的 `path + ".tmp"`：`dag/store.go` 的老实现就是这么写的，
// 两个写者（两个引擎进程、或一个进程里的两条路径）会撞在同一个临时文件名上，
// 后写的覆盖先写的，rename 之后落盘的内容谁写的说不清。store 的前提是单写者
// （见 FileStore 的注释），但临时名唯一是**零成本的第二道保险**：即使前提被
// 破坏，残留文件也能按 pid + 序号归因到具体写者。
var tmpCounter atomic.Uint64

// TmpName 返回 path 对应的唯一临时文件名。
//
// 形如 `<path>.tmp.<pid>.<n>`：pid 区分进程，n 区分同进程内的多次写。
func TmpName(path string) string {
	return fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), tmpCounter.Add(1))
}

// writeFileAtomic 原子地把 data 写到 path，权限 perm。
//
// 顺序：writeTmp → chmod → fsync(file) → rename → fsync(dir)。每一步都对应
// 一个具体的崩溃窗口：
//
//   - **先写临时文件再 rename**：读者要么看到旧版、要么看到新版，绝不会看到
//     半截 JSON。run.json 与 graph.json 都是恢复路径的入口，读到半截会让整个
//     run 无法恢复（`dag/store.go` 的老实现已有这一条，这里沿用）。
//   - **fsync(file)**：rename 只保证目录项的原子性，不保证数据已落盘。没有它，
//     崩溃后可能看到「目录项指向新文件、文件内容是空的」——这正是
//     `dag/store.go` 老实现缺的那一步（它只 rename）。
//   - **fsync(dir)**：rename 本身也是元数据操作，不 fsync 目录的话，崩溃后
//     目录项可能还指向旧文件（或什么都没有）。
//
// 任何一步失败都要删掉临时文件：残留的 `.tmp.*` 不会伤人，但会让人误以为
// 「有一次写没完成」，而下次写用的是另一个唯一名，永远不会被回收。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp := TmpName(path)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("建临时文件 %s 失败: %w", tmp, err)
	}
	// 收尾统一走这个闭包，避免每条错误分支各写一遍清理逻辑（漏一条就留下残渣）。
	cleanup := func(e error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return e
	}
	if _, err := f.Write(data); err != nil {
		return cleanup(fmt.Errorf("写临时文件 %s 失败: %w", tmp, err))
	}
	// chmod 兜底：OpenFile 的 mode 会被 umask 吃掉，而 run.json / graph.json
	// 必须是 0600（含凭证事实与目标地址，不该让同机其他用户读）。
	if err := f.Chmod(perm); err != nil {
		return cleanup(fmt.Errorf("chmod 临时文件 %s 失败: %w", tmp, err))
	}
	if err := f.Sync(); err != nil {
		return cleanup(fmt.Errorf("fsync 临时文件 %s 失败: %w", tmp, err))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("关闭临时文件 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s → %s 失败: %w", tmp, path, err)
	}
	if err := syncDir(dir); err != nil {
		// 数据已经在 path 上了，只有目录项可能没落盘。这仍然要报错——
		// 调用方以为「已经持久化」而崩溃后文件不见了，比直接失败更糟。
		return fmt.Errorf("fsync 目录 %s 失败: %w", dir, err)
	}
	return nil
}

// syncDir 把目录项变更刷到盘上。
//
// 某些文件系统（部分 overlayfs / 网络文件系统）不支持对目录 fsync 并返回
// EINVAL/ENOTSUP。那种情况下退化为「尽力而为」：**不报错**，因为报错会让
// store 在这些文件系统上完全不可用，而 rename 的原子性依然成立。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EBADF) {
			return nil
		}
		return err
	}
	return nil
}

// mkdirAllPrivate 建目录并**逐级**把权限设成 perm。
//
// 为什么要逐级：umask 会吃掉 MkdirAll 的 mode 参数（umask 022 下 0700 仍是
// 0700，但 0777 会变成 0755），而 `<StoreDir>` 与 `<StoreDir>/runs/` 同样是
// 公开面——宽松的父目录会把「哪些 run 存在」暴露给同机其他用户。所以建完
// 之后显式 chmod。
//
// 为什么只 chmod 自己创建的层级：向上 chmod 会碰到调用方给的路径的祖先
// （`/tmp` 之类）。把 `/tmp` chmod 成 0700 会连累同机所有用户——这不是理论
// 风险，`/tmp` 是 t.TempDir() 与默认 StoreDir 的父目录。
func mkdirAllPrivate(path string, perm os.FileMode) error {
	path = filepath.Clean(path)
	// 先找出**尚不存在**的层级（从叶子往上，遇到已存在的就停）。
	var created []string
	for p := path; ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			break
		}
		created = append(created, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	// 叶子无论先前是否存在都要 chmod：调用方可能把权限放宽过（拷来的 run
	// 目录常见 0755），而 store 的契约是 0700。
	if err := os.Chmod(path, perm); err != nil {
		return err
	}
	for _, p := range created {
		if err := os.Chmod(p, perm); err != nil {
			return err
		}
	}
	return nil
}

// SetPublicFileMode 把 path 的权限设成 0644（公开可读）。
//
// 报告（report.json / report.md）是**交付物**：会被拷进工单、贴进聊天、被
// 另一个用户读。它们必须 0644，而候选明文与凭证只出现在 private/ 里
// （见 private.go）。这里显式 chmod 而不是靠创建时的 mode：umask 会吃掉它，
// 而且报告可能是先被别的工具（report 包、编辑器）写出来的。
func SetPublicFileMode(path string) error {
	if err := os.Chmod(path, 0o644); err != nil {
		return fmt.Errorf("chmod %s 为 0644 失败: %w", path, err)
	}
	return nil
}
