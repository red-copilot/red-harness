import json
import sqlite3
import time
from pathlib import Path

from fastapi.testclient import TestClient

from harness.control_plane import create_control_plane
from harness.world import FileWorldRepository


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
                "schema_version": "harness/world/v1",
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
        "\n".join(
            [
                json.dumps(
                    {
                        "schema_version": "harness/world/v1",
                        "id": "wevt-1",
                        "ts": "2026-10-06T08:00:00+00:00",
                        "kind": "goal",
                        "op": "upsert",
                        "object": {"id": "goal-1", "description": "demo"},
                        "actor": "harness",
                    }
                ),
                json.dumps(
                    {
                        "schema_version": "harness/world/v1",
                        "id": "wevt-2",
                        "ts": "2026-10-06T08:00:01+00:00",
                        "kind": "capability",
                        "op": "upsert",
                        "object": {
                            "id": "cap-1",
                            "type": "network.reachability",
                            "subject": "agent",
                            "scope": "target",
                            "attributes": {},
                        },
                        "actor": "agent:test",
                    }
                ),
            ]
        )
        + "\n",
        encoding="utf-8",
    )
    (run_dir / "progress.json").write_text(
        json.dumps(
            {
                "schema_version": "harness/progress/v1",
                "active_goal": "goal-1",
                "completed_subgoals": [],
                "blocked_subgoals": [],
                "confirmed_facts": [],
                "current_subgoal": "enumerate services",
                "hypotheses": {},
                "expected_observation": None,
                "actual_observation": None,
                "replan_reasons": [],
                "last_action": None,
                "failure_count": 0,
                "no_progress_count": 2,
                "accepted_submissions": 0,
                "rejected_submissions": 0,
                "objective_completed": False,
                "last_event_type": None,
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "trace.jsonl").write_text(
        json.dumps(
            {
                "event_id": "evt_run_started",
                "parent_event_id": None,
                "plan_id": None,
                "subgoal_id": None,
                "action_id": None,
                "hypothesis_id": None,
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
    assert world["revision"] == 2
    world_events = client.get("/v1/runs/run_test/world/events", headers=headers).json()
    assert world_events[0]["sequence"] == 1
    assert world_events[0]["kind"] == "goal"
    incremental = client.get(
        "/v1/runs/run_test/world/events?after_sequence=1",
        headers=headers,
    ).json()
    assert [item["sequence"] for item in incremental] == [2]
    assert incremental[0]["kind"] == "capability"

    exhausted = client.get(
        "/v1/runs/run_test/world/events?after_sequence=2",
        headers=headers,
    )
    assert exhausted.status_code == 200
    assert exhausted.json() == []

    plan = client.get("/v1/runs/run_test/plan", headers=headers).json()
    assert plan["planner"] == "heuristic-skill-v1"
    assert "applicability" in plan
    assert plan["candidates"][0]["skill_id"] == "network.service-discovery"

    rolling = client.get(
        "/v1/runs/run_test/plan/rolling?horizon=2",
        headers=headers,
    ).json()
    assert rolling["planner"] == "rolling-horizon-world-v2"
    assert rolling["horizon"] == 2
    assert rolling["current_subgoal"] == "enumerate services"
    assert rolling["no_progress_count"] == 2
    assert rolling["actions"][0]["skill_id"] == "network.service-discovery"
    assert "no_progress_threshold" in rolling["actions"][0]["replan_triggers"]

    published = client.post(
        "/v1/runs/run_test/plan/publish",
        headers=headers,
        json={
            "skill_id": "network.service-discovery",
            "description": "enumerate reachable services",
            "goal_id": "goal-1",
            "priority": 0.8,
        },
    )
    assert published.status_code == 200
    work_id = published.json()["id"]

    claimed_work = client.post(
        "/v1/work/claim",
        headers=headers,
        json={"run_id": "run_test", "agent_id": "agent-a", "lease_seconds": 60},
    )
    assert claimed_work.status_code == 200
    assert claimed_work.json()["id"] == work_id
    assert claimed_work.json()["skill_id"] == "network.service-discovery"

    heartbeat_work = client.post(
        f"/v1/work/{work_id}/heartbeat",
        headers=headers,
        json={"agent_id": "agent-a", "lease_seconds": 60},
    )
    assert heartbeat_work.status_code == 200

    completed_work = client.post(
        f"/v1/work/{work_id}/complete",
        headers=headers,
        json={"agent_id": "agent-a", "result": {"status": "done"}},
    )
    assert completed_work.status_code == 200

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


def test_control_plane_uses_world_repository_factory(tmp_path: Path) -> None:
    runs_root = tmp_path / "runs"
    run_dir = runs_root / "run_repo"
    run_dir.mkdir(parents=True)
    (run_dir / "result.json").write_text(
        json.dumps({"run_id": "run_repo", "success": False, "score": 0}),
        encoding="utf-8",
    )
    (run_dir / "world.events.jsonl").write_text(
        json.dumps(
            {
                "schema_version": "harness/world/v1",
                "id": "wevt-repo",
                "ts": "2026-10-06T08:00:00+00:00",
                "kind": "goal",
                "op": "upsert",
                "object": {"id": "goal-repo", "description": "repo test"},
                "actor": "harness",
            }
        )
        + "\n",
        encoding="utf-8",
    )

    seen_paths: list[Path] = []

    def repository_factory(path: Path):
        seen_paths.append(path)
        return FileWorldRepository(path)

    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=runs_root,
        token="read-token",
        world_repository_factory=repository_factory,
    )
    client = TestClient(app)

    headers = {"Authorization": "Bearer read-token"}
    response = client.get("/v1/runs/run_repo/world", headers=headers)
    assert response.status_code == 200
    assert response.json()["goals"]["goal-repo"]["description"] == "repo test"
    assert seen_paths == [run_dir / "world.events.jsonl"]


def test_control_plane_fails_closed_without_authentication_configuration(tmp_path: Path) -> None:
    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=tmp_path / "runs",
    )
    client = TestClient(app)

    assert client.get("/health").status_code == 200
    assert client.get("/v1/jobs").status_code == 503


def test_control_plane_rejects_run_directory_symlink(tmp_path: Path) -> None:
    runs = tmp_path / "runs"
    actual = runs / "run_actual"
    actual.mkdir(parents=True)
    (actual / "result.json").write_text('{"success":true}', encoding="utf-8")
    (runs / "run_alias").symlink_to(actual, target_is_directory=True)
    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=runs,
        token="read-token",
    )

    response = TestClient(app).get(
        "/v1/runs/run_alias",
        headers={"Authorization": "Bearer read-token"},
    )

    assert response.status_code == 400


def test_control_plane_rejects_symlinked_world_artifact(tmp_path: Path) -> None:
    runs = tmp_path / "runs"
    run = runs / "run_world"
    run.mkdir(parents=True)
    outside = tmp_path / "outside.db"
    outside.write_bytes(b"not a database")
    (run / "world.db").symlink_to(outside)
    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=runs,
        token="read-token",
    )

    response = TestClient(app).get(
        "/v1/runs/run_world/world",
        headers={"Authorization": "Bearer read-token"},
    )

    assert response.status_code == 422
    assert outside.read_bytes() == b"not a database"


def test_otlp_endpoint_rejects_valid_json_with_invalid_trace_shape(tmp_path: Path) -> None:
    runs = tmp_path / "runs"
    run = runs / "run_invalid_otel"
    run.mkdir(parents=True)
    (run / "trace.jsonl").write_text(json.dumps({"type": "agent.output"}) + "\n")
    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=runs,
        token="read-token",
    )

    response = TestClient(app, raise_server_exceptions=False).get(
        "/v1/runs/run_invalid_otel/otel",
        headers={"Authorization": "Bearer read-token"},
    )

    assert response.status_code == 422
    assert response.json()["detail"] == "trace artifact is invalid"


