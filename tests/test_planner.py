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
