import asyncio
from pathlib import Path

import pytest
from pydantic import ValidationError

from harness.progress import ProgressLedger
from harness.runtime.checkpoint import (
    FileCheckpointStore,
    RunCheckpoint,
    load_recovery_state,
    reconcile_pending_actions,
    save_session_checkpoint,
)
from harness.session import AgentCheckpoint, AgentEvent


def test_checkpoint_roundtrip_and_replace(tmp_path: Path) -> None:
    store = FileCheckpointStore(tmp_path / "run" / "checkpoint.json")
    assert store.load() is None
    initial = RunCheckpoint(
        run_id="run-a",
        world_revision=3,
        event_offset=12,
        feedback_offset=4,
        budget_used={"tokens": 400.0},
        pending_actions=[{"id": "tool-1", "status": "unknown"}],
        agent_session_id="session-a",
        plan_revision=2,
    )
    store.save(initial)
    assert store.load() == initial
    store.save(initial.model_copy(update={"event_offset": 20}))
    assert store.load().event_offset == 20
    assert not list((tmp_path / "run").glob("*.tmp"))


def test_checkpoint_save_failure_keeps_previous_record(tmp_path: Path, monkeypatch) -> None:
    store = FileCheckpointStore(tmp_path / "checkpoint.json")
    initial = RunCheckpoint(run_id="run-a", world_revision=1, event_offset=3, feedback_offset=0)
    store.save(initial)

    def fail_replace(*_args, **_kwargs):
        raise OSError("injected replace failure")

    monkeypatch.setattr("harness.runtime.checkpoint.os.replace", fail_replace)
    with pytest.raises(OSError, match="injected"):
        store.save(initial.model_copy(update={"event_offset": 4}))

    assert store.load() == initial
    assert not list(tmp_path.glob("*.tmp"))


def test_checkpoint_rejects_negative_cursors() -> None:
    with pytest.raises(ValidationError):
        RunCheckpoint(run_id="run-a", world_revision=0, event_offset=-1)


def test_completed_session_snapshot(tmp_path: Path) -> None:
    class Session:
        async def checkpoint(self):
            return AgentCheckpoint(id="cp-3", event_offset=24, feedback_offset=9)

    run_dir = tmp_path / "run-1"
    run_dir.mkdir()
    ledger = ProgressLedger(last_action={"tool_call_id": "a", "status": "running"})
    result = asyncio.run(
        save_session_checkpoint(
            run_dir=run_dir,
            session=Session(),
            world_revision=4,
            progress=ledger,
            plan_revision=3,
            budget_used={"total_tokens": 20, "cost_usd": 0.03},
            processed_event_ids={"event-1": "hash-1"},
        )
    )
    assert result is not None
    assert result.session_checkpoint_id == "cp-3"
    assert result.pending_actions[0]["tool_call_id"] == "a"
    assert result.budget_used == {"total_tokens": 20, "cost_usd": 0.03}
    assert result.processed_event_ids == {"event-1": "hash-1"}
    assert FileCheckpointStore(run_dir / "checkpoint.json").load() == result


def test_checkpoint_retains_all_parallel_running_actions(tmp_path: Path) -> None:
    class Session:
        async def checkpoint(self):
            return AgentCheckpoint(id="cp-parallel", event_offset=10, feedback_offset=0)

    run_dir = tmp_path / "parallel-run"
    run_dir.mkdir()
    ledger = ProgressLedger()
    ledger.record_event(AgentEvent(type="tool.call", data={"tool": "read", "tool_call_id": "a"}))
    ledger.record_event(AgentEvent(type="tool.call", data={"tool": "bash", "tool_call_id": "b"}))

    checkpoint = asyncio.run(
        save_session_checkpoint(
            run_dir=run_dir,
            session=Session(),
            world_revision=0,
            progress=ledger,
        )
    )

    assert {action["tool_call_id"] for action in checkpoint.pending_actions} == {"a", "b"}


def test_legacy_session_without_checkpoint(tmp_path: Path) -> None:
    result = asyncio.run(
        save_session_checkpoint(
            run_dir=tmp_path,
            session=object(),
            world_revision=0,
            progress=ProgressLedger(),
        )
    )
    assert result is None


