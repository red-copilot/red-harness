# ruff: noqa: I001
from __future__ import annotations

import hashlib
import asyncio
import math
import time
import uuid
from collections.abc import Callable
from pathlib import Path

from pydantic import ValidationError

from ..agent import AgentResult, build_agent_adapter
from ..models import AgentSpec, BudgetSpec, TaskSpec
from ..session import AgentObservation
from ..trace import TraceRecorder, sanitize_observability_data
from ..runtime import bootstrap_solver
from ..runtime.solver_profile import DEFAULT_SOLVER_PROFILE, SolverProfile
from ..runtime.agent_workspace import create_agent_workspace, sync_agent_workspace
from ..runtime.checkpoint import run_action_owner, save_session_checkpoint
from ..runtime.evidence import persist_objective_verdict
from ..runtime.lifecycle import RunLifecycle, resolve_run_status
from ..runtime.projections import finalize_world_goal, network_result, world_result
from ..runtime.results import persist_run_result
from ..world import (
    Entity,
    Failure,
    Goal,
    Observation,
)
from .base import BenchmarkAdapter, BenchmarkCase, BenchmarkSession, EvaluationResult, Submission
from .protocol import SubmissionInbox


SubmissionExtractor = Callable[[AgentResult], list[Submission]]
BENCHMARK_TEARDOWN_TIMEOUT_SECONDS = 30.0


