"""Domain-neutral interfaces for explicitly registered Gateway tools.

Adapter results and their evidence references are untrusted observations. They
must never be fed directly into an objective verdict as proof of an effect.
"""
from __future__ import annotations

import asyncio
import json
import threading
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Literal, Protocol

from pydantic import BaseModel, ConfigDict, Field

from .secureio import read_regular_text


class ToolEvidenceRef(BaseModel):
    """Content identity for adapter output, explicitly marked as untrusted."""

    model_config = ConfigDict(frozen=True, extra="forbid")

    evidence_id: str = Field(min_length=1, max_length=256)
    sha256: str = Field(pattern=r"^[a-f0-9]{64}$")
    media_type: str = Field(default="application/json", min_length=1, max_length=128)
    trust: Literal["untrusted"] = "untrusted"


@dataclass(frozen=True)
class ToolExecutionContext:
    call_id: str
    workspace: Path
    task_dir: Path
    cancelled: asyncio.Event
    metadata: Mapping[str, str] = field(default_factory=dict)


@dataclass(frozen=True)
class ToolExecutionResult:
    output: Any
    evidence_refs: tuple[ToolEvidenceRef, ...] = ()
    metadata: Mapping[str, Any] = field(default_factory=dict)


class ToolCallRegistry:
    """Fail closed on a tool call ID already recorded in this run's event log."""

    def __init__(self, event_file: Path) -> None:
        self._lock = threading.Lock()
        self._seen: set[str] = set()
        if not event_file.exists():
            return
        text = read_regular_text(event_file)
        for line in text.splitlines():
            if not line.strip():
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError as exc:
                raise ValueError("cannot safely deduplicate calls from a malformed event log") from exc
            if not isinstance(event, dict) or not isinstance(event.get("data"), dict):
                continue
            if event.get("type") == "tool.call":
                call_id = event["data"].get("tool_call_id")
                if isinstance(call_id, str) and call_id:
                    self._seen.add(call_id)

    def claim(self, call_id: str) -> bool:
        with self._lock:
            if call_id in self._seen:
                return False
            self._seen.add(call_id)
            return True


class ToolAdapter(Protocol):
    """One explicitly selected tool implementation; no implicit shell fallback."""

    tool_name: str

    async def execute(
        self,
        arguments: Mapping[str, Any],
        context: ToolExecutionContext,
    ) -> ToolExecutionResult: ...


async def execute_tool_adapter(
    adapter: ToolAdapter,
    arguments: Mapping[str, Any],
    context: ToolExecutionContext,
    *,
    cancellation_grace_seconds: float = 1.0,
) -> ToolExecutionResult:
    """Signal cancellation to a plugin before cancelling its coroutine."""
    task = asyncio.create_task(adapter.execute(arguments, context))
    try:
        result = await asyncio.shield(task)
    except asyncio.CancelledError:
        context.cancelled.set()
        try:
            _, pending = await asyncio.wait({task}, timeout=cancellation_grace_seconds)
        except asyncio.CancelledError:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
            raise
        if pending:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        raise
    if not isinstance(result, ToolExecutionResult):
        raise TypeError("tool adapter must return ToolExecutionResult")
    return result
