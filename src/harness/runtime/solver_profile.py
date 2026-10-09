"""Experiment profiles for separating Agent, World, and heuristic planning."""
from __future__ import annotations

from enum import StrEnum


class SolverProfile(StrEnum):
    PI_ONLY = "pi-only"
    PI_WORLD = "pi-world"
    PI_WORLD_HEURISTIC = "pi-world-heuristic"


DEFAULT_SOLVER_PROFILE = SolverProfile.PI_WORLD_HEURISTIC


def solver_profile_settings(profile: SolverProfile | str) -> tuple[bool, bool]:
    """Return `(world_context_enabled, planner_enabled)` for a run profile."""
    try:
        selected = SolverProfile(profile)
    except ValueError as exc:
        choices = ", ".join(item.value for item in SolverProfile)
        raise ValueError(f"solver profile must be one of: {choices}") from exc
    return {
        SolverProfile.PI_ONLY: (False, False),
        SolverProfile.PI_WORLD: (True, False),
        SolverProfile.PI_WORLD_HEURISTIC: (True, True),
    }[selected]
