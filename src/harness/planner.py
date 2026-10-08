from __future__ import annotations

import fnmatch
import re
from typing import Protocol

from pydantic import BaseModel, Field

from .progress import ProgressLedger
from .skills import SkillSpec, StateSelector
from .world import WorldSnapshot


class PlanCandidate(BaseModel):
    skill_id: str
    score: float = Field(ge=0.0, le=1.0)
    novelty: float = Field(ge=0.0, le=1.0)
    goal_relevance: float = Field(default=0.0, ge=0.0, le=1.0)
    information_gain: float = Field(default=0.0, ge=0.0, le=1.0)
    failure_penalty: float = Field(default=0.0, ge=0.0, le=1.0)
    budget_fit: float = Field(default=1.0, ge=0.0, le=1.0)
    matched_requirements: int = Field(ge=0)
    expected_outputs: list[StateSelector] = Field(default_factory=list)


class PlannedAction(BaseModel):
    rank: int = Field(ge=1)
    skill_id: str
    description: str
    score: float = Field(ge=0.0, le=1.0)
    expected_observations: list[str] = Field(default_factory=list)
    rationale: str
    replan_triggers: list[str] = Field(default_factory=list)


class RollingPlan(BaseModel):
    planner: str = "rolling-horizon-world-v2"
    world_revision: int
    horizon: int = Field(ge=1, le=3)
    current_subgoal: str | None = None
    no_progress_count: int = 0
    replan_required: bool = False
    replan_reasons: list[str] = Field(default_factory=list)
    previous_verification_status: str | None = None
    remaining_budget_fraction: float = Field(default=1.0, ge=0.0, le=1.0)
    actions: list[PlannedAction] = Field(default_factory=list)


class Planner(Protocol):
    def propose(
        self,
        snapshot: WorldSnapshot,
        skills: list[SkillSpec],
        *,
        limit: int = 5,
        progress: ProgressLedger | None = None,
    ) -> list[PlanCandidate]: ...


_COLLECTIONS = {
    "entity": "entities",
    "relation": "relations",
    "observation": "observations",
    "artifact": "artifacts",
    "capability": "capabilities",
    "hypothesis": "hypotheses",
    "goal": "goals",
    "action": "actions",
    "constraint": "constraints",
    "failure": "failures",
}


def _selector_matches(snapshot: WorldSnapshot, selector: StateSelector) -> bool:
    collection = getattr(snapshot, _COLLECTIONS[selector.kind])
    trust_rank = {"unclassified": 1, "claim": 1, "evidence": 2, "verified": 3}
    minimum_rank = trust_rank[selector.minimum_trust]
    for item in collection.values():
        if not item.is_valid_at():
            continue
        provenance = getattr(item, "provenance", None)
        epistemic_status = getattr(provenance, "epistemic_status", "unclassified")
        if trust_rank.get(epistemic_status, 0) < minimum_rank:
            continue
        type_name = getattr(item, "type", None)
        if type_name is None or not fnmatch.fnmatchcase(type_name, selector.type):
            continue
        if selector.scope is not None:
            scope = getattr(item, "scope", None)
            if scope is None or not fnmatch.fnmatchcase(scope, selector.scope):
                continue
        return True
    return False


def _goal_tokens(snapshot: WorldSnapshot, progress: ProgressLedger) -> set[str]:
    parts = [progress.current_subgoal or ""]
    if progress.active_goal and progress.active_goal in snapshot.goals:
        parts.append(snapshot.goals[progress.active_goal].description)
    parts.extend(
        goal.description
        for goal in snapshot.goals.values()
        if goal.status in {"active", "pending"} and goal.is_valid_at()
    )
    return {token.lower() for part in parts for token in re.findall(r"[A-Za-z0-9]{3,}", part)}


def _goal_relevance(skill: SkillSpec, goal_tokens: set[str]) -> float:
    if not goal_tokens:
        return 0.5
    skill_text = " ".join(
        [skill.domain, skill.description, *skill.tags]
        + [selector.type for selector in skill.produces]
    )
    skill_tokens = {token.lower() for token in re.findall(r"[A-Za-z0-9]{3,}", skill_text)}
    return len(goal_tokens & skill_tokens) / len(goal_tokens)


