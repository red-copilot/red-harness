from __future__ import annotations

import json
from pathlib import Path

from pydantic import ValidationError

from ..secureio import open_regular_file
from .models import WorldObjectKind
from .protocol import IngestError, IngestReport, WorldSubmission
from .repository import WorldRepository


class WorldInboxCursor:
    """Incrementally validate and apply untrusted world-state JSONL submissions."""

    def __init__(
        self,
        path: Path,
        store: WorldRepository,
        *,
        actor: str = "agent",
    ) -> None:
        self.path = path
        self.store = store
        self.actor = actor
        self.offset = 0
        self.remainder = ""
        self.line_number = 0

    def poll(self) -> IngestReport:
        report = IngestReport()
        if not self.path.exists():
            return report

        with open_regular_file(self.path, "r", errors="replace") as handle:
            handle.seek(self.offset)
            chunk = handle.read()
            self.offset = handle.tell()

        if not chunk:
            return report

        text = self.remainder + chunk
        lines = text.splitlines(keepends=True)
        self.remainder = ""

        for line in lines:
            if not line.endswith(("\n", "\r")):
                self.remainder = line
                continue

            self.line_number += 1
            raw = line.strip()
            if not raw:
                continue

            try:
                submission = WorldSubmission.model_validate_json(raw)
                self._apply(submission)
                report.accepted += 1
            except (ValidationError, ValueError, TypeError, json.JSONDecodeError) as exc:
                report.rejected += 1
                report.errors.append(IngestError(line=self.line_number, message=str(exc)))

        return report

    def _apply(self, submission: WorldSubmission) -> None:
        if submission.op == "remove":
            object_id = submission.object.get("id")
            if not isinstance(object_id, str) or not object_id:
                raise ValueError("remove submissions require object.id")
            self.store.remove(
                submission.kind,
                object_id,
                actor=self.actor,
            )
            return

        self.store.upsert(
            submission.kind,
            submission.object,
            actor=self.actor,
            source_event_id=submission.source_event_id,
        )


def collection_name(kind: WorldObjectKind) -> str:
    return {
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
    }[kind]
