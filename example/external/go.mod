// 这是一个**独立 module**，它存在的唯一理由是证明「仓库外能被 import」。
//
// 为什么非要是独立 module：Go 的 internal 可见性规则让 `internal/...` 只能被
// 本模块 import，所以仓库**内部**的任何示例都无法证明公开面真的可用——它连
// import 一个不该被外部看见的包都能编译通过。只有换一个 module 路径，编译器的
// 可见性检查才真的开始工作（N0.1 的出口门）。
//
// ⚠️ 它**不是**发布产物，也不该被 `go run ./...` 从仓库根扫到：
// 带 go.mod 的目录是嵌套 module，父模块的 `./...` 模式不会走进去。
// 验收方式是 `cd example/external && go build ./...`。
//
// replace 指向仓库根：本地校验时用的是工作副本里的代码，而不是任何已发布的
// 版本——这正是「检查的是我手上这份」的意思。
module example.com/red-harness-external-check

go 1.26

require github.com/red-copilot/red-harness v0.0.0

replace github.com/red-copilot/red-harness => ../..
