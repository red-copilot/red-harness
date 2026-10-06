from __future__ import annotations

import json
from pathlib import Path
from typing import Literal

from pydantic import BaseModel, Field, ValidationError

from .models import WorldObjectKind
from .store import WorldStore


class WorldSubmission(BaseModel):
    """Untrusted Agent-to-Harness world-state submission."""

    schema_version: Literal["redharness.world.submit/v1"] = "redharness.world.submit/v1"
    kind: WorldObjectKind
    op: Literal["upsert", "remove"] = "upsert"
    object: dict
    source_event_id: str | None = None


class IngestError(BaseModel):
    line: int
    message: str


class IngestReport(BaseModel):
    accepted: int = 0
    rejected: int = 0
    errors: list[IngestError] = Field(default_factory=list)


def ingest_world_inbox(
    path: Path,
    store: WorldStore,
    *,
    actor: str = "agent",
) -> IngestReport:
    """Validate an untrusted JSONL inbox before appending authoritative WorldEvents."""

    report = IngestReport()
    if not path.exists():
        return report

    with path.open("r", encoding="utf-8") as handle:
        for number, line in enumerate(handle, start=1):
            if not line.strip():
                continue
            try:
                submission = WorldSubmission.model_validate_json(line)
                if submission.op == "remove":
                    object_id = submission.object.get("id")
                    if not isinstance(object_id, str) or not object_id:
                        raise ValueError("remove submissions require object.id")
                    store.remove(submission.kind, object_id, actor=actor)
                else:
                    store.upsert(
                        submission.kind,
                        submission.object,
                        actor=actor,
                        source_event_id=submission.source_event_id,
                    )
                report.accepted += 1
            except (ValidationError, ValueError, TypeError) as exc:
                report.rejected += 1
                report.errors.append(IngestError(line=number, message=str(exc)))

    return report


def write_submission(path: Path, submission: WorldSubmission) -> None:
    """Append one submission. Intended for trusted adapters and tests."""

    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(submission.model_dump_json() + "\n")
        handle.flush()
