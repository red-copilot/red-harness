#!/usr/bin/env python3
"""red-harness 依赖与拓扑提取器。

输出 `topology.json`（喂交互式 viewer）、三份 `.mmd`（喂文档/PR）与一份人读摘要。

设计原则（对应 /modernize-map 的四条要求）：

1. **边藏在两个地方**。静态 import 图只是其中一半。本项目里真正把系统拼起来的那几条边
   在 import 图里**完全看不见**——`cli.SetWire` 的包级函数变量、`scenario` 经 `Platform`
   窄接口调到 `bridge`、`piai` 经 `harness.SandboxSession` 拿到 `executor`。所以本脚本
   把边分成两类：`call`（import 可证）与 `dispatch`（配置/注入表可证），后者必须带
   证据模式，脚本启动时逐条回查源码——模式失配就报警，防止图随代码漂移而静默说谎。
2. **代码↔存储的接缝是外部配置**。`graph.json` / `report.*` 的路径常量在 `store`，
   但**生产者**在别处（或不存在）。所以 `ds:graph.json`、`ds:report` 这类节点是"有端口、
   无装配方"，本脚本对它们做**抑制**处理（见 NAIVE_GRAPH_FALSE_POSITIVES）。
3. **入口点在部署/构建描述里**。Go 的 `main()`、Python 子进程的 argv、Dockerfile 的
   ENTRYPOINT——不解析它们，顶层模块看起来全都不可达。
4. **死端只在边齐了之后才有意义**。本项目的提取结果是 0，而"看起来像孤岛"的每一处都
   命中抑制规则。这不是空结果，这是结论。

不读凭据：脚本显式跳过 `.env*` / `*.ovpn` / `.agent.env`，且只引用环境变量**名**
（`BENCHMARK_TOKEN`），绝不解析其值。datastore 一律用逻辑标识（文件名 / DD 名 / 表名），
不落任何 URL、DSN 或含 userinfo 的配置值。
"""

from __future__ import annotations

import json
import os
import re
import sys
from collections import defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
OUT_DIR = HERE

# 显式跳过的路径段。`.gocache` 里躺着整份 stdlib + 依赖源码（1200+ .go 文件），
# `.claude/worktrees` 是并行 agent 的工作副本，两者都会把图彻底带偏。
SKIP_DIR_PARTS = {".git", ".gocache", ".claude", ".remember", "vendor", "__pycache__", "node_modules"}
# 凭据与私钥：绝不读。
SKIP_FILE_PATTERNS = (re.compile(r"^\.env"), re.compile(r"\.ovpn$"), re.compile(r"\.pyc$"))

MODULE_PATH = "github.com/red-copilot/red-harness"

# ── 域划分 ──
# 域来自仓库自己的架构描述（CLAUDE.md「架构」节）与实测的包边界，不是按目录名猜的。
DOMAINS = [
    ("dom:contract", "契约层", "根包：只有类型与接口，零实现、零内部依赖。并行开发的前提。"),
    ("dom:reasoning", "事实与答案层", "事实层（dag）与答案层（gate）严格分离；明文只在 gate 的账本里。"),
    ("dom:store", "持久化", "事件日志 + 原子快照 + 私密账本 + 公开指标。单写者模型。"),
    ("dom:execution", "隔离执行", "Docker argv 级隔离、iptables 规则链、白名单代理。不引 Docker SDK。"),
    ("dom:agent", "Agent 接入", "pi 的 RPC 适配（Go）与 TSecBench 的常驻 Python 桥。"),
    ("dom:scenario", "场景适配", "离线 fake 与真实平台两条路径；测试替身钉住 SDK 契约。"),
    ("dom:composition", "装配与接口", "CLI 与唯一的装配层。实现包之间互不可见，只在这里相遇。"),
    ("dom:data", "数据存储", "逻辑落点：运行目录、私密账本、Docker、平台与 provider。"),
]

# 模块：id → (显示名, 域, 语言, 相对路径或目录, kind)
# loc 由脚本现算（非测试行数），不手写——手写的数字一定会烂。
MODULES = [
    ("harness", "harness（根契约）", "dom:contract", "go", ".", "module"),
    ("answer", "answer", "dom:reasoning", "go", "answer", "module"),
    ("dag", "dag", "dom:reasoning", "go", "dag", "module"),
    ("gate", "gate", "dom:reasoning", "go", "gate", "module"),
    ("store", "store", "dom:store", "go", "store", "module"),
    ("executor", "executor", "dom:execution", "go", "executor", "module"),
    ("runner/Dockerfile", "runner/Dockerfile（镜像构建）", "dom:execution", "dockerfile", "runner/Dockerfile", "job"),
    ("runner/Dockerfile.teststub", "Dockerfile.teststub（测试镜像）", "dom:execution", "dockerfile", "runner/Dockerfile.teststub", "job"),
    ("piai", "piai", "dom:agent", "go", "piai", "module"),
    ("piai/testdata/stubpi", "stubpi（测试替身）", "dom:agent", "go", "piai/testdata/stubpi", "module"),
    ("bridge", "bridge（Go 侧）", "dom:agent", "go", "bridge", "module"),
    ("bridge/bridge.py", "bridge.py（Python 桥）", "dom:agent", "python", "bridge/bridge.py", "module"),
    ("bridge/protocol.py", "protocol.py", "dom:agent", "python", "bridge/protocol.py", "module"),
    ("scenario", "scenario", "dom:scenario", "go", "scenario", "module"),
    ("bridge/testdata/mock_sdk.py", "mock_sdk（测试替身）", "dom:scenario", "python", "bridge/testdata/mock_sdk.py", "module"),
    ("internal/cli", "internal/cli", "dom:composition", "go", "internal/cli", "module"),
    ("internal/wire", "internal/wire", "dom:composition", "go", "internal/wire", "module"),
    ("cmd/red-harness", "cmd/red-harness", "dom:composition", "go", "cmd/red-harness", "module"),
]

