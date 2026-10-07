from __future__ import annotations

from pathlib import Path
from typing import Any, Literal

import yaml
from pydantic import BaseModel, ConfigDict, Field

from .world.models import WorldObjectKind


class StateSelector(BaseModel):
    kind: WorldObjectKind
    type: str = Field(min_length=1)
    scope: str | None = None


class SkillSpec(BaseModel):
    """Planner-facing metadata for a reusable offensive technique."""

    model_config = ConfigDict(populate_by_name=True)

    api_version: Literal["redharness/skill/v1"] = Field(
        default="redharness/skill/v1",
        alias="apiVersion",
    )
    id: str = Field(min_length=1)
    domain: str = Field(default="general", min_length=1)
    description: str = Field(min_length=1)
    tags: list[str] = Field(default_factory=list)
    requires: list[StateSelector] = Field(default_factory=list)
    produces: list[StateSelector] = Field(default_factory=list)
    cost: float = Field(default=0.5, ge=0.0, le=1.0)
    risk: float = Field(default=0.5, ge=0.0, le=1.0)
    noise: float = Field(default=0.5, ge=0.0, le=1.0)
    metadata: dict[str, Any] = Field(default_factory=dict)


def load_skill(path: str | Path) -> SkillSpec:
    skill_path = Path(path)
    with skill_path.open("r", encoding="utf-8") as handle:
        data = yaml.safe_load(handle)
    if not isinstance(data, dict):
        raise TypeError(f"{skill_path} must contain a YAML object")
    return SkillSpec.model_validate(data)


def load_skills(root: str | Path) -> list[SkillSpec]:
    skill_root = Path(root)
    if not skill_root.exists():
        return []
    skills: list[SkillSpec] = []
    for path in sorted(skill_root.rglob("*.yaml")):
        skills.append(load_skill(path))
    return skills


def scan_skills(root: str | Path) -> list[dict]:
    skill_root = Path(root)
    results: list[dict] = []
    if not skill_root.exists():
        return results

    for path in sorted(skill_root.rglob("*.yaml")):
        try:
            skill = load_skill(path)
        except (ValueError, TypeError) as exc:
            results.append(
                {
                    "path": str(path),
                    "valid": False,
                    "error": str(exc),
                }
            )
            continue
        results.append(
            {
                "path": str(path),
                "valid": True,
                "id": skill.id,
                "domain": skill.domain,
                "description": skill.description,
                "tags": skill.tags,
                "requires": [item.model_dump() for item in skill.requires],
                "produces": [item.model_dump() for item in skill.produces],
                "cost": skill.cost,
                "risk": skill.risk,
                "noise": skill.noise,
            }
        )
    return results
