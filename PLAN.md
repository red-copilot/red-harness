# 进攻性安全 Harness SDK v1 实施计划

## 总结

构建面向安全 Agent 开发者的 Go SDK、CLI 与本地看板。v1 仅接入 [TSecBench](https://tsecbench.zc.tencent.com/resources)，通过官方 Python SDK bridge 完成题目生命周期；内核使用通用 `Run/Target/Objective/Candidate/Evaluation` 模型，为后续授权渗透、源码和云原生场景保留扩展点。

允许对 v0.2.0 做破坏性 API 整理，但复用现有已通过测试的 DAG、候选证据闸和 pi RPC 实现。v1 为单机、单用户、题目串行执行。

明确不纳入 v1：真实资产签名授权校验、逐动作策略拦截、人工审批流、多用户/RBAC、远程 worker、多 Agent 后端。

## 公共接口与数据模型

- 以 `Engine.Start(ctx, RunSpec)`、`Engine.Resume(ctx, RunID)` 替换现有 `Session.Run`；返回 `RunHandle`，提供 `Snapshot`、`Events`、`Pause`、`Resume`、`Cancel`、`Wait`。
- `RunSpec` 固定包含场景、目标选择、Agent、Docker、预算、提示策略、存储和运行级策略配置；创建后计算摘要，恢复时拒绝配置漂移。
- 通用模型使用：
  - `Target`：授权目标与端点。
  - `Objective`：成功条件。
  - `Candidate`：结果种类、私密值、证据引用和来源类别。
  - `Evaluation`：accepted、progress、completed、score 与平台消息。
  - `RunState`：`created/preparing/running/paused/completed/failed/cancelled`。
- `Scenario` 提供 `Discover/Prepare/Hint/Evaluate/Reconcile/Cleanup`；TSecBench 适配器内部映射 challenge、container、flag 和平台进度。
- 可替换端口包括 `AgentFactory`、`Executor`、`Planner`、`CandidateGate`、`RunPolicy`、`Store`、`EvidenceStore`。v1 仅提供 pi Agent、Docker Executor 和文件 Store。
- 统一错误携带 `Kind/Op/Retryable/RunID/Cause`，区分配置、范围、平台、provider、执行器、预算、持久化和取消。
- 事实与答案继续严格分离；候选明文不得进入 DAG、普通事件、看板或报告。

## 实施里程碑

1. **设计归档与内核 API**
   - 将已确认设计写入 `docs/superpowers/specs/2026-09-20-offensive-security-harness-design.md`，并生成对应实施计划文档。
   - 建立新 Engine、通用类型、状态机和 typed error；移除一次性 `Solver` 门面。
   - 迁移现有 DAG、gate、piai 测试到新接口，保持 provenance、轮末提交、预算和 provider 故障护栏。
   - 版本提升到 v0.3.0，并提供 v0.2 `Session/Platform/Outcome` 到新 API 的迁移表。

2. **单写者控制器、存储与恢复**
   - Engine 成为唯一状态写入者；所有状态变化先追加带单调序号的领域事件，再原子保存快照。
   - 每个运行目录包含 `run.json`、`graph.json`、`events.jsonl`、`private/`、`report.json` 和 `report.md`。
   - `private/` 使用 `0700/0600`，保存原始证据和候选明文账本；公开文件只保存指纹、哈希和脱敏片段。
   - 暂停时 abort 当前 pi round，将 intent 标记为 `interrupted` 并落盘；恢复后根据已知事实重新调度，不假定中断动作成功。
   - 恢复以快照的 `lastAppliedSeq` 为基线重放事件；配置摘要不一致、私密账本缺失或 schema 不支持时 fail closed。
   - 运行进程创建权限为 `0600` 的 Unix control socket，供其他 CLI 进程执行 pause/resume/cancel。

3. **TSecBench 场景适配器**
   - Go 监管常驻 Python JSONL bridge，wire 命令固定为 `check_vpn/list/start/hint/submit/close`，带 request ID、deadline 和结构化错误。
   - bridge 仅包装仓库 [SDK 接入文档](/root/red-harness/SDK_API.md:1) 描述的官方 SDK；token、base URL 和 flag 不进入普通日志。
   - `Reconcile` 对账题目进度与容器状态；平台写操作失败后必须先对账，再决定重试，禁止盲目重复 start/submit/close。
   - 重复 flag 视为已确认；平台权威进度决定提前结束。默认提示策略保持连续三轮无进展后最多请求一次。
   - 单题结束、取消和异常退出都执行 cleanup；cleanup 错误单独记录，不覆盖主要终止原因。

4. **Docker 隔离执行器与 pi 接入**
   - 创建专用 runner 镜像并记录基础镜像 digest；pi、Node 和安全工具全部在容器内运行。
   - 容器使用非 root、只读 rootfs、tmpfs、capability 全移除、`no-new-privileges`、独立 PID/IPC，以及 CPU、内存、PID 和墙钟限制。
   - 每次运行创建独立 bridge 网络：目标流量仅允许到 TSecBench 返回的 IP:port；provider 流量通过宿主侧域名白名单代理；其他出站默认拒绝。
   - 只挂载本题工作目录；运行状态、token 和私有证据不挂载进容器。
   - 以 run label 管理容器和网络，正常结束、取消、宿主重启恢复时均可精确回收。
   - v1 不实现 `tool_call` 级拦截或人工审批；隔离和网络边界是最终约束。

5. **CLI、看板与报告**
   - CLI 提供 `doctor`、`list`、`run`、`resume`、`pause`、`cancel`、`serve`、`report`。
   - `doctor` 检查 Python SDK、凭证、VPN、Docker、runner 镜像、pi 版本和 provider 配置，任何缺失均在平台写操作前失败。
   - 看板采用 Go embed 静态资源，通过快照和支持 `Last-Event-ID` 的 SSE 展示 DAG、当前 intent、预算、事实、候选分族和错误。
   - Web 服务默认仅监听 `127.0.0.1`，使用随机 bearer token；只允许 pause/resume/cancel，不允许注入命令或编辑事实。
   - 报告包含得分、预算、终止原因、平台进度、候选来源和证据引用；flag、凭证及原始工具输出保持脱敏。
   - 提供 fake TSecBench + stub pi 的一键本地示例，以及真实 VPN 环境的运行说明。

## 测试与验收

- 单元测试覆盖状态转换、预算边界、DAG 调度、候选 provenance、脱敏、事件序号、错误分类和配置摘要。
- 契约测试覆盖所有可替换端口，以及 Python bridge 的字段映射、请求关联、超时、崩溃重启和平台错误码。
- Docker 集成测试验证只读文件系统、资源限制、目标端点可达、非授权端点不可达、provider 代理可用及容器回收。
- 端到端测试覆盖 list→start→solve→submit→close、重复候选、错误提交、自动提示、暂停恢复、进程崩溃恢复和最终 cleanup。
- 看板测试覆盖 SSE 重连、控制鉴权、已结束运行的只读展示和报告脱敏。
- 必须通过 `go test ./...`、`go test -race ./...`、`go vet ./...`；Docker 测试通过显式 integration tag 运行。
- 真实 TSecBench 冒烟测试仅在显式提供 token/base URL 且 VPN 连通时启用；验收要求至少完成一道受控题目的完整生命周期。

## 假设与默认值

- Linux + Docker 是 v1 唯一受支持运行环境；挑战默认串行。
- 默认预算沿用现值：40 rounds、30 分钟、600 tool turns，模型成本默认不限但可配置。
- 文件存储是唯一 v1 后端；不实现数据库、遥测上传和自动证据清理。
- 旧 DAG schema 继续可读，但缺少新 `run.json` 的历史目录不能直接恢复为 Engine run。
- 当前目录不是 Git 仓库；实施不得擅自初始化仓库或创建提交。
- 计划按 `brainstorming` 与 `writing-plans` 技能拆成可独立测试的里程碑；实施开始前先完成书面设计归档与复核。
