# 进攻性安全 Harness SDK —— 设计（v0.3.0）

> 状态：已冻结。本文件是 v0.3.0 实现的**权威设计依据**。需求来源是仓库根目录的 `PLAN.md`（v1 实施计划）；任务分解见 `docs/superpowers/plans/2026-09-20-red-harness-v0.3.0.md`；从 v0.2 迁移的逐项对照见 `docs/migration-v0.2-to-v0.3.md`。

## 1. 目标与边界

构建面向安全 Agent 开发者的 Go SDK、CLI 与本地看板。v1 仅接入 TSecBench，通过官方 Python SDK bridge 完成题目生命周期；内核使用通用 `Run/Target/Objective/Candidate/Evaluation` 模型，为后续授权渗透、源码和云原生场景保留扩展点。

v1 为单机、单用户、题目串行执行。允许对 v0.2.0 做破坏性 API 整理，但复用现有已通过测试的 DAG、候选证据闸和 pi RPC 实现。

**明确不纳入 v1**（`PLAN.md:9`）：真实资产签名授权校验、逐动作策略拦截、人工审批流、多用户/RBAC、远程 worker、多 Agent 后端。

## 2. 为什么要有 v0.3.0：v0.2 的缺陷清单

以下每一条都经过代码核对（`file:line` 为 v0.2.0 基线提交 `f9718a3` 的位置），每条留一行「为什么这么改」。

### 2.1 架构性缺陷

| # | 位置 | 现状 | 为什么这么改 |
|---|---|---|---|
| A1 | `harness.go:520-528` | `emit` 在 agent 的 reader 协程上做四路同步扇出：`observe`(→`OnEvent` 任意用户代码) → `Observer.Observe` → `Gate.Observe` → `Ingest` 包装器。`OnEvent` 里跑任意用户代码，且 `Gate.Observe` 要取锁 | v0.3 改为**单写者 channel 消费**：agent 只把事件推进带缓冲的 channel，消费在唯一的 `runLoop` goroutine 上。扇出发生在 reader 协程意味着一个慢回调会反压 pi 的 stdout 管道；channel 满时阻塞是**有意的背压**，而不是在别人的协程里跑用户代码 |
| A2 | `harness.go:473-477` | 只有 `Saver.Save` 一个持久化点；`dag.Load`/`LoadOrNew`（`dag/store.go:200,270`）从未被根包调用 | 无法恢复。v0.3 引入事件日志 + 单调序号 + 原子快照，`Store` 成为唯一持久化出口 |
| A3 | `harness.go:111-179` | `Session` 的 20 个字段里有 7 个是**可选接线点**（`Gate`/`Scheduler`/`Renderer`/`Observer`/`Ingest`/`IntentSink`/`Saver`），漏接是**静默**的 | 收进接口后漏接变成编译错误。`wiring_test.go:85` 就是为一次真实事故写的：DAG 的事实回流漏接后图零事实、7 个阶段只有前 4 个可达，而所有包自测全绿——用户看到的不是报错，是「DAG 驱动」退化成写死的阶段链、白烧预算 |
| A4 | `harness.go:111-179` | 契约（`Challenge`/`Gate`/`Scheduler`）与实现（`Session.Run` 的 260 行轮循环）在同一个包里 | 并行化的前提。拆成纯契约后每个子包是一个 agent 的独占领地，波次内文件集合天然不相交 |

### 2.2 死代码与失效护栏（全部经代码核对确认）

