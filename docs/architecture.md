# red-harness SDK 架构（v0.4.0-research）

> 更新：2026-09-21。本文按当前工作区代码描述实现状态；目标行为见 [PLAN v0.4](PLAN%20v0.4.md)，交付顺序见 [roadmap.md](roadmap.md)。
> 旧 Engine、RunHandle、事件快照和 CLI 仍在源码中，供 v0.3 读取与测试使用；它们尚未成为 v0.4 的可用入口。

## 1. 定位与边界

v0.4 是面向**明确授权的 CTF、TSecBench 和本地靶场**的单机研究 SDK。一次同步 Harness.Run 串行处理题目；每题一个 Docker sandbox、一个 pi Agent 会话。宿主负责平台 API、目标范围、调度、候选判定和结果；Agent 与工具都在 sandbox 内运行。当前不支持一般真实资产、多用户、远程 worker、并发 Run 或崩溃续跑。

~~~mermaid
flowchart LR
    Caller["SDK 调用者 / 未来 CLI"] --> H["Harness.Run<br/>同步编排"]
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

以上是目标不变式。当前 Docker 适配器已实现基础隔离配置；越界阻断、凭据泄漏和所有清理路径仍需真实容器集成门证明。

## 2. SDK 端口与依赖方向

根包定义通用模型与端口；实现包依赖根包，装配代码负责组合，根包不导入具体适配器。

| 端口 / 模块 | 当前职责 | 代码状态 |
|---|---|---|
| Harness.Run(ctx, RunSpec) | 同步遍历题目、调用轮循环、保存结果 | 已实现初版；缺硬化与纵向验收 |
| Scenario | Discover → Prepare → Hint/Evaluate/Reconcile → Cleanup；平台副作用唯一入口 | scenario/Fake 与 TSecBench 已有；后者依赖宿主 bridge |
| Sandbox / SandboxSession | 为每题建隔离网络，Launch 一个 attached 主进程，Close/Reclaim 回收 | executor/Docker 已有；Probe 当前只检查镜像 |
| AgentFactory / Agent | 将 pi 绑定到已创建的 session，通过 RPC 执行轮次并发事件 | piai/Factory 已有；生产链路尚未做容器内验收 |
| Planner / Renderer | DAG 事实、意图、剪枝和 prompt 渲染 | dag 可复用；由调用者注入 |
| CandidateGate | 按来源归类、去重、判定候选可提交性 | gate.NewAll 提供 v0.4 的 observed/derived 视图 |
| ResultStore | 保存公开指标并按维度聚合 | store/ResultFileStore 已有初版；指标口径未齐 |
| CLI 装配 | doctor/list/run/stats 对接同步 Harness | **未实现**；当前 CLI 仍使用 v0.3 Engine.Start/Resume，WireFunc 为 nil |

v0.4 的接口集中在根包 v04.go、model.go 和 ports.go。Version = 0.3.0 仍用于旧图 schema；ResearchVersion = 0.4.0-research 是新 SDK 标识。源码目前同时保留两套 API，不能把测试通过理解为 CLI 已切换。

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

1. Prepare 返回的目标须由宿主解析为不可变的 IP:port 集合，再交给 Sandbox；调用者不能扩大 AllowHosts。
2. Agent 的 reader 只向有界事件队列发送事件；编排 goroutine 消费并更新 DAG、Gate 和运行状态。
3. observed 与 derived 候选允许提交，fabricated 不提交；同一 Run 对同一候选只提交一次。提交结果不确定时，先按平台权威进度 Reconcile，再决定是否重试。
4. 每题最多一次提示；连续两轮无平台进度且无新增宿主验证事实时触发。提示后再次停滞，应切换未尝试的意图。
5. 可重试的 provider/进程故障最多重建 Agent 一次，并只回灌脱敏事实摘要；取消、超时、正常结束和失败都必须清理资源。

**当前实现偏差**：eventSink 是无界切片且回调可在 reader 路径直接执行；Run 没有进程级单运行锁，Reclaim 只传入新生成的 run ID；SandboxSpec.AllowHosts 可由调用者填写；Probe 只做镜像检查；若 Planner/Renderer/Gate 缺失，题目会无轮次返回；墙钟与成本预算、一次重启、事实型停滞检测、cleanup 错误记录均未完成。以上均属路线图 P0，不应标记为已满足。

## 4. 数据与结果语义

- DAG 的事实层只记录可追溯的目标、服务、凭据线索和负面事实；答案层由 Gate 单独管理。answer.Fingerprint 是唯一指纹格式源。
- RunResult 是内存返回值，可能包含 Outcome.Flags；ResultFileStore 用专门的公开结构序列化，避免把整个返回值写盘。
- 目标公开结果位于 &lt;ResultDir&gt;/results/&lt;runID&gt;.json，只含指标与错误类别。原始 trace、证据和候选若需要持久化，应进入权限为 0700/0600 的 private/；当前 v0.4 私密 trace 持久化尚未接通。
- 比较通过率时，应记录**起跑时剩余 flag 数**与本次新增确认数。当前聚合把最终累计进度当作本次确认量，RemainingAtStart 等字段未填，因此现有 Stats 不能用于通过率结论。
- SolverProfile 的 prompt、只读扩展、Planner 参数和提示策略应在一次 Run 中冻结并以摘要标识。当前 RunResult.ProfileDigest 使用构造 Harness 时的 profile，运行时又读取 RunSpec.Profile 的 bundle；需统一为一份来源。

## 5. 当前状态与验收口径

截至本文更新，go test ./... -count=1 通过。它证明现有包测试可编译并运行，**未证明**同步 Harness 的真实 Docker 纵向闭环、CLI 可用、默认拒绝出站、取消清理或线上通过率。下一步先完成 Fake + stub pi 的容器内全生命周期，再按 [roadmap.md](roadmap.md) 逐项封闭 P0。
