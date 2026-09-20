# bridge —— TSecBench Python 桥

Go 侧监管一个**常驻** Python 子进程（`bridge.py`），用 JSONL over stdio 把六个
平台操作送进去、把结构化结果收回来。

```
harness.Platform / harness.HealthChecker
        ↑
   bridge.Client        Go 侧：进程监管、request ID 关联、deadline、错误分类
        ↓  JSONL/stdio
   bridge.py            Python 侧：SDK 调用、异常收敛、字段转发
        ↓
   tsec_benchmark      官方 SDK v0.1.2（唯一契约真源）
```

**为什么是子进程而不是纯 Go HTTP 客户端**：官方 SDK 的数据类字段、异常分类、
VPN 预检语义与鉴权头拼写是唯一真源，而 SDK 只有 Python 实现。Go 侧重写一遍
HTTP 调用就是第二份契约实现——两份实现之间的漂移无人发现（前身
`dag/store.go` 的 `FlagFingerprint` 就是这么从 `answer.Fingerprint` 分叉出来的，
而且已经漂移过一次）。

## 文件

| 文件 | 职责 |
|---|---|
| `bridge.py` | 主循环、六个命令的分发、异常收敛（**唯一**的 `except` 收敛点） |
| `protocol.py` | 线协议常量、错误分类与可重试性、一行 JSON 编解码 |
| `wire.go` | Go 侧线格式结构体、命令名与错误码常量、`invalid_state` 判据 |
| `proc.go` | 子进程监管：reader/writer、pending 表、stderr 落 private/、kill |
| `client.go` | 端口适配（`Platform` + `HealthChecker`）、重启策略、doctor 探针 |
| `testdata/mock_sdk.py` | 离线用的假 `tsec_benchmark`（含两个 SDK 缺陷的原样复现） |
| `client_test.go` | 六个命令的字段映射、错误分类、SDK 缺陷包装、deadline |
| `proc_test.go` | 常驻复用、ID 关联、崩溃重启、重做预检、stderr 落点 |
| `probe_test.go` | 桥命令可覆盖、doctor 的两级 SDK 可达性结论 |

## Wire 协议

一行一个 JSON 对象。命令名固定为 `check_vpn/list/start/hint/submit/close`
（PLAN.md:43）。

```
→ {"id":"1","cmd":"list","deadline":"2026-09-20T18:00:00Z"}
← {"id":"1","ok":true,"result":{...}}
← {"id":"1","ok":false,"error":{"code":"vpn_check_failed","class":"platform","retryable":false,"message":"...","statusCode":503}}
```

启动时桥先打一行握手（**没有 `id`**）：

```
← {"event":"hello","sdk":"0.1.2","python":"3.14.0","protocol":1}
```

`deadline` 是 RFC3339 **绝对时刻**而不是「还剩几秒」：桥与 Go 侧可能不在同一台
机器（本机真跑走 `docker exec`），时钟偏移下只有绝对时刻有意义。桥收到已过期的
deadline 时**不开始平台调用**，直接回 `deadline_exceeded`（可重试）；真正兜底的
是 Go 侧的计时器。

### 错误码与可重试性

| `code` | 来源 | 可重试 |
|---|---|---|
| `vpn_check_failed` | `VpnCheckError`（`code == "vpn_check_failed"`） | 否 |
| `task_not_found` | `TaskNotFound`（404，token 无效） | 否 |
| `challenge_not_found` | `ChallengeNotFound`（404，code 不在题目集） | 否 |
| `invalid_state` | `InvalidState`（409） | **按消息细分**（见下） |
| `duplicate` | `DuplicateSubmit`（409，幂等命中） | 否 |
| `resource_unavailable` | `ResourceUnavailable`（503） | 是 |
| `internal_error` | `InternalError`（500） | 是 |
| `validation_error` | `ValidationError`（422） | 否 |
| `connection_error` | `TSecConnectionError`（**`code is None`**） | 是 |
| `invalid_response` | 桥自造：SDK 的两个源码级缺陷 | 否 |
| `sdk_missing` / `missing_credential` / `protocol_error` / `unknown_command` / `invalid_request` | 桥自造（配置类） | 否 |
| `deadline_exceeded` | 桥自造 | 是 |
| `subprocess_exit` | 桥自造（进程退出） | — |

`invalid_state` 的两种语义按消息细分（SDK_API.md:209-213）：

* 含 `max active` ⇒ **可重试**（活跃容器数达上限，关掉一个再试）；
* 其余（任务已结束/超时）⇒ **不可重试**。

判定放在 **Python 侧**（那里能看到异常原文与 `detail`）；Go 侧只消费 `retryable`
字段，另留一份同样的规则用于兜底与自测——不两处独立判定，避免第二个口径。

