# red-harness SDK 架构（v0.4.0-research）

> 更新：2026-09-22。本文按当前工作区代码描述实现状态；目标行为见 [PLAN v0.4](PLAN%20v0.4.md)，收尾出口门见 [roadmap.md](roadmap.md)。
> 旧 Engine、RunHandle 和事件快照仍在源码中供 v0.3 读取与测试使用；CLI 已切到 v0.4 同步入口。

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

以上是目标不变式。当前 Docker 适配器已实现基础隔离配置；新同步入口的越界阻断、凭据保密和所有清理路径仍需真实容器集成门证明。主进程环境已改为经 0600 的临时 `--env-file` 交给 `docker create`（argv canary 有集成回归），provider key 不再出现在宿主进程参数里；剩余绑定项是真实容器纵向闭环与授权平台冒烟。

## 2. SDK 端口与依赖方向

根包定义通用模型与端口；实现包依赖根包，装配代码负责组合，根包不导入具体适配器。

| 端口 / 模块 | 当前职责 | 代码状态 |
|---|---|---|
| Harness.Run(ctx, RunSpec) | 同步遍历题目、调用轮循环、保存结果 | 已实现初版；缺硬化与纵向验收 |
| Scenario | Discover → Prepare → Hint/Evaluate/Reconcile → Cleanup；平台副作用唯一入口 | scenario/Fake 与 TSecBench 已有；后者依赖宿主 bridge |
| Sandbox / SandboxSession | 为每题建隔离网络，Launch 一个 attached 主进程，Close/Reclaim 回收 | executor/Docker 已有；Probe 在运行所用镜像里执行 `pi --version` 并回报 `ProbeResult.PiVersion`，取不到版本即拒绝启动 |
| AgentFactory / Agent | 将 pi 绑定到已创建的 session，通过 RPC 执行轮次并发事件 | piai/Factory 已有；生产链路尚未做容器内验收 |
| Planner / Renderer | DAG 事实、意图、剪枝和 prompt 渲染 | dag 可复用；由调用者注入 |
| CandidateGate | 按来源归类、去重、判定候选可提交性 | gate.NewAll 提供 v0.4 的 observed/derived 视图 |
| ResultStore | 保存公开指标并按维度聚合 | 起跑剩余量、增量召回率与题级通过率已落地；`Kind` 错误类别可落盘，私密 trace 由 `AppendTrace` 落 `private/` |
| CLI 装配 | doctor/list/run/stats 对接同步 Harness | 已由 cmd/red-harness → internal/cli → internal/wire 接线；真实容器纵向闭环待验收 |

v0.4 的接口集中在根包 v04.go、model.go 和 ports.go。Version = 0.3.0 仍用于旧图 schema；ResearchVersion = 0.4.0-research 是新 SDK 标识。源码目前同时保留两套 API；CLI 已切换，但通过现有单元测试仍不能证明真实容器运行可用。

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

**当前实现偏差**：`eventSink` 已是有界队列（队列长度、单轮事件数、单条体积三重上限），DAG/Gate 回调改到消费者 goroutine 上执行并由 `Flush` 做轮次屏障；Docker `Probe` 在同一镜像里核验 pi 版本；成本与 turns 预算已从 Agent 统计取值；提交不确定时先 Reconcile 再以不确定终态结束本题；一次 Agent 重启、有界清理、取消后结果保存与清理失败记账均已落地。仍未完成：事实型停滞与换支，以及真实容器的纵向验收。CLI 装配已默认提供跨进程锁并在启动前扫描遗留资源，`NewHarness` 也已拒绝缺 Locker/Planner/Renderer/Gate/Results。未完成项对应 [roadmap.md](roadmap.md) 的 M4 出口门。

## 4. 数据与结果语义

- DAG 的事实层只记录可追溯的目标、服务、凭据线索和负面事实；答案层由 Gate 单独管理。answer.Fingerprint 是唯一指纹格式源。
- RunResult 是内存返回值，可能包含 Outcome.Flags；ResultFileStore 用专门的公开结构序列化，避免把整个返回值写盘。
- 目标公开结果位于 &lt;ResultDir&gt;/results/&lt;runID&gt;.json，只含指标与错误类别。原始 trace、证据和候选若需要持久化，应进入权限为 0700/0600 的 private/；`ResultFileStore.AppendTrace` 已把原始事件落到 &lt;ResultDir&gt;/private/&lt;runID&gt;/（题目编号取哈希，不直接做路径）。
- `RemainingAtStart` 已在提交前记录并进入公开结果，`stats` 以「本次新增确认 / 起跑时剩余」计算召回率；分母未知的题目不计入比率。`ResultStore` 已放行契约的 `Kind` 枚举作为公开错误类别（`config`/`provider`/`executor` 等）。仍需用真实运行核验数据完整性。
- SolverProfile 的 system prompt 已进入 AgentStart，`RunResult.ProfileDigest` 与 sandbox 的 profile bundle 都取自 `Run` 开始时冻结的那份 `RunSpec.Profile`（空则回落到装配 profile），Planner 的 `dryRoundsBeforeHint` 与 Renderer 的 `maxFacts`/`maxNegative` 也读同一份。剩余缺口是这些键目前靠约定而非 schema 校验。

## 5. 当前状态与验收口径

截至本文更新，`go test ./... -count=1` 通过。它证明现有包测试可编译并运行，CLI 装配也有单元测试；**尚未证明**同步 Harness 的真实 Docker 纵向闭环、默认拒绝出站、取消清理或线上通过率。现有 Docker 集成测试主要走旧 `Executor` 入口。下一步按 [roadmap.md](roadmap.md) 先完成 Fake + stub pi 的真实容器全生命周期，再封闭凭据与隔离门。
