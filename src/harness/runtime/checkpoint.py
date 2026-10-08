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
