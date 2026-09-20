"""桥的线协议：一行一个 JSON 对象，请求/响应靠 ``id`` 关联。

这个模块只做三件事，都不依赖 SDK 是否可导入：

1. 定义命令名与错误码（与 Go 侧 ``bridge/wire.go`` 逐字对应）；
2. 把 SDK 的异常翻译成**结构化错误**，含分类与可重试性；
3. 把 Python 对象序列化成**一行** JSON。

为什么错误分类放在 Python 侧而不是 Go 侧：只有这里能看到异常对象的原文、
``detail`` 与 ``status_code``。Go 侧若靠消息字符串匹配来判断「该不该重试」，
那就是前身反复踩的坑（``errors.go:8-10`` 明确禁止这么做）。
"""

from __future__ import annotations

import json

# ── 命令名 ──
#
# **固定为这六个**（PLAN.md:43）。前身设计用的是 `op` 字段名，本计划统一为
# `cmd`；Go 侧 bridge/wire.go 的常量必须与这里逐字一致，否则桥回
# unknown_command 而 Go 侧会一直等到 deadline。
CMD_CHECK_VPN = "check_vpn"
CMD_LIST = "list"
CMD_START = "start"
CMD_HINT = "hint"
CMD_SUBMIT = "submit"
CMD_CLOSE = "close"

KNOWN_COMMANDS = frozenset(
    {CMD_CHECK_VPN, CMD_LIST, CMD_START, CMD_HINT, CMD_SUBMIT, CMD_CLOSE}
)

# ── 错误码 ──
#
# 前九个与 SDK ``errors.py`` 的类属性逐字对应（已对源码核对，v0.1.2）。
# 后几个是桥自己造的，每一个都对应一个**具体的失效模式**，不是「兜底」：
#
#   invalid_response     SDK 的两个源码级缺陷——畸形载荷抛的裸 ``KeyError``
#                        与 2xx 非 JSON 抛的 ``json.JSONDecodeError``，
#                        两者都逃出了 SDK 自己的异常网，桥必须给它们一个稳定的码。
#   connection_error     ``TSecConnectionError.code is None``，用 None 做 map 键
#                        会与「没有 code 的通用错误」撞车，所以给个稳定别名。
#   sdk_missing          ``import tsec_benchmark`` 失败（宿主缺 httpx）。
#   protocol_error       stdio 协议被污染（有东西往 stdout 打印）。
#   unknown_command      Go 侧与 bridge.py 版本不一致。
#   missing_credential   缺 BENCHMARK_TOKEN / BENCHMARK_BASE_URL（消息不含值）。
#   deadline_exceeded   请求带的 deadline 已过，桥**不会开始**平台调用。
#   invalid_request      请求行本身不合法（缺 cmd 等）。
CODE_VPN_CHECK_FAILED = "vpn_check_failed"
CODE_TASK_NOT_FOUND = "task_not_found"
CODE_CHALLENGE_NOT_FOUND = "challenge_not_found"
CODE_INVALID_STATE = "invalid_state"
CODE_DUPLICATE = "duplicate"
CODE_RESOURCE_UNAVAILABLE = "resource_unavailable"
CODE_INTERNAL_ERROR = "internal_error"
CODE_VALIDATION_ERROR = "validation_error"
CODE_CONNECTION_ERROR = "connection_error"
CODE_INVALID_RESPONSE = "invalid_response"
CODE_SDK_MISSING = "sdk_missing"
CODE_PROTOCOL_ERROR = "protocol_error"
CODE_UNKNOWN_COMMAND = "unknown_command"
CODE_MISSING_CREDENTIAL = "missing_credential"
CODE_DEADLINE_EXCEEDED = "deadline_exceeded"
CODE_INVALID_REQUEST = "invalid_request"

# 错误「类」。**故意粗于 code**：Go 侧只需要知道该找谁（配置 / 平台 / 取消），
# 精确诊断看 code 与 message。取值与 harness.Kind 刻意同名。
CLASS_PLATFORM = "platform"
CLASS_CONFIG = "config"
CLASS_INTERNAL = "internal"
CLASS_CANCELED = "cancelled"

# 平台错误码 → 可重试性。
#
# 可重试 = 「同样的输入稍后重试可能有不同结果」。逐条给理由：
#   resource_unavailable(503) 靶场实例未就绪/池子空了 ⇒ 稍后重试有意义。
#   internal_error(500)       平台内部故障 ⇒ 稍后重试有意义。
#   connection_error          传输层抖动 ⇒ 稍后重试有意义。
# 其余（404 的两类、422、duplicate）重试无意义：同样的输入会得到同样的结果。
#
# **invalid_state(409) 不在这张表里**：它把两种语义混在一个码里，必须按消息
# 细分（见 ``invalid_state_retryable``），不能一刀切。
RETRYABLE_CODES = frozenset(
    {CODE_RESOURCE_UNAVAILABLE, CODE_INTERNAL_ERROR, CODE_CONNECTION_ERROR}
)

