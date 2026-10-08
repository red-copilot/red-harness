"""Atomic, versioned run-control checkpoints.

The world event store remains authoritative for domain state. This module
persists the *control-plane* cursor needed to resume without replaying
completed agent events as new work.
"""

from __future__ import annotations

import json
import os
import tempfile
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal

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
    processed_event_ids: dict[str, str] = Field(default_factory=dict)


class ActionReconciliation(BaseModel):
    run_id: str
    reconciled_at: datetime = Field(default_factory=lambda: datetime.now(UTC))
    outcomes: dict[str, Literal["succeeded", "failed", "unknown"]]


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
    budget_used: dict[str, float] | None = None,
    processed_event_ids: dict[str, str] | None = None,
) -> RunCheckpoint | None:
    """Capture a drained session at the end of event consumption.

    Legacy adapters may not implement checkpoint(). A saved record describes
    a completed stream, NOT an exactly-once crash-resume guarantee.
    """
    get_checkpoint = getattr(session, "checkpoint", None)
    if not callable(get_checkpoint):
        return None
    cursor = await get_checkpoint()
    active_actions = getattr(progress, "active_actions", {})
    pending = [dict(action) for action in active_actions.values()]
    last_action = getattr(progress, "last_action", None)
    if not pending and isinstance(last_action, dict) and last_action.get("status") == "running":
        pending = [dict(last_action)]
    record = RunCheckpoint(
        run_id=run_dir.name,
        world_revision=world_revision,
        event_offset=cursor.event_offset,
        feedback_offset=cursor.feedback_offset,
        budget_used=budget_used or {},
        pending_actions=pending,
        session_checkpoint_id=cursor.id,
        plan_revision=plan_revision,
        processed_event_ids=processed_event_ids or {},
    )
    FileCheckpointStore(run_dir / "checkpoint.json").save(record)
    return record


def load_recovery_state(
    run_dir: Path, *, current_world_revision: int | None = None
) -> tuple[RunCheckpoint, Any]:
    """Load a checkpoint and progress without launching a session."""
    from ..progress import ProgressLedger

    record = FileCheckpointStore(run_dir / "checkpoint.json").load()
    if record is None:
        raise FileNotFoundError(run_dir / "checkpoint.json")
    if record.run_id != run_dir.name:
        raise ValueError("checkpoint run ID mismatch")
    if current_world_revision is not None and record.world_revision != current_world_revision:
        raise ValueError("checkpoint world revision mismatch")
    if record.pending_actions:
        raise RuntimeError("unresolved actions require manual reconciliation")
    ledger = ProgressLedger.model_validate_json(
        (run_dir / "progress.json").read_text(encoding="utf-8")
    )
    return record, ledger


def reconcile_pending_actions(
    run_dir: str | Path,
    *,
    outcomes: dict[str, Literal["succeeded", "failed", "unknown"]],
) -> ActionReconciliation:
    """Record operator-supplied outcomes without replaying interrupted actions."""
    path = Path(run_dir)
    if not outcomes:
        raise ValueError("at least one pending action outcome is required")
    store = FileCheckpointStore(path / "checkpoint.json")
    checkpoint = store.load()
    if checkpoint is None:
        raise FileNotFoundError(path / "checkpoint.json")
    if checkpoint.run_id != path.name:
        raise ValueError("checkpoint run ID mismatch")
    from ..progress import ProgressLedger

    progress_path = path / "progress.json"
    progress = ProgressLedger.model_validate_json(progress_path.read_text(encoding="utf-8"))
    report_path = path / "action.reconciliation.json"
    pending_ids = {
        action.get("tool_call_id")
        for action in checkpoint.pending_actions
        if isinstance(action.get("tool_call_id"), str)
    }
    if not pending_ids:
        if report_path.is_file():
            prior = ActionReconciliation.model_validate_json(
                report_path.read_text(encoding="utf-8")
            )
            if prior.outcomes != outcomes:
                raise ValueError("checkpoint has no pending actions for the supplied outcomes")
            return prior
        if all(progress.reconciled_actions.get(key) == value for key, value in outcomes.items()):
            reconciliation = ActionReconciliation(run_id=checkpoint.run_id, outcomes=outcomes)
            report_path.write_text(
                reconciliation.model_dump_json(indent=2) + "\n", encoding="utf-8"
            )
            return reconciliation
        raise ValueError("checkpoint has no identifiable pending actions")
    if set(outcomes) != pending_ids:
        raise ValueError("reconciliation outcomes must match pending action IDs exactly")
    last_action = progress.last_action or {}
    last_id = last_action.get("tool_call_id")
    for call_id, outcome in outcomes.items():
        if isinstance(progress.active_actions, dict):
            progress.active_actions.pop(call_id, None)
        if call_id in progress.reconciled_actions:
            if progress.reconciled_actions[call_id] != outcome:
                raise ValueError(f"action {call_id} was already reconciled differently")
            continue
        progress.reconciled_actions[call_id] = outcome
        if call_id == last_id:
            progress.last_action = {**last_action, "status": outcome}
        if outcome == "succeeded":
            progress.execution_succeeded += 1
            progress.no_progress_count += 1
        elif outcome == "failed":
            progress.failure_count += 1
            progress.no_progress_count += 1

    reconciliation = ActionReconciliation(run_id=checkpoint.run_id, outcomes=outcomes)
    progress.write(progress_path)
    store.save(checkpoint.model_copy(update={"pending_actions": []}))
    report_path.write_text(reconciliation.model_dump_json(indent=2) + "\n", encoding="utf-8")
    return reconciliation
