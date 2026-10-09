import sqlite3
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from threading import Barrier

import pytest
from pydantic import ValidationError

from harness.queue import JobPayload, SQLiteQueue


def test_job_payload_limits_path_and_gateway_configuration_lengths() -> None:
    with pytest.raises(ValidationError):
        JobPayload(task_path="t" * 4097, agent_path="agent.yaml")
    with pytest.raises(ValidationError):
        JobPayload(
            task_path="task.yaml",
            agent_path="agent.yaml",
            gateway={"mode": "unsupported"},
        )


def test_queue_claim_heartbeat_complete_and_leaderboard(tmp_path: Path) -> None:
    queue = SQLiteQueue(tmp_path / "queue.db")
    submitted = queue.submit(
        JobPayload(
            task_path="benchmarks/examples/hello/task.yaml",
            agent_path="agents/examples/demo.yaml",
        )
    )
    assert submitted["run_id"].startswith("run_")

    claimed = queue.claim("worker-a", lease_seconds=60)
    assert claimed is not None
    assert claimed["id"] == submitted["id"]
    assert claimed["run_id"] == submitted["run_id"]
    assert claimed["state"] == "running"
    assert claimed["attempts"] == 1
    assert queue.heartbeat(claimed["id"], "worker-a", lease_seconds=60)
    assert not queue.heartbeat(claimed["id"], "worker-b", lease_seconds=60)

    result = {
        "agent_id": "demo-agent",
        "success": True,
        "score": 100,
        "metrics": {"duration_ms": 250, "cost_usd": 0.1},
    }
    assert queue.complete(claimed["id"], "worker-a", result)
    assert queue.complete(claimed["id"], "worker-a", result)
    assert not queue.complete(claimed["id"], "worker-a", {"success": False})
    assert queue.get(claimed["id"])["state"] == "completed"

    board = queue.leaderboard()
    assert board == [
        {
            "agent_id": "demo-agent",
            "runs": 1,
            "success_rate": 1.0,
            "mean_score": 100.0,
            "median_duration_ms": 250.0,
            "total_cost_usd": 0.1,
        }
    ]


@pytest.mark.parametrize(
    "result",
    [
        {"success": "yes", "score": 100},
        {"success": True, "score": "100"},
        {"success": True, "score": float("nan")},
        {"success": True, "score": 100, "metrics": []},
        {"success": True, "score": 100, "metrics": {"cost_usd": -1}},
        {"success": True, "score": 100, "metrics": {"duration_ms": float("inf")}},
    ],
)
def test_queue_rejects_malformed_terminal_results_before_state_change(
    tmp_path: Path, result: dict
) -> None:
    queue = SQLiteQueue(tmp_path / "queue.db")
    job = queue.submit(JobPayload(task_path="task.yaml", agent_path="agent.yaml"))
    claimed = queue.claim("worker-a")
    assert claimed is not None

    with pytest.raises(ValueError, match="terminal result"):
        queue.complete(job["id"], "worker-a", result)

    current = queue.get(job["id"])
    assert current["state"] == "running"
    assert current["result"] is None


def test_queue_leaderboard_ignores_corrupt_legacy_result_rows(tmp_path: Path) -> None:
    queue = SQLiteQueue(tmp_path / "queue.db")
    with sqlite3.connect(queue.path) as db:
        db.execute(
            "INSERT INTO jobs(id, run_id, state, payload_json, result_json, created_at, updated_at) "
            "VALUES ('job_bad', 'run_bad', 'completed', '{}', ?, 1, 1)",
            ('{"agent_id":"bad","success":true,"score":"not-a-number"}',),
        )
    assert queue.leaderboard() == []


def test_expired_lease_can_be_reclaimed(tmp_path: Path) -> None:
    queue = SQLiteQueue(tmp_path / "queue.db")
    queue.submit(JobPayload(task_path="task.yaml", agent_path="agent.yaml"))

    first = queue.claim("worker-a", lease_seconds=10)
    assert first is not None
    with sqlite3.connect(tmp_path / "queue.db") as db:
        db.execute(
            "UPDATE jobs SET lease_expires_at=? WHERE id=?",
            (time.time() - 1, first["id"]),
        )
    assert queue.claim("worker-b", lease_seconds=60) is None
    needs_review = queue.get(first["id"])
    assert needs_review["state"] == "reconciliation_required"
    assert queue.requeue(first["id"])
    second = queue.claim("worker-b", lease_seconds=60)
    assert second is not None
    assert second["id"] == first["id"]
    assert second["lease_owner"] == "worker-b"
    assert second["attempts"] == 2


