from pathlib import Path
from types import SimpleNamespace

from harness.runtime.projections import finalize_world_goal, network_result, world_result
from harness.world import Goal, SQLiteWorldRepository, WorldContextBuilder


def test_finalize_world_goal_and_result(tmp_path: Path) -> None:
    world = SQLiteWorldRepository(tmp_path / "world.events.jsonl")
    goal = Goal(id="goal:test", description="Test objective", status="active", priority=1.0)
    world.upsert("goal", goal)
    finalize_world_goal(
        world=world,
        goal=goal,
        context_builder=WorldContextBuilder(),
        run_dir=tmp_path,
        status="finished",
        success=True,
        score=1.0,
    )
    assert world.snapshot.revision >= 2
    assert goal.status == "completed"
    assert goal.attributes["score"] == 1.0
    assert (tmp_path / "world.context.txt").is_file()

    stats = SimpleNamespace(
        aci_accepted=2,
        aci_rejected=1,
        aci_world_mutations=3,
        inbox_accepted=4,
        inbox_rejected=5,
    )
    result = world_result(
        world=world, solver_loop=SimpleNamespace(stats=stats), agent_id="test"
    )
    assert result["aci"]["accepted_events"] == 2
    assert result["legacy_inbox"]["rejected"] == 5
    assert result["agent_authored_records"] == 0


def test_network_result_preserves_profiles() -> None:
    assert network_result(SimpleNamespace(network_profile="offline", type="pi")) == {
        "profile": "offline", "enforcement": "docker-none",
    }
    assert network_result(SimpleNamespace(network_profile="benchmark-only", type="pi"))[
        "enforcement"
    ] == "advisory"
    assert network_result(SimpleNamespace(network_profile="unrestricted", type="docker"))[
        "enforcement"
    ] == "unrestricted"
