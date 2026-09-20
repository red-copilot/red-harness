"""离线测试用的假 ``tsec_benchmark`` 模块。

**它的唯一目的是让 bridge 的全部契约可以在没有 VPN、没有真实 SDK 的机器上
被测。** 所以它与真 SDK 的关系是「逐字段对齐」，而不是「大致像」：

* 数据类字段、顺序、默认值照抄 ``models.py``（v0.1.2）；
* 异常层级照抄 ``errors.py``，含 ``TSecConnectionError.code is None``、
  ``VpnCheckError.code == "vpn_check_failed"``；
* **两个源码级缺陷也照抄**——畸形载荷抛裸 ``KeyError``、2xx 非 JSON 抛
  ``json.JSONDecodeError``。把它们「修好」就等于删掉了桥必须包住的那两个
  失效模式，测试会变成永远绿的假测试。

驱动方式：Go 测试用 ``TSEC_MOCK=1`` + ``PYTHONPATH=bridge/testdata`` 起
``bridge/bridge.py``。剧本由环境变量控制：

===========================  ==================================================
``TSEC_MOCK_SCENARIO``       场景名（见 ``SCENARIOS``），默认 ``ok``
``TSEC_MOCK_VPN_FAIL``       非空 ⇒ ``check_vpn`` 抛 ``VpnCheckError``
``TSEC_MOCK_LOG``            把每个调用的命令名追加写进这个文件（Go 测试
                             用它断言「重启后重做了预检」）
``TSEC_MOCK_SELF_KILL``      非空 ⇒ 收到该命令时 ``os._exit(1)``（模拟桥崩溃）
``TSEC_MOCK_KILL_COUNT``     前 N 次调用自杀，之后正常（测「重启一次后成功」）
===========================  ==================================================
"""

from __future__ import annotations

import json
import os
import sys
from dataclasses import dataclass, field
from typing import List, Optional

__version__ = "0.1.2"

# ── 异常（照抄 errors.py 的层级与类属性） ──


class TSecError(Exception):
    """基类。``code`` 默认 ``"app_error"``。"""

    code: Optional[str] = "app_error"

    def __init__(
        self,
        message: str,
        *,
        code: Optional[str] = None,
        detail: Optional[dict] = None,
        status_code: Optional[int] = None,
    ) -> None:
        self.code = code if code is not None else self.code
        self.message = message
        self.detail = detail or {}
        self.status_code = status_code
        super().__init__(self._format())

    def _format(self) -> str:
        parts = [self.message]
        if self.code:
            parts.append(f"[code={self.code}]")
        if self.status_code is not None:
            parts.append(f"[http={self.status_code}]")
        return " ".join(parts)


class TSecConnectionError(TSecError):
    """传输层错误。**code 是 None**（errors.py:62）——这条必须照抄，否则桥的
    分类顺序（connection 必须排在 TSecError 之前）就测不到。"""

    code = None

    def __init__(self, message: str, *, detail=None) -> None:
        super().__init__(message, code=None, detail=detail, status_code=None)


class VpnCheckError(TSecError):
    code = "vpn_check_failed"

    def __init__(self, message: str = "VPN检测未通过,请检查靶场VPN网络配置", *, detail=None) -> None:
        super().__init__(message, code=self.code, detail=detail, status_code=None)


class TaskNotFound(TSecError):
    code = "task_not_found"


class ChallengeNotFound(TSecError):
    code = "challenge_not_found"


class InvalidState(TSecError):
    code = "invalid_state"


class DuplicateSubmit(TSecError):
    code = "duplicate"


class ResourceUnavailable(TSecError):
    code = "resource_unavailable"


class InternalError(TSecError):
    code = "internal_error"


class ValidationError(TSecError):
    code = "validation_error"


# ── 数据类（照抄 models.py 的字段与默认值） ──


