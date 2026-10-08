from harness.action_verifier import ActionVerifier
from harness.progress import ProgressLedger


def test_action_verifier_matching_agent_text_is_pending() -> None:
    progress = ProgressLedger(
        expected_observation="admin endpoint exists",
        actual_observation="the admin endpoint exists at /admin",
        last_action={"status": "succeeded"},
    )

    result = ActionVerifier().verify(progress)

    assert result.status == "pending"
    assert result.replan_required is True


def test_action_verifier_marks_failed_action_contradicted() -> None:
    progress = ProgressLedger(
        expected_observation="service is reachable",
        last_action={"status": "failed"},
    )

    result = ActionVerifier().verify(progress)

    assert result.status == "contradicted"
    assert result.replan_required is True
    assert "action_failed" in result.replan_reasons


def test_action_verifier_marks_missing_actual_inconclusive() -> None:
    progress = ProgressLedger(
        expected_observation="port 443 is open",
        last_action={"status": "succeeded"},
    )

    result = ActionVerifier().verify(progress)

    assert result.status == "pending"
    assert result.replan_required is True
    assert "expected_observation_missing" in result.replan_reasons



def test_action_verifier_skips_without_expected_observation() -> None:
    progress = ProgressLedger(last_action={"status": "succeeded"})
    assert ActionVerifier().verify(progress) is None


def test_agent_negative_text_is_not_trusted_verdict() -> None:
    progress = ProgressLedger(
        expected_observation="service reachable",
        actual_observation="service reachable failed",
        last_action={"status": "succeeded"},
    )
    result = ActionVerifier().verify(progress)
    assert result is not None
    assert result.status == "pending"


def test_trusted_verifier_can_confirm_matching_tool_call() -> None:
    progress = ProgressLedger(
        expected_observation="service reachable",
        actual_observation="reachable",
        last_action={"status": "succeeded", "tool_call_id": "call-1"},
    )
    evidence = {"source": "tool_adapter", "verdict": "verified", "action_id": "call-1"}
    result = ActionVerifier().verify(progress, trusted_evidence=evidence)
    assert result is not None
    assert result.status == "verified"


def test_trusted_verdict_rejects_mismatched_action_id() -> None:
    progress = ProgressLedger(
        expected_observation="service reachable",
        actual_observation="reachable",
        last_action={"status": "succeeded", "tool_call_id": "call-1"},
    )
    evidence = {"source": "tool_adapter", "verdict": "verified", "action_id": "other"}
    result = ActionVerifier().verify(progress, trusted_evidence=evidence)
    assert result is not None
    assert result.status == "pending"
