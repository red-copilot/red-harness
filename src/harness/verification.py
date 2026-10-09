from __future__ import annotations

from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Literal, Self

from pydantic import BaseModel, ConfigDict, Field, model_validator

EvidenceProducer = Literal["tool_adapter", "task_verifier", "benchmark_evaluator"]
VerdictProducer = Literal[
    "tool_adapter", "task_verifier", "benchmark_evaluator", "harness_verifier"
]
ActionStatus = Literal["verified", "contradicted", "pending"]
ExecutionStatus = Literal["succeeded", "failed", "unknown"]
ObjectiveStatus = Literal["verified", "contradicted", "inconclusive", "unavailable"]


class EvidenceRef(BaseModel):
    """Immutable provenance for evidence captured by a Harness-owned producer."""

    model_config = ConfigDict(frozen=True, extra="forbid")

    schema_version: Literal["harness/evidence-ref/v1"] = "harness/evidence-ref/v1"
    evidence_id: str = Field(min_length=1, max_length=256)
    run_id: str = Field(min_length=1, max_length=256)
    action_id: str = Field(min_length=1, max_length=256)
    producer: EvidenceProducer
    captured_at: datetime
    world_revision: int = Field(ge=0)
    artifact_ref: str | None = Field(default=None, min_length=1, max_length=2048)
    artifact_hash: str | None = Field(default=None, pattern=r"^[a-fA-F0-9]{64}$")

    @model_validator(mode="after")
    def validate_provenance(self) -> Self:
        if self.captured_at.tzinfo is None or self.captured_at.utcoffset() is None:
            raise ValueError("captured_at must be timezone-aware")
        if self.artifact_ref is None and self.artifact_hash is None:
            raise ValueError("evidence requires an artifact_ref or artifact_hash")
        return self


class ActionVerdict(BaseModel):
    """Action outcome with execution and observed-effect status kept separate."""

    model_config = ConfigDict(frozen=True, extra="forbid")

    schema_version: Literal["harness/action-verdict/v1"] = "harness/action-verdict/v1"
    run_id: str = Field(min_length=1, max_length=256)
    action_id: str = Field(min_length=1, max_length=256)
    producer: VerdictProducer
    captured_at: datetime
    world_revision: int = Field(ge=0)
    execution_status: ExecutionStatus
    status: ActionStatus
    expected_observation: str | None = None
    actual_observation: str | None = None
    evidence: tuple[EvidenceRef, ...] = ()
    replan_required: bool = False
    replan_reasons: tuple[str, ...] = ()

    @property
    def effect_status(self) -> ActionStatus:
        return self.status

    @model_validator(mode="after")
    def validate_verdict(self) -> Self:
        if self.captured_at.tzinfo is None or self.captured_at.utcoffset() is None:
            raise ValueError("captured_at must be timezone-aware")
        if self.execution_status == "failed" and self.status == "verified":
            raise ValueError("failed execution cannot verify an observed effect")
        if self.status == "verified" and not self.evidence:
            raise ValueError("verified action verdict requires evidence")
        evidence_ids: set[str] = set()
        for ref in self.evidence:
            if ref.evidence_id in evidence_ids:
                raise ValueError("action verdict cannot contain conflicting duplicate evidence IDs")
            evidence_ids.add(ref.evidence_id)
            if ref.run_id != self.run_id or ref.action_id != self.action_id:
                raise ValueError("evidence run_id and action_id must match the verdict")
            if ref.world_revision != self.world_revision:
                raise ValueError("evidence world_revision must match the verdict")
            if self.producer != "harness_verifier" and ref.producer != self.producer:
                raise ValueError("evidence producer must match the verdict producer")
        return self


