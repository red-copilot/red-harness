# red-harness Roadmap（v0.4.0-research）

> 更新：2026-09-21。架构与真实状态见 [architecture.md](architecture.md)，v0.4 目标行为见 [PLAN v0.4](PLAN%20v0.4.md)。本文只规划明确授权的 CTF、TSecBench 和本地靶场。

## 目标与发布边界

v0.4 的目标是一个可复现的单 Agent 研究 SDK：同步 Harness.Run、每题一个真实 Docker sandbox、可信宿主调用平台、可解释的候选判定和可比较的公开指标。单机单用户、题目串行、文件结果后端。公开发布前必须证明隔离、停止、清理、结果保密和指标口径；测试通过率是研究指标，不替代这些门槛。

当前工作区已有根包同步契约、Docker attached session、pi factory、Fake/TSecBench Scenario、DAG/Gate 和文件 ResultStore 初版；go test ./... -count=1 通过。CLI 仍是 v0.3 骨架，真实 sandbox 纵向闭环和关键安全性质尚未验收。

## 交付阶段

| 阶段 | 优先级 / 状态 | 交付范围 | 可观察出口门 |
|---|---|---|---|
| M0 契约收敛 | P0 / 进行中 | 冻结 RunSpec、SolverProfile、SandboxSession、结果 schema；清理 v0.3/v0.4 双入口的含糊处 | SDK 示例能只用 v0.4 端口编译；版本与结果字段含义固定；无凭据字段进入可序列化规格 |
| M1 真实隔离闭环 | P0 / 待验收 | 装配 Fake → Docker → stub pi → DAG/Gate → Evaluate → Cleanup；CLI 改为 doctor/list/run/stats | stub pi 与工具证明在同一容器内；一条命令完成离线题目全生命周期；正常、取消、失败后零遗留 |
| M2 运行控制与候选可靠性 | P0 / 未完成 | 宿主解析 scope、有界事件队列、单运行锁、预算、候选去重、平台对账、提示/换支、一次 Agent 重启 | 未授权端点不可达；重复/不确定提交不误判；墙钟、轮数、turns、成本受限；故障注入能确定终态 |
| M3 指标与研究循环 | P1 / 初版 | profile 冻结、私密 trace、公开结果、stats 聚合和对照口径 | 起跑剩余量与本次增量可重算；同 profile/model/challenge 可对照；公开文件与 CLI 无 canary 明文 |
| M4 发布验收 | P0 / 未开始 | Docker 安全集成、真实 pi 最小冒烟、TSecBench 授权环境冒烟、打包和操作说明 | build/vet/test/race 全绿；隔离、网络、权限、清理验收全绿；真实场景失败明确记失败 |

## 近期工作包：按依赖顺序

### 1. 冻结边界与输入

- RunSpec 只描述题目、Agent、预算、profile 和结果路径；目标白名单由 Scenario.Prepare 的目标经宿主解析产生，拒绝调用者覆盖或扩大。
- SolverProfile 在 Run 开始时冻结，摘要同时驱动挂载、prompt、结果分组；避免 Harness profile 与 RunSpec.Profile 不一致。
- NewHarness 要求 Planner、Renderer、Gate、ResultStore 等生产必需端口齐备；缺失时启动失败，不能静默把空轮次当作成功。
- 定义 RunResult.Completed 为题目目标完成或明确区分 RunFinished 与 ChallengeSolved，避免“运行无错误”等同“解题成功”。

### 2. 跑通真实 sandbox

- 用 Fake 场景和 stub pi 做一条完整链路：Discover → Prepare → NewSession → Launch → Round → Evaluate → Reconcile → Close → Cleanup → Save。
- Probe 在同一镜像中核验 pi 版本与运行位置；Launch 只允许一个 attached 主进程，工具随容器停止。
- 启动前取得跨进程单运行锁，按固定 label 清理上次崩溃遗留资源；不能只按新 run ID 回收。
- CLI 切到同步 API，仅暴露 doctor/list/run/stats；SIGINT 经 context 触发清理。

### 3. 完成运行策略

- Agent 事件进入**有界**队列，由同一个编排消费者更新 DAG、Gate 和进度；对重复、过大或不可信事件设置限额。
- 轮次前后检查 ctx、墙钟、rounds、tool turns 与成本；取消优先于 provider 故障分类。
- Gate 中 observed/derived 可提交，fabricated 阻断；错误候选本 Run 不重提。平台写入超时后先 Reconcile，能确认状态前不重发。
- 两轮无**平台进度且无新增宿主验证事实**才请求一次提示；再次停滞切换未尝试意图。provider/进程可重试故障至多重启 Agent 一次，回灌脱敏摘要。
- Cleanup 的错误单独记录；取消后的清理使用有界独立 context，所有资源回收路径可幂等重试。

### 4. 修正研究指标

- 每题记录起跑时已解数、目标总数、本次新增确认数、完成状态、得分、成本、耗时、轮次、提示和失败类别。
- 主指标：挑战完成率与“本次新增确认 / 起跑时剩余”召回率。不同题目集合只能给描述性累计数据；直接比较只使用重叠 challenge/profile/model。
- 公开 result.json 只存指标、指纹和配置摘要；原始 trace 与候选只存权限受限的 private/。用固定 canary 检查所有公开输出与 argv。

## 跨阶段验收

每次合并：

~~~bash
gofmt -l .
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
~~~

Docker 集成门另跑 go test -tags integration ./executor/... -count=1，并增加同步 Harness 的真实容器测试。验收必须覆盖非 root/只读 rootfs、有界 tmpfs、CPU/内存/PID/墙钟限制、目标 IP:port 白名单、provider 代理、宿主端口与公网默认拒绝、宿主目录及 Docker socket 不可见。正常结束、SIGINT、Agent 崩溃、平台超时和宿主意外退出后，容器、网络、代理与防火墙规则均不得残留。

## 后续边界

Web、公开报告、崩溃续跑、远程 worker、多 Agent、数据库和通用真实资产不进入 v0.4。若扩展到一般授权资产，先另立阶段完成签名授权清单、目标和时窗验证、动作级策略、审计与默认拒绝，再扩展执行范围。
