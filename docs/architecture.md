# red-harness SDK 架构（v0.4.0-research + v0.5 契约整理）

> 更新：2026-09-22。本文按当前工作区代码描述实现状态；目标行为见 [PLAN v0.4](PLAN%20v0.4.md)，收尾出口门见 [roadmap.md](roadmap.md)。
> 下一阶段的目标架构、接口演进和发布路线见 [offensive-harness-sdk-roadmap.md](offensive-harness-sdk-roadmap.md)。
> 新架构提案见 [offensive-harness-sdk-architecture-next.md](offensive-harness-sdk-architecture-next.md)：公开装配、运行内核、提交审计与实验复现的具体设计；其中计划能力尚未实现。
> **旧 Engine、RunHandle、事件快照与 v0.3 的三个存储端口已移出根包到 `legacy/`**（R1 落地）；
> CLI 已切到 v0.4 同步入口。⚠️ **R0（v0.4 发布门）仍未通过**——`legacy/` 的移出是
> 契约整理，不是发布信号。

## 1. 定位与边界

v0.4 是面向**明确授权的 CTF、TSecBench 和本地靶场**的单机研究 SDK。一次同步 Harness.Run 串行处理题目；每题一个 Docker sandbox、一个 pi Agent 会话。宿主负责平台 API、目标范围、调度、候选判定和结果；Agent 与工具都在 sandbox 内运行。当前不支持一般真实资产、多用户、远程 worker、并发 Run 或崩溃续跑。

~~~mermaid
flowchart LR
    Caller["SDK 调用者 / CLI"] --> H["Harness.Run<br/>同步编排"]
    H --> S["Scenario<br/>Fake / TSecBench"]
    S --> B["bridge<br/>宿主平台凭据"]
    H --> D["DAG Planner + Renderer"]
    H --> G["Candidate Gate"]
    H --> R["ResultStore<br/>公开指标"]
    H --> X["Sandbox.NewSession"]
    X --> P["pi 主进程<br/>Docker 容器"]
    P --> T["工具<br/>同一容器"]
    P -->|"规范化事件"| H
    X -->|"目标 IP:port 白名单"| Target["授权靶场"]
    X -->|"provider 代理"| Model["模型服务"]
~~~

### 信任边界

| 边界 | 责任 | 不允许流过的内容 |
|---|---|---|
| 宿主控制面 | 解析授权目标、调用平台、维持预算、提交候选、保存指标 | Agent 不持有平台 token，不直接调用平台写接口 |
| Sandbox | 非 root、只读 rootfs、有界 tmpfs、资源和网络限制；pi 与工具在容器内 | 宿主可写工作目录、Docker socket、未授权目标 |
| 公开结果 | profile 摘要、题目、轮次、进度、耗时、失败类别等 | flag、候选明文、模型 key、平台 token、原始 trace |
| 私密证据 | 仅研究所需的原始输出与候选，限制文件权限 | 自动进入公开结果或 CLI 输出 |

以上是目标不变式。新同步入口的真实容器生命周期与凭据 argv canary 已通过集成门；生产装配路径的目标可达、非目标不可达和 provider 代理白名单已在授权环境复核（见 §5 与 [roadmap.md](roadmap.md) M2）。主进程环境经 0600 的临时 `--env-file` 交给 `docker create`。尚未通过的发布门是平台确认的真实提交闭环。

## 2. SDK 端口与依赖方向

根包定义通用模型与端口；实现包依赖根包，装配代码负责组合，根包不导入具体适配器。

