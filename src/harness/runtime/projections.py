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
    render_context: bool = True,
) -> None:
    """Persist final objective state and render the final snapshot."""
    goal.status = "completed" if success else "failed"
    goal.attributes.update({"run_status": status, "score": score, "success": success})
    world.upsert("goal", goal)
    if render_context:
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
    strict_profile = agent.network_profile in {
        "offline",
        "target-only",
        "model-allowed",
        "fully-offline",
    }
    enforcement = {
        "fully-offline": "docker-none",
        "offline": "docker-none",
        "target-only": "docker-internal-network",
        "model-allowed": "docker-internal-network-gateway-proxy",
    }.get(agent.network_profile)
    return {
        "profile": agent.network_profile,
        "enforcement": (
            enforcement
            if enforcement and agent.type in {"docker", "pi"}
            else "advisory"
            if agent.network_profile == "benchmark-only" or strict_profile
            else "unrestricted"
        ),
    }
