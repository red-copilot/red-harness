# red-harness SDK 架构（v0.4.0-research + v0.5 契约整理）

> 更新：2026-09-22。本文按当前工作区代码描述实现状态；目标行为见 [PLAN v0.4](PLAN%20v0.4.md)，收尾出口门见 [roadmap.md](roadmap.md)。
> 下一阶段的目标架构、接口演进和发布路线见 [offensive-harness-sdk-roadmap.md](offensive-harness-sdk-roadmap.md)。
> 新架构提案见 [offensive-harness-sdk-architecture-next.md](offensive-harness-sdk-architecture-next.md)：公开装配、运行内核、提交审计与实验复现的具体设计；其中计划能力尚未实现。
> **旧 Engine、RunHandle、事件快照与 v0.3 的三个存储端口已移出根包到 `legacy/`**（R1 落地）；
> CLI 已切到 v0.4 同步入口。⚠️ **R0（v0.4 发布门）仍未通过**——`legacy/` 的移出是
> 契约整理，不是发布信号。
> **N0.1–N0.4 已落地**（公开装配 `local`、锁与回收作用域修正、每题图路径、审计失败可见性），
> 出口门已离线实测；N0.5 按决定推迟。逐条证据见
> [offensive-harness-sdk-roadmap.md](offensive-harness-sdk-roadmap.md) 的「N0 的落地状态」。

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

### 资源归属与单运行锁（信任边界的地基）

宿主的容器、网络与 iptables 规则是**跨 run 共享的命名空间**，所以「哪些资源是我的、什么时候能删」必须由**标注**决定，不能由名字决定：

| 标签 | 值 | 作用 |
|---|---|---|
| `red-harness.owner` | `harness.OwnerID` = `&lt;hostname&gt;/&lt;uid&gt;`（hostname 过字符集闸，如 `box/0`） | 「哪个部署主体建的」——回收判据的一半 |
| `red-harness.run` | `RunID` | 「哪次运行」——回收判据的另一半 |
| `red-harness.challenge` / `red-harness.attempt` / `red-harness.role` | 题目 ID / 尝试号 / 资源角色 | 只用于**定位**与索引，**不作为删除依据** |

⚠️ **标签值是 owner 本身，不是它的指纹。** `OwnerID` 有两个投影：原串（`<hostname>/<uid>`）进 label，8 位十六进制摘要走 `OwnerID.Fingerprint()` 且**只用在长度敏感的落点**（iptables 规则注释要求 ≤256 字符且无引号/空白/换行，而 hostname 可能很长）。两者不可互换——把标签写成指纹，回收判据就再也匹配不上任何东西。这与 `answer.Fingerprint` 是同一条纪律的另一处应用：同一个身份的第二处截断必然与第一处漂移，漂移的形态（「两条规则看起来都像本 owner 的」）是静默的。

**回收判据是 `run + owner` 两条**（权威表在 `harness.ReclaimReport` 的注释，实现在 `executor/reclaim.go` 的 `judgeStale`）：

```
owner 匹配 且 runID ∉ live ⇒ 删除，进 Reclaimed
owner 匹配 且 runID ∈  live ⇒ 保留，两条都不进（正常活跃，不是遗留）
owner 不匹配                ⇒ 保留，进 Pending(owner_mismatch)  别人的部署；共享宿主上正常
无 owner 标签               ⇒ 保留，进 Pending(owner_unknown)   升级前的旧资源；无法证明归属
解析不出 run 标签           ⇒ 保留，进 Pending(unparsable)      扫描器无法归属 ⇒ 这是警报
```

**「保留」的含义是绝不删除，不是稍后重试。** 只按 run 标签删是不够的：名字可以被抢注，而 run 标签在宿主上**不唯一**——两个用不同 `--store` 的进程各自开一个 `run-1` 是合法的。owner 解析不出来时 `ReclaimStale` 返回**零值报告**并 fail closed（一个都不删）：「我是谁」不可判定时，「谁都不像我的」不能当成「可以删」。

⚠️ **这一段同时是「单运行锁」这个设计的存在理由，而且它是在 N0.2 之后才真正成立的。** `ReclaimStale` 曾经在生产路径上**完全空转**：扫描用 `docker ps --format '{{json .Labels}}'`，而它打印的是一个**逗号拼接的字符串**、不是 JSON 对象，于是每个对象都解析失败 ⇒ 归 `unparsable`，而 `unparsable` 的判据是「只报告」⇒ **一件都不删、且完全静默**。修在 `3d3d299`（改走 `docker inspect`：先 `--quiet` 取 ID，再 inspect 取结构）。

