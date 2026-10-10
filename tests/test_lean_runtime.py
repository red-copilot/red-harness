"""Lean solver keeps verification/world durability without building heuristic plans."""

import importlib

from harness.runtime.solver_profile import SolverProfile
from harness.trace import TraceRecorder
from harness.world import Goal

bootstrap_module = importlib.import_module("harness.runtime.bootstrap")


def test_lean_runtime_does_not_load_skills_or_build_planner(tmp_path, monkeypatch):
    def unexpected_skills_load(_root):
        raise AssertionError("disabled heuristic planner must not load skills")

    monkeypatch.setattr(bootstrap_module, "load_skills", unexpected_skills_load)
    runtime = bootstrap_module.bootstrap_solver(
        run_dir=tmp_path,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run-test", "case-test"),
        goal=Goal(id="goal:test", description="test challenge", status="active"),
        actor="agent:test",
        context_query="test challenge",
        skills_root=tmp_path / "absent-skills",
        solver_profile=SolverProfile.PI_WORLD,
    )
    assert runtime.loop.planner is None
    assert runtime.loop.skills == []
    assert runtime.loop.planner_enabled is False
    assert (tmp_path / "world.context.txt").exists()
    assert (tmp_path / "progress.json").exists()


def test_legacy_profile_still_loads_skills(tmp_path, monkeypatch):
    seen = []
    monkeypatch.setattr(bootstrap_module, "load_skills", lambda root: seen.append(root) or [])
    runtime = bootstrap_module.bootstrap_solver(
        run_dir=tmp_path,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run-legacy", "case-test"),
        goal=Goal(id="goal:test", description="test challenge", status="active"),
        actor="agent:test",
        context_query="test challenge",
        skills_root=tmp_path / "skills",
        solver_profile=SolverProfile.PI_WORLD_HEURISTIC,
    )
    assert len(seen) == 1
    assert runtime.loop.planner is not None


def test_default_profile_skips_world_projection_and_skills(tmp_path, monkeypatch):
    def fail_skills(_root):
        raise AssertionError("default must not load heuristic skills")

    monkeypatch.setattr(bootstrap_module, "load_skills", fail_skills)
    runtime = bootstrap_module.bootstrap_solver(
        run_dir=tmp_path,
        trace=TraceRecorder(tmp_path / "trace.jsonl", "run-lean", "case-test"),
        goal=Goal(id="goal:test", description="test challenge", status="active"),
        actor="agent:test",
        context_query="test challenge",
        skills_root=tmp_path / "absent",
    )
    assert runtime.loop.planner is None
    assert not runtime.loop.world_context_enabled
    assert (tmp_path / "world.context.txt").read_text(encoding="utf-8") == ""
    assert (tmp_path / "progress.json").exists()