def test_rolling_plan_rejects_symlinked_world_database(tmp_path: Path) -> None:
    runs = tmp_path / "runs"
    run = runs / "run_rolling"
    run.mkdir(parents=True)
    (run / "world.events.jsonl").write_text("", encoding="utf-8")
    outside_db = tmp_path / "outside.db"
    outside_db.write_bytes(b"sentinel")
    (run / "world.db").symlink_to(outside_db)
    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=runs,
        token="read-token",
    )

    response = TestClient(app).get(
        "/v1/runs/run_rolling/plan/rolling",
        headers={"Authorization": "Bearer read-token"},
    )

    assert response.status_code == 422
    assert outside_db.read_bytes() == b"sentinel"


def test_control_plane_reports_malformed_trace_as_client_error(tmp_path: Path) -> None:
    runs = tmp_path / "runs"
    run = runs / "run_malformed"
    run.mkdir(parents=True)
    (run / "trace.jsonl").write_text('{"type":\n', encoding="utf-8")
    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=runs,
        token="read-token",
    )

    response = TestClient(app).get(
        "/v1/runs/run_malformed/trace",
        headers={"Authorization": "Bearer read-token"},
    )

    assert response.status_code == 422


def test_trace_endpoint_paginates_with_physical_line_cursor(tmp_path: Path) -> None:
    runs = tmp_path / "runs"
    run = runs / "run_trace_pages"
    run.mkdir(parents=True)
    (run / "trace.jsonl").write_text(
        "".join(
            json.dumps({"type": f"event.{index}", "data": {}}) + "\n"
            for index in range(5)
        ),
        encoding="utf-8",
    )
    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=runs,
        token="read-token",
    )
    client = TestClient(app)
    headers = {"Authorization": "Bearer read-token"}

    first = client.get("/v1/runs/run_trace_pages/trace?limit=2", headers=headers)
    second = client.get(
        "/v1/runs/run_trace_pages/trace?limit=2&after_line=2", headers=headers
    )
    final = client.get(
        "/v1/runs/run_trace_pages/trace?limit=2&after_line=4", headers=headers
    )

    assert [event["type"] for event in first.json()] == ["event.0", "event.1"]
    assert first.headers["x-next-line"] == "2"
    assert first.headers["x-has-more"] == "true"
    assert [event["type"] for event in second.json()] == ["event.2", "event.3"]
    assert second.headers["x-next-line"] == "4"
    assert second.headers["x-has-more"] == "true"
    assert [event["type"] for event in final.json()] == ["event.4"]
    assert final.headers["x-next-line"] == "5"
    assert final.headers["x-has-more"] == "false"


