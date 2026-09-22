package local

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// 本文件只测锁的**可观察行为**，不测它的实现细节（fd、flock 参数）。
// 每一条都对应一个真实事故形态，不是「为了覆盖率」。

// TestFileLockMutualExclusion 钉「同一时刻只有一个持有者」。
//
// 为什么用两个**独立实例**而不是同一个实例重复 Lock：flock 的语义是「与打开
// 文件描述绑定」，同一个实例重复 Lock 是 no-op（那是幂等，不是互斥）。两个实例
// 会各自 open 一次，得到两个互相冲突的描述符——这正是「两个进程」在单进程里的
// 等价物。真实的跨进程互斥由 TestFileLockCrashDoesNotBlock 里的子进程覆盖。
func TestFileLockMutualExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), runLockName)
	a, b := NewFileLock(path), NewFileLock(path)

	if err := a.Lock(context.Background()); err != nil {
		t.Fatalf("第一个锁应当立刻拿到: %v", err)
	}
	defer func() { _ = a.Unlock() }()

	// 第二个必须**拿不到**：给它一个很短的上限，让「拿不到」以 ctx 超时的形式
	// 返回，而不是把测试挂死。
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := b.Lock(ctx); err == nil {
		t.Fatal("第二个锁在第一个还持有时拿到了锁：互斥失效")
	}
	// 释放之后必须能拿到：否则「拿不到」也可能只是因为它永远拿不到。
	if err := a.Unlock(); err != nil {
		t.Fatalf("Unlock 报错: %v", err)
	}
	if err := b.Lock(context.Background()); err != nil {
		t.Fatalf("第一个锁释放后第二个应当拿到: %v", err)
	}
	_ = b.Unlock()
}

// TestFileLockCancelReturnsError 钉「等待可被取消」。
//
// 没有这条，`Lock` 一旦改成忙等就再也没人发现——而它挂在的是 Ctrl-C 的清理路径
// 上（CLI 的 SIGINT 走 ctx 取消）。
func TestFileLockCancelReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), runLockName)
	held := NewFileLock(path)
	if err := held.Lock(context.Background()); err != nil {
		t.Fatalf("预置持有者失败: %v", err)
	}
	defer func() { _ = held.Unlock() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewFileLock(path).Lock(ctx)
	}()
	// 给等待者一点时间真的进入重试循环，再取消。
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ctx 取消后 Lock 返回了 nil")
		}
		if !harness.IsKind(err, harness.KindConfig) {
			t.Fatalf("取消应当是可诊断的配置类错误，得到 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后 Lock 没有返回：等待不可取消")
	}
}

// TestFileLockUnlockIdempotent 钉「重复 Unlock 是 no-op」。
//
// 为什么这是硬要求：调用方会同时用 defer 与显式路径释放（装配失败路径也会放），
// 第二次若报错，那个错误会把真正的失败原因盖掉。
func TestFileLockUnlockIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), runLockName)
	l := NewFileLock(path)
	if err := l.Lock(context.Background()); err != nil {
		t.Fatalf("Lock 失败: %v", err)
	}
	for i := range 3 {
		if err := l.Unlock(); err != nil {
			t.Fatalf("第 %d 次 Unlock 报错: %v", i+1, err)
		}
	}
	// 未持有过就 Unlock 也必须是 nil。
	if err := NewFileLock(path).Unlock(); err != nil {
		t.Fatalf("未持有就 Unlock 报错: %v", err)
	}
}

// TestFileLockSameInstanceRelockIsNoop 钉「同一个实例重复 Lock 不自杀」。
//
// flock 下同一个描述符重复 LOCK_EX 是成功的；如果实现改成「先关再开」，第二次
// Lock 会把第一次的锁放掉——那是个只在重入路径上出现的静默 bug。
func TestFileLockSameInstanceRelockIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), runLockName)
	l := NewFileLock(path)
	if err := l.Lock(context.Background()); err != nil {
		t.Fatalf("首次 Lock 失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := l.Lock(ctx); err != nil {
		t.Fatalf("同实例重复 Lock 应当立刻成功: %v", err)
	}
	// 重复 Lock 之后仍然真的持有：另一个实例必须还拿不到。
	other := NewFileLock(path)
	short, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if err := other.Lock(short); err == nil {
		t.Fatal("重复 Lock 之后锁已经丢了")
	}
	_ = l.Unlock()
}

// TestFileLockCrashDoesNotBlock 钉「上一次崩溃留下的锁文件不会让机器永久不可运行」。
//
// 这是选 flock 而不是 `O_CREATE|O_EXCL` 的**唯一理由**，所以它必须有一条测试，
// 而且必须是**真子进程**：只有真的进程死掉才能证明「内核释放了锁」——在同一个
// 进程里 close 一个 fd 也能释放，但那什么都没证明。
//
// 子进程用 SIGKILL 杀掉：它不给任何 defer / signal handler 机会，正是「崩溃」的
// 最强形式（正常退出、panic、SIGTERM 都可能跑到清理路径）。
func TestFileLockCrashDoesNotBlock(t *testing.T) {
	if os.Getenv("RED_HARNESS_LOCK_CHILD") == "1" {
		// 子进程分支：拿到锁、打印一行、然后一直睡到被 SIGKILL。
		path := os.Getenv("RED_HARNESS_LOCK_PATH")
		if err := NewFileLock(path).Lock(context.Background()); err != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString("locked\n")
		select {} // 永不主动退出：只能被杀。
	}

	dir := t.TempDir()
	path := filepath.Join(dir, runLockName)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取测试可执行文件失败: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestFileLockCrashDoesNotBlock")
	cmd.Env = append(os.Environ(),
		"RED_HARNESS_LOCK_CHILD=1",
		"RED_HARNESS_LOCK_PATH="+path,
	)
	// 子进程的 stdout 必须接住：我们要等它**真的拿到锁**再杀，否则可能杀在一个
	// 还没取到锁的进程上，测出来的就不是「崩溃后残留」。
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("接子进程 stdout 失败: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("起子进程失败: %v", err)
	}
	buf := make([]byte, 32)
	if _, err := stdout.Read(buf); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("子进程没有报「已拿锁」: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("杀子进程失败: %v", err)
	}
	_ = cmd.Wait()

	// 锁文件**确实还在**（O_EXCL 方案就是被它挡住的），但锁必须已经由内核释放。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("锁文件应当留在原地（这正是 O_EXCL 会永久阻塞的场景）: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l := NewFileLock(path)
	if err := l.Lock(ctx); err != nil {
		t.Fatalf("崩溃残留的锁文件挡住了新进程（flock 的意义就是它不该挡）: %v", err)
	}
	_ = l.Unlock()
}
