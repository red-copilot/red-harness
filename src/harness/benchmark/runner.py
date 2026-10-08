# ruff: noqa: I001
from __future__ import annotations

import hashlib
import time
import uuid
from collections.abc import Callable
from pathlib import Path

from ..agent import AgentResult, build_agent_adapter
from ..models import AgentSpec, BudgetSpec, TaskSpec
from ..session import AgentObservation
from ..trace import TraceRecorder
from ..runtime import bootstrap_solver
from ..runtime.lifecycle import resolve_run_status
from ..runtime.projections import finalize_world_goal, network_result, world_result
from ..runtime.results import persist_run_result
from ..world import (
    Entity,
    Failure,
    Goal,
    Observation,
)
from .base import BenchmarkAdapter, BenchmarkCase, Submission
from .protocol import SubmissionInbox


SubmissionExtractor = Callable[[AgentResult], list[Submission]]


def _submission_hash(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


class BenchmarkRunner:
    """Generic lifecycle for externally provisioned and evaluated benchmarks."""

    def __init__(
        self,
        *,
        runs_root: str | Path,
        skills_root: str | Path = "skills",
    ) -> None:
        self.runs_root = Path(runs_root)
        self.skills_root = Path(skills_root)

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
        run_id = f"{case.benchmark}_{case.id}_{uuid.uuid4().hex[:8]}"
        run_dir = (self.runs_root / run_id).resolve()
        run_dir.mkdir(parents=True, exist_ok=False)
        trace = TraceRecorder(run_dir / "trace.jsonl", run_id, case.id)
        try:
            session = await adapter.provision(case)
        except Exception as exc:
            trace.emit(
                "benchmark.provision.error",
                data={"error_type": type(exc).__name__, "message": str(exc)},
            )
            raise
        started = time.monotonic()
        root_goal = Goal(
            id=f"goal:{session.case_id}:objective",
            description=session.objective.description,
            status="active",
            priority=1.0,
            attributes={"benchmark": session.benchmark, "case_id": session.case_id},
        )
        targets = [
            Entity(
                id=target.id,
                type="benchmark.target",
                attributes={"address": target.address, **target.metadata},
            )
            for target in session.targets
        ]
        runtime = bootstrap_solver(
            run_dir=run_dir,
            trace=trace,
            goal=root_goal,
            actor=f"agent:{agent.id}",
            context_query=session.objective.description,
            skills_root=self.skills_root,
            targets=targets,
            target_actor=f"benchmark:{session.benchmark}",
        )
        world = runtime.world
        context_builder = runtime.context_builder
        progress = runtime.progress
        solver_loop = runtime.loop
        progress_path = run_dir / "progress.json"

        task = TaskSpec(
            apiVersion="harness/v1",
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

        if agent.type in {"docker", "pi"} and agent.network == "environment":
            raise ValueError(
                "externally provisioned benchmarks do not provide a Harness environment network; "
                "configure the container Agent with network: host or a future benchmark network"
            )

        agent_result: AgentResult | None = None
        submission_results: list[dict] = []
        submitted_keys: set[tuple[str, str]] = set()
        teardown_error: str | None = None
        submission_inbox = SubmissionInbox(run_dir / "submission.inbox.jsonl")
        async def submit_candidate(submission: Submission, agent_session=None) -> bool:
            key = (submission.type, submission.value)
            if key in submitted_keys:
                return False
            submitted_keys.add(key)
            trace.emit(
                "benchmark.submission.proposed",
                actor="agent",
                data={
                    "type": submission.type,
                    "value_sha256": _submission_hash(submission.value),
                },
            )
            submitted = await adapter.submit(session, submission)
            progress.record_submission(
                accepted=submitted.accepted,
                completed=submitted.completed,
            )
            progress.write(progress_path)
            submission_digest = _submission_hash(submission.value)
            data = submitted.model_dump()
            data["value_sha256"] = submission_digest
            submission_results.append(data)
            trace.emit("benchmark.submission.result", data=data)

            revision_before_feedback = world.snapshot.revision
            feedback_id = f"benchmark-feedback:{submission_digest[:16]}"
            world.upsert(
                "observation",
                Observation(
                    id=feedback_id,
                    type="benchmark.submission.feedback",
                    content={
                        "submission_type": submission.type,
                        "value_sha256": submission_digest,
                        "accepted": submitted.accepted,
                        "score_delta": submitted.score_delta,
                        "completed": submitted.completed,
                        "metadata": submitted.metadata,
                    },
                    confidence=1.0,
                    source=session.benchmark,
                ),
                actor=f"benchmark:{session.benchmark}",
            )
            if not submitted.accepted:
                world.upsert(
                    "failure",
                    Failure(
                        id=f"benchmark-rejection:{submission_digest[:16]}",
                        type="benchmark.candidate_rejected",
                        message="Benchmark rejected submitted candidate",
                        recoverable=True,
                        attributes={
                            "submission_type": submission.type,
                            "value_sha256": submission_digest,
                        },
                    ),
                    actor=f"benchmark:{session.benchmark}",
                )
            revision_after_feedback = world.snapshot.revision
            (run_dir / "world.context.txt").write_text(
                context_builder.render(
                    world.snapshot,
                    query=session.objective.description,
                ),
                encoding="utf-8",
            )

            feedback_data = {
                "submission_type": submission.type,
                "accepted": submitted.accepted,
                "score_delta": submitted.score_delta,
                "completed": submitted.completed,
                "metadata": submitted.metadata,
                "value_sha256": submission_digest,
                "world_revision": revision_after_feedback,
                "replan_required": "benchmark_negative_feedback"
                in progress.replan_reasons,
                "replan_reasons": list(progress.replan_reasons),
            }
            if agent_session is not None:
                await agent_session.observe(
                    AgentObservation(
                        type="benchmark.feedback",
                        data=feedback_data,
                    )
                )
                if feedback_data["replan_required"]:
                    await agent_session.observe(
                        AgentObservation(
                            type="solver.replan_requested",
                            data={
                                "reasons": feedback_data["replan_reasons"],
                                "world_revision": revision_after_feedback,
                                "context_path": "world.context.txt",
                                "progress_path": "progress.json",
                            },
                        )
                    )
                    await solver_loop.maybe_replan(agent_session)
                if revision_after_feedback != revision_before_feedback:
                    await agent_session.observe(
                        AgentObservation(
                            type="world.state.updated",
                            data={
                                "revision_before": revision_before_feedback,
                                "revision_after": revision_after_feedback,
                                "context_path": "world.context.txt",
                                "cause": "benchmark.feedback",
                            },
                        )
                    )
            return submitted.completed

        try:
            adapter_instance = build_agent_adapter(
                agent,
                allow_host_agent=allow_host_agent,
                trace=trace,
            )
            run_kwargs = {
                "task": task,
                "task_dir": run_dir,
                "run_dir": run_dir,
                "environment_project": None,
                "environment_network": None,
                "seed": seed,
            }

            start_session = getattr(adapter_instance, "start_session", None)
            completed_online = False
            if callable(start_session):
                agent_session = await start_session(**run_kwargs)
                await solver_loop.maybe_replan(agent_session, force=True)
                async for event in agent_session.events():
                    await solver_loop.process_event(agent_session, event)
                    for submission in submission_inbox.poll():
                        if await submit_candidate(submission, agent_session):
                            completed_online = True
                            await agent_session.close("objective-complete")
                            break
                    if completed_online:
                        break
                agent_result = await agent_session.result()
            else:
                agent_result = adapter_instance.run(**run_kwargs)

            solver_loop.finish_ingest()

            if not completed_online:
                for submission in submission_inbox.poll():
                    if await submit_candidate(submission):
                        completed_online = True
                        break

            if not completed_online:
                for submission in submission_extractor(agent_result):
                    if await submit_candidate(submission):
                        break

            evaluation = await adapter.evaluate(session)
            if evaluation.success:
                progress.objective_completed = True
            progress.write(progress_path)
        finally:
            try:
                await adapter.teardown(session)
            except Exception as exc:  # noqa: BLE001 - teardown must preserve run result.
                teardown_error = f"{type(exc).__name__}: {exc}"
                trace.emit(
                    "benchmark.teardown.error",
                    data={"message": teardown_error},
                )

        if agent_result is None:
            raise RuntimeError("agent did not start")

        status = resolve_run_status(
            objective_completed=progress.objective_completed,
            timed_out=agent_result.timed_out,
            budget_exceeded=bool(agent_result.budget_exceeded),
        )
        finalize_world_goal(
            world=world, goal=root_goal, context_builder=context_builder,
            run_dir=run_dir, status=status, success=evaluation.success, score=evaluation.score,
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
            "termination_reason": (
                "objective-complete" if progress.objective_completed else None
            ),
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
            "world": world_result(world=world, solver_loop=solver_loop, agent_id=agent.id),
            "network": network_result(agent),
            "progress": progress.model_dump(mode="json"),
            "metrics": {
                "duration_ms": int((time.monotonic() - started) * 1000),
                **agent_result.metrics.as_dict(),
            },
        }
        persist_run_result(run_dir=run_dir, trace=trace, result=result)
        return result
