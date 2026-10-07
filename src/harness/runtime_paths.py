from __future__ import annotations

from pathlib import Path


def control_dir(run_dir: Path) -> Path:
    path = run_dir.resolve().parent / ".control" / run_dir.name
    path.mkdir(parents=True, exist_ok=True)
    return path


def runtime_event_path(run_dir: Path) -> Path:
    return control_dir(run_dir) / "runtime.events.jsonl"
