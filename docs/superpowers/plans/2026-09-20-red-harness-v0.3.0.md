# red-harness v0.3.0 并发实施计划

> **给实施者：** 本计划由主上下文（协调者）按「契约冻结 → 波次并行 subagent → 主上下文集成门」执行。每个任务含精确文件路径、接口签名与验收命令。步骤用 `- [ ]` 跟踪。

**目标：** 把 red-harness 从 v0.2.0 的 `Session.Run` 单函数库，重写为 v0.3.0 的 `Engine.Start/Resume` + `RunHandle` SDK，补齐单写者事件溯源存储、TSecBench Python bridge、Docker 隔离执行器、CLI 与本地看板，并用 fake platform + stub pi 完成离线端到端验证。

**架构：** 根包 `harness` 退化为**纯契约**（类型 + 接口，零实现、零内部依赖）；全部实现搬进子包 `engine/`、`store/`、`executor/`、`bridge/`、`scenario/`、`report/`、`web/`、`internal/cli/`。引擎是唯一状态写入者：状态变化先追加带单调序号的领域事件（`events.jsonl`），再原子落盘快照（`run.json`）；恢复以 `lastAppliedSeq` 为基线重放。事实层与答案层严格分离——候选明文只进 `private/`，公开文件只有指纹。

**为什么根包必须是纯契约：** 这是并行化的前提。v0.2 的根包把契约（`Challenge`/`Gate`/`Scheduler`）和实现（`Session.Run` 的 260 行轮循环）混在一起，任何 agent 碰轮循环就会与所有引用根包类型的人冲突。拆开后每个子包是**一个 agent 的独占领地**，波次内文件集合天然不相交。

**交付边界（相对于 PLAN.md）：** PLAN.md 的 5 个里程碑全部纳入，但有三处**明确的范围修正**：
- **`docs/superpowers/specs/` 与 `docs/superpowers/plans/`** 是 PLAN.md:29 指定的归档位置（`writing-plans` 技能的默认目录约定）。本计划是权威工作副本。
- **`bash_guard.ts` 与 `report_fact` extension 本体不在范围内**——它们不是 PLAN.md 任何里程碑的交付物，且验证必须用真 pi 跑真实命令。契约层预留 `AgentSpec.Extensions` 装载位。Docker 隔离（T8）在资源限制上部分覆盖同一风险，但不覆盖磁盘写入量。
- **`git init` 是 PLAN.md:81 的例外**（见 Global Constraints）——经用户明确授权后执行，用于给并行实施提供回滚粒度。

**技术栈：** Go 1.26（`go.mod` 已声明；本机工具链 go1.27.1）、纯标准库（保持零依赖，与现有仓库一致）、Python ≥ 3.9 + 官方 `tsec-benchmark` SDK（bridge 子进程）、Docker CLI（executor 用 `docker` 命令行而非 SDK，避免引入依赖）、pi 0.86.0（bundled node v22）。

**规格来源：** `/root/red-harness/PLAN.md`（v1 实施计划）、`/root/red-harness/SDK_API.md`（TSecBench 官方 SDK 接入文档）。计划从规格论证，执行者两份都要读。

## Global Constraints

- **`git init`：已获用户明确授权，但 PLAN.md:81 明令禁止。** 计划在 W0 开头插入 **Task 0**（`git init` + `.gitignore`），因为用户批准时选了「允许 git init 做隔离」并说明要「每任务一提交」的回滚粒度——这直接决定每个任务的收尾步骤是「跑测试」还是「跑测试 + 提交」。
  - **`.gitignore` 必须先于任何 `git add` 就位**，至少排除：`.env`（含真实 `OPENCODE_API_KEY`）、`.remember/`、`runs/`、`private/`、`*.tmp`、`node_modules/`。
  - **绝不 `git add -A`**——逐任务显式列出文件路径。`.env` 与 `/tmp/tsec/TsecBench-main/.agent.env` 里的凭据不得进入任何提交。
  - 每个任务一个提交，提交消息用 `feat:`/`fix:`/`test:`/`docs:` 前缀 + 任务号（例如 `feat(engine): 单写者轮循环 [T11]`）。
- **并行隔离手段：** git 分支/worktree（用户授权）+ 冻结契约 + 包级所有权（见「并发编排」）。
- **零第三方依赖。** `go.mod` 保持无 `require` 块。不引入 Docker SDK、不引入 toml/yaml 库、不引入测试框架。需要容器编排就用 `docker` CLI。
- **`go test ./... -count=1`、`go test -race ./...`、`go vet ./...`、`gofmt -l .` 必须全绿**（PLAN.md:72）。Docker 集成测试走 `-tags integration` 显式启用。
- **预算默认值不变**（PLAN.md:78）：40 rounds、30 分钟、600 tool turns；模型成本默认不限但可配置。
- **Reason 字符串是 API**（`solver.go:47-61`），三个回归测试钉死了它们：`completed/timeout/stalled/stopped/max_turns/error/no_intent/solved/provider_failure`。重写可以**新增**，不得改变已有值。
- **轮循环的分支顺序是契约**（`harness.go:427 → 445 → 450 → 458 → 463`）：`ctx.Err()` 判定必须先于 provider 护栏，否则一次零回合的墙钟超时会变成「模型服务挂了」。
- **指纹唯一真源是 `answer.Fingerprint`**（`answer/fingerprint.go:88`）。`dag.FlagFingerprint` 是逐字节重复实现，必须改为转发。格式：`fp:<sha256[:8]>/len=<rune数>/<首>…<尾>`。
- **候选明文绝不进入** DAG、普通事件、看板或报告（PLAN.md:24）。
- **本机限制：** 无 VPN、未安装 `tsec-benchmark` SDK、2 核 3 GB 内存、Docker 29.8.0 可用。里程碑 3（TSecBench）真实验证不可能——用 mock SDK 离线验证。里程碑 4（Docker 与 pi）本机可做：runner 镜像本轮真构建，pi 真身与凭据都在（`.env` 有 `OPENCODE_API_KEY`），W4 跑真实 pi 最小冒烟。

## Review Focus

规格隐含、最容易伤到真实用户、且**必须由某条测试钉死**的五类输入，按可能性排序。每一条都已落到拥有该代码的任务里（括号内是落点）：

1. **恢复一个已经结束的 run。** 用户对 `completed`/`failed`/`cancelled` 的 run 调 `Resume`，合理预期是明确拒绝，而不是重新跑一轮、重复向平台提交 flag、重复消耗预算。→ **T11 `TestResumeRejectsTerminalRun`**
2. **`private/` 账本缺失或被截断时的恢复。** 用户在别处拷了 run 目录、或进程在写账本中途被杀。合理预期是 **fail closed**（明确报错、拒绝恢复），而不是「账本为空 ⇒ 没有任何候选被提交过 ⇒ 把全部候选重提一遍」。→ **T7 的 `Private` 缺失报错 + T11 `TestResumeRejectsMissingPrivateLedger`**
3. **平台写操作超时但实际已生效。** 用户看到的是 `submit` 超时。合理预期是恢复后先 `Reconcile` 对账再决定是否重试，而不是盲目重发——重发会浪费一次提交额度，或把已确认的 flag 记成判错。→ **T11 `TestResumeReconcilesBeforeRetry`**
4. **`events.jsonl` 末尾有半行（进程被杀）。** 用户重启后恢复。合理预期是跳过残缺末行并从最后一个完整事件继续，而不是整份日志解析失败、run 永久无法恢复。→ **T7 的半行测试 + T11 `TestResumeAfterTornEventLine`**
5. **`Resume` 时 `RunSpec` 与快照摘要不一致。** 用户改了预算或模型再恢复。合理预期是拒绝并指出哪个字段漂移了，而不是静默用新配置继续（会让预算护栏与报告失真）。→ **T11 `TestResumeRejectsDigestDrift`**

---

## 并发编排（本计划的执行方式）

### 核心约束

并行 agent 共享**一个 git 仓库**（已获用户授权初始化），冲突控制有三层：

1. **git worktree 隔离。** 每个实现 agent 用 **`isolation: "worktree"`** 分派——harness 会为它创建独立 git worktree，agent 的编辑不落到主工作目录，未改动的 worktree 自动清理。这比手工 `git worktree add` 更省事，也让「谁改了哪些文件」在合并时显式暴露。**注意：worktree 隔离只在仓库已有至少一个提交时可用**——所以 T0（`git init` + 首个提交）必须先于任何并行分派。
2. **契约先行、协调者单写者冻结。** 协调者**独自**完成 W0 的契约与迁移，跑通 `go build ./...` 与全部子包测试后才分派实现 agent。契约文件此后**冻结**——只有协调者能改。
3. **一个子包 = 一个 agent。** 波次内每个任务独占一组包目录。**Go 不允许同包并行编辑**——两个 agent 同时往同一个包加文件，任何一个的编译错误都会让另一个空转。协调者在分派前核对包级所有权。

**集成门（每个波次结束后，协调者在主上下文跑）：**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./... -count=1
```

全绿才进入下一波。红灯由协调者自己修（或派单个 agent 修），不带着红灯往下走。

**每个实现 agent 只跑自己的包测试**（`go test ./<pkg>/... -count=1`），不跑全量——避免因别人正在写的包编译失败而空转。全量在集成门跑。

**合并策略：** 同一波次内包级互斥 ⇒ 合并应是**零冲突**的。出现冲突说明所有权核对漏了，协调者要停下来查，而不是手工解冲突蒙过去。

### 包级所有权矩阵

每个 agent 在**自己的 worktree** 上工作（harness 的 `isolation: "worktree"` 会自动建分支，名字由它决定）：

| 波次 | agent A | agent B | agent C | agent D |
|---|---|---|---|---|
| W0 | 协调者独占（在 `main`）：根包 `harness` + `dag` + `gate` + `answer` + `docs/` | — | — | — |
| W1 | `store/` | `runner/` + `executor/` | `bridge/` | `cmd/` + `internal/cli/` |
| W2 | `engine/` | `piai/` | `scenario/` | `internal/cli/`（续） |
| W3 | `web/` | `report/` | `example/` | 根包收尾 + `docs/` + `README.md` |
| W4 | 核验 agent（独立复现全部断言，只读） | 真实 pi 最小冒烟 | — | — |

### 波次总览

| 波次 | 内容 | 并行度 | 门的验收 |
|---|---|---|---|
| W0 | git 初始化 + 设计归档 + 契约冻结 + 根包替换 + dag/gate 迁移 | 1（协调者） | `go build ./...` + 全部子包测试绿 |
| W1 | store / runner+executor / bridge / CLI 骨架 | 4 | `go test ./... -count=1` 全绿 |
| W2 | engine（内核+恢复+控制）/ piai / scenario / CLI 完整 | 4 | 同上 |
| W3 | web / report / example / 收尾 | 4 | 同上 + 端到端离线跑通 |
| W4 | 核验 agent + 真实 pi 冒烟 | 2 | 核验报告 + 全量绿 |

**提交粒度 = 任务。** 每个 agent 在**自己的 worktree 里**跑通自己的包测试后提交一次（消息带任务号，例如 `feat(engine): 单写者轮循环 [T11]`）；协调者在集成门通过后把 worktree 的改动合到 `main` 并再提交一次集成结果。这样任何一步出错都能 `git revert` 或 `git reset --hard` 回滚到上一个任务。

**协调者在 W0 与 W4 直接在 `main` 上提交**（W0 无并行、W4 是只读核验 + 冒烟）。

> **关于并行度：** 波次内最多 4 个 agent，但**不是每个波次都真有 4 个并行任务**。W4 只有 2 个。W2 的 agent D（T14）依赖 T8/T9/T11/T12/T13 的产物，实际上是**波次末段**才真正可跑——协调者可以先派 A/B/C，D 晚一步派，或把 T14 顺延到 W3 与 T15/T16 并行（此时 agent D 的包 `internal/cli/` 与 A 的 `web/`、B 的 `report/` 仍不相交）。**协调者按实际依赖决定 D 的派遣时机**，不要为了凑满 4 路而让 D 空转。

### 里程碑 → 任务映射

| PLAN.md 里程碑 | 任务 |
|---|---|
| M1 设计归档与内核 API | T0（git 初始化）、T1（设计归档）、T2（契约）、T3（根包替换）、T4（dag/gate 迁移）、T5（回归断言归档）、T6（迁移表） |
| M2 单写者控制器、存储与恢复 | T7（store）、T11（engine 内核+恢复+控制） |
| M3 TSecBench 场景适配器 | T9（bridge）、T13（scenario） |
| M4 Docker 隔离执行器与 pi 接入 | T8（runner 镜像 + executor）、T12（piai 适配）、T20（真实 pi 冒烟） |
| M5 CLI、看板与报告 | T10（CLI 骨架）、T14（CLI 完整 + doctor）、T15（看板）、T16（报告）、T17（示例） |

---

# W0：设计与契约冻结（协调者单独完成，不可并行）

**为什么 W0 必须串行：** 两个原因。
1. Go 的包级编译单元意味着根包换血（`harness.go`/`solver.go`/`platform.go`/`evidence.go` → `model.go`/`ports.go`/`events.go`/`handle.go`/`engine.go`/`errors.go`）在文件层面是「同一批符号被重新声明」。中途任何时刻 `go build ./...` 都是红的，只有全部改完才绿。
2. **W0 里 `dag` 的接口被改**（`Scheduler` → `Planner`）。`dag` 是 `piai` 的间接依赖链上游，且 `store`（W1）明确不导入它——但 W1 的四个 agent 都要 `go build ./...` 通过才能跑自己的包测试。所以 `dag` 的改动必须在分派 W1 之前**完全落地并验证**。

**W0 内部可以并行的地方：** T1（设计文档）与 T2–T5（契约与迁移）可以并行——一个写文档、一个写代码，文件集合不相交（`docs/` vs 根包/dag/gate/answer）。**协调者可以派一个 agent 写 T1 的文档**，自己在主上下文做 T2–T5。但 T0（git 初始化）必须最先，T6（迁移表）必须最后。

### Task 0: git 初始化与忽略规则

**文件：**
- 创建：`.gitignore`

**用户已授权 `git init`**（PLAN.md:81 的例外），用于给并行实施提供按任务回滚的粒度。

- [ ] **Step 1:** `git init`。
- [ ] **Step 2:** 写 `.gitignore`，**至少**包含：

```gitignore
# 凭据——绝不入库
.env
*.env
!.env.example

