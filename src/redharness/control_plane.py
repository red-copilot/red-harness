from __future__ import annotations

import json
import re
from pathlib import Path

from fastapi import FastAPI, HTTPException, Query, Request
from pydantic import BaseModel, Field

from .execution import ExecutionCapabilities
from .otel import trace_to_otlp_json
from .planner import HeuristicSkillPlanner
from .queue import JobPayload, SQLiteQueue
from .registry import scan_benchmarks
from .skills import load_skills, scan_skills
from .world import WorldSnapshot


class ClaimRequest(BaseModel):
    worker_id: str = Field(min_length=1, max_length=200)
    lease_seconds: int = Field(default=60, ge=10, le=3600)


class LeaseRequest(BaseModel):
    worker_id: str = Field(min_length=1, max_length=200)
    lease_seconds: int = Field(default=60, ge=10, le=3600)


class CompleteRequest(BaseModel):
    worker_id: str = Field(min_length=1, max_length=200)
    result: dict


class FailRequest(BaseModel):
    worker_id: str = Field(min_length=1, max_length=200)
    error: str = Field(min_length=1, max_length=4000)


def _authorize(request: Request, token: str | None) -> None:
    if token is None:
        return
    if request.headers.get("authorization") != f"Bearer {token}":
        raise HTTPException(status_code=401, detail="invalid control-plane token")


def _run_dir(runs_root: Path, run_id: str) -> Path:
    if not re.fullmatch(r"run_[A-Za-z0-9_\-]+", run_id):
        raise HTTPException(status_code=400, detail="invalid run id")
    candidate = (runs_root / run_id).resolve()
    root = runs_root.resolve()
    if candidate.parent != root:
        raise HTTPException(status_code=400, detail="invalid run id")
    if not candidate.is_dir():
        raise HTTPException(status_code=404, detail="run not found")
    return candidate