@dataclass(frozen=True)
class Challenge:
    unique_code: str
    difficulty: str
    level: int
    total_score: int
    flag_count: int
    correct_flag_count: int
    is_completed: bool
    description: Optional[str] = None
    container_status: str = "stopped"
    container_addr: List[str] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: dict) -> "Challenge":
        return cls(
            unique_code=data["unique_code"],
            difficulty=data["difficulty"],
            level=data["level"],
            total_score=data["total_score"],
            flag_count=data["flag_count"],
            correct_flag_count=data["correct_flag_count"],
            is_completed=data["is_completed"],
            description=data.get("description"),
            container_status=data.get("container_status", "stopped"),
            container_addr=list(data.get("container_addr") or []),
        )


@dataclass(frozen=True)
class StartResult:
    unique_code: str
    container_addr: List[str]

    @classmethod
    def from_dict(cls, data: dict) -> "StartResult":
        return cls(
            unique_code=data["unique_code"],
            container_addr=list(data.get("container_addr") or []),
        )


@dataclass(frozen=True)
class HintResult:
    unique_code: str
    hint: Optional[str]

    @classmethod
    def from_dict(cls, data: dict) -> "HintResult":
        return cls(unique_code=data["unique_code"], hint=data.get("hint"))


@dataclass(frozen=True)
class SubmitResult:
    """**没有 unique_code 字段**（models.py:75）。桥必须自己回填。"""

    correct: bool
    awarded: int
    cumulative_score: int
    correct_flag_count: int
    total_flag_count: int
    matched_flag_index: Optional[int]

    @classmethod
    def from_dict(cls, data: dict) -> "SubmitResult":
        return cls(
            correct=data["correct"],
            awarded=data["awarded"],
            cumulative_score=data["cumulative_score"],
            correct_flag_count=data["correct_flag_count"],
            total_flag_count=data["total_flag_count"],
            matched_flag_index=data.get("matched_flag_index"),
        )


@dataclass(frozen=True)
class CloseResult:
    unique_code: str
    closed: bool

    @classmethod
    def from_dict(cls, data: dict) -> "CloseResult":
        return cls(
            unique_code=data["unique_code"],
            closed=data.get("closed", True),
        )


@dataclass(frozen=True)
class VpnCheckResult:
    status: str
    client_ip: Optional[str]
    time: Optional[str]
    ok: bool

    @classmethod
    def from_dict(cls, data: dict) -> "VpnCheckResult":
        status = data.get("status", "")
        return cls(
            status=status,
            client_ip=data.get("client_ip"),
            time=data.get("time"),
            ok=status == "ok",
        )


def parse_challenge_list(data: list) -> List[Challenge]:
    return [Challenge.from_dict(item) for item in data]


# ── 场景剧本 ──
#
# 每个场景给出「平台原始响应 dict」，再由**真 SDK 的 from_dict 逐字路径**
# 构造成数据类——这样字段名漂移会被立刻测出来（而不是被 mock 自己掩盖）。

_OK_CHALLENGES = [
    {
        "unique_code": "web-01",
        "difficulty": "easy",
        "level": 1,
        "total_score": 100,
        "flag_count": 2,
        "correct_flag_count": 1,
        "is_completed": False,
        "description": "一个登录页",
        "container_status": "available",
        "container_addr": ["10.0.0.11:8080"],
    },
    {
        "unique_code": "pwn-02",
        "difficulty": "hard",
        "level": 3,
        "total_score": 300,
        "flag_count": 1,
        "correct_flag_count": 1,
        "is_completed": True,
        "description": None,  # 可选字段缺失/为 null 是正常响应
        "container_status": "stopped",
        "container_addr": [],
    },
    {
        # container_status 不是 available ⇒ container_addr 必须为空。
        # 这条钉住 SDK_API.md:129-130 的异步起题语义。
        "unique_code": "crypto-03",
        "difficulty": "medium",
        "level": 2,
        "total_score": 200,
        "flag_count": 1,
        "correct_flag_count": 0,
        "is_completed": False,
        "container_status": "pending",
        "container_addr": [],
    },
]