# 数据存储：id → (显示名, 补充说明)。**只用逻辑标识**，不含任何 URL/DSN/凭据。
DATASTORES = [
    ("ds:run.lock", "run.lock", "跨进程单运行 flock；保护「整机同时只有一个 run」"),
    ("ds:events.jsonl", "events.jsonl", "领域事件日志，0600，序号单调"),
    ("ds:run.json", "run.json", "原子快照，0600，恢复以 lastAppliedSeq 为基线"),
    ("ds:graph.json", "graph.json", "DAG 载荷，0600；落盘前 scrub 擦明文"),
    ("ds:graph.mmd", "graph.mmd", "同图的 mermaid 导出，0600；从刚落盘的那份真源派生"),
    ("ds:candidates.jsonl", "private/candidates.jsonl", "候选明文账本，0600——全仓明文唯一落点"),
    ("ds:evidence", "private/evidence/", "原始工具输出（含明文），0700/0600"),
    ("ds:private-trace", "private/<runID>/*.jsonl", "原始事件 trace，单文件 64 MiB 上限"),
    ("ds:bridge-stderr", "private/bridge-stderr.log", "桥的 stderr：平台未识别响应体的唯一可见处"),
    ("ds:results", "results/<runID>.json", "公开指标，只有计数与指纹，0600"),
    ("ds:report", "report.json / report.md", "公开报告，0644；写者 report/ 包不存在"),
    ("ds:docker-daemon", "Docker 守护进程", "经 docker CLI 的 socket；不引 Docker SDK"),
    ("ds:runner-image", "runner 镜像（docker images）", "red-harness-runner 镜像标签（tag 由 runner/README 记录）"),
    ("ds:host-netfilter", "宿主 netfilter", "DOCKER-USER 等链上的按 run 规则集"),
    ("ds:tsecbench-api", "TSecBench 平台 API", "题目清单 / 起题 / 提示 / 提交判分"),
    ("ds:provider-api", "模型 provider API", "容器内经白名单代理出网"),
    ("ds:env-file", ".env（凭据）", "piai 解析 provider 凭据；**只读路径，不读内容**"),
    ("ds:pi-session", "pi 会话目录", "pi 的 session/home，容器内路径"),
]

# ── 边 ──
# 每条 dispatch 边都带证据模式：(文件, 正则)。脚本启动时回查，失配即报警。
# 这是本脚本对抗"图随代码漂移"的核心手段——文档会说谎，正则不会。
DISPATCH_EDGES = [
    ("internal/cli", "internal/wire", "dispatch",
     "cli.SetWire 安装包级 WireFunc；import 图里没有这条边",
     "cmd/red-harness/main.go", r"cli\.SetWire\(newPorts\)"),
    ("scenario", "bridge", "dispatch",
     "scenario.TSecBench 只认 Platform 窄接口，实体由 wire 注入 *bridge.Client",
     "internal/wire/wire.go", r"scenario\.TSecBench\{Platform: client\}"),
    ("piai", "executor", "dispatch",
     "AgentFactory.New 收 harness.SandboxSession；piai 不 import executor",
     "piai/factory.go", r"session harness\.SandboxSession"),
    # ⚠️ R1（v0.5）把这两个端口移出根包到 legacy/：它们在 v0.4 的生产路径上零
    # 调用方，而根包公开面的每一项都应当是「活的」。证据因此改指 legacy/ports.go
    # ——它们仍然是「gate/dag 不 import store」这条不可见边的凭据，只是住在别处。
    ("gate", "store", "dispatch",
     "gate 经 legacy.EvidenceStore 写账本；gate 不 import store",
     "legacy/ports.go", r"type EvidenceStore interface"),
    ("dag", "store", "dispatch",
     "dag 经 legacy.GraphStore 落图；dag 不 import store",
     "legacy/ports.go", r"type GraphStore interface"),
    ("internal/wire", "scenario", "dispatch",
     "场景按名字表选：fake / tsecbench",
     "internal/wire/wire.go", r"ScenarioFake\s*=\s*\"fake\""),
    ("bridge", "bridge/bridge.py", "dispatch",
     "exec 常驻子进程，JSONL over stdio；argv 可指向容器内 python",
     "bridge/client.go", r"func DefaultCommand\(\)"),
    ("bridge/bridge.py", "bridge/testdata/mock_sdk.py", "dispatch",
     "TSEC_MOCK=1 + PYTHONPATH=bridge/testdata ⇒ sys.modules 里把 mock_sdk 顶成 tsec_benchmark",
     "bridge/bridge.py", r'sys\.modules\.setdefault\("tsec_benchmark", mock_sdk\)'),
    ("piai", "piai/testdata/stubpi", "dispatch",
     "仅 go test 编译并运行（go build ./testdata/stubpi），生产路径不涉及",
     "piai/agent_test.go", r"go\", \"build\", \"-o\", stubBin, \"\./testdata/stubpi\""),
    ("internal/wire", "gate", "dispatch",
     "Gate 是函数工厂，每题新建（gate 是每题独立的）",
     "internal/wire/wire.go", r"Gate:\s+func\(ch harness\.Challenge\)"),
    ("internal/wire", "dag", "dispatch",
     "SolverWithProfile 是函数工厂，同一道题上 Planner/Renderer 共享一份图",
     "internal/wire/wire.go", r"SolverWithProfile:\s+func\(ch harness\.Challenge"),
]

