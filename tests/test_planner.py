from datetime import UTC, datetime, timedelta

from harness.planner import HeuristicSkillPlanner, RollingHorizonPlanner
from harness.progress import ProgressLedger
from harness.skills import SkillSpec
from harness.world import Capability, Observation, WorldSnapshot


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


def test_rejected_submission_becomes_planner_replan_signal() -> None:
    progress = ProgressLedger()
    progress.record_submission(accepted=False, completed=False)

    assert progress.rejected_submissions == 1
    assert progress.no_progress_count == 1
    assert "benchmark_negative_feedback" in progress.replan_reasons

    skill = SkillSpec(
        id="alternate-path",
        description="Try an alternate path after negative benchmark feedback",
        produces=[{"kind": "observation", "type": "alternate.evidence"}],
    )
    plan = RollingHorizonPlanner().propose(
        WorldSnapshot(),
        [skill],
        progress=progress,
        horizon=1,
    )

    assert plan.replan_required is True
    assert "benchmark_negative_feedback" in plan.replan_reasons
    assert "benchmark_negative_feedback" in plan.actions[0].replan_triggers


def test_planner_prefers_goal_relevant_skill() -> None:
    snapshot = WorldSnapshot()
    web_skill = SkillSpec(
        id="web-admin-discovery",
        domain="web",
        description="Discover hidden admin endpoints",
        tags=["admin", "endpoint", "discovery"],
        produces=[{"kind": "observation", "type": "web.endpoint"}],
        information_gain=0.6,
        success_prior=0.5,
    )
    dns_skill = SkillSpec(
        id="dns-enumeration",
        domain="dns",
        description="Enumerate DNS records",
        tags=["dns", "records"],
        produces=[{"kind": "observation", "type": "dns.record"}],
        information_gain=0.6,
        success_prior=0.5,
    )

    candidates = HeuristicSkillPlanner().propose(
        snapshot,
        [dns_skill, web_skill],
        goal_text="find the hidden admin endpoint",
    )

    assert candidates[0].skill_id == "web-admin-discovery"
    assert candidates[0].goal_relevance > candidates[1].goal_relevance


def test_planner_penalizes_repeated_failed_skill() -> None:
    snapshot = WorldSnapshot()
    repeated = SkillSpec(
        id="repeated-path",
        description="Probe alternate endpoint",
        produces=[{"kind": "observation", "type": "web.endpoint"}],
        information_gain=0.8,
        success_prior=0.6,
    )
    fresh = SkillSpec(
        id="fresh-path",
        description="Probe alternate endpoint",
        produces=[{"kind": "observation", "type": "web.endpoint"}],
        information_gain=0.8,
        success_prior=0.6,
    )
    progress = ProgressLedger(
        skill_attempts={"repeated-path": 3},
        skill_failures={"repeated-path": 3},
    )

    candidates = HeuristicSkillPlanner().propose(
        snapshot,
        [repeated, fresh],
        progress=progress,
        goal_text="probe alternate endpoint",
    )

    assert candidates[0].skill_id == "fresh-path"
    repeated_candidate = next(item for item in candidates if item.skill_id == "repeated-path")
    assert repeated_candidate.repetition_penalty == 1.0
    assert repeated_candidate.failure_penalty == 1.0


def test_verification_updates_skill_history() -> None:
    progress = ProgressLedger()
    progress.record_intent(
        {
            "action_id": "a1",
            "skill_id": "service-discovery",
            "description": "discover services",
            "expected_observations": ["service found"],
        }
    )
    progress.record_verification(
        {
            "status": "contradicted",
            "expected_observation": "service found",
            "actual_observation": "service not found",
            "replan_reasons": ["expected_observation_contradicted"],
        }
    )

    assert progress.skill_attempts["service-discovery"] == 1
    assert progress.skill_failures["service-discovery"] == 1
