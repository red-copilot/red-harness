"""Shared result finalization for all Harness execution entry points."""
from __future__ import annotations

import json
import os
import stat
import tempfile
from pathlib import Path
from typing import Any

from ..trace import TraceRecorder


def persist_run_result(*, run_dir: Path, trace: TraceRecorder, result: dict[str, Any]) -> None:
    """Write the public result schema and emit the terminal trace event.

    Callers own their domain-specific fields, verification and teardown.
    This function preserves the existing JSON encoding and event payload.
    """
    path = run_dir / "result.json"
    payload = (json.dumps(result, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    descriptor, temporary_name = tempfile.mkstemp(prefix=".result-", dir=run_dir)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        try:
            # A hard link publishes the fully written file atomically and fails if
            # another finalizer has already committed this run's terminal result.
            os.link(temporary, path, follow_symlinks=False)
            created = True
        except FileExistsError:
            created = False
            if not stat.S_ISREG(path.lstat().st_mode):
                raise RuntimeError("terminal result path is not a regular file")
            existing = path.read_bytes()
            if existing != payload:
                raise RuntimeError("terminal result already exists with different content")
        if created:
            directory_fd = os.open(run_dir, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
    finally:
        temporary.unlink(missing_ok=True)
    if not created:
        return
    trace.emit(
        "run.finished",
        data={
            "status": result["status"],
            "success": result["success"],
            "score": result["score"],
            "metrics": result["metrics"],
        },
    )
