from pathlib import Path

from harness.queue import JobPayload, SQLiteQueue


def test_queue_claim_heartbeat_complete_and_leaderboard(tmp_path: Path) -> None:
    queue = SQLiteQueue(tmp_path / "queue.db")
    submitted = queue.submit(
        JobPayload(
            task_path="benchmarks/examples/hello/task.yaml",
            agent_path="agents/examples/demo.yaml",
        )
    )

    claimed = queue.claim("worker-a", lease_seconds=60)
    assert claimed is not None
    assert claimed["id"] == submitted["id"]
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


def test_expired_lease_can_be_reclaimed(tmp_path: Path) -> None:
    queue = SQLiteQueue(tmp_path / "queue.db")
    queue.submit(JobPayload(task_path="task.yaml", agent_path="agent.yaml"))

    first = queue.claim("worker-a", lease_seconds=0)
    assert first is not None
    second = queue.claim("worker-b", lease_seconds=60)
    assert second is not None
    assert second["id"] == first["id"]
    assert second["lease_owner"] == "worker-b"
    assert second["attempts"] == 2