#: ``InvalidState`` 里表示「活跃容器数达上限」的判据（SDK_API.md:209-213）。
#: 撞上限时**释放一个名额再重试**是正确动作；而「任务已结束（超时）」
#: 重试多少次都不会变——把两者当成一回事会让一次超时变成无限重试。
MAX_ACTIVE_MARKER = "max active"


def invalid_state_retryable(message: str) -> bool:
    """``InvalidState`` 是否可重试。判据是消息里的 ``max active``。"""
    return MAX_ACTIVE_MARKER in (message or "")


def error(
    code: str,
    message: str,
    *,
    cls: str = CLASS_PLATFORM,
    retryable: bool = False,
    status_code: int | None = None,
) -> dict:
    """构造线格式的错误对象。字段名与 Go 侧 ``wireError`` 逐字对应。"""
    err = {
        "code": code,
        "class": cls,
        "retryable": bool(retryable),
        "message": _clip(message),
    }
    if status_code is not None:
        err["statusCode"] = int(status_code)
    return err


def _clip(text: str, limit: int = 400) -> str:
    """截断消息。

    上限存在的理由：异常文本可能带平台响应体片段。桥只把它当诊断信息，
    而 Go 侧会把 message 放进**不参与 JSON 序列化**的 Err（见
    ``wireErrorToHarness``），所以这里不需要为了脱敏而丢弃，只需要防止
    一条病态响应把 stdout 撑爆。
    """
    text = "" if text is None else str(text)
    if len(text) <= limit:
        return text
    return text[: limit - 3] + "..."


def encode(obj: dict) -> str:
    """序列化成**一行**（带换行）。``ensure_ascii=True`` 是刻意的：桥与 Go 侧
    之间只走 ASCII，避免两端对编码的假设不一致（平台消息里常有中文）。
    """
    return json.dumps(obj, ensure_ascii=True, separators=(",", ":")) + "\n"


def decode_request(line: str) -> dict:
    """解析一行请求。失败抛 ``ValueError``（调用方包成 invalid_request）。"""
    obj = json.loads(line)
    if not isinstance(obj, dict):
        raise ValueError("request 不是一个 JSON 对象")
    return obj


def ok_response(req_id: str, result) -> dict:
    return {"id": req_id, "ok": True, "result": result}


def err_response(req_id: str, err: dict) -> dict:
    return {"id": req_id, "ok": False, "error": err}


def hello(sdk_version: str, python_version: str) -> dict:
    """启动握手行。

    **为什么需要它**：Go 侧的启动探活必须是「桥命令能跑 + SDK 在桥侧能导入」
    两件事的合成判据。单独跑 ``python3 -c "import tsec_benchmark"`` 在宿主上
    必然失败（宿主 python3.12 没有 httpx），而桥命令可能指向容器——那条探测
    会把「宿主失败但容器可用」这种**正常配置**误报成缺依赖。
    """
    return {
        "event": "hello",
        "sdk": sdk_version,
        "python": python_version,
        "protocol": 1,
    }


def deadline_passed(deadline: str | None) -> bool:
    """请求带的 deadline 是否已过。

    deadline 是 RFC3339 字符串而不是「还剩几秒」：桥侧与 Go 侧可能不在同一台
    机器（本机真跑的唯一路径是 ``docker exec``），时钟偏移下「还剩 12 秒」是
    错的，「不晚于 18:00:00Z」才有意义。

    解析失败**当作未过期**：一个格式不对的 deadline 不该让一次本来能成功的
    调用直接失败——真正兜底的是 Go 侧的计时器。
    """
    if not deadline:
        return False
    from datetime import datetime, timezone

    text = deadline.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        when = datetime.fromisoformat(text)
    except ValueError:
        return False
    if when.tzinfo is None:
        when = when.replace(tzinfo=timezone.utc)
    return when <= datetime.now(timezone.utc)


# ── 异常 → 结构化错误 ──


