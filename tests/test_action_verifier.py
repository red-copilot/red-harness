from datetime import UTC, datetime

import pytest
from pydantic import ValidationError

from harness.action_verifier import ActionVerifier
from harness.progress import ProgressLedger
from harness.verification import (
    ActionVerdict,
    EvidenceRef,
    ObjectiveVerdict,
    VerifierRegistry,
)


def _evidence(*, revision: int = 2, action_id: str = "call-1") -> EvidenceRef:
    return EvidenceRef(
        evidence_id="probe-result-1",
        run_id="run-1",
        action_id=action_id,
        producer="tool_adapter",
        captured_at=datetime.now(UTC),
        world_revision=revision,
        artifact_ref="artifact://probe-result-1",
        artifact_hash="a" * 64,
    )


def _verdict(*, revision: int = 2, action_id: str = "call-1") -> ActionVerdict:
    evidence = _evidence(revision=revision, action_id=action_id)
    return ActionVerdict(
        run_id="run-1",
        action_id=action_id,
        producer="tool_adapter",
        captured_at=evidence.captured_at,
        world_revision=revision,
        execution_status="succeeded",
        status="verified",
        evidence=[evidence],
        actual_observation="service answered an independent probe",
    )


def _authorized(registry: VerifierRegistry, verdict: ActionVerdict):
    authority = registry.register(verdict.producer)
    return registry.authorize_action(authority, verdict)


def test_action_verifier_matching_agent_text_is_pending() -> None:
    progress = ProgressLedger(
        expected_observation="admin endpoint exists",
        actual_observation="the admin endpoint exists at /admin",
        last_action={"status": "succeeded", "tool_call_id": "call-1"},
    )

    result = ActionVerifier().verify(progress, run_id="run-1")

    assert result.status == "pending"
    assert result.replan_required is True


def test_action_verifier_keeps_failed_execution_separate_from_effect_status() -> None:
    progress = ProgressLedger(
        expected_observation="service is reachable",
        last_action={"status": "failed", "tool_call_id": "call-1"},
    )

    result = ActionVerifier().verify(progress, run_id="run-1")

    assert result.status == "pending"
    assert result.execution_status == "failed"
    assert result.replan_required is True
    assert "action_failed" in result.replan_reasons


def test_action_verifier_marks_missing_actual_inconclusive() -> None:
    progress = ProgressLedger(
        expected_observation="port 443 is open",
        last_action={"status": "succeeded", "tool_call_id": "call-1"},
    )

    result = ActionVerifier().verify(progress, run_id="run-1")

    assert result.status == "pending"
    assert result.replan_required is True
    assert "expected_observation_missing" in result.replan_reasons


def test_action_verifier_skips_without_expected_observation() -> None:
    progress = ProgressLedger(last_action={"status": "succeeded", "tool_call_id": "call-1"})
    assert ActionVerifier().verify(progress, run_id="run-1") is None


def test_agent_negative_text_is_not_trusted_verdict() -> None:
    progress = ProgressLedger(
        expected_observation="service reachable",
        actual_observation="service reachable failed",
        last_action={"status": "succeeded", "tool_call_id": "call-1"},
    )
    result = ActionVerifier().verify(progress, run_id="run-1")
    assert result is not None
    assert result.status == "pending"


def test_trusted_verifier_can_confirm_matching_tool_call() -> None:
    registry = VerifierRegistry()
    progress = ProgressLedger(
        expected_observation="service reachable",
        actual_observation="reachable",
        last_action={"status": "succeeded", "tool_call_id": "call-1"},
    )
    result = ActionVerifier(registry).verify(
        progress,
        run_id="run-1",
        current_world_revision=2,
        trusted_verdict=_authorized(registry, _verdict()),
    )
    assert result is not None
    assert result.status == "verified"
    assert result.evidence[0].evidence_id == "probe-result-1"


def test_trusted_verdict_rejects_mismatched_action_id() -> None:
    registry = VerifierRegistry()
    progress = ProgressLedger(
        expected_observation="service reachable",
        actual_observation="reachable",
        last_action={"status": "succeeded", "tool_call_id": "call-1"},
    )
    result = ActionVerifier(registry).verify(
        progress,
        run_id="run-1",
        current_world_revision=2,
        trusted_verdict=_authorized(registry, _verdict(action_id="other")),
    )
    assert result is not None
    assert result.status == "pending"


