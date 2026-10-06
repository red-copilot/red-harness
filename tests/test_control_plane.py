import json
from pathlib import Path

from fastapi.testclient import TestClient

from redharness.control_plane import create_control_plane


def test_control_plane_endpoints(tmp_path: Path) -> None:
    runs_root = tmp_path / "runs"
    run_dir = runs_root / "run_test"
    run_dir.mkdir(parents=True)
    (run_dir / "result.json").write_text(
        json.dumps({"run_id": "run_test", "success": True, "score": 100}),
        encoding="utf-8",
    )
    (run_dir / "world.snapshot.json").write_text(
        json.dumps(
            {
                "schema_version": "redharness.world/v1",
                "revision": 1,
                "entities": {},
                "relations": {},
                "observations": {},
                "artifacts": {},
                "capabilities": {
                    "cap-1": {
                        "id": "cap-1",
                        "type": "network.reachability",
                        "subject": "agent",
                        "scope": "target",
                        "attributes": {},
                    }
                },
                "hypotheses": {},
                "goals": {"goal-1": {"id": "goal-1", "description": "demo"}},
                "failures": {},
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "world.events.jsonl").write_text(
        json.dumps(
            {
                "schema_version": "redharness.world/v1",
                "id": "wevt-1",
                "ts": "2026-10-06T08:00:00+00:00",
                "kind": "goal",
                "op": "upsert",
                "object": {"id": "goal-1", "description": "demo"},
                "actor": "harness",
            }
        )
        + "\n",
        encoding="utf-8",
    )
    (run_dir / "trace.jsonl").write_text(
        json.dumps(
            {
                "ts": "2026-10-06T08:00:00+00:00",
                "run_id": "run_test",
                "task_id": "hello-001",
                "type": "run.started",
                "actor": "harness",
                "data": {"seed": 1},
            }
        )
        + "\n",
        encoding="utf-8",
    )

    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=runs_root,
        benchmarks_root=Path("benchmarks/examples"),
        skills_root=Path("skills/examples"),
        token="secret",
    )
    client = TestClient(app)
    headers = {"Authorization": "Bearer secret"}

    assert client.get("/health").status_code == 200
    assert client.get("/v1/jobs").status_code == 401

    skills = client.get("/v1/skills", headers=headers)
    assert skills.status_code == 200
    assert any(item.get("id") == "network.service-discovery" for item in skills.json())

    benchmarks = client.get("/v1/benchmarks", headers=headers)
    assert benchmarks.status_code == 200
    assert any(item.get("id") == "hello-001" for item in benchmarks.json())

    submitted = client.post(
        "/v1/jobs",
        headers=headers,
        json={
            "task_path": "benchmarks/examples/hello/task.yaml",
            "agent_path": "agents/examples/demo.yaml",
        },
    )
    assert submitted.status_code == 200
    job_id = submitted.json()["id"]

    claimed = client.post(
        "/v1/jobs/claim",
        headers=headers,
        json={"worker_id": "worker-1", "lease_seconds": 60},
    )
    assert claimed.status_code == 200
    assert claimed.json()["id"] == job_id

    completed = client.post(
        f"/v1/jobs/{job_id}/complete",
        headers=headers,
        json={
            "worker_id": "worker-1",
            "result": {
                "agent_id": "demo-agent",
                "success": True,
                "score": 100,
                "metrics": {"duration_ms": 100, "cost_usd": 0},
            },
        },
    )
    assert completed.status_code == 200

    board = client.get("/v1/leaderboard", headers=headers).json()
    assert board[0]["agent_id"] == "demo-agent"
    assert board[0]["mean_score"] == 100

    assert client.get("/v1/runs/run_test", headers=headers).json()["success"] is True
    trace = client.get("/v1/runs/run_test/trace", headers=headers).json()
    assert trace[0]["type"] == "run.started"

    world = client.get("/v1/runs/run_test/world", headers=headers).json()
    assert world["revision"] == 1
    world_events = client.get("/v1/runs/run_test/world/events", headers=headers).json()
    assert world_events[0]["kind"] == "goal"

    plan = client.get("/v1/runs/run_test/plan", headers=headers).json()
    assert plan["planner"] == "heuristic-skill-v1"
    assert plan["candidates"][0]["skill_id"] == "network.service-discovery"

    otel = client.get("/v1/runs/run_test/otel", headers=headers).json()
    spans = otel["resourceSpans"][0]["scopeSpans"][0]["spans"]
    assert spans[0]["name"] == "run.started"

    capabilities = client.get("/v1/capabilities", headers=headers)
    assert set(capabilities.json()) == {
        "docker",
        "gvisor_runsc",
        "firecracker",
        "kvm",
        "pi",
    }