# 运行产物（含候选明文）
runs/
private/
*.tmp
*.jsonl

# 工具与缓存
.remember/
node_modules/
.pi-sessions/
.pi-home/
.claude/worktrees/
```

- [ ] **Step 3:** 提交初始状态：`git add .gitignore` + **显式列出**全部现有源码与文档（`harness.go solver.go platform.go evidence.go answer/ dag/ gate/ piai/ go.mod PLAN.md SDK_API.md .env.example LICENSE`），`git commit -m "chore: v0.2.0 baseline + ignore rules [T0]"`。
      **绝不要 `git add -A`** ——先确认 `git status --short` 里没有 `.env`。
      **为什么要有这个 baseline 提交：** harness 的 `isolation: "worktree"` 需要一个可用的 HEAD 才能建 worktree；没有基线提交，W1 的四个并行 agent 全都起不来。
- [ ] **Step 4:** 验证三件事：
      - `git status --short` 输出里**不得出现** `.env`
      - `git ls-files | grep -c '\.env$'` 必须为 0
      - `git ls-files | grep -c '^harness.go$'` 必须为 1（基线确实提交了）

### Task 1: 设计归档

**文件：**
- 创建：`docs/superpowers/specs/2026-09-20-offensive-security-harness-design.md`
- 创建：`docs/superpowers/plans/2026-09-20-red-harness-v0.3.0.md`（本文件的归档副本）

**内容要求：** 把 `PLAN.md` 的「公共接口与数据模型」「假设与默认值」两节展开成正式设计文档，并**明确记录 v0.2 → v0.3 的决策依据**——尤其是下面这些已确认缺陷，每条留一行「为什么这么改」：

- `harness.go:520-528` `emit` 在 agent reader 协程上做三路扇出（`OnEvent` 任意用户代码、`Observer`、`Gate.Observe` 取锁）；v0.3 改为**单写者 channel 消费**。
- `harness.go:473-477` 只有 `Saver.Save` 一个持久化点，`dag.Load`/`LoadOrNew`（`dag/store.go:200,270`）从未被根包调用 ⇒ 无法恢复。
- `harness.go:375-380` `MaxCostUSD` 在轮循环内恒为 0（`out.Stats` 只在循环结束后 `harness.go:492-494` 才赋值）⇒ 成本预算实际是死代码。
- `harness.go:42` 成本耗尽返回 `ReasonTimeout`（拷贝粘贴自墙钟分支）。
- `harness.go:65,67,85` `Outcome.IntentDone`/`Negative`/`Report` 从未被写入。
- `harness.go:186-187` vs `506-508`：`HintAlways` 文档说「每题一次」，代码无守卫、每轮都请求。
- `harness.go:258` `defer Platform.Close` 丢弃结果与错误，且复用可能已取消的 ctx。
- `harness.go:632-634` `Observer` 接口无任何实现；`platform.go:65-77` `Platform`/`HealthChecker` 无任何实现（`platform.go:64` 注释指向的 `platform` 包不存在）。
- `solver.go:44` `DefaultFlagFormat` 从未被引用；`solver.go:50` `ReasonStalled` 从未被写入。
- `dag/store.go:174` `FlagFingerprint` 是 `answer.Fingerprint` 的逐字节重复，注释里已写明待统一（`dag/store.go:161-173`）。
- `piai/proc.go:365` `proc.stderr()` 无任何调用者 ⇒ pi 的启动错误收进 `stderrTail` 却从不透出。
- `gate/provenance.go:110` `stateFileReCI` 过宽（详见 T4）。
- `piai/agent_test.go:800-810` 记录了根包 `solver.go` 注释与实现的语义不符（`Err` 是轮级而非轮级累积）——**设计文档要给出结论**：以实现为准（轮级），修正 `solver.go` 的注释。

**必须继承的前身事故教训**（`/root/.claude/plans/purrfect-scribbling-bentley.md` §十二，每条都有对应的落地位置）：

| 严重度 | 事故 | v0.3 的落地 |
|---|---|---|
| 致命 | LLM 一条命令写满 **307 GB** 磁盘（pi 的 bash 工具 timeout 可选、无默认） | **T2 契约层预留 `AgentSpec.Extensions` 装载位**；extension 本体与真 pi 验证**不在本计划范围内**（见「未决与风险」）。T8 的只读 rootfs + tmpfs 是唯一挡住「写满磁盘」的机制 |
| 致命 | pi 0.74.2 **静默烧题库**：provider 400 → 0 回合 + `stopReason=error` + 无错误文本，跑掉 280 run / 0 flag / 63 题 | T11 的两条 provider 护栏 + T12 的启动即校凭据 |
| 高 | 子 agent 的 `exitCode: 0` 初值让运行中进程被算成已完成 | `RoundResult` 的 `Turns`/`Reason` 初值必须表达「未完成」，不能是「已完成」 |
| 高 | `stop_check` 是死代码，「通关立即终止」从未被调用 | T11 轮首通关判定 + `TestRegression_SolvedStopsWithoutFlagCount` |
| 高 | pi 进程**不自己退出** | T12 的 `Close` 显式 `killpg` |
| 中 | flag 明文泄漏进 `MEMORY.md` / `_blackboard.json` | `dag/store.go` 的 `scrub` + T7 的 `private/` 分离 |
| 中 | RPC 信封：`type` 字段是必需的，缺了报 `Unknown command: undefined` | T12 不得改 `frames.go` 的出站编码 |
| 中 | **每题独立 `HOME`**：pi 从 `$HOME/.pi/agent/*` 发现扩展 / 角色 / 技能，共享 HOME 跨题污染 | T2 的 `AgentSpec.HomeDir` + T12 的 `childEnv` |
| 中 | pi 版本漂移（前身被 0.74.2 咬过） | T12 的 `DefaultVersionRange` 版本闸 |
| 低 | `turns` 靠数 `tool_execution_start` 是猜 | 保持同语义（护栏依赖），用 `get_session_stats().toolCalls` 交叉校验 |

**已经验证过的事实（不要重新推导）**：`new_session` 语义成立（`sessionId` 变、`messageCount` 归 0、历史确实清空）；`-e <绝对路径>` + `--approve` 能加载 extension；`details` 262 KB 不截断；长思考期 `get_state` 往返最坏 414 ms；`--append-system-prompt` 与 `AGENTS.md` 都进 system prompt 且**分节可见**（`sections.addendum` / `sections.project_context`）。证据在 `/tmp/m0/s*.json`。

**前身资产的位置**（设计文档要记下这些引用，实施时可能需要）：

| 资产 | 位置 | 用途 |
|---|---|---|
| 事实抽取正则 + B14 凭证质量闸 | `/tmp/base_72ae35f/adapter/blackboard.py` | `gate` 的宿主侧抽取通道；B14 噪音语料已是回归 fixture |
| 幻觉族 / 推导族分账 | `/tmp/base_72ae35f/packages/worker/ghost_worker/hallucination.py` | gate 来源闸的分类依据 |
| 作业教条（`_INTRANET_ORCHESTRATION` / `_CLAUDE_MD`） | `/tmp/base_72ae35f/adapter/taskprompt.py` | 写进 workdir 的 `AGENTS.md` |
| bash 三约束 | `/tmp/base_72ae35f/packages/worker/ghost_worker/pi_ext/bash_guard.js` | `bash_guard.ts` 的蓝本（**本计划范围外**，见「未决与风险」） |
| 前身平台配置语义（`ADAPTER_*` / `SOLVER_*`） | `/tmp/tsec/TsecBench-main/.env` | CLI flag 命名与默认值参考（`ADAPTER_MAX_CONCURRENCY=3`、`ADAPTER_ROUND_TIMEBOXES=480,820,1500,2000`、`ADAPTER_PER_CHALLENGE_SECONDS=4000`、`ADAPTER_USE_HINTS=1`） |
| 真实 `tsec_benchmark` SDK 源码（v0.1.2） | `/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/204/fs/usr/local/lib/python3.14/dist-packages/tsec_benchmark/` | T9 的契约依据——**已核实，见 T9 的表**。注意它在容器层、面向 py3.14 |
| 平台服务端实现 | `/tmp/tsec/TsecBench-main/tsecbench/api.py:112-162` | 与 SDK 对账（已核对：路由、鉴权头、响应模型逐字段一致） |
| ⚠️ 敏感文件 | `/tmp/tsec/TsecBench-main/.agent.env` | **含看起来有效的 `BENCHMARK_TOKEN` 与 API key。当成敏感文件对待，不要读值、不要提交、不要写进任何日志或报告。** |

**明确不复活（设计文档要原文记下，否则下一个人会加回来）：**
- 前身那份**从未实现**的 MITRE ATT&CK 攻击路径规划层（`/tmp/base_72ae35f/.claude/plans/autonomous-redteam-architecture.md` 的 `AttackPath`/`AttackStage`）。不要因为那份文档就把「图」做成攻击路径规划——图只做剪枝 / 推导链 / 分支 / 续跑四件事（`dag/node.go:14`、`dag/schedule.go:11-16` 已有同样的明令）。
- Heimdall 式 LLM 旁路观察者（默认关闭是有原因的）。
- monkeypatch `node_modules`。
- legacy/assignment 双模式驱动。
- 固定角色分工的多 agent（用户已否决）。

- [ ] **Step 1:** 写设计文档，含上述缺陷清单与 v0.3 决策。
- [ ] **Step 2:** 把本计划归档到 `docs/superpowers/plans/`。
- [ ] **Step 3:** 无代码变更，无需测试。

### Task 2: 契约层——新根包

**文件：**
- 创建：`model.go`、`errors.go`、`ports.go`、`events.go`、`handle.go`、`engine.go`（均在根包 `harness`）

**Interfaces（Produces，后续全部任务消费）：**

```go
// ── model.go ──
type Target struct {
	Code    string   `json:"code"`
	Addrs   []string `json:"addrs"`
	Network string   `json:"network"` // "tcp" / "http" / "unix"
}

type Objective struct {
	Kind      string `json:"kind"` // "flag_count" / "score" / "predicate"
	Want      int    `json:"want"`
	Got       int    `json:"got"`
	Completed bool   `json:"completed"`
}

type Evaluation struct {
	Accepted  bool   `json:"accepted"`
	Progress  bool   `json:"progress"`
	Completed bool   `json:"completed"`
	Score     int    `json:"score"`
	Message   string `json:"message"`
}

type RunState string
const (
	RunCreated   RunState = "created"
	RunPreparing RunState = "preparing"
	RunRunning   RunState = "running"
	RunPaused    RunState = "paused"
	RunCompleted RunState = "completed"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
)
func (s RunState) Terminal() bool

type RunID string

type RunSpec struct {
	Scenario   string       `json:"scenario"`
	Targets    []string     `json:"targets"` // 题目 code；空 = 全部未完成
	Agent      AgentSpec    `json:"agent"`
	Executor   ExecutorSpec `json:"executor"`
	Budget     Budget       `json:"budget"`
	HintPolicy string       `json:"hintPolicy"`
	Submit     bool         `json:"submit"`
	StoreDir   string       `json:"storeDir"`
	Policy     PolicySpec   `json:"policy"`
}
func (s RunSpec) Digest() string // sha256[:16]

type AgentSpec struct {
	Provider   string   `json:"provider"`
	Model      string   `json:"model"`
	Thinking   string   `json:"thinking"`
	Extensions []string `json:"extensions"` // 绝对路径；非交互模式下相对路径被静默忽略
	Approve    bool     `json:"approve"`    // 不开则项目资源被静默忽略
	SessionDir string   `json:"sessionDir"`
	// HomeDir 是**每题独立**的 HOME。pi 从 `$HOME/.pi/agent/*` 发现扩展 / 角色 /
	// 技能——共享 HOME 会跨题污染（前身已踩过）。为空时由引擎按
	// `<runDir>/.pi-home` 生成。
	HomeDir string `json:"homeDir"`
	// SessionReuse 为假时每题强制新进程（前身降级开关
	// REDCOPILOT_PI_SESSION_REUSE=0 的等价物）。
	SessionReuse bool `json:"sessionReuse"`
}

type ExecutorSpec struct {
	Image      string            `json:"image"`
	Workdir    string            `json:"workdir"`
	CPUs       float64           `json:"cpus"`
	MemoryMB   int               `json:"memoryMB"`
	PidsLimit  int               `json:"pidsLimit"`
	ReadOnly   bool              `json:"readOnly"`
	AllowHosts []string          `json:"allowHosts"` // 目标 IP:port 白名单
	Env        map[string]string `json:"env"`
}

type PolicySpec struct {
	MaxAttemptsPerIntent int `json:"maxAttemptsPerIntent"`
	DryRoundsBeforeHint  int `json:"dryRoundsBeforeHint"`
}

// Budget 沿用 harness.go:14-23 的字段与 DefaultBudget() 默认值，
// 但修掉 Exhausted 的两个缺陷（成本分支返回 ReasonTimeout、rounds 分支
// 复用 ReasonMaxTurns）。新增两个常量，不改旧值：
const (
	ReasonMaxRounds = "max_rounds" // 新增
	ReasonMaxCost   = "max_cost"   // 新增
)
type Budget struct {
	MaxRounds  int           `json:"maxRounds"`
	MaxWall    time.Duration `json:"maxWall"`
	MaxTurns   int           `json:"maxTurns"`
	MaxCostUSD float64       `json:"maxCostUSD"`
}
func DefaultBudget() Budget
func (b Budget) Exhausted(used Budget) (bool, string)

// ── errors.go ──
type Kind string
const (
	KindConfig      Kind = "config"
	KindScope       Kind = "scope"
	KindPlatform    Kind = "platform"
	KindProvider    Kind = "provider"
	KindExecutor    Kind = "executor"
	KindBudget      Kind = "budget"
	KindPersistence Kind = "persistence"
	KindCancelled   Kind = "cancelled"
)
type Error struct {
	Kind      Kind    `json:"kind"`
	Op        string  `json:"op"`
	Retryable bool    `json:"retryable"`
	RunID     RunID   `json:"runId,omitempty"`
	Err       error   `json:"-"`
	Msg       string  `json:"message"`
}
func (e *Error) Error() string
func (e *Error) Unwrap() error
func E(kind Kind, op string, err error) *Error
func Retryable(err error) bool
func IsKind(err error, k Kind) bool

// ── ports.go ──
// Platform 保持 platform.go:65-71 的五个方法签名不变。
type Platform interface {
	List(ctx context.Context) ([]Challenge, error)
	Start(ctx context.Context, code string) (StartResult, error)
	Hint(ctx context.Context, code string) (HintResult, error)
	Submit(ctx context.Context, code, flag string) (SubmitResult, error)
	Close(ctx context.Context, code string) (CloseResult, error)
}
type HealthChecker interface{ Health(ctx context.Context) error }

// Challenge / StartResult / SubmitResult / HintResult / CloseResult 从
// platform.go 移入，加 json tag，字段名与类型一律不变。

type Agent interface {
	Start(ctx context.Context, req AgentStart) error
	Round(ctx context.Context, req RoundRequest) (RoundResult, error)
	Steer(ctx context.Context, msg string) error
	Stats(ctx context.Context) (Stats, error)
	Close(ctx context.Context) error
}
type AgentFactory interface {
	New(spec AgentSpec, ev EventSink) (Agent, error)
}

// EventSink 是 agent 把流式事件交给引擎的唯一通道。v0.2 用
// `emit func(Event)` 从 agent 的 reader 协程直接扇出（harness.go:520-528），
// v0.3 改成显式接口：agent 只推事件，谁消费由引擎决定。
type EventSink interface{ Emit(Event) }

type RoundRequest struct {
	Prompt   string
	Round    int
	IntentID string
	Timeout  time.Duration
}

type Executor interface {
	Prepare(ctx context.Context, spec ExecSpec) (ExecHandle, error)
	Exec(ctx context.Context, h ExecHandle, cmd []string, opts ExecOptions) (ExecResult, error)
	Reclaim(ctx context.Context, runID RunID) error
	Available(ctx context.Context) error
}

type Planner interface {
	Next(ctx context.Context, in PlannerInput) (*IntentRef, error)
	Activate(it *IntentRef)
	Settle(it *IntentRef, res RoundResult)
	// ObserveEvent 把轮内事件喂给事实抽取。
	//
	// 为什么叫 ObserveEvent 而不是 Ingest：`dag.Scheduler` 已有一个
	// `Ingest(ev, round) IngestResult`（`dag/schedule.go:131`），Go 不允许同名的
	// 两个方法只因返回值不同而共存。改 dag 的签名会打断读返回值的既有测试，
	// 所以接口用新名字，dag 侧加一个薄包装方法（见 T4）。
	//
	// 为什么必须收进接口：v0.2 靠 `Session.Ingest func(Event,int)` 接线
	// （`harness.go:138`），漏接是**静默**的——DAG 零事实、7 个阶段只有前 4 个
	// 可达，而所有包自测全绿（`wiring_test.go:85` 就是为这个写的）。收进接口后
	// 漏接变成编译错误。
	ObserveEvent(ev Event, round int)
}
type Renderer interface {
	Render(ctx context.Context, ch Challenge, it *IntentRef, out *OutcomeView) string
}
type CandidateGate interface {
	Observe(ev Event)
	Candidates() []Candidate
	New() []Candidate
	Mark(flag string, res SubmitResult, err error)
	SetIntent(intentID string, round int)
}
type RunPolicy interface {
	// OnRoundStart 决定本轮是否继续；返回 (false, reason) 即终止。
	OnRoundStart(ctx context.Context, in PolicyInput) (bool, string)
	// OnCandidate 决定一个候选是否允许提交。
	OnCandidate(ctx context.Context, c Candidate) bool
}
type Scenario interface {
	Discover(ctx context.Context, spec RunSpec) ([]Challenge, error)
	Prepare(ctx context.Context, ch Challenge) (Target, error)
	Hint(ctx context.Context, ch Challenge) (string, error)
	Evaluate(ctx context.Context, ch Challenge, flag string) (Evaluation, error)
	Reconcile(ctx context.Context, ch Challenge) (Objective, error)
	Cleanup(ctx context.Context, ch Challenge) error
}
type Store interface {
	// Append 先写事件日志，再原子写快照。顺序不可颠倒。
	Append(ev DomainEvent) error
	Snapshot(ctx context.Context) (Snapshot, error)
	LoadEvents(afterSeq int64) ([]DomainEvent, error)
	Private() EvidenceStore
	Dir() string
}
type EvidenceStore interface {
	PutCandidate(c Candidate) error
	PutEvidence(ref string, data []byte) error
	GetEvidence(ref string) ([]byte, error)
	// Rejected 返回已判错答案的指纹集（明文不出 private/）。
	Rejected() ([]string, error)
}

// ── 从 v0.2 原样保留（只加 json tag，字段名与类型一律不变）──
// Event, EventKind（solver.go:7-39）
// AgentStart, RoundResult, Stats（solver.go:84-141）
// Challenge, StartResult, SubmitResult, HintResult, CloseResult（platform.go）
// Provenance, Candidate, RejectedLedger（evidence.go）
// IntentRef（harness.go:592-597）
// HintOff / HintAuto / HintAlways（harness.go:181-188）
// 全部 Reason* 常量（solver.go:47-61）

// ── 新定义 ──
type OutcomeView struct {
	Code      string
	Reason    string
	Flags     []string // 只作为**返回值**存在，绝不落盘
	Candidates []Candidate
	Submitted int // 去重后的确认数
	Duplicates int
	Rejected  int
	Rounds    int
	IntentDone int
	Negative   int
	HintUsed   int
	ProgressConfirmed int
	ProgressTotal     int
	Stats     Stats
	Report    string
	Err       string
	StartedAt time.Time
	EndedAt   time.Time
}

type PlannerInput struct {
	Challenge Challenge
	Outcome   OutcomeView
	Round     int
}
type PolicyInput struct {
	Challenge  Challenge
	Outcome    OutcomeView
	BudgetUsed Budget
	Round      int
}

type RunSummary struct {
	RunID     RunID     `json:"runId"`
	State     RunState  `json:"state"`
	Scenario  string    `json:"scenario"`
	Targets   []string  `json:"targets"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Objective Objective `json:"objective"`
	Score     int       `json:"score"`
}

type DoctorReport struct {
	Checks []DoctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}
type DoctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail"`
	Fatal   bool   `json:"fatal"`
}

// ── Executor 的类型 ──
// ExecSpec 是 ExecutorSpec 的**已解析**形式：RunSpec 里是用户写的配置，
// ExecSpec 是引擎加上运行身份与网络边界之后交给执行器的东西。
type ExecSpec struct {
	RunID     RunID
	Target    Target
	Executor  ExecutorSpec
	Network   string // 本次运行独占的 bridge 网络名
	Workdir   string // 唯一允许挂进容器的宿主目录
}
type ExecHandle struct {
	ContainerID string
	Network     string
	// Addrs 是容器内视角可达的目标地址。
	Addrs []string
}
type ExecOptions struct {
	Env     map[string]string
	Workdir string
	Timeout time.Duration
	Stdin   io.Reader
}
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// ── events.go ──
type DomainEvent struct {
	Seq     int64           `json:"seq"`
	At      time.Time       `json:"at"`
	Type    DomainEventType `json:"type"`
	RunID   RunID           `json:"runId"`
	Round   int             `json:"round,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
type DomainEventType string
const (
	EvRunCreated    DomainEventType = "run_created"
	EvRunPreparing  DomainEventType = "run_preparing"
	EvTargetStarted DomainEventType = "target_started"
	EvRoundStarted  DomainEventType = "round_started"
	EvIntentActive  DomainEventType = "intent_active"
	EvIntentSettled DomainEventType = "intent_settled"
	EvCandidateSeen DomainEventType = "candidate_seen" // 只含指纹
	EvSubmitResult  DomainEventType = "submit_result"  // 只含指纹与判定
	EvHintRequested DomainEventType = "hint_requested"
	EvFactLearned   DomainEventType = "fact_learned"
	EvBudgetUsed    DomainEventType = "budget_used"
	EvRunPaused     DomainEventType = "run_paused"
	EvRunResumed    DomainEventType = "run_resumed"
	EvRunEnded      DomainEventType = "run_ended"
)

// ── handle.go ──
type Snapshot struct {
	SchemaVersion  int           `json:"schemaVersion"`
	RunID          RunID         `json:"runId"`
	State          RunState      `json:"state"`
	Spec           RunSpec       `json:"spec"`
	SpecDigest     string        `json:"specDigest"`
	LastAppliedSeq int64         `json:"lastAppliedSeq"`
	Objective      Objective     `json:"objective"`
	BudgetUsed     Budget        `json:"budgetUsed"`
	StartedAt      time.Time     `json:"startedAt"`
	EndedAt        time.Time     `json:"endedAt,omitempty"`
	Reason         string        `json:"reason,omitempty"`
	Err            string        `json:"err,omitempty"`
	Public         PublicSummary `json:"public"`
}
// Snapshot **没有** Flags 字段——明文只在 private/ 与 OutcomeView 里。

type PublicSummary struct {
	CandidatesSeen     int      `json:"candidatesSeen"`
	SubmittedConfirmed int      `json:"submittedConfirmed"`
	Duplicates         int      `json:"duplicates"`
	Rejected           int      `json:"rejected"`
	HintUsed           int      `json:"hintUsed"`
	Rounds             int      `json:"rounds"`
	IntentDone         int      `json:"intentDone"`
	Negative           int      `json:"negative"`
	ConfirmedFP        []string `json:"confirmedFingerprints"`
}

type RunHandle interface {
	ID() RunID
	Snapshot(ctx context.Context) (Snapshot, error)
	Events(ctx context.Context, afterSeq int64) (<-chan DomainEvent, error)
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Cancel(ctx context.Context) error
	Wait(ctx context.Context) (Snapshot, error)
}

// ── engine.go ──
type Engine interface {
	Start(ctx context.Context, spec RunSpec) (RunHandle, error)
	Resume(ctx context.Context, id RunID) (RunHandle, error)
	List(ctx context.Context) ([]RunSummary, error)
	Doctor(ctx context.Context) (DoctorReport, error)
}
type Options struct {
	Platform  Platform
	Executor  Executor
	Agents    AgentFactory
	Store     Store
	Scenarios map[string]Scenario
	// Policy 为 nil 时引擎用默认策略：MaxAttemptsPerIntent=3、
	// DryRoundsBeforeHint=3（沿用 dag.DefaultMaxAttempts 与 HintAuto 的阈值）。
	Policy RunPolicy
	// Gate 为 nil 时引擎按每道题的题面形状构造 gate（生产实现见 T4）。
	// 可注入是为了让 engine 包的测试能用一个记账型 fake 观察提交行为。
	Gate func(ch Challenge) CandidateGate
	// Planner / Renderer 为 nil 时引擎按每道题构造 dag 的实例。
	Planner  func(ch Challenge) Planner
	Renderer func(ch Challenge) Renderer
	// OnEvent 是流式事件回调（transcript / 日志）。在 runLoop goroutine 上调用。
	OnEvent func(Event)
	Now     func() time.Time
}
// 必需端口：Platform、Executor、Agents、Store、Scenarios（至少一项）。
// 其余为 nil 时取默认实现。任何必需端口为 nil ⇒ KindConfig 错误。
func New(opts Options) (Engine, error)
// 注意：`Options.Gate`/`Planner`/`Renderer` 是**工厂**而不是实例——
// 引擎一道 run 可能跑多道题，而 gate/planner/renderer 都是**每题独立**的
// （v0.2 里 `dag.NewScheduler(g)` 与 `gate.NewGate(desc)` 就是每题新建）。
//
// ⚠️ **这条决定了一个包依赖：** 「按题面形状构造 gate」与「按题构造 dag 的
// planner/renderer」的默认实现不能写在 `engine/` 里——`engine` 若导入
// `dag`/`gate`，就与「每个子包独立并行开发」的编排直接冲突（engine 的 agent
// 会被 dag/gate 的编译状态卡住）。所以默认实现放在**装配层**：
// `internal/cli/wire.go`（T14 交付，包级所有权矩阵里属于 agent D）。
// `engine/` 自己只依赖根包契约 + `store/`，测试用注入的 fake。
```

- [ ] **Step 1: 写失败测试** —— `contract_test.go`（新文件，`package harness_test`）：`RunState.Terminal()` 七状态真值表；`RunSpec.Digest()` 同配置同摘要 / 字段漂移则变 / `Targets` 顺序敏感；`Budget.Exhausted` 四条分支的原因（含新增的 `ReasonMaxRounds`/`ReasonMaxCost`）；三层 wrap 后 `errors.IsKind(err, KindPlatform)` 为真；`DomainEvent` JSON round-trip；`Snapshot` 序列化后**不含**任何候选明文（用一个假 flag 断言 `!bytes.Contains`）；`New(Options{})` 返回 `KindConfig` 错误且消息指出缺哪个端口；`New` 端口齐备时返回非 nil。
- [ ] **Step 2: 跑测试确认失败** —— `go test . -run 'TestContract' -v`，预期 `undefined: RunState`。
- [ ] **Step 3: 写实现**（六个新文件）。
- [ ] **Step 4: 跑测试确认通过** —— 同上命令。
- [ ] **Step 5:** 此时 `go build ./...` **仍会红**（旧文件与新契约重复声明 `Challenge`/`Budget`/`Gate` 等）——这是预期的，T3 解决。

### Task 3: 根包替换——删除 v0.2 实现

**文件：**
- 修改：`harness.go` → 只保留 `Version`（改为 `"0.3.0"`）、`DefaultPrompt`、`answerFormatHint`、`IntentRef`、`HintOff/HintAuto/HintAlways`。删除 `Session` 及其全部方法（`Run`/`resolveChallenge`/`runOneShot`/`runRounds`/`maybeHint`/`emit`/`harvest`/`observe`/`now`）、`Outcome`、`Scheduler`、`Renderer`、`Saver`、`Observer`、`appendUnique`、`DefaultBudget`（已移入 `model.go`）。
- 修改：`solver.go` → 只保留 `EventKind` 常量、`Event`、`Reason*` 常量、`AgentStart`、`RoundResult`、`Stats`。删除 `Solver` 接口、`SolveRequest`、`SolveResult`、`DefaultFlagFormat`（从未被引用）。
- 删除：`platform.go`、`evidence.go`（内容已移入 `model.go`/`ports.go`；`Provenance`/`Candidate`/`Gate`/`RejectedLedger` 的去向见下）
- 删除：`regression_test.go`、`wiring_test.go`（**原文已在 T5 归档**到 `docs/superpowers/plans/fixtures/`，T11 据此移植）

**关键决策：**
- **`Solver` 接口与 `runOneShot` 一并删除**（PLAN.md:30「移除一次性 Solver 门面」）。仓库里**本来就没有任何实现**（`grep 'func .* Solve('` 零命中），所以删的是死代码，不是能力。
- `Outcome` 的 19 个字段去向：`Code`/`Reason`/`Flags`/`Candidates`/`Submitted`/`Duplicates`/`Rejected`/`Rounds`/`HintUsed`/`ProgressConfirmed`/`ProgressTotal`/`Stats`/`Err`/`StartedAt`/`EndedAt` → 新类型 `OutcomeView`（供 `Renderer` 与报告消费，`Flags` 保留因为它是**返回值**不是落盘物）；`Solve` 删除（Solver 没了）；`IntentDone`/`Negative`/`Report` 三个死字段**改为真实回填**（分别来自 `dag.Graph.Stats()`、`dag.Graph.Stats()`、报告路径），不得保留死字段。
- `Provenance`/`Candidate`/`RejectedLedger` 移入 `ports.go`；`Gate` 接口改名 `CandidateGate` 并**吸收 `SetIntent`**（v0.2 靠 `Session.IntentSink` 接线，`harness.go:145`）。
- **保留** `answer.Shape` 的 json 表示逐字节兼容：`dag/store.go:32` 把 `answer.Shape` 无 tag 嵌进 schema 1 的 JSON，而 `answer.Shape` 的字段名是 `Envelopes/AllowRaw/RawMinLen/RawMaxLen`。**不要给 `Shape` 加 json tag 改字段名**——那会静默破坏所有现存 `graph.json`。若确需改名，必须同时写 `MarshalJSON`/`UnmarshalJSON` 显式保持旧拼写。

- [ ] **Step 1:** 删改四个文件，`go build ./...` 必须绿（`dag`/`gate`/`piai` 会因为接口改名而红——T4 解决）。
- [ ] **Step 2:** `go vet ./...` 确认无残留引用。

### Task 4: dag / gate / answer 迁移到新契约

**文件：**
- 修改：`dag/schedule.go`（`Scheduler` 实现新的 `harness.Planner`，含 `ObserveEvent` 薄包装；`Renderer` 签名对齐 `OutcomeView`）
- 修改：`dag/node.go`（`IntentState` 新增 `IntentInterrupted = "interrupted"` + `Valid()` 认它）
- 修改：`dag/store.go`（`FlagFingerprint` 改为转发 `answer.Fingerprint`；`migrate` 认 `interrupted`）
- 修改：`dag/render.go`（`RenderInput` 对齐 `OutcomeView`）
- 修改：`gate/gate.go`（实现 `CandidateGate`，含 `SetIntent`）
- 修改：`gate/provenance.go`（**修 `stateFileReCI` 过宽缺陷**，见下）
- 修改：`answer/shape.go`（**不加 json tag**，见 T3）
- 修改：对应的 `*_test.go`（改签名，不改断言）

**必须保留的既有契约（这些是前几轮用真事故换来的）：**
- `dag/schedule.go:284-294` 刻意留的「为什么不把平台判错的答案转成 negative 事实」注释；`dag/node.go:14` 与 `dag/schedule.go:11-16` 的「不要加拓扑排序式调度或攻击路径规划」明令。**不得删除这些注释，也不得把被否决的机制加回来。**
- `dag.Render` 的 golden 文件 `dag/testdata/render_golden.txt`。改动 `render.go` 后要显式跑一次 `UPDATE_GOLDEN=1` 并**人工确认 diff**——golden 是渲染契约的唯一防线。
- `dag/store.go` 的 `Save` 落盘前 `scrub` 掉答案明文（`Content`/`Raw`/`Goal`/`Evidence`/`Rejection.Content`）——`Raw` 是最容易漏的那个，有专门测试（`TestRawScrubbedOnSave`）。
- `gate` 的「首现优先、只升不降、`locked` 不可解锁」模型（`gate/provenance.go`）。`derived` 族**故意没有访问器**（`gate.go:627-631`）——不要为了「完整性」加一个。

**必须修的缺陷：`gate/provenance.go:110` 的 `stateFileReCI` 过宽。** 现状：CI 变体让裸小写 `notes`/`memory`/`todolist`/`_blackboard*`/`tried_commands*`/`_transcripts` 也大小写不敏感匹配，于是：

| 命令 | 现状判定 | 应为 |
|---|---|---|
| `curl -s http://10.0.0.1/flag` | observed | observed ✅ |
| `cat FLAG` | self_readback | self_readback ✅ |
| `cat FLAG.txt` | self_readback | self_readback ✅ |
| `curl -s http://t/flag.txt` | **self_readback** | **observed** ❌ |
| `curl -s http://10.0.0.1/notes` | **self_readback** | **observed** ❌ |
| `curl -s http://10.0.0.1/memory` | **self_readback** | **observed** ❌ |
| `grep -r memory /etc` | **self_readback** | **observed** ❌ |

判成 `self_readback` 会置 `locked=true`——**永久不可提交**。这正是 `provenance.go:98-104` 自己记录的「29 条被拒里 19 条实为正确答案」那一类。收窄方式：CI 变体只保留 `flag(?:\.(?:txt|md|json|log))` 一项，其余名字回到大小写敏感。

- [ ] **Step 1: 写失败测试** —— `dag/store_test.go` 加 `TestLoadV0_2GraphJSON`：写死一段 v0.2 的 `graph.json` 字面量，断言 `Load` 成功且 `Shape` 各字段值正确。这是最容易静默失败的一处。
- [ ] **Step 2: 写失败测试（gate）** —— `gate/provenance_test.go` 加 `TestStateFileReCINarrowed`：上表七个命令逐条断言。现有 `TestTargetPathFlagIsNotOwnStateFile` 只测了 `/flag` 一个。
- [ ] **Step 3–5:** 标准 TDD 循环，`go test ./dag/... ./gate/... ./answer/... -count=1`。
- [ ] **Step 6:** 人工确认 `dag/testdata/render_golden.txt` 的 diff 是预期内的。

### Task 5: 归档 v0.2 的回归断言（不写 Go 代码）

**文件：**
- 创建：`docs/superpowers/plans/fixtures/v0.2-regression_test.go.txt`（`regression_test.go` 的逐字副本）
- 创建：`docs/superpowers/plans/fixtures/v0.2-wiring_test.go.txt`（`wiring_test.go` 的逐字副本）
- 创建：`docs/superpowers/plans/fixtures/README.md`

**为什么这么做（而不是在 W0 就写新测试）：** T3 要删掉这两个文件，但它们的断言是前几轮事故换来的。**不能把它们写成 `engine/` 包里的测试**——W0 时 `engine` 包还不存在，而 `t.Skip` 只跳过执行、不跳过编译，引用 `engine.New` 的测试文件会让 `go build ./...` 直接红。所以 W0 只**归档原文**，移植是 T11 的第一步（那时 `engine` 包已存在，测试能编译）。

`README.md` 要写清每一条断言的**意图**（不只是它断言了什么，还有它防的是哪次事故），这样 T11 的移植者不会因为「类型对不上」而把断言改弱：

| 原用例 | 位置 | 防的事故 |
|---|---|---|
| `TestRegression_SolvedStopsWithoutFlagCount` | `regression_test.go:96` | 「通关立即终止」是死代码（`stop_check` 从未被调用）。断言：`List` 返回 nil 且无 `Challenge` ⇒ `FlagCount` 不可用，靠平台权威进度（`CorrectFlagCount:2, TotalFlagCount:2`）必须在 **1 轮**后停，`Reason == ReasonSolved`，且 `Cleanup` 被调用。 |
| `TestRegression_ZeroTurnTimeoutIsNotProviderFailure` | `:153` | 一次零回合的墙钟超时被误记成「模型服务挂了」。断言：60 ms 超时 + 零回合 ⇒ `ReasonStopped`（**不是** `ReasonProviderFailure`），且 `Run` 返回 `nil` error。钉死分支顺序。 |
| `TestRegression_ProviderErrorCaughtEvenWithTurns` | `:201` | provider 故障只在 `turns == 0` 时被识别，带回合的故障漏网。断言：`Turns:3` + `ProviderError` ⇒ 仍须 `ReasonProviderFailure`。 |
| `TestWiring_IngestReceivesRoundScopedEvents` | `wiring_test.go:85` | DAG 静默退化：漏接 `Ingest` ⇒ 图零事实、7 个阶段只有前 4 个可达，而所有包自测全绿。断言：3 轮 × 2 事件 ⇒ `Ingest` 恰 6 次，轮号 `1,2,3` 都出现。 |
| `TestWiring_IntentSinkFillsProvenanceAnchor` | `:119` | 候选的推导链锚点丢失（`IntentID` 全空、`Round` 全 0），族别判定不受影响所以**不报错**。断言：2 轮 ⇒ `SetIntent` 恰 2 次，id `intent-A`/`intent-B`，轮号 `1`/`2`。 |
| `TestWiring_SaverCalledEachRound` | `:148` | 断点续跑的粒度被破坏。断言：4 轮 ⇒ 落盘恰 4 次。 |
| `TestWiring_SaveFailureIsNotSilent` | `:167` | 落盘失败被吞掉，这一轮的进展没保住而调用方不知道。断言：`Save` 出错必须以 `EventError` 透出。 |

- [ ] **Step 1:** 逐字复制两个测试文件到 `docs/superpowers/plans/fixtures/`（加 `.txt` 后缀，避免被 `go build ./...` 当成源码）。
- [ ] **Step 2:** 写 `README.md`，含上表的「防的事故」列。
- [ ] **Step 3:** 无 Go 代码变更，`go build ./...` 不受影响。

### Task 6: 迁移表文档

**文件：**
- 创建：`docs/migration-v0.2-to-v0.3.md`
- 修改：`docs/superpowers/specs/2026-09-20-offensive-security-harness-design.md`（T1 已创建；本任务补「接线字段去向」一节）

**内容：** 逐项对照 `Session.Run` → `Engine.Start`；`Outcome` 19 字段去向；`Solver` 删除说明；`Scheduler` → `Planner`（含 `ObserveEvent` 收进接口的理由，以及它与 `dag.Scheduler.Ingest` 的关系）；`Ingest`/`IntentSink`/`Saver`/`GraphPath`/`Observer` 五个接线字段如何并入 `RunSpec` + `Options`；旧 `Reason*` 常量全部保留 + 新增两个；`Version` `"0.2.0"` → `"0.3.0"`（`harness.go:11`，被 `dag/store.go:65` 消费）；`Outcome` → `OutcomeView` 的字段对照表。

- [ ] **Step 1:** 写文档。无测试。
- [ ] **Step 2: W0 集成门** —— `gofmt -l . && go build ./... && go vet ./... && go test ./dag/... ./gate/... ./answer/... ./piai/... -count=1` 全绿。**注意 `./...` 现在包含根包，而根包已无测试**——这是预期的。

---

# W1：独立子系统（4 agent 并行，包级互斥）

### Task 7: 文件 Store 与私密账本（agent A）

**文件：** 创建 `store/store.go`、`store/events.go`、`store/private.go`、`store/atomic.go` + 三个 `*_test.go`

**Interfaces:**
- Consumes: `RunID`、`RunSpec`、`Snapshot`、`DomainEvent`、`RunState`、`Store`、`EvidenceStore`（T2 冻结）。
- Produces: `store.New(dir string) (*FileStore, error)`，满足 `harness.Store`。

**目录布局（PLAN.md:37）：**
```
<StoreDir>/runs/<runID>/
├── run.json       0600  快照
├── graph.json     0600  DAG（格式由 T4 的 dag 迁移结果决定，见下）
├── events.jsonl   0600  领域事件，每行一个 DomainEvent
├── private/       0700
│   ├── candidates.jsonl  0600  候选明文账本
│   └── evidence/         0600  原始工具输出
├── report.json    0644
└── report.md      0644
```

**`graph.json` 的读写（关键约束）：** **不要另写 DAG 序列化**——`dag/store.go` 的 schema 1 + `migrate`（`store.go:285`）是前向兼容的唯一实现。
**`store` 只负责路径与原子性包装**：把 `graph.json` 的内容当成 `[]byte` 透传（`PutGraph([]byte)` / `GetGraph() ([]byte, error)`），由调用方（T11 的 engine）去调 `dag.Graph.Save`/`Load`。这样 `store` **不导入 `dag`**，agent A 不会被 dag 的在建状态卡住——`dag` 在 W1 由协调者改，`store` 由 agent A 写，两者必须零耦合。

**store 必须实现的语义：**
- `Append(ev DomainEvent) error`：**先写 `events.jsonl`**（`O_APPEND` + 一次 `write` 系统调用写完整行，保证不撕裂），**再原子写 `run.json`**。顺序不可颠倒——先快照后事件会在崩溃时产生「快照指向不存在的事件」。
- 序号单调：`Append` 拒绝 `ev.Seq != lastSeq+1`，返回 `KindPersistence` 错误。
- 原子写：`writeTmp → fsync(file) → rename → fsync(dir)`。复用 `dag/store.go:59-100` 的 `.tmp` + `os.Rename` 模式，但**必须补 fsync**——现有实现只做了 rename。另外现有 `.tmp` 名是固定的（`path + ".tmp"`），两个写者会撞车；单写者下没问题，store 层要显式注释这个前提。
- 权限：目录 `0700`、私密文件 `0600`，创建后 `os.Chmod` 兜底（umask 会吃掉）。
- `LoadEvents(afterSeq int64) ([]DomainEvent, error)`：**跳过末尾残缺行**（Review Focus #4）——`bufio.Scanner` 逐行解析，`json.Unmarshal` 失败的**最后一行**停止并返回已解析部分；非最后一行失败则报错。
- `Private` 账本缺失返回明确的 `KindPersistence` 错误（Review Focus #2，T11 消费）。

- [ ] **Step 1: 写失败测试** —— 序号非单调被拒；`events.jsonl` 末尾半行可恢复；权限位 `0600`/`0700`；原子写后无残留 `.tmp`；`run.json` 与 `events.jsonl` 的 `lastAppliedSeq` 一致；先事件后快照的顺序（注入一个会失败的快照写，断言事件已落盘）。
- [ ] **Step 2: 跑测试确认失败** —— `go test ./store/... -count=1`，预期 `no Go files`。
- [ ] **Step 3: 写实现。**
- [ ] **Step 4: 跑测试确认通过** —— `go test ./store/... -count=1 -race`。
- [ ] **Step 5:** `gofmt -l store/` 无输出。

### Task 8: Docker 隔离执行器与 runner 镜像（agent B）

**文件：** 创建 `runner/Dockerfile`、`runner/README.md`、`executor/docker.go`、`executor/spec.go`、`executor/network.go`、`executor/proxy.go`、`executor/reclaim.go`、`executor/spec_test.go`（无 tag）、`executor/docker_test.go`（`//go:build integration`）

**Interfaces:**
- Consumes: `Executor`、`ExecutorSpec`、`ExecSpec`、`ExecHandle`、`ExecOptions`、`ExecResult`（T2 冻结）。
- Produces: `executor.NewDocker(cfg DockerConfig) (*Docker, error)`。

**硬约束（PLAN.md:51-54，每条要有断言）：** 非 root 用户；`--read-only` rootfs + `--tmpfs /tmp`；`--cap-drop=ALL`；`--security-opt no-new-privileges`；`--pids-limit`；`--memory`；`--cpus`；墙钟超时；独立 PID/IPC namespace。

**网络（PLAN.md:52）：** 每次运行独立 bridge 网络；目标流量只允许到 TSecBench 返回的 `IP:port`；provider 流量走宿主侧域名白名单代理（`executor/proxy.go`，容器内 `HTTP_PROXY` 指向它）；其余出站默认拒绝。实现方式：`docker network create` + `--internal` 网络 + 显式端口映射，或 `iptables` 规则。

**挂载：** 只挂本题工作目录；**运行状态、token、私有证据不挂载进容器**（要有断言）。

**回收（PLAN.md:54）：** run label `red-harness.run=<runID>`；`Reclaim` 按 label 精确回收；`reclaim.go` 能扫描 label 做宿主重启后的回收。

**runner 镜像：** 基于本机已有的 `ghost/kali:latest`（先记录其 digest 到 `runner/README.md`），装 pi 的 bundled node v22 运行时 + 安全工具。**本轮真构建**（用户决定）。

- [ ] **Step 1: 写失败测试** —— `spec_test.go`（无 tag）：断言 `ExecSpec` 渲染的 `docker run` argv 逐条含 `--read-only`/`--cap-drop=ALL`/`--security-opt=no-new-privileges`/非 root `--user`/`--pids-limit`/`--memory`/`--cpus`/网络名/label，且**不含**任何宿主状态目录的 `-v` 挂载。
- [ ] **Step 2: 跑测试确认失败。**
- [ ] **Step 3: 写实现**（`spec.go` 的 argv 渲染先于 `docker.go`）。
- [ ] **Step 4: 跑测试确认通过** —— `go test ./executor/... -count=1`。
- [ ] **Step 5: 构建 runner 镜像** —— `docker build -t red-harness-runner:v0.3.0 runner/`，记录基础镜像 digest 与产物 digest 到 `runner/README.md`。超时上限 30 分钟；若 OOM/超时，降级为「交付 Dockerfile + digest，构建延后」并在核验报告里记为**已知缺口**（不得记成「已通过」）。
- [ ] **Step 6:** 写 `docker_test.go`（`//go:build integration`）：只读文件系统写入被拒、资源限制生效、目标端点可达、非授权端点不可达、容器内 `pi --version` 可用、`Reclaim` 后容器与网络消失。

### Task 9: TSecBench Python bridge（agent C）

**文件：** 创建 `bridge/bridge.py`、`bridge/protocol.py`、`bridge/client.go`、`bridge/proc.go`、`bridge/wire.go`、`bridge/testdata/mock_sdk.py`、`bridge/client_test.go`、`bridge/proc_test.go`

**Interfaces:**
- Produces: `bridge.NewClient(cfg ClientConfig) (*Client, error)`，暴露 `CheckVPN/List/Start/Hint/Submit/Close`，满足 `harness.Platform` + `harness.HealthChecker`。

**wire 协议：** JSONL over stdio，一行一个请求/响应，带 `id` 与 `deadline`（RFC3339）。

```json
→ {"id":"1","cmd":"list","deadline":"2026-09-20T18:00:00Z"}
← {"id":"1","ok":true,"result":{...}}
← {"id":"1","ok":false,"error":{"code":"vpn_check_failed","class":"platform","retryable":false,"message":"...","statusCode":503}}
```

**命令名固定为 `check_vpn/list/start/hint/submit/close`**（PLAN.md:43；前身设计用的是 `op` 字段名，本计划统一为 `cmd`）。

**SDK 源码位置（已勘察核实，v0.1.2）：** `/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/204/fs/usr/local/lib/python3.14/dist-packages/tsec_benchmark/`（`snapshots/232` 有逐字节相同的副本）。文件：`__init__.py`(73L)、`client.py`(203L)、`sync.py`(144L)、`models.py`(137L)、`errors.py`(182L)、`_cli.py`(72L)、`_version.py`。
**注意：它在容器层里、面向 python3.14；宿主 python3.12 没有 `httpx`，`import tsec_benchmark` 在宿主上会 `ModuleNotFoundError`。** 这使「桥命令可覆盖」从便利项变成**必需项**（见下）。

**已核实的 SDK 契约（对着源码，不是对着文档）：**

| 项 | 核实结果 |
|---|---|
| 异步类 | `client.py:49` `class TSecBenchmarkAsync`；`__init__(base_url, token, *, timeout=30.0, transport=None, auto_check_vpn=True)` |
| 同步封装 | `sync.py:35` `class TSecBenchmark`；`__init__(base_url, token, *, timeout=30.0, auto_check_vpn=True)` |
| **能否不经预检构造** | ✅ **能。** 预检**只在** `__enter__`/`__aenter__` 里（`sync.py:83-86`、`client.py:84-87`）。`auto_check_vpn=False` 构造出的裸 client **零网络 I/O** 且完全可用。同步类直到首次 `_run` 才起线程/loop（`sync.py:66-74`），构造是惰性的。 |
| 六个方法 | `list_challenges()` / `start_challenge(unique_code)` / `get_hint(unique_code)` / `submit_flag(unique_code, flag)` / `close_challenge(unique_code)` / `check_vpn()`；另有 `close()`(sync) / `aclose()`(async) |
| 数据类字段 | **与文档声明零出入**（`models.py`，全部 `@dataclass(frozen=True)`）。注意 `SubmitResult` **没有** `unique_code` 字段。 |
| 异常 | 基类 `TSecError(Exception)`，类属性 `code` 默认 `"app_error"`；实例带 `.code`/`.message`/`.detail`(dict)/`.status_code`。`.code` 取值与文档一致：`task_not_found`/`challenge_not_found`/`invalid_state`/`duplicate`/`resource_unavailable`/`internal_error`/`validation_error`。**`VpnCheckError.code == "vpn_check_failed"`，`TSecConnectionError.code is None`。** |
| 鉴权头 | 字面量 `BENCHMARK_TOKEN: <token>`（`client.py:76`，`_TOKEN_HEADER`）——**不是** `Authorization` |
| 端点 | `GET /openapi/v1/challenges`；`POST .../start?unique_code=`；`GET .../hint?unique_code=`；`POST .../submit`（JSON body `{unique_code, flag}`）；`POST .../close?unique_code=`。URL 是朴素字符串拼接（`client.py:140`），非 urljoin。 |
| VPN 预检 | `GET http://10.0.100.58`，timeout 10.0，期望 HTTP 200 + JSON 且 `status == "ok"`；`detail["reason"]` ∈ `network_error`/`bad_status`/`bad_body`/`status_not_ok`（`client.py:115-126`） |
| 服务端对账 | `/tmp/tsec/TsecBench-main/tsecbench/api.py:112-162` 注册同样五条路由，`Header(alias="BENCHMARK_TOKEN")`，响应模型与数据类逐字段一致 ⇒ **文档与源码一致，可信** |

**桥必须处理的 SDK 缺陷（源码级，逐条要有测试）：**
1. **`from_dict` 抛裸 `KeyError`**（不是 `TSecError`）——畸形载荷会穿透所有异常处理。桥必须包住。
2. **`_handle_response` 对 2xx 无保护地调 `response.json()`**（`client.py:149-155`）——2xx 但非 JSON 的响应抛 `json.JSONDecodeError`，**逃出** `client.py:143-146` 的 `httpx.HTTPError` 网。桥必须包住。
3. **无 `atexit` 清理**——桥忘了 `close()` 就会留一个 daemon 线程 + 一个 httpx 连接池。
4. **`close()` 吞掉全部异常**（`sync.py:105-106`）。
5. **绝不要调 `tsec_benchmark._cli.main`**——它往 stdout 打印（`_cli.py:57-67`），会污染 JSONL 协议。
6. 默认 timeout 30s、VPN 探测 10s，**无重试逻辑**。

**六个命令的映射（`cmd` 字段名固定为 `check_vpn/list/start/hint/submit/close`，PLAN.md:43）：**
- `list` → `list_challenges()` → `Challenge{unique_code, difficulty, level, total_score, flag_count, correct_flag_count, is_completed, description, container_status, container_addr}`。注意 `container_addr` **仅当 `container_status == "available"` 时非空**。
- `start` → `start_challenge(code)` → `StartResult{unique_code, container_addr}`。
- `hint` → `get_hint(code)` → `HintResult{unique_code, hint}`，`hint` 可为 `None`。查看提示会按比例扣分。
- `submit` → `submit_flag(code, flag)` → `SubmitResult{correct, awarded, cumulative_score, correct_flag_count, total_flag_count, matched_flag_index}`。
- `close` → `close_challenge(code)` → `CloseResult{unique_code, closed}`。
- `check_vpn` → **独立的 `client.check_vpn()` 方法**（不是上下文管理器副产物）。

**错误码映射：** `VpnCheckError→vpn_check_failed`、`TaskNotFound→task_not_found`、`ChallengeNotFound→challenge_not_found`、`InvalidState→invalid_state`、`DuplicateSubmit→duplicate`、`ResourceUnavailable→resource_unavailable`、`InternalError→internal_error`、`ValidationError→validation_error`、`TSecConnectionError→connection_error`。**`DuplicateSubmit` 要映射成 `ok:true` + `duplicate:true`**，不是错误——它是幂等命中（`harness.go:550-552` 的语义）。`InvalidState` 含 `max active` 时可重试（`SDK_API.md:209-213`），任务已结束则不可重试。

**必须处理的桥特有风险（前身设计的既有结论，`purrfect-scribbling-bentley.md:485-507`）：**
- **长驻进程**：整个跑分期间一个 Python 进程（复用 httpx client，VPN 预检只做一次）；死亡则按退避重启并**重做预检**。
- **启动探活**：先 `python3 -c "import tsec_benchmark"`，缺依赖给**明确可操作的报错**，而不是等第一次调用炸。
- **桥命令必须可覆盖**（**本机是硬需求**）：默认 `python3 <repo>/bridge/bridge.py`，可用环境变量覆盖成 `docker exec <带 SDK 的容器> python3 -m bridge` 之类。**宿主 python3.12 缺 `httpx`，所以本机真跑的唯一路径是容器**——`doctor` 要把「宿主导入失败但容器可用」如实报出来，而不是笼统报「缺 SDK」。
- **`vpn_check` 在任何平台调用之前**（对齐 SDK 语义）。桥用 `auto_check_vpn=False` 构造裸 client，自己显式调 `check_vpn()`——**不要**用上下文管理器（它会在 `__enter__` 里隐式预检，让「预检失败」和「构造失败」无法区分）。

**Go 侧：** 常驻子进程；request ID 关联响应；deadline 到期返回 `KindPlatform` 可重试错误；子进程崩溃自动重启一次并重发未完成请求（PLAN.md:68）。

**安全：** token、base URL、flag **不进入普通日志**。`proc.go` 的 stderr 重定向到 `private/`，不继承宿主 stderr。**特别注意 `/tmp/tsec/TsecBench-main/.agent.env` 里有一个看起来有效的 `BENCHMARK_TOKEN`——不得读它的值、不得把它写进任何测试夹具或日志。**

**离线测试：** `bridge/testdata/mock_sdk.py` 提供假 `tsec_benchmark` 模块（同样的数据类与异常），测试用 `TSEC_MOCK=1` + `PYTHONPATH=testdata` 驱动。**不需要真实 VPN 与 SDK** 就能测全部字段映射、请求关联、超时、崩溃重启、平台错误码。

- [ ] **Step 1: 对照源码核对** —— **已完成（见上表）**：SDK 在容器层、面向 py3.14；宿主 py3.12 无 `httpx`。实施者只需复核上表并把它抄进 `bridge/README.md`。
- [ ] **Step 2: 写失败测试** —— `List` 字段完整映射；`Submit` 遇 `DuplicateSubmit` 返回 `Duplicate:true` 且 `err == nil`；deadline 到期返回可重试错误；杀掉 mock 子进程后下次调用自动重启成功并重做预检；`InvalidState` 映射到 `invalid_state` 且可重试性按 `max active` 区分；`hint` 为 `None` 时不报错；**畸形载荷（缺字段）被包成结构化错误而不是裸 `KeyError` 穿透**；**2xx 非 JSON 响应被包住而不是 `JSONDecodeError` 穿透**。
- [ ] **Step 3: 跑测试确认失败。**
- [ ] **Step 4: 写 `protocol.py` + `mock_sdk.py` + `wire.go` + `proc.go` + `client.go` + `bridge.py`。**
- [ ] **Step 5: 跑测试确认通过** —— `go test ./bridge/... -count=1`。
- [ ] **Step 6:** 手工冒烟：`echo '{"id":"1","cmd":"check_vpn"}' | PYTHONPATH=bridge/testdata python3 bridge/bridge.py` 必须回一行合法 JSON。

### Task 10: CLI 骨架（agent D）

**文件：** 创建 `cmd/red-harness/main.go`、`internal/cli/cli.go`、`internal/cli/run.go`、`internal/cli/control.go`、`internal/cli/report.go`、`internal/cli/serve.go`、`internal/cli/doctor.go`、`internal/cli/cli_test.go`

**Interfaces:**
- Produces: `cli.Main(args []string, stdout, stderr io.Writer) int` —— 可测试入口（`main.go` 只做 `os.Exit(cli.Main(...))`）。

**子命令（PLAN.md:58）：** `doctor`、`list`、`run`、`resume`、`pause`、`cancel`、`serve`、`report`。标准库 `flag`，每个子命令一个 `flag.FlagSet`。`pause`/`cancel` 通过 run 目录下的 Unix control socket（`0600`，PLAN.md:40）连接运行中的进程。

**本波次只交付骨架**：全部子命令能解析并返回 `KindConfig` 的「未实现」错误；`serve`/`report` 的完整实现在 T15/T16，`doctor` 的完整实现在 T14。CLI 对 engine 的依赖通过**注入的接口**（本包内定义的窄接口，例如 `type engineFactory interface{ Start(...) ... }`），避免本波次编译依赖 T11。

- [ ] **Step 1: 写失败测试** —— `Main([]string{"--help"})` 返回 0 且输出含全部 8 个子命令名；`Main([]string{"bogus"})` 返回非 0；`Main([]string{"run", "--budget-rounds=abc"})` 返回非 0 且消息指明 flag 名。
- [ ] **Step 2: 跑测试确认失败。**
- [ ] **Step 3: 写实现。**
- [ ] **Step 4: 跑测试确认通过** —— `go test ./internal/cli/... ./cmd/... -count=1`。

---

# W2：引擎与适配（4 agent 并行，包级互斥）

### Task 11: engine 包——单写者内核 + 恢复 + 控制面（agent A）

**文件：** 创建 `engine/engine.go`、`engine/runloop.go`、`engine/singlewriter.go`、`engine/budget.go`、`engine/resume.go`、`engine/replay.go`、`engine/control.go`、`engine/fake.go`（测试用 fake 端口）+ `engine/runloop_test.go`、`engine/resume_test.go`、`engine/control_test.go`

**为什么内核+恢复+控制是**一个**任务：** 三者共用同一个 `runLoop` 与同一份 run 状态，在同一包内。拆成两个 agent 会让「同一包并行编辑」成为必然冲突。

**依赖约束（关键）：** `engine/` **只导入根包 `harness` + `store/`**，不导入 `dag`/`gate`/`piai`/`bridge`/`scenario`。默认的 gate/planner/renderer 实现通过 `harness.Options` 的工厂字段**注入**（见 T2 的 `Options` 注释）。这样 engine 的 agent 不会被其他包的在建状态卡住——这是「一个子包 = 一个 agent」在本任务上的具体落地。

**Interfaces:**
- Consumes: `Store`、`Planner`、`Renderer`、`CandidateGate`、`Platform`、`AgentFactory`、`Scenario`、`RunPolicy`。
- Produces: `engine.New(opts harness.Options) (harness.Engine, error)`（在 `engine` 包内实现，供 `internal/cli` 与 `example` 使用）。

**单写者模型（PLAN.md:35）：** 一个 `runLoop` goroutine 是**唯一**改状态的地方。全部输入（agent 事件、平台结果、控制命令）走 channel 进 loop。`EventSink` 的实现把事件写进 loop 的 `events chan Event`（带缓冲，满了阻塞——这是有意的背压，避免 `harness.go:520-528` 那种在 reader 协程里做任意用户代码的情形）。

**轮循环（保留 v0.2 的语义与分支顺序，`harness.go:362-490`）：**
1. 轮首通关判定：`(ch.FlagCount > 0 && len(confirmed) >= ch.FlagCount) || (ProgressTotal > 0 && ProgressConfirmed >= ProgressTotal)` → `ReasonSolved`。两个判据取**或**：`FlagCount` 可能缺失（`StartResult` 不携带它），平台进度来自每次 `Submit` 的返回值、总是权威。
2. 预算判定：`Budget.Exhausted(used)`，`used` 用**本轮实时**的 turns/cost（修掉 `harness.go:375-380` 成本恒为 0 的缺陷——v0.2 只在循环结束后才取 `Stats`）。
3. `Planner.Next` → nil 则 `ReasonNoIntent`。
4. `Planner.Activate` + `EvIntentActive`。
5. `Renderer.Render`。
6. `Agent.Round(ctx, RoundRequest{Prompt, Round, IntentID, Timeout})`。
7. **分支顺序不可动**：`ctx.Err()` → `ReasonStopped`；`turns==0 && Err!=""` → `ReasonProviderFailure`；`err != nil` → `ReasonError`；`ProviderError != ""` → `ReasonProviderFailure`；`Err != ""` → `ReasonError`。
8. `Planner.Settle` + `EvIntentSettled`。
9. 落盘：`Store.Append`（先事件后快照）。
10. dry 计数 → 提示策略（**修掉 `HintAlways` 每轮都请求的缺陷**：`HintAlways` 也受「每题一次」守卫）。
11. `harvest`：轮末提交，语义完全沿用 `harness.go:532-572`——去重、ledger 永不重提、`Duplicate` 计确认、`Submitted` = **去重确认数**、平台进度只在 `> 0` 时覆盖、提交出错既不计确认也不记入判错账本。

**`OnEvent` 的调用点：** 在 loop goroutine 上、带 `recover()`。**不再有 `Observer`**（接口已删）——DAG 的事实抽取走 `Planner.ObserveEvent`，在同一条事件路径上调用。只有一个消费者，不会两处改状态。

**恢复语义（PLAN.md:38-39）：**
- 以快照 `lastAppliedSeq` 为基线重放 `events.jsonl`。
- **fail closed 三种情形**：`RunSpec.Digest()` 与 `Snapshot.SpecDigest` 不一致 → `KindConfig` 并**指出漂移字段**（Review Focus #5）；`private/` 账本缺失 → `KindPersistence`（Review Focus #2）；`SchemaVersion` 不支持 → `KindConfig`。
- **对终态 run 调 `Resume`：** 返回 `KindConfig` 错误（`RunCompleted`/`RunFailed`/`RunCancelled` 一律拒绝），**不重跑**（Review Focus #1）。
- 暂停：abort 当前 round，把 intent 标记为 `interrupted` 并落盘（`EvRunPaused`）；恢复后按已知事实重新调度，**不假定中断动作成功**。
- 恢复后**先 `Scenario.Reconcile` 对账**再决定是否重试平台写操作（Review Focus #3）——禁止盲目重发 `start`/`submit`/`close`。`Reconcile` 返回的平台进度以平台为准，覆盖本地推测。

**控制面（PLAN.md:40）：** run 进程创建权限 `0600` 的 Unix control socket，供其他 CLI 进程执行 pause/resume/cancel。socket 路径在 run 目录下，`control.go` 实现 server，客户端由 T14 接进 CLI。

- [ ] **Step 1: 写失败测试** —— **先移植 W0 归档的七个回归用例**（`docs/superpowers/plans/fixtures/v0.2-*.txt`，**逐条保留原断言，只改类型**）。再加：`TestEngineMaxCostBudgetIsLive`（每轮上报成本，断言在第 N 轮因 `ReasonMaxCost` 终止——钉死旧缺陷）；`TestEngineHintAlwaysRequestsOnce`；`TestResumeRejectsTerminalRun`；`TestResumeRejectsDigestDrift`；`TestResumeRejectsMissingPrivateLedger`；`TestResumeAfterTornEventLine`（真杀进程写半行）；`TestResumeDoesNotResubmitConfirmed`（计数的 fake platform，断言 `Submit` 调用次数）；`TestInterruptedIntentNotAssumedSuccessful`；`TestResumeReconcilesBeforeRetry`。
- [ ] **Step 2: 跑测试确认失败。**
- [ ] **Step 3: 写实现。**
- [ ] **Step 4: 跑测试确认通过** —— `go test ./engine/... -count=1 -race`。

### Task 12: pi agent 适配（agent B）

**文件：** 修改 `piai/agent.go`、`piai/proc.go`、`piai/env.go`、`piai/watchdog.go`、`piai/frames.go`、`piai/agent_test.go`、`piai/env_test.go`；创建 `piai/factory.go`、`piai/factory_test.go`

**Interfaces:**
- Produces: `piai.NewFactory(cfg FactoryConfig) harness.AgentFactory`。

**必须保持的既有硬事实（M0 实测，全部有测试钉着）：**
- pi 不在 PATH；真身 `/root/.local/share/pi-node/node-v22.23.2-linux-x64/bin/pi`（`piai/proc.go:47`）。系统 node 是 v18，pi 的 shebang 是 `#!/usr/bin/env node`，所以 `childEnv` **必须**把 bundled node 目录前置到 `PATH`（`env.go:86`）。
- 非交互模式必须**绝对路径 `-e`** + 显式 `--approve`，否则 extension 静默失效。
- **保留 `buildArgs` 的 argv 顺序**（`piai/proc.go:566`，`TestBuildArgs` 逐字断言）：`--mode rpc` → `--session-dir` → `--provider`/`--model` → 每个 `-e <绝对路径>` → `--approve` → `--thinking` → `--append-system-prompt`。
- **启动即校凭据**（`agent.go:238`）：`UPPER(provider, '-'→'_') + "_API_KEY"`，`opencode-go`/`opencode` 有别名 `OPENCODE_API_KEY`/`OPENCODE_GO_API_KEY`。401 会以**静默空会话**形式出现，所以缺凭据必须在启动前拒。
- **版本闸**（`proc.go:65`）：`DefaultVersionRange = {Min:"0.85.0", Max:"0.86.99"}`，闭区间。
- **extension 自检**（`agent.go:286`）：`Extensions` 非空时 `get_commands` 必须含一个 `source == "extension"` 的命令，否则在 `new_session` 之前失败。
- `report_fact` 的结构化事实走 `tool_execution_end.result.details`（262 KB 不截断）。
- **不要改传输层**（`proc.go` 的 `frameQueue` 单写者、`frames.go` 的 `FrameReader` LF-only 解析、watchdog 的「探活成功但状态没变不算卡死」判定）。54 个测试钉着它们，且都是真管道上实测出来的（`TestWriterDoesNotDeadlock` 只在真管道上复现死锁）。
- **provider 故障的轮级隔离**：`Err` 是「本轮任何异常」的混合字段，`ProviderError` 是干净的 provider 判据，两者刻意分开——第 1 轮的 401 不得污染第 3 轮（`TestProviderErrorIsRoundScoped`）。

**必须修的缺陷：stderr 诊断缺口。** `readStderr`（`proc.go:350`）把 pi 的 stderr 收进 `stderrTail`，但 `proc.stderr()`（`proc.go:365`）**无任何调用者**。pi 的版本/凭据/extension 错误全在 stderr——不接出来这类故障无法诊断。把 stderr 尾部接进启动失败与 `ErrProcessDied` 的错误消息里。

**改动：** `Round` 签名从 `(ctx, prompt string, emit func(Event))` 改为 `(ctx, req RoundRequest)`，事件通过 `AgentFactory.New(spec, sink)` 注入的 `EventSink` 推送。

- [ ] **Step 1: 写失败测试** —— `factory_test.go`：`Factory` 按 `AgentSpec` 渲染 argv，断言含绝对路径 `-e`、`--approve`、`--provider`/`--model`、`--append-system-prompt`；加 `TestStartFailureIncludesStderr`（stubpi 新增一个场景往 stderr 写一行后退出非 0，断言错误消息含该行）。**保留 `piai/agent_test.go` 的全部既有断言**（改签名，不改断言）。
- [ ] **Step 2–4:** 标准 TDD 循环，`go test ./piai/... -count=1 -race`。

### Task 13: TSecBench 场景适配（agent C）

**文件：** 创建 `scenario/tsec.go`、`scenario/fake.go`、`scenario/tsec_test.go`、`scenario/fake_test.go`

**Interfaces:**
- Produces: `scenario.NewTSec(p harness.Platform, cfg TSecConfig) harness.Scenario`；`scenario.NewFake(cfg FakeConfig) harness.Scenario`。

**六方法映射（PLAN.md:21）：**
- `Discover` → bridge `list`，过滤 `is_completed`（已通关的跳过）。
- `Prepare` → bridge `start` + 把 `container_addr` 填进 `Target.Addrs`。注意 `start` 是异步的——`container_status` 从 `pending` 到 `available` 需要轮询（`SDK_API.md:129-130`）。
- `Hint` → bridge `hint`；`None` 时返回空串。
- `Evaluate` → bridge `submit` → `Evaluation{Accepted: correct, Progress: correct, Completed: correct_flag_count == total_flag_count, Score: awarded, Message}`。**`DuplicateSubmit` 映射成 `Accepted: true`**（幂等命中，`harness.go:550-552`）。
- `Reconcile` → bridge `list` 后按 code 比对平台进度与容器状态，返回平台权威 `Objective`。
- `Cleanup` → bridge `close`；错误单独记录，**不覆盖主要终止原因**（PLAN.md:47）。

**默认提示策略（PLAN.md:46）：** 连续三轮无进展后最多请求一次——沿用 `harness.go:499-517` 的 `HintAuto` 语义（`dry >= 3` 且本题未用过）。

- [ ] **Step 1: 写失败测试** —— 用 `scenario/fake.go` 驱动：`Evaluate` 把 `Duplicate` 映射成 `Accepted:true`；`Reconcile` 以平台进度为准；`Cleanup` 错误不覆盖主要终止原因；`Discover` 跳过 `is_completed`；`Prepare` 轮询到 `available` 才返回地址。
- [ ] **Step 2–4:** 标准 TDD 循环，`go test ./scenario/... -count=1`。

### Task 14: CLI 完整实现 + doctor（agent D）

**文件：** 修改 `internal/cli/doctor.go`、`run.go`、`control.go`、`cli.go`、`cli_test.go`；创建 `internal/cli/wire.go`、`internal/cli/doctor_test.go`

**Interfaces:**
- Consumes: `engine.New(harness.Options)`（T11）、`bridge.NewClient`（T9）、`executor.NewDocker`（T8）、`piai.NewFactory`（T12）、`scenario.NewTSec`（T13）、`control.Client`（T11）。

**`doctor` 检查项（PLAN.md:59），任何缺失都在平台写操作前失败、退出码非 0：**
1. **Python SDK 可达性（分两级，不要笼统报「缺 SDK」）** —— 宿主 `python3 -c "import tsec_benchmark"`；失败时再探「桥命令覆盖是否指向一个可用的容器」。SDK 装在容器层且面向 py3.14，**宿主 py3.12 缺 `httpx`，宿主导入必然失败**——所以「宿主失败 + 容器可用」是**正常配置**，必须报成通过（附说明），而不是报成缺依赖。
2. 凭证：`BENCHMARK_TOKEN`/`BENCHMARK_BASE_URL` 是否设置（**不打印值**）。
3. VPN：bridge `check_vpn`。
4. Docker：`executor.Available`。
5. runner 镜像：`docker image inspect`。
6. pi 版本：复用 `piai` 的 `DiscoverBin` + `checkVersion`。
7. provider 配置：`.env` 的 `PI_PROVIDER`/`PI_MODEL` + **启动即校凭据**（防 401 静默烧钱）。

**`wire.go`：** 把七个端口装配成 `harness.Options`。这是**唯一**一处把具体实现绑到接口的地方——保持它薄且可读。

- [ ] **Step 1: 写失败测试** —— `doctor_test.go` 用注入的探针断言缺 SDK / 缺 Docker / 缺凭据 / pi 版本越界各自退出码非 0 且消息指出缺哪一项；`cli_test.go` 加：`run --scenario fake --targets demo-1` 能跑完并落盘；`pause` 经 control socket 生效。
- [ ] **Step 2–4:** 标准 TDD 循环，`go test ./internal/cli/... ./cmd/... -count=1`。
- [ ] **Step 5:** 把 `serve`/`report` 两个子命令接到 T15/T16 的实现（若那两个任务已完成）；否则保持「未实现」并在 T18 收尾时接线。

---

# W3：交付面（4 agent 并行，包级互斥）

### Task 15: 本地看板（agent A）

**文件：** 创建 `web/server.go`、`web/sse.go`、`web/assets/index.html`、`web/assets/app.js`、`web/assets/style.css`、`web/server_test.go`

**Interfaces:**
- Consumes: `RunHandle.Events(afterSeq)`、`Snapshot`、`RunSummary`。
- Produces: `web.NewServer(cfg ServerConfig) (*Server, error)`。

**约束（PLAN.md:60-61）：** Go `embed` 静态资源；SSE 支持 `Last-Event-ID`；默认只监听 `127.0.0.1`；随机 bearer token；**只允许 pause/resume/cancel**，不允许注入命令或编辑事实。展示 DAG、当前 intent、预算、事实、候选分族和错误。

- [ ] **Step 1: 写失败测试** —— SSE 重连带 `Last-Event-ID` 只补发增量；无 token 的 pause 返回 401；已结束 run 的页面只读；响应体**不含**候选明文（泄漏面）。
- [ ] **Step 2–4:** 标准 TDD 循环，`go test ./web/... -count=1 -race`。

### Task 16: 报告生成（agent B）

**文件：** 创建 `report/report.go`、`report/markdown.go`、`report/json.go`、`report/report_test.go`

**Interfaces:**
- Produces: `report.Build(snap harness.Snapshot, view harness.OutcomeView) (Report, error)`、`report.Write(dir string, r Report) error`。

**内容（PLAN.md:62）：** 得分、预算、终止原因、平台进度、候选来源和证据引用；**flag、凭证及原始工具输出保持脱敏**——只给 `answer.Fingerprint` 指纹（`report` 因此导入 `answer`，这是允许的：`answer` 是叶子包）。

- [ ] **Step 1: 写失败测试** —— 报告 JSON 与 Markdown 都不含明文（假 flag 断言 `!bytes.Contains`）；证据引用指向 `private/evidence/` 的相对路径。
- [ ] **Step 2–4:** 标准 TDD 循环，`go test ./report/... -count=1`。

### Task 17: 端到端离线示例（agent C）

**文件：** 创建 `example/main.go`、`example/README.md`、`example/e2e_test.go`

**内容（PLAN.md:63）：** fake TSecBench + stub pi 的一键本地示例，以及真实 VPN 环境的运行说明。

- [ ] **Step 1: 写失败测试** —— `e2e_test.go` 走完整生命周期：`list→start→solve→submit→close`、重复候选、错误提交、自动提示、暂停恢复、进程崩溃恢复、最终 cleanup。用 `scenario.NewFake` + `piai` 的 `stubpi`。
- [ ] **Step 2–4:** 标准 TDD 循环，`go test ./example/... -count=1 -race`。
- [ ] **Step 5:** 手工验证 `go run ./example` 能一键跑完并打印报告路径。

### Task 18: 收尾——死代码清理与文档（agent D）

**文件：** 修改根包与各子包的散落死代码；创建 `README.md`；修改 `docs/migration-v0.2-to-v0.3.md`；修改 `internal/cli/serve.go`、`report.go`（接 T15/T16 的实现，若 T14 时它们还没就绪）

**已确认的死代码，逐个处置（不得留悬）：**

| 位置 | 现状 | 处置 |
|---|---|---|
| `solver.go:44` `DefaultFlagFormat` | 从未被引用 | T3 已删除 |
| `solver.go:50` `ReasonStalled` | 从未被写入 | **保留常量**（Reason 字符串是 API），README 标注「v1 未使用」 |
| `Outcome.IntentDone`/`Negative` | 从未被写入 | T3 已改为真实回填（来自 `dag.Graph.Stats()`） |
| `Outcome.Report` | 从未被写入 | T16 的报告路径回填 |
| `platform.go:23,31` `Challenge.Remaining`/`Done` | 零调用者 | 保留（纯函数，`Objective` 会用到） |
| `gate/provenance.go:564` `itoa` | 零调用者 | 删除 |
| `piai/proc.go:365` `proc.stderr()` | 零调用者 | T12 已接线 |
| `piai/proc.go:251` `frameQueue.depth()` | 零调用者 | 删除，或接进 watchdog 的观测事件 |
| `piai/frames.go:35,41,43,44` `CmdFollowUp`/`CmdClearQ`/`CmdThinkLvl`/`CmdAutoRetry` | 从未使用 | **保留常量**（pi 协议里的合法命令），但删掉从未赋值的 `Command.Mode`/`Command.Level` 字段 |
| `dag/node.go:205` `Node.Expects` | 无生产调用点，注释说 M5 要用 | 接进 T11 的轮循环（agent 申报 kind 与预期不符时提示换类别）或删除——**二选一，不得留悬** |
| `gate/Ledger.SortedFingerprints` | 零调用者 | 接进 T16 报告，或删除 |
| `dag.Produced`/`EnabledFrom`/`DerivedFrom`、`dag.EnvelopeHit`/`AnswerShaped`/`ShapeOf`/`Describe`、`gate.Shape`/`Fabrications`/`FormatRejected`、`gate.Ledger.Reasons` | 仅测试调用 | 保留（是测试的观测面），README 不必提 |

**必须逐条核对 PLAN.md:26-82 每一项都有落地**，并在 README 写明「v1 明确不纳入」的六项（PLAN.md:9）。

**不要为了让文档好看而写不属于本项目的内容。** 具体地：不要创建 `docs/superpowers/specs/` 下除了 T1 那份设计文档以外的文件，不要写「路线图」「未来工作」「贡献指南」这类无人要求的小节，不要把 `PLAN.md` 的文本重新抄一遍。README 只写**使用者需要知道的**：怎么跑 `doctor`、怎么跑一道题、怎么恢复、v1 不做什么。

- [ ] **Step 1:** 按上表逐条处置。`grep -rn "Solver\|Session\.\|Outcome{" --include="*.go"` 必须零命中（除注释）。
- [ ] **Step 2:** `gofmt -l . && go build ./... && go vet ./... && go test ./... -count=1 -race` 全绿。
- [ ] **Step 3:** 写 README，含「v1 不纳入」六项与 `ReasonStalled` 未使用说明。

---

# W4：核验

### Task 19: 独立核验 agent（1 个，跑完全部断言）

**不是**「跑一遍测试」——核验 agent 必须**独立复现**下列断言，用自己的方法（写一次性探针程序、读源码逐行对照、构造对抗输入），并报告每一条的**原始证据**：

1. **单写者：** 全局只有 `runLoop` goroutine 改 `Snapshot`——用一个故意在 `OnEvent` 里改状态的探针 + `-race`，断言不污染。
2. **恢复 fail-closed 三种情形**各独立复现一次（摘要漂移 / 账本缺失 / schema 不支持）。
3. **`events.jsonl` 半行恢复**——真杀进程，不模拟。
4. **候选明文不泄漏**：`grep` 真跑出来的假 flag 明文，断言在 `run.json`/`events.jsonl`/`report.json`/`report.md`/看板 HTML 中**零命中**。
5. **恢复后不重复提交**已确认 flag：用会计数的 fake platform，断言 `Submit` 调用次数。
6. **Docker 隔离六条硬约束**逐条真跑（`-tags integration`）。
7. **bridge 的字段映射与错误码逐字段对照真实 SDK 源码**（`.../dist-packages/tsec_benchmark/models.py` 与 `errors.py`，**不是**对照 `SDK_API.md`，也不是对照我写的代码）。特别核对：`SubmitResult` 没有 `unique_code` 字段；`VpnCheckError.code == "vpn_check_failed"`；`TSecConnectionError.code is None`；鉴权头是 `BENCHMARK_TOKEN` 而非 `Authorization`。
8. **旧 `graph.json`（schema 1）仍可 `Load`**。
9. **`answer.Fingerprint` 是唯一真源**：`dag.FlagFingerprint` 与 `gate.Fingerprint` 都是转发。
10. **`dag.Render` golden 文件**能 `UPDATE_GOLDEN=1` 重生成且只有预期内 diff。
11. **`stateFileReCI` 收窄生效**：上表七个命令逐条真跑。
12. **`piai` 的 stderr 接出来了**：stubpi 往 stderr 写一行后退出非 0，断言错误消息含该行。
13. **没有凭据泄漏**：`grep` 全仓 + 全部 run 产物（`run.json`/`events.jsonl`/`report.*`/看板 HTML/stderr 重定向文件），确认不含 `/tmp/tsec/TsecBench-main/.agent.env` 里那个 `BENCHMARK_TOKEN` 的明文，也不含 `.env` 里 `OPENCODE_API_KEY` 的明文。

**核验 agent 必须清理自己创建的临时文件。**

**核验 agent 的独立性要求：** 它**不得**读取本计划文件、也不得读取实施 agent 写的任何总结或注释来「确认」结论。每一条断言都要从**被测代码本身**（源码 + 可执行行为）重新推导。发现实施 agent 的注释与实际行为不符时，**以实现为准**并单独列出。

### Task 20: 真实 pi 最小冒烟（与核验并行）

**用户决定：跑。** 目的是验证 T12 改了 `Round` 签名之后 pi 接入没被弄坏——这是 stubpi 覆盖不到的那部分。

**前置：** `.env` 里 `OPENCODE_API_KEY` / `PI_PROVIDER=opencode-go` / `PI_MODEL=deepseek-v4-flash` 已存在（值不打印）。pi 真身在 `/root/.local/share/pi-node/node-v22.23.2-linux-x64/bin/pi`（v0.86.0）。

**必须用真 pi 验的断言（M0 实测过、v0.3 必须仍成立）：**
1. `Start` 起进程、握手、`new_session` 后会话确实复位（`get_state` 断言 `sessionId` 变了且 `messageCount == 0`）。
2. 一轮 prompt 后 `RoundResult.Turns > 0`（**不是** 0 回合——0 回合 + 有错误就是 provider 静默烧钱模式）。
3. `get_session_stats` 字段完整映射进 `Stats`。
4. 非交互模式下 extension 真的加载了（绝对路径 `-e` + `--approve`；不生效时是**静默**的）。
5. `Close` 后进程组确实被杀（`killpg`）——pi 不会自己退出。
6. **每题独立 `HOME` 生效**：设 `AgentSpec.HomeDir` 到一个临时目录，断言 pi 的 `$HOME/.pi/agent/*` 发现路径确实被改写（前身确认过共享 HOME 会跨题污染）。

**这条冒烟会真实消耗 token。** 用最小 prompt（例如「回复 ok」），一轮即止，预算设 `MaxRounds: 1`。若凭据失效（401），必须**报失败**，不得记为跳过——M0 记录过凭据可能已失效。

### Task 21（可选，需换机器）: 真实 TSecBench 冒烟

仅在显式提供 `BENCHMARK_TOKEN`/`BENCHMARK_BASE_URL` 且 VPN 连通时启用（PLAN.md:73）。**本机不可能**——无 VPN、无 SDK。验收要求：至少完成一道受控题目的完整生命周期。

---

## 验证

**每个波次的门（协调者在主上下文跑）：**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./... -count=1
```

**全量验收：**

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
gofmt -l .                                          # 必须无输出
go test -tags integration ./executor/... -count=1   # 需要 Docker，且 runner 镜像已构建
go run ./example                                    # 端到端离线一键跑通
```

**端到端手工验证（离线）：**

```bash
# 1. doctor：宿主 py3.12 缺 httpx ⇒ 必须指出「宿主导入失败」，并说明容器路径是否可用
go run ./cmd/red-harness doctor; echo "exit=$?"   # 预期非 0，且消息指出缺哪一项

# 2. 用 fake scenario 跑一道题
go run ./cmd/red-harness run --scenario fake --targets demo-1 --store /tmp/rh-demo

# 3. 中途暂停再恢复
go run ./cmd/red-harness pause --store /tmp/rh-demo --run <id>
go run ./cmd/red-harness resume --store /tmp/rh-demo --run <id>

# 4. 看板（打印带 token 的 127.0.0.1 URL）
go run ./cmd/red-harness serve --store /tmp/rh-demo

# 5. 报告脱敏
go run ./cmd/red-harness report --store /tmp/rh-demo --run <id>
grep -rF 'flag{' /tmp/rh-demo/runs/<id>/run.json /tmp/rh-demo/runs/<id>/events.jsonl   # 零命中
```

## 未决与风险

- **本机 2 核 3 GB：** T8 的 runner 镜像构建已定为本轮执行，但有超时/OOM 风险；Step 5 已写明降级路径与「记为已知缺口而非已通过」的要求。
- **本机无 VPN，但 `tsec_benchmark` SDK 源码已安装可读**（在容器层、面向 py3.14；宿主 py3.12 无 `httpx`）。这意味着 T9 的**契约可以对着真源码写**（已核实，见 T9 的表），比对着文档写可靠。但**真实网络行为仍无法验证**（无 VPN、`10.0.100.58` 不可达），所以 T9 的验收仍是 mock 驱动的离线测试，不是真跑。这是已知缺口，不是「已通过」。
- **`/tmp/tsec/TsecBench-main/.agent.env` 含看起来有效的 `BENCHMARK_TOKEN` 与 API key。** 任何 agent 都不得读取其值、不得写进日志/报告/事件/测试夹具。核验 agent（T19）要把这条列为一项检查：`grep` 全仓与全部 run 产物，确认没有该 token 的明文。
- **真实 pi 冒烟会花 token，且凭据可能已失效（M0 记录过 401）。** 若失败，T20 必须报失败而不是静默跳过。
- **git 已初始化（T0），按任务提交。** 无法回滚单个任务的问题已解决。但**`.gitignore` 必须先于任何 `git add` 就位**，且**绝不 `git add -A`**——`.env` 里有真实的 `OPENCODE_API_KEY`。
- **`answer.Shape` 的 JSON 兼容：** 若现存 `graph.json` 无法 `Load`，是静默破坏（`dag/store.go:32` 无 tag 嵌入）。T4 的 fixture 测试是这条的唯一防线。
- **`dag.Render` golden：** 若 diff 未被人工确认，渲染契约的回归会静默通过。
- **`bash_guard.ts`（前身 307 GB 磁盘事故的对策）不在本计划范围内。** 前身记录的对策是「强制 `timeout -k 15 -s KILL 600 bash -c '...'` + `ulimit -f` + 自引用重定向检测，用 extension API 而**不要** monkeypatch pi 的 `bash.js`」，蓝本在 `/tmp/base_72ae35f/packages/worker/ghost_worker/pi_ext/bash_guard.js`。本计划的 5 个里程碑里没有「pi extension 交付物」这一项，而验证它必须用真 pi + 真实命令。**本计划的 Docker 隔离（T8）在资源限制上部分覆盖了同一风险**（`--pids-limit`/`--memory`/`--cpus` + 墙钟），但**不覆盖磁盘写入量**——只读 rootfs + tmpfs 会限制容器内可写空间，这是唯一挡住「写满磁盘」的机制。**若用户需要 extension 级护栏，那是独立于本计划的交付物。**
- **每题独立 `HOME` 是前身确认的事故源（跨题污染），本计划通过 `AgentSpec.HomeDir` 落地，但真实效果未在本机验证**——它需要真 pi 才能确认 `$HOME/.pi/agent/*` 的发现路径确实被改写。T20 的冒烟要顺带确认。
- **任务数口径：** 本计划共 **22** 个任务（T0–T21），**并发波次 5 个**（W0–W4）。W0 是单点串行且工作量最大（git 初始化 + 契约 + 根包换血 + dag/gate 迁移 + 两个缺陷修复）——这是本计划的固有瓶颈，不是可以优化掉的。
