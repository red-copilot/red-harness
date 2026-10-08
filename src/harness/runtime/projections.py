"""Shared world and network result projections for execution entry points."""
from __future__ import annotations

from pathlib import Path

from ..models import AgentSpec
from ..solver_loop import SolverLoop
from ..world import Goal, WorldContextBuilder, WorldRepository


def finalize_world_goal(
    *,
    world: WorldRepository,
    goal: Goal,
    context_builder: WorldContextBuilder,
    run_dir: Path,
    status: str,
    success: bool,
    score: float,
) -> None:
    """Persist final objective state and render the final snapshot."""
    goal.status = "completed" if success else "failed"
    goal.attributes.update({"run_status": status, "score": score, "success": success})
    world.upsert("goal", goal)
    (run_dir / "world.context.txt").write_text(
        context_builder.render(world.snapshot),
        encoding="utf-8",
    )


def world_result(
    *,
    world: WorldRepository,
    solver_loop: SolverLoop,
    agent_id: str,
) -> dict:
    return {
        "revision": world.snapshot.revision,
        "agent_authored_records": sum(
            1 for event in world.events() if event.actor == f"agent:{agent_id}"
        ),
        "aci": {
            "accepted_events": solver_loop.stats.aci_accepted,
            "rejected_events": solver_loop.stats.aci_rejected,
            "world_mutations": solver_loop.stats.aci_world_mutations,
        },
        "legacy_inbox": {
            "accepted": solver_loop.stats.inbox_accepted,
            "rejected": solver_loop.stats.inbox_rejected,
        },
    }


def network_result(agent: AgentSpec) -> dict:
    return {
        "profile": agent.network_profile,
        "enforcement": (
            "docker-none"
            if agent.network_profile == "offline" and agent.type in {"docker", "pi"}
            else "advisory"
            if agent.network_profile == "benchmark-only"
            else "unrestricted"
        ),
    }
