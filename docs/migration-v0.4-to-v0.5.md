# v0.4.0-research → v0.5 迁移表

> 适用对象：已经在用 v0.4 同步门面（`NewHarness` / `Harness.Run` / `ResultStore`）
> 或经 CLI 的调用方。
> 基线：本文描述的是 R1（`docs/offensive-harness-sdk-roadmap.md`）这一轮的
> **破坏性变更**。R0（v0.4 发布门）**尚未通过**，所以本文描述的是「代码已就位」，
> 不是「可以发布」——发布边界见 `docs/roadmap.md`。

## 一句话

**profile 从任意键 map 变成带 schema 的结构体**；**v0.3 的双代公开面移出根包到
`legacy/`**；**答案形态的兜底收紧**；**新增每题提交次数上限**。生产入口
（`NewHarness` / `Harness.Run` / `Doctor`）签名不变。

## 1. 编译期就会撞上的改动

### 1.1 `SolverProfile.Planner` / `PromptPolicy`：map → 结构体

```go
// v0.4
Profile: harness.SolverProfile{
    Planner:      map[string]any{"dryRoundsBeforeHint": 5},
    PromptPolicy: map[string]any{"maxFacts": 8, "maxNegative": 4},
}

// v0.5
Profile: harness.SolverProfile{
    Planner:      harness.PlannerConfig{DryRoundsBeforeHint: 5},
    PromptPolicy: harness.PromptConfig{MaxFacts: 8, MaxNegative: 4},
}
```

为什么值得这次破坏性变更：旧写法里**拼错一个键不会报错**（消费点取不到就静默回落
默认值），而那个拼错的键**仍然进 `ProfileDigest`**——于是「改了一个拼错的键」在
报告里表现为「换了一次实验分组」。两个后果都不可见。

文本进来的配置走新入口，它会在**任何副作用之前**拒绝未知键、类型错与越界值：

```go
p, err := harness.LoadProfile(f)   // f 是 io.Reader；内部 DisallowUnknownFields + Validate
```

### 1.2 v0.3 公开面移出根包

| v0.4 位置 | v0.5 位置 |
|---|---|
| `harness.Engine` | `legacy.Engine` |
| `harness.Options` / `harness.New` / `harness.RegisterEngine` | `legacy.Options` / `legacy.New` / `legacy.RegisterEngine` |
| `harness.RunHandle` | `legacy.RunHandle` |
| `harness.Snapshot` / `harness.PublicSummary` / `harness.SchemaVersion` | `legacy.*` |
| `harness.DomainEvent` / `harness.EvDomainEventType` / `harness.Ev*` | `legacy.*` |
| `harness.Store` / `harness.GraphStore` / `harness.EvidenceStore` | `legacy.*` |
| `harness.Executor`（旧端口） | `legacy.Executor` |
| `harness.RunPolicy` / `harness.PolicyInput` / `harness.RunSummary` | `legacy.*` |

**没有移出的**（它们看起来像 v0.3，实际在 v0.4 活路径上）：

- `ExecSpec` / `ExecHandle` / `ExecOptions` / `ExecResult`：`executor/session.go`
  的 `NewSession` 构造 `ExecSpec`，它是 v0.4 生产路径的一部分。
- `ExecutorSpec`：仍进 `RunSpec` 与 `RunSpec.Digest()`。
- `Reason*` 常量、`answer.Fingerprint`、图 schema 1——都是契约。

`legacy` 是**叶子**：只 import 标准库与根包。这条规矩由
`legacy/layering_test.go` 用 `go/parser` 扫全仓 import 图来钉，不是靠注释。

### 1.3 `CandidateGate.Mark` 多了一个返回值

```go
// v0.4
Mark(flag string, res Evaluation, err error)
// v0.5
Mark(flag string, res Evaluation, err error) SubmissionVerdict
```

只把返回值当语句用的调用方**不受影响**（Go 允许忽略返回值）。自己实现
`CandidateGate` 的调用方需要加返回值——`Duplicate` 的派生（`Accepted && !Progress`）
现在由 gate 回答，而不是让每个调用方各推一遍。

### 1.4 `HarnessOptions.Graphs` 的错误分类多了一档

`graphSaveStage` 现在返回 `"marshal"` / `"write"` / `"export"` / **`"unknown"`**。
`"unknown"` 表示失败原因不是本仓库那三个哨兵之一——`GraphSaver` 是公开端口，实现
可以是别人写的。此前任何未知错误都被报成 `"marshal"`，那是在断言一件我们并不知道
的事。

## 2. 行为变更（编译得过，但结果会变）

### 2.1 答案形态的兜底不再默认允许裸串

`answer.Infer` 在题面既没说 `flag{` 也没提密码/密钥/hash 时，此前**同时**接受信封
与裸串，现在**只接受默认信封**。

代价是「题面什么都没说的裸串题」可能拿不到分；收益是提交次数回到正常量级——旧兜底
的真实代价不是「一条候选被平台判错」，而是**提交次数**，而提交次数有配额：

> 2026-09-22 授权真跑：一道题面既无 `flag{` 也无裸串线索的题因此把每个原始 token
> 都收成了候选，打出 **147 次 `POST /submit`**（146 次是原始 token）。

如果你的题面确实在描述裸串答案却没有任何相关字样，两条路：把线索写进题面，或设
`Challenge.FlagFormat`（`answer.InferFor` 会把两者一起喂给形态判定）。

### 2.2 gate 与 dag 用同一个形态来源（修了一处不一致）

