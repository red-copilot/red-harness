from __future__ import annotations

import json
import os
from collections.abc import Callable
from pathlib import Path

from fastapi import APIRouter, HTTPException, Query, Request, Response
from starlette.concurrency import run_in_threadpool

from ..audit import audit_run
from ..planner import HeuristicSkillPlanner, RollingHorizonPlanner
from ..progress import ProgressLedger
from ..secureio import open_regular_file, read_regular_text
from ..skills import load_skills
from ..trace import MAX_TRACE_EVENT_BYTES
from ..world import SQLiteWorldRepository, WorldRepository
from .common import authorize, run_dir

MAX_TRACE_FILE_BYTES = 64 * 1024 * 1024
MAX_TRACE_RESPONSE_BYTES = 4 * 1024 * 1024


def build_runs_router(
    *,
    run_root: Path,
    skill_root: Path | None,
    token: str | None,
    world_repository_factory: Callable[[Path], WorldRepository] = SQLiteWorldRepository,
) -> APIRouter:
    router = APIRouter()

    def repository_for(directory: Path) -> WorldRepository:
        if any(
            (directory / name).is_symlink()
            for name in ("world.db", "world.events.jsonl")
        ):
            raise HTTPException(status_code=422, detail="World artifacts are unsafe")
        return world_repository_factory(directory / "world.events.jsonl")

    @router.get("/v1/runs/{run_id}")
    async def run_result(request: Request, run_id: str) -> dict:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        result_path = directory / "result.json"
        if not result_path.is_file():
            raise HTTPException(status_code=404, detail="result not found")
        try:
            return json.loads(read_regular_text(result_path))
        except (OSError, json.JSONDecodeError) as exc:
            raise HTTPException(status_code=422, detail="result artifact is invalid") from exc

    @router.get("/v1/runs/{run_id}/audit")
    async def run_audit(request: Request, run_id: str) -> dict:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        report = await run_in_threadpool(audit_run, directory)
        return report.model_dump(mode="json")

    @router.get("/v1/runs/{run_id}/trace")
    async def run_trace(
        request: Request,
        response: Response,
        run_id: str,
        limit: int = Query(default=1000, ge=1, le=5000),
        after_line: int = Query(default=0, ge=0),
    ) -> list[dict]:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        trace_path = directory / "trace.jsonl"
        if not trace_path.is_file() or trace_path.is_symlink():
            raise HTTPException(status_code=404, detail="trace not found")
        events: list[dict] = []
        try:
            with open_regular_file(trace_path, "rb") as handle:
                file_size = os.fstat(handle.fileno()).st_size
                if file_size > MAX_TRACE_FILE_BYTES:
                    raise OSError("trace exceeds the read limit")
                bytes_remaining = file_size
                line_number = 0
                next_line = after_line
                response_bytes = 2  # JSON list brackets.
                has_more = False

                while bytes_remaining:
                    raw_line = handle.readline(
                        min(MAX_TRACE_EVENT_BYTES + 2, bytes_remaining)
                    )
                    if not raw_line:
                        raise OSError("trace changed during read")
                    bytes_remaining -= len(raw_line)
                    line_number += 1
                    if len(raw_line) > MAX_TRACE_EVENT_BYTES + 1:
                        raise OSError("trace event exceeds the line size limit")
                    if line_number <= after_line:
                        continue

                    line = raw_line.decode("utf-8", errors="replace")
                    if not line.strip():
                        next_line = line_number
                        continue
                    try:
                        event = json.loads(line)
                    except json.JSONDecodeError as exc:
                        raise HTTPException(
                            status_code=422,
                            detail=f"trace contains malformed event at line {line_number}",
                        ) from exc
                    if not isinstance(event, dict):
                        raise HTTPException(
                            status_code=422, detail="trace event must be an object"
                        )

                    encoded_size = len(
                        json.dumps(event, ensure_ascii=True, separators=(",", ":")).encode(
                            "utf-8"
                        )
                    )
                    separator_size = 1 if events else 0
                    if response_bytes + separator_size + encoded_size > MAX_TRACE_RESPONSE_BYTES:
                        has_more = True
                        break
                    events.append(event)
                    response_bytes += separator_size + encoded_size
                    next_line = line_number

                    if len(events) >= limit:
                        while bytes_remaining:
                            remaining_line = handle.readline(
                                min(MAX_TRACE_EVENT_BYTES + 2, bytes_remaining)
                            )
                            if not remaining_line:
                                raise OSError("trace changed during read")
                            bytes_remaining -= len(remaining_line)
                            if len(remaining_line) > MAX_TRACE_EVENT_BYTES + 1:
                                raise OSError("trace event exceeds the line size limit")
                            if remaining_line.strip():
                                has_more = True
                                break
                        break
        except OSError as exc:
            raise HTTPException(status_code=413, detail="trace artifact is unsafe or too large") from exc
        response.headers["X-Next-Line"] = str(next_line)
        response.headers["X-Has-More"] = str(has_more).lower()
        return events

    @router.get("/v1/runs/{run_id}/world")
    async def run_world(request: Request, run_id: str) -> dict:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        repository = repository_for(directory)
        if repository.snapshot.revision == 0:
            raise HTTPException(status_code=404, detail="world snapshot not found")
        return repository.snapshot.model_dump(mode="json")

    @router.get("/v1/runs/{run_id}/world/events")
    async def run_world_events(
        request: Request,
        run_id: str,
        limit: int = Query(default=1000, ge=1, le=5000),
        after_sequence: int | None = Query(default=None, ge=0),
    ) -> list[dict]:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        repository = repository_for(directory)
        sequenced = getattr(repository, "sequenced_events", None)
        if callable(sequenced):
            records = sequenced(limit=limit, after_sequence=after_sequence)
            if not records:
                if after_sequence is not None:
                    return []
                raise HTTPException(status_code=404, detail="world event log not found")
            return [
                {"sequence": sequence, **event.model_dump(mode="json")}
                for sequence, event in records
            ]

        events = repository.events(limit=limit, after_sequence=after_sequence)
        if not events:
            if after_sequence is not None:
                return []
            raise HTTPException(status_code=404, detail="world event log not found")
        start = after_sequence or 0
        return [
            {"sequence": start + index, **event.model_dump(mode="json")}
            for index, event in enumerate(events, start=1)
        ]

    @router.get("/v1/runs/{run_id}/plan")
    async def run_plan(
        request: Request,
        run_id: str,
        limit: int = Query(default=5, ge=1, le=50),
    ) -> dict:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        repository = repository_for(directory)
        snapshot = repository.snapshot
        if snapshot.revision == 0:
            raise HTTPException(status_code=404, detail="world snapshot not found")
        skills = load_skills(skill_root) if skill_root is not None else []
        planner = HeuristicSkillPlanner()
        candidates = planner.propose(snapshot, skills, limit=limit)
        return {
            "planner": "heuristic-skill-v1",
            "world_revision": snapshot.revision,
            "candidates": [candidate.model_dump() for candidate in candidates],
            "applicability": [
                item.model_dump() for item in planner.explain(snapshot, skills)
            ],
        }

    @router.get("/v1/runs/{run_id}/plan/rolling")
    async def run_rolling_plan(
        request: Request,
        run_id: str,
        horizon: int = Query(default=3, ge=1, le=3),
    ) -> dict:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        repository = repository_for(directory)
        snapshot = repository.snapshot
        if snapshot.revision == 0:
            raise HTTPException(status_code=404, detail="world snapshot not found")
        skills = load_skills(skill_root) if skill_root is not None else []

        progress_path = directory / "progress.json"
        progress = ProgressLedger()
        if progress_path.is_file():
            try:
                progress = ProgressLedger.model_validate_json(read_regular_text(progress_path))
            except (OSError, ValueError) as exc:
                raise HTTPException(status_code=422, detail="progress artifact is invalid") from exc

        plan = RollingHorizonPlanner().propose(
            snapshot,
            skills,
            progress=progress,
            horizon=horizon,
        )
        return plan.model_dump(mode="json")

    return router