def test_trace_endpoint_streams_and_bounds_serialized_page(
    tmp_path: Path, monkeypatch
) -> None:
    runs = tmp_path / "runs"
    run = runs / "run_trace_stream"
    run.mkdir(parents=True)
    trace_path = run / "trace.jsonl"
    with trace_path.open("w", encoding="utf-8") as handle:
        for index in range(32):
            handle.write(
                json.dumps(
                    {"type": f"event.{index}", "payload": "x" * 180_000},
                    separators=(",", ":"),
                )
                + "\n"
            )

    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=runs,
        token="read-token",
    )
    monkeypatch.setattr(
        "harness.api.runs.read_regular_text",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(AssertionError("whole-file read")),
    )

    response = TestClient(app).get(
        "/v1/runs/run_trace_stream/trace?limit=100",
        headers={"Authorization": "Bearer read-token"},
    )

    assert response.status_code == 200
    assert len(response.content) <= 4 * 1024 * 1024
    assert response.headers["x-has-more"] == "true"
    assert 0 < int(response.headers["x-next-line"]) < 32


def test_control_plane_artifact_routes_reject_symlinked_inputs(tmp_path: Path) -> None:
    runs = tmp_path / "runs"
    run = runs / "run_linked_artifacts"
    run.mkdir(parents=True)
    outside = tmp_path / "outside.json"
    outside.write_text('{"run_id":"outside","success":true}\n', encoding="utf-8")
    for name in ("result.json", "progress.json", "trace.jsonl"):
        (run / name).symlink_to(outside)
    (run / "world.events.jsonl").write_text(
        json.dumps(
            {
                "schema_version": "harness/world/v1",
                "id": "world-event-linked",
                "ts": "2026-10-09T00:00:00+00:00",
                "kind": "goal",
                "op": "upsert",
                "object": {"id": "goal-linked", "description": "linked artifact test"},
                "actor": "harness",
            }
        )
        + "\n",
        encoding="utf-8",
    )
    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=runs,
        token="read-token",
    )
    client = TestClient(app)
    headers = {"Authorization": "Bearer read-token"}

    assert client.get("/v1/runs/run_linked_artifacts", headers=headers).status_code == 422
    assert client.get(
        "/v1/runs/run_linked_artifacts/trace", headers=headers
    ).status_code == 404
    audit = client.get("/v1/runs/run_linked_artifacts/audit", headers=headers)
    assert audit.status_code == 200
    assert audit.json()["status"] == "incomplete"
    assert client.get("/v1/runs/run_linked_artifacts/otel", headers=headers).status_code == 422
    rolling = client.get(
        "/v1/runs/run_linked_artifacts/plan/rolling", headers=headers
    )
    assert rolling.status_code == 422
    assert outside.read_text(encoding="utf-8") == '{"run_id":"outside","success":true}\n'