| # | 位置 | 现状 | 为什么这么改 |
|---|---|---|---|
| B1 | `harness.go:376` + `harness.go:493` | `out.Stats` 全文件**唯一**赋值点在循环结束后（`harness.go:493`），所以传进 `Budget.Exhausted` 的 `used.MaxCostUSD` 在循环内**恒为 0** ⇒ `MaxCostUSD` 分支（`harness.go:41-43`）从轮循环不可达。成本预算实际是死代码 | 每轮实时上报成本。这是「预算护栏看起来在、实际不在」——比没有护栏更危险 |
| B2 | `harness.go:42` | 成本耗尽返回 `ReasonTimeout`（拷贝粘贴自墙钟分支 `harness.go:36`） | 新增 `ReasonMaxCost`。报告里「超时」与「花超了」是两件事 |
| B3 | `harness.go:33` | rounds 耗尽返回 `ReasonMaxTurns` | 新增 `ReasonMaxRounds`。**这是行为变更**：rounds 耗尽的原因字符串从 `max_turns` 变成 `max_rounds`，已在迁移表里显式记录 |
| B4 | `harness.go:65,67,85` | `Outcome.IntentDone`/`Negative`/`Report` 三个字段**全文件零赋值点** | 改为真实回填（`IntentDone`/`Negative` 来自 `dag.Graph.Stats()`，`Report` 来自报告路径）。死字段会让人以为能力存在 |
| B5 | `harness.go:186-187` vs `harness.go:499-517` | `HintAlways` 的文档说「每道题开始就请求一次」，但 `maybeHint` 只对 `HintAuto` 有 `out.HintUsed > 0` 守卫 ⇒ `HintAlways` **每轮都请求** | 实现与文档必须一致。`HintAlways` 也受「每题一次」守卫 |
| B6 | `harness.go:258` | `defer func() { _, _ = s.Platform.Close(ctx, code) }()` 丢弃结果与错误，且复用可能已取消的 ctx | cleanup 错误单独记录，不覆盖主要终止原因；用独立的 ctx |
| B7 | `harness.go:632-634` + `platform.go:65-77` | `Observer` 接口**无任何实现**；`Platform`/`HealthChecker` 无任何实现（`platform.go:64` 注释指向的 `platform` 包不存在） | 删 `Observer`（DAG 的事实抽取走 `Planner.ObserveEvent`，在同一条事件路径上，只有一个消费者）；`Platform` 由 bridge 实现 |
| B8 | `solver.go:44` | `DefaultFlagFormat` 从未被引用 | 删除。占位符由 `answerFormatHint` 按题面渲染 |
| B9 | `solver.go:50` | `ReasonStalled` 从未被写入 | **保留常量**（Reason 字符串是 API），README 标注「v1 未使用」 |
| B10 | `harness.go:26` | `DefaultBudget()` 零调用点 | 保留（是公开 API 的一部分，`Engine` 默认值用它） |
| B11 | `harness.go:94,102` | `Outcome.Duration()`/`Solved()` 零调用点 | `Duration()` 移到 `OutcomeView`；`Solved()` 语义被 `Objective.Completed` 取代，删除 |
| B12 | `platform.go:23,31` | `Challenge.Remaining()`/`Done()` 零调用点 | 保留（纯函数，`Objective` 会用到） |
| B13 | `gate/provenance.go:564` | `itoa` 零调用点 | 删除 |
| B14 | `piai/proc.go:365` | `proc.stderr()` 零调用点——`readStderr`（`proc.go:350`）把 pi 的 stderr 收进 `stderrTail` 却从不透出 | pi 的版本/凭据/extension 错误全在 stderr，不接出来这类故障无法诊断。接进启动失败与 `ErrProcessDied` 的错误消息 |
| B15 | `piai/proc.go:251` | `frameQueue.depth()` 零调用点 | 删除，或接进 watchdog 的观测事件 |
| B16 | `piai/frames.go:35,41,43,44` | `CmdFollowUp`/`CmdClearQ`/`CmdThinkLvl`/`CmdAutoRetry` 从未使用 | **保留常量**（pi 协议里的合法命令），但删掉从未赋值的 `Command.Mode`/`Command.Level` 字段 |
| B17 | `dag/node.go:205` | `Node.Expects` 无生产调用点 | 接进轮循环（agent 申报 kind 与预期不符时提示换类别），或删除——**二选一，不得留悬** |
| B18 | `gate.Ledger.SortedFingerprints` | 零调用点 | 接进报告，或删除 |

### 2.3 静默错误（最容易伤到真实用户的一类）