def test_recovery_restores_progress(tmp_path: Path) -> None:
    run_dir = tmp_path / "safe-run"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(run_id="safe-run", world_revision=2, event_offset=19, feedback_offset=0)
    )
    ProgressLedger(verified_actions=2).write(run_dir / "progress.json")
    checkpoint, progress = load_recovery_state(run_dir)
    assert checkpoint.event_offset == 19
    assert progress.verified_actions == 2


def test_recovery_refuses_unresolved_actions(tmp_path: Path) -> None:
    run_dir = tmp_path / "interrupted-run"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(
            run_id="interrupted-run",
            world_revision=1,
            event_offset=0,
            feedback_offset=0,
            pending_actions=[{"tool_call_id": "call-1", "status": "running"}],
        )
    )
    ProgressLedger().write(run_dir / "progress.json")
    with pytest.raises(RuntimeError, match="unresolved actions"):
        load_recovery_state(run_dir)


def test_pending_action_reconciliation_is_explicit_and_idempotent(tmp_path: Path) -> None:
    run_dir = tmp_path / "reconcile-run"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(
            run_id="reconcile-run",
            world_revision=2,
            event_offset=5,
            feedback_offset=1,
            pending_actions=[
                {"tool_call_id": "call-1", "status": "running"},
                {"tool_call_id": "call-2", "status": "running"},
            ],
        )
    )
    ProgressLedger(
        last_action={"tool_call_id": "call-2", "status": "running"},
        active_actions={
            "call-1": {"tool_call_id": "call-1", "status": "running"},
            "call-2": {"tool_call_id": "call-2", "status": "running"},
        },
    ).write(run_dir / "progress.json")

    outcomes = {"call-1": "succeeded", "call-2": "unknown"}
    first = reconcile_pending_actions(run_dir, outcomes=outcomes)
    checkpoint = FileCheckpointStore(run_dir / "checkpoint.json").load()
    _, progress = load_recovery_state(run_dir)
    second = reconcile_pending_actions(run_dir, outcomes=outcomes)

    assert first.outcomes == second.outcomes == outcomes
    assert checkpoint.pending_actions == []
    assert progress.last_action["status"] == "unknown"
    assert progress.execution_succeeded == 1
    assert progress.reconciled_actions == outcomes
    assert (run_dir / "action.reconciliation.json").is_file()


def test_reconciliation_retry_after_checkpoint_write_failure_is_idempotent(
    tmp_path: Path, monkeypatch
) -> None:
    run_dir = tmp_path / "reconcile-retry"
    run_dir.mkdir()
    store = FileCheckpointStore(run_dir / "checkpoint.json")
    store.save(
        RunCheckpoint(
            run_id="reconcile-retry",
            world_revision=0,
            event_offset=1,
            feedback_offset=0,
            pending_actions=[{"tool_call_id": "call-2", "status": "running"}],
        )
    )
    ProgressLedger(last_action={"tool_call_id": "call-2", "status": "running"}).write(
        run_dir / "progress.json"
    )
    real_save = FileCheckpointStore.save
    fail_once = True

    def injected_save(self, checkpoint):
        nonlocal fail_once
        if fail_once:
            fail_once = False
            raise OSError("injected checkpoint failure")
        real_save(self, checkpoint)

    monkeypatch.setattr(FileCheckpointStore, "save", injected_save)
    with pytest.raises(OSError, match="injected"):
        reconcile_pending_actions(run_dir, outcomes={"call-2": "succeeded"})

    after_failure = ProgressLedger.model_validate_json(
        (run_dir / "progress.json").read_text(encoding="utf-8")
    )
    assert after_failure.execution_succeeded == 1
    assert store.load().pending_actions

    reconcile_pending_actions(run_dir, outcomes={"call-2": "succeeded"})
    checkpoint, after_retry = load_recovery_state(run_dir)
    assert checkpoint.pending_actions == []
    assert after_retry.execution_succeeded == 1


def test_recovery_rejects_stale_world_revision(tmp_path: Path) -> None:
    run_dir = tmp_path / "revision-run"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(run_id="revision-run", world_revision=3, event_offset=0, feedback_offset=0)
    )
    ProgressLedger().write(run_dir / "progress.json")
    with pytest.raises(ValueError, match="world revision mismatch"):
        load_recovery_state(run_dir, current_world_revision=4)
    record, _ = load_recovery_state(run_dir, current_world_revision=3)
    assert record.world_revision == 3
