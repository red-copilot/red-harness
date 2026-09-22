# red-harness Roadmap（v0.4.0-research 收尾）

> 更新：2026-09-22。当前实现见 [architecture.md](architecture.md)，目标行为见 [PLAN v0.4](PLAN%20v0.4.md)。适用范围仅为明确授权的 CTF、TSecBench 和本地靶场。

## 目标与发布边界

v0.4 的目标是一个可复现的单 Agent 研究 SDK：同步 `Harness.Run`、每题一个 Docker sandbox、可信宿主平台控制面、可解释的候选判定和可比较的公开指标。部署范围是 Linux、Docker、单机单用户、题目串行和文件结果后端。通过率是研究指标；隔离、停止、清理、凭据保密和指标口径是发布门槛。

截至 2026-09-22，CLI 已接线为 `doctor/list/run/stats`；装配层、默认跨进程单运行锁、Fake/TSecBench Scenario、Docker attached session、pi factory、DAG/Gate、文件 ResultStore 和离线假件全生命周期测试均已落地。`go test ./... -count=1` 与 `go test -race ./... -count=1` 通过。**M1 的纵向闭环（`CLI → Harness.Run → 真实 Docker → pi → Evaluate → Cleanup`）已实测通过**：`go test -tags integration ./cmd/red-harness/... -count=1` 在本机 Docker 29.8 / root / runner 镜像在位时 14.3s 全绿，证明 pi 与工具同处目标容器、provider key 不进 argv、正常与取消与启动失败三条路径零遗留。旧 `Executor` 入口的集成测试保留为兼容回归。

## 交付阶段

| 阶段 | 优先级 / 状态 | 交付范围 | 可观察出口门 |
|---|---|---|---|
| M1 离线真实容器闭环 | P0 / **已通过** | Fake + stub pi 经 CLI、同步 Harness 和真实 SandboxSession 完成一题 | pi 与工具同处目标容器；提交由 Fake 确认；正常、取消和启动失败后资源零遗留 |
| M2 上线前隔离与凭据门 | P0 / **未通过**（隔离待重跑复核） | 凭据传递、镜像内版本核验、网络与文件系统隔离 | canary 不进 argv/公开输出（**已实测**）；版本可核验（**已实测**，镜像内 pi 0.85.1）；**生产装配的隔离一度整片失效（已修，待授权环境重跑复核）**；`DefaultDockerConfig()` 路径下的 25 条隔离用例全通过，但那验的是另一条构造路径 |
| M3 运行可靠性 | P0 / **已完成（离线）** | 有界事件、真实预算、平台对账、恢复策略与清理 | 故障注入得到确定的题级终态；取消后仍保存安全结果；无残留资源；**事实型停滞与换支已落地** |
| M4 指标与发布验收 | P0/P1 / **进行中** | 冻结 profile、私密 trace、指标修正、真实 pi 与授权平台冒烟 | 指标可重算（**已落地**）；公开面无明文（**已落地**）；真实 pi 与授权平台冒烟**未通过** |

下列工作按依赖与验收门槛排序；阶段状态以出口门是否通过为准，不以代码存在或单元测试通过为准。

## M1：离线真实容器闭环

- 用 Fake 场景和 stub pi 从 CLI `run` 走 `Discover → Prepare → NewSession → Launch → Round → Evaluate → Reconcile → Close → Cleanup → Save`。stub pi 回报自身及工具的容器身份，测试断言二者处于同一目标容器。
- 为 `SandboxSession` 和同步 `Harness.Run` 增加真实 Docker 集成测试，覆盖只读 rootfs、有界 tmpfs、非 root、资源上限，以及正常完成、SIGINT 和启动失败后的容器、网络、代理和规则回收。旧 `Executor` 集成测试保留为兼容回归，但不计入本阶段的新入口出口门。
- 一条离线命令应产出 Fake 平台确认的结果及可读取的公开指标；失败时保留明确失败分类和清理证据。Fake 成绩只证明编排接线，不证明真实解题能力。

**实测状态（2026-09-22，本机 Docker 29.8 / root / runner 镜像在位）**：
`go test -tags integration ./cmd/red-harness/... -count=1` 14.3s 全绿，覆盖了上面第 1、3 条与第 2 条里的**生命周期**部分——同容器身份（stub pi 与其子工具回报同一个 hostname，且等于 `Probe` 的容器 ID）、provider key 不进任何一次 docker argv、正常 / 取消 / 启动失败三条路径按 run label 查容器与网络均为空。

