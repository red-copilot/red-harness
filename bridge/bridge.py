#!/usr/bin/env python3
"""TSecBench Python 桥：JSONL over stdio，一行一个请求/响应。

Go 侧（``bridge/client.go``）起这个进程并常驻复用：整个跑分期间只有一个 Python
进程、一个 httpx 连接池、一次 VPN 预检。进程死掉由 Go 侧按退避重启并**重做
预检**。

    → {"id":"1","cmd":"list","deadline":"2026-09-20T18:00:00Z"}
    ← {"id":"1","ok":true,"result":{...}}
    ← {"id":"1","ok":false,"error":{"code":"...","class":"...","retryable":false,...}}

启动时先打一行握手：

    ← {"event":"hello","sdk":"0.1.2","python":"3.14.0","protocol":1}

**这一行是启动探活的判据**。Go 侧不单独跑 ``python3 -c "import tsec_benchmark"``
——宿主 python3.12 没有 httpx，那个探测必然失败，而桥命令可能指向容器（本机
真跑的唯一路径）。用握手行探活，天然覆盖两种配置，不会把「宿主失败但容器可用」
这种正常配置误报成缺依赖。

**绝不调用 ``tsec_benchmark._cli.main``**：它往 stdout 打印表格
（``_cli.py:57-67``），会污染 JSONL 协议。本文件另有一道防线：把 ``sys.stdout``
换成 ``sys.stderr``（见 ``main``），任何误打印都进不了协议通道。
"""

from __future__ import annotations

import atexit
import json
import os
import signal
import sys
import traceback

# 让 `python3 bridge/bridge.py` 也能 import 同目录的 protocol 模块——
# 否则从仓库根直接跑会因为 sys.path[0] 是 bridge/ 而成功、从别处跑会失败。
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import protocol  # noqa: E402

# 真 stdout 必须在换掉之前抓住。之后协议只走这个句柄。
_PROTOCOL_OUT = sys.stdout


def _log(msg: str) -> None:
    """诊断输出走 stderr。

    **stderr 里不得出现凭据**：Go 侧把 stderr 落盘到 ``private/``（0700/0600），
    但 private/ 也只是「不公开」，不是「可以放 token」。所以这里只打结构性
    信息（命令名、错误码、异常类型），不打请求体。
    """
    print(msg, file=sys.stderr, flush=True)