def test_queue_expires_lease_at_exact_expiration_time(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    import harness.queue as queue_module

    queue = SQLiteQueue(tmp_path / "exact-expiration.db")
    job = queue.submit(JobPayload(task_path="task.yaml", agent_path="agent.yaml"))
    claimed = queue.claim("worker-a", lease_seconds=10)
    assert claimed is not None
    monkeypatch.setattr(queue_module.time, "time", lambda: claimed["lease_expires_at"])

    assert queue.claim("worker-b", lease_seconds=10) is None
    assert queue.get(job["id"])["state"] == "reconciliation_required"


def test_expired_worker_cannot_heartbeat_complete_or_fail(tmp_path: Path) -> None:
    queue = SQLiteQueue(tmp_path / "queue.db")
    job = queue.submit(JobPayload(task_path="task.yaml", agent_path="agent.yaml"))
    claimed = queue.claim("worker-a", lease_seconds=10)
    assert claimed is not None
    with sqlite3.connect(tmp_path / "queue.db") as db:
        db.execute(
            "UPDATE jobs SET lease_expires_at=? WHERE id=?",
            (time.time() - 1, job["id"]),
        )

    assert not queue.heartbeat(job["id"], "worker-a", lease_seconds=60)
    assert not queue.complete(job["id"], "worker-a", {"success": True})
    assert not queue.fail(job["id"], "worker-a", "stale worker")
    assert queue.get(job["id"])["state"] == "running"


def test_concurrent_queue_claims_do_not_dispatch_one_job_twice(tmp_path: Path) -> None:
    queue = SQLiteQueue(tmp_path / "queue.db")
    for index in range(8):
        queue.submit(
            JobPayload(task_path=f"task-{index}.yaml", agent_path="agent.yaml")
        )

    def claim(index: int):
        return queue.claim(f"worker-{index}")

    with ThreadPoolExecutor(max_workers=8) as pool:
        claims = list(pool.map(claim, range(8)))

    ids = [item["id"] for item in claims if item is not None]
    assert len(ids) == len(set(ids)) == 8


def test_concurrent_queue_initialization_safely_creates_new_database(tmp_path: Path) -> None:
    path = tmp_path / "parallel-new.db"
    barrier = Barrier(12)

    def initialize(_index: int) -> SQLiteQueue:
        barrier.wait(timeout=5)
        return SQLiteQueue(path)

    with ThreadPoolExecutor(max_workers=12) as pool:
        queues = list(pool.map(initialize, range(12)))

    job = queues[0].submit(JobPayload(task_path="task.yaml", agent_path="agent.yaml"))
    claimed = queues[-1].claim("worker-after-startup")
    assert claimed is not None
    assert claimed["id"] == job["id"]


def test_queue_migrates_legacy_jobs_to_stable_run_ownership(tmp_path: Path) -> None:
    path = tmp_path / "legacy.db"
    with sqlite3.connect(path) as db:
        db.execute(
            """
            CREATE TABLE jobs (
                id TEXT PRIMARY KEY,
                state TEXT NOT NULL,
                payload_json TEXT NOT NULL,
                result_json TEXT,
                error TEXT,
                lease_owner TEXT,
                lease_expires_at REAL,
                attempts INTEGER NOT NULL DEFAULT 0,
                created_at REAL NOT NULL,
                updated_at REAL NOT NULL
            )
            """
        )
        now = time.time()
        payload = JobPayload(task_path="task.yaml", agent_path="agent.yaml")
        db.execute(
            "INSERT INTO jobs(id, state, payload_json, created_at, updated_at) "
            "VALUES ('job_legacy', 'queued', ?, ?, ?)",
            (payload.model_dump_json(), now, now),
        )

    queue = SQLiteQueue(path)

    assert queue.get("job_legacy")["run_id"] == "run_legacy"


def test_concurrent_queue_initialization_serializes_legacy_schema_migration(
    tmp_path: Path,
) -> None:
    path = tmp_path / "concurrent-legacy.db"
    with sqlite3.connect(path) as db:
        db.execute(
            """
            CREATE TABLE jobs (
                id TEXT PRIMARY KEY,
                state TEXT NOT NULL,
                payload_json TEXT NOT NULL,
                result_json TEXT,
                error TEXT,
                lease_owner TEXT,
                lease_expires_at REAL,
                attempts INTEGER NOT NULL DEFAULT 0,
                created_at REAL NOT NULL,
                updated_at REAL NOT NULL
            )
            """
        )
        db.execute(
            "INSERT INTO jobs(id, state, payload_json, created_at, updated_at) "
            "VALUES ('job_legacy', 'queued', '{}', 1, 1)"
        )

    with ThreadPoolExecutor(max_workers=12) as pool:
        queues = list(pool.map(lambda _index: SQLiteQueue(path), range(12)))

    assert all(queue.get("job_legacy")["run_id"] == "run_legacy" for queue in queues)
    with sqlite3.connect(path) as db:
        columns = {row[1] for row in db.execute("PRAGMA table_info(jobs)")}
    assert {"run_id", "terminal_owner"} <= columns