class HeuristicSkillPlanner:
    """Reference planner that ranks applicable skills without executing them."""

    def propose(
        self,
        snapshot: WorldSnapshot,
        skills: list[SkillSpec],
        *,
        limit: int = 5,
        progress: ProgressLedger | None = None,
    ) -> list[PlanCandidate]:
        progress = progress or ProgressLedger()
        goal_tokens = _goal_tokens(snapshot, progress)
        budget_remaining = progress.remaining_budget_fraction
        candidates: list[PlanCandidate] = []
        for skill in skills:
            if not all(_selector_matches(snapshot, selector) for selector in skill.requires):
                continue

            novel = [
                selector for selector in skill.produces if not _selector_matches(snapshot, selector)
            ]
            novelty = len(novel) / len(skill.produces) if skill.produces else 0.0
            relevance = _goal_relevance(skill, goal_tokens)
            failure_penalty = min(
                1.0,
                progress.failed_skill_counts.get(skill.id, 0) / 3.0,
            )
            budget_fit = 0.0 if skill.cost > budget_remaining and skill.cost > 0 else 1.0
            if budget_fit == 0.0:
                continue
            friction = (skill.cost + skill.risk + skill.noise) / 3.0
            score = (
                0.35 * novelty
                + 0.25 * relevance
                + 0.25 * (1.0 - friction)
                + 0.15 * budget_fit
                - 0.20 * failure_penalty
            )

            candidates.append(
                PlanCandidate(
                    skill_id=skill.id,
                    score=max(0.0, min(1.0, score)),
                    novelty=novelty,
                    goal_relevance=relevance,
                    information_gain=novelty,
                    failure_penalty=failure_penalty,
                    budget_fit=budget_fit,
                    matched_requirements=len(skill.requires),
                    expected_outputs=skill.produces,
                )
            )

        candidates.sort(key=lambda item: (-item.score, item.skill_id))
        return candidates[:limit]


class RollingHorizonPlanner:
    """Build a short action horizon over the existing skill candidate ranker."""

    def __init__(self, candidate_planner: HeuristicSkillPlanner | None = None) -> None:
        self.candidate_planner = candidate_planner or HeuristicSkillPlanner()

    @staticmethod
    def _expected_observations(candidate: PlanCandidate) -> list[str]:
        return [
            (
                f"{selector.kind}:{selector.type}"
                + (f" scope={selector.scope}" if selector.scope else "")
            )
            for selector in candidate.expected_outputs
        ]

    def propose(
        self,
        snapshot: WorldSnapshot,
        skills: list[SkillSpec],
        *,
        progress: ProgressLedger | None = None,
        horizon: int = 3,
    ) -> RollingPlan:
        horizon = max(1, min(3, horizon))
        progress = progress or ProgressLedger()
        by_id = {skill.id: skill for skill in skills}
        candidates = self.candidate_planner.propose(
            snapshot,
            skills,
            limit=max(horizon * 3, horizon),
            progress=progress,
        )

        actions: list[PlannedAction] = []
        for candidate in candidates[:horizon]:
            skill = by_id[candidate.skill_id]
            expected = self._expected_observations(candidate)
            rationale_parts = [
                f"novelty={candidate.novelty:.2f}",
                f"goal_relevance={candidate.goal_relevance:.2f}",
                f"information_gain={candidate.information_gain:.2f}",
                f"failure_penalty={candidate.failure_penalty:.2f}",
                f"budget_fit={candidate.budget_fit:.2f}",
                f"score={candidate.score:.2f}",
                f"matched_requirements={candidate.matched_requirements}",
            ]
            if progress.current_subgoal:
                rationale_parts.append(f"subgoal={progress.current_subgoal}")
            if progress.no_progress_count:
                rationale_parts.append(f"no_progress_count={progress.no_progress_count}")

            replan_triggers = [
                "action_failed",
                "expected_observation_missing",
                "world_revision_changed",
            ]
            if progress.no_progress_count >= 2:
                replan_triggers.append("no_progress_threshold")
            if any(hypothesis.status == "refuted" for hypothesis in progress.hypotheses.values()):
                replan_triggers.append("hypothesis_contradicted")
            for reason in progress.replan_reasons:
                if reason not in replan_triggers:
                    replan_triggers.append(reason)

            actions.append(
                PlannedAction(
                    rank=len(actions) + 1,
                    skill_id=skill.id,
                    description=skill.description,
                    score=candidate.score,
                    expected_observations=expected,
                    rationale="; ".join(rationale_parts),
                    replan_triggers=replan_triggers,
                )
            )

        previous_verification_status = None
        if progress.last_verification is not None:
            previous_verification_status = progress.last_verification.get("status")

        replan_reasons = list(progress.replan_reasons)
        if progress.no_progress_count >= 2 and "no_progress_threshold" not in replan_reasons:
            replan_reasons.append("no_progress_threshold")

        return RollingPlan(
            world_revision=snapshot.revision,
            horizon=horizon,
            current_subgoal=progress.current_subgoal,
            no_progress_count=progress.no_progress_count,
            replan_required=bool(replan_reasons),
            replan_reasons=replan_reasons,
            previous_verification_status=previous_verification_status,
            remaining_budget_fraction=progress.remaining_budget_fraction,
            actions=actions,
        )
