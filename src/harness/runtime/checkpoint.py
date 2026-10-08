"""Atomic, versioned run-control checkpoints.

The world event store remains authoritative for domain state. This module
persists the *control-plane* cursor needed to resume without replaying
completed agent events as new work.
"""
from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path
from typing import Any

from pydantic import BaseModel, Field


class RunCheckpoint(BaseModel):
    schema_version: str = "harness/checkpoint/v1"
    run_id: str
    world_revision: int = Field(ge=0)
    event_offset: int = Field(ge=0)
    feedback_offset: int = Field(ge=0)
    budget_used: dict[str, float] = Field(default_factory=dict)
    pending_actions: list[dict[str, Any]] = Field(default_factory=list)
    agent_session_id: str | None = None
    session_checkpoint_id: str | None = None
    plan_revision: int | None = Field(default=None, ge=0)


class FileCheckpointStore:
    """Replace a JSON checkpoint atomically on the same filesystem."""

    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)

    def save(self, checkpoint: RunCheckpoint) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        payload = checkpoint.model_dump_json(indent=2) + "\n"
        fd, tmp_name = tempfile.mkstemp(
            dir=self.path.parent, prefix=f".{self.path.name}.", suffix=".tmp"
        )
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(tmp_name, self.path)
        finally:
            if os.path.exists(tmp_name):
                os.unlink(tmp_name)

    def load(self) -> RunCheckpoint | None:
        if not self.path.exists():
            return None
        return RunCheckpoint.model_validate(json.loads(self.path.read_text(encoding="utf-8")))


async def save_session_checkpoint(
    *,
    run_dir: Path,
    session: Any,
    world_revision: int,
    progress: Any,
    plan_revision: int | None = None,
) -> RunCheckpoint | None:
    """Capture a drained session at the end of event consumption.

    Legacy adapters may not implement checkpoint(). A saved record describes
    a completed stream, NOT an exactly-once crash-resume guarantee.
    """
    get_checkpoint = getattr(session, "checkpoint", None)
    if not callable(get_checkpoint):
        return None
    cursor = await get_checkpoint()
    last_action = getattr(progress, "last_action", None)
    pending = (
        [dict(last_action)]
        if isinstance(last_action, dict) and last_action.get("status") == "running"
        else []
    )
    record = RunCheckpoint(
        run_id=run_dir.name,
        world_revision=world_revision,
        event_offset=cursor.event_offset,
        feedback_offset=cursor.feedback_offset,
        pending_actions=pending,
        session_checkpoint_id=cursor.id,
        plan_revision=plan_revision,
    )
    FileCheckpointStore(run_dir / "checkpoint.json").save(record)
    return record


def load_recovery_state(run_dir: Path) -> tuple[RunCheckpoint, Any]:
    """Load a checkpoint and progress without launching a session."""
    from ..progress import ProgressLedger

    record = FileCheckpointStore(run_dir / "checkpoint.json").load()
    if record is None:
        raise FileNotFoundError(run_dir / "checkpoint.json")
    if record.run_id != run_dir.name:
        raise ValueError("checkpoint run ID mismatch")
    if record.pending_actions:
        raise RuntimeError("unresolved actions require manual reconciliation")
    ledger = ProgressLedger.model_validate_json(
        (run_dir / "progress.json").read_text(encoding="utf-8")
    )
    return record, ledger