def classify(exc: BaseException, tsec_mod) -> dict:
    """把异常翻译成结构化错误。

    **顺序是关键**：具体子类必须排在基类 ``TSecError`` 之前，否则
    ``DuplicateSubmit``/``InvalidState`` 会被当成通用平台错误，可重试性也随之
    出错。``KeyError``/``ValueError`` 排在最后，因为它们是**非 SDK 异常**
    ——SDK 的源码级缺陷正是让它们逃出来。
    """
    # 1) 预检失败：message 固定，detail.reason 是 network_error / bad_status /
    #    bad_body / status_not_ok（client.py:115-126）。**不可重试**——VPN 没连
    #    上时重试只是白等。
    if isinstance(exc, tsec_mod.VpnCheckError):
        return error(
            CODE_VPN_CHECK_FAILED,
            _exc_message(exc),
            cls=CLASS_PLATFORM,
            retryable=False,
        )

    # 2) 404 两类：token 无效 / code 不在题目集里。重试无意义。
    if isinstance(exc, tsec_mod.TaskNotFound):
        return error(
            CODE_TASK_NOT_FOUND,
            _exc_message(exc),
            retryable=False,
            status_code=_exc_status(exc),
        )
    if isinstance(exc, tsec_mod.ChallengeNotFound):
        return error(
            CODE_CHALLENGE_NOT_FOUND,
            _exc_message(exc),
            retryable=False,
            status_code=_exc_status(exc),
        )

    # 3) 409 invalid_state：**一个码两种语义**，可重试性按消息细分。
    #    「max active」= 活跃容器数达上限 ⇒ 关掉一个再重试。
    #    「任务已结束」⇒ 停。
    if isinstance(exc, tsec_mod.InvalidState):
        msg = _exc_message(exc)
        return error(
            CODE_INVALID_STATE,
            msg,
            retryable=invalid_state_retryable(msg),
            status_code=_exc_status(exc),
        )

    # 4) 409 duplicate：幂等命中，**不是错误**。正常路径下 submit 处理器会先
    #    截住它并折成成功；走到这里说明调用方没截住，仍然按可重试=false 上报，
    #    绝不因为「重复」就重试（重试只会再拿一个 duplicate）。
    if isinstance(exc, tsec_mod.DuplicateSubmit):
        return error(
            CODE_DUPLICATE,
            _exc_message(exc),
            retryable=False,
            status_code=_exc_status(exc),
        )

    # 5) 503 / 500：稍后重试有意义。
    if isinstance(exc, tsec_mod.ResourceUnavailable):
        return error(
            CODE_RESOURCE_UNAVAILABLE,
            _exc_message(exc),
            retryable=True,
            status_code=_exc_status(exc),
        )
    if isinstance(exc, tsec_mod.InternalError):
        return error(
            CODE_INTERNAL_ERROR,
            _exc_message(exc),
            retryable=True,
            status_code=_exc_status(exc),
        )

    # 6) 422：请求参数不合法，重试无意义（改了参数才可能过）。
    if isinstance(exc, tsec_mod.ValidationError):
        return error(
            CODE_VALIDATION_ERROR,
            _exc_message(exc),
            retryable=False,
            status_code=_exc_status(exc),
        )

    # 7) 传输层错误。**必须排在 TSecError 之前**：它是 TSecError 的子类，而
    #    它的 code 是 None（errors.py:62）——用 None 去查表会得到「没有 code
    #    的通用错误」，把一次网络抖动误报成平台业务错误。
    if isinstance(exc, tsec_mod.TSecConnectionError):
        return error(
            CODE_CONNECTION_ERROR,
            _exc_message(exc),
            retryable=True,
        )

    # 8) 其他 TSecError：用平台给的 code，查不到就用 app_error。
    if isinstance(exc, tsec_mod.TSecError):
        code = getattr(exc, "code", None) or "app_error"
        return error(
            code,
            _exc_message(exc),
            retryable=code in RETRYABLE_CODES,
            status_code=_exc_status(exc),
        )

    # 9) **SDK 缺陷 1**：``from_dict`` 抛的裸 ``KeyError``。
    #    models.py 的每个 from_dict 都用 ``data["field"]`` 取必填字段，缺一个
    #    就抛 KeyError——而 KeyError **不是** TSecError，会穿透 SDK 自己的
    #    异常网（连 ``except TSecError`` 都拦不住）。桥必须把它包成结构化错误，
    #    否则一次平台 schema 漂移会变成桥的进程崩溃。
    if isinstance(exc, KeyError):
        return error(
            CODE_INVALID_RESPONSE,
            f"平台响应缺少必填字段 {_exc_message(exc)}",
            retryable=False,
        )

    # 10) **SDK 缺陷 2**：``_handle_response`` 对 2xx 无保护地调
    #     ``response.json()``（client.py:149-155）。2xx 但非 JSON 的响应抛
    #     ``json.JSONDecodeError``（ValueError 的子类），它**逃出**了
    #     client.py:143-146 的 ``httpx.HTTPError`` 网。
    #     归到 invalid_response 而不是 connection_error：连接是通的，是响应
    #     内容不对——两者的排查方向完全不同（查网络 vs 查平台/中间层）。
    if isinstance(exc, ValueError):
        return error(
            CODE_INVALID_RESPONSE,
            f"平台响应不是合法 JSON: {_exc_message(exc)}",
            retryable=False,
        )

    # 11) 真·未知异常。归 internal 类：调用方能做的动作与平台故障相同
    #     （稍后重试或放弃），但**不谎称**它是平台业务错误。
    return error(
        CODE_INTERNAL_ERROR,
        f"{type(exc).__name__}: {_exc_message(exc)}",
        cls=CLASS_INTERNAL,
        retryable=False,
    )


def _exc_message(exc: BaseException) -> str:
    """取异常的消息。

    ``TSecError.__str__`` 是 ``_format()``（message + code + http），
    ``str(exc)`` 对 SDK 异常已经足够；对非 SDK 异常取 ``str(exc)``。
    空消息回落到类名，避免线格式里出现 ``"message": ""``。
    """
    text = str(exc)
    return text if text else type(exc).__name__


def _exc_status(exc: BaseException) -> int | None:
    """取 HTTP 状态码。**只读属性、不读值内容**：SDK 异常带 ``status_code``，
    非 SDK 异常没有——用 getattr 而不是类型断言，因为 TSecConnectionError
    把它固定成 None。"""
    code = getattr(exc, "status_code", None)
    return int(code) if isinstance(code, int) else None
