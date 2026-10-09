from datetime import UTC, datetime

import pytest

from harness.progress import ProgressLedger
from harness.verification import (
    ActionVerdict,
    EvidenceRef,
    ObjectiveVerdict,
    VerifierRegistry,
)


def _action_verdict(*, producer: str = "tool_adapter") -> ActionVerdict:
    captured_at = datetime.now(UTC)
    evidence = EvidenceRef(
        evidence_id="evidence-1",
        run_id="run-1",
        action_id="call-1",
        producer=producer,
        captured_at=captured_at,
        world_revision=2,
        artifact_ref="artifact://evidence-1",
    )
    return ActionVerdict(
        run_id="run-1",
        action_id="call-1",
        producer=producer,
        captured_at=captured_at,
        world_revision=2,
        execution_status="succeeded",
        status="verified",
        evidence=(evidence,),
    )


def test_registered_producer_can_authorize_verdict() -> None:
    registry = VerifierRegistry()
    authority = registry.register("tool_adapter")

    authorized = registry.authorize_action(authority, _action_verdict())

    assert registry.is_authorized_action(authorized)


def test_unregistered_or_fabricated_authority_is_rejected() -> None:
    registry = VerifierRegistry()
    other_registry = VerifierRegistry()
    authority = other_registry.register("tool_adapter")

    with pytest.raises(ValueError):
        registry.authorize_action(authority, _action_verdict())


def test_authority_cannot_assert_a_different_producer() -> None:
    registry = VerifierRegistry()
    authority = registry.register("tool_adapter")

    with pytest.raises(ValueError):
        registry.authorize_action(authority, _action_verdict(producer="task_verifier"))


def test_agent_cannot_create_an_authorized_verdict_from_serialized_fields() -> None:
    registry = VerifierRegistry()
    authority = registry.register("tool_adapter")
    authorized = registry.authorize_action(authority, _action_verdict())
    serialized = authorized.verdict.model_dump(mode="json")

    assert registry.is_authorized_action(serialized) is False
    assert registry.is_authorized_action(_action_verdict()) is False


def test_objective_completion_requires_registered_evaluator_authority() -> None:
    registry = VerifierRegistry()
    authority = registry.register("task_verifier")
    captured_at = datetime.now(UTC)
    evidence = EvidenceRef(
        evidence_id="objective-evidence-1",
        run_id="run-1",
        action_id="objective-evaluation:run-1",
        producer="task_verifier",
        captured_at=captured_at,
        world_revision=2,
        artifact_ref="evidence/sha256/example.json",
        artifact_hash="b" * 64,
    )
    verdict = ObjectiveVerdict(
        run_id="run-1",
        producer="task_verifier",
        captured_at=captured_at,
        world_revision=2,
        status="verified",
        evidence=(evidence,),
    )
    progress = ProgressLedger()

    with pytest.raises(ValueError):
        progress.record_objective_verdict(verdict, registry)  # type: ignore[arg-type]

    authorized = registry.authorize_objective(authority, verdict)
    progress.record_objective_verdict(authorized, registry)

    assert progress.objective_completed is True
    assert progress.objective_verdict["evidence"][0]["evidence_id"] == "objective-evidence-1"
