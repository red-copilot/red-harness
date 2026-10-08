from __future__ import annotations

import asyncio
import json
import uuid
from collections.abc import AsyncIterator
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Protocol

from .agent import AgentResult


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

        remainder = ""
        while True:
            if self.event_path.exists():
                with self.event_path.open("r", encoding="utf-8", errors="replace") as handle:
                    handle.seek(self._event_offset)
                    chunk = handle.read()
                    self._event_offset = handle.tell()
                if chunk:
                    text = remainder + chunk
                    lines = text.splitlines(keepends=True)
                    remainder = ""
                    for line in lines:
                        if not line.endswith(("\n", "\r")):
                            remainder = line
                            continue
                        self._event_line += 1
                        event = self._parse_event(
                            line,
                            fallback_event_id=f"agent-event-line:{self._event_line}",
                        )
                        if event is not None:
                            yield event

            if self._task.done():
                if remainder.strip():
                    self._event_line += 1
                    event = self._parse_event(
                        remainder,
                        fallback_event_id=f"agent-event-line:{self._event_line}",
                    )
                    if event is not None:
                        yield event
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
        with self.feedback_path.open("a", encoding="utf-8") as handle:
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
        with self.control_path.open("a", encoding="utf-8") as handle:
            handle.write(line)
            handle.flush()

    async def result(self) -> AgentResult:
        if self._task is None:
            raise RuntimeError("session has not been started")
        return await self._task
