import asyncio
import errno
import json
import os
import stat
import threading
from pathlib import Path
from typing import ClassVar

import pytest
from pydantic import ValidationError

from harness.progress import ProgressLedger
from harness.runtime.checkpoint import (
    ActionReconciliation,
    FileCheckpointStore,
    RunCheckpoint,
    load_recovery_state,
    reconcile_pending_actions,
    run_action_owner,
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
        pending_actions=[{"tool_call_id": "tool-1", "status": "unknown"}],
        agent_session_id="session-a",
        plan_revision=2,
    )
    store.save(initial)
    assert store.load() == initial
    store.save(initial.model_copy(update={"event_offset": 20}))
    assert store.load().event_offset == 20


def test_checkpoint_refuses_unrecoverable_oversized_payload_before_replacing(
    tmp_path: Path,
) -> None:
    store = FileCheckpointStore(tmp_path / "checkpoint.json")
    initial = RunCheckpoint(
        run_id="bounded-checkpoint",
        world_revision=0,
        event_offset=0,
        feedback_offset=0,
    )
    store.save(initial)
    original = store.path.read_bytes()
    large = initial.model_copy(
        update={
            "processed_event_ids": {
                f"event-{index:06d}": "a" * 64 for index in range(100_000)
            }
        }
    )

    with pytest.raises(ValueError, match="8 MiB recovery limit"):
        store.save(large)

    assert store.path.read_bytes() == original
    assert store.load() == initial
    assert not list(tmp_path.glob(".checkpoint.json.*.tmp"))


def test_session_checkpoint_keeps_recent_event_dedupe_ids_within_read_limit(
    tmp_path: Path,
) -> None:
    class Session:
        async def checkpoint(self):
            return AgentCheckpoint(
                id="checkpoint-large-map",
                event_offset=100_000,
                feedback_offset=0,
            )

    class Progress:
        active_actions: ClassVar[dict] = {}
        last_action = None
        event_count = 100_000

    processed = {
        f"agent-event-{index:06d}": "a" * 64 for index in range(100_000)
    }
    record = asyncio.run(
        save_session_checkpoint(
            run_dir=tmp_path,
            session=Session(),
            world_revision=1,
            progress=Progress(),
            processed_event_ids=processed,
        )
    )

    assert record is not None
    assert len(record.processed_event_ids) < len(processed)
    assert "agent-event-099999" in record.processed_event_ids
    assert "agent-event-000000" not in record.processed_event_ids
    assert FileCheckpointStore(tmp_path / "checkpoint.json").load() == record
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


def test_checkpoint_disk_full_during_sync_preserves_previous_record(
    tmp_path: Path, monkeypatch
) -> None:
    store = FileCheckpointStore(tmp_path / "checkpoint.json")
    initial = RunCheckpoint(run_id="run-a", world_revision=1, event_offset=3, feedback_offset=0)
    store.save(initial)

    def disk_full(_descriptor: int) -> None:
        raise OSError(errno.ENOSPC, "injected disk full")

    monkeypatch.setattr("harness.runtime.checkpoint.os.fsync", disk_full)
    with pytest.raises(OSError) as error:
        store.save(initial.model_copy(update={"event_offset": 4}))

    assert error.value.errno == errno.ENOSPC
    assert store.load() == initial
    assert not list(tmp_path.glob("*.tmp"))


def test_checkpoint_syncs_file_and_directory_entries(tmp_path: Path, monkeypatch) -> None:
    synced_modes = []
    real_fsync = os.fsync

    def record_fsync(descriptor: int) -> None:
        synced_modes.append(os.fstat(descriptor).st_mode)
        real_fsync(descriptor)

    monkeypatch.setattr("harness.runtime.checkpoint.os.fsync", record_fsync)
    FileCheckpointStore(tmp_path / "checkpoint.json").save(
        RunCheckpoint(run_id="run-fsync", world_revision=0, event_offset=0, feedback_offset=0)
    )

    assert any(stat.S_ISREG(mode) for mode in synced_modes)
    assert any(stat.S_ISDIR(mode) for mode in synced_modes)


