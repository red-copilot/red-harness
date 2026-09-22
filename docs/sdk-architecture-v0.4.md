# v0.4 Offensive Security Harness SDK 架构设计

## 交付物

新增 `docs/sdk-architecture-v0.4.md`，作为研究版 SDK 的目标架构规范。文档包含模块依赖图、单题运行时序图、公开接口、信任边界，以及现有实现与目标设计的差距表；现状和交付进度继续由现有架构文档与路线图记录。

## 架构

- **宿主控制面：** `Harness.Run` 串行处理题目；`Scenario` 独占平台生命周期与权威进度；DAG 负责事实和意图，Gate 负责候选来源与去重。Agent 事件进入有界队列，由单一消费者更新状态，轮末设置消费屏障。
- **隔离执行面：** 每题创建一个 Docker `SandboxSession`，pi 是容器主工作进程，工具在同一容器运行。目标 IP:port 仅从 `Scenario.Prepare` 生成；模型流量经宿主域名白名单代理。文档明确 Docker 宿主可信、同一 bridge 内流量的现有边界。
- **结果面：** 公开结果只含进度、预算、错误类别、配置摘要和指纹；候选明文与原始 trace 进入受限私密目录。一次 Run 冻结 Profile 及扩展包内容摘要，供执行和指标分组使用。
- **生命周期：** 明确取消优先级、预算检查、候选提交、结果不确定时对账、最多一次 Agent 重启，以及独立限时清理的顺序。

## 目标公开接口

- 保留同步 `NewHarness`、`Harness.Run(ctx, RunSpec)`、`Doctor` 和独立 `ResultStore`；构造时要求所有生产必需端口齐备。
- 将 `RunSpec` 限定为运行意图与资源上限；运行目录和凭据来源归入装配配置。`SandboxSpec` 由宿主解析目标后生成，调用者不能填写目标白名单。
- 将 Gate 的可提交候选统一为显式接口，覆盖 `observed` 与 `derived`，并用通用 `Evaluation` 回填判定；移除当前可选 `NewAll` 类型断言。
- 将运行终态 `finished / failed / cancelled` 与题目是否解出分别表示。研究版允许上述破坏性接口调整；旧图读取格式和既有 `Reason` 字符串仍按仓库契约处理。

## 验收场景

设计文档把以下场景列为实现验收门：Fake + stub pi 的真实容器闭环；目标与非目标网络可达性；凭据及候选明文 canary；正常、取消和故障后的资源回收；候选去重与提交后对账；预算和增量召回率口径。授权 TSecBench 冒烟单列为发布门，不由离线测试代替。

**假设：** v0.4 面向明确授权的 CTF、TSecBench 和本地靶场；运行环境为 Linux + Docker、单机单用户、单 Agent、题目串行、文件结果后端。