# 运行时/数据边：(source, target, kind, 说明)
RUNTIME_EDGES = [
    # 存储写入面：store 是这些落点的唯一写者（单写者模型）
    ("store", "ds:events.jsonl", "write", "**能力边**：FileStore 在 v0.4 无装配方。Append 先事件后快照的顺序仍是硬契约"),
    ("store", "ds:run.json", "write", "**能力边**：FileStore 在 v0.4 无装配方"),
    ("store", "ds:graph.json", "write", "v0.4 **实际生效**：`PutGraph` 由装配层的 dagGraphSaver 驱动（`wire.go:300` 恒提供 `Graphs`）"),
    ("store", "ds:graph.mmd", "write", "v0.4 **实际生效**：`PutGraphExport`，与 graph.json 并列、同权限 0600"),
    ("internal/wire", "ds:graph.json", "write", "装配层 `dagGraphSaver.SaveGraph`：`ForRun(runID).PutGraph(json.Marshal(graph))`，载荷走 dag 的统一擦洗"),
    ("internal/wire", "ds:graph.mmd", "write", "同一处 `PutGraphExport(dag.Mermaid(graph))`，从**刚落盘的真源**派生，不另拼一份"),
    ("store", "ds:candidates.jsonl", "write", "**能力边**：`Private()`/`PutCandidate` 零生产调用方"),
    ("store", "ds:evidence", "write", "**能力边**：零生产调用方"),
    ("store", "ds:private-trace", "write", "v0.4 **实际生效**：题目名 sha256 后当文件名"),
    ("store", "ds:results", "write", "v0.4 **实际生效**：只 marshal 计数与指纹"),
    ("store", "ds:report", "write", "**能力边**：生产调用方为零，`report/` 包不存在"),
    ("internal/wire", "ds:run.lock", "write", "flock；进程以任何方式退出都由内核释放"),
    ("dag", "ds:graph.json", "read", "**能力边**：`Load`/`LoadOrNew` 零生产调用方（v0.4 不做续跑）。注意方向：dag 是**写**侧的载荷与擦洗所有者（`document()`，与 `Save` 共用），但落盘动作在装配层"),
    ("bridge", "ds:bridge-stderr", "write", "把桥的 stderr 落到 private/，否则平台异常无从定位"),
    # 执行隔离
    ("executor", "ds:docker-daemon", "call", "docker CLI（info/run/exec/ps/network），全程 argv 级"),
    ("executor", "ds:host-netfilter", "write", "按 run 安装/卸载 DOCKER-USER 链规则"),
    ("executor", "ds:runner-image", "read", "image inspect 拿 ID，再 run 起容器"),
    ("runner/Dockerfile", "ds:runner-image", "write", "docker build 产出镜像（tag 由 runner/README 记录）"),
    ("runner/Dockerfile.teststub", "ds:runner-image", "write", "集成门现场构建 `red-harness-runner:teststub`：静态编译的 stub pi，不需要 provider 凭据"),
    ("executor", "ds:provider-api", "write", "容器出网只走白名单代理（provider allowHosts）"),
    # 平台与 agent
    ("bridge/bridge.py", "ds:tsecbench-api", "read", "list / get / start / hint / check_vpn"),
    ("bridge/bridge.py", "ds:tsecbench-api", "write", "submit：候选明文经子进程环境传凭据，不进 argv"),
    ("piai", "ds:env-file", "read", "只解析 provider 凭据；路径来自 Options.EnvFile"),
    ("piai", "ds:pi-session", "write", "pi 的 session/home，可选映射到容器内路径"),
]

# 入口点：不解析构建/部署描述的话，这些顶层模块看起来全都不可达。
ENTRY_POINTS = [
    ("cmd/red-harness", "CLI 进程入口：os.Exit(cli.Main(os.Args[1:], ...))；子命令 doctor/list/run/stats"),
    ("bridge/bridge.py", "Python 子进程入口：JSONL over stdio，启动先打一行 hello 握手"),
    ("runner/Dockerfile", "镜像入口：docker build 的产物 tag 是 executor 的缺省镜像"),
    ("piai/testdata/stubpi", "假 pi 进程入口：仅 go test 编译，是 pi RPC 帧格式的唯一可执行规范"),
    ("runner/Dockerfile.teststub", "测试镜像入口：由 `executor/session_integration_test.go` 现场 docker build，证明 stub pi 真的跑在目标容器里而不是宿主"),
]

# 朴素图（只看 import/文本 grep）会误判成孤岛的两处。`/modernize-map` 的规则是：
# 任何"可能是未解析动态调用的目标"都不许进 deadEnds，只能记进 observations。
# 两处全部命中，所以本图的 deadEnds 是空集——这不是提取失败，这是结论。
#
# 注：这份清单**曾经有三条**，第一条是 `ds:graph.json`（当时的判词是「端口有、装配方无」）。
# 2026-09-22 复核时该判词已过期：装配层 `wire.go` 现在恒提供 `Graphs`，图有生产写入方了。
# 它因此**不再是**死端候选（有入边），整条从清单移除——留下的两条是复核后仍然成立的。
NAIVE_GRAPH_FALSE_POSITIVES = [
    ("ds:report", "`store.FileStore.PutReport` 是公开方法，未来的 `report/` 包或任何 SDK 调用方"
                  "都能直接调。生产调用方为零 ≠ 不可达。"),
    ("legacy.New / legacy.RegisterEngine", "v0.3 的引擎注册表，R1（v0.5）已从根包移出到 `legacy/`。"
                                          "复核订正：它现在是**零调用方**（含测试）——旧判词说"
                                          "「全仓只有 contract_test.go 在调」是错的，那两行调的是 "
                                          "`harness.New`。而且 `engine/` 目录从来不存在，"
                                          "所以 `legacy.New` 的默认实现（「引擎实现未注册」）"
                                          "是**唯一可达**的结果：这个注册表从来没有被填充过。"
                                          "保留它是因为删掉是公开 API 的破坏性变更，不是因为它在工作。"),
]

