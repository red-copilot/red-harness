from __future__ import annotations

import re
from pathlib import Path

from fastapi import HTTPException, Request


def authorize(request: Request, token: str | None) -> None:
    if token is None:
        return
    if request.headers.get("authorization") != f"Bearer {token}":
        raise HTTPException(status_code=401, detail="invalid control-plane token")


def run_dir(runs_root: Path, run_id: str) -> Path:
    if not re.fullmatch(r"run_[A-Za-z0-9_\-]+", run_id):
        raise HTTPException(status_code=400, detail="invalid run id")
    candidate = (runs_root / run_id).resolve()
    root = runs_root.resolve()
    if candidate.parent != root:
        raise HTTPException(status_code=400, detail="invalid run id")
    if not candidate.is_dir():
        raise HTTPException(status_code=404, detail="run not found")
    return candidate
