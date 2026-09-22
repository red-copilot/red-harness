# R0 执行手册（v0.4 发布门收尾）

> ⚠️ **本文是手册，不是记录。** 下面每一步都**需要授权环境**——`tun0` 在、有效的
> `BENCHMARK_TOKEN`、有效的 provider 凭据。本机（离线）跑不了，**也不得用 Fake
> 或旧入口的结果替代**。
>
> 依据：`docs/offensive-harness-sdk-roadmap.md` 的 R0 一节。状态以
> `docs/roadmap.md` 的出口门表为准。

## 凭据纪律（每一步都适用）

- **不读取** `.env` / `/tmp/tsec/TsecBench-main/.agent.env` 的**值**；体检只看「有没有设置」。
- 凭据只经**子进程环境变量**传递，**不进 argv**（`ps` 可见）。provider key 走 0600 的
  `--env-file`，平台 token 走 bridge 子进程环境变量。
- 候选明文与平台响应片段只进 `0700/0600` 的私密面；**报告里贴证据前先脱敏**
  （token、flag、内网地址、request ID 视情况）。
- 失败要报失败。**不得记为跳过**——「跳过」与「通过」在 CI 里必须可区分。

## 步骤 1：发布配置核验

```bash
go run ./cmd/red-harness doctor --scenario tsecbench --provider <provider>
```

逐项确认：

| 检查项 | 期望 | 备注 |
|---|---|---|
| `scenario_name` | `tsecbench` | 传了 `--scenario` 才算；不传时这两项**形同虚设**（曾经的真缺陷，已修） |
| `provider_credentials` | ok | 只看「有没有设置」，不打印值 |
| `runner_image` | ok 且 digest 与 `runner/README.md` 记录一致 | tag 会漂移，digest 才是身份 |
| `graph_saver` / `candidate_audit` | ok | 可选端口，但「没接」必须看得见 |
| VPN | `tun0` UP | 平台调用必需 |

镜像内 pi 版本会被 `Probe` 核验（`ProbeResult.PiVersion`），空值即拒绝启动。
授权环境实测记录：`0.85.1`，落在支持区间 `0.85.0–0.86.99`。

**隔离正反探针**（跑动中在容器内执行，用 `curl` 而不是 `/dev/tcp`——后者是 bashism，
容器里的 `sh` 是 dash，会让**每一个**读数都返回「不可达」，从而得出一个自信的错误结论）：

| 观测点 | 期望 |
|---|---|
| 授权目标 `IP:port` 直连 | 可达（如 302/200） |
| 公网 `1.1.1.1:443` 直连 | 不可达 |
| 非授权内网地址直连 | 不可达 |
| 白名单内域名经代理 | 200 |
| 白名单外域名经代理 | 405（代理拒绝） |
| 容器内 `*_PROXY` 环境变量 | 6 个（大小写各一份） |
| `iptables -S DOCKER-USER` / `-S INPUT` | 各有 red-harness 规则，注释带 runID |

⚠️ **边界照旧**：同一 Docker bridge 内的流量不经过当前 iptables 规则。所以结论是
「非授权端点不可达」，**不是**「对任意同桥容器也隔离」。

## 步骤 2：定位 `submit`

背景（两段读数必须合起来看，缺一段就会得出错误结论）：

- 首次打通那次：`submit` 被平台以 `app_error (http 501)` 挡下。
- 复跑 `a-05`：147 次 `POST /submit` **全部 200，501 未复现**——但全程经 HTTP/1.1
  中继，**直连路径仍未证**。

所以本步要做的是**无中继直连**复现，并判定成因：

1. 关掉中继，直连端点重跑一次最小提交。
2. 记录脱敏证据：request ID、HTTP 状态、SDK 错误类别、平台权威进度。
3. 比较官方 SDK 的调用形状与平台契约——**区分「我方请求形状错」与「服务端状态」**。
   注意 `bridge/README.md` 记的第 7 条 SDK 缺陷：payload 非标准形状时 `code`/`message`/
   `detail` 全空，body 在 SDK 内部就被丢掉——要拿原始字节只能**绕开 SDK** 直连同端点。
4. 原始 flag 与 token 只进受限私密面（`<ResultDir>/private/<runID>/`）。
5. **结果不确定时先 `Reconcile`**，不得盲目重发。

**若判定为平台故障**：记录最小脱敏证据，**保持发布门未通过**，不要把它写成「已解决」。

相关证据落点：

- 桥的 stderr：`<ResultDir>/private/bridge-stderr.log`（`NewResultStore` 在构造时就建好
  `private/`，所以这一份一定拿得到——此前晚建目录导致过一次「出事时想要的证据正好没了」）。
- 每次提交的判定：`<ResultDir>/private/<runID>/submissions.jsonl`（v0.5 新增）。它直接
  回答「提交了什么、平台怎么判的」，包括**提交结果不确定**那一档。

## 步骤 3：真实 pi 跑通一题的完整生命周期

```bash
go run ./cmd/red-harness run --scenario tsecbench --provider <provider> \
    --targets <code> --submit --budget-rounds <N> --store <dir>
```

出口条件（全部满足才算通过）：

- [ ] `list → prepare → solve → submit → cleanup` 至少一题走完；
- [ ] **平台权威确认**了提交结果（不只是「我们提交了」）；
- [ ] 无残留：按 run label 查容器与网络均为空；
- [ ] 公开结果与私密证据都正确（公开面只有计数与指纹；明文只在 `private/`）。

`--max-submissions`（默认 50）是本题的提交次数护栏。若它被触发，`Reason` 会是
`submit_limit` 且 `submissionsCapped` 会记下还剩多少没提交——**那说明候选集合不正常**，
先去看答案形态判定，而不是直接调高上限。

## 步骤 4：统一文档表述

把 `architecture.md` / `roadmap.md` / `PLAN v0.4.md` 里与最新证据冲突的表述统一，
**之后才标记 v0.4 完成**。特别检查：

- 501 的措辞必须是「未复现（经中继）／直连未证」，不能写成「已修」或「仍 501」。
- M4「至少解出一题」若仍未达成，不得因为 501 未复现就记为通过。
- 本文档与 `roadmap.md` 的出口门表必须一致。

## 记录格式

每条证据留：**命令、环境（`tun0` 状态、镜像 digest、pi 版本）、读数、时间**。
跳过的项单独列出并说明原因——**跳过的集成测试不算通过**（`docs/roadmap.md` 验收与证据）。
