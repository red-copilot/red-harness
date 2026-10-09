import pytest

from harness.solver_policy import SolverAction, decide_solver_action


@pytest.mark.parametrize(
    ("kwargs", "expected"),
    [
        ({"event_type": "progress.updated"}, SolverAction.VERIFY),
        ({"event_type": "tool.call"}, SolverAction.CONTINUE),
        ({"event_type": "tool.result", "replan_reasons": ["world_state_changed"]}, SolverAction.REPLAN),
        ({"event_type": "progress.updated", "no_progress_count": 6}, SolverAction.STOP),
        ({"event_type": "tool.call", "replan_reasons": ["changed"], "replans_used": 8}, SolverAction.STOP),
        ({"event_type": "tool.call", "remaining_budget_fraction": 0.03, "no_progress_count": 2}, SolverAction.STOP),
        ({"event_type": "tool.call", "remaining_budget_fraction": 0}, SolverAction.STOP),
    ],
)
def test_solver_decisions_cover_continue_verify_replan_and_stop(kwargs, expected) -> None:
    decision = decide_solver_action(**kwargs)

    assert decision.action is expected
    assert decision.reason


def test_force_replan_is_explicit_and_bounded() -> None:
    assert decide_solver_action(force_replan=True).action is SolverAction.REPLAN
    stopped = decide_solver_action(force_replan=True, replans_used=8)
    assert stopped.action is SolverAction.STOP
    assert stopped.reason == "replan_limit"