| # | 位置 | 现状 | 为什么这么改 |
|---|---|---|---|
| C1 | `dag/store.go:174` | `FlagFingerprint` 是 `answer.Fingerprint` 的**逐字节重复实现**（注释 `dag/store.go:161-173` 已写明待统一） | 指纹唯一真源是 `answer.Fingerprint`（`answer/fingerprint.go:88`）。两份实现意味着格式漂移无人发现——**已经漂移过一次**：`gate` 旧格式 `6326baf8:24:f}` 与 `dag` 的 `fp:…` 永不匹配 |
| C2 | `gate/provenance.go:110` | `stateFileReCI` 过宽：CI 变体让裸小写 `notes`/`memory`/`todolist`/`_blackboard*`/`tried_commands*`/`_transcripts` 也大小写不敏感匹配 ⇒ `curl -s http://t/flag.txt`、`curl -s http://10.0.0.1/notes`、`grep -r memory /etc` 全被误判成 `self_readback` ⇒ `locked=true` ⇒ **永久不可提交** | 这正是 `provenance.go:98-104` 自己记录的「29 条被拒里 19 条实为正确答案」那一类。收窄方式：CI 变体只保留 `flag(?:\.(?:txt\|md\|json\|log))`，其余名字回到大小写敏感 |
| C3 | `dag/store.go:32` | `answer.Shape` 被无 json tag 嵌入 schema 1 的 JSON（`answer/shape.go:28-38` 的字段也没有 tag）⇒ 持久化键是 Go 字段名原样 | **不要给 `Shape` 加 json tag 或改字段名**——那会静默破坏所有现存 `graph.json`。若确需改名，必须同时写 `MarshalJSON`/`UnmarshalJSON` 显式保持旧拼写 |
| C4 | `piai/agent_test.go:800-810` | 记录了根包 `solver.go` 注释与实现的语义不符（`Err` 是轮级而非轮级累积） | **以实现为准（轮级）**，修正 `solver.go` 的注释。`Err` 与 `ProviderError` 刻意分开：`Err` 是「本轮任何异常」的混合字段，`ProviderError` 是干净的 provider 判据——第 1 轮的 401 不得污染第 3 轮 |

### 2.4 必须保留的既有契约

这些是前几轮用真实事故换来的，**不得删除，也不得把被否决的机制加回来**：

- **轮循环的分支顺序是契约**（`harness.go:427 → 445 → 450 → 458 → 463`）：`ctx.Err()` 判定必须先于 provider 护栏，否则一次零回合的墙钟超时会变成「模型服务挂了」。
- **`Reason*` 字符串是 API**（`solver.go:47-61`）。三个回归测试钉死了它们。可以新增，不得改变已有值。
- **`dag/node.go:14` 与 `dag/schedule.go:11-16`**：明令不要加拓扑排序式调度或攻击路径规划。图只做剪枝 / 推导链 / 分支 / 续跑四件事。
- **`dag/schedule.go:284-294`**：「为什么不把平台判错的答案转成 negative 事实」。
- **`dag/store.go` 的 `scrub`**：落盘前擦掉答案明文（`Content`/`Raw`/`Goal`/`Evidence`/`Rejection.Content`）。`Raw` 是最容易漏的那个，有专门测试 `TestRawScrubbedOnSave`。
- **`gate` 的「首现优先、只升不降、`locked` 不可解锁」模型**。`derived` 族**故意没有访问器**（`gate.go:627-631`）——不要为了「完整性」加一个。
- **`dag.Render` 的 golden 文件** `dag/testdata/render_golden.txt` 是渲染契约的唯一防线。
- **`piai` 的传输层**（`proc.go` 的 `frameQueue` 单写者、`frames.go` 的 `FrameReader` LF-only 解析、watchdog 的「探活成功但状态没变不算卡死」判定）。54 个测试钉着它们，且都是真管道上实测出来的。

## 3. 目标架构

### 3.1 分层

```
harness（根包）—— 纯契约：类型 + 接口，零实现、零内部依赖
   ↑ 所有子包只依赖它
   ├── engine/      单写者内核 + 恢复 + 控制面（只依赖根包 + store/）
   ├── store/       文件存储与私密账本（不导入 dag）
   ├── executor/    Docker 隔离执行器（runner/ 是它的镜像）
   ├── bridge/      TSecBench Python bridge
   ├── scenario/    场景适配（TSecBench + fake）
   ├── report/      报告生成（导入 answer，叶子）
   ├── web/         本地看板
   ├── piai/        pi agent 适配
   ├── dag/         事实—意图图（导入 answer）
   ├── gate/        候选证据闸（导入 answer）
   └── answer/      答案形状与指纹（叶子，不导入任何内部包）
```

**根包为什么必须是纯契约：** 这是并行化的前提。v0.2 的根包把契约和 260 行轮循环混在一起，任何 agent 碰轮循环就会与所有引用根包类型的人冲突。拆开后每个子包是一个 agent 的独占领地。

