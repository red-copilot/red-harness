# v0.2 回归断言归档

本目录保存 v0.2.0 的两个测试文件在删除前的**逐字副本**，用于 v0.3.0 的 `engine/` 包移植。

| 文件 | 原名 | 说明 |
|---|---|---|
| `v0.2-regression_test.go.txt` | `regression_test.go` | 三个轮循环护栏回归用例 |
| `v0.2-wiring_test.go.txt` | `wiring_test.go` | 四个接线点回归用例 |

**为什么加 `.txt` 后缀：** 避免被 `go build ./...` 当成源码编译。

**为什么在 W0 只归档、不在 W0 就写新测试：** `T3` 要删掉这两个文件，但它们的断言是前几轮事故换来的。**不能把它们直接改写成 `engine/` 包里的测试**——W0 时 `engine` 包还不存在，而 `t.Skip` 只跳过执行、不跳过编译，引用 `engine.New` 的测试文件会让 `go build ./...` 直接红。所以 W0 只归档原文，移植是 T11 的第一步（那时 `engine` 包已存在，测试能编译）。

## ⚠️ 跨文件依赖

`wiring_test.go` **没有自己的 platform fake**——它用的是 `regression_test.go:18` 声明的 `regPlatform`。移植时这两个文件必须一起搬，或者把 `regPlatform` 显式提出来共享。分开移植会编译失败。

## 移植要求

**逐条保留原断言，只改类型。** 不要因为「类型对不上」而把断言改弱——每一条都对应一次真实事故。

## 断言清单与它们防的事故

| 原用例 | 位置 | 防的事故 |
|---|---|---|
| `TestRegression_SolvedStopsWithoutFlagCount` | `regression_test.go:96` | 「通关立即终止」是死代码（`stop_check` 从未被调用）。断言：`List` 返回 nil 且无 `Challenge` ⇒ `FlagCount` 不可用，靠平台权威进度（`CorrectFlagCount:2, TotalFlagCount:2`）必须在 **1 轮**后停，`Reason == ReasonSolved`，且 `Cleanup` 被调用。 |
| `TestRegression_ZeroTurnTimeoutIsNotProviderFailure` | `:153` | 一次零回合的墙钟超时被误记成「模型服务挂了」。断言：60 ms 超时 + 零回合 ⇒ `ReasonStopped`（**不是** `ReasonProviderFailure`），且 `Run` 返回 `nil` error。钉死分支顺序。 |
| `TestRegression_ProviderErrorCaughtEvenWithTurns` | `:201` | provider 故障只在 `turns == 0` 时被识别，带回合的故障漏网。断言：`Turns:3` + `ProviderError` ⇒ 仍须 `ReasonProviderFailure`。 |
| `TestWiring_IngestReceivesRoundScopedEvents` | `wiring_test.go:85` | DAG 静默退化：漏接 `Ingest` ⇒ 图零事实、7 个阶段只有前 4 个可达，而所有包自测全绿。断言：3 轮 × 2 事件 ⇒ `Ingest` 恰 6 次，轮号 `1,2,3` 都出现。 |
| `TestWiring_IntentSinkFillsProvenanceAnchor` | `:119` | 候选的推导链锚点丢失（`IntentID` 全空、`Round` 全 0），族别判定不受影响所以**不报错**。断言：2 轮 ⇒ `SetIntent` 恰 2 次，id `intent-A`/`intent-B`，轮号 `1`/`2`。 |
| `TestWiring_SaverCalledEachRound` | `:148` | 断点续跑的粒度被破坏。断言：4 轮 ⇒ 落盘恰 4 次。 |
| `TestWiring_SaveFailureIsNotSilent` | `:167` | 落盘失败被吞掉，这一轮的进展没保住而调用方不知道。断言：`Save` 出错必须以 `EventError` 透出。 |

## 移植时必须保留的语义细节（已核对源码）

这些细节不是断言的字面内容，但断言依赖它们——改实现时不要顺手「统一」掉：

1. **`regSched` 恒返回非 nil 直到 `max`**（`regression_test.go:68-75`）。这是刻意的：让「通关立即停」成为**唯一**能提前终止循环的机制。若改成「意图耗尽返回 nil」，第一个用例的 `Rounds == 1` 断言就失去意义了。
2. **`harvest` 在 `Gate == nil` 时直接返回**（`harness.go:533`）——这就是 `wiring_test.go` 的四个用例能留空 `Gate` 而不提交的原因。
3. **`Saver` 失败走 `observe` 而不是 `emit`**（`harness.go:475`）：所以它到得了 `OnEvent`，**到不了** `Observer` 与 `Gate`。`TestWiring_SaveFailureIsNotSilent` 靠 `OnEvent` 收 `EventError`，移植时不要把它改成走 `emit`。
4. **`OnEvent` 在轮循环同一协程上同步调用**（v0.2 里 `emit` 全文件无 `go` 语句），所以测试里直接 append 到切片没有数据竞争。v0.3 改成 channel 消费后，测试需要显式等待事件被消费完（例如 `RunHandle.Wait` 或读到确定的序号）——**这是移植时唯一需要重新设计同步的地方**，不是断言本身。
5. **`Submitted` 是去重后的确认数**（`harness.go:570` 的 `out.Submitted = len(out.Flags)`），**不是提交调用次数**。`Duplicate` 也计入 `Flags`。
6. **提交出错什么都不计**（`harness.go:548-549`）：不计确认、不记判错账本、不更新平台进度。
7. **平台进度只在 `> 0` 时覆盖**，且**不防下降**（`harness.go:562-567`）；同轮多次提交时**最后一次赢**。
8. **`dry` 计数只看候选数**（`harness.go:480-484` 比较 `len(out.Candidates)` 与 `before`），与「新事实」无关——注释说「也没产出新事实」，但代码没有查事实数。