## SDK 源码级缺陷（v0.1.2）与桥的对策

对着 `/usr/local/lib/python3.14/dist-packages/tsec_benchmark/` 逐条核对。

| # | 缺陷 | 源码位置 | 桥的对策 |
|---|---|---|---|
| 1 | `from_dict` 抛**裸 `KeyError`**（不是 `TSecError`），畸形载荷穿透所有异常处理 | `models.py` 各 `from_dict` 用 `data["field"]` | `protocol.classify` 把 `KeyError` 包成 `invalid_response` |
| 2 | `_handle_response` 对 2xx **无保护地**调 `response.json()`；2xx 非 JSON 抛的 `json.JSONDecodeError` **逃出** `httpx.HTTPError` 网 | `client.py:149-155` | `protocol.classify` 把 `ValueError` 包成 `invalid_response` |
| 3 | 无 `atexit` 清理，忘了 `close()` 就留一个 daemon 线程 + 一个 httpx 连接池 | `sync.py` | 桥注册 `atexit` + 接 `SIGTERM`/`SIGINT` |
| 4 | `close()` **吞掉全部异常** | `sync.py:105-106` | 桥自己保证幂等，并把异常记进 stderr |
| 5 | **绝不要调 `tsec_benchmark._cli.main`**——它往 stdout 打印，会污染 JSONL 协议 | `_cli.py:57-67` | 桥把 `sys.stdout` 换成 `sys.stderr`，协议通道物理上不可污染 |
| 6 | 默认 timeout 30s、VPN 探测 10s、**无重试逻辑** | `client.py:38,46` | Go 侧 deadline + 崩溃重启一次；平台级重试交给引擎按 `Reconcile` 决定 |

## 已核实的 SDK 契约（v0.1.2，对源码）

| 项 | 核实结果 |
|---|---|
| 异步类 | `client.py:49` `TSecBenchmarkAsync.__init__(base_url, token, *, timeout=30.0, transport=None, auto_check_vpn=True)` |
| 同步封装 | `sync.py:35` `TSecBenchmark.__init__(base_url, token, *, timeout=30.0, auto_check_vpn=True)` |
| 能否不经预检构造 | **能。** 预检**只在** `__enter__`/`__aenter__` 里（`sync.py:83-86`、`client.py:84-87`）。`auto_check_vpn=False` 构造出的裸 client 零网络 I/O；同步类直到首次 `_run` 才起线程/loop（`sync.py:66-74`），构造是惰性的 |
| 六个方法 | `list_challenges()` / `start_challenge(unique_code)` / `get_hint(unique_code)` / `submit_flag(unique_code, flag)` / `close_challenge(unique_code)` / `check_vpn()`；另有 `close()`(sync) / `aclose()`(async) |
| 数据类字段 | 与文档声明零出入（`models.py`，全部 `@dataclass(frozen=True)`）。**`SubmitResult` 没有 `unique_code` 字段** |
| 异常 | 基类 `TSecError(Exception)`，类属性 `code` 默认 `"app_error"`；实例带 `.code`/`.message`/`.detail`(dict)/`.status_code`。**`VpnCheckError.code == "vpn_check_failed"`；`TSecConnectionError.code is None`** |
| 鉴权头 | 字面量 `BENCHMARK_TOKEN: <token>`（`client.py:40,76`）——**不是** `Authorization` |
| 端点 | `GET /openapi/v1/challenges`；`POST .../start?unique_code=`；`GET .../hint?unique_code=`；`POST .../submit`（JSON body `{unique_code, flag}`）；`POST .../close?unique_code=`。URL 是朴素字符串拼接（`client.py:140`），非 urljoin |
| VPN 预检 | `GET http://10.0.100.58`，timeout 10.0，期望 HTTP 200 + JSON 且 `status == "ok"`；`detail["reason"]` ∈ `network_error`/`bad_status`/`bad_body`/`status_not_ok`（`client.py:115-126`） |
| 服务端对账 | `/tmp/tsec/TsecBench-main/tsecbench/api.py:112-162` 注册同样五条路由，`Header(alias="BENCHMARK_TOKEN")`，响应模型与数据类逐字段一致 ⇒ 文档与源码一致 |

## 运行与配置

```bash
# 默认：python3 <repo>/bridge/bridge.py
export BENCHMARK_BASE_URL=... BENCHMARK_TOKEN=...   # 值不会被打印

# 本机（宿主 python3.12 没有 httpx，SDK 装在容器层）：
export REDCOPILOT_BRIDGE_CMD='docker exec <容器> python3 -m bridge'
```

`REDCOPILOT_BRIDGE_CMD` 按空白切分（`strings.Fields`），**刻意不做 shell 解析**
——桥命令来自环境变量，shell 解析会让一个配置错误变成一条命令注入面。