OBSERVATIONS = [
    "**文档与代码的漂移是双向的，这次漂的是本图自己。** 16:16 那版提取给 `ds:graph.json` 的判词是「端口有、装配方无」，而装配层随后补上了 `dagGraphSaver`（`internal/wire/wire.go:300` 恒提供 `Graphs`）——图从此有了生产写入方。同一份 `CLAUDE.md` 当时还在描述 v0.3（`engine/` 是中心缺口、CLI 8 个子命令、`scenario/` 尚未创建），17:44 已按实况改写，反而比图更准。**两边都没有 CI 校验，所以两边的漂移都只能靠下一次复核抓到**：本脚本的 dispatch 证据回查挡得住「源码挪了」，挡不住「语义变了、证据模式还在」这种漂移——`graph.json` 这条正是后者（端口名没变，装配方从无到有）。",
    "**`internal/wire` 是唯一同时看得见全部 7 个实现包的模块，而真正把系统拼起来的边在 import 图里完全不可见。** 实现包之间两两互不 import（executor 不认识 scenario，scenario 不认识 dag），这是「一个子包 = 一个 agent」并行编排能成立的前提；代价是 wire 成为编译耦合的单点，也是「谁把 X 交给 Y」这个问题的唯一答案所在。三条最关键的边因此只能按 dispatch 建模：`internal/cli → internal/wire`（`cli.SetWire` 装包级 `WireFunc` 变量）、`scenario → bridge`（`Platform` 窄接口由 wire 注入）、`piai → executor`（`AgentFactory.New` 收 `harness.SandboxSession`）。任何 grep-import 的依赖图都会漏掉它们，并据此把 cli/scenario/piai 误判成孤立——本图的 11 条 dispatch 边每条都带源码证据回查，就是为了让这种误判当场露馅。",
    "**明文的纪律比它的落点更稳定——落点换过两次，纪律没换。** 允许出现候选明文的地方只有私密面：v0.4/v0.5 **实际生效**的是 `<ResultDir>/private/<runID>/*.jsonl`（目录 0700、文件 0600，v0.5 起多了 `submissions.jsonl` 这一份候选审计），而 `store/private.go` 那条 v0.3 账本（`PutCandidate`/`PutEvidence`）**仍然零生产调用方**（R1 把它的端口 `legacy.EvidenceStore` 移出了根包，但没给它接调用方——它是 v0.3 的公开 API，不是待办的缺口）。gate 不 import store，它经 `legacy.EvidenceStore` 写账本——这是装配层之外又一条不可见的边。出私密面的最后一道闸是图落盘前的**统一**擦洗（`dag` 的 `document()`，由装配层的 dagGraphSaver 驱动，`Save` 与 `json.Marshal` 共用同一份），而 `Raw` 字段（工具输出原文摘录）是最容易漏的那个——它曾经真的漏过。",
    "**桥的两侧契约都由可执行替身钉死，而其中一条边本机永远走不通。** `bridge/testdata/mock_sdk.py` 与真 SDK 逐字段对齐，**连两个源码级缺陷一起照抄**（畸形载荷抛裸 KeyError、2xx 非 JSON 抛 JSONDecodeError）——「修好」它们等于删掉桥必须包住的失效模式；`piai/testdata/stubpi` 是 pi RPC 帧格式唯一的可执行规范。而 `bridge.py` 的 SDK 解析是个**二选一**：`TSEC_MOCK=1` + `PYTHONPATH=bridge/testdata` 走 mock_sdk，否则 `import tsec_benchmark` 走真 SDK。本图只解析了前一条。后一条在宿主上**必然失败**（py3.12 缺 httpx），真跑的唯一路径是 `docker exec <容器> python3 -m bridge`——所以这条未解析边同时是「本机限制」与「SDK 装在容器层」的体现。",
    "**落盘面在 v0.4 裂成两半：图那一半已接线，事件溯源那一半仍悬空。** `store.FileStore` **确实在装配路径上被构造了**（`wire.go:219` 的 `store.New(storeDir)`，拿的是图存储的根句柄），`PutGraph`/`PutGraphExport`/`ForRun` 因此有生产写入方，落点是 `<StoreDir>/runs/<runID>/graph.json` + `graph.mmd`（0600）。但同一个 `FileStore` 的另一半——`Append`（events.jsonl）、`PutSnapshot`（run.json）、`PutCandidate`、`PutEvidence`（private/）、`PutReport`（report.*）——**生产调用方仍然是零**，`Private()` 也无人调用。**本图照样画着 `store → ds:events.jsonl` 这类 write 边**：那是 store 的**能力**（代码里确实能写），不是当前**接线**。区分「端口存在」与「装配存在」是这张图最容易骗人的地方，而它现在在同一张图上**同时出现两种答案**。详见 `docs/v0.4-open-items.md` A 节（该文已于 R1 之后重写，订正了三处实质错误：`store.New` 其实有生产调用方、`graph.json` 其实有写入方、`RegisterEngine` 其实零调用方）。**明文纪律没有破**：v0.4 的明文落点是 `<ResultDir>/private/<runID>/*.jsonl`，目录 0700、文件 0600，图的载荷另经 dag 的统一擦洗。",
    "**发布门的缺口在这张图上就是一条边：`bridge/bridge.py ⇒ ds:tsecbench-api` 的 submit 从未被平台确认过。** 真实 pi 已在 sandbox 内跑通工具调用并从靶场拿到 200。提交路径的现状比「501」更细：**2026-09-22 的复跑里 147 次 `POST /submit` 全部返回 200、501 未复现，但那一次全程经 HTTP/1.1 中继，直连路径仍未证**——所以既不成立「501 已经好了」，也不成立「submit 通了」。发布门保持未通过（`docs/roadmap.md` M4）。harness 的处理是对的——按「提交结果不确定」结束本题、先对账、并正确关掉了题目容器（平台侧确认 `stopped`、无遗留）——但这意味着**这条边在图上只应读作「已实现、未验证」**。同一张图上还有一条类似的边：`executor → ds:host-netfilter`，它的边界已在 `roadmap.md` 与 `docs/offensive-harness-sdk-roadmap.md` 写明——**同一 Docker bridge 内的流量不经过当前 iptables 规则**，所以「非授权端点不可达」成立，「对任意同桥容器也隔离」不成立。两条最该被质疑的边都不是画错，是**证据边界**。",
    "**v0.5 的破坏性整理已落地（R1），本图的双代结构不再是它的目标而是它的结果。** `docs/offensive-harness-sdk-roadmap.md` 第 3 节列的几件事都已执行：`SolverProfile` 的 `map[string]any` 换成了带 schema 的 `PlannerConfig`/`PromptConfig`；私密候选审计落地为 `<ResultDir>/private/<runID>/submissions.jsonl`（**生产路径已接线**，回答「提交了什么、平台怎么判的」）；v0.3 的 `Engine`/`RunHandle`/`New`/`RegisterEngine`/`Store`/`EvidenceStore`/旧 `Executor` 已移出根包到 `legacy/`；`Reason*` 字符串、图 schema 读取能力与 `answer.Fingerprint` 唯一实现都保留。**代价与收益都要说清楚**：根包公开面变小了（「哪些端口是活的」不必再靠读注释判断），而多了一个 `legacy` 叶子包——它只准 import 标准库与根包，这条规矩由 `legacy/layering_test.go` 用 go/parser 扫全仓 import 图来钉，不再只写在文档里。",
]


