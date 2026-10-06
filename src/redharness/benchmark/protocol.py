from __future__ import annotations

import json
from pathlib import Path

from .base import Submission


class SubmissionInbox:
    """Incrementally read Agent-proposed benchmark submissions from JSONL."""

    def __init__(self, path: Path) -> None:
        self.path = path
        self.offset = 0
        self.remainder = ""
        self.seen: set[tuple[str, str]] = set()

    def poll(self) -> list[Submission]:
        if not self.path.exists():
            return []
        with self.path.open("r", encoding="utf-8", errors="replace") as handle:
            handle.seek(self.offset)
            chunk = handle.read()
            self.offset = handle.tell()
        if not chunk:
            return []

        text = self.remainder + chunk
        lines = text.splitlines(keepends=True)
        self.remainder = ""
        submissions: list[Submission] = []
        for line in lines:
            if not line.endswith(("\n", "\r")):
                self.remainder = line
                continue
            try:
                payload = json.loads(line)
                submission = Submission.model_validate(payload)
            except (json.JSONDecodeError, ValueError):
                continue
            key = (submission.type, submission.value)
            if key in self.seen:
                continue
            self.seen.add(key)
            submissions.append(submission)
        return submissions
