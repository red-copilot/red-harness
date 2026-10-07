from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path

from fastapi import APIRouter, HTTPException, Query, Request

from ..otel import trace_to_otlp_json
from ..planner import HeuristicSkillPlanner, RollingHorizonPlanner
from ..progress import ProgressLedger
from ..skills import load_skills
from ..world import SQLiteWorldRepository, WorldRepository
from .common import authorize, run_dir


def build_runs_router(
    *,
    run_root: Path,
    skill_root: Path | None,
    token: str | None,
    world_repository_factory: Callable[[Path], WorldRepository] = SQLiteWorldRepository,
) -> APIRouter:
    router = APIRouter()

    @router.get("/v1/runs/{run_id}")
    async def run_result(request: Request, run_id: str) -> dict:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        result_path = directory / "result.json"
        if not result_path.is_file():
            raise HTTPException(status_code=404, detail="result not found")
        return json.loads(result_path.read_text(encoding="utf-8"))

    @router.get("/v1/runs/{run_id}/trace")
    async def run_trace(
        request: Request,
        run_id: str,
        limit: int = Query(default=1000, ge=1, le=5000),
    ) -> list[dict]:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        trace_path = directory / "trace.jsonl"
        if not trace_path.is_file():
            raise HTTPException(status_code=404, detail="trace not found")
        events: list[dict] = []
        with trace_path.open("r", encoding="utf-8") as handle:
            for line in handle:
                if line.strip():
                    events.append(json.loads(line))
                    if len(events) >= limit:
                        break
        return events

    @router.get("/v1/runs/{run_id}/world")
    async def run_world(request: Request, run_id: str) -> dict:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        repository = world_repository_factory(directory / "world.events.jsonl")
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
        repository = world_repository_factory(directory / "world.events.jsonl")
        sequenced = getattr(repository, "sequenced_events", None)
        if callable(sequenced):
            records = sequenced(limit=limit, after_sequence=after_sequence)
            if not records:
                raise HTTPException(status_code=404, detail="world event log not found")
            return [
                {"sequence": sequence, **event.model_dump(mode="json")}
                for sequence, event in records
            ]

        events = repository.events(limit=limit, after_sequence=after_sequence)
        if not events:
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
        repository = world_repository_factory(directory / "world.events.jsonl")
        snapshot = repository.snapshot
        if snapshot.revision == 0:
            raise HTTPException(status_code=404, detail="world snapshot not found")
        skills = load_skills(skill_root) if skill_root is not None else []
        candidates = HeuristicSkillPlanner().propose(snapshot, skills, limit=limit)
        return {
            "planner": "heuristic-skill-v1",
            "world_revision": snapshot.revision,
            "candidates": [candidate.model_dump() for candidate in candidates],
        }

    @router.get("/v1/runs/{run_id}/plan/rolling")
    async def run_rolling_plan(
        request: Request,
        run_id: str,
        horizon: int = Query(default=3, ge=1, le=3),
    ) -> dict:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        repository = world_repository_factory(directory / "world.events.jsonl")
        snapshot = repository.snapshot
        if snapshot.revision == 0:
            raise HTTPException(status_code=404, detail="world snapshot not found")
        skills = load_skills(skill_root) if skill_root is not None else []

        progress_path = directory / "progress.json"
        progress = ProgressLedger()
        if progress_path.is_file():
            progress = ProgressLedger.model_validate(
                json.loads(progress_path.read_text(encoding="utf-8"))
            )

        plan = RollingHorizonPlanner().propose(
            snapshot,
            skills,
            progress=progress,
            horizon=horizon,
        )
        return plan.model_dump(mode="json")

    @router.get("/v1/runs/{run_id}/otel")
    async def run_otel(request: Request, run_id: str) -> dict:
        authorize(request, token)
        directory = run_dir(run_root, run_id)
        trace_path = directory / "trace.jsonl"
        if not trace_path.is_file():
            raise HTTPException(status_code=404, detail="trace not found")
        return trace_to_otlp_json(trace_path)

    return router
