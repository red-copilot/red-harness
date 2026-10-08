"""Shared result finalization for all Harness execution entry points."""
from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from ..trace import TraceRecorder


def persist_run_result(*, run_dir: Path, trace: TraceRecorder, result: dict[str, Any]) -> None:
    """Write the public result schema and emit the terminal trace event.

    Callers own their domain-specific fields, verification and teardown.
    This function preserves the existing JSON encoding and event payload.
    """
    (run_dir / "result.json").write_text(
        json.dumps(result, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    trace.emit(
        "run.finished",
        data={
            "status": result["status"],
            "success": result["success"],
            "score": result["score"],
            "metrics": result["metrics"],
        },
    )
