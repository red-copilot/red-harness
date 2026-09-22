// Package wire 是 `local` 包的**迁移窗口薄转发层**。它不再有任何实现。
//
// 它曾经是 v0.4 的唯一装配层（实现就在这个文件里）。N0.1 把实现整体搬到了
// `local/`，理由是 Go 的 internal 可见性规则：`internal/wire` 只能被本模块
// import，于是**仓库内的示例证明不了「外部可用」**——那正是 N0.1 要证明的事。
// 搬完之后 `example/external/`（独立 module）import 的是 `local`。
//
// 为什么要留一轮转发，而不是当场删掉：
//
//   - `cmd/red-harness/main.go` 与 `internal/cli` 的接线点在这次搬迁时还没切过
//     来，这里必须继续编译、继续工作一阵子。
//
// ⚠️ **那一轮已经结束了**：CLI 侧在 `ed6f7af` 切到 `local`（`cli.Main` 收显式的
// 装配函数参数，包级 `SetWire` 删除），于是本包现在**没有任何 import 方**，连它
// 自己的测试都只测转发。`legacy/layering_test.go` 的装配点集合也已收紧成单个
// `local`。本包现在的去处要么是删掉，要么是继续当别名层——但**不要再往这里加
// 任何东西**：它已经不在任何生产路径上了。
//   - 下面用的是**别名**（`type Options = local.Options`）而不是「字段相同的另
//     一个结构体」：别名让两边的类型**就是同一个**，`cli`/`cmd` 传进来的值不
//     需要任何转换，也不存在「两份结构体悄悄漂移」的中间态。
//
// ⚠️ **本包不得 import 任何实现包**（dag/gate/store/executor/bridge/piai/
// scenario）：`legacy/layering_test.go` 的第 4 条断言要求「同时 import ≥2 个
// 实现包的包**恰好只有一个**」，那一个是 `local`。这里一旦为了省事 import 一个
// dag 或 gate，断言立刻变红——那正是它存在的意义，不要改断言去迁就它。
//
// 迁移窗口结束（`cmd`/`cli` 切完之后）整个目录可以直接删掉；删之前请确认
// `grep -rn "red-harness/internal/wire"` 只剩本目录自己的测试。
package wire

import (
	"context"

	harness "github.com/red-copilot/red-harness"
	"github.com/red-copilot/red-harness/local"
)

// 场景名。**是别名不是重新定义的常量**：值只在 local 里写一次，两处各写一遍
// 迟早会出现「CLI 认这个串、装配层认那个串」。
const (
	// ScenarioFake 是离线场景：状态全在内存里，不碰网络、不碰平台。
	ScenarioFake = local.ScenarioFake
	// ScenarioTSecBench 是真实平台场景（经 Python SDK bridge 常驻子进程）。
	ScenarioTSecBench = local.ScenarioTSecBench
)

// 配置与运行时类型：全部是别名，所以 `wire.X` 与 `local.X` 是同一个类型。
type (
	// Options 是装配一台 Harness 需要的全部输入。
	Options = local.Options
	// AgentOptions 是 pi 的启动配置。
	AgentOptions = local.AgentOptions
	// SandboxOptions 是 Docker sandbox 的配置。
	SandboxOptions = local.SandboxOptions
	// RunOptions 是每次运行的缺省配置。
	RunOptions = local.RunOptions
	// Runner 是装配完成的 Harness 及其附属物。
	Runner = local.Runner
	// FileLock 是 harness.RunLocker 的文件锁实现。
	FileLock = local.FileLock
)

// New 转发到 local.New。见那里的注释（两道装配闸都在实现里）。
func New(opts Options) (*Runner, error) { return local.New(opts) }

// NewFileLock 转发到 local.NewFileLock。
func NewFileLock(path string) *FileLock { return local.NewFileLock(path) }

// 编译期断言：转发层没有改变 CLI 要的那两个动作的形状。别名让这条断言在今天
// 恒真，但它钉住的是「将来有人把别名换成手抄结构体」——那时这条会立刻编译不过。
var _ interface {
	Run(context.Context, harness.RunSpec) (harness.RunResult, error)
	Doctor(context.Context) harness.DoctorReport
} = (*Runner)(nil)