| 端口 / 模块 | 当前职责 | 代码状态 |
|---|---|---|
| Harness.Run(ctx, RunSpec) | 同步遍历题目、调用轮循环、保存结果 | 已实现并通过离线故障注入与真实容器闭环；授权平台提交闭环待验收 |
| Scenario | Discover → Prepare → Hint/Evaluate/Reconcile → Cleanup；平台副作用唯一入口 | scenario/Fake 与 TSecBench 已有；后者依赖宿主 bridge |
| Sandbox / SandboxSession | 为每题建隔离网络，Launch 一个 attached 主进程，Close/Reclaim 回收 | executor/Docker 已有；Probe 在运行所用镜像里执行 `pi --version` 并回报 `ProbeResult.PiVersion`，取不到版本即拒绝启动 |
| AgentFactory / Agent | 将 pi 绑定到已创建的 session，通过 RPC 执行轮次并发事件 | stub pi 的真实容器闭环已通过；真实 pi 已在 sandbox 内执行工具，平台确认的提交闭环待验收 |
| Planner / Renderer | DAG 事实、意图、剪枝和 prompt 渲染 | dag 可复用；由调用者注入。`Planner` 含 `HostFacts`（宿主验证事实计数）与 `Abandon`（换支），两者都是**接口方法**而非可选断言 |
| CandidateGate | 按来源归类、去重、判定候选可提交性 | `NewAll`（observed + derived）与 `New`（v0.3 的 observed-only 视图）**都在接口里**；`Mark` 收平台无关的 `Evaluation` |
| ResultStore | 保存公开指标并按维度聚合 | 起跑剩余量、增量召回率与题级通过率已落地；`Kind` 错误类别可落盘，私密 trace 由 `AppendTrace` 落 `private/` |
| CLI 装配 | doctor/list/run/stats 对接同步 Harness | 已由 cmd/red-harness → internal/cli → internal/wire 接线；真实容器纵向闭环已通过，平台提交闭环待验收 |
| `legacy`（v0.5 新增） | v0.3 的公开面：`Engine`/`Options`/`New`/`RegisterEngine`、`Snapshot`/`PublicSummary`/`RunHandle`/`SchemaVersion`、`DomainEvent` 及 14 个 `Ev*`、`Store`/`GraphStore`/`EvidenceStore`/旧 `Executor`、`RunPolicy`/`PolicyInput`/`RunSummary` | **叶子包**：只 import 标准库与根包。移出的判据是「是否被 v0.4 活路径使用」——`ExecSpec`/`ExecResult`/`ExecHandle`/`ExecOptions` 与 `ExecutorSpec` **留在根包**（前者在 `executor/session.go` 的 `NewSession` 上）。零生产调用方的清单见 [v0.4-open-items.md](v0.4-open-items.md) |

v0.4 的接口集中在根包 v04.go、model.go 和 ports.go。Version = 0.3.0 仍用于旧图 schema；ResearchVersion = 0.4.0-research 是新 SDK 标识。源码目前同时保留两套 API；CLI 已切换。真实容器运行由带 `integration` tag 的测试和授权环境记录证明，单元测试本身不承担这一证明。

## 3. 一题的运行流程

~~~mermaid
sequenceDiagram
    participant H as Harness
    participant S as Scenario
    participant X as Sandbox
    participant A as Agent
    participant G as Gate / DAG
    participant R as ResultStore
    H->>S: Discover / Prepare
    S-->>H: Challenge / Target
    H->>X: NewSession(宿主解析的目标)
    H->>A: New(session) / Start
    loop 预算内每轮
        H->>G: Next / Render
        H->>A: Round(prompt)
        A-->>G: 规范化事件 / 候选
        H->>G: Settle(轮次结果)
        H->>S: Evaluate(允许提交的候选)
        H->>S: Reconcile(平台权威进度)
    end
    H->>A: Close
    H->>X: Close / Reclaim
    H->>S: Cleanup
    H->>R: Save(公开指标)
~~~

关键规则：

1. Prepare 返回的目标须由宿主校验为不可变的 IP:port 集合，再交给 Sandbox；v0.4 `SandboxSpec` 没有调用者可填写的 AllowHosts。
2. Agent 的 reader 只向有界事件队列发送事件；编排 goroutine 消费并更新 DAG、Gate 和运行状态。
3. observed 与 derived 候选允许提交，fabricated 不提交；同一 Run 对同一候选只提交一次。提交结果不确定时，先按平台权威进度 Reconcile，再决定是否重试。
4. 每题最多一次提示；连续两轮无平台进度且无新增宿主验证事实时触发。提示后再次停滞，应切换未尝试的意图。
5. 可重试的 provider/进程故障最多重建 Agent 一次，并只回灌脱敏事实摘要；取消、超时、正常结束和失败都必须清理资源。

**当前实现偏差**：`eventSink` 已是有界队列（队列长度、单轮事件数、单条体积三重上限），DAG/Gate 回调改到消费者 goroutine 上执行并由 `Flush` 做轮次屏障；Docker `Probe` 在同一镜像里核验 pi 版本并把空版本读成「未核验」；成本与 turns 预算已从 Agent 统计取值；提交不确定时先 Reconcile 再以不确定终态结束本题；一次 Agent 重启、有界清理、取消后结果保存与清理失败记账均已落地；**事实型停滞与换支已落地**——停滞判据现在是「连续两轮既无平台进度、也无新增宿主验证事实」，提示一次后再次连续停滞即 `Abandon` 当前意图并转去未尝试的方向（`HintOff` 下不换支，那一档的契约是「从不请求提示」）。`RunSpec` 只承载运行意图与资源上限，运行目录归装配配置。CLI 装配已默认提供跨进程锁并在启动前扫描遗留资源，`NewHarness` 也已拒绝缺 Locker/Planner/Renderer/Gate/Results。