**修好之前**「两个进程互删资源」并不成立（回收根本删不掉东西），所以「资源还在」这类断言当时是**空转**的。**修好之后，锁第一次成为载荷**：两个进程若没有锁互斥，第二个会在启动时按「owner 匹配 且 runID 不在本次 live 集合里」判定，把第一个进程**正在跑的**容器 `docker rm --force` 掉——这是实测结论（`cmd/red-harness/lock_integration_test.go` 起两个真 CLI 二进制验证），不是推理。这也是 `Run` 的副作用顺序「**先取锁，再回收**」不可调换的原因。

**锁的位置与命名**（`local/lock.go`）：

- 默认落在 `/run/lock/red-harness/run-&lt;端点指纹&gt;.lock`，**不随 `--store` 变**。这是本文件最重要的一条规矩：flock 绑的是 **inode 而不是路径**，锁与被它保护的数据住在一起时，`rm -rf` 旧 store（或换一个 `--store`）会让同一路径指向新 inode，正在跑的那次部署**被静默解锁**，下一个进程顺手拿到锁、两个 run 同时开跑。一句话：**锁不能与被它保护的数据住在一起。**
- 文件名按 **daemon 端点**取指纹（`harness.DockerEndpoint.Fingerprint`，规范化后 `scheme://host` 的 8 位摘要）。默认端点 `unix:///var/run/docker.sock` 的指纹是常量 `13c4025c`，所以默认部署下锁文件名是确定的。**为什么按端点区分而不是一把全局锁**：同一宿主上的两个不同本地 socket 是**两个不同的 daemon**，不共享容器、网络或 iptables 表，本就不该互斥；一把全局锁会把它们串行化，那是把「作用域算错」换成另一个方向的算错。选 `/run/lock` 是因为它是 FHS 给锁文件留的位置，且**重启即清空**（tmpfs）——与 flock 的语义正好配套。
- 实现是 **flock**，不是 `O_CREATE|O_EXCL`：锁由**内核**持有，进程以任何方式退出（正常返回、panic、SIGKILL、段错误）都会自动释放，所以残留的锁文件不构成阻塞——一次崩溃不会把这台机器变成永久不可运行。
- `Run` 在**任何平台或容器副作用之前**取锁；`--lock &lt;path&gt;` 是**显式覆盖**，⚠️ **不同 `--lock` 路径之间不互斥**。
- **跨用户与跨主机互斥不在设计范围内**：远程 daemon 直接拒绝（宿主级锁无法跨主机互斥，`harness.ResolveDockerEndpoint`）；`owner` 身份含 uid，所以同一宿主上不同用户的资源在回收判据里互为 `owner_mismatch`（只报告、不删）。锁的作用域是「**同一宿主 + 同一 daemon 端点**」，把它读成「同一台机器上任何两个用户之间都互斥」是错的。

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
| CLI 装配 | doctor/list/run/stats 对接同步 Harness | 已由 cmd/red-harness → internal/cli → **`local`** 接线（`cli.Main` 收显式装配函数参数）；真实容器纵向闭环已通过，平台提交闭环待验收 |
| `legacy`（v0.5 新增） | v0.3 的公开面：`Engine`/`Options`/`New`/`RegisterEngine`、`Snapshot`/`PublicSummary`/`RunHandle`/`SchemaVersion`、`DomainEvent` 及 14 个 `Ev*`、`Store`/`GraphStore`/`EvidenceStore`/旧 `Executor`、`RunPolicy`/`PolicyInput`/`RunSummary` | **叶子包**：只 import 标准库与根包。移出的判据是「是否被 v0.4 活路径使用」——`ExecSpec`/`ExecResult`/`ExecHandle`/`ExecOptions` 与 `ExecutorSpec` **留在根包**（前者在 `executor/session.go` 的 `NewSession` 上）。零生产调用方的清单见 [v0.4-open-items.md](v0.4-open-items.md) |

v0.4 的接口集中在根包 v04.go、model.go 和 ports.go。Version = 0.3.0 仍用于旧图 schema；ResearchVersion = 0.4.0-research 是新 SDK 标识。源码目前同时保留两套 API；CLI 已切换。真实容器运行由带 `integration` tag 的测试和授权环境记录证明，单元测试本身不承担这一证明。

