from __future__ import annotations

from fastapi import APIRouter, HTTPException, Query, Request
from pydantic import BaseModel, Field, field_validator

from ..queue import JobPayload, SQLiteQueue, validate_terminal_result
from .common import authorize


class ClaimRequest(BaseModel):
    worker_id: str = Field(min_length=1, max_length=200)
    lease_seconds: int = Field(default=60, ge=10, le=3600)


class LeaseRequest(BaseModel):
    worker_id: str = Field(min_length=1, max_length=200)
    lease_seconds: int = Field(default=60, ge=10, le=3600)


class CompleteRequest(BaseModel):
    worker_id: str = Field(min_length=1, max_length=200)
    result: dict

    @field_validator("result")
    @classmethod
    def result_is_bounded_json(cls, value: dict) -> dict:
        return validate_terminal_result(value)


class FailRequest(BaseModel):
    worker_id: str = Field(min_length=1, max_length=200)
    error: str = Field(min_length=1, max_length=4000)


def build_jobs_router(queue: SQLiteQueue, token: str | None) -> APIRouter:
    router = APIRouter()

    @router.post("/v1/jobs")
    async def submit_job(request: Request, payload: JobPayload) -> dict:
        authorize(request, token)
        return queue.submit(payload)

    @router.get("/v1/jobs")
    async def list_jobs(
        request: Request,
        limit: int = Query(default=100, ge=1, le=1000),
    ) -> list[dict]:
        authorize(request, token)
        return queue.list(limit=limit)

    @router.get("/v1/jobs/{job_id}")
    async def get_job(request: Request, job_id: str) -> dict:
        authorize(request, token)
        job = queue.get(job_id)
        if job is None:
            raise HTTPException(status_code=404, detail="job not found")
        return job

    @router.post("/v1/jobs/claim")
    async def claim_job(request: Request, body: ClaimRequest) -> dict | None:
        authorize(request, token)
        return queue.claim(body.worker_id, lease_seconds=body.lease_seconds)

    @router.post("/v1/jobs/{job_id}/heartbeat")
    async def heartbeat(request: Request, job_id: str, body: LeaseRequest) -> dict[str, bool]:
        authorize(request, token)
        if not queue.heartbeat(job_id, body.worker_id, lease_seconds=body.lease_seconds):
            raise HTTPException(status_code=409, detail="lease is not owned by worker")
        return {"ok": True}

    @router.post("/v1/jobs/{job_id}/complete")
    async def complete(
        request: Request,
        job_id: str,
        body: CompleteRequest,
    ) -> dict[str, bool]:
        authorize(request, token)
        if not queue.complete(job_id, body.worker_id, body.result):
            raise HTTPException(status_code=409, detail="lease is not owned by worker")
        return {"ok": True}

    @router.post("/v1/jobs/{job_id}/fail")
    async def fail(request: Request, job_id: str, body: FailRequest) -> dict[str, bool]:
        authorize(request, token)
        if not queue.fail(job_id, body.worker_id, body.error):
            raise HTTPException(status_code=409, detail="lease is not owned by worker")
        return {"ok": True}

    @router.post("/v1/jobs/{job_id}/requeue")
    async def requeue(request: Request, job_id: str) -> dict[str, bool]:
        authorize(request, token)
        if not queue.requeue(job_id):
            raise HTTPException(
                status_code=409,
                detail="job is not awaiting operator reconciliation",
            )
        return {"ok": True}

    @router.get("/v1/leaderboard")
    async def leaderboard(request: Request) -> list[dict]:
        authorize(request, token)
        return queue.leaderboard()

    return router
