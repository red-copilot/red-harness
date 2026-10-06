from datetime import UTC, datetime, timedelta

from redharness.planner import HeuristicSkillPlanner
from redharness.skills import SkillSpec
from redharness.world import Capability, Observation, WorldSnapshot


def test_planner_filters_unmet_requirements() -> None:
    snapshot = WorldSnapshot()
    skills = [
        SkillSpec(
            id="needs-network",
            description="Needs reachability",
            requires=[{"kind": "capability", "type": "network.reachability"}],
            produces=[{"kind": "observation", "type": "network.service"}],
            cost=0.2,
            risk=0.1,
            noise=0.2,
        )
    ]

    assert HeuristicSkillPlanner().propose(snapshot, skills) == []


def test_planner_prefers_novel_outputs() -> None:
    snapshot = WorldSnapshot(
        capabilities={
            "cap-1": Capability(
                id="cap-1",
                type="network.reachability",
                subject="agent",
                scope="target",
            )
        }
    )
    novel = SkillSpec(
        id="novel",
        description="Produce a new observation",
        requires=[{"kind": "capability", "type": "network.*"}],
        produces=[{"kind": "observation", "type": "network.service"}],
        cost=0.2,
        risk=0.1,
        noise=0.2,
    )
    redundant = SkillSpec(
        id="redundant",
        description="Produce an observation already present",
        requires=[{"kind": "capability", "type": "network.*"}],
        produces=[{"kind": "observation", "type": "network.banner"}],
        cost=0.2,
        risk=0.1,
        noise=0.2,
    )
    snapshot.observations["obs-1"] = Observation(
        id="obs-1",
        type="network.banner",
        content={"value": "known"},
    )

    candidates = HeuristicSkillPlanner().propose(snapshot, [redundant, novel])

    assert [item.skill_id for item in candidates] == ["novel", "redundant"]
    assert candidates[0].novelty == 1.0
    assert candidates[1].novelty == 0.0



def test_planner_ignores_expired_capabilities() -> None:
    now = datetime.now(UTC)
    snapshot = WorldSnapshot(
        capabilities={
            "cap-expired": Capability(
                id="cap-expired",
                type="network.reachability",
                subject="agent",
                scope="target",
                valid_from=now - timedelta(minutes=10),
                expires_at=now - timedelta(minutes=1),
            )
        }
    )
    skill = SkillSpec(
        id="needs-live-network",
        description="Requires a currently valid capability",
        requires=[{"kind": "capability", "type": "network.reachability"}],
        produces=[{"kind": "observation", "type": "network.service"}],
    )

    assert HeuristicSkillPlanner().propose(snapshot, [skill]) == []