def test_progress_ledger_write_is_atomic_on_replace_failure(tmp_path: Path, monkeypatch) -> None:
    path = tmp_path / "progress.json"
    prior = ProgressLedger(verified_actions=1)
    prior.write(path)
    original = path.read_bytes()

    def fail_replace(*_args, **_kwargs):
        raise OSError("injected progress replace failure")

    monkeypatch.setattr("harness.progress.os.replace", fail_replace)
    with pytest.raises(OSError, match="injected progress"):
        ProgressLedger(verified_actions=2).write(path)

    assert path.read_bytes() == original
    assert ProgressLedger.model_validate_json(path.read_bytes()).verified_actions == 1
    assert not list(tmp_path.glob(".progress.json.*.tmp"))


def test_checkpoint_rejects_negative_cursors() -> None:
    with pytest.raises(ValidationError):
        RunCheckpoint(run_id="run-a", world_revision=0, event_offset=-1)


@pytest.mark.parametrize(
    "values",
    [
        {"schema_version": "harness/checkpoint/v0"},
        {"budget_used": {"tokens": float("nan")}},
        {"budget_used": {"tokens": -1}},
        {"budget_used": {"tokens": "12"}},
        {"budget_used": {"tokens": True}},
        {"budget_used": {"total_tokens": 3.0}},
        {"budget_used": {"tool_calls": 3.9}},
        {
            "pending_actions": [
                {"tool_call_id": "same", "status": "running"},
                {"tool_call_id": "same", "status": "running"},
            ]
        },
    ],
)
def test_checkpoint_rejects_invalid_recovery_fields(values: dict) -> None:
    with pytest.raises(ValidationError):
        RunCheckpoint.model_validate(
            {
                "run_id": "run-a",
                "world_revision": 0,
                "event_offset": 0,
                "feedback_offset": 0,
                **values,
            }
        )


def test_checkpoint_preserves_unidentified_inflight_action() -> None:
    checkpoint = RunCheckpoint(
        run_id="run-a",
        world_revision=0,
        event_offset=0,
        feedback_offset=0,
        pending_actions=[{"status": "running", "tool": "file.write"}],
    )
    assert checkpoint.pending_actions[0]["tool"] == "file.write"


def test_checkpoint_v1_pending_action_id_migrates_to_v2(tmp_path: Path) -> None:
    path = tmp_path / "checkpoint.json"
    path.write_text(
        json.dumps(
            {
                "schema_version": "harness/checkpoint/v1",
                "run_id": "legacy-run",
                "world_revision": 1,
                "event_offset": 2,
                "feedback_offset": 0,
                "pending_actions": [{"id": "legacy-call", "status": "running"}],
            }
        ),
        encoding="utf-8",
    )

    migrated = FileCheckpointStore(path).load()

    assert migrated.schema_version == "harness/checkpoint/v2"
    assert migrated.pending_actions[0]["tool_call_id"] == "legacy-call"


def test_completed_session_snapshot(tmp_path: Path) -> None:
    class Session:
        async def checkpoint(self):
            return AgentCheckpoint(
                id="cp-3",
                event_offset=24,
                feedback_offset=9,
                session_id="pi-session-3",
                gateway_event_offset=31,
            )

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
    assert result.agent_session_id == "pi-session-3"
    assert result.gateway_event_offset == 31
    assert result.pending_actions[0]["tool_call_id"] == "a"
    assert result.budget_used == {"total_tokens": 20, "cost_usd": 0.03}
    assert result.processed_event_ids == {"event-1": "hash-1"}
    assert FileCheckpointStore(run_dir / "checkpoint.json").load() == result


def test_live_checkpoint_captures_session_budget_usage(tmp_path: Path) -> None:
    class Session:
        @property
        def usage_metrics(self):
            return {"total_tokens": 17, "tool_calls": 2, "cost_usd": 0.03}

        async def checkpoint(self):
            return AgentCheckpoint(id="cp-live", event_offset=3, feedback_offset=1)

    run_dir = tmp_path / "live-budget"
    run_dir.mkdir()
    result = asyncio.run(
        save_session_checkpoint(
            run_dir=run_dir,
            session=Session(),
            world_revision=2,
            progress=ProgressLedger(),
        )
    )
    assert result.budget_used == {"total_tokens": 17, "tool_calls": 2, "cost_usd": 0.03}


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


