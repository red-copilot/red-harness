"""Versioned data contracts; parsing a producer label never authenticates it."""

from __future__ import annotations

import hashlib
import json
from collections.abc import AsyncIterator
from pathlib import Path, PurePosixPath
from typing import Annotated, Literal, Protocol, Self

from pydantic import (
    AwareDatetime,
    BaseModel,
    ConfigDict,
    Field,
    StringConstraints,
    field_validator,
    model_validator,
)

Name = Annotated[str, StringConstraints(strict=True, strip_whitespace=True, min_length=1)]
EnvName = Annotated[str, StringConstraints(strict=True, pattern=r"^[A-Z_][A-Z0-9_]*$")]
Digest = Annotated[
    str,
    StringConstraints(
        strict=True,
        min_length=64,
        max_length=64,
        pattern=r"^[0-9a-f]{64}$",
    ),
]
Counter = Annotated[int, Field(strict=True, ge=0)]
Limit = Annotated[int, Field(strict=True, gt=0)]
Amount = Annotated[float, Field(strict=True, ge=0, allow_inf_nan=False)]
PositiveAmount = Annotated[float, Field(strict=True, gt=0, allow_inf_nan=False)]
Producer = Literal["local_verifier", "tsec_evaluator"]
VerdictStatus = Literal["passed", "failed", "unknown"]
StopReason = Literal[
    "completed",
    "timeout",
    "budget_exceeded",
    "cancelled",
    "setup_error",
    "agent_error",
    "evaluation_error",
    "interrupted",
]
VerificationError = Literal[
    "unregistered_producer",
    "artifact_mismatch",
    "invalid_response",
    "evaluator_unavailable",
    "evaluator_unknown",
    "evidence_unavailable",
    "evidence_mismatch",
]


class Contract(BaseModel):
    model_config = ConfigDict(
        extra="forbid",
        frozen=True,
        populate_by_name=True,
        revalidate_instances="always",
    )


class TaskSpec(Contract):
    id: Name
    description: Name
    input_dir: Path | None = None


class PiSpec(Contract):
    image: Name
    provider: Name
    model: Name
    version: Literal["1.0.4"] = "1.0.4"
    credential_env: EnvName | None = None


class BudgetSpec(Contract):
    wall_time: PositiveAmount
    cleanup_timeout: PositiveAmount = 30
    max_tokens: Limit | None = None
    max_model_calls: Limit | None = None
    max_tool_calls: Limit | None = None
    max_cost_usd: PositiveAmount | None = None


class LocalEvaluationSpec(Contract):
    kind: Literal["local"]
    image: Name
    command: tuple[Name, ...] = Field(min_length=1)


class TSecEvaluationSpec(Contract):
    kind: Literal["tsec"]
    base_url_env: EnvName = "BENCHMARK_BASE_URL"
    token_env: EnvName = "BENCHMARK_TOKEN"


class RunSpec(Contract):
    api_version: Literal["harness/v2"] = Field(alias="apiVersion")
    task: TaskSpec
    pi: PiSpec
    budgets: BudgetSpec
    evaluation: Annotated[LocalEvaluationSpec | TSecEvaluationSpec, Field(discriminator="kind")]
    seed: Counter = 0

    @model_validator(mode="after")
    def separate_credentials(self) -> Self:
        if isinstance(self.evaluation, TSecEvaluationSpec) and self.pi.credential_env in {
            self.evaluation.token_env,
            self.evaluation.base_url_env,
        }:
            raise ValueError("Pi cannot receive platform credential selectors")
        return self


def relative_artifact_path(value: str) -> str:
    path = PurePosixPath(value)
    if (
        not value
        or path.is_absolute()
        or path.as_posix() != value
        or ".." in path.parts
        or value == "."
        or "\\" in value
        or ":" in value
        or any(ord(char) < 32 for char in value)
    ):
        raise ValueError("artifact reference must be a canonical relative POSIX path")
    return value


class ArtifactRef(Contract):
    path: Name
    sha256: Digest

    @field_validator("path")
    @classmethod
    def validate_path(cls, value: str) -> str:
        return relative_artifact_path(value)


