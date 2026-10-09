import pytest

from harness.runtime.lifecycle import RunLifecycle, resolve_run_status


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
    assert (
        resolve_run_status(
            objective_completed=completed,
            timed_out=timed_out,
            budget_exceeded=budget_exceeded,
        )
        == expected
    )


@pytest.mark.parametrize(
    ("source", "target"),
    [
        ("created", "running"),
        ("running", "paused"),
        ("paused", "running"),
        ("running", "recovering"),
        ("recovering", "running"),
        ("running", "verifying"),
        ("verifying", "succeeded"),
        ("verifying", "failed"),
        ("running", "exhausted"),
        ("running", "cancelled"),
        ("running", "failed"),
    ],
)
def test_lifecycle_accepts_only_explicit_transitions(source: str, target: str) -> None:
    lifecycle = RunLifecycle()
    paths = {
        "created": [],
        "running": ["running"],
        "paused": ["running", "paused"],
        "recovering": ["running", "recovering"],
        "verifying": ["running", "verifying"],
    }
    for phase in paths[source]:
        lifecycle.transition(phase)  # type: ignore[arg-type]

    lifecycle.transition(target)  # type: ignore[arg-type]
    assert lifecycle.phase == target


@pytest.mark.parametrize(
    ("initial", "target"),
    [("created", "succeeded"), ("succeeded", "failed"), ("failed", "running")],
)
def test_lifecycle_rejects_illegal_and_post_terminal_transitions(initial: str, target: str) -> None:
    lifecycle = RunLifecycle()
    if initial == "succeeded":
        lifecycle.transition("running")
        lifecycle.transition("verifying")
        lifecycle.transition("succeeded")
    elif initial == "failed":
        lifecycle.transition("failed")
    with pytest.raises(ValueError, match="transition"):
        lifecycle.transition(target)  # type: ignore[arg-type]


@pytest.mark.parametrize(
    ("first", "second"),
    [("cancelled", "succeeded"), ("succeeded", "cancelled")],
)
def test_terminal_decision_is_single_and_cancellation_race_is_ordered(
    first: str, second: str
) -> None:
    lifecycle = RunLifecycle()
    lifecycle.transition("running")
    lifecycle.transition(first)  # type: ignore[arg-type]
    with pytest.raises(ValueError, match="terminal run phase"):
        lifecycle.transition(second)  # type: ignore[arg-type]