class Bridge:
    """一个常驻的 SDK 会话。

    它**不用上下文管理器**：``TSecBenchmark.__enter__`` 会在入口隐式做 VPN
    预检（``sync.py:83-86``），那会让「预检失败」和「构造失败」变成同一个
    异常，无法区分。这里用 ``auto_check_vpn=False`` 构造裸 client，预检由
    ``check_vpn`` 命令显式触发（Go 侧 ``CheckVPN``）。
    """

    def __init__(self, tsec, base_url: str, token: str) -> None:
        self._tsec = tsec
        # timeout 沿用 SDK 默认 30s：Go 侧的 deadline 是**更早**的那道闸门
        # （它按 ctx 与客户端默认超时取更早者），所以这里不需要更短。
        self._client = tsec.TSecBenchmark(
            base_url=base_url, token=token, auto_check_vpn=False
        )
        self._closed = False

    def close(self) -> None:
        """关掉 client。

        **SDK 缺陷 4**：``sync.py:105-106`` 的 ``close()`` 吞掉全部异常。
        吞掉的后果是「连接池没关干净却以为关掉了」——桥把它记进 stderr，
        让至少有一次可查的痕迹；同时自己保证 ``_closed`` 幂等，避免重复关。

        **SDK 缺陷 3**：SDK 没有 atexit 清理，忘了 close 就留一个 daemon 线程
        + 一个 httpx 连接池。桥在 ``main`` 里注册了 atexit，并且把 close 放在
        正常退出路径上（EOF / 信号）。
        """
        if self._closed:
            return
        self._closed = True
        try:
            self._client.close()
        except Exception as exc:  # noqa: BLE001 - 这里刻意兜底，但必须留痕
            _log(f"bridge: close() 抛异常（SDK 吞异常之外的情况）: {type(exc).__name__}")

    # ── 六个命令 ──

    def check_vpn(self, req: dict):
        res = self._client.check_vpn()
        return {
            "ok": bool(getattr(res, "ok", False)),
            "status": _s(getattr(res, "status", "")),
            "client_ip": _s(getattr(res, "client_ip", "")),
            "time": _s(getattr(res, "time", "")),
        }

    def list(self, req: dict):
        challenges = self._client.list_challenges()
        return [_challenge(c) for c in challenges]

    def start(self, req: dict):
        code = req["code"]
        res = self._client.start_challenge(code)
        return {
            "unique_code": _s(getattr(res, "unique_code", "")) or code,
            # **仅当 container_status == "available" 时非空**。起题是异步的，
            # pending 期间这里是空数组——那不是错误，调用方要轮询。
            "container_addr": _addrs(getattr(res, "container_addr", None)),
        }

    def hint(self, req: dict):
        code = req["code"]
        res = self._client.get_hint(code)
        # **hint 可以是 None**（平台没给提示）。回 null 而不是空串：空串会被
        # 调用方当成「平台给了一个空提示」，而 null 明确表示「没有」。
        hint = getattr(res, "hint", None)
        return {
            "unique_code": _s(getattr(res, "unique_code", "")) or code,
            "hint": None if hint is None else _s(hint),
        }

    def submit(self, req: dict):
        code = req["code"]
        flag = req["flag"]
        try:
            res = self._client.submit_flag(code, flag)
        except self._tsec.DuplicateSubmit:
            # **幂等命中，不是错误**：平台已经把这个 flag 计入了。映射成
            # ok:true + duplicate:true，让 Go 侧的 SubmitResult.Duplicate 为真、
            # err 为 nil（ports.go:57：它等价于已确认）。
            #
            # 注意这里**不重试**：重试只会再拿一个 duplicate。
            return {
                "unique_code": code,
                "correct": True,
                "duplicate": True,
                "awarded": 0,
                "cumulative_score": 0,
                "correct_flag_count": 0,
                "total_flag_count": 0,
                "matched_flag_index": None,
            }
        # **SubmitResult 没有 unique_code 字段**（models.py:75，已对源码核对），
        # 所以这里用请求里的 code 回填——Go 侧的 Submit 需要它来归属判定。
        idx = getattr(res, "matched_flag_index", None)
        return {
            "unique_code": code,
            "correct": bool(getattr(res, "correct", False)),
            "duplicate": False,
            "awarded": _i(getattr(res, "awarded", 0)),
            "cumulative_score": _i(getattr(res, "cumulative_score", 0)),
            "correct_flag_count": _i(getattr(res, "correct_flag_count", 0)),
            "total_flag_count": _i(getattr(res, "total_flag_count", 0)),
            "matched_flag_index": None if idx is None else _i(idx),
        }

    def close_challenge(self, req: dict):
        code = req["code"]
        res = self._client.close_challenge(code)
        return {
            "unique_code": _s(getattr(res, "unique_code", "")) or code,
            "closed": bool(getattr(res, "closed", True)),
        }

    # ── 分发表 ──
    #
    # **命令名固定为这六个**（PLAN.md:43）。表是显式的而不是 getattr 拼名字：
    # 拼名字意味着一个请求可以调到 Bridge 的任意方法（包括 close），那是一
    # 条注入面。
    def dispatch(self, cmd: str, req: dict):
        if cmd == protocol.CMD_CHECK_VPN:
            return self.check_vpn(req)
        if cmd == protocol.CMD_LIST:
            return self.list(req)
        if cmd == protocol.CMD_START:
            return self.start(req)
        if cmd == protocol.CMD_HINT:
            return self.hint(req)
        if cmd == protocol.CMD_SUBMIT:
            return self.submit(req)
        if cmd == protocol.CMD_CLOSE:
            return self.close_challenge(req)
        raise UnknownCommand(cmd)


class UnknownCommand(Exception):
    """命令名不在表里。**通常是 Go 侧与 bridge.py 版本不一致**，所以单独一类。"""


def _s(v) -> str:
    return "" if v is None else str(v)


def _i(v) -> int:
    try:
        return int(v)
    except (TypeError, ValueError):
        return 0


