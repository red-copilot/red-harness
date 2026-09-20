// Command red-harness 是 red-harness 的 CLI 入口。
//
// 这里**只做一件事**：把进程参数与进程 stdio 交给 `cli.Main`，用它的返回值
// 作为退出码。全部逻辑（子命令解析、输出归属、退出码映射）都在
// `internal/cli` 里，这样 CLI 才能被单元测试覆盖——把逻辑写在这个文件里意味着
// 它只能在真起进程时才能被测到。
package main

import (
	"os"

	"github.com/red-copilot/red-harness/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