**隔离性质的取证在旧 `Executor` 端口上，且那批用例全绿**：只读 rootfs、有界 tmpfs、非 root、
资源上限、宿主状态不可见、未授权端点不可达等 25 条集成用例全部通过（见 M2 的实测状态）。
**本阶段新增的**同步入口用例（`cmd/red-harness`）证明的是编排闭环本身——同容器身份、
凭据不进 argv、三条路径零遗留——不重复取证隔离性质。

⚠️ 但注意这批用例的**构造路径**：它们一律用 `DefaultDockerConfig()` 造执行器，而生产装配
一度手写部分结构体、把隔离开关全落成零值（见 M2）。所以「用例全绿」不等于「装起来就隔离」——
这正是 M2 那段实测状态的由来。M1 标为「出口门已通过、隔离性质由 M2 侧的用例覆盖，且该覆盖
有待一次授权环境重跑确认」。

## M2：上线前隔离与凭据门

- 消除 provider key 经 `ProcessSpec.Env → docker run -e KEY=value` 出现在宿主进程参数的路径。用固定 canary 检查 Docker 命令参数、CLI、日志和公开结果；平台 token 继续只留在宿主 bridge。
- `SandboxSession.Probe` 必须从运行所用镜像取得 `ProbeResult.PiVersion` 并核对支持区间；空值或无法核验时拒绝进入真实平台运行。镜像 tag、digest 和实测版本应可追溯。
- 从 `Scenario.Prepare` 的目标生成 IP:port 白名单，测试目标可达、非目标、宿主监听端口及公网默认不可达，provider 仅经白名单代理可达。明确记录同一 Docker bridge 内流量不经过当前 iptables 规则的边界，避免将其误报为已隔离。
- 任一凭据、版本或隔离检查失败，即停止在离线阶段，不执行授权平台冒烟。

**实测状态（2026-09-22）**：

- **凭据**：provider key 走 0600 的 `--env-file` 而非 `-e KEY=value`（argv canary 有集成回归，实测通过）。
- **版本核验**：`Probe` 在运行所用镜像里跑 `pi --version` 并回报 `PiVersion`，空值拒绝启动。授权环境实测：镜像内 pi 为 `0.85.1`，落在支持区间 `0.85.0–0.86.99` 内。
- **⚠️ 隔离：一度整片失效，已修但**待重跑复核**。**

授权环境真跑时实测发现：sandbox 容器内**公网 `1.1.1.1:443` 可达、非授权内网地址可达、且没有任何 `*_PROXY` 环境变量**。根因在装配层：

    executor.NewDocker(executor.DockerConfig{ProviderAllowHosts: hosts})   // 旧写法

手写部分结构体 ⇒ 其余字段落零值 ⇒ 而 `DockerConfig` 零值里**所有 bool 都是 false = 关掉隔离**：`ManageIptables=false` 让 `installNetworkRules` 第一行就静默返回，`ProviderProxy=false` 让模型流量不走宿主侧域名白名单代理。`normalize()` 不补这两个开关。

**为什么集成门没拦住**：那 25 条用例一律用 `DefaultDockerConfig()` 构造执行器，验的是**另一条构造路径**。于是「未授权端点不可达」「公网不可达」在测试里全绿、在生产装配上完全失效——`DefaultDockerConfig` 的注释里恰好写着这个危险（「零值里的 bool 全是 false（= 关掉隔离），直接拿零值构造执行器是不安全的」）。

已修：装配从 `DefaultDockerConfig()` 起手再覆盖 `ProviderAllowHosts`，并在 `wire_test.go` 直接钉 `r.docker.Config()` 的两个开关（已验证断言非空转：改回旧写法会精确报出这两条）。

**复核状态**：修复后**尚未重跑授权环境**。上一次真跑用的是修复前的二进制，所以那次现场不能作为「已隔离」的证据。在重跑并实测非授权端点不可达之前，M2 的隔离出口门记为**未通过**。

补齐这条门时还发现旧 `Executor` 端口有 2 个先于本轮就存在的失败（在改动前的 `8d40807` 上逐条复现），已定位并修复：

| 用例 | 现象 | 原因 |
|---|---|---|
| `TestIntegrationReadOnlyRootfs` | 容器内写 `/` 确实被拒（退出码非 0），但 `ExecResult.Stderr` **恒为空**，断言「失败原因含 read-only」永不成立 | `Exec` 用 `stderrOf(ee)` 取 stderr，而 `runCmdStdin` 已把它收进局部 buffer——Go 只在走 `Output()` 时才填 `ExitError.Stderr`，显式设过 `cmd.Stderr` 的情况下它恒为 nil |
| `TestIntegrationWallClockTimeout` | `sleep 60` 超时后容器**没了**，后续 `echo alive` 失败 | 超时补刀是 `pkill -f -- <cmd[0]>`，对本例即 `pkill -f sleep`——它同时匹配容器的 PID 1（`sleep infinity`），杀的是容器本身 |

