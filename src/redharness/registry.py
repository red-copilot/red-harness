from __future__ import annotations

from pathlib import Path

from .models import load_task


def scan_benchmarks(root: str | Path) -> list[dict]:
    base = Path(root).resolve()
    if not base.exists():
        return []
    entries: list[dict] = []
    for task_path in sorted(base.rglob("task.yaml")):
        try:
            task = load_task(task_path)
        except Exception as exc:  # noqa: BLE001 - registry reports invalid entries instead of aborting.
            entries.append(
                {
                    "path": str(task_path),
                    "valid": False,
                    "error": f"{type(exc).__name__}: {exc}",
                }
            )
            continue
        entries.append(
            {
                "path": str(task_path),
                "relative_path": str(task_path.relative_to(base)),
                "valid": True,
                "id": task.id,
                "name": task.name,
                "category": task.category,
                "difficulty": task.difficulty,
                "tags": task.tags,
                "environment": task.environment.provider,
                "verifier": task.verification.type,
            }
        )
    return entries