### 装配层与依赖方向（N0.1 之后）

**`local` 是唯一的装配点**（`local/wire.go` 的包文档），全仓只有它同时 import 全部 7 个实现包（`dag`/`gate`/`store`/`executor`/`bridge`/`piai`/`scenario`）；实现包之间两两不 import，`answer` 与 `legacy` 是共享叶子。这条断言**不再只写在文档里**：`legacy/layering_test.go` 用 `go/parser` 扫全仓 import 图做四条断言（根包零内部依赖、`legacy` 只 import 标准库与根包、实现包两两零边、同时 import ≥2 个实现包的恰好只有 `local`），`assemblyPkgs` 已是单元素集合。

- **`local` 必须在 `internal/` 之外**：Go 的 internal 可见性规则让 `internal/wire` 只能被本模块 import，于是仓库**内部**的任何示例都证明不了「公开面真的可用」。`example/external/` 是一个独立 module（自己的 `go.mod`，`replace` 指回仓库根），它 import `local` 并跑通一次 Fake 运行——那才是这件事的可执行证据（N0.1 的出口门）。
- **`internal/wire` 已退化为薄转发层，且当前没有任何 import 方**。留着它是给外部调用方一轮迁移窗口，**它不是第二个装配点**。
- **CLI 不 import 实现包**：它只依赖包内定义的窄接口（`internal/cli.Ports`），装配函数由 `cmd/red-harness/main.go` 的 `main()` **显式**传给 `cli.Main(args, stdout, stderr, wire WireFunc)`。包级 `SetWire` 已删除——接线发生在一个没人调用的 `init()` 里时，「CLI 连的到底是谁、默认值从哪来」只能靠读 `init()` 才知道，而测试要换掉它就得改包级变量、并行用例互相污染。
- **默认值的唯一真源是装配层 `local`**。RunSpec 级的默认值由 `local.Runner.resolve()` 补齐（场景、agent、镜像与扩展包、预算回落、提示策略、目标集合、policy）；而**运行目录（`StoreDir`/`ResultDir`）与锁路径不写回 spec**——它们由 `Runner` 自己持有、在 `local.New` 里就落定（结果后端按它们建好，锁按 daemon 端点算好），因为它们是**部署属性**而不是运行意图。CLI 与直接调 SDK 的调用方都从这一处拿默认值；`cmd/red-harness` 的 `newPorts` 只做「CLI 输入 → `local.Options`」的薄翻译，**不补任何缺省**（唯一的例外是 doctor 的空 storeDir，理由是 doctor 体检的是宿主环境、与某个 store 无关）。历史上这里替 `StoreDir`/`ResultDir` 各补过一次缺省，两次都让「同一次运行」在两条路径上变成两个落点。

三条把系统拼起来的边在 import 图里**看不见**，只按 import 判依赖会误判 `internal/cli` / `scenario` / `piai` 是孤立：

