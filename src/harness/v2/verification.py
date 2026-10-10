"""Host-composed verification boundary with evidence-store integrity checks."""

from __future__ import annotations

import asyncio
import hashlib
import json
from collections.abc import Mapping
from typing import Protocol

from .contracts import EvaluationReply, EvidenceRef, FrozenArtifacts, ObjectiveVerdict, Producer


class Evaluator(Protocol):
    async def evaluate(self, artifacts: FrozenArtifacts) -> EvaluationReply: ...


class EvidenceStore(Protocol):
    """Implementations must commit evidence before returning its host-assigned reference."""

    def persist(
        self,
        *,
        run_id: str,
        task_id: str,
        producer: Producer,
        input_digest: str,
        payload: bytes,
    ) -> EvidenceRef: ...

    def read(self, ref: EvidenceRef) -> bytes: ...


class VerificationBoundary:
    """Only invokes evaluator objects registered by trusted host composition.

    There is deliberately no API to promote an Agent event or caller-supplied
    result dictionary. Producer labels in serialized evidence are descriptive;
    registry ownership and the host evidence store establish their authority.
    """

    def __init__(
        self,
        producers: Mapping[Producer, Evaluator],
        *,
        evidence_store: EvidenceStore,
    ) -> None:
        if set(producers) - {"local_verifier", "tsec_evaluator"}:
            raise ValueError("only host verifier channels may be registered")
        if any(
            not callable(getattr(evaluator, "evaluate", None)) for evaluator in producers.values()
        ):
            raise TypeError("registered evaluators must implement evaluate")
        self._producers = dict(producers)
        self._store = evidence_store

    async def evaluate(
        self,
        producer: str,
        *,
        run_id: str,
        task_id: str,
        artifacts: FrozenArtifacts,
    ) -> ObjectiveVerdict:
        def unknown(code: str) -> ObjectiveVerdict:
            return ObjectiveVerdict(
                run_id=run_id, task_id=task_id, status="unknown", error_code=code
            )

        evaluator = self._producers.get(producer)
        if evaluator is None:
            return unknown("unregistered_producer")
        if not await asyncio.to_thread(artifacts.verify):
            return unknown("artifact_mismatch")
        try:
            reply = await evaluator.evaluate(artifacts)
        except Exception:  # noqa: BLE001 - untrusted errors must not leak credentials.
            return unknown("evaluator_unavailable")
        if isinstance(reply, EvaluationReply) and type(reply) is not EvaluationReply:
            return unknown("invalid_response")
        try:
            reply = EvaluationReply.model_validate(reply)
        except (ValueError, TypeError):
            return unknown("invalid_response")
        if not await asyncio.to_thread(artifacts.verify):
            return unknown("artifact_mismatch")

        input_digest = artifacts.digest
        payload = json.dumps(
            {
                "apiVersion": "harness/v2",
                "run_id": run_id,
                "task_id": task_id,
                "producer": producer,
                "input_digest": input_digest,
                "evaluation": reply.model_dump(mode="json"),
            },
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=False,
        ).encode()
        try:
            ref = EvidenceRef.model_validate(
                self._store.persist(
                    run_id=run_id,
                    task_id=task_id,
                    producer=producer,
                    input_digest=input_digest,
                    payload=payload,
                )
            )
            stored_payload = self._store.read(ref)
        except Exception:  # noqa: BLE001 - no verdict until durable evidence is readable.
            return unknown("evidence_unavailable")
        if (
            ref.run_id != run_id
            or ref.task_id != task_id
            or ref.producer != producer
            or ref.input_digest != input_digest
            or ref.sha256 != hashlib.sha256(payload).hexdigest()
            or stored_payload != payload
        ):
            return unknown("evidence_mismatch")
        return ObjectiveVerdict(
            run_id=run_id,
            task_id=task_id,
            **reply.model_dump(),
            evidence=(ref,),
            error_code="evaluator_unknown" if reply.status == "unknown" else None,
        )
