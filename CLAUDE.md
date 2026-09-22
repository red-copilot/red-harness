# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 这是什么

面向安全 Agent 开发者的 Go SDK + CLI：用通用 `Run/Target/Objective/Candidate/Evaluation`
模型驱动**明确授权**的 CTF / 靶场场景（当前只接 TSecBench），通过官方 Python SDK 的常驻
子进程 bridge 完成题目生命周期。（v0.3 设想过的本地看板 / Web 已明确不做。）

当前处于 **v0.4.0-research**（研究版：单 Agent、同步 `Harness.Run`、每题一个 Docker sandbox）。
源码里**并存两代 API**：v0.3 的 `Engine`/`RunHandle`/事件快照仍在，供 v0.3 读图与测试；
CLI 与生产路径已全部切到 v0.4。权威现状见 `docs/architecture.md`。
仓库的注释、文档、提交消息**全部是中文**——新增代码请保持一致。

## 常用命令

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./... -count=1   # 集成门：必须全绿
go test ./dag/... -count=1                      # 单个包（实现 agent 只跑自己的包）
go test ./store/ -run TestLoadEventsSkipsTornLastLine -v   # 单个用例
go test -race ./... -count=1                    # 全量 race
UPDATE_GOLDEN=1 go test ./dag/                  # 一次重写两份 golden（render + mermaid）；diff 必须人工确认只动了预期那份
go test -tags integration ./executor/... -count=1   # 隔离用例 25 条（13 条走 v0.4 SandboxSession 生产路径）
go test -tags integration ./cmd/red-harness/... -count=1   # 新同步入口的纵向闭环（M1）
docker build -t red-harness-runner:v0.3.0 runner/   # runner 镜像（约 1.9 GB，内含真 pi）
go run ./cmd/red-harness doctor                     # 体检；不给 --provider 时 provider_credentials 必然 FAIL
go run ./cmd/red-harness run --scenario fake        # 离线纵向闭环：内置演示题，不碰网络与平台（仍需 Docker + runner 镜像）
echo '{"id":"1","cmd":"check_vpn"}' | PYTHONPATH=bridge/testdata python3 bridge/bridge.py   # bridge 手工冒烟，必须恰好回一行 JSON
```

**CLI 退出码是 API**（`internal/cli/cli.go`）：`0` 成功 / `1` 失败 / `2` 用法错 /
`3`＝**跑完了但有题没解出来**。3 与 1 必须分开——「模型没解出来」是研究结论，
「跑的过程中坏了」要查日志，合成一个码会让两者在 CI 里完全同形。

两条 `integration` 门的前置是 **root + Docker + `red-harness-runner:v0.3.0` 在位**，缺任何一项
测试走 `t.Skip` 而不是 fail——**「全绿」可能是一条都没真跑**，跑完要核对 PASS/SKIP 计数。
`executor/session_integration_test.go` 会现场用 `runner/Dockerfile.teststub` 构建
`red-harness-runner:teststub`（静态编译的 stub pi），所以隔离门不需要 provider 凭据。
跳过的集成测试**不算通过**（`docs/roadmap.md` 验收与证据）。

`go test ./...` 里 `bridge` 约 24 s、`piai` 约 11 s——它们驱动**真实的** Python 子进程与 stub pi，
不是 mock。全量跑请留足超时。

## 架构

**根包 `harness` 零内部依赖**，但**不再是「零实现」**：`v04.go` 里有 v0.4 的 `Harness` 门面
（`Run`/`Doctor`/`runChallenge`/`reclaimStale`），只依赖标准库。契约在
`model.go`/`ports.go`/`events.go`/`handle.go`/`engine.go`/`errors.go`/`solver.go`/`harness.go`。
**根包依然不许 import 任何子包**——那是并行开发的前提。

编译期依赖方向（子包只依赖根包，`answer` 是叶子）：

```
harness（契约 + v0.4 Harness 门面，零内部依赖）
 ├── answer   叶子：Shape 推断 + Fingerprint（唯一真源）
 ├── dag      事实—意图图（导入 harness + answer）
 ├── gate     候选证据闸与指纹账本（导入 harness + answer）
 ├── store    FileStore（事件/快照/账本）+ ResultFileStore（公开指标）
 ├── executor Docker 隔离执行器（argv 级隔离，不引 Docker SDK）
 ├── bridge   TSecBench Python 常驻子进程桥（JSONL/stdio）
 ├── piai     pi agent 适配（RPC 帧解析、看门狗）
 ├── scenario 场景适配：Fake（离线）/ TSecBench（真实平台）
 ├── internal/cli + internal/wire + cmd/red-harness
 └── report/ web/   ← 未创建，且 v0.4 明确不做（example/ 是空目录）
