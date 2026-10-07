from __future__ import annotations

from pathlib import Path

from fastapi import APIRouter, HTTPException, Query, Request
from pydantic import BaseModel, Field

from ..coordination import CoordinationStore, WorkItemSpec
from ..skills import load_skills
from .common import authorize, run_dir


class PublishPlanRequest(BaseModel):
    skill_id: str = Field(min_length=1)
    description: str = Field(min_length=1)
    goal_id: str | None = None
    priority: float = Field(default=0.5, ge=0.0, le=1.0)
    metadata: dict = Field(default_factory=dict)


class WorkClaimRequest(BaseModel):
    run_id: str = Field(min_length=1, max_length=300)
    agent_id: str = Field(min_length=1, max_length=200)
    lease_seconds: int = Field(default=60, ge=10, le=3600)


class WorkLeaseRequest(BaseModel):
    agent_id: str = Field(min_length=1, max_length=200)
    lease_seconds: int = Field(default=60, ge=10, le=3600)


class WorkCompleteRequest(BaseModel):
    agent_id: str = Field(min_length=1, max_length=200)
    result: dict = Field(default_factory=dict)


class WorkFailRequest(BaseModel):
    agent_id: str = Field(min_length=1, max_length=200)
    error: str = Field(min_length=1, max_length=4000)
    requeue: bool = False


def build_coordination_router(
    *,
    coordination: CoordinationStore,
    run_root: Path,
    skill_root: Path | None,
    token: str | None,
) -> APIRouter:
    router = APIRouter()

    @router.post("/v1/runs/{run_id}/plan/publish")
    async def publish_plan_candidate(
        request: Request,
        run_id: str,
        body: PublishPlanRequest,
    ) -> dict:
        authorize(request, token)
        run_dir(run_root, run_id)
        if skill_root is None:
            raise HTTPException(status_code=409, detail="skill registry is not configured")
        skills_by_id = {skill.id: skill for skill in load_skills(skill_root)}
        skill = skills_by_id.get(body.skill_id)
        if skill is None:
            raise HTTPException(status_code=404, detail="skill not found")
        return coordination.submit(
            WorkItemSpec(
                run_id=run_id,
                goal_id=body.goal_id,
                skill_id=skill.id,
                description=body.description,
                priority=body.priority,
                metadata={
                    "skill_domain": skill.domain,
                    "skill_tags": skill.tags,
                    **body.metadata,
                },
            )
        )

    @router.post("/v1/work")
    async def submit_work(request: Request, body: WorkItemSpec) -> dict:
        authorize(request, token)
        return coordination.submit(body)

    @router.get("/v1/work")
    async def list_work(
        request: Request,
        run_id: str | None = None,
        state: str | None = None,
        limit: int = Query(default=100, ge=1, le=1000),
    ) -> list[dict]:
        authorize(request, token)
        if state not in {None, "queued", "running", "completed", "failed"}:
            raise HTTPException(status_code=400, detail="invalid work state")
        return coordination.list(run_id=run_id, state=state, limit=limit)

    @router.post("/v1/work/claim")
    async def claim_work(request: Request, body: WorkClaimRequest) -> dict | None:
        authorize(request, token)
        return coordination.claim(
            body.run_id,
            body.agent_id,
            lease_seconds=body.lease_seconds,
        )

    @router.post("/v1/work/{work_id}/heartbeat")
    async def heartbeat_work(
        request: Request,
        work_id: str,
        body: WorkLeaseRequest,
    ) -> dict[str, bool]:
        authorize(request, token)
        if not coordination.heartbeat(
            work_id,
            body.agent_id,
            lease_seconds=body.lease_seconds,
        ):
            raise HTTPException(status_code=409, detail="work lease is not owned by agent")
        return {"ok": True}

    @router.post("/v1/work/{work_id}/complete")
    async def complete_work(
        request: Request,
        work_id: str,
        body: WorkCompleteRequest,
    ) -> dict[str, bool]:
        authorize(request, token)
        if not coordination.complete(work_id, body.agent_id, body.result):
            raise HTTPException(status_code=409, detail="work lease is not owned by agent")
        return {"ok": True}

    @router.post("/v1/work/{work_id}/fail")
    async def fail_work(
        request: Request,
        work_id: str,
        body: WorkFailRequest,
    ) -> dict[str, bool]:
        authorize(request, token)
        if not coordination.fail(
            work_id,
            body.agent_id,
            body.error,
            requeue=body.requeue,
        ):
            raise HTTPException(status_code=409, detail="work lease is not owned by agent")
        return {"ok": True}

    @router.post("/v1/work/{work_id}/release")
    async def release_work(
        request: Request,
        work_id: str,
        body: WorkLeaseRequest,
    ) -> dict[str, bool]:
        authorize(request, token)
        if not coordination.release(work_id, body.agent_id):
            raise HTTPException(status_code=409, detail="work lease is not owned by agent")
        return {"ok": True}

    return router
