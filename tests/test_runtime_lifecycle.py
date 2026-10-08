import pytest

from harness.runtime.lifecycle import resolve_run_status


@pytest.mark.parametrize(
    ("completed", "timed_out", "budget_exceeded", "expected"),
    [
        (False, False, False, "finished"),
        (False, True, False, "timeout"),
        (False, False, True, "budget_exceeded"),
        (False, True, True, "timeout"),
        (True, False, False, "objective_completed"),
        (True, True, True, "objective_completed"),
    ],
)
def test_run_status_precedence(
    completed: bool, timed_out: bool, budget_exceeded: bool, expected: str
) -> None:
    assert resolve_run_status(
        objective_completed=completed,
        timed_out=timed_out,
        budget_exceeded=budget_exceeded,
    ) == expected