SCENARIOS = {
    # 全绿：list/start/hint/submit/close 都正常。
    "ok": {},
    # submit 回 DuplicateSubmit（幂等命中）⇒ 桥必须回 ok:true + duplicate:true。
    "duplicate": {"submit": "duplicate"},
    # submit 被平台判错（correct=false）。
    "wrong": {"submit": "wrong"},
    # InvalidState 带 "max active" ⇒ 可重试。
    "invalid_max_active": {
        "start": "invalid_state",
        "start_message": "max active challenges reached (3/3)",
    },
    # InvalidState 不带 max active（任务已结束）⇒ 不可重试。
    "invalid_task_ended": {
        "start": "invalid_state",
        "start_message": "task already ended",
    },
    # hint 为 None ⇒ 桥回 hint:null，不报错。
    "hint_none": {"hint": None},
    # **SDK 缺陷 1**：平台响应缺必填字段 ⇒ from_dict 抛裸 KeyError。
    "malformed_missing_field": {"malformed": "missing_field"},
    # **SDK 缺陷 2**：2xx 但 body 不是 JSON ⇒ json.JSONDecodeError。
    "non_json_2xx": {"malformed": "non_json"},
    # start 回 pending：地址为空，不是错误。
    "start_pending": {"start": "pending"},
    # 传输层错误。
    "connection_error": {"list": "connection_error"},
    # 平台 503。
    "resource_unavailable": {"start": "resource_unavailable"},
    # list 回 404 task_not_found。
    "task_not_found": {"list": "task_not_found"},
}


def _scenario() -> dict:
    name = os.environ.get("TSEC_MOCK_SCENARIO", "ok")
    return dict(SCENARIOS.get(name, {}))


def _log_cmd(cmd: str) -> None:
    path = os.environ.get("TSEC_MOCK_LOG")
    if not path:
        return
    with open(path, "a", encoding="utf-8") as fh:
        fh.write(cmd + "\n")


class _SelfKill:
    """模拟桥进程崩溃：前 N 次命中命令时直接 ``os._exit(1)``。

    ``os._exit`` 而不是 ``sys.exit``：后者会被 bridge.py 的异常收敛点接住，
    变成一行结构化错误而不是进程死亡——那就测不到「崩溃重启」这条路径。
    """

    def __init__(self) -> None:
        self.remaining = int(os.environ.get("TSEC_MOCK_KILL_COUNT", "0") or 0)
        self.target = os.environ.get("TSEC_MOCK_SELF_KILL", "")

    def maybe_kill(self, cmd: str) -> None:
        if self.target and cmd == self.target and self.remaining > 0:
            self.remaining -= 1
            sys.stderr.write(f"mock: 按剧本自杀于 {cmd}\n")
            sys.stderr.flush()
            os._exit(1)


_KILL = _SelfKill()


