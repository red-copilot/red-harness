# Harness 架构设计方案 vNext

基于 red-copilot/red-harness · main · 2026-10-08

核心结论：保留 Python 模块化单体，采用「事件驱动执行内核 + 可验证世界状态 + 闭环决策 + 隔离执行环境」的架构。

Harness 不应该演变成另一个庞大的 Agent 框架，也不应该成为专门针对 TSecBench 的解题脚本集合。

它的定位应当是：

> 一个面向授权安全测试、CTF 和安全评测任务的通用自主求解运行时，能够管理任务、环境、证据、决策、执行、验证与恢复，并通过反馈持续提高任务完成能力。

## 一、当前仓库诊断

我已经读取了 `main` 分支的架构文档、World State、SolverLoop、Planner、Pi Adapter、Benchmark Runner、SQLite 和相关测试代码。

现有架构基础

值得保留

| 模块           | 当前实现                     | 评价       |
| ------------ | ------------------------ | -------- |
| Python 模块化单体 | `src/harness/`           | 合适       |
| World State  | SQLite、事件日志、投影           | 基础良好     |
| SolverLoop   | 事件处理、验证、重规划              | 闭环仍需加强   |
| Planner      | Rolling Horizon、Skill 排序 | 决策能力较弱   |
| Agent        | Pi/Kali Docker           | 可继续使用    |
| Benchmark    | 通用协议、TSec Adapter        | 方向正确     |
| Coordination | SQLite 租约、Blackboard     | 暂不需要扩张   |
| Recovery     | Checkpoint、World Resume  | 尚非完整故障恢复 |

### 最重要的四个问题

P0

1\. Pi 尚未形成可靠的双向交互闭环

当前主路径使用 `pi --mode json`，Agent 运行时 Harness 把反馈写入文件，并依赖 Agent 主动读取。它不等同于 Harness 能在下一轮模型推理前主动注入反馈。

Pi 官方已经提供长期运行的 RPC 模式，支持 `prompt`、`steer`、`abort` 等控制能力，更适合这种场景。

