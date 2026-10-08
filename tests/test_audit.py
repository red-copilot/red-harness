from __future__ import annotations

import json
from pathlib import Path

from fastapi.testclient import TestClient
from typer.testing import CliRunner

import harness.audit as audit_module
from harness.audit import audit_run
from harness.cli import app
from harness.control_plane import create_control_plane


def _event(kind: str, data: dict, event_id: str, **extra: object) -> dict:
    return {"event_id": event_id, "type": kind, "data": data, **extra}


def _write_run(
    directory: Path,
    *,
    result: dict | None = None,
    events: list[dict] | None = None,
    progress: dict | None = None,
    snapshot: dict | None = None,
) -> None:
    directory.mkdir(parents=True, exist_ok=True)
    result = result or {"run_id": "run_audit", "status": "finished", "success": True, "score": 1.0}
    events = events or [
        _event("run.finished", {"status": "finished", "success": True, "score": 1.0}, "end")
    ]
    (directory / "result.json").write_text(json.dumps(result), encoding="utf-8")
    (directory / "trace.jsonl").write_text(
        "".join(json.dumps(item) + "\n" for item in events), encoding="utf-8"
    )
    if progress is not None:
        (directory / "progress.json").write_text(json.dumps(progress), encoding="utf-8")
    if snapshot is not None:
        (directory / "world.snapshot.json").write_text(json.dumps(snapshot), encoding="utf-8")


def test_audit_classifies_claims_and_untraceable_verdicts(tmp_path: Path) -> None:
    _write_run(
        tmp_path,
        events=[
            _event("progress.updated", {"confirmed_fact": "claimed fact"}, "claim"),
            _event("tool.result", {"is_error": False}, "tool"),
            _event("action.verified", {"status": "pending"}, "pending"),
            _event("action.verified", {"status": "verified"}, "verified"),
            _event("tool.call", {}, "call", action_id="action-1"),
            _event("run.finished", {"status": "finished", "success": True, "score": 1.0}, "end"),
        ],
    )
    report = audit_run(tmp_path)
    assert report.status == "incomplete"
    assert report.counts["claims"] == 2
    assert report.counts["pending"] == report.counts["verified"] == 1
    assert report.counts["untraceable_verdicts"] == 2
    assert {i.code for i in report.issues} >= {
        "claim_without_linked_evidence",
        "verdict_untraceable",
    }


def test_audit_detects_terminal_and_world_conflicts(tmp_path: Path) -> None:
    _write_run(
        tmp_path,
        result={"run_id": "run_audit", "status": "error", "success": False, "score": 0},
        events=[_event("run.finished", {"status": "finished", "success": True, "score": 1}, "end")],
        progress={"objective_completed": False},
        snapshot={"goals": {"goal": {"status": "completed", "parent_id": None}}},
    )
    report = audit_run(tmp_path)
    assert report.status == "contradictory"
    assert {i.code for i in report.issues} >= {
        "terminal_result_mismatch",
        "world_goal_result_mismatch",
    }


def test_accepted_submission_does_not_mean_objective_complete(tmp_path: Path) -> None:
    _write_run(
        tmp_path,
        result={
            "run_id": "run_audit",
            "status": "finished",
            "success": False,
            "score": 0,
            "submissions": [{"accepted": True, "completed": False}],
        },
        events=[
            _event("run.finished", {"status": "finished", "success": False, "score": 0}, "end")
        ],
        progress={"objective_completed": False},
    )
    assert audit_run(tmp_path).status == "consistent"


def test_audit_deduplicates_events_and_reports_id_conflicts(tmp_path: Path) -> None:
    event = _event("run.finished", {"status": "finished", "success": True, "score": 1.0}, "end")
    _write_run(tmp_path, events=[event, event, {**event, "data": {"success": False}}])
    report = audit_run(tmp_path)
    assert report.status == "contradictory"
    assert report.counts["events"] == 2
    assert {i.code for i in report.issues} >= {"duplicate_event", "event_id_conflict"}