def _addrs(v) -> list:
    """容器地址列表。None 与空都归成 ``[]``——Go 侧的 ``Addrs`` 是
    ``omitempty`` 的切片，null 会让它变成 nil 而不是空切片，两者在
    ``len()`` 上一样但 JSON 里不同，统一成空列表更可预测。"""
    if not v:
        return []
    return [str(x) for x in v]


def _challenge(ch) -> dict:
    """把 SDK 的 ``Challenge`` 数据类映射成线格式。

    **逐字段显式写**，不用 ``dataclasses.asdict``：显式列表就是契约本身，
    SDK 将来加字段时不会静默漏进协议。字段名照抄 ``models.py:13``，不改名
    ——桥的职责是转发，改名会制造第二个命名口径。

    ``level`` 与 ``total_score`` 也照发：Go 侧的 ``harness.Challenge`` 目前
    没有对应字段（加字段会改 ``dag/store.go`` schema 1 的持久化键），但线格式
    保留它们，将来要用时不必再动协议。
    """
    return {
        "unique_code": _s(getattr(ch, "unique_code", "")),
        "difficulty": _s(getattr(ch, "difficulty", "")),
        "level": _i(getattr(ch, "level", 0)),
        "total_score": _i(getattr(ch, "total_score", 0)),
        "flag_count": _i(getattr(ch, "flag_count", 0)),
        "correct_flag_count": _i(getattr(ch, "correct_flag_count", 0)),
        "is_completed": bool(getattr(ch, "is_completed", False)),
        "description": _s(getattr(ch, "description", "")),
        "container_status": _s(getattr(ch, "container_status", "")),
        "container_addr": _addrs(getattr(ch, "container_addr", None)),
    }


def _import_sdk():
    """导入 SDK。失败时给出**可操作**的错误，而不是等第一次调用炸。

    返回 ``(module, err_dict)``。宿主 python3.12 没有 httpx，所以本机唯一
    可行的路径是把桥命令指向容器（``REDCOPILOT_BRIDGE_CMD``）——这条真实
    路径必须写进错误消息里，否则用户看到的只是 ``ModuleNotFoundError``。

    ``TSEC_MOCK=1`` 时改用 ``mock_sdk``（离线测试）。**mock 只在显式设了这个
    环境变量时生效**：任何别的拼写（拼错、大小写、空串）都会走真实导入路径，
    不会让一次测试配置错误在生产里静默变成「用一个假平台跑分」。
    """
    if os.environ.get("TSEC_MOCK") == "1":
        try:
            import mock_sdk  # noqa: PLC0415 - 只在测试路径导入

            sys.modules.setdefault("tsec_benchmark", mock_sdk)
            _log("bridge: TSEC_MOCK=1，使用 testdata/mock_sdk 作为 tsec_benchmark")
            return mock_sdk, None
        except Exception as exc:  # noqa: BLE001
            detail = f"{type(exc).__name__}: {exc}"
            _log("bridge: TSEC_MOCK=1 但无法导入 mock_sdk（PYTHONPATH 要含 bridge/testdata）")
            _log(detail)
            return None, protocol.error(
                protocol.CODE_SDK_MISSING,
                "TSEC_MOCK=1 但无法导入 mock_sdk。请设 PYTHONPATH=bridge/testdata。"
                f"原始错误: {detail}",
                cls=protocol.CLASS_CONFIG,
                retryable=False,
            )

    try:
        import tsec_benchmark  # noqa: PLC0415 - 刻意延迟导入

        return tsec_benchmark, None
    except Exception as exc:  # noqa: BLE001 - 缺依赖可能是任意 ImportError
        detail = f"{type(exc).__name__}: {exc}"
        _log("bridge: 无法导入 tsec_benchmark")
        _log(detail)
        return None, protocol.error(
            protocol.CODE_SDK_MISSING,
            "无法导入 tsec_benchmark。本机宿主 python3 没有 httpx，"
            "请把桥命令指向带 SDK 的容器："
            "REDCOPILOT_BRIDGE_CMD='docker exec <容器> python3 -m bridge'。"
            f"原始错误: {detail}",
            cls=protocol.CLASS_CONFIG,
            retryable=False,
        )