仍未完成：真实平台的授权冒烟（M2/M4 的发布门），以及默认拒绝出站与 canary 的真机复核。这两项都不由离线测试代替。

## 4. 数据与结果语义

- DAG 的事实层只记录可追溯的目标、服务、凭据线索和负面事实；答案层由 Gate 单独管理。answer.Fingerprint 是唯一指纹格式源。
- RunResult 是内存返回值，可能包含 Outcome.Flags；ResultFileStore 用专门的公开结构序列化，避免把整个返回值写盘。
- 目标公开结果位于 &lt;ResultDir&gt;/results/&lt;runID&gt;.json，只含指标与错误类别。原始 trace、证据和候选若需要持久化，应进入权限为 0700/0600 的 private/；`ResultFileStore.AppendTrace` 已把原始事件落到 &lt;ResultDir&gt;/private/&lt;runID&gt;/（题目编号取哈希，不直接做路径）。
- **私密候选审计（v0.5 新增）**：`ResultFileStore.AppendAudit` 把每次提交写到 `&lt;ResultDir&gt;/private/&lt;runID&gt;/submissions.jsonl`（0700/0600，一行一次提交，含指纹、来源与推导链锚点、提交时间、平台判定与候选明文）。它回答的是此前**不可回答**的问题——「提交了什么、平台怎么判的」；在此之前判定只活在内存里、随本题结束消失，公开结果只剩聚合计数。文件名**刻意不叫** `candidates.jsonl`：那个名字属于 v0.3 的候选账本（`&lt;StoreDir&gt;/runs/&lt;id&gt;/private/`，至今没有生产写入方）。
- `RemainingAtStart` 已在提交前记录并进入公开结果，`stats` 以「本次新增确认 / 起跑时剩余」计算召回率；分母未知的题目不计入比率。`ResultStore` 已放行契约的 `Kind` 枚举作为公开错误类别（`config`/`provider`/`executor` 等）。按 profile 分组时**要同时给 `--bundle`**：`ProfileDigest` 里存的只是扩展包的路径，同一个路径换了内容它不变，只按它分组会把两次不同的实验算作同一次。
- SolverProfile 的 system prompt 已进入 AgentStart，`RunResult.ProfileDigest` 与 sandbox 的 profile bundle 都取自 `Run` 开始时冻结的那份 `RunSpec.Profile`（空则回落到装配 profile），Planner 的 `dryRoundsBeforeHint` 与 Renderer 的 `maxFacts`/`maxNegative` 也读同一份。剩余缺口是这些键目前靠约定而非 schema 校验。
- **运行终态与「是否解出」是两个字段**：`RunResult.State` 取 `finished`/`failed`/`cancelled`（起跑前就失败的路径也置 `failed`，不留空串），`RunResult.Completed` 仍只表示「有题目达成平台权威的目标」。一次正常跑完却一题未解是 `ReasonNoProgress` + `State=finished` —— 把两者混成一个字段，正是前身「280 run / 0 flag」在报告里一片绿的成因。
- **`RunResult.Manifest` 冻结「实际生效的配置与产物身份」**（v0.5 新增）：停滞阈值、提交上限、提示策略、请求的镜像 tag、Probe 解析出的镜像 digest、镜像内 pi 版本、镜像不一致标记。为什么需要它：`ProfileDigest` 只有 16 个十六进制字符且**不可逆**——它能证明「两次运行不一样」，却回答不了「差在哪」，而「这次跑的是 2 轮还是 5 轮」「用的哪个镜像」正是复现一次实验所必需的。镜像 tag 与 digest **并列**是有意的：「tag 没变但内容变了」只有并列才看得出来。
- **`RunResult.BundleDigest` 冻结扩展包内容摘要**，与 `ProfileDigest` 并列不合并：后者描述「解法配置是什么」（`ExtensionBundle` 在其中是**路径字符串**），前者描述「那份配置指向的内容是什么」。空串表示**未核验**（本题没配 bundle），路径存在但读不了则 `Run` 在任何副作用之前以 `KindConfig` 失败。缺了它，「同一路径换了 bundle 内容」会被算作同一次实验。
- 题级公开指标含 `branchesAbandoned`（换支次数）：一次 `no_intent` 到底是方向都做完了，还是编排层把几个方向判成停滞扔掉了，两者的改法完全不同。

### GraphSaver：图是可选研究产物，不是运行成败的一部分

