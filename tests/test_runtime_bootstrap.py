from pathlib import Path
from unittest.mock import Mock

from harness.runtime import bootstrap_solver
from harness.world import Entity, Goal, SQLiteWorldRepository


def test_bootstrap_solver_initializes_state_and_targets(tmp_path: Path) -> None:
    trace = Mock()
    goal = Goal(id="goal:example", description="Find evidence", status="active", priority=1.0)
    target = Entity(id="target:one", type="benchmark.target", attributes={"address": "localhost"})
    runtime = bootstrap_solver(
        run_dir=tmp_path,
        trace=trace,
        goal=goal,
        actor="agent:sample",
        context_query=goal.description,
        skills_root=tmp_path / "nonexistent-skills",
        targets=[target],
        target_actor="benchmark:example",
    )
    assert runtime.progress.active_goal == goal.id
    assert runtime.loop.world is runtime.world
    assert runtime.loop.progress is runtime.progress
    assert (tmp_path / "progress.json").is_file()
    assert (tmp_path / "world.context.txt").is_file()
    assert (tmp_path / "world.context.txt").read_text(encoding="utf-8")
    assert runtime.world.snapshot.revision >= 2
    assert any(event.actor == "benchmark:example" for event in runtime.world.events())


def test_bootstrap_solver_uses_custom_repository_factory(tmp_path: Path) -> None:
    paths = []

    def factory(path: Path) -> SQLiteWorldRepository:
        paths.append(path)
        return SQLiteWorldRepository(path)

    goal = Goal(id="goal:factory", description="Analyze target", status="active", priority=1.0)
    bootstrap_solver(
        run_dir=tmp_path,
        trace=Mock(),
        goal=goal,
        actor="agent:sample",
        context_query=goal.description,
        skills_root=tmp_path / "nonexistent-skills",
        world_repository_factory=factory,
    )
    assert paths == [tmp_path / "world.events.jsonl"]
