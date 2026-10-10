from datetime import UTC, datetime
from pathlib import Path

import pytest
import yaml
from pydantic import ValidationError

from harness.v2.contracts import (
    ArtifactRef,
    BudgetSpec,
    CleanupResult,
    EvidenceRef,
    ObjectiveVerdict,
    RunResult,
    RunSpec,
    UsageMetrics,
)


def run_spec_payload() -> dict:
    return {
        "apiVersion": "harness/v2",
        "task": {"id": "proof", "description": "Write proof.txt."},
        "pi": {"image": "pi-test:1.0.4", "provider": "test", "model": "fake"},
        "budgets": {"wall_time": 30},
        "evaluation": {"kind": "local", "image": "verifier-test:1", "command": ["verify"]},
    }


def evidence_ref(**updates) -> EvidenceRef:
    return EvidenceRef.model_validate(
        {
            "id": "evidence-1",
            "run_id": "run-1",
            "task_id": "proof",
            "producer": "local_verifier",
            "captured_at": datetime.now(UTC),
            "artifact_ref": "evidence/evidence-1.json",
            "sha256": "a" * 64,
            "input_digest": "b" * 64,
            **updates,
        }
    )


def test_run_spec_has_explicit_v2_and_required_time_budget() -> None:
    spec = RunSpec.model_validate(run_spec_payload())
    assert spec.api_version == "harness/v2"
    assert spec.pi.version == "1.0.4"
    assert spec.budgets.cleanup_timeout == 30
    assert RunSpec.model_validate_json(spec.model_dump_json(by_alias=True)) == spec


def test_documented_configuration_matches_the_versioned_contract() -> None:
    payload = yaml.safe_load(Path("examples/minimal-run-v2.yaml").read_text(encoding="utf-8"))
    spec = RunSpec.model_validate(payload)
    assert spec.task.id == "proof"
    assert spec.evaluation.kind == "local"


@pytest.mark.parametrize("version", ["harness/v1", "harness/v3", None])
def test_run_spec_rejects_unsupported_or_missing_version(version) -> None:
    payload = run_spec_payload()
    if version is None:
        del payload["apiVersion"]
    else:
        payload["apiVersion"] = version
    with pytest.raises(ValidationError):
        RunSpec.model_validate(payload)


@pytest.mark.parametrize("budget", [{}, {"wall_time": 0}, {"wall_time": float("inf")}])
def test_run_spec_rejects_missing_or_unbounded_deadline(budget) -> None:
    with pytest.raises(ValidationError):
        BudgetSpec.model_validate(budget)


@pytest.mark.parametrize("extra", ["checkpoint", "resume", "gateway", "workers"])
def test_run_spec_rejects_removed_features(extra) -> None:
    payload = run_spec_payload()
    payload[extra] = "unused"
    with pytest.raises(ValidationError):
        RunSpec.model_validate(payload)


def test_pi_configuration_rejects_other_versions_and_plaintext_keys() -> None:
    for update in ({"version": "0.61.0"}, {"api_key": "secret-canary"}, {"model": " "}):
        payload = run_spec_payload()
        payload["pi"].update(update)
        with pytest.raises(ValidationError):
            RunSpec.model_validate(payload)


def test_tsec_config_contains_environment_selectors_only() -> None:
    payload = run_spec_payload()
    payload["evaluation"] = {"kind": "tsec"}
    spec = RunSpec.model_validate(payload)
    assert spec.evaluation.token_env == "BENCHMARK_TOKEN"
    payload["evaluation"]["token"] = "secret-canary"
    with pytest.raises(ValidationError):
        RunSpec.model_validate(payload)


def test_platform_credentials_cannot_be_selected_for_pi() -> None:
    payload = run_spec_payload()
    payload["evaluation"] = {"kind": "tsec", "token_env": "PRIVATE_PLATFORM_TOKEN"}
    payload["pi"]["credential_env"] = "PRIVATE_PLATFORM_TOKEN"
    with pytest.raises(ValidationError):
        RunSpec.model_validate(payload)


@pytest.mark.parametrize("path", ["../proof", "/proof", "a/../proof", "a\\proof", "a//b", "."])
def test_artifact_references_cannot_escape_their_root(path) -> None:
    with pytest.raises(ValidationError):
        ArtifactRef(path=path, sha256="a" * 64)


@pytest.mark.parametrize("sha256", ["a" * 63, "g" * 64, "A" * 64])
def test_evidence_requires_a_complete_canonical_sha256(sha256) -> None:
    with pytest.raises(ValidationError):
        evidence_ref(sha256=sha256)


def test_evidence_requires_timezone_and_harness_registered_producer() -> None:
    naive_time = datetime.now(UTC).replace(tzinfo=None)
    for update in ({"captured_at": naive_time}, {"producer": "agent"}):
        with pytest.raises(ValidationError):
            evidence_ref(**update)


def test_evidence_and_verdict_are_immutable() -> None:
    ref = evidence_ref()
    verdict = ObjectiveVerdict(run_id="run-1", task_id="proof", status="passed", evidence=(ref,))
    with pytest.raises(ValidationError):
        ref.producer = "tsec_evaluator"
    with pytest.raises(ValidationError):
        verdict.status = "failed"
    assert isinstance(verdict.evidence, tuple)


@pytest.mark.parametrize("status", ["passed", "failed"])
def test_conclusive_verdict_requires_evidence(status) -> None:
    with pytest.raises(ValidationError):
        ObjectiveVerdict(run_id="run-1", task_id="proof", status=status)


def test_verdict_rejects_other_runs_and_conflicting_evidence_ids() -> None:
    for refs in (
        (evidence_ref(run_id="another-run"),),
        (evidence_ref(task_id="another-task"),),
        (evidence_ref(), evidence_ref(sha256="c" * 64)),
    ):
        with pytest.raises(ValidationError):
            ObjectiveVerdict(run_id="run-1", task_id="proof", status="passed", evidence=refs)


def test_missing_usage_is_unknown_not_zero() -> None:
    assert UsageMetrics().model_dump() == {
        "input_tokens": None,
        "output_tokens": None,
        "total_tokens": None,
        "model_calls": None,
        "tool_calls": None,
        "cost_usd": None,
    }
    for payload in ({"total_tokens": -1}, {"tool_calls": True}, {"cost_usd": float("nan")}):
        with pytest.raises(ValidationError):
            UsageMetrics.model_validate(payload)


def test_cleanup_is_unknown_until_the_owner_confirms_it() -> None:
    assert CleanupResult().ok is None
    for payload in (
        {"ok": False},
        {"ok": True, "error_code": "timeout"},
        {"ok": False, "error_code": "secret-canary"},
    ):
        with pytest.raises(ValidationError):
            CleanupResult.model_validate(payload)


def test_result_separates_goal_verdict_and_cleanup_failure() -> None:
    verdict = ObjectiveVerdict(
        run_id="run-1", task_id="proof", status="passed", evidence=(evidence_ref(),), score=100
    )
    result = RunResult(
        run_id="run-1",
        task_id="proof",
        stop_reason="completed",
        objective=verdict,
        cleanup=CleanupResult(ok=False, error_code="timeout"),
    )
    assert result.objective.status == "passed"
    assert not result.cleanup.ok
    assert RunResult.model_validate_json(result.model_dump_json(by_alias=True)) == result
    with pytest.raises(ValidationError):
        RunResult(run_id="another-run", task_id="proof", stop_reason="completed", objective=verdict)
    with pytest.raises(ValidationError):
        RunResult(run_id="run-1", task_id="proof", stop_reason="timeout", objective=verdict)
