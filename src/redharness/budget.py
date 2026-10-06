from __future__ import annotations

import json
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

from .models import BudgetSpec
from .trace import TraceRecorder


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
        self._remainder = ""
        self.exceeded: str | None = None

    def poll(self) -> str | None:
        if self.exceeded or not self.event_file.exists():
            return self.exceeded

        with self.event_file.open("r", encoding="utf-8") as handle:
            handle.seek(self._offset)
            chunk = handle.read()
            self._offset = handle.tell()

        if not chunk:
            return None

        text = self._remainder + chunk
        lines = text.splitlines(keepends=True)
        self._remainder = ""
        for line in lines:
            if not line.endswith(("\n", "\r")):
                self._remainder = line
                continue
            self._consume(line.strip())
            if self.exceeded:
                break
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
            payload = json.loads(line)
            event_type = str(payload["type"])
            data = payload.get("data", {})
            if not isinstance(data, dict):
                raise TypeError("event data must be an object")
        except (json.JSONDecodeError, KeyError, TypeError, ValueError) as exc:
            self.trace.emit(
                "agent.telemetry_error",
                actor="agent",
                data={"message": str(exc), "line": line[:500]},
            )
            return

        self.trace.emit(event_type, actor="agent", data=data)
        self._account(event_type, data)
        self._check()

    def _account(self, event_type: str, data: dict[str, Any]) -> None:
        if event_type == "model.request":
            self.metrics.model_calls += int(data.get("count", 1))
        elif event_type == "model.usage":
            input_tokens = int(data.get("input_tokens", 0))
            output_tokens = int(data.get("output_tokens", 0))
            total_tokens = int(data.get("total_tokens", input_tokens + output_tokens))
            self.metrics.input_tokens += input_tokens
            self.metrics.output_tokens += output_tokens
            self.metrics.total_tokens += total_tokens
            self.metrics.model_calls += int(data.get("model_calls", 0))
            self.metrics.cost_usd += float(data.get("cost_usd", 0.0))
        elif event_type in {"tool.call", "tool.denied"}:
            self.metrics.tool_calls += int(data.get("count", 1))

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
