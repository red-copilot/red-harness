from __future__ import annotations

import json
from pathlib import Path

from fastapi.testclient import TestClient
from typer.testing import CliRunner

import harness.audit as audit_module
from harness.audit import audit_run
from harness.cli import app
from harness.control_plane import create_control_plane
from harness.progress import ProgressLedger
from harness.runtime.evidence import persist_objective_verdict
from harness.trace import TraceRecorder
from harness.verification import VerifierRegistry


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
    assert report.status == "contradictory"
    assert report.counts["claims"] == 2
    assert report.counts["pending"] == report.counts["verified"] == 1
    assert report.counts["untraceable_verdicts"] == 2
    assert {i.code for i in report.issues} >= {
        "claim_without_linked_evidence",
        "verdict_untraceable",
        "verdict_evidence_missing",
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


def _write_verified_v2_run(directory: Path) -> Path:
    directory.mkdir(parents=True, exist_ok=True)
    run_id = "run_verified"
    trace = TraceRecorder(directory / "trace.jsonl", run_id, "task-1")
    registry = VerifierRegistry()
    authority = registry.register("task_verifier")
    progress = ProgressLedger()
    persist_objective_verdict(
        run_dir=directory,
        run_id=run_id,
        world_revision=3,
        producer_authority=authority,
        registry=registry,
        progress=progress,
        trace=trace,
        success=True,
        score=100.0,
        milestones={"proof": True},
    )
    progress.write(directory / "progress.json")
    result = {
        "schema_version": "harness/result/v2",
        "run_id": run_id,
        "status": "objective_completed",
        "success": True,
        "score": 100.0,
        "progress": progress.model_dump(mode="json"),
    }
    trace.emit(
        "run.finished",
        data={"status": result["status"], "success": True, "score": 100.0},
    )
    (directory / "result.json").write_text(json.dumps(result), encoding="utf-8")
    return directory / progress.objective_verdict["evidence"][0]["artifact_ref"]


def test_audit_requires_durable_objective_verdict_for_v2_success(tmp_path: Path) -> None:
    _write_run(
        tmp_path,
        result={
            "schema_version": "harness/result/v2",
            "run_id": "run_audit",
            "status": "objective_completed",
            "success": True,
            "score": 100.0,
        },
        events=[
            _event(
                "run.finished",
                {"status": "objective_completed", "success": True, "score": 100.0},
                "end",
            )
        ],
    )

    report = audit_run(tmp_path)

    assert report.status == "contradictory"
    assert "objective_verdict_missing_or_untraceable" in {i.code for i in report.issues}


def test_audit_verifies_objective_artifact_hash_and_provenance(tmp_path: Path) -> None:
    artifact = _write_verified_v2_run(tmp_path)

    assert audit_run(tmp_path).status == "consistent"

    artifact.write_text('{"status":"verified"}\n', encoding="utf-8")
    report = audit_run(tmp_path)
    assert report.status == "contradictory"
    assert {
        "objective_evidence_hash_mismatch",
        "objective_verdict_missing_or_untraceable",
    } <= {i.code for i in report.issues}


def test_newer_revision_objective_verdict_supersedes_earlier_success(tmp_path: Path) -> None:
    _write_verified_v2_run(tmp_path)
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run_verified", "task-1")
    registry = VerifierRegistry()
    authority = registry.register("task_verifier")
    persist_objective_verdict(
        run_dir=tmp_path,
        run_id="run_verified",
        world_revision=4,
        producer_authority=authority,
        registry=registry,
        progress=ProgressLedger(),
        trace=trace,
        success=False,
        score=0,
        milestones={},
    )
    events = [json.loads(line) for line in (tmp_path / "trace.jsonl").read_text().splitlines()]
    verdict_event = events.pop()
    terminal_index = next(
        index for index, event in enumerate(events) if event["type"] == "run.finished"
    )
    events.insert(terminal_index, verdict_event)
    (tmp_path / "trace.jsonl").write_text(
        "".join(json.dumps(event) + "\n" for event in events), encoding="utf-8"
    )

    report = audit_run(tmp_path)

    assert report.status == "contradictory"
    codes = {issue.code for issue in report.issues}
    assert "objective_verdict_conflict" not in codes
    assert "objective_verdict_superseded" in codes
    assert "objective_verdict_missing_or_untraceable" in codes


def test_objective_verdict_audit_replay_is_deterministic(tmp_path: Path) -> None:
    _write_verified_v2_run(tmp_path)

    first = audit_run(tmp_path).model_dump(mode="json")
    replayed = audit_run(tmp_path).model_dump(mode="json")

    assert replayed == first


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


def test_audit_classifies_interrupted_run_without_terminal_as_incomplete(tmp_path: Path) -> None:
    _write_run(
        tmp_path,
        result={
            "run_id": "run_audit",
            "status": "error",
            "success": False,
            "score": 0,
        },
        events=[
            _event("run.started", {}, "start"),
            _event("run.interrupted", {"checkpoint_preserved": True}, "interrupt"),
        ],
    )

    report = audit_run(tmp_path)

    assert report.status == "incomplete"
    assert "terminal_event_missing" in {issue.code for issue in report.issues}


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


def test_audit_bounds_jsonl_physical_lines_including_blank_lines(
    tmp_path: Path, monkeypatch
) -> None:
    _write_run(tmp_path)
    (tmp_path / "trace.jsonl").write_text("\n\n\n\n", encoding="utf-8")
    monkeypatch.setattr("harness.audit.MAX_INPUT_BYTES", 1024 * 1024)
    monkeypatch.setattr("harness.audit.MAX_TRACE_EVENTS", 2)

    report = audit_run(tmp_path)

    assert "line_count_limit_exceeded" in {issue.code for issue in report.issues}


def test_audit_rejects_oversized_jsonl_line(tmp_path: Path, monkeypatch) -> None:
    _write_run(tmp_path)
    monkeypatch.setattr("harness.audit.MAX_INPUT_BYTES", 1024 * 1024)
    monkeypatch.setattr("harness.audit.MAX_JSONL_LINE_BYTES", 16)

    report = audit_run(tmp_path)

    assert "event_size_limit_exceeded" in {issue.code for issue in report.issues}


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
