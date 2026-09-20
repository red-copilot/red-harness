# v0.2.0 → v0.3.0 迁移表

> 适用对象：已经在用 v0.2 的 `harness.Session` 的调用方。
> 设计依据：`docs/superpowers/specs/2026-09-20-offensive-security-harness-design.md`。
> v0.2 的基线提交是 `f9718a3`，本文里的行号都指向那一版。

## 一句话

`Session.Run(ctx, code) (Outcome, error)` → `Engine.Start(ctx, RunSpec) (RunHandle, error)`。
**一次性求解后端（`Solver`）被删除**，状态改由引擎独占持有，运行可以被暂停/恢复/取消。

## 1. 顶层入口

| v0.2 | v0.3 |
|---|---|
| `&harness.Session{...}` 手工填 20 个字段 | `harness.New(harness.Options{...})` → `Engine` |
| `sess.Run(ctx, "web-01")` → `(Outcome, error)` | `eng.Start(ctx, RunSpec{Targets: []string{"web-01"}})` → `(RunHandle, error)` |
| 一道题一次 `Run` | 一次 `Start` 可以跑多道题（`RunSpec.Targets`） |
| 没有恢复 | `eng.Resume(ctx, runID)`；以事件日志重放为基线 |
| 没有控制面 | `RunHandle.Pause/Resume/Cancel` |
| 没有列表 | `eng.List(ctx)` → `[]RunSummary` |
| 没有体检 | `eng.Doctor(ctx)` → `DoctorReport` |

### 端口从「字段」变成「Options」

| v0.2 字段 | v0.3 去向 |
|---|---|
| `Session.Platform` | `Options.Platform` |
| `Session.Agent` | `Options.Agents`（**工厂**：每题一个 agent） |
| `Session.Gate` | `Options.Gate`（**工厂**：`func(Challenge) CandidateGate`） |
| `Session.Scheduler` | `Options.Planner`（工厂） |
| `Session.Renderer` | `Options.Renderer`（工厂） |
| `Session.Ledger` | 由 gate 实现内部持有（`gate.NewLedger`）；不再单独注入 |
| `Session.OnEvent` | `Options.OnEvent` |
| `Session.Now` | `Options.Now` |
| `Session.Budget` | `RunSpec.Budget` |
| `Session.Submit` | `RunSpec.Submit` |
| `Session.HintPolicy` | `RunSpec.HintPolicy` |
| `Session.Workdir` | `RunSpec.Executor.Workdir` |
| `Session.Challenge` | `RunSpec.Targets` 选中后由 `Scenario.Discover` 提供完整 `Challenge` |
| `Session.Prompt` | **删除**。它只在 `runOneShot`（Solver 路径）里被用过（`harness.go:323-324`），而那条路径整个删掉了。要覆盖「本题」段就实现自己的 `Renderer` |
| `Session.Solver` | **删除**，见 §4 |

**为什么 `Gate`/`Planner`/`Renderer` 变成工厂：** 一道 run 可以跑多道题，而这三个
是**每题独立**的（v0.2 里 `gate.NewGate(desc)` 与 `dag.NewScheduler(g)` 就是每题
新建）。工厂签名统一是 `func(Challenge) T`。

### 接线字段并入接口

v0.2 有五个「漏接是静默的」字段。它们现在要么进接口（漏接变编译错误），要么被
引擎接管：

| v0.2 字段 | v0.3 去向 | 漏接在 v0.2 的表现 |
|---|---|---|
| `Session.Ingest func(Event, int)` | `Planner.ObserveEvent(Event, int)` | DAG 零事实，7 个阶段只有前 4 个可达，所有包自测全绿 |
| `Session.IntentSink func(string, int)` | `CandidateGate.SetIntent(string, int)` | 候选 `IntentID` 全空、`Round` 全 0；族别判定不受影响所以**不报错**，但推导链锚点丢失 |
| `Session.Saver Saver` + `Session.GraphPath string` | 引擎每轮调 `Store.Append`（事件 + 快照）；图的落盘由装配层接 `dag.Graph.Save` | 断点续跑的粒度被破坏 |
| `Session.Observer Observer` | **删除**。DAG 的事实抽取走 `Planner.ObserveEvent` | 无实现、无测试——接口本来就是死的 |

**为什么 `Observer` 删掉而不是保留：** 它和 `Planner.ObserveEvent` 是同一条事件
路径上的两个消费者，而「谁在图里写东西」必须只有一个答案。v0.2 里 `Observer`
零实现，删它不损失能力。

**注意 `Saver` 失败的错误路径变了。** v0.2 里 `Saver.Save` 出错走 `observe`（只到
`OnEvent`，**不到** `Observer`/`Gate`，`harness.go:475`）；v0.3 里落盘失败是
`Store.Append` 的错误，由引擎处理（记录事件 + 计入 run 的 `Err`）。