@pytest.mark.parametrize("ledger_fields", [
    {"active_actions": {"call-1": {"tool_call_id": "call-1", "status": "running"}}},
    {"last_action": {"tool_call_id": "call-1", "status": "running"}},
])
def test_recovery_refuses_action_in_progress_ledger_ahead_of_checkpoint(
    tmp_path: Path, ledger_fields: dict
) -> None:
    run_dir = tmp_path / "stale-checkpoint"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(run_id=run_dir.name, world_revision=0, event_offset=0, feedback_offset=0)
    )
    ProgressLedger(**ledger_fields).write(run_dir / "progress.json")

    with pytest.raises(RuntimeError, match="progress ledger"):
        load_recovery_state(run_dir)


def test_recovery_refuses_progress_event_ahead_of_checkpoint(tmp_path: Path) -> None:
    run_dir = tmp_path / "event-ledger-ahead"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(
            run_id=run_dir.name,
            world_revision=0,
            event_offset=12,
            feedback_offset=0,
            processed_event_ids={"evt-1": "hash-1"},
            progress_event_count=0,
        )
    )
    ProgressLedger(last_event_type="progress.updated", event_count=1).write(
        run_dir / "progress.json"
    )

    with pytest.raises(RuntimeError, match="event counts do not match"):
        load_recovery_state(run_dir)


def test_recovery_rejects_corrupt_and_wrong_run_checkpoints(tmp_path: Path) -> None:
    run_dir = tmp_path / "validation-run"
    run_dir.mkdir()
    checkpoint_path = run_dir / "checkpoint.json"
    checkpoint_path.write_text("{not-json", encoding="utf-8")
    with pytest.raises(ValueError):
        load_recovery_state(run_dir)

    FileCheckpointStore(checkpoint_path).save(
        RunCheckpoint(run_id="another-run", world_revision=0, event_offset=0, feedback_offset=0)
    )
    with pytest.raises(ValueError, match="run ID mismatch"):
        load_recovery_state(run_dir)


def test_recovery_rejects_symlinked_checkpoint_and_progress(tmp_path: Path) -> None:
    run_dir = tmp_path / "symlink-run"
    run_dir.mkdir()
    outside = tmp_path / "outside.json"
    outside.write_text("{}", encoding="utf-8")
    (run_dir / "checkpoint.json").symlink_to(outside)
    with pytest.raises(ValueError, match="regular file"):
        FileCheckpointStore(run_dir / "checkpoint.json").load()

    (run_dir / "checkpoint.json").unlink()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(run_id="symlink-run", world_revision=0, event_offset=0, feedback_offset=0)
    )
    (run_dir / "progress.json").symlink_to(outside)
    with pytest.raises(OSError):
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
    with pytest.raises(RuntimeError, match="unknown action effects"):
        load_recovery_state(run_dir)
    assert checkpoint.pending_actions == [
        {"tool_call_id": "call-2", "status": "unknown"}
    ]
    second = reconcile_pending_actions(run_dir, outcomes={"call-2": "unknown"})
    resolved = reconcile_pending_actions(run_dir, outcomes={"call-2": "succeeded"})
    _, progress = load_recovery_state(run_dir)

    assert first.outcomes == outcomes
    assert second.outcomes == {"call-2": "unknown"}
    assert resolved.outcomes == {"call-2": "succeeded"}
    assert FileCheckpointStore(run_dir / "checkpoint.json").load().pending_actions == []
    assert progress.last_action["status"] == "succeeded"
    assert progress.execution_succeeded == 2
    assert progress.verified_actions == 0
    assert progress.reconciled_actions == {"call-1": "succeeded", "call-2": "succeeded"}
    assert (run_dir / "action.reconciliation.json").is_file()


def test_pending_action_reconciliation_refuses_terminal_run_without_mutation(
    tmp_path: Path,
) -> None:
    run_dir = tmp_path / "reconcile-terminal"
    run_dir.mkdir()
    checkpoint_path = run_dir / "checkpoint.json"
    progress_path = run_dir / "progress.json"
    FileCheckpointStore(checkpoint_path).save(
        RunCheckpoint(
            run_id=run_dir.name,
            world_revision=0,
            event_offset=1,
            feedback_offset=0,
            pending_actions=[{"tool_call_id": "call-final", "status": "running"}],
        )
    )
    ProgressLedger(
        last_action={"tool_call_id": "call-final", "status": "running"},
        active_actions={"call-final": {"tool_call_id": "call-final", "status": "running"}},
    ).write(progress_path)
    checkpoint_before = checkpoint_path.read_bytes()
    progress_before = progress_path.read_bytes()
    (run_dir / "result.json").write_text('{"status":"success"}\n', encoding="utf-8")

    with pytest.raises(ValueError, match="terminal runs cannot be reconciled"):
        reconcile_pending_actions(run_dir, outcomes={"call-final": "succeeded"})

    assert checkpoint_path.read_bytes() == checkpoint_before
    assert progress_path.read_bytes() == progress_before
    assert not (run_dir / "action.reconciliation.json").exists()


