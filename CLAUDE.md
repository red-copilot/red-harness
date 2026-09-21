# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 这是什么

面向安全 Agent 开发者的 Go SDK + CLI + 本地看板：用通用 `Run/Target/Objective/Candidate/Evaluation`
模型驱动授权安全场景（v1 只接 TSecBench CTF 平台），通过官方 Python SDK 的常驻子进程 bridge
完成题目生命周期。

当前处于 **v0.2.0 → v0.3.0 重写的中途**（W0 + W1 已合并，W2/W3 未开始）。
仓库的注释、文档、提交消息**全部是中文**——新增代码请保持一致。

## 常用命令

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./... -count=1   # 集成门：必须全绿
go test ./dag/... -count=1                      # 单个包（实现 agent 只跑自己的包）
go test ./store/ -run TestLoadEventsSkipsTornLastLine -v   # 单个用例
go test -race ./... -count=1                    # 全量 race
UPDATE_GOLDEN=1 go test ./dag/                  # 重生成渲染契约 golden（diff 必须人工确认）
go test -tags integration ./executor/... -count=1   # 需 Docker + runner 镜像已构建
docker build -t red-harness-runner:v0.3.0 runner/   # runner 镜像（约 1.9 GB）
go run ./cmd/red-harness <doctor|list|run|resume|pause|cancel|serve|report>
echo '{"id":"1","cmd":"check_vpn"}' | PYTHONPATH=bridge/testdata python3 bridge/bridge.py   # bridge 手工冒烟，必须恰好回一行 JSON
```

`go test ./...` 里 `bridge` 约 24 s、`piai` 约 11 s——它们驱动**真实的** Python 子进程与 stub pi，
不是 mock。全量跑请留足超时。

## 架构

**根包 `harness` 是纯契约**：只有类型与接口，零实现、零内部依赖（`model.go`/`ports.go`/`events.go`/
`handle.go`/`engine.go`/`errors.go`/`solver.go`/`harness.go`）。这是并行开发的前提，**不要往根包加实现**。
`harness.New` 通过包级变量 `newEngine` 调用 `RegisterEngine` 注册进来的构造函数——引擎实现不在根包里。

编译期依赖方向（子包只依赖根包，`answer` 是叶子）：

```
harness（契约）
 ├── answer   叶子：Shape 推断 + Fingerprint（唯一真源）
 ├── dag      事实—意图图（导入 harness + answer）
 ├── gate     候选证据闸与指纹账本（导入 harness + answer）
 ├── store    FileStore：事件日志 + 原子快照 + private/ 账本
 ├── executor Docker 隔离执行器（argv 级隔离，不引 Docker SDK）
 ├── bridge   TSecBench Python 常驻子进程桥（JSONL/stdio）
 ├── piai     pi agent 适配（RPC 帧解析、看门狗）
 ├── internal/cli + cmd/red-harness
 └── engine/ scenario/ report/ web/   ← 尚未创建（example/ 是空目录）
