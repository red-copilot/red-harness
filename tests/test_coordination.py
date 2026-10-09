from __future__ import annotations

import sqlite3
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from threading import Barrier

import pytest

from harness.coordination import CoordinationStore, WorkItemSpec


def test_concurrent_coordination_initialization_is_safe(tmp_path: Path) -> None:
    path = tmp_path / "parallel-init.db"
    barrier = Barrier(12)

    def initialize(_index: int) -> CoordinationStore:
        barrier.wait(timeout=5)
        return CoordinationStore(path)

    with ThreadPoolExecutor(max_workers=12) as pool:
        stores = list(pool.map(initialize, range(12)))

    item = stores[0].submit(WorkItemSpec(run_id="run-init", description="ready"))
    claimed = stores[-1].claim("run-init", "agent-init", lease_seconds=30)
    assert claimed is not None
    assert claimed["id"] == item["id"]


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


def test_coordination_reclaims_lease_at_exact_expiration_time(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    import harness.coordination as coordination_module

    store = CoordinationStore(tmp_path / "exact-expiration.db")
    item = store.submit(WorkItemSpec(run_id="run-1", description="exact expiry"))
    claimed = store.claim("run-1", "agent-old", lease_seconds=10)
    assert claimed is not None
    expiration = claimed["lease_expires_at"]
    monkeypatch.setattr(coordination_module.time, "time", lambda: expiration)

    reclaimed = store.claim("run-1", "agent-new", lease_seconds=10)

    assert reclaimed is not None
    assert reclaimed["id"] == item["id"]
    assert reclaimed["lease_owner"] == "agent-new"


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


@pytest.mark.parametrize("operation", ["heartbeat", "complete", "fail", "release"])
def test_expired_coordination_owner_cannot_mutate_work(
    tmp_path: Path, operation: str
) -> None:
    path = tmp_path / f"{operation}.db"
    store = CoordinationStore(path)
    item = store.submit(WorkItemSpec(run_id="run-1", description="lease guarded"))
    claimed = store.claim("run-1", "agent-stale", lease_seconds=60)
    assert claimed is not None
    with sqlite3.connect(path) as db:
        db.execute(
            "UPDATE work_items SET lease_expires_at=? WHERE id=?",
            (time.time() - 1, item["id"]),
        )

    if operation == "heartbeat":
        accepted = store.heartbeat(item["id"], "agent-stale", lease_seconds=60)
    elif operation == "complete":
        accepted = store.complete(item["id"], "agent-stale", {"done": True})
    elif operation == "fail":
        accepted = store.fail(item["id"], "agent-stale", "stale", requeue=True)
    else:
        accepted = store.release(item["id"], "agent-stale")

    assert accepted is False
    current = store.get(item["id"])
    assert current is not None
    assert current["state"] == "running"
    assert current["lease_owner"] == "agent-stale"
    assert current["result"] is None
    assert current["error"] is None

    reclaimed = store.claim("run-1", "agent-current", lease_seconds=60)
    assert reclaimed is not None
    assert reclaimed["id"] == item["id"]
    assert reclaimed["lease_owner"] == "agent-current"
