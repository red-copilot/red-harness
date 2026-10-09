from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

from harness.progress import ProgressLedger
from harness.runtime.evidence import persist_objective_verdict
from harness.trace import TraceRecorder
from harness.verification import VerifierRegistry


def test_objective_verdict_has_content_addressed_redacted_evidence(tmp_path: Path) -> None:
    registry = VerifierRegistry()
    authority = registry.register("task_verifier")
    progress = ProgressLedger()
    trace = TraceRecorder(tmp_path / "trace.jsonl", "run-1", "task-1")

    authorized = persist_objective_verdict(
        run_dir=tmp_path,
        run_id="run-1",
        world_revision=4,
        producer_authority=authority,
        registry=registry,
        progress=progress,
        trace=trace,
        success=True,
        score=100.0,
        milestones={"proof": True},
    )

    verdict = authorized.verdict
    evidence = verdict.evidence[0]
    artifact = tmp_path / evidence.artifact_ref
    raw = artifact.read_bytes()
    assert hashlib.sha256(raw).hexdigest() == evidence.artifact_hash
    assert evidence.evidence_id == f"sha256:{evidence.artifact_hash}"
    assert json.loads(raw)["status"] == "verified"
    assert progress.objective_completed is True
    assert '"type":"objective.verdict"' in (tmp_path / "trace.jsonl").read_text()
    assert artifact.stat().st_mode & 0o777 == 0o600


@pytest.mark.parametrize("producer", ["task_verifier", "benchmark_evaluator"])
@pytest.mark.parametrize("success,expected", [(True, "verified"), (False, "contradicted")])
def test_local_and_benchmark_evaluators_share_objective_verdict_contract(
    tmp_path: Path, producer: str, success: bool, expected: str
) -> None:
    registry = VerifierRegistry()
    authority = registry.register(producer)
    progress = ProgressLedger()
    authorized = persist_objective_verdict(
        run_dir=tmp_path,
        run_id="run-contract",
        world_revision=5,
        producer_authority=authority,
        registry=registry,
        progress=progress,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run-contract", "task-contract"),
        success=success,
        score=42.0 if success else 0.0,
        milestones={"contract": success},
    )

    assert authorized.verdict.status == expected
    assert authorized.verdict.producer == producer
    assert authorized.verdict.evidence[0].world_revision == 5
    assert progress.objective_completed is success


@pytest.mark.parametrize("producer", ["task_verifier", "benchmark_evaluator"])
def test_unavailable_verifier_outcome_is_durable_and_never_completes_goal(
    tmp_path: Path, producer: str
) -> None:
    registry = VerifierRegistry()
    authority = registry.register(producer)
    progress = ProgressLedger()
    authorized = persist_objective_verdict(
        run_dir=tmp_path,
        run_id="run-unavailable",
        world_revision=2,
        producer_authority=authority,
        registry=registry,
        progress=progress,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run-unavailable", "task-contract"),
        status="unavailable",
        failure_type="TimeoutError",
    )

    verdict = authorized.verdict
    evidence = verdict.evidence[0]
    artifact = json.loads((tmp_path / evidence.artifact_ref).read_text(encoding="utf-8"))
    assert verdict.status == "unavailable"
    assert artifact["status"] == "unavailable"
    assert artifact["failure_type"] == "TimeoutError"
    assert "message" not in artifact
    assert progress.objective_completed is False
    assert progress.objective_verdict == verdict.model_dump(mode="json")


def test_objective_evidence_fails_closed_on_symlink_directory(tmp_path: Path) -> None:
    registry = VerifierRegistry()
    authority = registry.register("task_verifier")
    outside = tmp_path / "outside"
    outside.mkdir()
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    (run_dir / "evidence").symlink_to(outside, target_is_directory=True)

    with pytest.raises(OSError):
        persist_objective_verdict(
            run_dir=run_dir,
            run_id="run-1",
            world_revision=0,
            producer_authority=authority,
            registry=registry,
            progress=ProgressLedger(),
            trace=TraceRecorder(run_dir / "trace.jsonl", "run-1", "task-1"),
            success=True,
            score=1.0,
            milestones={},
        )
    assert list(outside.iterdir()) == []