def create_control_plane(
    *,
    queue_db: Path,
    runs_root: Path,
    benchmarks_root: Path | None = None,
    skills_root: Path | None = None,
    token: str | None = None,
) -> FastAPI:
    app = FastAPI(title="Red Harness Control Plane", version="0.6.0")
    queue = SQLiteQueue(queue_db)
    run_root = runs_root.resolve()
    benchmark_root = benchmarks_root.resolve() if benchmarks_root else None
    skill_root = skills_root.resolve() if skills_root else None

    @app.get("/health")
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/v1/capabilities")
    async def capabilities(request: Request) -> dict[str, bool]:
        _authorize(request, token)
        return ExecutionCapabilities.detect().as_dict()

    @app.get("/v1/benchmarks")
    async def benchmarks(request: Request) -> list[dict]:
        _authorize(request, token)
        if benchmark_root is None:
            return []
        return scan_benchmarks(benchmark_root)

    @app.get("/v1/skills")
    async def skills(request: Request) -> list[dict]:
        _authorize(request, token)
        if skill_root is None:
            return []
        return scan_skills(skill_root)

    @app.post("/v1/jobs")
    async def submit_job(request: Request, payload: JobPayload) -> dict:
        _authorize(request, token)
        return queue.submit(payload)

    @app.get("/v1/jobs")
    async def list_jobs(
        request: Request,
        limit: int = Query(default=100, ge=1, le=1000),
    ) -> list[dict]:
        _authorize(request, token)
        return queue.list(limit=limit)

    @app.get("/v1/jobs/{job_id}")
    async def get_job(request: Request, job_id: str) -> dict:
        _authorize(request, token)
        job = queue.get(job_id)
        if job is None:
            raise HTTPException(status_code=404, detail="job not found")
        return job

    @app.post("/v1/jobs/claim")
    async def claim_job(request: Request, body: ClaimRequest) -> dict | None:
        _authorize(request, token)
        return queue.claim(body.worker_id, lease_seconds=body.lease_seconds)

    @app.post("/v1/jobs/{job_id}/heartbeat")
    async def heartbeat(request: Request, job_id: str, body: LeaseRequest) -> dict[str, bool]:
        _authorize(request, token)
        if not queue.heartbeat(job_id, body.worker_id, lease_seconds=body.lease_seconds):
            raise HTTPException(status_code=409, detail="lease is not owned by worker")
        return {"ok": True}

    @app.post("/v1/jobs/{job_id}/complete")
    async def complete(request: Request, job_id: str, body: CompleteRequest) -> dict[str, bool]:
        _authorize(request, token)
        if not queue.complete(job_id, body.worker_id, body.result):
            raise HTTPException(status_code=409, detail="lease is not owned by worker")
        return {"ok": True}

    @app.post("/v1/jobs/{job_id}/fail")
    async def fail(request: Request, job_id: str, body: FailRequest) -> dict[str, bool]:
        _authorize(request, token)
        if not queue.fail(job_id, body.worker_id, body.error):
            raise HTTPException(status_code=409, detail="lease is not owned by worker")
        return {"ok": True}

    @app.get("/v1/leaderboard")
    async def leaderboard(request: Request) -> list[dict]:
        _authorize(request, token)
        return queue.leaderboard()

    @app.get("/v1/runs/{run_id}")
    async def run_result(request: Request, run_id: str) -> dict:
        _authorize(request, token)
        directory = _run_dir(run_root, run_id)
        result_path = directory / "result.json"
        if not result_path.is_file():
            raise HTTPException(status_code=404, detail="result not found")
        return json.loads(result_path.read_text(encoding="utf-8"))

    @app.get("/v1/runs/{run_id}/trace")
    async def run_trace(
        request: Request,
        run_id: str,
        limit: int = Query(default=1000, ge=1, le=5000),
    ) -> list[dict]:
        _authorize(request, token)
        directory = _run_dir(run_root, run_id)
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

    @app.get("/v1/runs/{run_id}/world")
    async def run_world(request: Request, run_id: str) -> dict:
        _authorize(request, token)
        directory = _run_dir(run_root, run_id)
        world_path = directory / "world.snapshot.json"
        if not world_path.is_file():
            raise HTTPException(status_code=404, detail="world snapshot not found")
        return json.loads(world_path.read_text(encoding="utf-8"))

    @app.get("/v1/runs/{run_id}/world/events")
    async def run_world_events(
        request: Request,
        run_id: str,
        limit: int = Query(default=1000, ge=1, le=5000),
    ) -> list[dict]:
        _authorize(request, token)
        directory = _run_dir(run_root, run_id)
        event_path = directory / "world.events.jsonl"
        if not event_path.is_file():
            raise HTTPException(status_code=404, detail="world event log not found")
        events: list[dict] = []
        with event_path.open("r", encoding="utf-8") as handle:
            for line in handle:
                if line.strip():
                    events.append(json.loads(line))
                    if len(events) >= limit:
                        break
        return events

    @app.get("/v1/runs/{run_id}/plan")
    async def run_plan(
        request: Request,
        run_id: str,
        limit: int = Query(default=5, ge=1, le=50),
    ) -> dict:
        _authorize(request, token)
        directory = _run_dir(run_root, run_id)
        world_path = directory / "world.snapshot.json"
        if not world_path.is_file():
            raise HTTPException(status_code=404, detail="world snapshot not found")
        snapshot = WorldSnapshot.model_validate(
            json.loads(world_path.read_text(encoding="utf-8"))
        )
        skills = load_skills(skill_root) if skill_root is not None else []
        candidates = HeuristicSkillPlanner().propose(snapshot, skills, limit=limit)
        return {
            "planner": "heuristic-skill-v1",
            "world_revision": snapshot.revision,
            "candidates": [candidate.model_dump() for candidate in candidates],
        }

    @app.get("/v1/runs/{run_id}/otel")
    async def run_otel(request: Request, run_id: str) -> dict:
        _authorize(request, token)
        directory = _run_dir(run_root, run_id)
        trace_path = directory / "trace.jsonl"
        if not trace_path.is_file():
            raise HTTPException(status_code=404, detail="trace not found")
        return trace_to_otlp_json(trace_path)

    return app
