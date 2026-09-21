# red-harness Roadmap

> 目标版本：v1（当前实现版本 v0.3.0）  
> 更新日期：2026-09-21  
> 配套架构：[architecture.md](architecture.md)  
> 详细施工任务：[2026-09-20-red-harness-v0.3.0.md](superpowers/plans/2026-09-20-red-harness-v0.3.0.md)

## 1. 产品目标

red-harness 是面向**已授权安全任务**的本地编排内核。它负责把目标范围、Agent、隔离执行、候选验证、平台生命周期、恢复和审计组合成一个可复现的 Run。

v1 的成功标准不是“Agent 能执行命令”，而是同时满足：

1. **可控**：目标与出站范围由宿主侧强制执行，Agent 不能自行扩大范围。
2. **可停**：预算、超时、暂停、取消和 provider 故障能可靠终止执行。
3. **可恢复**：进程崩溃后可以重放；恢复不会盲目重做平台副作用。
4. **可审计**：每次状态变化有单调事件序号和来源链。
5. **不泄密**：候选明文、凭据和原始证据不进入公开事件、报告或看板。
6. **可替换**：Engine 只依赖端口，场景、Agent、执行器和存储均可替换。

## 2. 范围边界

### v1 纳入

- Linux 单机、单用户、题目串行执行。
- TSecBench 场景与 fake 场景。
- pi Agent、Docker Executor、文件 Store。
- CLI、本地只读看板、脱敏报告。
- 进程崩溃恢复、暂停/继续/取消、资源回收。
- 目标 `IP:port` 白名单、provider 域名代理、默认拒绝出站。

### v1 不纳入

- 通用真实资产攻击、签名授权文件和资产所有权验证。
- 每个 tool call 的策略审批或人工审批流。
- 多用户、RBAC、远程 worker、分布式调度。
- 固定角色的多 Agent 编排。
- 云控制面、数据库后端和遥测上传。

因此，v1 只能用于明确授权的 TSecBench/本地 fake 环境。把它扩展到一般网络目标之前，必须先完成本路线图的 R4 授权与策略阶段。

## 3. 优先级原则

路线图按风险而不是界面完整度排序：

1. 先接通**最小可信闭环**：Engine → Scenario → Executor/Agent → Gate → Store。
2. 再证明**中断、恢复和清理**正确；这些路径出错会重复提交、失控运行或遗留容器。
3. 然后交付报告、看板和示例。
4. 最后才扩展场景、并发和远程执行。

任何阶段都不能以“单元测试全绿”代替纵向闭环；每一阶段都有独立的出口门。

## 4. 路线图

| 阶段 | 目标 | 主要交付 | 出口门 |
|---|---|---|---|
| R0 基线（已完成） | 冻结契约和叶子适配器 | 根包契约、DAG、Gate、Store、Docker Executor、Python bridge、CLI 骨架 | `go test ./...` 全绿；已对齐的适配器有编译期契约断言 |
| R1 可信纵向闭环（P0） | 让一次 Run 真正可执行、可控制、可恢复 | `engine/`、`scenario/`、piai 契约迁移、默认 Policy、`wire.go` | fake 场景完成 `discover→prepare→round→evaluate→cleanup`；暂停/恢复、崩溃恢复和终态拒绝均通过 |
| R2 交付面（P1） | 让结果可观察、可解释、可复现 | report、Web/SSE、离线 example、完整 CLI/doctor | 报告/看板零明文泄漏；SSE 可续传；一条命令完成离线全生命周期 |
| R3 v1 安全与发布验收（P0） | 证明约束真实生效，并建立可发布、可回滚的单机产品 | Docker 集成测试、故障注入、孤儿回收、真实 pi 最小冒烟、SBOM、运维手册、独立核验 | 未授权端点不可达；资源上限生效；无资源残留；race/vet/build/test 全绿；升级回滚演练通过 |
| R4 授权与策略（后续） | 从“题库专用”升级为可接一般授权靶场 | 签名 scope manifest、动作级 policy hook、审计导出、审批接口（默认关闭） | 无有效授权清单不得启动；目标、端口、时窗、工具和速率均能 fail closed |
| R5 扩展性（后续） | 降低新增场景与 Agent 后端成本 | Scenario conformance suite、版本化插件协议、能力协商、Store 迁移工具 | 新适配器无需改 Engine；兼容性测试可自动验证 |

### R1：可信纵向闭环

这是当前的唯一 P0。按依赖顺序实施：

