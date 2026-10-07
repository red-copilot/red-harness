from datetime import UTC, datetime, timedelta

from redharness.planner import HeuristicSkillPlanner, RollingHorizonPlanner
from redharness.progress import ProgressLedger
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



def test_rolling_horizon_plan_uses_progress_and_expected_outputs() -> None:
    snapshot = WorldSnapshot(
        revision=7,
        capabilities={
            "cap-1": Capability(
                id="cap-1",
                type="network.reachability",
                subject="agent",
                scope="target",
            )
        },
    )
    skills = [
        SkillSpec(
            id="service-discovery",
            description="Enumerate reachable services",
            requires=[{"kind": "capability", "type": "network.reachability"}],
            produces=[
                {"kind": "observation", "type": "network.service"},
                {"kind": "entity", "type": "network.service"},
            ],
            cost=0.2,
            risk=0.1,
            noise=0.2,
        ),
        SkillSpec(
            id="banner-grab",
            description="Collect service banners",
            requires=[{"kind": "capability", "type": "network.reachability"}],
            produces=[{"kind": "observation", "type": "network.banner"}],
            cost=0.3,
            risk=0.1,
            noise=0.2,
        ),
    ]
    progress = ProgressLedger(
        current_subgoal="enumerate services",
        no_progress_count=2,
    )

    plan = RollingHorizonPlanner().propose(
        snapshot,
        skills,
        progress=progress,
        horizon=2,
    )

    assert plan.world_revision == 7
    assert plan.horizon == 2
    assert plan.current_subgoal == "enumerate services"
    assert plan.replan_required is True
    assert "no_progress_threshold" in plan.replan_reasons
    assert len(plan.actions) == 2
    assert plan.actions[0].rank == 1
    assert plan.actions[0].expected_observations
    assert "no_progress_threshold" in plan.actions[0].replan_triggers
    assert "subgoal=enumerate services" in plan.actions[0].rationale


def test_rolling_horizon_clamps_to_three_actions() -> None:
    snapshot = WorldSnapshot()
    skills = [
        SkillSpec(
            id=f"skill-{index}",
            description=f"Skill {index}",
            produces=[{"kind": "observation", "type": f"obs.{index}"}],
        )
        for index in range(5)
    ]

    plan = RollingHorizonPlanner().propose(snapshot, skills, horizon=20)

    assert plan.horizon == 3
    assert len(plan.actions) == 3



def test_rolling_horizon_consumes_verifier_replan_state() -> None:
    snapshot = WorldSnapshot()
    progress = ProgressLedger(
        replan_reasons=["expected_observation_missing"],
        last_verification={
            "status": "inconclusive",
            "expected_observation": "admin endpoint exists",
            "actual_observation": None,
            "evidence": [],
            "replan_required": True,
            "replan_reasons": ["expected_observation_missing"],
        },
    )

    plan = RollingHorizonPlanner().propose(
        snapshot,
        [],
        progress=progress,
        horizon=1,
    )

    assert plan.replan_required is True
    assert plan.replan_reasons == ["expected_observation_missing"]
    assert plan.previous_verification_status == "inconclusive"