两者都不是环境问题：用同样的 `--read-only --user 65534:65534 -v <host>:/work
--workdir /work` 手工起容器，`docker exec` 的输出与退出码都符合预期。

**仍需授权环境**：同一 bridge 内流量不经过当前 iptables 规则的边界已有文字记录，
但「目标可达 / 非目标不可达」的真机结论只在本机 Docker 上验证过，未在真实靶场复核。

## M3：运行可靠性

- 将当前无界 `eventSink.events` 和同步回调改为有界事件队列，由一个编排消费者更新 DAG、Gate 与进度；限制重复、超大和不可信事件，并验证背压与取消不会死锁。
- 每轮从 Agent 权威统计读取 turns 与成本，连同墙钟和轮数检查预算；取消优先于 provider 故障分类。停滞以「无平台进度且无新增宿主验证事实」判定：两轮后每题至多请求一次提示，再次停滞切换未尝试意图。
- observed/derived 候选可提交，fabricated 阻断；错误候选单 Run 不重提。submit/start/close 结果不确定时先按平台权威状态对账，确认未生效后才重试。可重试 provider/进程故障最多重启 Agent 一次，仅回灌脱敏事实摘要。
- Agent、Sandbox 和 Scenario 清理使用独立的有界 context，错误单独记录而不覆盖主终态；取消后仍以有界 context 保存安全的公开结果。故障注入覆盖写超时、重复提交、Agent 崩溃、清理失败和宿主异常退出后的遗留回收。

**实测状态（2026-09-22）**：上述三条的**离线部分已全部落地**，其中停滞与换支是最后补上的一条——
判据取自 `Planner.HostFacts()`（只数宿主验证族，不数 agent 自述族），读数在轮末消费屏障之后；
提示一次后 `dryRounds` 归零重数，再次连续停滞即 `Abandon` 当前意图。`HintOff` 下不换支
（那一档的契约是「从不请求提示」，而规范里的换支是提示链的下游）。终止性由 DAG 自身保证：
前沿耗尽时 `Next` 返回 `(nil, nil)`，轮循环以 `ReasonNoIntent` 收场。

**尚未覆盖**：真实平台上的故障注入（写超时、重复提交、平台侧 5xx）。这些需要授权环境，
Fake 场景的 `Evaluate` 是同步的，造不出真实的写超时。

## M4：指标与发布验收

- 以每次 Run 冻结的有效 `RunSpec.Profile` 同时驱动 system prompt、只读 extension bundle、Planner/提示参数及 `ProfileDigest`；`HarnessOptions.Profile` 仅提供默认值。生产 `Harness` 必须有跨进程 `RunLocker`，直接使用 SDK 与 CLI 装配遵循同一约束。
- 公开结果记录每题起跑剩余量、目标总量、本次新增确认、完成状态、成本、耗时、轮次、提示与失败类别；修正 `Kind` 分类被 `ResultFileStore` 归为 `unclassified` 的路径。原始 trace 和候选仅进入权限为 `0700/0600` 的 `private/`。
- 主指标为挑战完成率及「本次新增确认 / 起跑时剩余」召回率；分母未知时明确标注不可计算。跨题目集合只报描述性累计数据，直接比较只使用重叠的 challenge/profile/model。
- 通过新同步入口的 Docker 集成门与真实 pi 最小冒烟后，才在明确授权且 VPN 连通的 TSecBench 环境完成至少一题 `list → prepare → solve → submit → cleanup`。若授权环境不可用，保持发布门未通过，不用 Fake 或旧入口测试替代。

**实测状态（2026-09-22）**：前两条的**离线部分已落地**——`Kind` 枚举不再被折成 `unclassified`，
题级指标（题目数 / 已解 / 通过率）已进公开结果与 `stats`，原始 trace 只进 `0700/0600` 的 `private/`。
profile 冻结现在同时覆盖**扩展包内容摘要**（`RunResult.BundleDigest`）：`ProfileDigest` 描述配置
是什么，其中的 `ExtensionBundle` 只是一个路径字符串，所以「同一路径换了 bundle 内容」过去会被算作
同一次实验。运行终态（`finished`/`failed`/`cancelled`）与「是否解出」也已拆成两个字段。

**未通过**：真实 pi 最小冒烟与授权 TSecBench 平台冒烟。发布门保持未通过。

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