def _skip(path_parts, name):
    if any(p in SKIP_DIR_PARTS for p in path_parts):
        return True
    return any(rx.search(name) for rx in SKIP_FILE_PATTERNS)


def walk_files(rel):
    """遍历仓库（相对路径 rel），返回 (relpath, abspath) 列表，跳过缓存与凭据。"""
    base = os.path.join(ROOT, rel) if rel != "." else ROOT
    for dirpath, dirnames, filenames in os.walk(base):
        relparts = os.path.relpath(dirpath, ROOT).split(os.sep)
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIR_PARTS]
        for fn in sorted(filenames):
            if _skip(relparts, fn):
                continue
            ab = os.path.join(dirpath, fn)
            yield os.path.relpath(ab, ROOT), ab


GO_IMPORT_BLOCK = re.compile(r"^import\s*\((.*?)^\)", re.S | re.M)
GO_IMPORT_ONE = re.compile(r'^import\s+(?:[\w.]+\s+)?("([^"]+)")', re.M)
GO_QUOTED = re.compile(r'"([^"]+)"')


def parse_go_imports(ab):
    try:
        src = open(ab, encoding="utf-8", errors="replace").read()
    except OSError:
        return []
    out = []
    for block in GO_IMPORT_BLOCK.findall(src):
        out.extend(GO_QUOTED.findall(block))
    for _, one in GO_IMPORT_ONE.findall(src):
        out.append(one)
    return out


PY_IMPORT = re.compile(r"^\s*(?:from\s+([\w.]+)\s+import|import\s+([\w.]+))", re.M)


def parse_python_imports(ab):
    try:
        src = open(ab, encoding="utf-8", errors="replace").read()
    except OSError:
        return []
    return [a or b for a, b in PY_IMPORT.findall(src)]


def count_lines(ab):
    try:
        with open(ab, encoding="utf-8", errors="replace") as f:
            return sum(1 for _ in f)
    except OSError:
        return 0


def go_loc(rel_dir):
    """Go 包的**非测试**行数；测试行数单独返回（它钉着契约，值得单列）。

    只算该包目录**自身**的文件：`piai/testdata/stubpi` 是子目录里独立的 main 包，
    不能被算进 `piai`。用 abspath 判断而不是相对路径字符串，才能正确处理根包（"."）。
    """
    prod = test = 0
    target = os.path.normpath(os.path.join(ROOT, rel_dir))
    for rel, ab in walk_files(rel_dir):
        if os.path.normpath(os.path.dirname(ab)) != target:
            continue
        if not rel.endswith(".go"):
            continue
        n = count_lines(ab)
        if rel.endswith("_test.go"):
            test += n
        else:
            prod += n
    return prod, test


def build_module_index():
    """目录 → 模块 id。用来把 import 路径解析回模块（对应要求 1 的"解析变量"）。"""
    return {m[4]: m[0] for m in MODULES if m[3] == "go"}


def resolve_import(path, mod_by_dir):
    if not path.startswith(MODULE_PATH):
        return None
    rest = path[len(MODULE_PATH):].lstrip("/")
    if rest == "":
        return "harness"
    return mod_by_dir.get(rest)


def extract():
    mod_by_dir = build_module_index()
    edges = []
    static_edges = []
    seen = set()

    def add(src, dst, kind, why=""):
        if src is None or dst is None or src == dst:
            return
        key = (src, dst, kind)
        if key in seen:
            return
        seen.add(key)
        e = {"source": src, "target": dst, "kind": kind}
        if why:
            e["why"] = why
        edges.append(e)

    # ── 1. 静态 import 边（call）──
    for rel, ab in walk_files("."):
        if not rel.endswith(".go") or rel.endswith("_test.go"):
            continue                       # 测试边不进生产图；测试行数另行统计
        src_dir = os.path.dirname(rel) or "."
        src = "harness" if src_dir == "." else mod_by_dir.get(src_dir)
        if src is None:
            continue
        for imp in parse_go_imports(ab):
            dst = resolve_import(imp, mod_by_dir)
            if dst:
                static_edges.append((src, dst, "call"))
                add(src, dst, "call")

    for rel, ab in walk_files("."):
        if not rel.endswith(".py"):
            continue
        src = rel if rel in {m[4] for m in MODULES} else None
        if src is None:
            continue
        for imp in parse_python_imports(ab):
            # 同目录模块（bridge.py → protocol.py）
            cand = os.path.join(os.path.dirname(rel), imp.replace(".", "/") + ".py")
            if cand in {m[4] for m in MODULES}:
                static_edges.append((src, cand, "call"))
                add(src, cand, "call")

    # ── 2. 派发/注入边（dispatch），逐条回查证据 ──
    evidence_report = []
    for src, dst, kind, why, ev_file, ev_rx in DISPATCH_EDGES:
        ev_path = os.path.join(ROOT, ev_file)
        try:
            text = open(ev_path, encoding="utf-8", errors="replace").read()
        except OSError:
            text = ""
        ok = re.search(ev_rx, text) is not None
        evidence_report.append((src, dst, ev_file, ev_rx, ok))
        if ok:
            add(src, dst, kind, why)

    # ── 3. 运行时/数据边 ──
    for src, dst, kind, why in RUNTIME_EDGES:
        add(src, dst, kind, why)

    return edges, evidence_report, static_edges


def loc_table():
    out = {}
    for mid, name, dom, lang, path, kind in MODULES:
        ab = os.path.join(ROOT, path)
        if lang == "go":
            prod, test = go_loc(path)
            out[mid] = (prod, test)
        else:
            out[mid] = (count_lines(ab), 0)
    return out