**`answer` 是叶子：** `dag` 与 `gate` 都依赖它（`answer.Shape`/`answer.Infer`/`answer.Fingerprint`），`piai` 完全不用它。这是允许的——契约约束的是「子包不互相依赖」，不是「只能依赖根包」。

### 3.2 引擎是唯一状态写入者

一个 `runLoop` goroutine 是**唯一**改状态的地方。全部输入（agent 事件、平台结果、控制命令）走 channel 进 loop。状态变化先追加带单调序号的领域事件（`events.jsonl`），再原子保存快照（`run.json`）。恢复以快照的 `lastAppliedSeq` 为基线重放事件。

`EventSink` 的实现把事件写进 loop 的 `events chan Event`（带缓冲，满了阻塞——这是有意的背压）。

### 3.3 事实层与答案层严格分离

- 候选明文只进 `private/`（`0700`/`0600`），保存原始证据和候选明文账本。
- 公开文件（`run.json`/`events.jsonl`/`graph.json`/`report.*`/看板 HTML）只保存指纹、哈希和脱敏片段。
- 事实抽取用**指纹匹配**，不落原文。
- 领域事件 `EvCandidateSeen`/`EvSubmitResult` **只含指纹**。
- `Snapshot` **没有** `Flags` 字段——明文只在 `private/` 与 `OutcomeView`（返回值）里。

## 4. 公共接口

接口的完整 Go 声明见根包的 `model.go`/`ports.go`/`events.go`/`handle.go`/`engine.go`/`errors.go`。设计要点：

- **`Engine.Start(ctx, RunSpec)` / `Engine.Resume(ctx, RunID)`** 取代 `Session.Run`；返回 `RunHandle`，提供 `Snapshot`/`Events`/`Pause`/`Resume`/`Cancel`/`Wait`。
- **`RunSpec`** 固定包含场景、目标选择、Agent、Docker、预算、提示策略、存储和运行级策略配置；创建后计算摘要（`Digest()`），恢复时拒绝配置漂移。
- **可替换端口**：`AgentFactory`、`Executor`、`Planner`、`Renderer`、`CandidateGate`、`RunPolicy`、`Store`、`EvidenceStore`、`Platform`、`Scenario`。v1 仅提供 pi Agent、Docker Executor 和文件 Store。
- **统一错误**携带 `Kind/Op/Retryable/RunID/Cause`，区分配置、范围、平台、provider、执行器、预算、持久化和取消。
- **`Planner` 的方法名是 `ObserveEvent` 而不是 `Ingest`**：`dag.Scheduler` 已有一个 `Ingest(ev, round) IngestResult`（`dag/schedule.go:131`），Go 不允许同名的两个方法只因返回值不同而共存。改 `dag` 的签名会打断读返回值的既有测试，所以接口用新名字，`dag` 侧加一个薄包装。
- **`Options.Gate`/`Planner`/`Renderer` 是工厂而不是实例**：一道 run 可能跑多道题，而它们是**每题独立**的（v0.2 里 `dag.NewScheduler(g)` 与 `gate.NewGate(desc)` 就是每题新建）。
- **默认实现放在装配层**（`internal/cli/wire.go`）：`engine/` 若导入 `dag`/`gate`，就与「每个子包独立并行开发」的编排直接冲突。`engine/` 自己只依赖根包契约 + `store/`，测试用注入的 fake。

## 5. 恢复语义

- 以快照 `lastAppliedSeq` 为基线重放 `events.jsonl`。
- **fail closed 三种情形**：
  - `RunSpec.Digest()` 与 `Snapshot.SpecDigest` 不一致 → `KindConfig` 并**指出漂移字段**。
  - `private/` 账本缺失 → `KindPersistence`。
  - `SchemaVersion` 不支持 → `KindConfig`。
- **对终态 run 调 `Resume`：** 返回 `KindConfig` 错误（`RunCompleted`/`RunFailed`/`RunCancelled` 一律拒绝），**不重跑**。
- **暂停：** abort 当前 round，把 intent 标记为 `interrupted` 并落盘（`EvRunPaused`）；恢复后按已知事实重新调度，**不假定中断动作成功**。
- **恢复后先 `Scenario.Reconcile` 对账**再决定是否重试平台写操作——禁止盲目重发 `start`/`submit`/`close`。`Reconcile` 返回的平台进度以平台为准，覆盖本地推测。