def _handle_line(bridge, tsec_mod, line: str) -> str:
    """处理一行请求，返回一行响应（含换行）。"""
    try:
        req = protocol.decode_request(line)
    except ValueError as exc:
        # 请求行本身不合法。回 id=null：Go 侧按 id 关联，未知 id 会被丢弃，
        # 但至少协议上是合法的一行。
        return protocol.encode(
            protocol.err_response(
                None,
                protocol.error(
                    protocol.CODE_INVALID_REQUEST,
                    f"请求行不是合法 JSON: {exc}",
                    cls=protocol.CLASS_CONFIG,
                ),
            )
        )

    req_id = req.get("id")
    cmd = req.get("cmd") or ""

    if cmd not in protocol.KNOWN_COMMANDS:
        return protocol.encode(
            protocol.err_response(
                req_id,
                protocol.error(
                    protocol.CODE_UNKNOWN_COMMAND,
                    f"未知命令 {cmd!r}（Go 侧与 bridge.py 版本不一致）",
                    cls=protocol.CLASS_CONFIG,
                ),
            )
        )

    if bridge is None:
        # 不可达（缺依赖时 main 已经退出）。留这道守卫是因为**静默 None** 的
        # 后果很坏：NoneType has no attribute 'dispatch' 会被 classify 归到
        # internal，把一次配置错误报成平台故障。
        return protocol.encode(
            protocol.err_response(
                req_id,
                protocol.error(
                    protocol.CODE_SDK_MISSING,
                    "桥没有可用的 SDK client（进程应以 sdk_missing 退出）",
                    cls=protocol.CLASS_CONFIG,
                ),
            )
        )

    # deadline 已过 ⇒ **不开始平台调用**。检查放在这里（分发之前）而不是每个
    # 处理器里：一处判定覆盖六个命令，漏一个的可能性为零。
    if protocol.deadline_passed(req.get("deadline")):
        return protocol.encode(
            protocol.err_response(
                req_id,
                protocol.error(
                    protocol.CODE_DEADLINE_EXCEEDED,
                    "请求 deadline 已过，未发起平台调用",
                    retryable=True,
                ),
            )
        )

    # code / flag 缺失是 Go 侧的编码错误，不是平台错误。显式挡一道，否则
    # KeyError 会走 classify 变成 invalid_response，把「我们发错了」误报成
    # 「平台响应不对」。
    if cmd in (protocol.CMD_START, protocol.CMD_HINT, protocol.CMD_CLOSE) and not req.get("code"):
        return protocol.encode(
            protocol.err_response(
                req_id,
                protocol.error(
                    protocol.CODE_INVALID_REQUEST,
                    f"{cmd} 缺少 code",
                    cls=protocol.CLASS_CONFIG,
                ),
            )
        )
    if cmd == protocol.CMD_SUBMIT and (not req.get("code") or req.get("flag") is None):
        return protocol.encode(
            protocol.err_response(
                req_id,
                protocol.error(
                    protocol.CODE_INVALID_REQUEST,
                    "submit 缺少 code 或 flag",
                    cls=protocol.CLASS_CONFIG,
                ),
            )
        )

    try:
        result = bridge.dispatch(cmd, req)
        return protocol.encode(protocol.ok_response(req_id, result))
    except UnknownCommand as exc:
        return protocol.encode(
            protocol.err_response(
                req_id,
                protocol.error(
                    protocol.CODE_UNKNOWN_COMMAND, f"未知命令 {exc}", cls=protocol.CLASS_CONFIG
                ),
            )
        )
    except Exception as exc:  # noqa: BLE001 - 这里是**唯一的**异常收敛点
        # 收敛点必须宽：SDK 的源码级缺陷正是让**非 SDK 异常**逃出来
        # （裸 KeyError、json.JSONDecodeError）。窄的 except 会让它们穿透到
        # 进程顶部，表现为「桥崩了」——一次平台 schema 漂移变成一次崩溃重启。
        err = protocol.classify(exc, tsec_mod)
        # traceback 只进 stderr（Go 侧落 private/），不进协议。
        _log(f"bridge: {cmd} 失败 code={err['code']} {type(exc).__name__}")
        _log(traceback.format_exc())
        return protocol.encode(protocol.err_response(req_id, err))