| 边 | 机制 |
|---|---|
| `internal/cli → local` | `cli.Main` 的第四个参数（`WireFunc`），由 `cmd/red-harness/main.go` 的 `main()` 显式传入 `newPorts` |
| `scenario → bridge` | `local` 注入的 `Platform` 窄接口（`*bridge.Client`） |
| `piai → executor` | `AgentFactory.New` 收 `harness.SandboxSession`，`piai` 不 import `executor` |

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
- **私密候选审计（v0.5 新增）**：`ResultFileStore.AppendAudit` 把每次提交写到 `&lt;ResultDir&gt;/private/&lt;runID&gt;/submissions.jsonl`（0700/0600，一行一次提交，含指纹、来源与推导链锚点、提交时间、平台判定与候选明文）。它回答的是此前**不可回答**的问题——「提交了什么、平台怎么判的」；在此之前判定只活在内存里、随本题结束消失，公开结果只剩聚合计数。文件名**刻意不叫** `candidates.jsonl`：那个名字属于 v0.3 的候选账本（`&lt;StoreDir&gt;/runs/&lt;id&gt;/private/`），它**没有生产写入方**——⚠️ 但 `store.FileStore.ForRun` → `ensureLedger` 会在建骨架上建出一个 **0 字节**的空账本与 `private/evidence/` 目录，所以「文件在」不等于「写过」。
- `RemainingAtStart` 已在提交前记录并进入公开结果，`stats` 以「本次新增确认 / 起跑时剩余」计算召回率；分母未知的题目不计入比率。`ResultStore` 已放行契约的 `Kind` 枚举作为公开错误类别（`config`/`provider`/`executor` 等）。按 profile 分组时**要同时给 `--bundle`**：`ProfileDigest` 里存的只是扩展包的路径，同一个路径换了内容它不变，只按它分组会把两次不同的实验算作同一次。
- SolverProfile 的 system prompt 已进入 AgentStart，`RunResult.ProfileDigest` 与 sandbox 的 profile bundle 都取自 `Run` 开始时冻结的那份 `RunSpec.Profile`（空则回落到装配 profile），Planner 的 `dryRoundsBeforeHint` 与 Renderer 的 `maxFacts`/`maxNegative` 也读同一份。剩余缺口是这些键目前靠约定而非 schema 校验。
- **运行终态与「是否解出」是两个字段**：`RunResult.State` 取 `finished`/`failed`/`cancelled`（起跑前就失败的路径也置 `failed`，不留空串），`RunResult.Completed` 仍只表示「有题目达成平台权威的目标」。一次正常跑完却一题未解是 `ReasonNoProgress` + `State=finished` —— 把两者混成一个字段，正是前身「280 run / 0 flag」在报告里一片绿的成因。
- **`RunResult.Manifest` 冻结「实际生效的配置与产物身份」**（v0.5 新增）：停滞阈值、提交上限、提示策略、请求的镜像 tag、Probe 解析出的镜像 digest、镜像内 pi 版本、镜像不一致标记。为什么需要它：`ProfileDigest` 只有 16 个十六进制字符且**不可逆**——它能证明「两次运行不一样」，却回答不了「差在哪」，而「这次跑的是 2 轮还是 5 轮」「用的哪个镜像」正是复现一次实验所必需的。镜像 tag 与 digest **并列**是有意的：「tag 没变但内容变了」只有并列才看得出来。
- **`RunResult.BundleDigest` 冻结扩展包内容摘要**，与 `ProfileDigest` 并列不合并：后者描述「解法配置是什么」（`ExtensionBundle` 在其中是**路径字符串**），前者描述「那份配置指向的内容是什么」。空串表示**未核验**（本题没配 bundle），路径存在但读不了则 `Run` 在任何副作用之前以 `KindConfig` 失败。缺了它，「同一路径换了 bundle 内容」会被算作同一次实验。
- 题级公开指标含 `branchesAbandoned`（换支次数）：一次 `no_intent` 到底是方向都做完了，还是编排层把几个方向判成停滞扔掉了，两者的改法完全不同。

### GraphSaver：图是可选研究产物，不是运行成败的一部分

`GraphSaver` 是**可选端口**（`HarnessOptions.Graphs`，nil = 不落盘），生产装配恒提供它
（`local` 的 `dagGraphSaver`）。落点是**每题一份**的
`&lt;StoreDir&gt;/runs/&lt;runID&gt;/challenges/&lt;题目 ID&gt;/attempts/1/` 下的 `graph.json` +
`graph.mmd`（0600）——路径由 `store.FileStore.ForAttempt` 拼，**装配层不拼路径**（布局只有
一处定义，否则 store 的测试证明不了布局，而「图写到 A、索引指向 B」这类错配没有断言能挡）。
四条必须一起读的性质：

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
4. **产物带题目身份，且旧路径只读兼容**（N0.3）：一 run 一份图时，后一题会把前一题的图
   原样盖掉，跑完只剩最后一道题的图——而复盘的人无从知道前面那些题留过图。现在每题一份，
   另加一份 `artifacts.json` 产物索引（0600）回答「图到底留下来没有」。旧路径
   `&lt;runID&gt;/graph.json` 由 `FileStore.ReadGraph` 在**文件不存在**时回退读取，并如实返回
   `GraphSourceChallenge` / `GraphSourceLegacyRunRoot` 让调用方自己判断「读到的是不是我要的
   那道题」：旧 run 里只有一份图，谁也无法反推出「另一道题的那份」，所以**不尝试补造已覆盖
   的历史图**。⚠️ `attempts/&lt;n&gt;` 这一层必须保留——`harness.FirstAttempt` 是带名字的
   **常量而不是计数器**（今天每题一次、没有重试），将来真加重试时 reader 才不必同时认
   「两层」与「三层」两种目录形态。