[image](https://www.google.com/s2/favicons?domain=https://pi.dev\&sz=32)

Pi

+1



P0

2\. 可信验证没有真正接入主循环

`ActionVerifier` 支持 `trusted_evidence`，但 `SolverLoop.process_event()` 调用 `verify()` 时没有传入这个参数。

因此正常事件路径中，Agent 报告的成功通常只能得到 `pending`，无法转化为独立验证的 `verified`。这是代码层面的实际断点。

P1

3\. World State 对决策的作用还不够强

当前规划主要依据 Skill 前置条件、产出新颖度、成本、风险和噪声排序，并非真正的目标驱动推理。

世界状态已经可以持久化，但「某条新证据具体应当改变哪个行动选择」尚缺少严格机制。

P1

4\. 恢复机制尚不能保证任务级连续性

现有 Checkpoint 保存事件偏移量、World Revision 等信息，但代码明确没有提供崩溃后 exactly-once 执行保证。

对长时间安全任务，还需要未完成动作对账、提交幂等和预算恢复。

这些不是要求重写项目的理由。恰恰相反，现有基础已经足够，下一阶段应当把已有模块连接成真正有效的求解系统，而非继续增加概念和框架层数。

## 二、目标架构

Harness vNext — 逻辑架构

\#chatgpt-mermaid-\_r_2rp\_{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:16px;fill:rgb(237, 237, 237);}@keyframes edge-animation-frame{from{stroke-dashoffset:0;}}@keyframes dash{to{stroke-dashoffset:0;}}#chatgpt-mermaid-\_r_2rp\_ .edge-animation-slow{stroke-dasharray:9,5!important;stroke-dashoffset:900;animation:dash 50s linear infinite;stroke-linecap:round;}#chatgpt-mermaid-\_r_2rp\_ .edge-animation-fast{stroke-dasharray:9,5!important;stroke-dashoffset:900;animation:dash 20s linear infinite;stroke-linecap:round;}#chatgpt-mermaid-\_r_2rp\_ .error-icon{fill:rgb(48, 48, 48);}#chatgpt-mermaid-\_r_2rp\_ .error-text{fill:rgb(237, 237, 237);stroke:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2rp\_ .edge-thickness-normal{stroke-width:1px;}#chatgpt-mermaid-\_r_2rp\_ .edge-thickness-thick{stroke-width:3.5px;}#chatgpt-mermaid-\_r_2rp\_ .edge-pattern-solid{stroke-dasharray:0;}#chatgpt-mermaid-\_r_2rp\_ .edge-thickness-invisible{stroke-width:0;fill:none;}#chatgpt-mermaid-\_r_2rp\_ .edge-pattern-dashed{stroke-dasharray:3;}#chatgpt-mermaid-\_r_2rp\_ .edge-pattern-dotted{stroke-dasharray:2;}#chatgpt-mermaid-\_r_2rp\_ .marker{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_2rp\_ .marker.cross{stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_2rp\_ svg{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:16px;}#chatgpt-mermaid-\_r_2rp\_ p{margin:0;}#chatgpt-mermaid-\_r_2rp\_ .label{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2rp\_ .cluster-label text{fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2rp\_ .cluster-label span{color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2rp\_ .cluster-label span p{background-color:transparent;}#chatgpt-mermaid-\_r_2rp\_ .label text,#chatgpt-mermaid-\_r_2rp\_ span{fill:rgb(237, 237, 237);color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2rp\_ .node rect,#chatgpt-mermaid-\_r_2rp\_ .node circle,#chatgpt-mermaid-\_r_2rp\_ .node ellipse,#chatgpt-mermaid-\_r_2rp\_ .node polygon,#chatgpt-mermaid-\_r_2rp\_ .node path{fill:rgb(9, 23, 44);stroke:rgb(31, 78, 148);stroke-width:1px;}#chatgpt-mermaid-\_r_2rp\_ .rough-node .label text,#chatgpt-mermaid-\_r_2rp\_ .node .label text,#chatgpt-mermaid-\_r_2rp\_ .image-shape .label,#chatgpt-mermaid-\_r_2rp\_ .icon-shape .label{text-anchor:middle;}#chatgpt-mermaid-\_r_2rp\_ .node .katex path{fill:#000;stroke:#000;stroke-width:1px;}#chatgpt-mermaid-\_r_2rp\_ .rough-node .label,#chatgpt-mermaid-\_r_2rp\_ .node .label,#chatgpt-mermaid-\_r_2rp\_ .image-shape .label,#chatgpt-mermaid-\_r_2rp\_ .icon-shape .label{text-align:center;}#chatgpt-mermaid-\_r_2rp\_ .node.clickable{cursor:pointer;}#chatgpt-mermaid-\_r_2rp\_ .root .anchor path{fill:rgb(175, 175, 175)!important;stroke-width:0;stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_2rp\_ .arrowheadPath{fill:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_2rp\_ .edgePath .path{stroke:rgb(175, 175, 175);stroke-width:1px;}#chatgpt-mermaid-\_r_2rp\_ .flowchart-link{stroke:rgb(175, 175, 175);fill:none;}#chatgpt-mermaid-\_r_2rp\_ .edgeLabel{background-color:rgb(0, 0, 0);text-align:center;}#chatgpt-mermaid-\_r_2rp\_ .edgeLabel p{background-color:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_2rp\_ .edgeLabel rect{opacity:0.5;background-color:rgb(0, 0, 0);fill:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_2rp\_ .labelBkg{background-color:rgba(0, 0, 0, 0.5);}#chatgpt-mermaid-\_r_2rp\_ .cluster rect{fill:rgb(48, 48, 48);stroke:rgba(255, 255, 255, 0.15);stroke-width:1px;}#chatgpt-mermaid-\_r_2rp\_ .cluster text{fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2rp\_ .cluster span{color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2rp\_ div.mermaidTooltip{position:absolute;text-align:center;max-width:200px;padding:2px;font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:12px;background:rgb(48, 48, 48);border:1px solid rgba(255, 255, 255, 0.15);border-radius:2px;pointer-events:none;z-index:100;}#chatgpt-mermaid-\_r_2rp\_ .flowchartTitleText{text-anchor:middle;font-size:18px;fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2rp\_ rect.text{fill:none;stroke-width:0;}#chatgpt-mermaid-\_r_2rp\_ .icon-shape,#chatgpt-mermaid-\_r_2rp\_ .image-shape{background-color:rgb(0, 0, 0);text-align:center;}#chatgpt-mermaid-\_r_2rp\_ .icon-shape p,#chatgpt-mermaid-\_r_2rp\_ .image-shape p{background-color:rgb(0, 0, 0);padding:2px;}#chatgpt-mermaid-\_r_2rp\_ .icon-shape .label rect,#chatgpt-mermaid-\_r_2rp\_ .image-shape .label rect{opacity:0.5;background-color:rgb(0, 0, 0);fill:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_2rp\_ .label-icon{display:inline-block;height:1em;overflow:visible;vertical-align:-0.125em;}#chatgpt-mermaid-\_r_2rp\_ .node .label-icon path{fill:currentColor;stroke:revert;stroke-width:revert;}#chatgpt-mermaid-\_r_2rp\_ .node .neo-node{stroke:rgb(31, 78, 148);}#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].node rect,#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].cluster rect,#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].node polygon{stroke:url(#chatgpt-mermaid-\_r_2rp\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].swimlane.cluster rect{filter:none;}#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].node path{stroke:url(#chatgpt-mermaid-\_r_2rp\_-gradient);stroke-width:1px;}#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].node .outer-path{filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].node .neo-line path{stroke:rgb(31, 78, 148);filter:none;}#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].node circle{stroke:url(#chatgpt-mermaid-\_r_2rp\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].node circle .state-start{fill:#000000;}#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].icon-shape .icon{fill:url(#chatgpt-mermaid-\_r_2rp\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_2rp\_ [data-look="neo"].icon-shape .icon-neo path{stroke:url(#chatgpt-mermaid-\_r_2rp\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_2rp\_ .node text{font-size:14px;font-weight:600;letter-spacing:normal;fill:rgb(153, 206, 255);}#chatgpt-mermaid-\_r_2rp\_ .edgeLabels text{font-size:13px;font-weight:600;letter-spacing:-0.08px;fill:rgb(153, 206, 255);}#chatgpt-mermaid-\_r_2rp\_ .node tspan[font-weight="normal"],#chatgpt-mermaid-\_r_2rp\_ .edgeLabels tspan[font-weight="normal"]{font-weight:600;}#chatgpt-mermaid-\_r_2rp\_ .edgeLabel .label rect{opacity:1;rx:13px;ry:13px;fill:rgb(0, 14, 26);stroke:rgb(26, 62, 95);stroke-width:1px;}#chatgpt-mermaid-\_r_2rp\_ .node rect,#chatgpt-mermaid-\_r_2rp\_ .node circle,#chatgpt-mermaid-\_r_2rp\_ .node ellipse,#chatgpt-mermaid-\_r_2rp\_ .node polygon,#chatgpt-mermaid-\_r_2rp\_ .node path{fill:rgb(0, 40, 77);stroke:rgba(255, 255, 255, 0.1);stroke-width:1px;}#chatgpt-mermaid-\_r_2rp\_ .node rect{rx:16px;ry:16px;}#chatgpt-mermaid-\_r_2rp\_ .node.mermaid-decision .label-container{fill:rgb(0, 14, 26);stroke:rgb(26, 62, 95);stroke-dasharray:2,2;}#chatgpt-mermaid-\_r_2rp\_ .edgePaths .flowchart-link{stroke:rgb(175, 175, 175);stroke-width:1px;stroke-linecap:round;stroke-linejoin:round;}#chatgpt-mermaid-\_r_2rp\_ .marker{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_2rp\_ :root{--mermaid-font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";}CLI / API / WorkerRun RuntimeBenchmark AdapterSolver EnginePi RPC SessionKali Tool EnvironmentWorld State / SQLiteEvidence VerifierContext RetrievalDecision PolicyCheckpoint / Trace / Budget

Runtime 管生命周期；Solver Engine 管闭环；Pi 负责实际推理和工具使用；World State 保存知识；Verifier 决定证据是否成立。

我建议保持以下职责分工。

| 组件            | 只负责什么          | 不应该负责什么          |
| ------------- | -------------- | ---------------- |
| Runtime       | 生命周期、调度、预算、恢复  | 漏洞推理             |
| AgentSession  | Pi RPC 通信、模型事件 | 世界状态权威写入         |
| Solver Engine | 反馈、决策控制、重规划    | Benchmark SDK 细节 |
| World         | 事实、假设、能力、证据、历史 | 自动执行攻击           |
| Planner       | 给出候选行动与选择依据    | 独占 Agent 的内部规划   |
| Verifier      | 独立确认行动结果       | 相信 Agent 的自述     |
| Benchmark     | 环境生命周期、提交与评分   | 控制求解策略           |

特别要避免 Harness 和 Pi 同时拥有两套完整的自主规划机制。

Pi 是主要的推理者，Harness 是拥有可验证状态、约束和反馈权威的控制器。

Harness 可以建议、纠偏、阻止、恢复，但不应每一步都强制替代 Pi 的思考。

## 三、核心求解循环

目标不是把更多事件写到 SQLite，而是让事件真正改变决策。

\#chatgpt-mermaid-\_r_2s2\_{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:16px;fill:rgb(237, 237, 237);}@keyframes edge-animation-frame{from{stroke-dashoffset:0;}}@keyframes dash{to{stroke-dashoffset:0;}}#chatgpt-mermaid-\_r_2s2\_ .edge-animation-slow{stroke-dasharray:9,5!important;stroke-dashoffset:900;animation:dash 50s linear infinite;stroke-linecap:round;}#chatgpt-mermaid-\_r_2s2\_ .edge-animation-fast{stroke-dasharray:9,5!important;stroke-dashoffset:900;animation:dash 20s linear infinite;stroke-linecap:round;}#chatgpt-mermaid-\_r_2s2\_ .error-icon{fill:rgb(48, 48, 48);}#chatgpt-mermaid-\_r_2s2\_ .error-text{fill:rgb(237, 237, 237);stroke:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2s2\_ .edge-thickness-normal{stroke-width:1px;}#chatgpt-mermaid-\_r_2s2\_ .edge-thickness-thick{stroke-width:3.5px;}#chatgpt-mermaid-\_r_2s2\_ .edge-pattern-solid{stroke-dasharray:0;}#chatgpt-mermaid-\_r_2s2\_ .edge-thickness-invisible{stroke-width:0;fill:none;}#chatgpt-mermaid-\_r_2s2\_ .edge-pattern-dashed{stroke-dasharray:3;}#chatgpt-mermaid-\_r_2s2\_ .edge-pattern-dotted{stroke-dasharray:2;}#chatgpt-mermaid-\_r_2s2\_ .marker{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_2s2\_ .marker.cross{stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_2s2\_ svg{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:16px;}#chatgpt-mermaid-\_r_2s2\_ p{margin:0;}#chatgpt-mermaid-\_r_2s2\_ .label{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2s2\_ .cluster-label text{fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2s2\_ .cluster-label span{color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2s2\_ .cluster-label span p{background-color:transparent;}#chatgpt-mermaid-\_r_2s2\_ .label text,#chatgpt-mermaid-\_r_2s2\_ span{fill:rgb(237, 237, 237);color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2s2\_ .node rect,#chatgpt-mermaid-\_r_2s2\_ .node circle,#chatgpt-mermaid-\_r_2s2\_ .node ellipse,#chatgpt-mermaid-\_r_2s2\_ .node polygon,#chatgpt-mermaid-\_r_2s2\_ .node path{fill:rgb(9, 23, 44);stroke:rgb(31, 78, 148);stroke-width:1px;}#chatgpt-mermaid-\_r_2s2\_ .rough-node .label text,#chatgpt-mermaid-\_r_2s2\_ .node .label text,#chatgpt-mermaid-\_r_2s2\_ .image-shape .label,#chatgpt-mermaid-\_r_2s2\_ .icon-shape .label{text-anchor:middle;}#chatgpt-mermaid-\_r_2s2\_ .node .katex path{fill:#000;stroke:#000;stroke-width:1px;}#chatgpt-mermaid-\_r_2s2\_ .rough-node .label,#chatgpt-mermaid-\_r_2s2\_ .node .label,#chatgpt-mermaid-\_r_2s2\_ .image-shape .label,#chatgpt-mermaid-\_r_2s2\_ .icon-shape .label{text-align:center;}#chatgpt-mermaid-\_r_2s2\_ .node.clickable{cursor:pointer;}#chatgpt-mermaid-\_r_2s2\_ .root .anchor path{fill:rgb(175, 175, 175)!important;stroke-width:0;stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_2s2\_ .arrowheadPath{fill:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_2s2\_ .edgePath .path{stroke:rgb(175, 175, 175);stroke-width:1px;}#chatgpt-mermaid-\_r_2s2\_ .flowchart-link{stroke:rgb(175, 175, 175);fill:none;}#chatgpt-mermaid-\_r_2s2\_ .edgeLabel{background-color:rgb(0, 0, 0);text-align:center;}#chatgpt-mermaid-\_r_2s2\_ .edgeLabel p{background-color:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_2s2\_ .edgeLabel rect{opacity:0.5;background-color:rgb(0, 0, 0);fill:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_2s2\_ .labelBkg{background-color:rgba(0, 0, 0, 0.5);}#chatgpt-mermaid-\_r_2s2\_ .cluster rect{fill:rgb(48, 48, 48);stroke:rgba(255, 255, 255, 0.15);stroke-width:1px;}#chatgpt-mermaid-\_r_2s2\_ .cluster text{fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2s2\_ .cluster span{color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2s2\_ div.mermaidTooltip{position:absolute;text-align:center;max-width:200px;padding:2px;font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:12px;background:rgb(48, 48, 48);border:1px solid rgba(255, 255, 255, 0.15);border-radius:2px;pointer-events:none;z-index:100;}#chatgpt-mermaid-\_r_2s2\_ .flowchartTitleText{text-anchor:middle;font-size:18px;fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_2s2\_ rect.text{fill:none;stroke-width:0;}#chatgpt-mermaid-\_r_2s2\_ .icon-shape,#chatgpt-mermaid-\_r_2s2\_ .image-shape{background-color:rgb(0, 0, 0);text-align:center;}#chatgpt-mermaid-\_r_2s2\_ .icon-shape p,#chatgpt-mermaid-\_r_2s2\_ .image-shape p{background-color:rgb(0, 0, 0);padding:2px;}#chatgpt-mermaid-\_r_2s2\_ .icon-shape .label rect,#chatgpt-mermaid-\_r_2s2\_ .image-shape .label rect{opacity:0.5;background-color:rgb(0, 0, 0);fill:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_2s2\_ .label-icon{display:inline-block;height:1em;overflow:visible;vertical-align:-0.125em;}#chatgpt-mermaid-\_r_2s2\_ .node .label-icon path{fill:currentColor;stroke:revert;stroke-width:revert;}#chatgpt-mermaid-\_r_2s2\_ .node .neo-node{stroke:rgb(31, 78, 148);}#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].node rect,#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].cluster rect,#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].node polygon{stroke:url(#chatgpt-mermaid-\_r_2s2\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].swimlane.cluster rect{filter:none;}#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].node path{stroke:url(#chatgpt-mermaid-\_r_2s2\_-gradient);stroke-width:1px;}#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].node .outer-path{filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].node .neo-line path{stroke:rgb(31, 78, 148);filter:none;}#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].node circle{stroke:url(#chatgpt-mermaid-\_r_2s2\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].node circle .state-start{fill:#000000;}#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].icon-shape .icon{fill:url(#chatgpt-mermaid-\_r_2s2\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_2s2\_ [data-look="neo"].icon-shape .icon-neo path{stroke:url(#chatgpt-mermaid-\_r_2s2\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_2s2\_ .node text{font-size:14px;font-weight:600;letter-spacing:normal;fill:rgb(153, 206, 255);}#chatgpt-mermaid-\_r_2s2\_ .edgeLabels text{font-size:13px;font-weight:600;letter-spacing:-0.08px;fill:rgb(153, 206, 255);}#chatgpt-mermaid-\_r_2s2\_ .node tspan[font-weight="normal"],#chatgpt-mermaid-\_r_2s2\_ .edgeLabels tspan[font-weight="normal"]{font-weight:600;}#chatgpt-mermaid-\_r_2s2\_ .edgeLabel .label rect{opacity:1;rx:13px;ry:13px;fill:rgb(0, 14, 26);stroke:rgb(26, 62, 95);stroke-width:1px;}#chatgpt-mermaid-\_r_2s2\_ .node rect,#chatgpt-mermaid-\_r_2s2\_ .node circle,#chatgpt-mermaid-\_r_2s2\_ .node ellipse,#chatgpt-mermaid-\_r_2s2\_ .node polygon,#chatgpt-mermaid-\_r_2s2\_ .node path{fill:rgb(0, 40, 77);stroke:rgba(255, 255, 255, 0.1);stroke-width:1px;}#chatgpt-mermaid-\_r_2s2\_ .node rect{rx:16px;ry:16px;}#chatgpt-mermaid-\_r_2s2\_ .node.mermaid-decision .label-container{fill:rgb(0, 14, 26);stroke:rgb(26, 62, 95);stroke-dasharray:2,2;}#chatgpt-mermaid-\_r_2s2\_ .edgePaths .flowchart-link{stroke:rgb(175, 175, 175);stroke-width:1px;stroke-linecap:round;stroke-linejoin:round;}#chatgpt-mermaid-\_r_2s2\_ .marker{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_2s2\_ :root{--mermaid-font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";}读取目标与当前 World选择下一步行动Pi 执行工具或推理收集结果和证据可信验证更新已证实状态记录失败与修正假设保持待验证目标完成？重新评估候选行动最终验证 / 结果归档成立反证证据不足否是

这里有三个设计要点。

第一，行动成功不等于任务取得进展。 当前 `ProgressLedger` 在很多非错误工具结果之后就清零 `no_progress_count`。但工具正常退出不代表产生了新能力、新证据或接近任务目标。

建议将进展区分为：

| 信号                    | 含义         |
| --------------------- | ---------- |
| `execution_succeeded` | 工具执行成功     |
| `evidence_confirmed`  | 获得可信新证据    |
| `capability_acquired` | 获得可用的新能力   |
| `goal_advanced`       | 对目标产生可验证进展 |
| `objective_completed` | 最终目标被确认完成  |

只有后面三类才能稳定影响任务进度。

第二，重规划应该针对具体失败原因。 例如工具不可用、假设被推翻、能力过期、证据互相矛盾和无进展，不应全部处理成「再执行同一批 Skill 排序」。

第三，Agent 的推测不能自行升级为已验证事实。 即使一个工具命令退出码是 0，也不能据此认定它报告的目标漏洞已经得到证实。可信验证必须识别证据来源和验证条件。

## 四、World State：保留，但改变使用方式

目前的十类对象设计基本合理，不需要再引入庞大的安全领域本体。

建议在现有模型上强化三个方面。

### 1. 事实可信度

将信息区分为三种使用层级：

Claim

Agent 或工具声称成立

Evidence

有可追溯的观测依据

Verified

通过可信验证规则

这三个层级不一定要做成三张新表。建议继续使用 World Event 和 Pydantic 模型，通过证据引用、来源、验证结论进行表达。

重要的是 `confirmed_fact` 不能直接由 Agent 的自由文本赋予权威性。

### 2. 以能力为中心，而不是固定攻击 DAG

通用安全任务不是一条预先确定的流程。

Web、二进制、逆向、取证、云环境以及多目标任务，可能同时存在循环探索、可选路径和反复验证。

因此：

- World State 使用一般有向属性图，允许循环。
- Goal 可以具有父子关系或前置条件，但不强制全局 DAG。
- Planner 维护短期行动候选，而不是生成庞大任务图。
- Capability 表示当前能做什么、在什么范围内有效，以及它是否已经过期。

这比强制设计一个通用攻击 DAG 更有适应性。

### 3. 世界模型不等于世界状态

当前实现准确说是事件驱动的符号状态模型，尚不能称为具备预测能力的完整世界模型。

要让它进一步有用，需要支持：

\\[ (s_t,a_t)\rightarrow \widehat{s}\_{t+1} \\]

即预测某个行动可能改变什么，再比较实际观测与预测的偏差。

但我不建议现阶段引入神经世界模型或复杂的学习型转移模型。先通过 Skill 元数据给出预期结果，再基于真实执行数据评估这些预期是否准确。

只要能够证明它比没有状态的求解流程更有效，再考虑学习型预测。

## 五、Pi 的正确集成方式

这是我建议优先改造的部分。

当前 `pi_adapter.py`、`pi_container.py` 和 `session.py` 具备可复用基础，可以渐进迁移为：

Harness

Pi RPC

Kali Tools

Harness 发送 prompt、steer、abort，接收执行事件并做验证。Pi 保持可交互会话，不再完全依赖 Agent 自行轮询反馈文件。

Pi 的 RPC 模式支持跨进程 JSONL 双向协议，很适合 Python Harness 控制 Docker 内的 Pi；无需仅为集成 Pi 而重构为 TypeScript。

[image](https://www.google.com/s2/favicons?domain=https://pi.dev\&sz=32)

Pi

+1



关键工程要求包括指令相关 ID、流式事件读取、消息顺序、超时取消、重复事件去重、会话崩溃检测，以及模型回合之间的反馈注入。

还应注意，Pi 的 `agent_end` 不一定代表所有后续工作都完成，判断整个会话空闲应按照 RPC 文档处理 `agent_settled`。

[image](https://www.google.com/s2/favicons?domain=https://pi.dev\&sz=32)

Pi

+1



### 离线和网络模型

必须区分：

| 网络范围                    | 访问规则           |
| ----------------------- | -------------- |
| Agent → Benchmark 目标    | 在授权范围内允许       |
| Agent → 公共互联网           | 默认禁止           |
| Harness → Benchmark SDK | 按部署要求放行        |
| Gateway → 模型提供方         | 允许指定端点，或使用本地模型 |

如果比赛完全隔离公网，远程模型也无法从同一隔离网络直接调用，就需要使用独立、合规的模型通道或本地模型。

`pi.offline` 和 Docker 网络隔离并不是同一个概念，也不应该把 `network: host` 当成默认安全方案。

建议提供 `isolated`、`benchmark`、`development` 三种明确的运行配置，分别做网络连通性测试，避免仅靠配置名称宣称隔离已生效。

## 六、模块调整：尽量少移动文件

不建议进行新一轮大规模目录重构。

在现有代码基础上增加少量明确的实现边界即可：

```
src/harness/
├── runtime/
│   ├── engine.py          # 求解生命周期协调
│   ├── events.py          # 统一事件信封与去重
│   ├── checkpoint.py      # 恢复游标
│   └── bootstrap.py
├── world/
│   ├── models.py
│   ├── sqlite.py
│   ├── evidence.py        # 证据来源与可信等级
│   ├── retrieval.py
│   └── context.py
├── benchmark/
│   ├── base.py
│   ├── runner.py
│   └── tsec_adapter.py
├── pi_adapter.py
├── pi_container.py
├── session.py
├── solver_loop.py
├── planner.py
├── action_verifier.py
└── ...
```

不要在这个阶段进一步拆成微服务、独立规划服务、向量数据库或分布式知识图谱。

SQLite 可以继续作为单次运行的事件权威存储。对于多 Worker 协调，复用现有队列和租约机制，而不是让多个 Agent 直接竞争同一个 World 数据库的写权限。

## 七、实施路线图

## P0

先修闭环正确性

将可信 Evidence 接入 SolverLoop；修正进展判定；让重规划原因在被处理后正确消费；保证最终结果以 Benchmark 或 Verifier 的结论为准。

目标：不再把工具成功或 Agent 自述当成任务成功。

## P1

Pi RPC 交互化

实现真正的双向 AgentSession，将 Harness 验证反馈在下一轮推理前可靠送达。保留 JSON 模式作为兼容路径。

目标：一次失败能够实际改变后续工具使用和推理。

## P2

World 驱动的目标决策

给候选行动增加目标相关度、信息增益、已有失败惩罚、能力有效性与预算约束。当前 Rolling Horizon 仍保持短期规划，不升级为大型 DAG 调度器。

目标：World State 的变化能产生可解释、可测试的行动差异。

## P3

可靠恢复和多场景验证

完善 Checkpoint、事件幂等、未完成操作对账、成本累计与故障注入测试。再验证 Web、Binary、Reverse、Forensics 等多领域 Benchmark Adapter。

目标：可靠完成长任务，并证明架构收益具有跨场景泛化性。

## 八、怎么证明新架构真的更强？

不能仅凭目录变整齐、事件数量增加或 Planner 输出更多字段判断成功。

建议同一批授权评测任务进行消融实验。

| 实验                 | 目标       |
| ------------------ | -------- |
| Pi 原生求解            | 建立基线     |
| Pi + Harness 验证反馈  | 测量交互闭环收益 |
| Pi + World Context | 测量持久知识收益 |
| Pi + Planner       | 测量行动选择收益 |
| 完整 Harness         | 评估总体效果   |

在统一模型、任务、预算和随机种子策略下，记录成功率、得分、完成时间、token 消耗、重复失败次数、错误验证次数与崩溃恢复成功率。

尤其需要两个关键测试：

反事实决策测试： 给定相同任务及不同 World State，检查下一步行动是否合理改变。如果结果总是相同，World State 对决策可能仍然没有实质贡献。

中断恢复测试： 在工具执行、提交和状态更新的不同阶段强制终止进程，恢复后检查有没有重复提交、重复记账、证据丢失和错误的目标完成状态。

## 九、最终架构原则

我建议用五条原则约束后续开发：

1. Correctness before intelligence：先保证证据、状态和反馈正确，再追求更复杂的规划。
2. Agent reasons, Harness verifies：Agent 自主推理，Harness 负责可信反馈和执行约束。
3. Events are authoritative：事件可重放，Snapshot、Context 和 Plan 都是派生结果。
4. General core, specialized adapters：通用内核不包含 TSec 特调逻辑。
5. Complexity must earn its place：新机制必须通过消融实验证明收益，否则不引入。

最应该马上做的不是新的世界模型，而是修复 `ActionVerifier → SolverLoop → Pi RPC → World` 这条闭环。

这会直接提升 Harness 利用失败反馈、纠正推理、避免重复探索的能力，也是后续建设真正的世界模型、自适应规划与多 Agent 协作的基础。

目前这是基于仓库代码和官方协议完成的架构设计审查，尚未修改仓库，也未执行其测试套件。