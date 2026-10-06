from __future__ import annotations

import fnmatch
from typing import Protocol

from pydantic import BaseModel, Field

from .skills import SkillSpec, StateSelector
from .world import WorldSnapshot


class PlanCandidate(BaseModel):
    skill_id: str
    score: float = Field(ge=0.0, le=1.0)
    novelty: float = Field(ge=0.0, le=1.0)
    matched_requirements: int = Field(ge=0)
    expected_outputs: list[StateSelector] = Field(default_factory=list)


class Planner(Protocol):
    def propose(
        self,
        snapshot: WorldSnapshot,
        skills: list[SkillSpec],
        *,
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
    """Reference planner that ranks applicable skills without executing them."""

    def propose(
        self,
        snapshot: WorldSnapshot,
        skills: list[SkillSpec],
        *,
        limit: int = 5,
    ) -> list[PlanCandidate]:
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
            friction = (skill.cost + skill.risk + skill.noise) / 3.0
            score = 0.65 * novelty + 0.35 * (1.0 - friction)

            candidates.append(
                PlanCandidate(
                    skill_id=skill.id,
                    score=max(0.0, min(1.0, score)),
                    novelty=novelty,
                    matched_requirements=len(skill.requires),
                    expected_outputs=skill.produces,
                )
            )

        candidates.sort(key=lambda item: (-item.score, item.skill_id))
        return candidates[:limit]
