from __future__ import annotations

import re
import secrets
from pathlib import Path

from fastapi import HTTPException, Request


def authorize(request: Request, token: str | None) -> None:
    if token is None:
        raise HTTPException(status_code=503, detail="control-plane authentication is not configured")
    supplied = request.headers.get("authorization", "")
    expected = f"Bearer {token}"
    if not secrets.compare_digest(supplied, expected):
        raise HTTPException(status_code=401, detail="invalid control-plane token")


def run_dir(runs_root: Path, run_id: str) -> Path:
    if not re.fullmatch(r"run_[A-Za-z0-9_\-]+", run_id):
        raise HTTPException(status_code=400, detail="invalid run id")
    unresolved = runs_root / run_id
    if unresolved.is_symlink():
        raise HTTPException(status_code=400, detail="invalid run id")
    candidate = unresolved.resolve()
    root = runs_root.resolve()
    if candidate.parent != root:
        raise HTTPException(status_code=400, detail="invalid run id")
    if not candidate.is_dir():
        raise HTTPException(status_code=404, detail="run not found")
    return candidate