`gate` 此前只拿 `ch.Description` 推断答案形态，而 `dag` 拿的是
`Description + " " + FlagFormat`。同一次运行里，候选判据与「答案形状内容拒入图」
的不变量用的不是同一个形态。现在两者都走 `answer.InferFor`。

### 2.3 新增每题提交次数上限

`PolicySpec.MaxSubmissionsPerChallenge`（0 = 默认 50）。到上限即停止本题提交、以
`ReasonSubmitLimit` 结束本题，并把「还剩多少候选没提交」记进
`OutcomeView.SubmissionsCapped`（进公开结果）。CLI 对应 `--max-submissions`。

如果你的实验需要更多次提交，显式调高它——上限的作用是让「候选集合失控」这件事
**有界且可见**，而不是替你决定合理的提交量。

### 2.4 运行清单进了公开结果

`RunResult.Manifest`（公开形态 `publicResult.manifest`）记录本次运行**实际生效**
的：停滞阈值、提交上限、提示策略、请求的镜像 tag、解析出的镜像 digest、镜像内 pi
版本、以及镜像不一致标记。此前公开结果只有 16 位 `ProfileDigest`，而摘要不可逆
——「这次跑的是 2 轮还是 5 轮」「用的哪个镜像」事后无法回答。

### 2.5 私密面多了一个文件

`<ResultDir>/private/<runID>/submissions.jsonl`（0700/0600）：每次提交一行，记
指纹、来源与推导链锚点、提交时间、平台判定、以及候选明文。

它回答的是此前**不可回答**的问题：「提交了什么、平台怎么判的」。在此之前判定只活
在内存里、随本题结束消失。注意文件名**不是** `candidates.jsonl`——那个名字属于
v0.3 的候选账本（`<StoreDir>/runs/<id>/private/`，至今没有生产写入方）。

### 2.6 两个此前恒为 0 的公开字段活了

`OutcomeView.Duplicates` / `Rejected` 在 v0.4 有定义、**全仓零赋值点**。现在由
`SubmissionVerdict` 回填并进公开结果。

## 3. 兼容性影响（不阻塞，但要知情）

- **旧 `run.json` 读回来时**，`planner` / `promptPolicy` 里的未知键会被
  `encoding/json` 静默丢弃。v0.4/v0.5 都**没有**恢复路径（`HarnessOptions` 里没有
  `Store`），所以这是读取兼容，不是行为回归。
- **`RunSpec.Digest()` 会算出新值**：同一份逻辑配置的摘要变了（profile 的序列化
  形状变了）。同样因为没有恢复路径而无实际影响，但**跨 v0.4/v0.5 比对
  `SpecDigest` 是不成立的**——不要在报告里把两者当成同一个东西。
- **`ProfileDigest`** 也会变（`Planner`/`PromptPolicy` 的序列化形状变了）。跨版本
  的 profile 分组同理不可比。
- `Snapshot` / `DomainEvent` 的 **JSON 键名逐字未变**（有 canary 钉着
  `legacy/schema_canary_test.go`），旧 `run.json` / `events.jsonl` 仍可读。
- 图 schema 1 与 `dag/testdata/` 两份 golden **字节未变**：本次迁移 `dag/` 零 diff。

## 4. CLI 变更

| 变更 | 说明 |
|---|---|
| 新增 `--profile <file>` | 走 `harness.LoadProfile`；未知键/非法值在装配之前拒绝 |
| 新增 `--bundle <dir>` | 部署级选项（不是运行意图），覆盖 profile 文件里的 `extensionBundle` |
| 新增 `--max-submissions N` | 对应 `PolicySpec.MaxSubmissionsPerChallenge` |
| `run` 摘要新增「图未落盘 `<阶段>`」 | 仅在非 0 时出现；常规摘要行的形状不变 |
| `run` 摘要的「重复/判错」两列 | 此前恒为 0（字段是死的），现在有真实值 |

`--bundle` 的**去向**变了：它不再写进 `RunSpec.Profile`，而是走
`wire.Options.Sandbox.ProfileDir`。这是为了让 CLI 与直接调 SDK 得到**同一个**
`ProfileDigest`——旧写法让 CLI 造出一份非空 profile、顶掉装配层的默认 profile，
实测两条路径的 `profileDigest` 一个是 `d96751574f823ad8` 一个是
`445d6dc940f6960e`（而 `bundleDigest` 逐字相同）。修完之后两条路径逐字一致。

## 5. 一眼看懂的对照

```go
// v0.4
h, _ := harness.NewHarness(harness.HarnessOptions{ /* … */ })
res, _ := h.Run(ctx, harness.RunSpec{
    Profile: harness.SolverProfile{
        Planner:      map[string]any{"dryRoundsBeforeHint": 5},
        PromptPolicy: map[string]any{"maxFacts": 8},
    },
})

// v0.5
h, _ := harness.NewHarness(harness.HarnessOptions{ /* … 不变 … */ })
res, _ := h.Run(ctx, harness.RunSpec{
    Profile: harness.SolverProfile{
        Planner:      harness.PlannerConfig{DryRoundsBeforeHint: 5},
        PromptPolicy: harness.PromptConfig{MaxFacts: 8},
    },
    Policy: harness.PolicySpec{MaxSubmissionsPerChallenge: 50},
})
// 多出来的：res.Manifest（生效值 + 镜像身份）、
//           res.Challenges[i].Outcome.{Duplicates,Rejected,SubmissionsCapped}
```

一个可直接运行的最小例子见 `example/main.go`。
