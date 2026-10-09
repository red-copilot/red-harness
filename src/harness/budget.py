from __future__ import annotations

import json
import math
import os
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

from .models import BudgetSpec
from .secureio import open_regular_file
from .trace import TraceRecorder

MAX_AGENT_TELEMETRY_BYTES = 64 * 1024 * 1024
MAX_AGENT_TELEMETRY_CHUNK_BYTES = 1024 * 1024
MAX_AGENT_TELEMETRY_EVENT_BYTES = 4 * 1024 * 1024


@dataclass
class UsageMetrics:
    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    model_calls: int = 0
    tool_calls: int = 0
    cost_usd: float = 0.0

    def as_dict(self) -> dict[str, int | float]:
        return asdict(self)


class BudgetMonitor:
    """Tail agent/gateway telemetry and enforce declared run budgets."""

    def __init__(
        self,
        event_file: Path,
        budgets: BudgetSpec,
        trace: TraceRecorder,
    ) -> None:
        self.event_file = event_file
        self.budgets = budgets
        self.trace = trace
        self.metrics = UsageMetrics()
        self._offset = 0
        self._remainder = b""
        self._event_file_identity: tuple[int, int] | None = None
        self.exceeded: str | None = None

    def poll(self) -> str | None:
        if self.exceeded:
            return self.exceeded

        try:
            with open_regular_file(self.event_file, "rb") as handle:
                info = os.fstat(handle.fileno())
                identity = (info.st_dev, info.st_ino)
                if self._event_file_identity is None:
                    self._event_file_identity = identity
                elif identity != self._event_file_identity or info.st_size < self._offset:
                    return self._fail_closed_telemetry("file was replaced or truncated")
                if info.st_size > MAX_AGENT_TELEMETRY_BYTES:
                    return self._fail_closed_telemetry("file exceeds the telemetry size limit")
                handle.seek(self._offset)
                chunk = handle.read(MAX_AGENT_TELEMETRY_CHUNK_BYTES)
                self._offset = handle.tell()
        except OSError:
            return self._fail_closed_telemetry("file is missing or cannot be read safely")

        if not chunk:
            return None

        data = self._remainder + chunk
        lines = data.splitlines(keepends=True)
        self._remainder = b""
        for line in lines:
            if not line.endswith((b"\n", b"\r")):
                if len(line) > MAX_AGENT_TELEMETRY_EVENT_BYTES:
                    return self._fail_closed_telemetry("record exceeds the telemetry event limit")
                self._remainder = line
                continue
            if len(line) > MAX_AGENT_TELEMETRY_EVENT_BYTES:
                return self._fail_closed_telemetry("record exceeds the telemetry event limit")
            self._consume(line.strip().decode("utf-8", errors="replace"))
            if self.exceeded:
                break
        return self.exceeded

    def _fail_closed_telemetry(self, reason: str) -> str:
        self.exceeded = "invalid_telemetry"
        self.trace.emit(
            "agent.telemetry_error",
            actor="harness",
            data={"message": f"untrusted telemetry {reason}"},
        )
        self.trace.emit(
            "budget.exceeded",
            data={"budget": self.exceeded, "reason": "untrusted telemetry could not be trusted"},
        )
        return self.exceeded

    def finish(self) -> str | None:
        self.poll()
        if self._remainder and not self.exceeded:
            self._consume(self._remainder.strip())
            self._remainder = ""
        return self.exceeded

    def _consume(self, line: str) -> None:
        if not line:
            return
        try:
            payload = json.loads(line, parse_constant=self._reject_json_constant)
            if not isinstance(payload, dict):
                raise TypeError("event must be an object")
            event_type = payload.get("type")
            if not isinstance(event_type, str) or not event_type:
                raise TypeError("event type must be a non-empty string")
            data = payload.get("data", {})
            if not isinstance(data, dict):
                raise TypeError("event data must be an object")
        except (json.JSONDecodeError, TypeError, ValueError):
            self._fail_closed_telemetry("record is malformed")
            return

        self.trace.emit(event_type, actor="agent", data=data)
        if not self._account(event_type, data):
            self.trace.emit(
                "agent.telemetry_error",
                actor="harness",
                data={"message": "invalid or negative usage telemetry", "source_type": event_type},
            )
            self.exceeded = "invalid_telemetry"
            self.trace.emit(
                "budget.exceeded",
                data={"budget": self.exceeded, "reason": "untrusted usage telemetry was invalid"},
            )
            return
        self._check()

    @staticmethod
    def _reject_json_constant(value: str) -> None:
        raise ValueError(f"non-finite JSON constant: {value}")

    @staticmethod
    def _integer(data: dict[str, Any], key: str, default: int = 0) -> int | None:
        value = data.get(key, default)
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            return None
        return value

    @staticmethod
    def _cost(data: dict[str, Any]) -> float | None:
        value = data.get("cost_usd", 0.0)
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            return None
        numeric = float(value)
        return numeric if math.isfinite(numeric) and numeric >= 0 else None

    def _account(self, event_type: str, data: dict[str, Any]) -> bool:
        if event_type == "model.usage_missing":
            return False
        if event_type == "model.request":
            count = self._integer(data, "count", 1)
            if count is None:
                return False
            self.metrics.model_calls += count
        elif event_type == "model.usage":
            input_tokens = self._integer(data, "input_tokens")
            output_tokens = self._integer(data, "output_tokens")
            total_tokens = self._integer(
                data, "total_tokens", (input_tokens or 0) + (output_tokens or 0)
            )
            model_calls = self._integer(data, "model_calls")
            cost = self._cost(data)
            if (
                None in (input_tokens, output_tokens, total_tokens, model_calls, cost)
                or total_tokens < input_tokens + output_tokens
            ):
                return False
            self.metrics.input_tokens += input_tokens
            self.metrics.output_tokens += output_tokens
            self.metrics.total_tokens += total_tokens
            self.metrics.model_calls += model_calls
            self.metrics.cost_usd += cost
        elif event_type in {"tool.call", "tool.denied"}:
            count = self._integer(data, "count", 1)
            if count is None:
                return False
            self.metrics.tool_calls += count
        return True

    def _check(self) -> None:
        checks = (
            ("max_tokens", self.budgets.max_tokens, self.metrics.total_tokens),
            ("max_model_calls", self.budgets.max_model_calls, self.metrics.model_calls),
            ("max_tool_calls", self.budgets.max_tool_calls, self.metrics.tool_calls),
            ("max_cost_usd", self.budgets.max_cost_usd, self.metrics.cost_usd),
        )
        for name, limit, value in checks:
            if limit is not None and value > limit:
                self.exceeded = name
                self.trace.emit(
                    "budget.exceeded",
                    data={"budget": name, "limit": limit, "value": value},
                )
                return