def build_tree(locs):
    by_dom = defaultdict(list)
    for mid, name, dom, lang, path, kind in MODULES:
        prod, test = locs[mid]
        node = {"id": mid, "name": name, "kind": kind, "language": lang,
                "loc": prod, "file": path if lang != "go" else path}
        if test:
            node["testLoc"] = test
        by_dom[dom].append(node)

    children = []
    for dom_id, dom_name, dom_desc in DOMAINS:
        kids = by_dom.get(dom_id, [])
        if dom_id == "dom:data":
            kids = [{"id": did, "name": dname, "kind": "datastore", "note": dnote}
                    for did, dname, dnote in DATASTORES]
        if not kids:
            continue
        children.append({"id": dom_id, "name": dom_name, "kind": "domain",
                         "note": dom_desc, "children": kids})
    return {"id": "sys", "name": "red-harness", "kind": "system", "children": children}


def validate(tree, edges, entry_points, dead_ends, flows):
    """viewer 的硬要求：每条边的两端、每个入口/死端/流程步骤的节点，
    都必须是树里存在的叶子 id。

    流程步骤也要校验：viewer 对未知 id 是**静默降级**的（`byId.get(id)?.name || id`），
    所以一个写错的节点 id 会安静地渲染成一串原始 id，不报错。这类错只能在这里挡住。
    """
    leaves = set()
    domains = set()

    def visit(n):
        if n.get("kind") == "domain":
            domains.add(n["id"])
        for c in n.get("children", []):
            leaves.add(c["id"])
            visit(c)

    visit(tree)
    problems = []
    for e in edges:
        for end in ("source", "target"):
            if e[end] not in leaves:
                problems.append(f"边 {e['source']}->{e['target']} 的 {end}={e[end]} 不是叶子")
    for ep in entry_points:
        if ep not in leaves:
            problems.append(f"入口点 {ep} 不是叶子")
    for de in dead_ends:
        if de not in leaves:
            problems.append(f"死端 {de} 不是叶子")
    for f in flows:
        if not f.get("steps"):
            problems.append(f"流程「{f.get('name')}」没有步骤")
        for i, st in enumerate(f.get("steps", []), 1):
            if not st.get("nodes"):
                problems.append(f"流程「{f.get('name')}」第 {i} 步没有节点")
            for nid in st.get("nodes", []):
                if nid not in leaves:
                    problems.append(f"流程「{f.get('name')}」第 {i} 步的节点 {nid} 不是叶子")
    return problems


def _mmid(node_id):
    """Mermaid 节点 id 只能有字母数字下划线。"""
    return "n_" + re.sub(r"[^A-Za-z0-9]", "_", node_id)


def mermaid_call_graph(edges, tree, entry_points):
    """域级 `graph TD`，入口点单独画出并高亮。

    为什么不逐模块画：17 个模块 + 54 条边远超 40 条，稠密的 Mermaid 不可读——
    那正是交互式 viewer 存在的理由。域级折叠 + 入口点单列，是这个规模下唯一
    既画得下又答得出「谁调谁」的形式。
    """
    dom_of = {}
    label = {}
    for dom in tree["children"]:
        label[dom["id"]] = dom["name"]
        for c in dom["children"]:
            dom_of[c["id"]] = dom["id"]
            label[c["id"]] = c["name"]

    # 域→域聚合。同一条域间边若既有 call 又有 dispatch，只画 dispatch（更强的断言）。
    agg = {}
    for e in edges:
        a, b = dom_of.get(e["source"]), dom_of.get(e["target"])
        if a and b and a != b:
            agg[(a, b)] = "dispatch" if e["kind"] == "dispatch" else agg.get((a, b), e["kind"])

    lines = ["graph TD",
             "  %% 域级调用图（由 extract_topology.py 生成）。",
             "  %% ==> dispatch：动态派发/注入，只看 import 是看不见的。",
             "  %% 蓝色 = 入口点（来自部署/构建描述，不是源码里的可达性）。"]
    for d in tree["children"]:
        lines.append(f'  {_mmid(d["id"])}["{d["name"]}"]')
    for ep in sorted(entry_points):
        lines.append(f'  {_mmid(ep)}["{label.get(ep, ep)}"]')
    for (a, b), k in sorted(agg.items()):
        arrow = "==>" if k == "dispatch" else "-->"
        lines.append(f'  {_mmid(a)} {arrow} {_mmid(b)}')
    # 入口点挂到它所在的域上，否则它们在图里是悬空的。
    for ep in sorted(entry_points):
        d = dom_of.get(ep)
        if d:
            lines.append(f'  {_mmid(ep)} --> {_mmid(d)}')
    lines.append("  classDef entry fill:#1f6feb,stroke:#0b3d91,color:#fff")
    lines.append("  class " + ",".join(_mmid(ep) for ep in sorted(entry_points)) + " entry")
    return "\n".join(lines) + "\n"


def mermaid_data_lineage(edges, tree):
    """`graph LR`，程序 → 数据存储，读/写分别标注。边数天然少，不必折叠。"""
    leaves = {}
    for dom in tree["children"]:
        for c in dom["children"]:
            leaves[c["id"]] = c["name"]
    data_edges = [e for e in edges if e["kind"] in ("read", "write")]

    # 节点先声明一次（同一个 store 会出现在多条边上，重复声明虽合法但很吵）。
    declared, order = set(), []
    for e in data_edges:
        for nid in (e["source"], e["target"]):
            if nid in declared:
                continue
            declared.add(nid)
            order.append(nid)

    lines = ["graph LR",
             "  %% 读写线（由 extract_topology.py 生成）。--> 读，==> 写。",
             "  %% 圆柱 = 数据存储。store 是绝大多数落点的写者，例外是图：",
             "  %% graph.json / graph.mmd 由装配层的 dagGraphSaver 驱动（store 提供方法，wire 提供装配）。"]
    for nid in order:
        name = leaves[nid]
        if nid.startswith("ds:"):
            lines.append(f'  {_mmid(nid)}[("{name}")]')
        else:
            lines.append(f'  {_mmid(nid)}["{name}"]')
    for e in data_edges:
        arrow = "==>" if e["kind"] == "write" else "-->"
        lines.append(f'  {_mmid(e["source"])} {arrow}|{e["kind"]}| {_mmid(e["target"])}')
    lines.append("  classDef store fill:#8b5cf6,stroke:#5b21b6,color:#fff")
    lines.append("  class " + ",".join(_mmid(n) for n in order if n.startswith("ds:")) + " store")
    return "\n".join(lines) + "\n"


