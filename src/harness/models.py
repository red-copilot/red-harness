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
    type: Literal["python", "docker"] = "python"
    entrypoint: str = "verifier.py"
    image: str | None = None
    command: list[str] = Field(default_factory=list)
    network: Literal["none", "environment"] = "none"
    timeout: int = Field(default=120, ge=1, le=3600)
    runtime: str | None = None

    @model_validator(mode="after")
    def validate_verifier(self) -> VerificationSpec:
        if self.type == "docker" and not self.image:
            raise ValueError("docker verifier requires image")
        if self.type == "python" and self.runtime is not None:
            raise ValueError("runtime is only valid for docker verifier")
        return self


class TaskSpec(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    api_version: Literal["harness/v1"] = Field(alias="apiVersion")
    id: str = Field(min_length=1)
    name: str = Field(min_length=1)
    category: str = "general"
    difficulty: str | None = None
    tags: list[str] = Field(default_factory=list)
    objective: ObjectiveSpec
    environment: EnvironmentSpec = Field(default_factory=EnvironmentSpec)
    budgets: BudgetSpec = Field(default_factory=BudgetSpec)
    verification: VerificationSpec = Field(default_factory=VerificationSpec)


class PiSpec(BaseModel):
    binary: str = Field(default="pi", min_length=1)
    session_mode: Literal["json", "rpc"] = "json"
    launcher_args: list[str] = Field(default_factory=list)
    provider: str | None = None
    model: str = Field(min_length=1)
    thinking: Literal["off", "minimal", "low", "medium", "high", "xhigh", "max"] | None = None
    tools: list[str] = Field(default_factory=lambda: ["read", "bash", "edit", "write"])
    approve_project: bool = False
    context_files: bool = False
    extensions: bool = False
    skills: bool = False
    prompt_templates: bool = False
    themes: bool = False
    mcp: bool = False
    offline: bool = True
    env_passthrough: list[str] = Field(default_factory=list)
    cap_add: list[str] = Field(default_factory=lambda: ["NET_RAW"])
    extra_args: list[str] = Field(default_factory=list)


class AgentSpec(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    api_version: Literal["harness/v1"] = Field(default="harness/v1", alias="apiVersion")
    id: str = Field(min_length=1)
    type: Literal["cli", "docker", "pi"] = "cli"
    command: list[str] = Field(default_factory=list)
    cwd: str | None = None
    env: dict[str, str] = Field(default_factory=dict)
    image: str | None = None
    network: Literal["environment", "none", "host"] = "environment"
    network_profile: Literal["offline", "benchmark-only", "unrestricted"] = "unrestricted"
    runtime: str | None = None
    pi: PiSpec | None = None

    @model_validator(mode="after")
    def validate_adapter(self) -> AgentSpec:
        if self.type == "cli" and not self.command:
            raise ValueError("cli agents require command")
        if self.type == "docker" and not self.image:
            raise ValueError("docker agents require image")
        if self.type == "pi":
            if self.pi is None:
                raise ValueError("pi agents require pi configuration")
            if not self.image:
                raise ValueError("pi agents require a container image")
            if self.command:
                raise ValueError("pi agents use pi.launcher_args instead of command")
        if self.type not in {"docker", "pi"} and self.runtime is not None:
            raise ValueError("runtime is only valid for container agents")
        return self


class SuiteTaskSpec(BaseModel):
    path: str = Field(min_length=1)
    weight: float = Field(default=1.0, gt=0)


class SuiteSpec(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    api_version: Literal["harness/v1"] = Field(alias="apiVersion")
    id: str = Field(min_length=1)
    repeat: int = Field(default=1, ge=1)
    base_seed: int = Field(default=0, ge=0)
    workers: int = Field(default=1, ge=1, le=64)
    pass_k: list[int] = Field(default_factory=lambda: [1])
    tasks: list[SuiteTaskSpec] = Field(min_length=1)

    @model_validator(mode="after")
    def validate_pass_k(self) -> SuiteSpec:
        if not self.pass_k:
            raise ValueError("pass_k must contain at least one k")
        if len(set(self.pass_k)) != len(self.pass_k):
            raise ValueError("pass_k values must be unique")
        if any(k < 1 or k > self.repeat for k in self.pass_k):
            raise ValueError("each pass_k value must be between 1 and repeat")
        self.pass_k.sort()
        return self


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


def load_suite(path: str | Path) -> SuiteSpec:
    return SuiteSpec.model_validate(_load_yaml(Path(path)))