@pytest.mark.skipif(os.name != "posix", reason="POSIX file locking is required")
def test_operator_reconciliation_refuses_while_agent_owns_run(tmp_path: Path) -> None:
    run_dir = tmp_path / "reconcile-active-owner"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(
            run_id=run_dir.name,
            world_revision=0,
            event_offset=1,
            feedback_offset=0,
            pending_actions=[{"tool_call_id": "call-live", "status": "running"}],
        )
    )
    ProgressLedger(last_action={"tool_call_id": "call-live", "status": "running"}).write(
        run_dir / "progress.json"
    )
    checkpoint_before = (run_dir / "checkpoint.json").read_bytes()
    progress_before = (run_dir / "progress.json").read_bytes()
    owner_entered = threading.Event()
    release_owner = threading.Event()

    def own_run() -> None:
        with run_action_owner(run_dir):
            owner_entered.set()
            assert release_owner.wait(timeout=5)

    owner_thread = threading.Thread(target=own_run)
    owner_thread.start()
    assert owner_entered.wait(timeout=5)
    try:
        with pytest.raises(RuntimeError, match="active action owner"):
            reconcile_pending_actions(run_dir, outcomes={"call-live": "unknown"})
    finally:
        release_owner.set()
        owner_thread.join(timeout=5)

    assert not owner_thread.is_alive()
    assert (run_dir / "checkpoint.json").read_bytes() == checkpoint_before
    assert (run_dir / "progress.json").read_bytes() == progress_before
    assert not (run_dir / "action.reconciliation.json").exists()


def test_reconciliation_refuses_symlinked_operator_report(tmp_path: Path) -> None:
    run_dir = tmp_path / "reconcile-report-symlink"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(
            run_id=run_dir.name,
            world_revision=0,
            event_offset=0,
            feedback_offset=0,
        )
    )
    ProgressLedger(reconciled_actions={"call-1": "succeeded"}).write(
        run_dir / "progress.json"
    )
    outside = tmp_path / "outside-reconciliation.json"
    report_payload = (
        ActionReconciliation(
            run_id=run_dir.name, outcomes={"call-1": "succeeded"}
        ).model_dump_json()
    )
    outside.write_text(report_payload, encoding="utf-8")
    (run_dir / "action.reconciliation.json").symlink_to(outside)

    with pytest.raises(OSError):
        reconcile_pending_actions(run_dir, outcomes={"call-1": "succeeded"})

    assert outside.read_text(encoding="utf-8") == report_payload


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


def test_reconciliation_retry_after_report_write_failure_is_idempotent(
    tmp_path: Path, monkeypatch
) -> None:
    import harness.runtime.checkpoint as checkpoint_module

    run_dir = tmp_path / "reconcile-report-retry"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(
            run_id=run_dir.name,
            world_revision=0,
            event_offset=1,
            feedback_offset=0,
            pending_actions=[{"tool_call_id": "call-3", "status": "running"}],
        )
    )
    ProgressLedger(last_action={"tool_call_id": "call-3", "status": "running"}).write(
        run_dir / "progress.json"
    )
    real_write_report = checkpoint_module._write_reconciliation_report
    fail_once = True

    def injected_write_report(path, reconciliation):
        nonlocal fail_once
        if path.name == "action.reconciliation.json" and fail_once:
            fail_once = False
            raise OSError("injected report failure")
        return real_write_report(path, reconciliation)

    monkeypatch.setattr(
        checkpoint_module, "_write_reconciliation_report", injected_write_report
    )
    outcomes = {"call-3": "failed"}
    with pytest.raises(OSError, match="injected report failure"):
        reconcile_pending_actions(run_dir, outcomes=outcomes)

    retry = reconcile_pending_actions(run_dir, outcomes=outcomes)
    checkpoint, progress = load_recovery_state(run_dir)
    assert retry.outcomes == outcomes
    assert checkpoint.pending_actions == []
    assert progress.failure_count == 1
    assert progress.reconciled_actions == outcomes
    assert (run_dir / "action.reconciliation.json").is_file()