def test_audit_reports_missing_and_truncated_inputs(tmp_path: Path) -> None:
    assert audit_run(tmp_path).status == "incomplete"
    _write_run(tmp_path)
    with (tmp_path / "trace.jsonl").open("a", encoding="utf-8") as stream:
        stream.write('{"event_id":')
    report = audit_run(tmp_path)
    assert report.status == "incomplete"
    assert "truncated_event" in {i.code for i in report.issues}


def test_audit_enforces_size_and_event_limits(tmp_path: Path, monkeypatch) -> None:
    _write_run(
        tmp_path,
        events=[
            _event("run.started", {}, "start"),
            _event("run.finished", {"status": "finished", "success": True, "score": 1}, "end"),
        ],
    )
    monkeypatch.setattr("harness.audit.MAX_INPUT_BYTES", 8)
    report = audit_run(tmp_path)
    assert report.status == "incomplete"
    assert "input_size_limit_exceeded" in {i.code for i in report.issues}

    monkeypatch.setattr("harness.audit.MAX_INPUT_BYTES", 1024 * 1024)
    monkeypatch.setattr("harness.audit.MAX_TRACE_EVENTS", 1)
    report = audit_run(tmp_path)
    assert report.status == "incomplete"
    assert "event_count_limit_exceeded" in {i.code for i in report.issues}


def test_audit_marks_unsupported_versions_and_read_races_incomplete(
    tmp_path: Path, monkeypatch
) -> None:
    _write_run(
        tmp_path,
        result={
            "run_id": "run_audit",
            "schema_version": "future/result/v9",
            "status": "finished",
            "success": True,
            "score": 1.0,
        },
    )
    report = audit_run(tmp_path)
    assert report.status == "incomplete"
    assert "unsupported_result_version" in {i.code for i in report.issues}

    original_read = audit_module._read

    def changed_read(path: Path, name: str, remaining: int):
        raw, summary = original_read(path, name, remaining)
        summary["changed_during_read"] = True
        return raw, summary

    monkeypatch.setattr("harness.audit._read", changed_read)
    report = audit_run(tmp_path)
    assert report.status == "incomplete"
    assert "input_changed_during_read" in {i.code for i in report.issues}


def test_audit_read_only_and_cli_exit_code(tmp_path: Path) -> None:
    _write_run(tmp_path)
    paths = [tmp_path / "result.json", tmp_path / "trace.jsonl"]
    before = [(path.read_bytes(), path.stat().st_mtime_ns) for path in paths]
    cli = CliRunner().invoke(app, ["audit-run", str(tmp_path)])
    assert cli.exit_code == 0
    assert json.loads(cli.stdout)["status"] == "consistent"
    assert [(path.read_bytes(), path.stat().st_mtime_ns) for path in paths] == before
    assert not (tmp_path / "world.db").exists()

    (tmp_path / "trace.jsonl").write_text(
        json.dumps(
            _event("run.finished", {"status": "error", "success": False, "score": 0}, "end")
        ),
        encoding="utf-8",
    )
    contradictory = CliRunner().invoke(app, ["audit-run", str(tmp_path)])
    assert contradictory.exit_code == 1


def test_audit_api_auth_and_report(tmp_path: Path) -> None:
    runs = tmp_path / "runs"
    _write_run(runs / "run_audit")
    client = TestClient(
        create_control_plane(queue_db=tmp_path / "control.db", runs_root=runs, token="secret")
    )
    path = "/v1/runs/run_audit/audit"
    assert client.get(path).status_code == 401
    response = client.get(path, headers={"Authorization": "Bearer secret"})
    assert response.status_code == 200
    assert response.json() == audit_run(runs / "run_audit").model_dump(mode="json")
    assert (
        client.get(
            "/v1/runs/run_missing/audit", headers={"Authorization": "Bearer secret"}
        ).status_code
        == 404
    )
    assert (
        client.get("/v1/runs/invalid/audit", headers={"Authorization": "Bearer secret"}).status_code
        == 400
    )