**为什么这五条是设计重点**（每一类输入都最容易伤到真实用户，且必须由测试钉死）：

1. 恢复一个已经结束的 run —— 合理预期是明确拒绝，而不是重新跑一轮、重复提交 flag、重复消耗预算。
2. `private/` 账本缺失或被截断时的恢复 —— 合理预期是 fail closed，而不是「账本为空 ⇒ 没有候选被提交过 ⇒ 把全部候选重提一遍」。
3. 平台写操作超时但实际已生效 —— 合理预期是先对账再决定重试，而不是盲目重发（浪费提交额度，或把已确认的 flag 记成判错）。
4. `events.jsonl` 末尾有半行（进程被杀）—— 合理预期是跳过残缺末行并从最后一个完整事件继续，而不是整份日志解析失败、run 永久无法恢复。
5. `Resume` 时 `RunSpec` 与快照摘要不一致 —— 合理预期是拒绝并指出哪个字段漂移了，而不是静默用新配置继续。

## 6. 存储布局

```
<StoreDir>/runs/<runID>/
├── run.json       0600  快照
├── graph.json     0600  DAG（schema 1，由 dag/store.go 读写）
├── events.jsonl   0600  领域事件，每行一个 DomainEvent
├── private/       0700
│   ├── candidates.jsonl  0600  候选明文账本
│   └── evidence/         0600  原始工具输出
├── report.json    0644
└── report.md      0644
```

`Append` **先写 `events.jsonl`**（`O_APPEND` + 一次 `write` 系统调用写完整行，保证不撕裂），**再原子写 `run.json`**。顺序不可颠倒——先快照后事件会在崩溃时产生「快照指向不存在的事件」。

原子写：`writeTmp → fsync(file) → rename → fsync(dir)`。

## 7. 从 v0.2 继承的事故教训

这些是前身（`/root/.claude/plans/purrfect-scribbling-bentley.md` §十二）用真实事故换来的，v0.3 逐条有落地位置：

| 严重度 | 事故 | v0.3 的落地 |
|---|---|---|
| 致命 | LLM 一条命令写满 **307 GB** 磁盘（pi 的 bash 工具 timeout 可选、无默认） | 契约层预留 `AgentSpec.Extensions` 装载位；extension 本体与真 pi 验证**不在本计划范围内**。Docker 隔离的只读 rootfs + tmpfs 是唯一挡住「写满磁盘」的机制 |
| 致命 | pi 0.74.2 **静默烧题库**：provider 400 → 0 回合 + `stopReason=error` + 无错误文本，跑掉 280 run / 0 flag / 63 题 | 两条 provider 护栏 + 启动即校凭据（401 会以**静默空会话**形式出现，所以缺凭据必须在启动前拒） |
| 高 | 子 agent 的 `exitCode: 0` 初值让运行中进程被算成已完成 | `RoundResult` 的 `Turns`/`Reason` 初值必须表达「未完成」，不能是「已完成」 |
| 高 | `stop_check` 是死代码，「通关立即终止」从未被调用 | 轮首通关判定 + `TestRegression_SolvedStopsWithoutFlagCount` |
| 高 | pi 进程**不自己退出** | `Close` 显式 `killpg` |
| 中 | flag 明文泄漏进 `MEMORY.md` / `_blackboard.json` | `dag/store.go` 的 `scrub` + `private/` 分离 |
| 中 | RPC 信封：`type` 字段是必需的，缺了报 `Unknown command: undefined` | 不得改 `frames.go` 的出站编码 |
| 中 | **每题独立 `HOME`**：pi 从 `$HOME/.pi/agent/*` 发现扩展 / 角色 / 技能，共享 HOME 跨题污染 | `AgentSpec.HomeDir` + `childEnv` |
| 中 | pi 版本漂移（前身被 0.74.2 咬过） | `DefaultVersionRange` 版本闸（`{Min:"0.85.0", Max:"0.86.99"}`，闭区间） |
| 低 | `turns` 靠数 `tool_execution_start` 是猜 | 保持同语义（护栏依赖），用 `get_session_stats().toolCalls` 交叉校验 |

### 已实测验证的事实（不要重新推导）