@pytest.mark.skipif(os.name != "posix", reason="POSIX file locking is required")
def test_concurrent_reconciliation_serializes_conflicting_operator_outcomes(
    tmp_path: Path, monkeypatch
) -> None:
    run_dir = tmp_path / "reconcile-concurrent"
    run_dir.mkdir()
    store = FileCheckpointStore(run_dir / "checkpoint.json")
    store.save(
        RunCheckpoint(
            run_id=run_dir.name,
            world_revision=0,
            event_offset=1,
            feedback_offset=0,
            pending_actions=[{"tool_call_id": "call-race", "status": "running"}],
        )
    )
    ProgressLedger(last_action={"tool_call_id": "call-race", "status": "running"}).write(
        run_dir / "progress.json"
    )

    first_loaded = threading.Event()
    second_loaded = threading.Event()
    release_first = threading.Event()
    load_lock = threading.Lock()
    load_count = 0
    original_load = FileCheckpointStore.load

    def delayed_load(self):
        nonlocal load_count
        if self.path.name == "checkpoint.json":
            with load_lock:
                load_count += 1
                current = load_count
            if current == 1:
                first_loaded.set()
                assert release_first.wait(timeout=5)
            elif current == 2:
                second_loaded.set()
        return original_load(self)

    monkeypatch.setattr(FileCheckpointStore, "load", delayed_load)
    results: list[str] = []

    def reconcile(outcome: str) -> None:
        try:
            reconcile_pending_actions(run_dir, outcomes={"call-race": outcome})
            results.append(outcome)
        except (RuntimeError, ValueError):
            results.append("conflict")

    first = threading.Thread(target=reconcile, args=("succeeded",))
    second = threading.Thread(target=reconcile, args=("failed",))
    first.start()
    assert first_loaded.wait(timeout=5)
    second.start()
    assert not second_loaded.wait(timeout=0.1)
    release_first.set()
    first.join(timeout=5)
    second.join(timeout=5)

    assert not first.is_alive() and not second.is_alive()
    assert sorted(results) == ["conflict", "succeeded"]
    checkpoint, progress = load_recovery_state(run_dir)
    report = ActionReconciliation.model_validate_json(
        (run_dir / "action.reconciliation.json").read_text(encoding="utf-8")
    )
    assert checkpoint.pending_actions == []
    assert progress.reconciled_actions == {"call-race": "succeeded"}
    assert progress.execution_succeeded == 1
    assert report.outcomes == {"call-race": "succeeded"}


@pytest.mark.skipif(os.name != "posix", reason="POSIX file locking is required")
def test_recovery_waits_for_reconciliation_to_publish_all_records(
    tmp_path: Path, monkeypatch
) -> None:
    run_dir = tmp_path / "reconcile-recovery-race"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(
            run_id=run_dir.name,
            world_revision=0,
            event_offset=1,
            feedback_offset=0,
            pending_actions=[{"tool_call_id": "call-race", "status": "running"}],
        )
    )
    ProgressLedger(last_action={"tool_call_id": "call-race", "status": "running"}).write(
        run_dir / "progress.json"
    )

    progress_published = threading.Event()
    release_reconciler = threading.Event()
    recovery_finished = threading.Event()
    original_write = ProgressLedger.write
    recovery: list[tuple] = []

    def delayed_write(self, path):
        original_write(self, path)
        if Path(path).name == "progress.json":
            progress_published.set()
            assert release_reconciler.wait(timeout=5)

    def reconcile() -> None:
        reconcile_pending_actions(run_dir, outcomes={"call-race": "succeeded"})

    def recover() -> None:
        recovery.append(load_recovery_state(run_dir))
        recovery_finished.set()

    monkeypatch.setattr(ProgressLedger, "write", delayed_write)
    reconciliation_thread = threading.Thread(target=reconcile)
    reconciliation_thread.start()
    assert progress_published.wait(timeout=5)

    recovery_thread = threading.Thread(target=recover)
    recovery_thread.start()
    assert not recovery_finished.wait(timeout=0.1)

    release_reconciler.set()
    reconciliation_thread.join(timeout=5)
    recovery_thread.join(timeout=5)

    assert not reconciliation_thread.is_alive() and not recovery_thread.is_alive()
    assert recovery_finished.is_set()
    checkpoint, progress = recovery[0]
    assert checkpoint.pending_actions == []
    assert progress.reconciled_actions == {"call-race": "succeeded"}


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