def mermaid_critical_path(flow):
    """主流程的 flowchart TD。无遥测数据，所以不标 p50/p99——留注释说明而不是编数字。"""
    lines = ["flowchart TD",
             f"  %% 主流程：{flow['name']}（人设：{flow['persona']}）",
             "  %% 无遥测接入（本机不接平台、不跑真实 provider），故不标 p50/p99。"]
    for i, step in enumerate(flow["steps"]):
        nid = f"s{i}"
        lines.append(f'  {nid}["{i + 1}. {step["label"]}"]')
        if i:
            lines.append(f"  s{i - 1} --> {nid}")
        for node in step["nodes"]:
            safe = _mmid(node)
            lines.append(f'  {safe}[/"{node}"/]')
            lines.append(f"  {nid} -.-> {safe}")
    return "\n".join(lines) + "\n"


def main():
    edges, evidence_report, static_edges = extract()
    locs = loc_table()
    tree = build_tree(locs)

    # 死端计算：在**全部**边（含 dispatch）都进图之后才做，这是要求 4 的前提。
    leaf_ids = set()
    for dom in tree["children"]:
        for c in dom["children"]:
            leaf_ids.add(c["id"])
    entry_ids = {e[0] for e in ENTRY_POINTS}
    inbound = defaultdict(int)
    for e in edges:
        inbound[e["target"]] += 1
    computed_dead = sorted(
        lid for lid in leaf_ids
        if inbound[lid] == 0 and lid not in entry_ids
    )
    # 应用抑制规则：朴素图会误判的那些 id 一律不进 deadEnds。
    suppressed = {s[0] for s in NAIVE_GRAPH_FALSE_POSITIVES}
    dead_ends = [d for d in computed_dead if d not in suppressed]

    flows = load_flows()

    topo = {
        "system": "red-harness",
        "subtitle": "面向安全 Agent 开发者的 Go SDK + CLI：通用 Run/Target/Objective/Candidate/Evaluation 契约，"
                    "外层是 TSecBench CTF 平台与 pi agent，内层是容器隔离与「事实/答案」分离的持久化。",
        "root": tree,
        "edges": edges,
        "entryPoints": sorted(entry_ids),
        "deadEnds": dead_ends,
        "observations": OBSERVATIONS,
        "flows": flows,
    }

    problems = validate(tree, edges, topo["entryPoints"], dead_ends, flows)
    if problems:
        print("!! 校验失败：", file=sys.stderr)
        for p in problems:
            print("   " + p, file=sys.stderr)
        return 1

    with open(os.path.join(OUT_DIR, "topology.json"), "w", encoding="utf-8") as f:
        json.dump(topo, f, ensure_ascii=False, indent=2)

    with open(os.path.join(OUT_DIR, "call-graph.mmd"), "w", encoding="utf-8") as f:
        f.write(mermaid_call_graph(edges, tree, topo["entryPoints"]))
    with open(os.path.join(OUT_DIR, "data-lineage.mmd"), "w", encoding="utf-8") as f:
        f.write(mermaid_data_lineage(edges, tree))
    with open(os.path.join(OUT_DIR, "critical-path.mmd"), "w", encoding="utf-8") as f:
        f.write(mermaid_critical_path(flows[0]))

    print_summary(topo, edges, evidence_report, computed_dead, dead_ends, locs, static_edges)
    return 0


def load_flows():
    """人设走查。锚在人身上（研究员 / 被评测的 Agent / 审计员 / 题目作者），不是数据流标签。"""
    return [
        {
            "name": "研究员跑一次评测，拿回分数",
            "persona": "安全研究员（红队工程师）",
            "description": "在授权环境里敲一条命令，跑完一批题，结束时拿到分数与可复现的运行记录。",
            "steps": [
                {"label": "敲下 run，指定场景与预算", "nodes": ["cmd/red-harness", "internal/cli"]},
                {"label": "装配层把契约与实现拼成一台 Harness，并抢下全机唯一那把锁", "nodes": ["internal/wire", "harness", "ds:run.lock"]},
                {"label": "从平台取回题目清单与目标地址", "nodes": ["scenario", "bridge", "bridge/bridge.py", "ds:tsecbench-api"]},
                {"label": "为这道题拉起隔离容器与内网", "nodes": ["executor", "ds:docker-daemon", "ds:runner-image"]},
                {"label": "在容器里驱动 pi 跑题，事件实时回流", "nodes": ["piai", "executor"]},
                {"label": "过程与候选落进私密 trace，公开面只留指纹", "nodes": ["store", "ds:private-trace"]},
                {"label": "每题终态的 DAG 落盘，供事后复盘剪枝链", "nodes": ["internal/wire", "ds:graph.json", "ds:graph.mmd"]},
                {"label": "写完公开指标，list / stats 读得到", "nodes": ["ds:results", "internal/cli"]},
            ],
        },
        {
            "name": "被评测的 Agent 在沙箱里解一道题",
            "persona": "被评测的 AI Agent（与它背后的模型 provider）",
            "description": "一台隔离容器、一条受控的出网通道，Agent 在里面读题、试、把候选交回来。",
            "steps": [
                {"label": "收到题面与目标地址", "nodes": ["harness", "piai"]},
                {"label": "在隔离容器里执行命令（argv 级隔离）", "nodes": ["executor", "ds:docker-daemon"]},
                {"label": "出网只走白名单代理", "nodes": ["ds:host-netfilter", "ds:provider-api"]},
                {"label": "工具输出被实时判读成候选证据", "nodes": ["gate", "answer"]},
                {"label": "事实与意图进图，供下一轮提示剪枝", "nodes": ["dag"]},
                {"label": "候选明文只写私密 trace（0700/0600）", "nodes": ["store", "ds:private-trace"]},
                {"label": "把候选提交回平台换分——这条边**尚未被平台确认过**（submit 501）", "nodes": ["bridge/bridge.py", "ds:tsecbench-api"]},
            ],
        },
        {
            "name": "审计员核对：明文有没有出过私密面",
            "persona": "合规审计员（以及事后复盘的人）",
            "description": "不碰私密目录，只读公开面，核验候选明文一次都没有离开过受控范围。",
            "steps": [
                {"label": "只读公开面：结果文件里只有计数与指纹", "nodes": ["ds:results", "internal/cli"]},
                {"label": "每条候选在公开面只以一个指纹出现", "nodes": ["answer", "store"]},
                {"label": "要核明文必须进 private/<runID>/（0700/0600）", "nodes": ["ds:private-trace", "store"]},
                {"label": "图落盘前被统一擦洗一遍（含最易漏的 Raw 字段）", "nodes": ["internal/wire", "dag", "ds:graph.json"]},
                {"label": "桥的平台异常只从这里定位，且同样在私密面", "nodes": ["ds:bridge-stderr", "bridge"]},
            ],
        },
        {
            "name": "题目作者离线自检桥与判分",
            "persona": "题目作者 / 平台运营",
            "description": "没有 VPN、没有真 SDK 的机器上，把整条平台链路跑一遍，确认判分与错误处理都对。",
            "steps": [
                {"label": "设 TSEC_MOCK=1，PYTHONPATH 指到 testdata", "nodes": ["bridge/bridge.py", "bridge/testdata/mock_sdk.py"]},
                {"label": "假 SDK 逐字段对齐真 SDK，连两个缺陷一起照抄", "nodes": ["bridge/testdata/mock_sdk.py"]},
                {"label": "等桥打出 hello 握手行（探活判据）", "nodes": ["bridge", "bridge/bridge.py"]},
                {"label": "平台响应体落 private stderr 供定位", "nodes": ["ds:bridge-stderr", "store"]},
                {"label": "用 fake 场景跑通全流程，不碰网络", "nodes": ["scenario", "internal/wire"]},
                {"label": "证明 pi 真的跑在目标容器里而不是宿主", "nodes": ["runner/Dockerfile.teststub", "ds:runner-image", "piai/testdata/stubpi"]},
            ],
        },
    ]