`GraphSaver` 是**可选端口**（`HarnessOptions.Graphs`，nil = 不落盘），生产装配恒提供它
（`internal/wire` 的 `dagGraphSaver`，落点 `&lt;StoreDir&gt;/runs/&lt;runID&gt;/graph.json` +
`graph.mmd`，0600）。三条必须一起读的性质：

1. **保存失败不算本题失败**：`Reason` 不变、`Completed` 不变——图是研究辅助面，把它算成
   失败会把「模型解出来了」在公开指标里降级成「跑坏了」。失败折成可比较的**阶段枚举**
   记进公开结果（`graphSaveFailures`）：`marshal` / `write` / `export` / `unknown`
   （未知错误归 `unknown`，不再假装是 `marshal`）。
2. **「没写出去」绝不能读成「写了」**：所以失败必须落在公开指标里，且 CLI 的 `run` 摘要
   会打印「图未落盘 &lt;阶段&gt;」。doctor 的 `graph_saver` 一行只说**装配在位**，不证明任何
   一次运行真的写出了图（题目没登记过图时 `dagGraphSaver` 会静默返回 nil——那不是失败，
   是「这一轮没有图」）。
3. **公开图必须经过 DAG 的统一擦洗路径**：`dag` 的 `document()` 由 `Graph.Save` 与
   `Graph.MarshalJSON` 共用，`Rejected[].Content` 里装的正是命中答案形状的原文——另拼一份
   序列化会把明文写出去，这条路曾经真的漏过。

## 5. 当前状态与验收口径

截至本文更新，`go test ./... -count=1` 与 `go test -race ./... -count=1` 均通过。三条真实容器/真实平台的门：

- **新同步入口的纵向闭环**（`go test -tags integration ./cmd/red-harness/... -count=1`，10.7s）：stub pi 与其子工具回报同一个容器 hostname 并等于 `Probe` 返回的容器 ID，provider key 不出现在任何一次 docker argv 里，正常 / 取消 / 启动失败三条路径按 run label 查容器与网络均为空。
- **执行器隔离用例**（`go test -tags integration ./executor/... -count=1`，**25 条，实测 84.1s 全绿、0 跳过**）：只读 rootfs 与有界 tmpfs、非 root、资源上限、宿主状态不可见、非授权端点不可达、provider 仅经白名单代理可达。这 25 条由两部分组成，**取证范围不同**：
  - **12 条 `TestIntegration*`** 走旧 `Exec` 端口（`executor/docker.go`）；
  - **13 条 `TestV04*`** 走 **`SandboxSession`（v0.4 生产路径，`executor/session.go`）**，2026-09-22 从不合并分支 `v04-container-lifecycle` 抢救回 master（`828d904`），并暴露出该路径上一个真缺口（`Launch` 的 Workdir 绕过校验、且一次被拒即永久废掉 session，见 `ededebb`）。
  两部分都**用 `DefaultDockerConfig()` 构造执行器**，所以它们证明的是执行器与 session 本身，**不能**代替对**装配路径**的验证（见下）——生产装配曾经把隔离开关整片落成零值，而那 25 条照样全绿。
- **授权 TSecBench 真跑（首次打通）**：`list_challenges` 拿到 63 题；`start_challenge` 起容器后本地 sandbox 内 pi `0.85.1` 跑了 58 次真实工具调用，从靶场拿到 `HTTP/1.1 200 OK`；217 条事件落入 `private/`，成本 $0.0117。提交被平台以 `app_error (http 501)` 挡下，harness 按设计以「提交结果不确定」结束本题并**正确关闭了题目容器**（平台侧确认 `stopped`，无遗留）。

**同一次真跑暴露了两个缺陷，均已修复**：

1. **生产装配路径的网络隔离曾整片失效**（`internal/wire`）。装配层手写 `DockerConfig{ProviderAllowHosts: hosts}`，其余字段落零值，而零值里的 `ManageIptables` 与 `ProviderProxy` 均为 false。修复为从 `DefaultDockerConfig()` 起手并加装配级回归断言后，已在授权环境重跑：目标可达，非目标及公网直连不可达，provider 仅经白名单代理可达；详细读数见 [roadmap.md](roadmap.md) M2。
2. **题目耗时恒为 0**：`OutcomeView.StartedAt/EndedAt` 从未被赋值，而 CLI 摘要、公开结果 `durationSeconds`、stats 累计耗时三处都读 `OutcomeView.Duration()`。已回填。

**尚未证明**：提交路径上平台 `app_error (http 501)` 的成因（我方请求形状还是平台侧）；平台确认的完整提交闭环与线上通过率、召回率。
