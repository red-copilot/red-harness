import asyncio
from pathlib import Path

import pytest
from pydantic import ValidationError

from harness.progress import ProgressLedger
from harness.runtime.checkpoint import (
    FileCheckpointStore,
    RunCheckpoint,
    load_recovery_state,
    save_session_checkpoint,
)
from harness.session import AgentCheckpoint


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
    result = asyncio.run(save_session_checkpoint(
        run_dir=run_dir,
        session=Session(),
        world_revision=4,
        progress=ledger,
        plan_revision=3,
    ))
    assert result is not None
    assert result.session_checkpoint_id == "cp-3"
    assert result.pending_actions[0]["tool_call_id"] == "a"
    assert FileCheckpointStore(run_dir / "checkpoint.json").load() == result


def test_legacy_session_without_checkpoint(tmp_path: Path) -> None:
    result = asyncio.run(save_session_checkpoint(
        run_dir=tmp_path, session=object(), world_revision=0,
        progress=ProgressLedger(),
    ))
    assert result is None


def test_recovery_restores_progress(tmp_path: Path) -> None:
    run_dir = tmp_path / "safe-run"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(run_id="safe-run", world_revision=2, event_offset=19)
    )
    ProgressLedger(verified_actions=2).write(run_dir / "progress.json")
    checkpoint, progress = load_recovery_state(run_dir)
    assert checkpoint.event_offset == 19
    assert progress.verified_actions == 2


def test_recovery_refuses_unresolved_actions(tmp_path: Path) -> None:
    run_dir = tmp_path / "interrupted-run"
    run_dir.mkdir()
    FileCheckpointStore(run_dir / "checkpoint.json").save(
        RunCheckpoint(run_id="interrupted-run", world_revision=1,
                      pending_actions=[{"tool_call_id": "call-1", "status": "running"}])
    )
    ProgressLedger().write(run_dir / "progress.json")
    with pytest.raises(RuntimeError, match="unresolved actions"):
        load_recovery_state(run_dir)
