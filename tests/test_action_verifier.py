from harness.action_verifier import ActionVerifier
from harness.progress import ProgressLedger


def test_action_verifier_marks_matching_observation_verified() -> None:
    progress = ProgressLedger(
        action_intent={"action_id": "action-1"},
        expected_observations=["admin endpoint exists"],
        expected_observation="admin endpoint exists",
        actual_observation="the admin endpoint exists at /admin",
        actual_observation_action_id="action-1",
    )

    result = ActionVerifier().verify(progress)

    assert result is not None
    assert result.status == "verified"
    assert result.replan_required is False


def test_action_verifier_waits_without_semantic_observation() -> None:
    progress = ProgressLedger(
        action_intent={"action_id": "action-1"},
        expected_observations=["service is reachable"],
        expected_observation="service is reachable",
        last_action={"status": "failed"},
    )

    result = ActionVerifier().verify(progress)

    assert result is not None
    assert result.status == "pending"
    assert result.replan_required is False
    assert result.replan_reasons == []


def test_action_verifier_skips_without_expected_observation() -> None:
    progress = ProgressLedger(last_action={"status": "succeeded"})
    assert ActionVerifier().verify(progress) is None


def test_action_verifier_negative_evidence_wins_over_token_overlap() -> None:
    progress = ProgressLedger(
        action_intent={"action_id": "action-1"},
        expected_observations=["service reachable"],
        expected_observation="service reachable",
        actual_observation="service reachable failed",
        actual_observation_action_id="action-1",
    )
    result = ActionVerifier().verify(progress)
    assert result is not None
    assert result.status == "contradicted"


def test_action_verifier_preserves_multiple_expectations() -> None:
    progress = ProgressLedger(
        action_intent={"action_id": "action-1"},
        expected_observations=["admin endpoint exists", "HTTP 403"],
        expected_observation="admin endpoint exists",
        actual_observation="HTTP 403 returned",
        actual_observation_action_id="action-1",
    )

    result = ActionVerifier().verify(progress)

    assert result is not None
    assert result.status == "verified"
    assert result.expected_observation == "HTTP 403"
    assert result.expected_observations == [
        "admin endpoint exists",
        "HTTP 403",
    ]