### 公开面的新增字段（N0 落地）

公开结果 `&lt;ResultDir&gt;/results/&lt;runID&gt;.json` 由**存储层自己的**结构序列化
（`store/results.go` 的 `publicResult` / `publicChallenge`），根包加一个字段**不会**自动出现
在这里——每个字段都得显式决定怎么脱敏。N0 之后新增的字段（拼写以 `grep -n 'json:"'
store/results.go` 为准）：

| 字段 | 层级 | 答的是什么问题 |
|---|---|---|
| `reclaimedStale` / `pendingStale` | run | 启动前那次遗留回收**真的删掉了几个** / **发现了但没删几个**。只放计数：`Pending` 的明细是**本机资源拓扑**，进这份文件等于把它拷进会被转发、归档、贴工单的结果里。缺了它，「回收跑过了、什么都没删」与「宿主上本来就没有孤儿」在结果里完全同形 |
| `attempts` | 题 | **实际发起 Evaluate 的次数**（含结果不确定的那一次）。它**不是** `submitted` 的别名：`submitted` 是去重后的确认数（幂等命中也算），一次 3 次提交、1 次判错时两个数是 2 与 3 |
| `duplicates` / `rejected` | 题 | 平台幂等命中数（`submitted` 的子集）/ 被平台判错的候选数。二者此前只活在内存里、随本题结束消失，**只在 CLI 的 stdout 里出现过**，于是「答案是蒙对的还是推出来的」事后不可答 |
| `auditIncomplete` | 题 | **本题的私密候选审计可能不完整**。它是一条不对称的唯一痕迹：审计写失败不算本题失败（`Reason` 不变、已确认成绩保留），所以公开面里没有别的东西能说明「这次运行的审计是缺的」——而把「缺」读成「完整」会让事后按审计行数做的结论系统性偏低 |
| `graphState` / `graphSaveFailures` | 题 | 图产物的结果枚举（`disabled`/`absent`/`saved`/`failed` 折算后）与失败阶段（`marshal`/`write`/`export`/`unknown`）。图写失败同样不算本题失败，所以「没写出去」必须落在公开指标里，否则会被读成「写了」 |
| `submissionsCapped` | 题 | 撞到提交上限而未提交的候选数 |
| `manifest` | run | 本次运行**实际生效**的配置与产物身份（镜像 tag 与 digest **并列**、pi 版本、停滞阈值、提交上限、提示策略）。它用 `omitzero` 而不是 `omitempty`：直接构造的 `RunResult`（测试、旧调用方）没有清单，不该在文件里留下一片零值字段——「没记录」与「记了一堆 0」看起来必须不一样 |

### 审计写失败的行为（N0.4）

候选审计 `&lt;ResultDir&gt;/private/&lt;runID&gt;/submissions.jsonl` 是**私密面**（0700/0600），
每次提交一行、含平台判定与候选明文。它写不出去时的语义是被明确选择过的，三条一起读：

1. **返回 `KindPersistence`**，错误链上带 `ErrAuditIncomplete` 哨兵——调用方用 `errors.Is`
   判断，不解析消息（`errors.go` 明令禁止按字符串分类）。
2. **停止整个 Run**（`Run` 的题目循环遇 `KindPersistence` 即 `break`）。这与「一道题起不来
   不该让剩下的几十道题陪葬」不矛盾：审计写不出去**不是「这道题的事」**——它意味着这次运行
   的提交记录**已经不完整了**，而 `Submit=true` 的每一次提交都是不可追回的平台写操作，
   继续跑只会让缺口越拉越大，事后没有别的办法回答「到底提交了什么、平台怎么判的」。
3. **保留已确认成绩**：`Submitted` / `Flags` / `Score` **不清零**——它们是审计失败**之前**
   平台已经确认的事实，清零等于把「已经解出来了」改写成「没解出来」。**停止不等于抹掉**；
   本题另标 `auditIncomplete`（见上表）说明缺口在哪。这条与 `harness.result`（`results.Save`
   失败）不同：那一处 `Run` 直接返回错误，因为它已经没有结果可交。

## 5. 当前状态与验收口径

截至本文更新，`go test ./... -count=1` 与 `go test -race ./... -count=1` 均通过。三条真实容器/真实平台的门：

