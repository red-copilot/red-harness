from __future__ import annotations

from collections.abc import Callable, Iterable
from dataclasses import dataclass
from pathlib import Path

from ..progress import ProgressLedger
from ..skills import load_skills
from ..solver_loop import SolverLoop
from ..trace import TraceRecorder
from ..world import (
    Entity,
    Goal,
    SQLiteWorldRepository,
    WorldContextBuilder,
    WorldRepository,
)


@dataclass(frozen=True)
class SolverRuntime:
    world: WorldRepository
    context_builder: WorldContextBuilder
    progress: ProgressLedger
    loop: SolverLoop


def bootstrap_solver(
    *,
    run_dir: Path,
    trace: TraceRecorder,
    goal: Goal,
    actor: str,
    context_query: str,
    skills_root: str | Path,
    world_repository_factory: Callable[[Path], WorldRepository] = SQLiteWorldRepository,
    targets: Iterable[Entity] = (),
    target_actor: str | None = None,
    budget_limits: dict[str, float] | None = None,
    processed_event_ids: dict[str, str] | None = None,
) -> SolverRuntime:
    """Initialize a solver run without coupling it to a benchmark or agent.

    Run directory creation and any world-state resume/import must occur before
    this call. Target entries are written before the initial context is rendered.
    """
    world = world_repository_factory(run_dir / "world.events.jsonl")
    world.upsert("goal", goal)
    for target in targets:
        world.upsert("entity", target, actor=target_actor or actor)

    context_builder = WorldContextBuilder()
    (run_dir / "world.context.txt").write_text(
        context_builder.render(world.snapshot, query=context_query),
        encoding="utf-8",
    )
    progress = ProgressLedger(active_goal=goal.id, budget_limits=budget_limits or {})
    progress.write(run_dir / "progress.json")
    loop = SolverLoop(
        world=world,
        progress=progress,
        run_dir=run_dir,
        trace=trace,
        actor=actor,
        context_query=context_query,
        planned_world_revision=world.snapshot.revision,
        context_builder=context_builder,
        skills=load_skills(skills_root),
        processed_event_ids=processed_event_ids,
    )
    return SolverRuntime(world=world, context_builder=context_builder, progress=progress, loop=loop)