```

**`internal/wire` 是唯一的装配层**，也是唯一同时 import 全部 7 个实现包的模块。
实现包之间**两两互不 import**（`executor` 不认识 `scenario`，`scenario` 不认识 `dag`），
所以「谁把 X 交给 Y」只在这里发生。CLI **不** import 实现包：它只依赖包内定义的窄接口
（`internal/cli.Ports`），由 `cmd/red-harness` 在 `init()` 里用 `cli.SetWire` 接进来。

⚠️ **三条把系统拼起来的边在 import 图里看不见**，任何 grep-import 的依赖图都会把
`internal/cli` / `scenario` / `piai` 误判成孤立：

| 边 | 机制 |
|---|---|
| `internal/cli → internal/wire` | 包级 `WireFunc` 变量 + `cli.SetWire`（`cmd/red-harness/main.go`） |
| `scenario → bridge` | `Platform` 窄接口，由 wire 注入 `*bridge.Client` |
| `piai → executor` | `AgentFactory.New` 收 `harness.SandboxSession`，piai 不 import executor |

`engine.go` 里的 `Engine`/`harness.New`/`RegisterEngine`/`newEngine` 是 **v0.3 遗留**：
v0.4 的 `NewHarness`（`v04.go:309`）直接构造 `*Harness`，**不经过**注册表，所以
`RegisterEngine` 全仓只有 `contract_test.go` 在调。旧 API 留着是为了 v0.3 读图与测试，
**不要**再往那条路上加东西（完整清单见 `docs/v0.4-open-items.md`）。

**事实层 / 答案层严格分离**（最容易破坏的不变式）：

- `dag` 的事实里**刻意没有 flag/answer 这类 FactKind**；答案只活在 `gate` 的账本里。
- 候选明文只允许出现在**私密面（`private/`，0700/0600）**与返回值 `OutcomeView.Flags`。
  ⚠️ v0.4 私密面的实际落点是 `<ResultDir>/private/<runID>/<题目哈希>.jsonl`（`AppendTrace`），
  **不是** v0.3 的候选账本 `<StoreDir>/runs/<id>/private/candidates.jsonl`——后者当前没有
  生产写入方（见 `docs/v0.4-open-items.md`）。纪律不变，机制变了。
- 公开面（`results/<runID>.json` / 看板 HTML / `DomainEvent` / `Snapshot` / `run.json` /
  `graph.json`）**一律只有计数与指纹**。`Snapshot` 故意没有 `Flags` 字段；`ResultFileStore`
  用专门的公开结构序列化，从不 marshal `OutcomeView`/`Candidate`/`Flags`。
- 走 v0.3 图路径时，落盘前 `dag/store.go` 的 `scrub` 擦掉明文，`Raw` 字段是最容易漏的那个（有专门测试）。

**v0.4 的运行模型是「同步编排 + 单一消费者」**，不是 v0.3 设想的 `runLoop` goroutine：
一次 `Harness.Run` 串行处理题目，每题一个 sandbox、一个 pi 会话。agent 的 reader 只向**有界**
事件队列投递（队列长度 / 单轮条数 / 单条体积三重上限），编排 goroutine 是唯一消费者，
轮末用 `Flush` 做屏障。队列满了阻塞 reader 是**有意**的背压（`piai/proc.go` 的 `frameQueue` 单写者）。

⚠️ **落盘面在 v0.4 变小了，别照着 v0.3 的图去读**：v0.4 明确不做崩溃续跑与事件重放，
`HarnessOptions` 里**没有** `Store`/`GraphStore`（事件日志与快照那两个端口），装配层只建
`ResultFileStore`，外加一个**可选**的 `Graphs`（`GraphSaver`，nil＝不落盘；它不是 Fatal，
但 Doctor 会报出 `graph_saver` 一行）。有生产写入方的落点只有四个：

| 落点 | 写者 |
|---|---|
| `<ResultDir>/results/<runID>.json` 公开指标 | `store/results.go` |
| `<ResultDir>/private/<runID>/<题目哈希>.jsonl` 原始 trace | `store/trace.go`（`AppendTrace`） |
| `<StoreDir>/runs/<runID>/graph.json` + `graph.mmd` 每题终态 DAG | `internal/wire` 的 `dagGraphSaver`（生产装配恒提供） |
| `<StoreDir>/run.lock` 跨进程单运行锁 | `internal/wire/lock.go` |

`events.jsonl` / `run.json` / `private/candidates.jsonl` / `private/evidence/` / `report.*`
在 v0.4 **仍然没有生产写入方**，只剩测试与 v0.3 兼容路径在用。**「先写事件、再原子写快照」
仍然是 `store.Append` 的硬契约**（由注入快照写失败来测），只是当前没有调用方。
完整清单见 `docs/v0.4-open-items.md`——⚠️ 该文写于 `graph.json` 落盘落地之前，
它表里的「`graph.json` 无写入方」已过期，读的时候以本表为准。

## 不可违反的硬规矩

这些都是前几轮真实事故换来的，注释里通常写明了原因：

- **`Reason*` 字符串是 API**：可以新增，**不得改变已有值**（`solver.go`）。三个回归测试钉死它们，
  原文归档在 `docs/superpowers/plans/fixtures/v0.2-*.go.txt`（含 `.txt` 后缀以免被编译）。
- **轮循环的分支顺序是契约**：`ctx.Err()` 判定必须先于预算与 provider 护栏，否则一次零回合的
  墙钟超时会被记成「模型服务挂了」。v0.4 实现在 `v04.go:702`。
- **`answer.Fingerprint` 是指纹唯一真源**（格式 `fp:<hex8>/len=<rune数>/<首>…<尾>`）。
  `dag.FlagFingerprint` 与 `gate.Fingerprint` **只准转发**，不得再实现一遍——已经漂移过一次。
- **`answer.Shape` 不能加 json tag、不能改字段名**：它被无 tag 嵌进图 schema 1（`graph.json` 的
  载荷格式），改名会静默破坏所有现存图。真要改必须写显式的 `MarshalJSON`/`UnmarshalJSON`。
  ⚠️ v0.4 **已经在写** `graph.json`（见「落盘面」那张表），所以这条契约在生产路径上是活的，
  `dag/testdata/render_golden.txt` 也一样。
- **零第三方依赖**：`go.mod` 无 `require` 块。需要容器编排就用 `docker` CLI，不引 Docker SDK；
  不引 yaml/toml/测试框架。
- **`dag` 不做拓扑排序式调度、不做攻击路径规划**：图只做剪枝 / 推导链 / 分支 / 续跑四件事。
  这个「不做」是明令写进注释的，不要「顺手补全」。
- **`gate` 的「首现优先、只升不降、`locked` 不可解锁」模型**；`derived` 族**故意没有访问器**，
  不要为了完整性加一个。
- **`dag/testdata/` 下两份 golden**（`render_golden.txt` 渲染、`mermaid_golden.txt` 人可读导出）
  是渲染契约的防线：图与 mermaid 都是拼字符串拼出来的，而本仓库没有 mermaid 解析器
  （零第三方依赖），golden 是唯一能挡住「样式/类名改一个字、没有任何测试变红」的东西。
- **`piai` 的传输层不要动**（`proc.go` 的 `frameQueue` 单写者、`frames.go` 的 LF-only 解析、
  watchdog 的「探活成功但状态没变不算卡死」判定）：54 个测试钉着，都是真管道上实测出来的。
- **凭据纪律**：`.env` 里有真实的 `OPENCODE_API_KEY`；`/tmp/tsec/TsecBench-main/.agent.env` 里
  有看起来有效的 `BENCHMARK_TOKEN`。**绝不读取其值、绝不写进日志/报告/事件/测试夹具/提交**。
  凭据只经子进程环境变量传递，**不进命令行**（`ps` 可见）。

## 并行实施的工作方式

既定编排是**协调者 + worktree 隔离的实现 agent**（v0.3 的 T0–T21 波次计划见
`docs/superpowers/plans/2026-09-20-red-harness-v0.3.0.md`，属**历史**；v0.4 的当前收尾项在
`docs/roadmap.md` 与 `docs/v0.4-open-items.md`）：

- **一个子包 = 一个 agent**。Go 不允许同包并行编辑，分派前核对包级所有权；波次内文件集合必须不相交。
- 每个 agent **只跑自己的包测试**（`go test ./<pkg>/... -count=1`），全量在集成门由协调者跑。
- 冲突说明所有权核对漏了，**停下来查**，不要手工解冲突蒙过去。
- 提交粒度 = 任务，消息形如 `fix(gate): 读自己的状态文件不得给候选坐实族别 [M4]`
  （`[Mx]` 是里程碑门编号，见 `docs/roadmap.md`）。
  **绝不 `git add -A`**——逐任务显式列出路径（`.env` 不能进任何提交）。
- worktree 在 `.claude/worktrees/`（已 gitignore），是手工 `git worktree add` 建的。

## 权威文档

| 文件 | 内容 |
|---|---|
| `docs/architecture.md` | **当前实现的权威描述**（v0.4.0-research）+ mermaid 架构图 + 信任边界表 + 未完成项 |
| `docs/PLAN v0.4.md` | v0.4 目标行为（研究版定位与破坏性变更） |
| `docs/roadmap.md` | 按出口门排序的交付路线与**发布边界**（M1/M2 已通过、M3 离线完成、M4 进行中——**状态以它的出口门表为准**） |
| `docs/offensive-harness-sdk-roadmap.md` | **下一阶段目标架构与路线**（v0.4 收尾之后的产品定位、接口演进、信任边界） |
| `docs/sdk-architecture-v0.4.md` | v0.4 目标架构规范（模块依赖、时序、公开接口、信任边界） |
| `docs/v0.4-open-items.md` | **未接线代码面清单**：哪些 v0.3 机制在 v0.4 没有生产调用方 |
| `PLAN.md` | v1 实施计划（需求真源，含「明确不纳入 v1」六项；**v0.4 已改掉其中若干**） |
| `docs/superpowers/specs/2026-09-20-*.md` | 已冻结的设计依据，含 v0.2 缺陷清单（每条带 `file:line`） |
| `docs/superpowers/plans/2026-09-20-*.md` | v0.3 的任务计划 T0–T21 + 波次编排 + 验收命令（历史） |
| `docs/migration-v0.2-to-v0.3.md` | 逐字段迁移表与**行为变更**清单 |
| `SDK_API.md` | TSecBench 官方 Python SDK 接入文档 |
| `bridge/README.md` | wire 协议、错误码表、**6 个 SDK 源码级缺陷**与对策 |
| `runner/README.md` | runner 镜像构建与基础镜像 digest 记录 |
| `analysis/red-harness/` | 依赖与拓扑图（`TOPOLOGY.html` + `extract_topology.py`，可重跑） |

## 本机环境限制

宿主 py3.12 缺 `httpx`（**宿主导入 `tsec_benchmark` 必然失败，这是正常配置**——
SDK 装在容器层、面向 py3.14），所以平台调用的**宿主路径永远走不通**，真跑只能走
`docker exec <容器> python3 -m bridge`。Docker 可用（实测 29.8）；2 核 3 GB；VPN 接口
`tun0` 当前是 UP。pi 真身在 `/root/.local/share/pi-node/node-v22.23.2-linux-x64/bin/pi`
（不在 PATH，且必须把 bundled node 目录前置进 `PATH`——系统 node 是 v18，pi 的 shebang
是 `#!/usr/bin/env node`）。

因此：**平台侧真跑是可能的**（2026-09-22 首次打通，63 题、58 次真实工具调用，见
`docs/architecture.md` §5），但受两个前提约束——`tun0` 在、且 `BENCHMARK_TOKEN` 有效。
凭据可能已失效：**失败要报失败，不得记为跳过**。真实 pi 冒烟会花 token，且
「0 回合 + 有错误」是 provider 静默烧钱模式，不是「跑完了」。