class FrozenArtifacts(Contract):
    """Host-owned manifest; the later Runner is responsible for freezing files."""

    root: Path
    files: tuple[ArtifactRef, ...] = ()

    @model_validator(mode="after")
    def unique_paths(self) -> Self:
        if not self.root.is_absolute():
            raise ValueError("snapshot root must be absolute")
        if len({ref.path for ref in self.files}) != len(self.files):
            raise ValueError("snapshot artifact paths must be unique")
        return self

    @property
    def digest(self) -> str:
        manifest = [ref.model_dump() for ref in sorted(self.files, key=lambda ref: ref.path)]
        payload = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode()
        return hashlib.sha256(payload).hexdigest()

    def verify(self) -> bool:
        """Check regular files and manifest hashes without following snapshot symlinks."""
        try:
            if self.root.is_symlink() or not self.root.is_dir():
                return False
            for ref in self.files:
                path = self.root / ref.path
                for component in (path, *path.parents):
                    if component == self.root.parent:
                        break
                    if component.is_symlink():
                        return False
                if not path.is_file():
                    return False
                with path.open("rb") as handle:
                    digest = hashlib.file_digest(handle, "sha256").hexdigest()
                if digest != ref.sha256:
                    return False
        except OSError:
            return False
        return True


class EvidenceRef(Contract):
    id: Name
    run_id: Name
    task_id: Name
    producer: Producer
    captured_at: AwareDatetime
    artifact_ref: Name
    sha256: Digest
    input_digest: Digest

    @field_validator("artifact_ref")
    @classmethod
    def validate_path(cls, value: str) -> str:
        return relative_artifact_path(value)


class EvaluationReply(Contract):
    """Untrusted evaluator response; producer and evidence are assigned by the host."""

    status: VerdictStatus
    score: Amount | None = None
    platform_cumulative_score: Amount | None = None


class ObjectiveVerdict(EvaluationReply):
    run_id: Name
    task_id: Name
    evidence: tuple[EvidenceRef, ...] = ()
    error_code: VerificationError | None = None

    @model_validator(mode="after")
    def consistent_evidence(self) -> Self:
        if self.status != "unknown" and (not self.evidence or self.error_code is not None):
            raise ValueError("conclusive verdict requires evidence and no verification error")
        if len({ref.id for ref in self.evidence}) != len(self.evidence):
            raise ValueError("evidence IDs must be unique")
        if any(ref.run_id != self.run_id or ref.task_id != self.task_id for ref in self.evidence):
            raise ValueError("evidence belongs to a different run or task")
        if len({ref.input_digest for ref in self.evidence}) > 1:
            raise ValueError("evidence refers to conflicting artifact snapshots")
        return self


class UsageMetrics(Contract):
    input_tokens: Counter | None = None
    output_tokens: Counter | None = None
    total_tokens: Counter | None = None
    model_calls: Counter | None = None
    tool_calls: Counter | None = None
    cost_usd: Amount | None = None


class CleanupResult(Contract):
    ok: bool | None = Field(default=None, strict=True)
    error_code: Literal["timeout", "release_failed", "interrupted"] | None = None

    @model_validator(mode="after")
    def consistent_status(self) -> Self:
        if self.ok is True and self.error_code is not None:
            raise ValueError("successful cleanup cannot contain an error")
        if self.ok is False and self.error_code is None:
            raise ValueError("failed cleanup requires an error code")
        return self


class RunResult(Contract):
    api_version: Literal["harness/v2"] = Field(default="harness/v2", alias="apiVersion")
    run_id: Name
    task_id: Name
    stop_reason: StopReason
    objective: ObjectiveVerdict
    metrics: UsageMetrics = Field(default_factory=UsageMetrics)
    cleanup: CleanupResult = Field(default_factory=CleanupResult)
    pi_version: Literal["1.0.4"] = "1.0.4"

    @model_validator(mode="after")
    def consistent_objective(self) -> Self:
        if self.objective.run_id != self.run_id or self.objective.task_id != self.task_id:
            raise ValueError("objective belongs to a different run or task")
        if self.stop_reason != "completed" and self.objective.status != "unknown":
            raise ValueError("abnormal termination cannot establish an objective verdict")
        return self


class AgentEvent(Contract):
    """Agent observations only; never accepted as an EvaluationReply."""

    id: Name
    type: Name
    text: str | None = None
    tool_exit_code: Annotated[int, Field(strict=True)] | None = None
    process_exit_code: Annotated[int, Field(strict=True)] | None = None
    usage: UsageMetrics | None = None


class PiSession(Protocol):
    async def start(self, task: TaskSpec, pi: PiSpec, workspace: Path) -> None: ...

    def events(self) -> AsyncIterator[AgentEvent]: ...

    async def close(self, reason: StopReason) -> int | None:
        """Return the observed process exit code, or None if exit is unconfirmed."""
        ...


class CaseAdapter(Protocol):
    async def prepare(self, spec: RunSpec, workspace: Path) -> TaskSpec: ...

    async def evaluate(self, artifacts: FrozenArtifacts) -> EvaluationReply: ...

    async def release(self) -> None: ...