def test_expired_job_requires_explicit_requeue_after_reconciliation(tmp_path: Path) -> None:
    queue_db = tmp_path / "control.db"
    app = create_control_plane(
        queue_db=queue_db,
        runs_root=tmp_path / "runs",
        token="control-token",
    )
    client = TestClient(app)
    headers = {"Authorization": "Bearer control-token"}
    submitted = client.post(
        "/v1/jobs",
        headers=headers,
        json={"task_path": "task.yaml", "agent_path": "agent.yaml"},
    ).json()
    job_id = submitted["id"]
    claimed = client.post(
        "/v1/jobs/claim",
        headers=headers,
        json={"worker_id": "worker-a", "lease_seconds": 10},
    ).json()
    assert claimed["id"] == job_id
    with sqlite3.connect(queue_db) as db:
        db.execute(
            "UPDATE jobs SET lease_expires_at=? WHERE id=?",
            (time.time() - 1, job_id),
        )

    assert client.post(
        "/v1/jobs/claim",
        headers=headers,
        json={"worker_id": "worker-b", "lease_seconds": 10},
    ).json() is None
    assert client.get(f"/v1/jobs/{job_id}", headers=headers).json()["state"] == (
        "reconciliation_required"
    )
    retry = client.post(f"/v1/jobs/{job_id}/requeue", headers=headers)
    assert retry.status_code == 200
    retried = client.post(
        "/v1/jobs/claim",
        headers=headers,
        json={"worker_id": "worker-b", "lease_seconds": 10},
    ).json()
    assert retried["id"] == job_id
    assert retried["attempts"] == 2


def test_control_plane_rejects_oversized_terminal_result_without_completing_job(
    tmp_path: Path,
) -> None:
    queue_db = tmp_path / "control.db"
    app = create_control_plane(
        queue_db=queue_db,
        runs_root=tmp_path / "runs",
        token="control-token",
    )
    client = TestClient(app)
    headers = {"Authorization": "Bearer control-token"}
    submitted = client.post(
        "/v1/jobs",
        headers=headers,
        json={"task_path": "task.yaml", "agent_path": "agent.yaml"},
    )
    assert submitted.status_code == 200
    job_id = submitted.json()["id"]
    claimed = client.post(
        "/v1/jobs/claim",
        headers=headers,
        json={"worker_id": "worker-a", "lease_seconds": 60},
    )
    assert claimed.status_code == 200

    response = client.post(
        f"/v1/jobs/{job_id}/complete",
        headers=headers,
        json={"worker_id": "worker-a", "result": {"artifact": "x" * (1024 * 1024 + 1)}},
    )

    assert response.status_code == 422
    assert client.get(f"/v1/jobs/{job_id}", headers=headers).json()["state"] == "running"


def test_control_plane_rejects_malformed_terminal_result_before_completion(
    tmp_path: Path,
) -> None:
    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=tmp_path / "runs",
        token="control-token",
    )
    client = TestClient(app)
    headers = {"Authorization": "Bearer control-token"}
    submitted = client.post(
        "/v1/jobs",
        headers=headers,
        json={"task_path": "task.yaml", "agent_path": "agent.yaml"},
    )
    job_id = submitted.json()["id"]
    claimed = client.post(
        "/v1/jobs/claim",
        headers=headers,
        json={"worker_id": "worker-a", "lease_seconds": 60},
    )
    assert claimed.status_code == 200

    response = client.post(
        f"/v1/jobs/{job_id}/complete",
        headers=headers,
        json={
            "worker_id": "worker-a",
            "result": {"agent_id": "agent", "success": True, "score": "100"},
        },
    )

    assert response.status_code == 422
    stored = client.get(f"/v1/jobs/{job_id}", headers=headers).json()
    assert stored["state"] == "running"
    assert stored["result"] is None


def test_control_plane_rejects_oversized_json_request_body(tmp_path: Path) -> None:
    app = create_control_plane(
        queue_db=tmp_path / "control.db",
        runs_root=tmp_path / "runs",
        token="control-token",
    )
    client = TestClient(app)
    headers = {"Authorization": "Bearer control-token"}

    response = client.post(
        "/v1/jobs",
        headers=headers,
        json={
            "task_path": "task.yaml",
            "agent_path": "agent.yaml",
            "ignored_extra": "x" * (2 * 1024 * 1024),
        },
    )

    assert response.status_code == 413
    assert client.get("/v1/jobs", headers=headers).json() == []


def test_request_body_limit_catches_oversized_stream_without_content_length() -> None:
    import asyncio

    from harness.control_plane import _RequestBodyLimitMiddleware

    called = False
    messages = [
        {"type": "http.request", "body": b"123", "more_body": True},
        {"type": "http.request", "body": b"456", "more_body": False},
    ]
    sent = []

    async def app(_scope, _receive, _send):
        nonlocal called
        called = True

    async def receive():
        return messages.pop(0)

    async def send(message):
        sent.append(message)

    asyncio.run(
        _RequestBodyLimitMiddleware(app, max_bytes=4)(
            {"type": "http", "method": "POST", "headers": []}, receive, send
        )
    )

    assert called is False
    assert sent[0]["status"] == 413
