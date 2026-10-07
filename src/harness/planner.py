from __future__ import annotations

import fnmatch
import re
from typing import Protocol

from pydantic import BaseModel, Field

from .progress import ProgressLedger
from .skills import SkillSpec, StateSelector
from .world import WorldSnapshot


_TOKEN_RE = re.compile(r"[A-Za-z0-9]{3,}")


class PlanCandidate(BaseModel):
    skill_id: str
    score: float = Field(ge=0.0, le=1.0)
    novelty: float = Field(ge=0.0, le=1.0)
    goal_relevance: float = Field(ge=0.0, le=1.0)
    information_gain: float = Field(ge=0.0, le=1.0)
    success_prior: float = Field(ge=0.0, le=1.0)
    repetition_penalty: float = Field(ge=0.0, le=1.0)
    failure_penalty: float = Field(ge=0.0, le=1.0)
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
    planner: str = "rolling-horizon-skill-v2"
    world_revision: int
    horizon: int = Field(ge=1, le=3)
    current_subgoal: str | None = None
    no_progress_count: int = 0
    replan_required: bool = False
    replan_reasons: list[str] = Field(default_factory=list)
    previous_verification_status: str | None = None
    actions: list[PlannedAction] = Field(default_factory=list)


class Planner(Protocol):
    def propose(
        self,
        snapshot: WorldSnapshot,
        skills: list[SkillSpec],
        *,
        progress: ProgressLedger | None = None,
        goal_text: str | None = None,
        limit: int = 5,
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
    for item in collection.values():
        if not item.is_valid_at():
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


class HeuristicSkillPlanner:
    """Deterministic skill ranker using goal, information value, and failure memory."""

    @staticmethod
    def _tokens(value: str | None) -> set[str]:
        if not value:
            return set()
        return {token.lower() for token in _TOKEN_RE.findall(value)}

    @classmethod
    def _goal_relevance(
        cls,
        skill: SkillSpec,
        *,
        goal_text: str | None,
        current_subgoal: str | None,
    ) -> float:
        query_tokens = cls._tokens(
            " ".join(value for value in (goal_text, current_subgoal) if value)
        )
        if not query_tokens:
            return 0.5
        skill_text = " ".join(
            [
                skill.id,
                skill.domain,
                skill.description,
                *skill.tags,
                *(selector.type for selector in skill.requires),
                *(selector.type for selector in skill.produces),
            ]
        )
        skill_tokens = cls._tokens(skill_text)
        if not skill_tokens:
            return 0.0
        overlap = len(query_tokens & skill_tokens)
        return min(1.0, overlap / max(1, len(query_tokens)))

    @staticmethod
    def _observed_success_prior(
        skill: SkillSpec,
        *,
        progress: ProgressLedger,
    ) -> float:
        attempts = progress.skill_attempts.get(skill.id, 0)
        verified = progress.skill_verified.get(skill.id, 0)
        if attempts <= 0:
            return skill.success_prior
        observed = (verified + 1.0) / (attempts + 2.0)
        return 0.5 * skill.success_prior + 0.5 * observed

    def propose(
        self,
        snapshot: WorldSnapshot,
        skills: list[SkillSpec],
        *,
        progress: ProgressLedger | None = None,
        goal_text: str | None = None,
        limit: int = 5,
    ) -> list[PlanCandidate]:
        progress = progress or ProgressLedger()
        candidates: list[PlanCandidate] = []
        for skill in skills:
            if not all(_selector_matches(snapshot, selector) for selector in skill.requires):
                continue

            novel = [
                selector
                for selector in skill.produces
                if not _selector_matches(snapshot, selector)
            ]
            novelty = len(novel) / len(skill.produces) if skill.produces else 0.0
            goal_relevance = self._goal_relevance(
                skill,
                goal_text=goal_text,
                current_subgoal=progress.current_subgoal,
            )
            information_gain = 0.6 * novelty + 0.4 * skill.information_gain
            success_prior = self._observed_success_prior(skill, progress=progress)
            attempts = progress.skill_attempts.get(skill.id, 0)
            failures = progress.skill_failures.get(skill.id, 0)
            repetition_penalty = min(1.0, attempts / 3.0)
            failure_penalty = min(1.0, failures / max(1, attempts))
            friction = (skill.cost + skill.risk + skill.noise) / 3.0

            raw_score = (
                0.30 * goal_relevance
                + 0.25 * information_gain
                + 0.20 * success_prior
                + 0.15 * novelty
                + 0.10 * (1.0 - friction)
                - 0.15 * repetition_penalty
                - 0.20 * failure_penalty
            )
            score = max(0.0, min(1.0, raw_score))

            candidates.append(
                PlanCandidate(
                    skill_id=skill.id,
                    score=score,
                    novelty=novelty,
                    goal_relevance=goal_relevance,
                    information_gain=information_gain,
                    success_prior=success_prior,
                    repetition_penalty=repetition_penalty,
                    failure_penalty=failure_penalty,
                    matched_requirements=len(skill.requires),
                    expected_outputs=skill.produces,
                )
            )

        candidates.sort(key=lambda item: (-item.score, item.skill_id))
        return candidates[:limit]


class RollingHorizonPlanner:
    """Build a short action horizon over goal-aware skill candidates."""

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
        goal_text: str | None = None,
        horizon: int = 3,
    ) -> RollingPlan:
        horizon = max(1, min(3, horizon))
        progress = progress or ProgressLedger()
        by_id = {skill.id: skill for skill in skills}
        candidates = self.candidate_planner.propose(
            snapshot,
            skills,
            progress=progress,
            goal_text=goal_text,
            limit=max(horizon * 3, horizon),
        )

        actions: list[PlannedAction] = []
        for candidate in candidates[:horizon]:
            skill = by_id[candidate.skill_id]
            expected = self._expected_observations(candidate)
            rationale_parts = [
                f"goal={candidate.goal_relevance:.2f}",
                f"info_gain={candidate.information_gain:.2f}",
                f"success_prior={candidate.success_prior:.2f}",
                f"novelty={candidate.novelty:.2f}",
                f"repetition_penalty={candidate.repetition_penalty:.2f}",
                f"failure_penalty={candidate.failure_penalty:.2f}",
                f"score={candidate.score:.2f}",
            ]
            if progress.current_subgoal:
                rationale_parts.append(f"subgoal={progress.current_subgoal}")
            if progress.no_progress_count:
                rationale_parts.append(
                    f"no_progress_count={progress.no_progress_count}"
                )

            replan_triggers = [
                "action_failed",
                "expected_observation_missing",
                "world_revision_changed",
            ]
            if progress.no_progress_count >= 2:
                replan_triggers.append("no_progress_threshold")
            if progress.hypotheses:
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
            actions=actions,
        )