def print_summary(topo, edges, evidence_report, computed_dead, dead_ends, locs, static_edges):
    W = 78
    print("=" * W)
    print("red-harness 依赖与拓扑提取")
    print("=" * W)

    print("\n── 模块（按域）──")
    for dom in topo["root"]["children"]:
        mods = [c for c in dom["children"] if c["kind"] != "datastore"]
        if not mods:
            continue
        print(f"\n  [{dom['name']}]  {dom.get('note', '')}")
        for c in sorted(mods, key=lambda x: -x["loc"]):
            t = f"  (+{c['testLoc']} 测试)" if c.get("testLoc") else ""
            print(f"    {c['name']:<26} {c['loc']:>6} 行{t}   {c['id']}")

    print("\n── 数据存储 ──")
    for dom in topo["root"]["children"]:
        for c in dom["children"]:
            if c["kind"] == "datastore":
                print(f"    {c['name']:<28} {c['id']}")

    kinds = defaultdict(int)
    for e in edges:
        kinds[e["kind"]] += 1
    print(f"\n── 边 ──  共 {len(edges)} 条：" +
          "，".join(f"{k} {v}" for k, v in sorted(kinds.items())))

    static = sorted(set(static_edges))
    print(f"\n  静态 import 边（{len(static)} 条，import 语句可证）：")
    for a, b, _ in static[:14]:
        print(f"    {a:<18} → {b}")
    if len(static) > 14:
        print(f"    …（余 {len(static) - 14} 条见 topology.json）")

    print(f"\n  dispatch 边（{len([e for e in edges if e['kind'] == 'dispatch'])} 条，"
          f"import 图**看不见**，逐条回查了源码证据）：")
    for e in edges:
        if e["kind"] == "dispatch":
            print(f"    {e['source']:<18} ⇒ {e['target']:<24} {e.get('why', '')}")

    print("\n── 证据回查（dispatch 边的真伪由源码现况决定）──")
    bad = 0
    for src, dst, ev_file, ev_rx, ok in evidence_report:
        mark = "OK  " if ok else "失配"
        if not ok:
            bad += 1
        print(f"    [{mark}] {src} ⇒ {dst}   ({ev_file})")
    print(f"    失配 {bad} 条" + ("" if bad == 0 else "  ← 图可能已漂移，请核对"))

    print(f"\n── 入口点（{len(topo['entryPoints'])} 个，来自部署/构建描述）──")
    for ep, why in ENTRY_POINTS:
        print(f"    {ep:<24} {why}")

    print(f"\n── 死端候选 ──")
    print(f"    计算得 {len(computed_dead)} 个无入边叶子，抑制后 **{len(dead_ends)}** 个。")
    print("    朴素图（只 grep import）会误判成孤岛的每一处，都命中抑制规则：")
    for did, why in NAIVE_GRAPH_FALSE_POSITIVES:
        print(f"      [抑制] {did}\n             {why}")
    if bad == 0 and not dead_ends:
        print("    结论：没有孤儿模块。看起来像孤岛的每一处都是「动态端口的预定目标」，不是死代码。")

    print(f"\n── 架构观察（{len(topo['observations'])} 条）──")
    for i, o in enumerate(topo["observations"], 1):
        print(f"  {i}. {o}")

    print(f"\n── 人设走查（{len(topo['flows'])} 条流程）──")
    for f in topo["flows"]:
        print(f"\n  【{f['name']}】  人设：{f['persona']}")
        print(f"    {f['description']}")
        for i, s in enumerate(f["steps"], 1):
            print(f"      {i}. {s['label']}")
            print(f"         └─ {', '.join(s['nodes'])}")

    print(f"\n{'=' * W}")
    print(f"写出：topology.json / call-graph.mmd / data-lineage.mmd / critical-path.mmd")
    print("=" * W)


if __name__ == "__main__":
    sys.exit(main())