- **新同步入口的纵向闭环 + 双进程争锁**（`go test -tags integration ./cmd/red-harness/... -count=1`，**2 个顶层用例**，2026-09-22 实测 36.1s 全绿）：`TestIntegrationCLIFakeDocker` 证明 stub pi 与其子工具回报同一个容器 hostname 并等于 `Probe` 返回的容器 ID，provider key 不出现在任何一次 docker argv 里，正常 / 取消 / 启动失败三条路径按 run label 查容器与网络均为空；`TestIntegrationTwoStoresContendForTheRunLock`（`lock_integration_test.go`）构建**真 CLI 二进制**并起两个不同 `--store` 的进程争同一把锁，第二个必须在**回收之前**失败，第一个正在跑的容器与网络原样在跑——它同时是「锁成为载荷之后，回收判据不会删别人的东西」的证据。
- **执行器集成门**（`go test -tags integration ./executor/... -count=1`，2026-09-22 实测 **70 PASS / 0 SKIP / 90.3s**）。⚠️ **其中需要 Docker 的只有 27 条**，另 43 条是纯单元用例（不带 tag 也跑，`go test ./executor/` 自己就是 43 PASS）——引用这个「70」时必须带上这个拆分，否则会把不需要容器的用例算成隔离证据。27 条验的是：只读 rootfs 与有界 tmpfs、非 root、资源上限、宿主状态不可见、非授权端点不可达、provider 仅经白名单代理可达。其中 **13 条 `TestV04*`** 走 **`SandboxSession`（v0.4 生产路径，`executor/session.go`）**，2026-09-22 从不合并分支 `v04-container-lifecycle` 抢救回 master（`828d904`），并暴露出该路径上一个真缺口（`Launch` 的 Workdir 绕过校验、且一次被拒即永久废掉 session，见 `ededebb`）；**14 条 `TestIntegration*`** 走旧 `Exec` 端口（`executor/docker.go`）；其余 43 条是 `TestSpec*`（22 条）、`TestPlan*` / `TestReclaim*`（各 3 条）、`TestParseInspect` / `TestClassifyScanned` / `TestScanRoundsTripToJudge` / `TestSelfOwnerFailsClosed`（N0.2 回收扫描的回归防线：容器把用户标签放在 `.Config.Labels`、网络放在顶层 `.Labels`，两种实测形态各钉一条）、以及 `TestJudge*` / `TestIptables*` / `TestProxy*` / `TestNew*` 等以 argv 渲染与判定表为对象的用例。⚠️ 需要 Docker 的那 27 条**一律用 `DefaultDockerConfig()` 构造执行器**，所以它们证明的是执行器与 session 本身，**不能**代替对**装配路径**的验证——生产装配曾经把隔离开关整片落成零值，而那批用例照样全绿。（另 43 条连执行器都不构造，它们钉的是 argv 渲染与判定表，同理不覆盖装配路径。）
- **授权 TSecBench 真跑（首次打通）**：`list_challenges` 拿到 63 题；`start_challenge` 起容器后本地 sandbox 内 pi `0.85.1` 跑了 58 次真实工具调用，从靶场拿到 `HTTP/1.1 200 OK`；217 条事件落入 `private/`，成本 $0.0117。提交被平台以 `app_error (http 501)` 挡下，harness 按设计以「提交结果不确定」结束本题并**正确关闭了题目容器**（平台侧确认 `stopped`，无遗留）。

**同一次真跑暴露了两个缺陷，均已修复**：

1. **生产装配路径的网络隔离曾整片失效**（当时的装配层 `internal/wire`；N0.1 之后这段代码搬到了 `local`，缺陷与修法随之一并搬走）。装配层手写 `DockerConfig{ProviderAllowHosts: hosts}`，其余字段落零值，而零值里的 `ManageIptables` 与 `ProviderProxy` 均为 false。修复为从 `DefaultDockerConfig()` 起手并加装配级回归断言后，已在授权环境重跑：目标可达，非目标及公网直连不可达，provider 仅经白名单代理可达；详细读数见 [roadmap.md](roadmap.md) M2。
2. **题目耗时恒为 0**：`OutcomeView.StartedAt/EndedAt` 从未被赋值，而 CLI 摘要、公开结果 `durationSeconds`、stats 累计耗时三处都读 `OutcomeView.Duration()`。已回填。

**尚未证明**：提交路径上平台 `app_error (http 501)` 的成因（我方请求形状还是平台侧）；平台确认的完整提交闭环与线上通过率、召回率。