class ObjectiveVerdict(BaseModel):
    """Independent task-level outcome; it is not inferred from action success."""

    model_config = ConfigDict(frozen=True, extra="forbid")

    schema_version: Literal["harness/objective-verdict/v1"] = "harness/objective-verdict/v1"
    run_id: str = Field(min_length=1, max_length=256)
    producer: Literal["task_verifier", "benchmark_evaluator"]
    captured_at: datetime
    world_revision: int = Field(ge=0)
    status: ObjectiveStatus
    evidence: tuple[EvidenceRef, ...] = ()

    @model_validator(mode="after")
    def validate_verdict(self) -> Self:
        if self.captured_at.tzinfo is None or self.captured_at.utcoffset() is None:
            raise ValueError("captured_at must be timezone-aware")
        if self.status == "verified" and not self.evidence:
            raise ValueError("verified objective verdict requires evidence")
        for ref in self.evidence:
            if ref.run_id != self.run_id:
                raise ValueError("evidence run_id must match the objective verdict")
            if ref.producer != self.producer:
                raise ValueError("evidence producer must match the objective verdict")
            if ref.world_revision != self.world_revision:
                raise ValueError("evidence world_revision must match the objective verdict")
            if ref.captured_at != self.captured_at:
                raise ValueError("evidence captured_at must match the objective verdict")
        return self


@dataclass(frozen=True, slots=True)
class VerifierAuthority:
    """Opaque capability issued to one Harness-owned verifier integration."""

    producer: EvidenceProducer
    _registry_nonce: object = field(repr=False, compare=False)
    _capability: object = field(repr=False, compare=False)


@dataclass(frozen=True, slots=True)
class AuthorizedActionVerdict:
    verdict: ActionVerdict
    _registry_nonce: object = field(repr=False, compare=False)
    _capability: object = field(repr=False, compare=False)


@dataclass(frozen=True, slots=True)
class AuthorizedObjectiveVerdict:
    verdict: ObjectiveVerdict
    _registry_nonce: object = field(repr=False, compare=False)
    _capability: object = field(repr=False, compare=False)


class VerifierRegistry:
    """Issues in-process capabilities for Harness-owned verification producers."""

    def __init__(self) -> None:
        self._nonce = object()
        self._capabilities: dict[EvidenceProducer, object] = {}

    def register(self, producer: EvidenceProducer) -> VerifierAuthority:
        if producer in self._capabilities:
            raise ValueError(f"verifier producer is already registered: {producer}")
        capability = object()
        self._capabilities[producer] = capability
        return VerifierAuthority(producer, self._nonce, capability)

    def authorize_action(
        self,
        authority: VerifierAuthority,
        verdict: ActionVerdict,
    ) -> AuthorizedActionVerdict:
        self._validate_authority(authority, verdict.producer)
        return AuthorizedActionVerdict(verdict, self._nonce, authority._capability)

    def authorize_objective(
        self,
        authority: VerifierAuthority,
        verdict: ObjectiveVerdict,
    ) -> AuthorizedObjectiveVerdict:
        self._validate_authority(authority, verdict.producer)
        return AuthorizedObjectiveVerdict(verdict, self._nonce, authority._capability)

    def is_authorized_action(self, authorized: object) -> bool:
        return self._is_authorized(authorized, AuthorizedActionVerdict)

    def is_authorized_objective(self, authorized: object) -> bool:
        return self._is_authorized(authorized, AuthorizedObjectiveVerdict)

    def _validate_authority(
        self,
        authority: VerifierAuthority,
        producer: EvidenceProducer,
    ) -> None:
        if (
            type(authority) is not VerifierAuthority
            or authority._registry_nonce is not self._nonce
            or authority.producer != producer
            or self._capabilities.get(producer) is not authority._capability
        ):
            raise ValueError("verifier authority is invalid for this producer")

    def _is_authorized(self, authorized: object, expected_type: type) -> bool:
        if type(authorized) is not expected_type:
            return False
        authority = authorized
        verdict = authority.verdict
        producer = verdict.producer
        return (
            authority._registry_nonce is self._nonce
            and self._capabilities.get(producer) is authority._capability
        )


def now_utc() -> datetime:
    return datetime.now(UTC)