### 启动探活与 doctor

`NewClient` 等的是**握手行**，而不是单独跑 `python3 -c "import tsec_benchmark"`。
理由：后者在宿主上必然失败（无 httpx），而桥命令可能指向容器——那条探测会把
「宿主失败但容器可用」这种**正常配置**误报成缺依赖。握手行经由**实际要用的那条
命令**回来，天然覆盖两种配置。

`bridge.Probe(ctx, cfg)` 返回 doctor 需要的两级结论：

```go
type ProbeResult struct {
    HostImportOK   bool   // 宿主 python3 -c "import tsec_benchmark"
    HostImportErr  string
    BridgeOK       bool   // 桥命令起来了并回了握手行
    SDKVersion     string
    PythonVersion  string
    Detail         string // 含「宿主失败但容器可用」这种组合结论
}
```

「宿主失败 + 桥可用」⇒ `BridgeOK: true`，`Detail` 里说明宿主导入失败是**正常
配置**。doctor 应据此报通过，而不是笼统报「缺 SDK」。

## 桥特有的风险与对策

* **长驻进程**：整个跑分期间一个 Python 进程（复用 httpx client，VPN 预检只做
  一次）。死亡则按退避重启（首次立刻，之后 250ms 起指数到 5s 上限）并**重做
  预检**——新进程没有任何「VPN 通」的证据。用户还没调过 `CheckVPN` 时不替它注入
  预检（否则「预检失败」与「重启失败」会再次混成一团）。
* **崩溃重发**：桥进程死掉时重启一次并重发该请求。重发是安全的，因为四个读命令
  幂等，两个写命令在平台侧都有幂等语义（重复 `start` 撞 `invalid_state`、重复
  `submit` 回 `duplicate`、重复 `close` 幂等）。**超时与取消不重发**——那时请求
  可能已到达平台，重发才是危险动作；由引擎侧 `Reconcile` 对账处理。
* **并发串行化**：`Client.mu` 把「发现死亡 → 重启 → 重发」整条串行化。否则两个
  并发调用各自起一个新进程，就有两个桥连着同一个 token，平台侧会看到并发的
  `start`/`submit`。
* **`Close` 与 `Shutdown` 是两件事**：`Close(ctx, code)` 是**平台操作**（关题目
  容器，来自 `harness.Platform` 的冻结方法集）；终止桥子进程是 `Shutdown()`。
* **Scanner 上限**：Go 的 `bufio.Scanner` 默认 64KiB；`list` 在题目多时会超，
  那会让 Scanner 报 `ErrTooLong` 并**静默停止读行**（表现为「桥卡住不回复」）。
  抬到 16MiB 并显式区分 `ErrTooLong`（协议错）与 EOF（进程退出）。

## 安全

* token、base URL 与 flag **不进入普通日志**。凭据只经子进程环境变量传递，
  **不进命令行**（命令行会出现在 `ps` 里）。
* `proc.go` 把子进程 stderr 重定向到 `private/bridge-stderr.log`（`0600`，
  目录 `0700`），**绝不继承宿主 stderr**——Python traceback 里可能有 base_url
  与请求 URL。
* 桥的错误消息会进 JSON（看板、报告、日志），所以 `harness.Error.Msg` 是**合成的
  安全短句**（code + 一句「下一步做什么」），平台原文放进不参与序列化的 `Err`。
* 测试里的 token 是假值（`test-token-not-real`）。**不要**读
  `/tmp/tsec/TsecBench-main/.agent.env` 的值，也不要把任何真实 token 写进夹具、
  测试、日志或提交。

## 离线测试

全部测试用 mock SDK 驱动**真实的** `bridge/bridge.py`（不 mock Python 侧）——
桥的价值全在「Python 侧怎么把 SDK 的缺陷与契约翻成结构化结果」这一层，把它
mock 掉就等于把被测对象换成了测试自己。

```bash
go test ./bridge/... -count=1
```

mock 的剧本由环境变量控制（见 `testdata/mock_sdk.py` 顶部）：`TSEC_MOCK_SCENARIO`
选择场景，`TSEC_MOCK_LOG` 记录每个调用（用于断言「重启后重做了预检」），
`TSEC_MOCK_SELF_KILL` / `TSEC_MOCK_KILL_COUNT` 模拟桥自杀。

手工冒烟：

```bash
echo '{"id":"1","cmd":"check_vpn"}' | PYTHONPATH=bridge/testdata python3 bridge/bridge.py
```

**必须恰好回一行合法 JSON。** 缺 SDK 的机器上那一行就是带可操作原因的
`sdk_missing` 错误行（握手行只在 SDK 可用时出现——这是刻意的：它让这条冒烟命令
在两种机器上都是「一行」）。