```

**`engine/` 是整个仓库的中心缺口**：它不存在，所以 `harness.New` 永远返回「引擎实现未注册」，
`internal/cli/wire.go` 不存在所以 CLI 的每个子命令都走「未实现」分支（退出码 3）。
`piai` 的 `Round(ctx, prompt, emit)` 仍是 v0.2 签名，与 `harness.Agent` 不兼容且没有
`var _ harness.Agent = ...` 断言——**编译绿但契约未对齐**。

**事实层 / 答案层严格分离**（最容易破坏的不变式）：

- `dag` 的事实里**刻意没有 flag/answer 这类 FactKind**；答案只活在 `gate` 的账本里。
- 候选明文只允许出现在 `<StoreDir>/runs/<id>/private/`（0700/0600）与返回值 `OutcomeView.Flags`。
- 公开面（`run.json` / `events.jsonl` / `graph.json` / `report.*` / 看板 HTML / `DomainEvent` /
  `Snapshot`）**一律只有指纹**。`Snapshot` 故意没有 `Flags` 字段。
- 落盘前 `dag/store.go` 的 `scrub` 擦掉明文，`Raw` 字段是最容易漏的那个（有专门测试）。

**运行期单写者模型**（`engine/` 的设计契约，待实现）：一个 `runLoop` goroutine 是唯一改状态的
地方，全部输入走 channel 进 loop；agent 只通过 `EventSink` 推事件，满了阻塞 = 有意的背压。
状态变化**先追加带单调序号的领域事件（`events.jsonl`），再原子写快照（`run.json`）——顺序不可颠倒**。
恢复以快照的 `lastAppliedSeq` 为基线重放。

## 不可违反的硬规矩

这些都是前几轮真实事故换来的，注释里通常写明了原因：

- **`Reason*` 字符串是 API**：可以新增，**不得改变已有值**（`solver.go`）。三个回归测试钉死它们，
  原文归档在 `docs/superpowers/plans/fixtures/v0.2-*.go.txt`（含 `.txt` 后缀以免被编译）。
- **轮循环的分支顺序是契约**：`ctx.Err()` 判定必须先于 provider 护栏，否则一次零回合的墙钟超时
  会被记成「模型服务挂了」。
- **`answer.Fingerprint` 是指纹唯一真源**（格式 `fp:<hex8>/len=<rune数>/<首>…<尾>`）。
  `dag.FlagFingerprint` 与 `gate.Fingerprint` **只准转发**，不得再实现一遍——已经漂移过一次。
- **`answer.Shape` 不能加 json tag、不能改字段名**：它被无 tag 嵌进 `graph.json` 的 schema 1，
  改名会静默破坏所有现存图。真要改必须写显式的 `MarshalJSON`/`UnmarshalJSON`。
- **零第三方依赖**：`go.mod` 无 `require` 块。需要容器编排就用 `docker` CLI，不引 Docker SDK；
  不引 yaml/toml/测试框架。
- **`dag` 不做拓扑排序式调度、不做攻击路径规划**：图只做剪枝 / 推导链 / 分支 / 续跑四件事。
  这个「不做」是明令写进注释的，不要「顺手补全」。
- **`gate` 的「首现优先、只升不降、`locked` 不可解锁」模型**；`derived` 族**故意没有访问器**，
  不要为了完整性加一个。
- **`dag/testdata/render_golden.txt`** 是渲染契约的唯一防线。
- **`piai` 的传输层不要动**（`proc.go` 的 `frameQueue` 单写者、`frames.go` 的 LF-only 解析、
  watchdog 的「探活成功但状态没变不算卡死」判定）：54 个测试钉着，都是真管道上实测出来的。
- **凭据纪律**：`.env` 里有真实的 `OPENCODE_API_KEY`；`/tmp/tsec/TsecBench-main/.agent.env` 里
  有看起来有效的 `BENCHMARK_TOKEN`。**绝不读取其值、绝不写进日志/报告/事件/测试夹具/提交**。
  凭据只经子进程环境变量传递，**不进命令行**（`ps` 可见）。

## 并行实施的工作方式

任务分解与精确接口签名在 `docs/superpowers/plans/2026-09-20-red-harness-v0.3.0.md`（T0–T21，5 个波次）。
既定编排是**协调者 + worktree 隔离的实现 agent**：

- **一个子包 = 一个 agent**。Go 不允许同包并行编辑，分派前核对包级所有权；波次内文件集合必须不相交。
- 每个 agent **只跑自己的包测试**（`go test ./<pkg>/... -count=1`），全量在集成门由协调者跑。
- 冲突说明所有权核对漏了，**停下来查**，不要手工解冲突蒙过去。
- 提交粒度 = 任务，消息形如 `feat(engine): 单写者轮循环 [T11]`。
  **绝不 `git add -A`**——逐任务显式列出路径（`.env` 不能进任何提交）。
- worktree 在 `.claude/worktrees/`（已 gitignore），是手工 `git worktree add` 建的。

## 权威文档

| 文件 | 内容 |
|---|---|
| `PLAN.md` | v1 实施计划（需求真源，含「明确不纳入 v1」六项） |
| `docs/superpowers/specs/2026-09-20-*.md` | **已冻结的设计依据**，含 v0.2 缺陷清单（每条带 `file:line`） |
| `docs/superpowers/plans/2026-09-20-*.md` | 任务计划 T0–T21 + 波次编排 + 验收命令 |
| `docs/architecture.md` | 当前真实状态 + mermaid 架构图 + 断链处清单 |
| `docs/migration-v0.2-to-v0.3.md` | 逐字段迁移表与**行为变更**清单 |
| `SDK_API.md` | TSecBench 官方 Python SDK 接入文档 |
| `bridge/README.md` | wire 协议、错误码表、**6 个 SDK 源码级缺陷**与对策 |
| `runner/README.md` | runner 镜像构建与基础镜像 digest 记录 |

## 本机环境限制

无 VPN、宿主 py3.12 缺 `httpx`（**宿主导入 `tsec_benchmark` 必然失败，这是正常配置**——
SDK 装在容器层、面向 py3.14）；Docker 可用；2 核 3 GB。pi 真身在
`/root/.local/share/pi-node/node-v22.23.2-linux-x64/bin/pi`（不在 PATH，且必须把 bundled node
目录前置进 `PATH`——系统 node 是 v18，pi 的 shebang 是 `#!/usr/bin/env node`）。

因此：真实 TSecBench 冒烟**本机不可能**（用 mock SDK 离线验证），真实 pi 冒烟可行但会花 token
且凭据可能已失效——**失败要报失败，不得记为跳过**。真实 pi 冒烟里「0 回合 + 有错误」是
provider 静默烧钱模式，不是「跑完了」。
