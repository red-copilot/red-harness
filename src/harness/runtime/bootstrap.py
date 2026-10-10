from __future__ import annotations

from collections.abc import Callable, Iterable
from dataclasses import dataclass
from pathlib import Path

from ..progress import ProgressLedger
from ..secureio import read_regular_text
from ..skills import load_skills
from ..solver_loop import SolverLoop
from ..trace import TraceRecorder
from ..verification import VerifierRegistry
from ..world import (
    Entity,
    Goal,
    SQLiteWorldRepository,
    WorldContextBuilder,
    WorldRepository,
)
from .agent_workspace import sync_agent_workspace
from .solver_profile import DEFAULT_SOLVER_PROFILE, SolverProfile, solver_profile_settings


@dataclass(frozen=True)
class SolverRuntime:
    world: WorldRepository
    context_builder: WorldContextBuilder
    progress: ProgressLedger
    verifier_registry: VerifierRegistry
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
    verifier_registry: VerifierRegistry | None = None,
    agent_workspace: Path | None = None,
    resume_existing: bool = False,
    solver_profile: SolverProfile = DEFAULT_SOLVER_PROFILE,
) -> SolverRuntime:
    """Initialize a solver run without coupling it to a benchmark or agent.

    Run directory creation and any world-state resume/import must occur before
    this call. Target entries are written before the initial context is rendered.
    """
    world = world_repository_factory(run_dir / "world.events.jsonl")
    if not resume_existing:
        world.upsert("goal", goal)
    for target in targets:
        world.upsert("entity", target, actor=target_actor or actor)

    context_builder = WorldContextBuilder()
    world_context_enabled, planner_enabled = solver_profile_settings(solver_profile)
    (run_dir / "world.context.txt").write_text(
        context_builder.render(world.snapshot, query=context_query) if world_context_enabled else "",
        encoding="utf-8",
    )
    progress = (
        ProgressLedger.model_validate_json(
            read_regular_text(run_dir / "progress.json", max_bytes=8 * 1024 * 1024)
        )
        if resume_existing
        else ProgressLedger(active_goal=goal.id, budget_limits=budget_limits or {})
    )
    verifier_registry = verifier_registry or VerifierRegistry()
    progress.write(run_dir / "progress.json")
    sync_workspace = lambda: (
        sync_agent_workspace(run_dir=run_dir, workspace=agent_workspace)
        if agent_workspace is not None
        else None
    )
    loop = SolverLoop(
        world=world,
        progress=progress,
        run_dir=run_dir,
        trace=trace,
        actor=actor,
        context_query=context_query,
        planned_world_revision=world.snapshot.revision,
        context_builder=context_builder,
        verifier_registry=verifier_registry,
        world_inbox_path=(agent_workspace / "world.inbox.jsonl")
        if agent_workspace is not None
        else None,
        sync_agent_views=sync_workspace,
        skills=load_skills(skills_root) if planner_enabled else [],
        planner_enabled=planner_enabled,
        world_context_enabled=world_context_enabled,
        processed_event_ids=processed_event_ids,
    )
    return SolverRuntime(
        world=world,
        context_builder=context_builder,
        progress=progress,
        verifier_registry=verifier_registry,
        loop=loop,
    )
