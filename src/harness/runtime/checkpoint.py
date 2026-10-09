"""Atomic, versioned run-control checkpoints.

The world event store remains authoritative for domain state. This module
persists the *control-plane* cursor needed to resume without replaying
completed agent events as new work.
"""

from __future__ import annotations

import json
import math
import os
import stat
import tempfile
from contextlib import contextmanager
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal, Self

from pydantic import BaseModel, Field, model_validator

from ..secureio import read_regular_text

MAX_CHECKPOINT_BYTES = 8 * 1024 * 1024


class RunCheckpoint(BaseModel):
    schema_version: Literal["harness/checkpoint/v2"] = "harness/checkpoint/v2"
    run_id: str
    world_revision: int = Field(ge=0)
    event_offset: int = Field(ge=0)
    feedback_offset: int = Field(ge=0)
    gateway_event_offset: int = Field(default=0, ge=0)
    budget_used: dict[str, int | float] = Field(default_factory=dict)
    pending_actions: list[dict[str, Any]] = Field(default_factory=list)
    agent_session_id: str | None = Field(default=None, min_length=1, max_length=256)
    session_checkpoint_id: str | None = None
    plan_revision: int | None = Field(default=None, ge=0)
    processed_event_ids: dict[str, str] = Field(default_factory=dict)
    progress_event_count: int | None = Field(default=None, ge=0)

    @model_validator(mode="before")
    @classmethod
    def validate_budget_usage_types(cls, value: Any) -> Any:
        if not isinstance(value, dict):
            return value
        budget_used = value.get("budget_used", {})
        if isinstance(budget_used, dict):
            if any(
                isinstance(metric, bool) or not isinstance(metric, (int, float))
                for metric in budget_used.values()
            ):
                raise ValueError("checkpoint budget usage must contain numeric values")
            counter_fields = {
                "input_tokens",
                "output_tokens",
                "total_tokens",
                "model_calls",
                "tool_calls",
            }
            if any(
                field in budget_used and type(budget_used[field]) is not int
                for field in counter_fields
            ):
                raise ValueError("checkpoint token and call usage must be integers")
        return value

    @model_validator(mode="after")
    def validate_recovery_data(self) -> Self:
        if any(not math.isfinite(value) or value < 0 for value in self.budget_used.values()):
            raise ValueError("checkpoint budget usage must be finite and non-negative")
        pending_ids: set[str] = set()
        for action in self.pending_actions:
            action_id = action.get("tool_call_id")
            # Preserve uncorrelated legacy in-flight actions so recovery can
            # fail closed instead of aborting the live run while saving.
            if action_id is not None and (not isinstance(action_id, str) or not action_id):
                raise ValueError("checkpoint action IDs must be non-empty strings")
            if isinstance(action_id, str) and action_id in pending_ids:
                raise ValueError("checkpoint pending action IDs must be unique")
            if isinstance(action_id, str):
                pending_ids.add(action_id)
        return self


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
        payload = (checkpoint.model_dump_json(indent=2) + "\n").encode("utf-8")
        if len(payload) > MAX_CHECKPOINT_BYTES:
            raise ValueError("checkpoint exceeds the 8 MiB recovery limit")
        fd, tmp_name = tempfile.mkstemp(
            dir=self.path.parent, prefix=f".{self.path.name}.", suffix=".tmp"
        )
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(tmp_name, self.path)
            _fsync_directory(self.path.parent)
        finally:
            if os.path.exists(tmp_name):
                os.unlink(tmp_name)

    def load(self) -> RunCheckpoint | None:
        try:
            path_stat = self.path.stat(follow_symlinks=False)
        except FileNotFoundError:
            return None
        if not stat.S_ISREG(path_stat.st_mode):
            raise ValueError("checkpoint path must be a regular file")
        payload = read_regular_text(self.path, max_bytes=MAX_CHECKPOINT_BYTES)
        data = json.loads(payload)
        if isinstance(data, dict) and data.get("schema_version") == "harness/checkpoint/v1":
            data["schema_version"] = "harness/checkpoint/v2"
            pending_actions = data.get("pending_actions", [])
            if isinstance(pending_actions, list):
                for action in pending_actions:
                    if isinstance(action, dict) and "tool_call_id" not in action:
                        legacy_id = action.get("id")
                        if isinstance(legacy_id, str) and legacy_id:
                            action["tool_call_id"] = legacy_id
        return RunCheckpoint.model_validate(data)


