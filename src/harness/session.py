from __future__ import annotations

import asyncio
import json
import math
import uuid
from collections.abc import AsyncIterator
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Protocol

from .agent import AgentResult
from .budget import UsageMetrics
from .secureio import open_regular_file


@dataclass(frozen=True)
class AgentEvent:
    type: str
    data: dict[str, Any]
    event_id: str | None = None


@dataclass(frozen=True)
class AgentObservation:
    type: str
    data: dict[str, Any]


@dataclass(frozen=True)
class AgentCheckpoint:
    id: str
    event_offset: int
    feedback_offset: int
    session_id: str | None = None
    gateway_event_offset: int = 0


class AgentSession(Protocol):
    async def events(self) -> AsyncIterator[AgentEvent]: ...

    async def observe(self, observation: AgentObservation) -> None: ...

    async def checkpoint(self) -> AgentCheckpoint: ...

    async def close(self, reason: str) -> None: ...

    async def result(self) -> AgentResult: ...


class OneShotAgentSession:
    """Compatibility session that streams normalized events around a legacy adapter.run()."""

    def __init__(
        self,
        adapter: Any,
        *,
        run_kwargs: dict[str, Any],
        run_dir: Path,
        poll_interval: float = 0.05,
    ) -> None:
        self.adapter = adapter
        self.run_kwargs = run_kwargs
        self.run_dir = run_dir
        self.poll_interval = poll_interval
        self.event_path = run_dir / "events.jsonl"
        self.feedback_path = run_dir / "agent.feedback.jsonl"
        self.control_path = run_dir / "agent.control.jsonl"
        self._task: asyncio.Task[AgentResult] | None = None
        self._event_offset = 0
        self._event_line = 0
        self._feedback_offset = 0
        self._closed = False
        self._usage_metrics = UsageMetrics()
        self._usage_telemetry_valid = True

    @property
    def usage_metrics(self) -> dict[str, int | float]:
        return self._usage_metrics.as_dict()

    @property
    def usage_telemetry_valid(self) -> bool:
        return self._usage_telemetry_valid

    @staticmethod
    def _counter(data: dict[str, Any], name: str, default: int = 0) -> int | None:
        value = data.get(name, default)
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            return None
        return value

    @staticmethod
    def _cost(data: dict[str, Any]) -> float | None:
        value = data.get("cost_usd", 0.0)
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            return None
        amount = float(value)
        return amount if math.isfinite(amount) and amount >= 0 else None

    def _account(self, event: AgentEvent) -> None:
        if event.type == "model.usage_missing":
            self._usage_telemetry_valid = False
        elif event.type == "model.request":
            count = self._counter(event.data, "count", 1)
            if count is None:
                self._usage_telemetry_valid = False
            else:
                self._usage_metrics.model_calls += count
        elif event.type == "model.usage":
            input_tokens = self._counter(event.data, "input_tokens")
            output_tokens = self._counter(event.data, "output_tokens")
            total_tokens = self._counter(
                event.data, "total_tokens", (input_tokens or 0) + (output_tokens or 0)
            )
            cost = self._cost(event.data)
            if (
                None in (input_tokens, output_tokens, total_tokens, cost)
                or total_tokens < input_tokens + output_tokens
            ):
                self._usage_telemetry_valid = False
            else:
                self._usage_metrics.input_tokens += input_tokens
                self._usage_metrics.output_tokens += output_tokens
                self._usage_metrics.total_tokens += total_tokens
                self._usage_metrics.cost_usd += cost
        elif event.type in {"tool.call", "tool.denied"}:
            count = self._counter(event.data, "count", 1)
            if count is None:
                self._usage_telemetry_valid = False
            else:
                self._usage_metrics.tool_calls += count

    @classmethod
    async def start(
        cls,
        adapter: Any,
        *,
        run_kwargs: dict[str, Any],
        run_dir: Path,
    ) -> OneShotAgentSession:
        session = cls(adapter, run_kwargs=run_kwargs, run_dir=run_dir)
        session.feedback_path.touch(exist_ok=True)
        session.control_path.touch(exist_ok=True)
        session._task = asyncio.create_task(asyncio.to_thread(adapter.run, **run_kwargs))
        return session

    async def events(self) -> AsyncIterator[AgentEvent]:
        if self._task is None:
            raise RuntimeError("session has not been started")

        while True:
            event = None
            if self.event_path.exists():
                with open_regular_file(
                    self.event_path, "r", encoding="utf-8", errors="replace"
                ) as handle:
                    handle.seek(self._event_offset)
                    line = handle.readline()
                    if line and (line.endswith(("\n", "\r")) or self._task.done()):
                        # Advance only through the event yielded below. A read-ahead
                        # chunk could contain later events that have not been applied.
                        self._event_offset = handle.tell()
                        self._event_line += 1
                        event = self._parse_event(
                            line,
                            fallback_event_id=f"agent-event-line:{self._event_line}",
                        )

            if event is not None:
                self._account(event)
                yield event
                continue

            if self._task.done():
                await self._task
                break
            await asyncio.sleep(self.poll_interval)

    @staticmethod
    def _parse_event(
        line: str,
        *,
        fallback_event_id: str | None = None,
    ) -> AgentEvent | None:
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            return AgentEvent(
                type="agent.telemetry_error",
                data={"message": "invalid JSON event", "line": line[:500]},
                event_id=fallback_event_id,
            )
        if not isinstance(payload, dict):
            return None
        data = payload.get("data")
        event_id = payload.get("event_id")
        return AgentEvent(
            type=str(payload.get("type", "unknown")),
            data=data if isinstance(data, dict) else {},
            event_id=(event_id if isinstance(event_id, str) and event_id else fallback_event_id),
        )

    async def observe(self, observation: AgentObservation) -> None:
        record = {
            "id": f"feedback_{uuid.uuid4().hex}",
            "type": observation.type,
            "data": observation.data,
        }
        line = json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n"
        await asyncio.to_thread(self._append_feedback, line)

    def _append_feedback(self, line: str) -> None:
        with open_regular_file(self.feedback_path, "a") as handle:
            handle.write(line)
            handle.flush()
            self._feedback_offset = handle.tell()

    async def checkpoint(self) -> AgentCheckpoint:
        return AgentCheckpoint(
            id=f"checkpoint_{uuid.uuid4().hex}",
            event_offset=self._event_offset,
            feedback_offset=self._feedback_offset,
        )

    async def close(self, reason: str) -> None:
        if self._closed:
            return
        self._closed = True
        record = {
            "type": "session.close_requested",
            "data": {"reason": reason},
        }
        line = json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n"
        await asyncio.to_thread(self._append_control, line)

    def _append_control(self, line: str) -> None:
        with open_regular_file(self.control_path, "a") as handle:
            handle.write(line)
            handle.flush()

    async def result(self) -> AgentResult:
        if self._task is None:
            raise RuntimeError("session has not been started")
        return await self._task
