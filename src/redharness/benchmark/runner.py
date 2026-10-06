from __future__ import annotations

import hashlib
import json
import time
import uuid
from collections.abc import Callable
from pathlib import Path

from ..agent import AgentResult, build_agent_adapter
from ..models import AgentSpec, BudgetSpec, TaskSpec
from ..trace import TraceRecorder
from ..world import Entity, FileWorldRepository, Goal, WorldContextBuilder, ingest_world_inbox
from .base import BenchmarkAdapter, BenchmarkCase, BenchmarkSession, Submission


SubmissionExtractor = Callable[[AgentResult], list[Submission]]


def _submission_hash(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


class BenchmarkRunner:
    """Generic lifecycle for externally provisioned and evaluated benchmarks."""

    def __init__(self, *, runs_root: str | Path) -> None:
        self.runs_root = Path(runs_root)

    async def run_case(
        self,
        *,
        adapter: BenchmarkAdapter,
        case: BenchmarkCase,
        agent: AgentSpec,
        budgets: BudgetSpec,
        seed: int,
        submission_extractor: SubmissionExtractor,
        allow_host_agent: bool = False,
    ) -> dict:
        session = await adapter.provision(case)
        run_id = f"{session.benchmark}_{session.case_id}_{uuid.uuid4().hex[:8]}"
        run_dir = (self.runs_root / run_id).resolve()
        run_dir.mkdir(parents=True, exist_ok=False)
        trace = TraceRecorder(run_dir / "trace.jsonl", run_id, session.case_id)
        world = FileWorldRepository(run_dir / "world.events.jsonl")
        context_builder = WorldContextBuilder()
        started = time.monotonic()

        root_goal = Goal(
            id=f"goal:{session.case_id}:objective",
            description=session.objective.description,
            status="active",
            priority=1.0,
            attributes={
                "benchmark": session.benchmark,
                "case_id": session.case_id,
            },
        )
        world.upsert("goal", root_goal)

        for target in session.targets:
            world.upsert(
                "entity",
                Entity(
                    id=target.id,
                    type="benchmark.target",
                    attributes={
                        "address": target.address,
                        **target.metadata,
                    },
                ),
                actor=f"benchmark:{session.benchmark}",
            )

        (run_dir / "world.context.txt").write_text(
            context_builder.render(world.snapshot),
            encoding="utf-8",
        )

        task = TaskSpec(
            apiVersion="redharness/v1",
            id=session.case_id,
            name=session.case_id,
            category=session.benchmark,
            difficulty=case.difficulty,
            objective=session.objective,
            budgets=budgets,
        )

        trace.emit(
            "run.started",
            data={
                "benchmark": session.benchmark,
                "case_id": session.case_id,
                "agent_id": agent.id,
                "seed": seed,
                "world_revision": world.snapshot.revision,
            },
        )

        agent_result: AgentResult | None = None
        submission_results: list[dict] = []
        teardown_error: str | None = None
        try:
            adapter_instance = build_agent_adapter(
                agent,
                allow_host_agent=allow_host_agent,
                trace=trace,
            )
            agent_result = adapter_instance.run(
                task,
                task_dir=run_dir,
                run_dir=run_dir,
                environment_project=None,
                environment_network=None,
                seed=seed,
            )

            ingest_report = ingest_world_inbox(
                run_dir / "world.inbox.jsonl",
                world,
                actor=f"agent:{agent.id}",
            )
            trace.emit(
                "world.ingested",
                data={
                    "accepted": ingest_report.accepted,
                    "rejected": ingest_report.rejected,
                    "errors": [error.model_dump() for error in ingest_report.errors[:10]],
                },
            )

            for submission in submission_extractor(agent_result):
                trace.emit(
                    "benchmark.submission.proposed",
                    actor="agent",
                    data={
                        "type": submission.type,
                        "value_sha256": _submission_hash(submission.value),
                    },
                )
                submitted = await adapter.submit(session, submission)
                data = submitted.model_dump()
                data["value_sha256"] = _submission_hash(submission.value)
                submission_results.append(data)
                trace.emit("benchmark.submission.result", data=data)
                if submitted.completed:
                    break

            evaluation = await adapter.evaluate(session)
        finally:
            try:
                await adapter.teardown(session)
            except Exception as exc:
                teardown_error = f"{type(exc).__name__}: {exc}"
                trace.emit(
                    "benchmark.teardown.error",
                    data={"message": teardown_error},
                )

        if agent_result is None:
            raise RuntimeError("agent did not start")

        status = (
            "timeout"
            if agent_result.timed_out
            else "budget_exceeded"
            if agent_result.budget_exceeded
            else "finished"
        )
        root_goal.status = "completed" if evaluation.success else "failed"
        root_goal.attributes.update(
            {
                "run_status": status,
                "score": evaluation.score,
                "success": evaluation.success,
            }
        )
        world.upsert("goal", root_goal)
        (run_dir / "world.context.txt").write_text(
            context_builder.render(world.snapshot),
            encoding="utf-8",
        )

        result = {
            "run_id": run_id,
            "benchmark": {
                "type": session.benchmark,
                "case_id": session.case_id,
                "domain": case.domain,
                "difficulty": case.difficulty,
                "session": session.metadata,
            },
            "agent_id": agent.id,
            "status": status,
            "success": evaluation.success,
            "score": evaluation.score,
            "message": evaluation.message,
            "milestones": evaluation.milestones,
            "evaluation": evaluation.metadata,
            "submissions": submission_results,
            "cleanup": {
                "ok": teardown_error is None,
                "error": teardown_error,
            },
            "world": {
                "revision": world.snapshot.revision,
            },
            "metrics": {
                "duration_ms": int((time.monotonic() - started) * 1000),
                **agent_result.metrics.as_dict(),
            },
        }
        (run_dir / "result.json").write_text(
            json.dumps(result, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        trace.emit(
            "run.finished",
            data={
                "status": status,
                "success": result["success"],
                "score": result["score"],
                "metrics": result["metrics"],
            },
        )
        return result
