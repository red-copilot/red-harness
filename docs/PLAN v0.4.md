# red-harness v0.4 研究型 Offensive Security Harness

> 更新：2026-09-22。本文描述目标行为；当前实现与验收状态见 [architecture.md](architecture.md)，按出口门排序的交付路线见 [roadmap.md](roadmap.md)。CLI 四个子命令、装配层、默认跨进程锁和离线假件测试已落地；真实同步 Docker 闭环尚未验收。

## 总结

将项目从“发布级、可恢复的单机产品”调整为“单 Agent、通过率优先的 CTF/本地靶场研究 Harness”。

v0.4 直接做破坏性升级：不实现 v0.3 尚未落地的暂停恢复、control socket、Web、公开报告和事件重放；保留已经验证过的 bridge、DAG、候选 gate、pi RPC 与 Docker 隔离能力，优先打通真实求解闭环。

同时修正当前最关键的架构断链：pi 必须作为 Docker sandbox 的主进程运行，Agent 的所有工具调用天然发生在容器内，不允许宿主 `exec.Command(pi)` 成为生产路径。

## 目标架构

```mermaid
flowchart LR
    CLI["CLI<br/>doctor · list · run · stats"] --> H["Synchronous Harness"]
    H --> SC["Scenario<br/>TSecBench / Fake"]
    H --> PL["DAG Planner + Renderer"]
    H --> GT["Candidate Gate"]
    H --> RS["Result Store"]

    SC --> BR["Python SDK Bridge<br/>宿主可信控制面"]
    H --> SB["Docker Sandbox Session"]
    SB --> PI["pi 主工作进程<br/>Docker sandbox"]
    PI --> TOOLS["安全工具<br/>同一容器"]
    TOOLS -->|"事件 / 候选"| H

    SB -->|"仅目标 IP:port"| TARGET["CTF / 本地靶场"]
    SB -->|"域名白名单代理"| PROVIDER["模型 Provider"]

    RS --> PUB["result.json<br/>指标、指纹、配置摘要"]
    RS --> PRIV["private/<br/>原始 trace、证据、候选明文"]
```

核心约束：

- Harness 使用同步 `Run`，一道题一个 sandbox、一个 pi 会话，题目串行执行。
- Harness 的事件消费保持单写者；Agent 事件只向有界 channel 推送，由编排消费者更新状态。
- Docker sandbox 创建独立网络，并以有上限的 tmpfs 提供 `/work`、`/tmp` 和 Agent HOME；不再 bind mount 可写宿主工作目录。
- pi 作为容器的唯一主工作进程启动，stdin/stdout 直接承载 RPC；Docker `--init` 负责 PID 1 的子进程回收，停止容器须终止 pi 及工具子进程。
- 平台 token 只存在于宿主 bridge；模型凭据仅注入 sandbox 主进程环境，不写入 RunSpec、argv、日志或结果。
- `Scenario.Reconcile` 保留，但只用于运行中不确定的平台写结果；不提供进程崩溃恢复。
- 启动新 Run 前取得全局单运行锁，并按 label 回收上次崩溃遗留的容器、网络和规则。
- 生产 `Harness` 必须具备跨进程 `RunLocker`；直接使用 SDK 与 CLI 装配遵循同一约束。

## 公共接口与行为变更

- 新 SDK 标识为 `v0.4.0-research`，不承诺 v0.3 API 兼容；旧 Engine/RunHandle 当前仍留在源码中供旧图 schema 和测试使用，CLI 已切换到同步入口。
- 用同步入口替代 `Engine.Start/Resume` 和 `RunHandle`：
  - `Harness.Run(ctx, RunSpec) (RunResult, error)`
  - `Harness.Doctor(ctx) DoctorReport`
  - 结果查询和聚合由独立 `ResultStore` 提供。
- v0.4 入口不提供 `Snapshot` 重放、pause/resume/cancel、Unix control socket、SSE、Web 和报告接口；CLI 已收敛为 `doctor/list/run/stats`，运行中取消使用 `SIGINT`/context。
- 将 `Executor` 改造成真正承载 Agent 的 `Sandbox`：
  - `NewSession(ctx, SandboxSpec) (SandboxSession, error)`
  - `SandboxSession.Probe` 在运行所用镜像中执行版本检查，返回非空 `ProbeResult.PiVersion`；无法核验时拒绝真实平台运行。
  - `SandboxSession.Launch(ProcessSpec)` 只允许启动一个附着式主进程，并返回可读写、可等待、可强杀的 `ManagedProcess`。
  - `SandboxSession.Close` 幂等回收容器、网络、代理和防火墙规则。
