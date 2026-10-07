from __future__ import annotations

import time
from pathlib import Path

from harness.coordination import CoordinationStore, WorkItemSpec


def test_coordination_claim_complete_and_release(tmp_path: Path) -> None:
    store = CoordinationStore(tmp_path / "coordination.db")
    first = store.submit(
        WorkItemSpec(
            run_id="run-1",
            description="high priority",
            priority=0.9,
            goal_id="goal-1",
        )
    )
    second = store.submit(
        WorkItemSpec(
            run_id="run-1",
            description="low priority",
            priority=0.2,
        )
    )

    claimed = store.claim("run-1", "agent-a", lease_seconds=60)
    assert claimed is not None
    assert claimed["id"] == first["id"]
    assert claimed["lease_owner"] == "agent-a"

    assert store.release(first["id"], "agent-a") is True
    reclaimed = store.claim("run-1", "agent-b", lease_seconds=60)
    assert reclaimed is not None
    assert reclaimed["id"] == first["id"]

    assert store.complete(first["id"], "agent-b", {"ok": True}) is True
    next_item = store.claim("run-1", "agent-c", lease_seconds=60)
    assert next_item is not None
    assert next_item["id"] == second["id"]


def test_coordination_reclaims_expired_lease(tmp_path: Path) -> None:
    store = CoordinationStore(tmp_path / "coordination.db")
    item = store.submit(
        WorkItemSpec(run_id="run-1", description="reclaim me", priority=0.5)
    )

    claimed = store.claim("run-1", "agent-a", lease_seconds=1)
    assert claimed is not None
    time.sleep(1.05)

    reclaimed = store.claim("run-1", "agent-b", lease_seconds=60)
    assert reclaimed is not None
    assert reclaimed["id"] == item["id"]
    assert reclaimed["lease_owner"] == "agent-b"
    assert reclaimed["attempts"] == 2


def test_coordination_fail_can_requeue(tmp_path: Path) -> None:
    store = CoordinationStore(tmp_path / "coordination.db")
    item = store.submit(
        WorkItemSpec(run_id="run-1", description="retryable")
    )
    claimed = store.claim("run-1", "agent-a", lease_seconds=60)
    assert claimed is not None

    assert store.fail(item["id"], "agent-a", "transient", requeue=True) is True
    queued = store.get(item["id"])
    assert queued is not None
    assert queued["state"] == "queued"
    assert queued["lease_owner"] is None