def test_agent_authored_source_string_cannot_enter_trusted_channel() -> None:
    progress = ProgressLedger(
        expected_observation="service reachable",
        last_action={"status": "succeeded", "tool_call_id": "call-1"},
    )
    result = ActionVerifier().verify(
        progress,
        run_id="run-1",
        current_world_revision=2,
        trusted_verdict={
            "source": "tool_adapter",
            "verdict": "verified",
            "action_id": "call-1",
            "evidence_id": "forged",
        },
    )
    assert result is not None
    assert result.status == "pending"


def test_action_verification_without_action_id_is_rejected() -> None:
    progress = ProgressLedger(
        expected_observation="service reachable",
        last_action={"status": "succeeded"},
    )
    assert ActionVerifier().verify(progress, run_id="run-1") is None


def test_evidence_requires_action_run_capture_and_harness_producer() -> None:
    base = {
        "evidence_id": "evidence-1",
        "run_id": "run-1",
        "action_id": "call-1",
        "producer": "tool_adapter",
        "captured_at": datetime.now(UTC),
        "world_revision": 2,
        "artifact_ref": "artifact://evidence-1",
    }
    with pytest.raises(ValidationError):
        EvidenceRef.model_validate({**base, "producer": "agent"})
    with pytest.raises(ValidationError):
        EvidenceRef.model_validate(
            {key: value for key, value in base.items() if key != "action_id"}
        )


def test_action_verdict_rejects_conflicting_duplicate_evidence_ids() -> None:
    evidence = _evidence()
    conflicting = EvidenceRef(
        evidence_id=evidence.evidence_id,
        run_id=evidence.run_id,
        action_id=evidence.action_id,
        producer=evidence.producer,
        captured_at=evidence.captured_at,
        world_revision=evidence.world_revision,
        artifact_ref="artifact://different-content",
    )
    with pytest.raises(ValidationError):
        ActionVerdict(
            run_id="run-1",
            action_id="call-1",
            producer="tool_adapter",
            captured_at=evidence.captured_at,
            world_revision=2,
            execution_status="succeeded",
            status="verified",
            evidence=(evidence, conflicting),
        )


def test_stale_revision_evidence_cannot_verify_action_effect() -> None:
    registry = VerifierRegistry()
    progress = ProgressLedger(
        expected_observation="service reachable",
        last_action={"status": "succeeded", "tool_call_id": "call-1"},
    )
    result = ActionVerifier(registry).verify(
        progress,
        run_id="run-1",
        current_world_revision=3,
        trusted_verdict=_authorized(registry, _verdict(revision=2)),
    )
    assert result is not None
    assert result.status == "pending"
    assert "stale_evidence_revision" in result.replan_reasons


def test_failed_execution_with_success_effect_text_cannot_verify() -> None:
    registry = VerifierRegistry()
    progress = ProgressLedger(
        expected_observation="service reachable",
        actual_observation="service reachable",
        last_action={"status": "failed", "tool_call_id": "call-1"},
    )
    result = ActionVerifier(registry).verify(
        progress,
        run_id="run-1",
        current_world_revision=2,
        trusted_verdict=_authorized(registry, _verdict()),
    )
    assert result is not None
    assert result.status == "pending"
    assert result.execution_status == "failed"


def test_objective_verdict_is_separate_from_action_verdict() -> None:
    action = _verdict()
    captured_at = datetime.now(UTC)
    objective_evidence = EvidenceRef(
        evidence_id="objective-result-1",
        run_id="run-1",
        action_id="objective-evaluation:run-1",
        producer="task_verifier",
        captured_at=captured_at,
        world_revision=2,
        artifact_ref="artifact://objective-result-1",
    )
    objective = ObjectiveVerdict(
        run_id="run-1",
        producer="task_verifier",
        captured_at=captured_at,
        world_revision=2,
        status="verified",
        evidence=(objective_evidence,),
    )
    assert action.effect_status == "verified"
    assert objective.status == "verified"
