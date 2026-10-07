from redharness.action_verifier import ActionVerifier
from redharness.progress import ProgressLedger


def test_action_verifier_marks_matching_observation_verified() -> None:
    progress = ProgressLedger(
        expected_observation="admin endpoint exists",
        actual_observation="the admin endpoint exists at /admin",
        last_action={"status": "succeeded"},
    )

    result = ActionVerifier().verify(progress)

    assert result.status == "verified"
    assert result.replan_required is False


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

    assert result.status == "inconclusive"
    assert result.replan_required is True
    assert "expected_observation_missing" in result.replan_reasons