`new_session` 语义成立（`sessionId` 变、`messageCount` 归 0、历史确实清空）；`-e <绝对路径>` + `--approve` 能加载 extension；`details` 262 KB 不截断；长思考期 `get_state` 往返最坏 414 ms；`--append-system-prompt` 与 `AGENTS.md` 都进 system prompt 且**分节可见**（`sections.addendum` / `sections.project_context`）。证据在 `/tmp/m0/s*.json`。

### 前身资产的位置

| 资产 | 位置 | 用途 |
|---|---|---|
| 事实抽取正则 + B14 凭证质量闸 | `/tmp/base_72ae35f/adapter/blackboard.py` | `gate` 的宿主侧抽取通道；B14 噪音语料已是回归 fixture |
| 幻觉族 / 推导族分账 | `/tmp/base_72ae35f/packages/worker/ghost_worker/hallucination.py` | gate 来源闸的分类依据 |
| 作业教条（`_INTRANET_ORCHESTRATION` / `_CLAUDE_MD`） | `/tmp/base_72ae35f/adapter/taskprompt.py` | 写进 workdir 的 `AGENTS.md` |
| bash 三约束 | `/tmp/base_72ae35f/packages/worker/ghost_worker/pi_ext/bash_guard.js` | `bash_guard.ts` 的蓝本（**本计划范围外**） |
| 前身平台配置语义（`ADAPTER_*` / `SOLVER_*`） | `/tmp/tsec/TsecBench-main/.env` | CLI flag 命名与默认值参考（`ADAPTER_MAX_CONCURRENCY=3`、`ADAPTER_ROUND_TIMEBOXES=480,820,1500,2000`、`ADAPTER_PER_CHALLENGE_SECONDS=4000`、`ADAPTER_USE_HINTS=1`） |
| 真实 `tsec_benchmark` SDK 源码（v0.1.2） | `/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/204/fs/usr/local/lib/python3.14/dist-packages/tsec_benchmark/` | bridge 的契约依据。注意它在容器层、面向 py3.14 |
| 平台服务端实现 | `/tmp/tsec/TsecBench-main/tsecbench/api.py:112-162` | 与 SDK 对账（已核对：路由、鉴权头、响应模型逐字段一致） |
| ⚠️ 敏感文件 | `/tmp/tsec/TsecBench-main/.agent.env` | **含看起来有效的 `BENCHMARK_TOKEN` 与 API key。当成敏感文件对待，不要读值、不要提交、不要写进任何日志或报告。** |

## 8. 明确不复活

以下机制在前身被讨论过或被否决，**不要因为看到旧文档就把它们加回来**：

- 前身那份**从未实现**的 MITRE ATT&CK 攻击路径规划层（`/tmp/base_72ae35f/.claude/plans/autonomous-redteam-architecture.md` 的 `AttackPath`/`AttackStage`）。图只做剪枝 / 推导链 / 分支 / 续跑四件事。
- Heimdall 式 LLM 旁路观察者（默认关闭是有原因的）。
- monkeypatch `node_modules`。
- legacy/assignment 双模式驱动。
- 固定角色分工的多 agent（用户已否决）。

## 9. 测试与验收

- 单元测试覆盖状态转换、预算边界、DAG 调度、候选 provenance、脱敏、事件序号、错误分类和配置摘要。
- 契约测试覆盖所有可替换端口，以及 Python bridge 的字段映射、请求关联、超时、崩溃重启和平台错误码。
- Docker 集成测试验证只读文件系统、资源限制、目标端点可达、非授权端点不可达、provider 代理可用及容器回收（`-tags integration`）。
- 端到端测试覆盖 `list→start→solve→submit→close`、重复候选、错误提交、自动提示、暂停恢复、进程崩溃恢复和最终 cleanup。
- 看板测试覆盖 SSE 重连、控制鉴权、已结束运行的只读展示和报告脱敏。
- 必须通过 `go test ./...`、`go test -race ./...`、`go vet ./...`、`gofmt -l .`。
- 真实 TSecBench 冒烟测试仅在显式提供 token/base URL 且 VPN 连通时启用。

## 10. 假设与默认值

- Linux + Docker 是 v1 唯一受支持运行环境；挑战默认串行。
- 默认预算：40 rounds、30 分钟、600 tool turns；模型成本默认不限但可配置。
- 文件存储是唯一 v1 后端；不实现数据库、遥测上传和自动证据清理。
- 旧 DAG schema 继续可读，但缺少新 `run.json` 的历史目录不能直接恢复为 Engine run。