async def _evaluate_benchmark(
    adapter: BenchmarkAdapter,
    session: BenchmarkSession,
    *,
    timeout: float,
) -> EvaluationResult:
    """Apply the shared evaluation contract and fail closed on timeout or bad output."""
    try:
        raw = await asyncio.wait_for(adapter.evaluate(session), timeout=max(timeout, 0.001))
    except TimeoutError as exc:
        raise RuntimeError("benchmark evaluator timed out") from exc
    if isinstance(raw, EvaluationResult):
        result = raw
    else:
        if (
            not isinstance(raw, dict)
            or not isinstance(raw.get("success"), bool)
            or isinstance(raw.get("score", 0), bool)
            or not isinstance(raw.get("score", 0), (int, float))
        ):
            raise TypeError("benchmark evaluator returned an inconsistent result")
        try:
            result = EvaluationResult.model_validate(raw)
        except (ValidationError, TypeError, ValueError) as exc:
            raise TypeError("benchmark evaluator returned an inconsistent result") from exc
    if not math.isfinite(result.score) or result.score < 0:
        raise ValueError("benchmark evaluator returned an inconsistent score")
    return result


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
        solver_profile: SolverProfile = DEFAULT_SOLVER_PROFILE,
    ) -> dict:
        if agent.type in {"docker", "pi"} and agent.network == "environment":
            raise ValueError(
                "externally provisioned benchmarks do not provide a Harness environment network; "
                "configure the container Agent with network: host or a future benchmark network"
            )
        run_id = f"{case.benchmark}_{case.id}_{uuid.uuid4().hex[:8]}"
        run_dir = (self.runs_root / run_id).resolve()
        run_dir.mkdir(parents=True, exist_ok=False)
        agent_workspace = create_agent_workspace(run_dir)
        trace = TraceRecorder(run_dir / "trace.jsonl", run_id, case.id)
        try:
            session = await adapter.provision(case)
        except Exception as exc:
            trace.emit(
                "benchmark.provision.error",
                data={
                    "error_type": type(exc).__name__,
                    "message": "benchmark provision failed; details omitted",
                },
            )
            raise
        lifecycle = RunLifecycle()
        lifecycle.transition("running")
        teardown_error: str | None = None
        teardown_started = False

        async def teardown_once() -> None:
            nonlocal teardown_error, teardown_started
            if teardown_started:
                return
            teardown_started = True
            cleanup_task = asyncio.create_task(adapter.teardown(session))
            interrupted = False
            cleanup_exception: BaseException | None = None
            deadline = (
                asyncio.get_running_loop().time() + BENCHMARK_TEARDOWN_TIMEOUT_SECONDS
            )
            try:
                while True:
                    remaining = max(
                        0.0, deadline - asyncio.get_running_loop().time()
                    )
                    try:
                        await asyncio.wait_for(
                            asyncio.shield(cleanup_task), timeout=remaining
                        )
                        break
                    except asyncio.CancelledError as exc:
                        if cleanup_task.done():
                            if cleanup_task.cancelled():
                                cleanup_exception = exc
                            break
                        interrupted = True
                    except TimeoutError as exc:
                        cleanup_exception = exc
                        cleanup_task.cancel()
                        try:
                            await asyncio.wait_for(asyncio.shield(cleanup_task), timeout=1.0)
                        except (asyncio.CancelledError, TimeoutError):
                            pass
                        except Exception as cancel_exc:  # noqa: BLE001
                            cleanup_exception = cleanup_exception or cancel_exc
                        break
                    except Exception as exc:  # noqa: BLE001 - preserve the run failure.
                        cleanup_exception = exc
                        break
                if cleanup_exception is None:
                    try:
                        cleanup_task.result()
                    except asyncio.CancelledError as exc:
                        cleanup_exception = exc
                    except Exception as exc:  # noqa: BLE001 - preserve the run failure.
                        cleanup_exception = exc
            finally:
                if not cleanup_task.done():
                    cleanup_task.cancel()
            if cleanup_exception is not None:
                teardown_error = f"{type(cleanup_exception).__name__}: details omitted"
                trace.emit(
                    "benchmark.teardown.error",
                    data={
                        "error_type": type(cleanup_exception).__name__,
                        "message": "benchmark teardown failed; details omitted",
                    },
                )
            current_task = asyncio.current_task()
            if interrupted or (current_task is not None and current_task.cancelling()):
                raise asyncio.CancelledError

        try:
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
                    attributes={
                        "address": target.address,
                        **sanitize_observability_data(target.metadata),
                    },
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
                budget_limits=budgets.model_dump(exclude_none=True),
                agent_workspace=agent_workspace,
                solver_profile=solver_profile,
            )
            world = runtime.world
            context_builder = runtime.context_builder
            progress = runtime.progress
            solver_loop = runtime.loop
            benchmark_verifier_authority = runtime.verifier_registry.register("benchmark_evaluator")
            progress_path = run_dir / "progress.json"
            sync_agent_workspace(run_dir=run_dir, workspace=agent_workspace)

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
                    "solver_profile": solver_profile.value,
                    "world_revision": world.snapshot.revision,
                },
            )

            agent_result: AgentResult | None = None
            submission_results: list[dict] = []
            submitted_keys: set[tuple[str, str]] = set()
            evaluation: EvaluationResult | None = None
            evaluation_error: Exception | None = None
            submission_inbox = SubmissionInbox(agent_workspace / "submission.inbox.jsonl")

        except asyncio.CancelledError:
            if not lifecycle.terminal:
                lifecycle.settle(succeeded=False, cancelled=True)
            await teardown_once()
            raise
        except BaseException:
            if not lifecycle.terminal:
                lifecycle.settle(succeeded=False)
            await teardown_once()
            raise

        async def evaluate_final() -> bool:
            nonlocal evaluation, evaluation_error
            if evaluation_error is not None:
                return False
            try:
                evaluation = await _evaluate_benchmark(
                    adapter,
                    session,
                    timeout=budgets.wall_time - (time.monotonic() - started),
                )
            except Exception as exc:  # noqa: BLE001 - unavailable evaluators fail closed.
                evaluation_error = exc
                return False
            return evaluation.success

        async def submit_candidate(submission: Submission, agent_session=None) -> bool:
            nonlocal evaluation
            key = (submission.type, submission.value)
            if key in submitted_keys:
                return False
            submitted_keys.add(key)
            evaluation = None
            trace.emit(
                "benchmark.submission.proposed",
                actor="agent",
                data={
                    "type": submission.type,
                    "value_sha256": _submission_hash(submission.value),
                },
            )
            submitted = await adapter.submit(session, submission)
            safe_submission_metadata = sanitize_observability_data(submitted.metadata)
            progress.record_submission(
                accepted=submitted.accepted,
                completed=submitted.completed,
            )
            progress.write(progress_path)
            submission_digest = _submission_hash(submission.value)
            data = submitted.model_dump()
            data["metadata"] = safe_submission_metadata
            data["value_sha256"] = submission_digest
            submission_results.append(data)
            trace.emit("benchmark.submission.result", data=data)
            submission_completed = False
            if submitted.completed:
                # The submission response is an intermediate signal. Stop only
                # after the adapter's independent final evaluator agrees.
                submission_completed = await evaluate_final()

            revision_before_feedback = world.snapshot.revision
            # In Pi-first mode, SDK feedback already lives in progress, trace,
            # and submission_results. Avoid duplicating it into the legacy World graph.
            if solver_loop.world_context_enabled:
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
                            "metadata": safe_submission_metadata,
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
            # Only legacy World profiles project the internal evidence graph to Pi.
            # The default Pi-first path receives benchmark feedback directly.
            if solver_loop.world_context_enabled:
                (run_dir / "world.context.txt").write_text(
                    context_builder.render(
                        world.snapshot,
                        query=session.objective.description,
                    ),
                    encoding="utf-8",
                )
            sync_agent_workspace(run_dir=run_dir, workspace=agent_workspace)

            feedback_data = {
                "submission_type": submission.type,
                "accepted": submitted.accepted,
                "score_delta": submitted.score_delta,
                "completed": submitted.completed,
                "metadata": safe_submission_metadata,
                "value_sha256": submission_digest,
                "world_revision": revision_after_feedback,
                "replan_required": "benchmark_negative_feedback" in progress.replan_reasons,
                "replan_reasons": list(progress.replan_reasons),
            }
            if agent_session is not None:
                await agent_session.observe(
                    AgentObservation(
                        type="benchmark.feedback",
                        data=feedback_data,
                    )
                )
                if solver_loop.planner_enabled and feedback_data["replan_required"]:
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
                if (
                    solver_loop.world_context_enabled
                    and revision_after_feedback != revision_before_feedback
                ):
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
            return submission_completed

        action_owner = run_action_owner(run_dir)
        action_owner_acquired = False
        try:
            action_owner.__enter__()
            action_owner_acquired = True
            adapter_instance = build_agent_adapter(
                agent,
                allow_host_agent=allow_host_agent,
                trace=trace,
            )
            run_kwargs = {
                "task": task,
                "task_dir": agent_workspace,
                "run_dir": agent_workspace,
                "environment_project": None,
                "environment_network": None,
                "seed": seed,
            }

            start_session = getattr(adapter_instance, "start_session", None)
            completed_online = False
            if callable(start_session):
                agent_session = await start_session(**run_kwargs)
                try:
                    await save_session_checkpoint(
                        run_dir=run_dir,
                        session=agent_session,
                        world_revision=world.snapshot.revision,
                        progress=progress,
                        plan_revision=solver_loop.planned_world_revision,
                        processed_event_ids=solver_loop.processed_event_ids,
                    )
                    startup_decision = solver_loop.decide(force_replan=True)
                    stop_before_stream = startup_decision.action.value == "stop"
                    if stop_before_stream:
                        trace.emit(
                            "solver.decision",
                            actor="harness",
                            data={"action": "stop", "reason": startup_decision.reason},
                        )
                        await agent_session.observe(
                            AgentObservation(
                                type="solver.stop", data={"reason": startup_decision.reason}
                            )
                        )
                        await agent_session.close(f"solver-stop:{startup_decision.reason}")
                    else:
                        await solver_loop.maybe_replan(agent_session, force=True)
                        async for event in agent_session.events():
                            decision = await solver_loop.process_event(agent_session, event)
                            sync_agent_workspace(run_dir=run_dir, workspace=agent_workspace)
                            await save_session_checkpoint(
                                run_dir=run_dir,
                                session=agent_session,
                                world_revision=world.snapshot.revision,
                                progress=progress,
                                plan_revision=solver_loop.planned_world_revision,
                                processed_event_ids=solver_loop.processed_event_ids,
                            )
                            for submission in submission_inbox.poll():
                                if await submit_candidate(submission, agent_session):
                                    completed_online = True
                                    await agent_session.close("objective-complete")
                                    break
                            if completed_online:
                                break
                            if decision.action.value == "stop":
                                await agent_session.close(f"solver-stop:{decision.reason}")
                                break
                    agent_result = await agent_session.result()
                    await save_session_checkpoint(
                        run_dir=run_dir,
                        session=agent_session,
                        world_revision=world.snapshot.revision,
                        progress=progress,
                        plan_revision=solver_loop.planned_world_revision,
                        budget_used=agent_result.metrics.as_dict(),
                        processed_event_ids=solver_loop.processed_event_ids,
                    )
                except BaseException as stream_exc:
                    if not completed_online:
                        close = getattr(agent_session, "close", None)
                        if callable(close):
                            try:
                                close_reason = (
                                    "cancelled"
                                    if isinstance(stream_exc, asyncio.CancelledError)
                                    else "benchmark_run_error"
                                )
                                await close(close_reason)
                            except Exception as close_exc:  # noqa: BLE001 - preserve original failure
                                trace.emit(
                                    "agent.close_error",
                                    data={"error_type": type(close_exc).__name__,
                                          "message": str(close_exc)},
                                )
                    raise
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

            if evaluation is None:
                await evaluate_final()
            lifecycle.transition("verifying")
            # The final evaluator is authoritative. A submission marked
            # completed is feedback, not a substitute for this result.
            if evaluation_error is None and evaluation is not None:
                persist_objective_verdict(
                    run_dir=run_dir,
                    run_id=run_id,
                    world_revision=world.snapshot.revision,
                    producer_authority=benchmark_verifier_authority,
                    registry=runtime.verifier_registry,
                    progress=progress,
                    trace=trace,
                    success=evaluation.success,
                    score=evaluation.score,
                    milestones=evaluation.milestones,
                )
            else:
                persist_objective_verdict(
                    run_dir=run_dir,
                    run_id=run_id,
                    world_revision=world.snapshot.revision,
                    producer_authority=benchmark_verifier_authority,
                    registry=runtime.verifier_registry,
                    progress=progress,
                    trace=trace,
                    status="unavailable",
                    failure_type=type(evaluation_error).__name__
                    if evaluation_error is not None
                    else "RuntimeError",
                )
                evaluation = EvaluationResult(
                    success=False,
                    score=0.0,
                    message="benchmark evaluator unavailable",
                    metadata={
                        "available": False,
                        "error_type": type(evaluation_error).__name__
                        if evaluation_error is not None
                        else "RuntimeError",
                    },
                )
            progress.write(progress_path)
        except asyncio.CancelledError:
            if not lifecycle.terminal:
                lifecycle.settle(succeeded=False, cancelled=True)
            raise
        except Exception:
            if not lifecycle.terminal:
                lifecycle.settle(succeeded=False)
            raise
        finally:
            try:
                await teardown_once()
            finally:
                if action_owner_acquired:
                    action_owner.__exit__(None, None, None)

        if agent_result is None:
            raise RuntimeError("agent did not start")

        status = (
            "error"
            if evaluation_error is not None or evaluation is None
            else resolve_run_status(
                objective_completed=progress.objective_completed,
                timed_out=agent_result.timed_out,
                budget_exceeded=bool(agent_result.budget_exceeded),
            )
        )
        lifecycle.settle(
            succeeded=progress.objective_completed,
            exhausted=not progress.objective_completed
            and (agent_result.timed_out or bool(agent_result.budget_exceeded)),
        )
        finalize_world_goal(
            world=world,
            goal=root_goal,
            context_builder=context_builder,
            run_dir=run_dir,
            status=status,
            success=bool(evaluation and evaluation.success),
            score=evaluation.score if evaluation is not None else 0.0,
        )
        world.flush()

        result = {
            "schema_version": "harness/result/v2",
            "run_id": run_id,
            "benchmark": {
                "type": session.benchmark,
                "case_id": session.case_id,
                "domain": case.domain,
                "difficulty": case.difficulty,
                "session": sanitize_observability_data(session.metadata),
            },
            "agent_id": agent.id,
            "status": status,
            "termination_reason": ("objective-complete" if progress.objective_completed else None),
            "success": bool(evaluation and evaluation.success),
            "score": evaluation.score if evaluation is not None else 0.0,
            "message": (
                sanitize_observability_data({"message": evaluation.message}).get("message")
                if evaluation is not None
                else "benchmark evaluator unavailable"
            ),
            "milestones": (
                sanitize_observability_data(evaluation.milestones) if evaluation is not None else {}
            ),
            "evaluation": (
                sanitize_observability_data(evaluation.metadata) if evaluation is not None else {}
            ),
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