def _fsync_directory(path: Path) -> None:
    """Persist the directory entry update after an atomic checkpoint rename."""
    if not hasattr(os, "O_DIRECTORY"):
        return
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _write_reconciliation_report(path: Path, reconciliation: ActionReconciliation) -> None:
    payload = reconciliation.model_dump_json(indent=2) + "\n"
    descriptor, temporary_name = tempfile.mkstemp(
        dir=path.parent, prefix=f".{path.name}.", suffix=".tmp"
    )
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary_name, path)
        _fsync_directory(path.parent)
    finally:
        if os.path.exists(temporary_name):
            os.unlink(temporary_name)


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
    """Persist the session cursor and current Harness control state.

    Callers may snapshot after each consumed event and at stream completion.
    Legacy adapters may not implement checkpoint(). This record is a recovery
    aid, not an exactly-once crash-resume guarantee.
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
    if budget_used is None:
        usage_snapshot = getattr(session, "usage_metrics", {})
        budget_used = usage_snapshot if isinstance(usage_snapshot, dict) else {}
    record = RunCheckpoint(
        run_id=run_dir.name,
        world_revision=world_revision,
        event_offset=cursor.event_offset,
        feedback_offset=cursor.feedback_offset,
        gateway_event_offset=cursor.gateway_event_offset,
        budget_used=budget_used or {},
        pending_actions=pending,
        session_checkpoint_id=cursor.id,
        agent_session_id=getattr(cursor, "session_id", None),
        plan_revision=plan_revision,
        processed_event_ids=processed_event_ids or {},
        progress_event_count=getattr(progress, "event_count", None),
    )
    encoded_size = len((record.model_dump_json(indent=2) + "\n").encode("utf-8"))
    if encoded_size > MAX_CHECKPOINT_BYTES and record.processed_event_ids:
        # Stream offsets are the primary cursor for consumed events. Preserve
        # the newest dedupe identities while keeping the checkpoint readable;
        # the in-memory solver map is already bounded independently.
        entries = list(record.processed_event_ids.items())
        low, high = 0, len(entries)
        while low < high:
            middle = (low + high) // 2
            candidate = record.model_copy(
                update={"processed_event_ids": dict(entries[middle:])}
            )
            candidate_size = len(
                (candidate.model_dump_json(indent=2) + "\n").encode("utf-8")
            )
            if candidate_size <= MAX_CHECKPOINT_BYTES:
                high = middle
            else:
                low = middle + 1
        record = record.model_copy(
            update={"processed_event_ids": dict(entries[low:])}
        )
    FileCheckpointStore(run_dir / "checkpoint.json").save(record)
    return record


def load_recovery_state(
    run_dir: Path, *, current_world_revision: int | None = None
) -> tuple[RunCheckpoint, Any]:
    """Load one coherent checkpoint/progress view without launching a session."""
    with _reconciliation_lock(run_dir):
        return _load_recovery_state_locked(
            run_dir, current_world_revision=current_world_revision
        )


def _load_recovery_state_locked(
    run_dir: Path, *, current_world_revision: int | None
) -> tuple[RunCheckpoint, Any]:
    from ..progress import ProgressLedger

    record = FileCheckpointStore(run_dir / "checkpoint.json").load()
    if record is None:
        raise FileNotFoundError(run_dir / "checkpoint.json")
    if record.run_id != run_dir.name:
        raise ValueError("checkpoint run ID mismatch")
    if current_world_revision is not None and record.world_revision != current_world_revision:
        raise ValueError("checkpoint world revision mismatch")
    if any(action.get("status") == "unknown" for action in record.pending_actions):
        raise RuntimeError("unknown action effects require explicit resolution")
    if record.pending_actions:
        raise RuntimeError("unresolved actions require manual reconciliation")
    ledger = ProgressLedger.model_validate_json(
        read_regular_text(run_dir / "progress.json", max_bytes=8 * 1024 * 1024)
    )
    if ledger.active_actions or (
        isinstance(ledger.last_action, dict) and ledger.last_action.get("status") == "running"
    ):
        raise RuntimeError("unresolved actions remain in progress ledger")
    if ledger.last_event_id and ledger.last_event_id not in record.processed_event_ids:
        raise RuntimeError("progress ledger contains events newer than its checkpoint")
    if (
        record.progress_event_count is not None
        and record.progress_event_count != ledger.event_count
    ):
        raise RuntimeError("progress and checkpoint event counts do not match")
    return record, ledger


def reconcile_pending_actions(
    run_dir: str | Path,
    *,
    outcomes: dict[str, Literal["succeeded", "failed", "unknown"]],
) -> ActionReconciliation:
    """Record operator-supplied outcomes without replaying interrupted actions."""
    path = Path(run_dir)
    with _run_action_owner(path), _reconciliation_lock(path):
        return _reconcile_pending_actions_locked(path, outcomes=outcomes)


@contextmanager
def run_action_owner(run_dir: str | Path):
    """Own action execution for one run, excluding operator reconciliation."""
    with _run_action_owner(Path(run_dir)):
        yield


@contextmanager
def _run_action_owner(run_dir: Path):
    """Acquire the per-run action owner lock without waiting behind another owner."""
    if not hasattr(os, "O_NOFOLLOW") or not hasattr(os, "O_DIRECTORY"):
        raise OSError("platform does not support safe action ownership locking")
    directory_fd = os.open(run_dir, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        flags = os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
        lock_fd = os.open(".run-action-owner.lock", flags, 0o600, dir_fd=directory_fd)
    finally:
        os.close(directory_fd)
    try:
        if not stat.S_ISREG(os.fstat(lock_fd).st_mode):
            raise OSError("run action owner lock must be a regular file")
        import fcntl

        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise RuntimeError("run has an active action owner") from exc
        try:
            yield
        finally:
            fcntl.flock(lock_fd, fcntl.LOCK_UN)
    finally:
        os.close(lock_fd)


@contextmanager
def _reconciliation_lock(run_dir: Path):
    """Serialize cross-process operator decisions for one interrupted run."""
    if not hasattr(os, "O_NOFOLLOW") or not hasattr(os, "O_DIRECTORY"):
        raise OSError("platform does not support safe action reconciliation locking")
    directory_fd = os.open(run_dir, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        flags = os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
        lock_fd = os.open(".action-reconciliation.lock", flags, 0o600, dir_fd=directory_fd)
    finally:
        os.close(directory_fd)
    try:
        if not stat.S_ISREG(os.fstat(lock_fd).st_mode):
            raise OSError("action reconciliation lock must be a regular file")
        import fcntl

        while True:
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_EX)
                break
            except InterruptedError:
                continue
        try:
            yield
        finally:
            fcntl.flock(lock_fd, fcntl.LOCK_UN)
    finally:
        os.close(lock_fd)


def _reconcile_pending_actions_locked(
    run_dir: Path,
    *,
    outcomes: dict[str, Literal["succeeded", "failed", "unknown"]],
) -> ActionReconciliation:
    """Reconcile after acquiring the run's cross-process mutation lock."""
    path = run_dir
    if not outcomes:
        raise ValueError("at least one pending action outcome is required")
    try:
        terminal_stat = (path / "result.json").lstat()
    except FileNotFoundError:
        terminal_stat = None
    if terminal_stat is not None:
        raise ValueError("terminal runs cannot be reconciled")
    store = FileCheckpointStore(path / "checkpoint.json")
    checkpoint = store.load()
    if checkpoint is None:
        raise FileNotFoundError(path / "checkpoint.json")
    if checkpoint.run_id != path.name:
        raise ValueError("checkpoint run ID mismatch")
    from ..progress import ProgressLedger

    progress_path = path / "progress.json"
    progress = ProgressLedger.model_validate_json(
        read_regular_text(progress_path, max_bytes=8 * 1024 * 1024)
    )
    report_path = path / "action.reconciliation.json"
    pending_ids = {
        action.get("tool_call_id")
        for action in checkpoint.pending_actions
        if isinstance(action.get("tool_call_id"), str)
    }
    if not pending_ids:
        if report_path.is_file():
            prior = ActionReconciliation.model_validate_json(
                read_regular_text(report_path, max_bytes=1024 * 1024)
            )
            if prior.run_id != checkpoint.run_id or prior.outcomes != outcomes:
                raise ValueError("checkpoint has no pending actions for the supplied outcomes")
            return prior
        if all(progress.reconciled_actions.get(key) == value for key, value in outcomes.items()):
            reconciliation = ActionReconciliation(run_id=checkpoint.run_id, outcomes=outcomes)
            _write_reconciliation_report(report_path, reconciliation)
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
            previous_outcome = progress.reconciled_actions[call_id]
            if previous_outcome == outcome:
                continue
            if previous_outcome != "unknown" or outcome == "unknown":
                raise ValueError(f"action {call_id} was already reconciled differently")
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
    unresolved = [
        {**action, "status": "unknown"}
        for action in checkpoint.pending_actions
        if outcomes.get(action.get("tool_call_id")) == "unknown"
    ]
    store.save(checkpoint.model_copy(update={"pending_actions": unresolved}))
    _write_reconciliation_report(report_path, reconciliation)
    return reconciliation
