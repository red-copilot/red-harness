from __future__ import annotations

import asyncio
import json
import uuid
from collections.abc import AsyncIterator
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Literal, Protocol

from .agent import AgentResult


EventSource = Literal["runtime", "agent"]
_AGENT_EVENT_TYPES = {
    "action.intent",
    "world.observe",
    "world.hypothesis",
    "world.capability",
    "world.artifact",
    "world.failure",
    "progress.updated",
}


@dataclass(frozen=True)
class AgentEvent:
    type: str
    data: dict[str, Any]
    event_id: str | None = None
    source: EventSource = "runtime"
    trusted: bool = True


@dataclass(frozen=True)
class AgentObservation:
    type: str
    data: dict[str, Any]


@dataclass(frozen=True)
class AgentCheckpoint:
    id: str
    event_offset: int
    feedback_offset: int
    agent_event_offset: int = 0


class AgentSession(Protocol):
    async def events(self) -> AsyncIterator[AgentEvent]: ...

    async def observe(self, observation: AgentObservation) -> None: ...

    async def checkpoint(self) -> AgentCheckpoint: ...

    async def close(self, reason: str) -> None: ...

    async def result(self) -> AgentResult: ...


class OneShotAgentSession:
    """Compatibility session around adapter.run() with split trusted/untrusted event streams."""

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
        self.runtime_event_path = run_dir / "runtime.events.jsonl"
        self.agent_event_path = run_dir / "agent.events.jsonl"
        self.feedback_path = run_dir / "agent.feedback.jsonl"
        self.control_path = run_dir / "agent.control.jsonl"
        self._task: asyncio.Task[AgentResult] | None = None
        self._runtime_event_offset = 0
        self._agent_event_offset = 0
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
        session.runtime_event_path.touch(exist_ok=True)
        session.agent_event_path.touch(exist_ok=True)
        session.feedback_path.touch(exist_ok=True)
        session.control_path.touch(exist_ok=True)
        session._task = asyncio.create_task(asyncio.to_thread(adapter.run, **run_kwargs))
        return session

    def _poll_path(
        self,
        path: Path,
        *,
        offset: int,
        source: EventSource,
        remainder: str,
    ) -> tuple[list[AgentEvent], int, str]:
        if not path.exists():
            return [], offset, remainder
        with path.open("r", encoding="utf-8", errors="replace") as handle:
            handle.seek(offset)
            chunk = handle.read()
            offset = handle.tell()
        if not chunk:
            return [], offset, remainder

        text = remainder + chunk
        lines = text.splitlines(keepends=True)
        remainder = ""
        events: list[AgentEvent] = []
        for line in lines:
            if not line.endswith(("\n", "\r")):
                remainder = line
                continue
            event = self._parse_event(line, source=source)
            if event is not None:
                events.append(event)
        return events, offset, remainder

    async def events(self) -> AsyncIterator[AgentEvent]:
        if self._task is None:
            raise RuntimeError("session has not been started")

        runtime_remainder = ""
        agent_remainder = ""
        while True:
            runtime_events, self._runtime_event_offset, runtime_remainder = self._poll_path(
                self.runtime_event_path,
                offset=self._runtime_event_offset,
                source="runtime",
                remainder=runtime_remainder,
            )
            agent_events, self._agent_event_offset, agent_remainder = self._poll_path(
                self.agent_event_path,
                offset=self._agent_event_offset,
                source="agent",
                remainder=agent_remainder,
            )
            for event in runtime_events:
                yield event
            for event in agent_events:
                yield event

            if self._task.done():
                for remainder, source in (
                    (runtime_remainder, "runtime"),
                    (agent_remainder, "agent"),
                ):
                    if remainder.strip():
                        event = self._parse_event(remainder, source=source)
                        if event is not None:
                            yield event
                await self._task
                break
            await asyncio.sleep(self.poll_interval)

    @staticmethod
    def _parse_event(line: str, *, source: EventSource) -> AgentEvent | None:
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            return AgentEvent(
                type="agent.telemetry_error",
                data={"message": "invalid JSON event", "line": line[:500]},
                source=source,
                trusted=source == "runtime",
            )
        if not isinstance(payload, dict):
            return None
        data = payload.get("data")
        event_type = str(payload.get("type", "unknown"))
        if source == "agent" and event_type not in _AGENT_EVENT_TYPES:
            return AgentEvent(
                type="agent.event_rejected",
                data={
                    "event_type": event_type,
                    "reason": "event type is not allowed on the agent event channel",
                },
                event_id=payload.get("event_id"),
                source="agent",
                trusted=False,
            )
        return AgentEvent(
            type=event_type,
            data=data if isinstance(data, dict) else {},
            event_id=payload.get("event_id"),
            source=source,
            trusted=source == "runtime",
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
            event_offset=self._runtime_event_offset,
            agent_event_offset=self._agent_event_offset,
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