1. **Engine 单写者内核**：状态机、领域事件、预算、控制命令、资源清理。
2. **恢复语义**：快照后事件重放、配置摘要校验、私密账本校验、恢复前 Reconcile。
3. **pi 适配**：实现 `harness.Agent`/`AgentFactory`，启动前版本与凭据校验，退出时杀进程组。
4. **TSecBench Scenario**：把 bridge 映射为 `Discover/Prepare/Hint/Evaluate/Reconcile/Cleanup`。
5. **装配层**：只在 `internal/cli/wire.go` 组合具体实现；Engine 不反向依赖适配器。

R1 的必须回归场景：

- 终态 Run 不能恢复。
- `private/` 缺失或损坏时 fail closed。
- 平台调用超时后，先 Reconcile，禁止盲目重试。
- `events.jsonl` 末尾半行可以修复并继续。
- 暂停中的 intent 标记为 `interrupted`，恢复时不假设动作成功。
- provider 出现“零回合 + error”时立即停止，不能继续消耗题目与预算。

### R2：交付面

- `report/` 只消费公开快照、指纹和证据引用，不读取候选明文生成公开内容。
- `web/` 默认仅监听 `127.0.0.1`，随机 bearer token；写操作只允许 pause/resume/cancel。
- SSE 使用领域事件序号作为 ID，支持 `Last-Event-ID` 增量补发。
- `example/` 使用 fake Scenario + stub pi，覆盖重复候选、错误候选、提示、暂停恢复和 cleanup。

### R3：v1 安全与发布验收

R3 不增加产品功能，只验证安全假设：

- 容器为非 root、只读 rootfs、无 capabilities、`no-new-privileges`，且 CPU/内存/PID/tmpfs/墙钟均有上限。
- 唯一可写工作区有容量上限；工具输出和单文件大小有上限，避免绕过 tmpfs 限制写满宿主。
- 目标流量只到授权 `IP:port`；provider 只经域名白名单代理；容器不能访问宿主其他监听端口。
- 取消、异常、宿主重启后都能按 run label 回收容器、网络和 iptables 规则。
- runner 基础镜像固定 digest，并产出软件清单；禁止依赖漂移的 `latest` 作为发布输入。
- 配置升级、回滚和长时间 soak 均不造成数据丢失、明文泄漏或资源残留。

这里有两个当前设计必须在发布前关闭的风险：

1. `--read-only` 与有界 `/tmp` 不能限制 bind-mounted workdir 的增长，需要 quota、专用有界卷或写入监控。
2. iptables 是关键安全边界，`ManageIptables=false` 只能作为显式的不安全调试模式，doctor 和报告必须醒目标注。

### R4：授权与动作策略

只有完成 R4，项目才可从题库扩展到一般授权测试：

- scope manifest 明确资产、端口、协议、时间窗、允许动作、速率和操作者，并由可信主体签名。
- Engine 启动前验证签名与有效期；Executor 只接受解析后的不可变范围。
- 高风险动作（破坏性写入、凭据使用、持久化、横向移动）进入动作级 policy hook；默认拒绝。
- 审批与策略结果进入公开事件，但凭据和原始 payload 仍只进私密账本。

## 5. 跨阶段质量门

每次合并必须满足：

```bash
gofmt -l .
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
```

需要 Docker 的安全性质通过显式集成门验证：

```bash
go test -tags integration ./executor/... -count=1
```

发布候选还必须满足：

- 所有适配器都有编译期接口断言。
- 所有公开产物通过固定 canary 明文的泄漏测试。
- 所有平台写操作都有“超时但已生效”的故障注入测试。
- 所有资源都有正常、取消、崩溃三条回收测试。
- 真环境冒烟失败必须记为失败，不得降级成 skipped。

## 6. 成功指标

| 维度 | v1 指标 |
|---|---|
| 安全 | 0 次越界连接；0 个凭据/候选明文进入公开产物 |
| 可恢复性 | 所有已确认候选恢复后 0 次重复提交；撕裂末行可恢复 |
| 资源治理 | 每个 Run 的 CPU、内存、PID、磁盘和墙钟都有硬上限 |
| 清理 | 正常、取消和崩溃路径均无孤儿容器/网络/规则 |
| 可审计性 | 100% 状态转换对应单调领域事件；报告可追到证据引用 |
| 可扩展性 | 新 Scenario/Agent 实现不修改 Engine 包 |

## 7. 暂不追求的指标

v1 不以挑战通过率、并发量或模型自动化程度作为首要发布门。先证明范围、停止、恢复、审计和保密正确，再优化求解效果与吞吐量。