## 2. `Outcome` → `OutcomeView`

19 个字段的去向。**逐字段列出，因为静默丢字段会让报告失真而无人察觉。**

| v0.2 `Outcome` 字段 | v0.3 去向 |
|---|---|
| `Code` | `OutcomeView.Code`（不变） |
| `Reason` | `OutcomeView.Reason`（不变；**新增两个可能值**，见 §5） |
| `Flags []string` | `OutcomeView.Flags`（不变；**只在返回值里，绝不落盘**） |
| `Candidates []Candidate` | `OutcomeView.Candidates`（不变） |
| `Submitted int` | `OutcomeView.Submitted`（不变；语义仍是**去重后的确认数**） |
| `Duplicates int` | `OutcomeView.Duplicates`（不变） |
| `Rejected int` | `OutcomeView.Rejected`（不变） |
| `Rounds int` | `OutcomeView.Rounds`（不变） |
| `IntentDone int` | `OutcomeView.IntentDone` —— **v0.2 是死字段（零赋值点），v0.3 真实回填**（来自 `dag.Graph.Stats()`） |
| `Negative int` | `OutcomeView.Negative` —— 同上，真实回填 |
| `HintUsed int` | `OutcomeView.HintUsed`（不变） |
| `ProgressConfirmed int` | `OutcomeView.ProgressConfirmed`（不变） |
| `ProgressTotal int` | `OutcomeView.ProgressTotal`（不变） |
| `Stats Stats` | `OutcomeView.Stats`（不变；但 v0.2 只在循环**结束后**赋值一次，v0.3 每轮实时更新） |
| `Solve SolveResult` | **删除**（随 `Solver` 一起） |
| `Report string` | `OutcomeView.Report` —— v0.2 是死字段，v0.3 由报告路径回填 |
| `Err string` | `OutcomeView.Err`（不变） |
| `StartedAt time.Time` | `OutcomeView.StartedAt`（不变） |
| `EndedAt time.Time` | `OutcomeView.EndedAt`（不变） |

方法：

| v0.2 | v0.3 |
|---|---|
| `Outcome.Duration() time.Duration` | `OutcomeView.Duration()`（不变） |
| `Outcome.Solved() bool` | **删除**。语义被 `Objective.Completed` 取代——留两个「解出来了吗」的判据只会让调用方各挑一个，而它们会在边界情形下不一致 |

## 3. 接口改名与签名变更

| v0.2 | v0.3 | 说明 |
|---|---|---|
| `Scheduler` | `Planner` | `Next(ctx, Challenge, *Outcome) *IntentRef` → `Next(ctx, PlannerInput) (*IntentRef, error)` |
| `Scheduler.Ingest(ev, round) IngestResult` | `Planner.ObserveEvent(ev, round)`（无返回值） | `dag.Scheduler.Ingest` **保留原名与返回值**，`ObserveEvent` 是薄包装。Go 不允许同名方法只因返回值不同而共存 |
| `Renderer.Render(ctx, ch, it, *Outcome)` | `Renderer.Render(ctx, ch, it, *OutcomeView)` | 只改类型名 |
| `Gate` | `CandidateGate` | 方法集增加 `SetIntent(intentID string, round int)` |
| `Observer` | **删除** | 零实现 |
| `Saver` | **删除** | 落盘改由 `Store` 承担 |
| `Agent.Round(ctx, prompt string, emit func(Event))` | `Agent.Round(ctx, RoundRequest) (RoundResult, error)` | 事件改走 `AgentFactory.New(spec, EventSink)` 注入的 `EventSink` |
| `Solver` + `SolveRequest` + `SolveResult` | **删除** | 见 §4 |

**为什么 `Next` 多了一个 `error`：** 前沿耗尽与图坏掉是两件事。v0.2 里两者都表现
为 `nil`，轮循环无法区分「做完了」与「图不可用」。现在 `(nil, nil)` 是正常的终局，
非 nil error 才是故障。

**为什么 `Round` 不再收 `emit func(Event)`：** v0.2 的 `emit` 从 agent 的 reader
协程直接扇出到 `OnEvent`/`Observer`/`Gate`/`Ingest`（`harness.go:520-528`），意味着
一个慢回调会反压 pi 的 stdout 管道，而且任意用户代码跑在别人的协程里。v0.3 改成
agent 只推事件、引擎决定谁在哪个协程消费。

## 4. `Solver` 为什么删掉

v0.2 的 `Solver`（`solver.go:79`）是「一次性求解后端：给一个 prompt，跑完，返回」，
配一个 `SolveRequest`/`SolveResult`，由 `Session.runOneShot`（`harness.go:319-342`）
驱动。

删它的依据：**仓库里没有任何实现**（`grep 'func .* Solve('` 零命中），也没有任何
测试或调用方设置 `Session.Solver`。删的是死代码，不是能力。

