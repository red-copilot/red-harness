package wire

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	harness "github.com/red-copilot/red-harness"
)

// runLockName 是跨进程单运行锁的文件名。
//
// 它落在 `<StoreDir>/` 下而不是某个 run 目录里：这把锁保护的是「整台宿主上
// 同一时刻只有一个 red-harness 在跑」，与具体某一次运行无关——两个并发的 run
// 会在平台侧互相踩（重复起题、重复提交），而且各自按 run label 回收容器与网络
// 时会**互相删掉对方正在用的资源**。
const runLockName = "run.lock"

// 退避参数。首次重试要快（大多数情况下锁马上就会被放开），上限要小到
// 不会让 ctx 取消后的响应变迟钝。
const (
	lockRetryMin = 20 * time.Millisecond
	lockRetryMax = 500 * time.Millisecond
)

// FileLock 是 harness.RunLocker 的文件锁实现。
//
// **为什么是 flock 而不是 `O_CREATE|O_EXCL`**：O_EXCL 只回答「锁文件在不在」，
// 而锁文件在持有者崩溃、被 SIGKILL、宿主断电之后会**留在原地**——一次崩溃就把
// 这台机器变成永久不可运行（下一个进程只看到「文件存在」，却没有任何办法区分
// 「有人正在跑」与「上次跑的人死了」）。flock 的锁由**内核**持有，进程以任何
// 方式退出（正常返回、panic、SIGKILL、段错误）都会自动释放，所以残留的锁文件
// 不构成任何阻塞：下一次运行照常拿到锁。这是本实现选择 flock 的唯一理由，
// 也是它比「写 pid + 探活」更好的地方——pid 方案要自己判断「pid 还活着吗」
// 「这个 pid 有没有被复用」「对方是不是同一个命名空间里的进程」，每一条都可能
// 猜错，而两种猜错后果（误判为死 ⇒ 两个 run 并发；误判为活 ⇒ 永久卡死）
// 都比把这件事交给内核差。
//
// 注意 flock 的语义是「与打开文件描述绑定」：同一个进程里两次独立的 open()
// 会得到两个互相冲突的描述符，所以**同一进程内的两个 FileLock 实例也会互斥**
// （这正是测试用来验证互斥性的方式）；而同一个实例重复 Lock 是 no-op（同一个
// 描述符，不会自己把自己锁死）。
type FileLock struct {
	path string

	mu sync.Mutex
	// f 非空表示当前持有锁。为 nil 表示未持有。
	f *os.File
}

var _ harness.RunLocker = (*FileLock)(nil)

// NewFileLock 返回路径上的文件锁。构造不碰文件系统——目录可能在后面才建。
func NewFileLock(path string) *FileLock { return &FileLock{path: path} }

// defaultLock 是装配层在调用方没给锁时用的那把：`<StoreDir>/run.lock` 上的 flock。
//
// 为什么缺省要有锁，而不是「不传就没有互斥」：跨进程单运行互斥是**生产必需**
// （两个并发 run 会在平台侧互相踩、并互相删掉对方正在用的容器与网络），把它做成
// 「调用方记得传」的选项，等于让最危险的那种部署成为最容易发生的那种。
func defaultLock(storeDir string) harness.RunLocker {
	return NewFileLock(filepath.Join(storeDir, runLockName))
}

// Lock 取得锁：拿不到就按退避重试，ctx 结束时返回错误。
//
// 为什么是「阻塞但可取消」而不是「立刻失败」：立刻失败会把重试逻辑推给每一个
// 调用方（而且是各写一遍的轮询），而 ctx 已经能表达「我愿意等多久」。
func (l *FileLock) Lock(ctx context.Context) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		// 已持有：同一个描述符上再取一次锁是 no-op，不能在这里把自己挡死。
		return nil
	}
	if err := ctx.Err(); err != nil {
		return harness.Ef(harness.KindCancelled, "wire.lock", "获取单运行锁前 ctx 已结束", err)
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return harness.Ef(harness.KindPersistence, "wire.lock", "创建锁文件所在目录失败", err)
	}
	// 0600：锁文件里没有任何内容，但同一台机器上的其他用户也没有理由碰它。
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return harness.Ef(harness.KindPersistence, "wire.lock", "打开锁文件失败", err)
	}
	delay := lockRetryMin
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			l.f = f
			return nil
		case errors.Is(err, syscall.EINTR):
			// 被信号打断不是「锁被占用」，立刻重试即可。
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			// 别人持有锁。等一小会儿再看——等待期间必须能被 ctx 打断，
			// 否则 Ctrl-C 之后进程会一直挂在这里（用户唯一的出路是再按一次
			// Ctrl-C，而第二次按下去的是默认处置，清理路径不会跑）。
		default:
			_ = f.Close()
			return harness.Ef(harness.KindPersistence, "wire.lock", "申请文件锁失败", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return harness.Ef(harness.KindConfig, "wire.lock",
				"获取单运行锁超时：另一个 red-harness 进程正在运行（同一时刻只允许一个 run）", ctx.Err())
		case <-timer.C:
		}
		if delay < lockRetryMax {
			delay *= 2
			if delay > lockRetryMax {
				delay = lockRetryMax
			}
		}
	}
}

// Unlock 释放锁。**幂等**：没持有、已释放、重复调用都返回 nil。
//
// 幂等是硬要求：调用方会同时用 defer 与显式路径释放（`defer locker.Unlock()`
// 之外，装配失败路径也会主动放），两条路径都跑的时候第二次必须是 no-op 而不是
// 报错——报错会被当成装配故障，把真正的失败原因盖掉。
func (l *FileLock) Unlock() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	// 先显式解锁再关闭：关闭描述符本身也会释放 flock，但显式 LOCK_UN 让
	// 「锁已释放」这件事不依赖 close 的实现细节。
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err := f.Close(); err != nil {
		return harness.Ef(harness.KindPersistence, "wire.lock", "关闭锁文件失败", err)
	}
	return nil
}
