from __future__ import annotations

from pathlib import Path
from typing import Literal

import yaml
from pydantic import BaseModel, ConfigDict, Field, model_validator


class ObjectiveSpec(BaseModel):
    description: str = Field(min_length=1)


class BudgetSpec(BaseModel):
    wall_time: int = Field(default=3600, ge=1)
    max_tokens: int | None = Field(default=None, ge=1)
    max_model_calls: int | None = Field(default=None, ge=1)
    max_tool_calls: int | None = Field(default=None, ge=1)
    max_cost_usd: float | None = Field(default=None, gt=0)


class EnvironmentSpec(BaseModel):
    provider: Literal["none", "docker-compose"] = "none"
    manifest: str | None = None

    @model_validator(mode="after")
    def validate_manifest(self) -> EnvironmentSpec:
        if self.provider == "docker-compose" and not self.manifest:
            raise ValueError("docker-compose environments require manifest")
        return self


class VerificationSpec(BaseModel):
    type: Literal["python"] = "python"
    entrypoint: str = "verifier.py"


class TaskSpec(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    api_version: Literal["redharness/v1"] = Field(alias="apiVersion")
    id: str = Field(min_length=1)
    name: str = Field(min_length=1)
    category: str = "general"
    difficulty: str | None = None
    tags: list[str] = Field(default_factory=list)
    objective: ObjectiveSpec
    environment: EnvironmentSpec = Field(default_factory=EnvironmentSpec)
    budgets: BudgetSpec = Field(default_factory=BudgetSpec)
    verification: VerificationSpec = Field(default_factory=VerificationSpec)


class AgentSpec(BaseModel):
    api_version: Literal["redharness/v1"] = Field(default="redharness/v1", alias="apiVersion")
    id: str = Field(min_length=1)
    type: Literal["cli"] = "cli"
    command: list[str] = Field(min_length=1)
    cwd: str | None = None
    env: dict[str, str] = Field(default_factory=dict)


class VerificationResult(BaseModel):
    success: bool
    score: float = Field(ge=0, le=100)
    message: str | None = None
    milestones: dict[str, bool] = Field(default_factory=dict)


def _load_yaml(path: Path) -> dict:
    with path.open("r", encoding="utf-8") as handle:
        data = yaml.safe_load(handle)
    if not isinstance(data, dict):
        raise TypeError(f"{path} must contain a YAML object")
    return data


def load_task(path: str | Path) -> TaskSpec:
    return TaskSpec.model_validate(_load_yaml(Path(path)))


def load_agent(path: str | Path) -> AgentSpec:
    return AgentSpec.model_validate(_load_yaml(Path(path)))