连带删除：`SolveRequest`、`SolveResult`、`Outcome.Solve`、`DefaultFlagFormat`
（`solver.go:44`，同样零引用）。`DefaultFlagFormat` 的职责由 `AnswerFormatHint`
承担——它按题面渲染，题目没说形态时不硬塞 `flag{...}`。

## 5. `Reason*` 常量：全部保留 + 新增两个

**九个已有值一个字都没改**（`contract_test.go` 的 `TestContract_ExistingReasonsUnchanged`
钉死）：

`completed` / `timeout` / `stalled` / `stopped` / `max_turns` / `error` /
`no_intent` / `solved` / `provider_failure`

新增两个：

| 新常量 | 值 | 取代了 v0.2 的什么 |
|---|---|---|
| `ReasonMaxRounds` | `max_rounds` | v0.2 里轮次耗尽返回 `ReasonMaxTurns`（`harness.go:33`），与工具调用次数耗尽合成一个字符串 |
| `ReasonMaxCost` | `max_cost` | v0.2 里成本耗尽返回 `ReasonTimeout`（`harness.go:42`，从墙钟分支拷来的） |

> ⚠️ **这是行为变更，不是纯新增。** 如果你的代码按 `out.Reason == "max_turns"`
> 判断「轮次用完了」，它在 v0.3 里需要改成 `max_rounds`（或同时接受两个）。
> 变更的理由：报告要能回答「为什么停」，而 `max_turns` 同时表示两件不同的事。

`ReasonStalled` 仍然**没有任何写入点**（v0.2 也是如此）。保留常量是因为它是公开
API 的一部分。

## 6. 行为变更清单（除了改名字之外，实际行为也变了的）

| # | 变更 | v0.2 行为 | v0.3 行为 |
|---|---|---|---|
| 1 | 轮次耗尽的原因 | `max_turns` | `max_rounds` |
| 2 | 成本预算 | **完全无效**——`used.MaxCostUSD` 取自 `out.Stats.CostUSD`，而 `out.Stats` 只在循环结束后赋值一次（`harness.go:493`），循环内恒为 0 | 每轮实时生效 |
| 3 | `HintAlways` | 每轮都请求提示、每轮都扣分（`maybeHint` 只对 `HintAuto` 有守卫） | 受「每题一次」守卫，与文档一致 |
| 4 | `Outcome.IntentDone`/`Negative`/`Report` | 永远是 0 / 0 / ""（零赋值点） | 真实回填 |
| 5 | cleanup 错误 | `defer` 里丢弃结果与错误，且复用可能已取消的 ctx（`harness.go:258`） | 单独记录，不覆盖主要终止原因 |
| 6 | 落盘 | 只有一个 `Saver.Save` 点，没有事件日志，没有单调序号 | 先事件后快照，原子写，可重放 |
| 7 | 恢复 | 不存在 | `Engine.Resume`；三种情形 fail closed |
| 8 | 状态所有权 | 调用方可以随时读 `Session` 字段并改它 | 引擎独占；调用方只通过 `RunHandle` 观察 |
| 9 | 平台预检失败 | `KindError` 字符串 | `Kind` 分类（`harness.IsKind`） |
| 10 | `piai` 的启动失败诊断 | pi 的 stderr 收进 `stderrTail` 却从不透出（`proc.stderr()` 零调用者） | 接进启动失败与进程死亡的错误消息 |

## 7. 版本号

`harness.Version`：`"0.2.0"` → `"0.3.0"`。

它被 `dag/store.go` 消费（写进 schema 文档的 `harnessVersion` 字段），所以改版本号
只影响新落盘文档的元信息，**不影响旧图的读取**——`dag` 的 schema 版本是独立的
`SchemaVersion`（当前 1），v0.2 写的 `graph.json` 仍可 `Load`（`dag/store_test.go`
的 `TestLoadV0_2GraphJSON` 钉死）。

## 8. 迁移步骤（调用方视角）

1. 把 `Session` 的字段搬到 `Options`；`Gate`/`Planner`/`Renderer` 包成工厂。
2. 把 `Budget`/`Submit`/`HintPolicy`/`Workdir` 搬进 `RunSpec`。
3. `sess.Run(ctx, code)` → `h, err := eng.Start(ctx, spec)` + `snap, err := h.Wait(ctx)`。
4. 事件回调从 `Agent.Round` 的参数改成 `Options.OnEvent`（或 `RunHandle.Events` 做流式消费）。
5. 如果按 `Reason` 字符串分支，检查 §5 的两个新值。
6. 如果有代码依赖 `Solver`/`Outcome.Solve`/`Outcome.Solved()`/`DefaultFlagFormat`——这些已删除，需要改用 `Renderer`/`Objective.Completed`/`AnswerFormatHint`。
7. 检查 `HintAlways` 的使用：它现在每题只请求一次提示（原本每轮都请求）。