- `AgentFactory` 必须接收 `SandboxSession`；`piai` 只负责构造 pi argv 和 RPC，不再自行选择宿主二进制。
- `RunSpec` 包含 scenario、targets、Agent、Sandbox、Solver Profile、预算、是否提交及结果目录；移除用户可直接关闭隔离或自行填写目标白名单的字段。
- Solver Profile 使用 JSON 描述并计算摘要，包含 system prompt、只读 extension bundle、Planner 参数和提示策略；profile 资源只读挂载到 sandbox。每次 Run 以有效 `RunSpec.Profile` 冻结这一整套配置，`HarnessOptions.Profile` 仅作为默认值。
- 默认候选策略：
  - `observed` 与 `derived` 均直接提交，以召回率优先。
  - `fabricated` 不提交。
  - 同一候选在单次 Run 内只提交一次；平台 duplicate 视为已确认。
- 连续两轮既无平台进度、也无新增宿主验证事实时定义为停滞；自动请求一次提示。提示后再次连续两轮停滞则放弃当前分支，调度未尝试 intent。
- retryable provider/进程故障允许重启 Agent 一次，并用 DAG 中的脱敏事实摘要恢复上下文；再次失败则结束当前题目并继续下一题。
- submit/start/close 出现结果不确定时先 `Reconcile`，确认未生效后才重试，禁止盲目重发。

## Roadmap

阶段以 [roadmap.md](roadmap.md) 的出口门为准，不按日期或代码存在与否判定完成：

1. **M1：离线真实容器闭环，P0。** Fake + stub pi 经 CLI 和同步 Harness 跑通真实 SandboxSession；证明 pi/工具同容器，正常、取消和启动失败后零遗留。
2. **M2：上线前隔离与凭据门，P0。** 移除 provider key 进入 `docker run` 参数的路径，核验镜像内 pi 版本，证明目标 IP:port 白名单及默认拒绝，并记录同桥网络边界。任一项失败不得进入真实平台冒烟。
3. **M3：运行可靠性，P0。** 有界事件队列、真实成本与 turns 预算、Reconcile-before-retry、一次 Agent 重启、停滞换支、有界清理和取消后结果保存；故障注入须产生确定终态。
4. **M4：指标与发布验收，P0/P1。** 冻结有效 profile，修正错误类别落盘，接通私密 trace，核验增量召回率；新同步入口 Docker 测试、真实 pi 和授权 TSecBench 冒烟全部通过后才可宣布 v0.4 完成。

Web、公开报告、崩溃续跑、远程 worker、多 Agent、数据库、通用真实资产和动作级审批继续延后；若进入产品化，另立阶段设计签名 scope、动作策略、持久化恢复和多用户控制面。

## 测试与验收

- 契约测试证明生产 `piai` 无宿主进程启动路径，AgentFactory 缺 SandboxSession 时构造失败。
- 新同步入口的 Docker 集成测试验证 pi 和工具都在容器内、rootfs 只读、工作区容量有界、CPU/内存/PID/墙钟限制生效、宿主目录及 Docker socket 不可见；现有旧 `Executor` 测试不代替这一门槛。
- 网络测试验证仅授权目标和 provider 代理可达，其他目标、宿主监听端口及公网默认不可达。
- 生命周期测试覆盖正常完成、SIGINT、Agent 崩溃、平台错误和 cleanup 错误；所有路径均无遗留容器、网络或规则。
- 求解测试覆盖两轮停滞后只请求一次提示、observed/derived 提交、fabricated 不提交、duplicate 确认、单 Run 去重以及 Reconcile-before-retry。
- 泄漏测试用固定 canary，确保平台 token、模型 key 和候选明文不出现在公开结果、CLI 输出、日志或 Docker 命令参数；原始 trace 仅位于 `private/` 且权限为 `0700/0600`。
- 指标测试验证部分进度分母、profile digest 分组和累计过滤。
- 合并门：`gofmt -l .`、`go build ./...`、`go vet ./...`、`go test ./... -count=1`、`go test -race ./... -count=1`；Docker 性质另跑 integration tag，跳过不计为通过。每阶段记录命令、结果与失败证据。
- 最终冒烟是在明确授权且 VPN 连通的 TSecBench 环境完成至少一道题的 `list→prepare→solve→submit→cleanup`，并确认所有 Agent 工具活动来自 sandbox。授权环境不可用时发布门保持未通过。

## 假设与默认值

- 仅支持 Linux、Docker、单机单用户和单 Run；题目串行。
- 场景仅限 TSecBench、fake 和显式本地 CTF 靶场。
- 默认预算沿用 40 rounds、30 分钟、600 tool turns；提示阈值改为两轮停滞且每题最多一次。
- 文件系统是唯一结果后端；公开结果只存指标与指纹，原始研究 trace 不自动清理。
- 保留零第三方 Go 依赖、答案/事实分离、`answer.Fingerprint` 唯一真源、pi RPC 传输层和现有 bridge 映射契约。
- v0.4 的发布门是可信闭环、隔离真实性和指标完整性，不预设绝对通过率；通过率优化依据后续线上累计数据迭代。