def main() -> int:
    # **防线：把 sys.stdout 换成 stderr。**
    #
    # 协议通道必须只有一处写者。SDK 与它依赖的 httpx 理论上不打印，但
    # ``tsec_benchmark._cli.main``（_cli.py:57-67）会往 stdout 打表格——一旦
    # 有人误调它（或将来 SDK 某处在 import 期打印），stdout 就被污染，
    # 表现为「桥卡住不回复」。换掉 sys.stdout 之后，任何误打印都进 stderr，
    # 协议通道物理上不可污染。
    sys.stdout = sys.stderr

    tsec_mod, import_err = _import_sdk()

    if import_err is not None:
        # **打不出握手行，直接退出。**
        #
        # 为什么不是「进降级循环、每个请求回 sdk_missing」：那会让 Go 侧的启动
        # 探活看不出问题——它等的是握手行，收到第一行就认为启动成功，然后
        # 第一次 `list` 才炸。缺依赖必须在 NewClient 阶段就报出来。
        # 这里先往协议通道写一行带 id=null 的错误行，让 Go 侧能把
        # 「可操作的原因」拼进启动错误，然后以非 0 退出。
        #
        # 顺带一个副作用（刻意的）：**握手行只在 SDK 可用时才出现**。所以
        # 「`echo '{"id":"1","cmd":"check_vpn"}' | python3 bridge/bridge.py`
        # 只回一行合法 JSON」在缺 SDK 的机器上成立——那一行就是这条可操作的
        # sdk_missing 错误，而不是「握手行 + 错误行」两行。
        _PROTOCOL_OUT.write(protocol.encode(protocol.err_response(None, import_err)))
        _PROTOCOL_OUT.flush()
        return 4

    version = _s(getattr(tsec_mod, "__version__", ""))
    py = f"{sys.version_info.major}.{sys.version_info.minor}.{sys.version_info.micro}"
    _PROTOCOL_OUT.write(protocol.encode(protocol.hello(version, py)))
    _PROTOCOL_OUT.flush()

    base_url = os.environ.get("BENCHMARK_BASE_URL", "")
    token = os.environ.get("BENCHMARK_TOKEN", "")
    if not base_url or not token:
        # **消息里不带值**。凭据缺失要在构造前报清楚，否则 SDK 会拿着空 token
        # 去请求，平台回 404 task_not_found——那会把「我们没配」误报成
        # 「token 无效」，排查方向完全错。
        missing = [
            name
            for name, val in (
                ("BENCHMARK_BASE_URL", base_url),
                ("BENCHMARK_TOKEN", token),
            )
            if not val
        ]
        _log(f"bridge: 缺少凭据 {','.join(missing)}")
        _PROTOCOL_OUT.write(
            protocol.encode(
                protocol.err_response(
                    None,
                    protocol.error(
                        protocol.CODE_MISSING_CREDENTIAL,
                        f"缺少环境变量 {','.join(missing)}（值不会被打印）",
                        cls=protocol.CLASS_CONFIG,
                    ),
                )
            )
        )
        _PROTOCOL_OUT.flush()
        return 2

    try:
        bridge = Bridge(tsec_mod, base_url, token)
    except Exception as exc:  # noqa: BLE001 - 构造失败必须报清楚
        _log(f"bridge: 构造 SDK client 失败 {type(exc).__name__}: {exc}")
        return 3

    # **SDK 缺陷 3 的对策**：SDK 没有 atexit 清理，忘了 close 就留下一个
    # daemon 线程 + 一个 httpx 连接池。桥注册 atexit 并接住 SIGTERM/SIGINT，
    # 保证正常退出与被杀两条路径都关一次。
    atexit.register(bridge.close)

    def _on_signal(signum, _frame):
        _log(f"bridge: 收到信号 {signum}，关闭 client 后退出")
        bridge.close()
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, _on_signal)
    signal.signal(signal.SIGINT, _on_signal)

    # 主循环：一行进一行出。**读一行、处理一行、立刻 flush**——批量缓冲会让
    # Go 侧的 deadline 计时器在「响应已生成但没发出去」时超时。
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        out = _handle_line(bridge, tsec_mod, line)
        try:
            _PROTOCOL_OUT.write(out)
            _PROTOCOL_OUT.flush()
        except BrokenPipeError:
            # Go 侧先死了。不是错误，安静退出。
            break

    bridge.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
