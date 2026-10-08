from pathlib import Path

import pytest
from pydantic import ValidationError

from harness.runtime.checkpoint import FileCheckpointStore, RunCheckpoint


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
