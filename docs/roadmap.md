# red-harness Roadmap（v0.4.0-research 收尾）

> 更新：2026-09-22。当前实现见 [architecture.md](architecture.md)，目标行为见 [PLAN v0.4](PLAN%20v0.4.md)。适用范围仅为明确授权的 CTF、TSecBench 和本地靶场。

## 目标与发布边界

v0.4 的目标是一个可复现的单 Agent 研究 SDK：同步 `Harness.Run`、每题一个 Docker sandbox、可信宿主平台控制面、可解释的候选判定和可比较的公开指标。部署范围是 Linux、Docker、单机单用户、题目串行和文件结果后端。通过率是研究指标；隔离、停止、清理、凭据保密和指标口径是发布门槛。

截至 2026-09-22，CLI 已接线为 `doctor/list/run/stats`；装配层、默认跨进程单运行锁、Fake/TSecBench Scenario、Docker attached session、pi factory、DAG/Gate、文件 ResultStore 和离线假件全生命周期测试均已落地。`go test ./... -count=1` 通过。该测试尚未证明 `CLI → Harness.Run → 真实 Docker → pi → Evaluate → Cleanup` 的纵向闭环。现有 Docker 集成测试主要覆盖旧 `Executor` 入口，不能代替新同步入口的验收。

## 交付阶段

| 阶段 | 优先级 / 状态 | 交付范围 | 可观察出口门 |
|---|---|---|---|
| M1 离线真实容器闭环 | P0 / 待验收 | Fake + stub pi 经 CLI、同步 Harness 和真实 SandboxSession 完成一题 | pi 与工具同处目标容器；提交由 Fake 确认；正常、取消和启动失败后资源零遗留 |
| M2 上线前隔离与凭据门 | P0 / 未通过 | 凭据传递、镜像内版本核验、网络与文件系统隔离 | canary 不进 argv/公开输出；版本可核验；未授权端点不可达；隔离测试全绿 |
| M3 运行可靠性 | P0 / 未完成 | 有界事件、真实预算、平台对账、恢复策略与清理 | 故障注入得到确定的题级终态；取消后仍保存安全结果；无残留资源 |
| M4 指标与发布验收 | P0/P1 / 未完成 | 冻结 profile、私密 trace、指标修正、真实 pi 与授权平台冒烟 | 指标可重算；公开面无明文；全部合并门与发布门通过 |

下列工作按依赖与验收门槛排序；阶段状态以出口门是否通过为准，不以代码存在或单元测试通过为准。

## M1：离线真实容器闭环

- 用 Fake 场景和 stub pi 从 CLI `run` 走 `Discover → Prepare → NewSession → Launch → Round → Evaluate → Reconcile → Close → Cleanup → Save`。stub pi 回报自身及工具的容器身份，测试断言二者处于同一目标容器。
- 为 `SandboxSession` 和同步 `Harness.Run` 增加真实 Docker 集成测试，覆盖只读 rootfs、有界 tmpfs、非 root、资源上限，以及正常完成、SIGINT 和启动失败后的容器、网络、代理和规则回收。旧 `Executor` 集成测试保留为兼容回归，但不计入本阶段的新入口出口门。
- 一条离线命令应产出 Fake 平台确认的结果及可读取的公开指标；失败时保留明确失败分类和清理证据。Fake 成绩只证明编排接线，不证明真实解题能力。

## M2：上线前隔离与凭据门

- 消除 provider key 经 `ProcessSpec.Env → docker run -e KEY=value` 出现在宿主进程参数的路径。用固定 canary 检查 Docker 命令参数、CLI、日志和公开结果；平台 token 继续只留在宿主 bridge。
- `SandboxSession.Probe` 必须从运行所用镜像取得 `ProbeResult.PiVersion` 并核对支持区间；空值或无法核验时拒绝进入真实平台运行。镜像 tag、digest 和实测版本应可追溯。
- 从 `Scenario.Prepare` 的目标生成 IP:port 白名单，测试目标可达、非目标、宿主监听端口及公网默认不可达，provider 仅经白名单代理可达。明确记录同一 Docker bridge 内流量不经过当前 iptables 规则的边界，避免将其误报为已隔离。
- 任一凭据、版本或隔离检查失败，即停止在离线阶段，不执行授权平台冒烟。

## M3：运行可靠性

- 将当前无界 `eventSink.events` 和同步回调改为有界事件队列，由一个编排消费者更新 DAG、Gate 与进度；限制重复、超大和不可信事件，并验证背压与取消不会死锁。
- 每轮从 Agent 权威统计读取 turns 与成本，连同墙钟和轮数检查预算；取消优先于 provider 故障分类。停滞以「无平台进度且无新增宿主验证事实」判定：两轮后每题至多请求一次提示，再次停滞切换未尝试意图。
- observed/derived 候选可提交，fabricated 阻断；错误候选单 Run 不重提。submit/start/close 结果不确定时先按平台权威状态对账，确认未生效后才重试。可重试 provider/进程故障最多重启 Agent 一次，仅回灌脱敏事实摘要。
- Agent、Sandbox 和 Scenario 清理使用独立的有界 context，错误单独记录而不覆盖主终态；取消后仍以有界 context 保存安全的公开结果。故障注入覆盖写超时、重复提交、Agent 崩溃、清理失败和宿主异常退出后的遗留回收。

## M4：指标与发布验收

- 以每次 Run 冻结的有效 `RunSpec.Profile` 同时驱动 system prompt、只读 extension bundle、Planner/提示参数及 `ProfileDigest`；`HarnessOptions.Profile` 仅提供默认值。生产 `Harness` 必须有跨进程 `RunLocker`，直接使用 SDK 与 CLI 装配遵循同一约束。
- 公开结果记录每题起跑剩余量、目标总量、本次新增确认、完成状态、成本、耗时、轮次、提示与失败类别；修正 `Kind` 分类被 `ResultFileStore` 归为 `unclassified` 的路径。原始 trace 和候选仅进入权限为 `0700/0600` 的 `private/`。
- 主指标为挑战完成率及「本次新增确认 / 起跑时剩余」召回率；分母未知时明确标注不可计算。跨题目集合只报描述性累计数据，直接比较只使用重叠的 challenge/profile/model。
- 通过新同步入口的 Docker 集成门与真实 pi 最小冒烟后，才在明确授权且 VPN 连通的 TSecBench 环境完成至少一题 `list → prepare → solve → submit → cleanup`。若授权环境不可用，保持发布门未通过，不用 Fake 或旧入口测试替代。

## 验收与证据

每次合并：

~~~bash
gofmt -l .
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
~~~

Docker 门运行 `go test -tags integration ./executor/... -count=1`，并纳入 M1 新增的同步 Harness 真实容器测试。每阶段记录测试命令、通过或失败结果、容器/网络/代理/规则回收证据与尚未验证的性质；跳过的集成测试不算通过。发布门另核对凭据 canary、真实 pi 和授权平台冒烟。通过率未设绝对阈值，不替代上述门槛。

## 后续边界

Web、公开报告、崩溃续跑、远程 worker、多 Agent、数据库和通用真实资产不进入 v0.4。若扩展到一般授权资产，另立阶段设计签名授权清单、目标与时窗验证、动作策略和审计。