class TSecBenchmark:
    """同步假 client。签名与 ``sync.py:48`` 一致。"""

    def __init__(
        self,
        base_url: str,
        token: str,
        *,
        timeout: float = 30.0,
        auto_check_vpn: bool = True,
    ) -> None:
        self._base_url = base_url
        self._token = token
        self._timeout = timeout
        self._auto_check_vpn = auto_check_vpn
        self._sc = _scenario()
        self._closed = False
        # **auto_check_vpn 只在 __enter__ 里生效**（sync.py:83-86）。
        # 桥用 auto_check_vpn=False 构造并显式调 check_vpn，所以这里也不该
        # 在构造里预检——照抄这个惰性，否则桥的「预检与构造可区分」就测不到。
        if auto_check_vpn:
            pass

    def __enter__(self) -> "TSecBenchmark":
        if self._auto_check_vpn:
            self.check_vpn()
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()

    def close(self) -> None:
        # 与真 SDK 的 `sync.py:101-112` 对应：关掉「线程 + loop」。
        # **不记进调用日志**：日志是用来断言「平台调用了什么」的，而 close 是
        # 桥自己的清理动作，不是平台调用——混进去会让「重启后有没有重做预检」
        # 这类断言多出噪音。
        self._closed = True

    # ── 六个方法 ──

    def check_vpn(self) -> VpnCheckResult:
        _log_cmd("check_vpn")
        _KILL.maybe_kill("check_vpn")
        if os.environ.get("TSEC_MOCK_VPN_FAIL"):
            raise VpnCheckError(detail={"reason": "network_error", "error": "mock"})
        return VpnCheckResult.from_dict(
            {"status": "ok", "client_ip": "10.0.100.7", "time": "2026-09-20T18:00:00Z"}
        )

    def list_challenges(self) -> List[Challenge]:
        _log_cmd("list")
        _KILL.maybe_kill("list")
        kind = self._sc.get("list")
        if kind == "connection_error":
            raise TSecConnectionError("mock: 连接被拒绝")
        if kind == "task_not_found":
            raise TaskNotFound("BENCHMARK_TOKEN 无效", status_code=404)
        if self._sc.get("malformed") == "missing_field":
            # **SDK 缺陷 1 的原样复现**：from_dict 用 data["unique_code"] 取
            # 必填字段，缺了就抛裸 KeyError（不是 TSecError）。
            return parse_challenge_list([{"difficulty": "easy"}])
        if self._sc.get("malformed") == "non_json":
            # **SDK 缺陷 2 的原样复现**：2xx 但 body 不是 JSON。真 SDK 在
            # _handle_response 里无保护地调 response.json()。
            raise json.JSONDecodeError("Expecting value", "<html>502 Bad Gateway</html>", 0)
        return parse_challenge_list(_OK_CHALLENGES)

    def start_challenge(self, unique_code: str) -> StartResult:
        _log_cmd("start")
        _KILL.maybe_kill("start")
        kind = self._sc.get("start")
        if kind == "invalid_state":
            raise InvalidState(self._sc.get("start_message", "invalid_state"), status_code=409)
        if kind == "resource_unavailable":
            raise ResourceUnavailable("池子空了", status_code=503)
        if kind == "pending":
            return StartResult.from_dict({"unique_code": unique_code, "container_addr": []})
        return StartResult.from_dict(
            {"unique_code": unique_code, "container_addr": ["10.0.0.11:8080"]}
        )

    def get_hint(self, unique_code: str) -> HintResult:
        _log_cmd("hint")
        _KILL.maybe_kill("hint")
        if "hint" in self._sc:
            return HintResult.from_dict(
                {"unique_code": unique_code, "hint": self._sc["hint"]}
            )
        return HintResult.from_dict(
            {"unique_code": unique_code, "hint": "试试 admin/admin"}
        )

    def submit_flag(self, unique_code: str, flag: str) -> SubmitResult:
        _log_cmd("submit")
        _KILL.maybe_kill("submit")
        kind = self._sc.get("submit")
        if kind == "duplicate":
            raise DuplicateSubmit("同一个 flag 已正确提交过", status_code=409)
        correct = kind != "wrong"
        return SubmitResult.from_dict(
            {
                "correct": correct,
                "awarded": 100 if correct else 0,
                "cumulative_score": 100 if correct else 0,
                "correct_flag_count": 2 if correct else 1,
                "total_flag_count": 2,
                "matched_flag_index": 1 if correct else None,
            }
        )

    def close_challenge(self, unique_code: str) -> CloseResult:
        _log_cmd("close_challenge")
        _KILL.maybe_kill("close_challenge")
        return CloseResult.from_dict({"unique_code": unique_code, "closed": True})


__all__ = [
    "__version__",
    "TSecBenchmark",
    "Challenge",
    "StartResult",
    "HintResult",
    "SubmitResult",
    "CloseResult",
    "VpnCheckResult",
    "TSecError",
    "TSecConnectionError",
    "VpnCheckError",
    "TaskNotFound",
    "ChallengeNotFound",
    "InvalidState",
    "DuplicateSubmit",
    "ResourceUnavailable",
    "InternalError",
    "ValidationError",
]
