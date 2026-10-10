"""Regression tests for the lean default solver profile and TSec wiring."""

import inspect

from harness.benchmark.runner import BenchmarkRunner
from harness.benchmark.tsec import TSecRunner
from harness.runtime.solver_profile import (
    DEFAULT_SOLVER_PROFILE,
    SolverProfile,
    solver_profile_settings,
)


def test_default_keeps_world_context_without_heuristic_planner():
    assert DEFAULT_SOLVER_PROFILE is SolverProfile.PI_WORLD
    assert solver_profile_settings(DEFAULT_SOLVER_PROFILE) == (True, False)


def test_ablation_profiles_remain_selectable():
    assert solver_profile_settings(SolverProfile.PI_ONLY) == (False, False)
    assert solver_profile_settings(SolverProfile.PI_WORLD_HEURISTIC) == (True, True)


def test_benchmark_and_tsec_run_use_same_default_profile():
    runner_arg = inspect.signature(BenchmarkRunner.run_case).parameters["solver_profile"]
    tsec_arg = inspect.signature(TSecRunner.run).parameters["solver_profile"]
    assert runner_arg.default is DEFAULT_SOLVER_PROFILE
    assert tsec_arg.default is DEFAULT_SOLVER_PROFILE
