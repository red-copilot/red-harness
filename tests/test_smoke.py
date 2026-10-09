import asyncio
import hashlib
import json
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

from harness.agent import AgentResult
from harness.budget import UsageMetrics
from harness.models import load_agent, load_task
from harness.orchestrator import Orchestrator, _remaining_budget_task
from harness.runtime.agent_workspace import create_agent_workspace
from harness.runtime.bootstrap import bootstrap_solver
from harness.runtime.checkpoint import save_session_checkpoint
from harness.session import AgentCheckpoint, AgentEvent
from harness.trace import TraceRecorder
from harness.world import Goal


def test_smoke_run(tmp_path: Path) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    result = Orchestrator(tmp_path / "runs").run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=42,
        run_id="run_worker_owned",
    )
    assert result["run_id"] == "run_worker_owned"
    assert result["status"] == "finished"
    assert result["success"] is True
    run_dir = tmp_path / "runs" / result["run_id"]
    assert (run_dir / "world.db").is_file()
    assert result["score"] == 100
    assert result["seed"] == 42
    assert result["metrics"]["total_tokens"] == 12
    assert result["metrics"]["model_calls"] == 1
    assert result["metrics"]["tool_calls"] == 1
    assert result["metrics"]["cost_usd"] == 0.01


def test_unavailable_task_verifier_is_recorded_and_run_fails_closed(monkeypatch, tmp_path: Path) -> None:
    from harness.audit import audit_run
    from harness.verifier import VerifierError

    def unavailable(*_args, **_kwargs):
        raise VerifierError("simulated verifier timeout with private details")

    monkeypatch.setattr("harness.orchestrator.run_verifier", unavailable)
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    result = Orchestrator(tmp_path / "runs").run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=1,
    )

    run_dir = tmp_path / "runs" / result["run_id"]
    verdict = result["progress"]["objective_verdict"]
    evidence = verdict["evidence"][0]
    artifact = json.loads((run_dir / evidence["artifact_ref"]).read_text(encoding="utf-8"))
    assert result["status"] == "error"
    assert result["success"] is False
    assert result["failure_class"] == "verifier_unavailable"
    assert verdict["status"] == "unavailable"
    assert artifact["failure_type"] == "VerifierError"
    assert "private details" not in json.dumps(artifact)
    assert audit_run(run_dir).status == "consistent"


def test_environment_start_failure_is_classified_separately_from_solver_failure(
    monkeypatch, tmp_path: Path
) -> None:
    class FailingEnvironment:
        def start(self):
            raise RuntimeError("simulated startup failure api_key=private-canary-value")

        def stop(self, *, preserve_state=False):
            return None

    monkeypatch.setattr(
        "harness.orchestrator.build_environment", lambda *_args, **_kwargs: FailingEnvironment()
    )
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    result = Orchestrator(tmp_path / "runs").run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=1,
    )

    assert result["success"] is False
    assert result["failure_class"] == "environment_failure"
    assert result["progress"]["objective_verdict"] is None
    run_dir = tmp_path / "runs" / result["run_id"]
    assert "private-canary-value" not in json.dumps(result)
    assert "private-canary-value" not in "".join(
        path.read_text(encoding="utf-8")
        for path in run_dir.iterdir()
        if path.is_file() and path.suffix in {".json", ".jsonl"}
    )


def test_trace_failure_during_gateway_cleanup_does_not_skip_environment_stop(
    monkeypatch, tmp_path: Path
) -> None:
    from harness.environment import EnvironmentHandle
    from harness.gateway_runtime import GatewayConfig

    class Gateway:
        def start(self):
            return None

        def stop(self):
            raise RuntimeError("gateway cleanup failed")

    class Environment:
        stopped = False

        def start(self):
            return EnvironmentHandle(provider="none")

        def stop(self, *, preserve_state=False):
            del preserve_state
            self.stopped = True

    environment = Environment()
    monkeypatch.setattr("harness.orchestrator.build_environment", lambda *_a, **_k: environment)
    monkeypatch.setattr("harness.orchestrator.build_gateway_runtime", lambda **_kwargs: Gateway())
    original_emit = TraceRecorder.emit

    def fail_gateway_error_trace(self, event_type, **kwargs):
        if event_type == "gateway.error":
            raise OSError("trace filesystem unavailable")
        return original_emit(self, event_type, **kwargs)

    monkeypatch.setattr(TraceRecorder, "emit", fail_gateway_error_trace)
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")

    result = Orchestrator(tmp_path / "runs").run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        gateway_config=GatewayConfig(),
    )

    assert environment.stopped is True
    assert (tmp_path / "runs" / result["run_id"] / "result.json").is_file()


def test_smoke_resume_world_state(tmp_path: Path) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    orchestrator = Orchestrator(tmp_path / "runs")

    first = orchestrator.run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=1,
    )
    first_dir = tmp_path / "runs" / first["run_id"]
    world_events = first_dir / "world.events.jsonl"
    world_context = first_dir / "world.context.txt"

    assert world_events.is_file()
    assert world_context.is_file()
    assert "Red Harness World Context" in world_context.read_text(encoding="utf-8")

    second = orchestrator.run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=2,
        resume_world_events=world_events,
    )

    assert second["world"]["resumed"] is True
    assert second["world"]["revision"] >= first["world"]["revision"]


def test_smoke_resume_sqlite_world_state(tmp_path: Path) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    orchestrator = Orchestrator(tmp_path / "runs")

    first = orchestrator.run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=3,
    )
    first_dir = tmp_path / "runs" / first["run_id"]
    world_db = first_dir / "world.db"
    assert world_db.is_file()

    second = orchestrator.run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=4,
        resume_world_events=world_db,
    )

    assert second["world"]["resumed"] is True
    assert second["world"]["revision"] >= first["world"]["revision"]
    second_dir = tmp_path / "runs" / second["run_id"]
    assert (second_dir / "world.db").is_file()


def test_orchestrator_refuses_to_resume_terminal_run(tmp_path: Path) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    orchestrator = Orchestrator(tmp_path / "runs")
    first = orchestrator.run(
        task=load_task(task_path), task_path=task_path,
        agent=load_agent(agent_path), agent_path=agent_path, allow_host_agent=True,
    )

    try:
        orchestrator.run(
            task=load_task(task_path), task_path=task_path,
            agent=load_agent(agent_path), agent_path=agent_path,
            allow_host_agent=True, resume_run_dir=tmp_path / "runs" / first["run_id"],
        )
    except ValueError as exc:
        assert "terminal" in str(exc)
    else:
        raise AssertionError("terminal run must not be resumed")


class _ResumeSession:
    def __init__(self, run_dir: Path) -> None:
        self.run_dir = run_dir

    async def events(self):
        (self.run_dir / "proof.txt").write_text("red-harness-ok", encoding="utf-8")
        if False:
            yield AgentEvent(type="unused", data={})

    async def result(self):
        return AgentResult(0, False, None, "", "", UsageMetrics())

    async def checkpoint(self):
        return AgentCheckpoint("checkpoint-resume", 0, 0, "pi-session-existing")

    async def observe(self, _observation):
        return None


class _ResumeAdapter:
    def __init__(self) -> None:
        self.resume_session_id = None
        self.session_task = None

    async def start_session(self, *, run_dir: Path, task, resume_session_id=None, **_kwargs):
        self.resume_session_id = resume_session_id
        self.session_task = task
        return _ResumeSession(run_dir)


def _prepare_incomplete_pi_run(
    runs_root: Path,
    task_path: Path,
    agent_path: Path,
    *,
    budget_used: dict | None = None,
):
    task = load_task(task_path)
    agent = load_agent(agent_path)
    run_dir = runs_root / "run_incomplete"
    run_dir.mkdir(parents=True)
    workspace = create_agent_workspace(run_dir)
    trace = TraceRecorder(run_dir / "trace.jsonl", run_dir.name, task.id)
    trace.emit("run.started", data={
        "agent_id": agent.id,
        "task_sha256": hashlib.sha256(task_path.read_bytes()).hexdigest(),
        "agent_sha256": hashlib.sha256(agent_path.read_bytes()).hexdigest(),
    })
    runtime = bootstrap_solver(
        run_dir=run_dir, trace=trace,
        goal=Goal(id=f"goal:{task.id}:objective", description=task.objective.description),
        actor=f"agent:{agent.id}", context_query=task.objective.description,
        skills_root="skills", budget_limits=task.budgets.model_dump(exclude_none=True),
        agent_workspace=workspace,
    )

    class _CheckpointSession:
        async def checkpoint(self):
            return AgentCheckpoint("checkpoint-existing", 0, 0, "pi-session-existing")

    asyncio.run(save_session_checkpoint(
        run_dir=run_dir, session=_CheckpointSession(),
        world_revision=runtime.world.snapshot.revision, progress=runtime.progress,
        budget_used=budget_used or {}, processed_event_ids={},
    ))
    return task, agent, run_dir


def test_resume_wall_time_budget_rounds_elapsed_time_up() -> None:
    task = load_task(Path("benchmarks/examples/hello/task.yaml"))
    task = task.model_copy(
        update={"budgets": task.budgets.model_copy(update={"wall_time": 10})}
    )
    started_at = datetime.now(UTC) - timedelta(seconds=3.1)

    resumed = _remaining_budget_task(task, started_at, UsageMetrics())

    assert resumed.budgets.wall_time == 6


def test_orchestrator_resumes_pi_session_in_existing_run(monkeypatch, tmp_path: Path) -> None:
    import harness.orchestrator as orchestrator_module

    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/pi.yaml")
    runs_root = tmp_path / "runs"
    task, agent, run_dir = _prepare_incomplete_pi_run(
        runs_root,
        task_path,
        agent_path,
        budget_used={"input_tokens": 4, "output_tokens": 5, "total_tokens": 9,
                     "model_calls": 1, "tool_calls": 2, "cost_usd": 0.01},
    )
    from harness.world import SQLiteWorldRepository

    world = SQLiteWorldRepository(run_dir / "world.events.jsonl")
    initial_revision = world.snapshot.revision
    fake = _ResumeAdapter()
    monkeypatch.setattr(orchestrator_module, "build_agent_adapter", lambda *_a, **_k: fake)

    result = Orchestrator(runs_root).run(
        task=task, task_path=task_path, agent=agent, agent_path=agent_path,
        resume_run_dir=run_dir,
    )

    assert result["run_id"] == run_dir.name
    assert result["success"] is True
    assert fake.resume_session_id == "pi-session-existing"
    assert fake.session_task.budgets.max_tokens == 91
    assert fake.session_task.budgets.max_tool_calls == 1
    assert result["metrics"]["total_tokens"] == 9
    assert result["metrics"]["tool_calls"] == 2
    assert result["world"]["revision"] > initial_revision
    goal_events = [event for event in world.events()
                   if event.actor == "harness" and event.kind == "goal"]
    assert len({event.object["id"] for event in goal_events}) == 1


def test_failed_resume_preserves_checkpoint_and_environment_state(monkeypatch, tmp_path: Path):
    import harness.orchestrator as orchestrator_module
    from harness.environment import EnvironmentHandle

    class _FailingSession(_ResumeSession):
        closed = False

        async def events(self):
            raise RuntimeError("injected resumed session failure")
            yield

        async def close(self, _reason):
            self.closed = True

    class _FailingAdapter(_ResumeAdapter):
        async def start_session(self, *, run_dir: Path, task, resume_session_id=None, **_kwargs):
            self.resume_session_id = resume_session_id
            self.session_task = task
            self.session = _FailingSession(run_dir)
            return self.session

    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/pi.yaml")
    runs_root = tmp_path / "runs"
    _task, _agent, run_dir = _prepare_incomplete_pi_run(
        runs_root, task_path, agent_path
    )
    adapter = _FailingAdapter()
    class _EnvironmentProbe:
        preserve_state = None

        def start(self):
            return EnvironmentHandle(provider="fake")

        def stop(self, *, preserve_state=False):
            self.preserve_state = preserve_state

    environment = _EnvironmentProbe()
    monkeypatch.setattr(orchestrator_module, "build_agent_adapter", lambda *_a, **_k: adapter)
    monkeypatch.setattr(orchestrator_module, "build_environment", lambda *_a, **_k: environment)

    result = Orchestrator(runs_root).run(
        task=load_task(task_path), task_path=task_path,
        agent=load_agent(agent_path), agent_path=agent_path,
        resume_run_dir=run_dir,
    )

    assert result["status"] == "error"
    assert result["run_id"] == run_dir.name
    assert not (run_dir / "result.json").exists()
    assert adapter.session.closed is True
    assert environment.preserve_state is True
    trace_events = [
        json.loads(line)
        for line in (run_dir / "trace.jsonl").read_text(encoding="utf-8").splitlines()
    ]
    assert not any(event["type"] == "run.finished" for event in trace_events)
    assert any(event["type"] == "run.interrupted" for event in trace_events)


def test_resume_rejects_agent_hash_mismatch(tmp_path: Path) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    pi_path = Path("agents/examples/pi.yaml")
    altered_agent_path = Path("agents/examples/demo.yaml")
    runs_root = tmp_path / "runs"
    task, agent, run_dir = _prepare_incomplete_pi_run(runs_root, task_path, pi_path)

    with pytest.raises(ValueError, match="agent hash"):
        Orchestrator(runs_root).run(
            task=task, task_path=task_path, agent=agent, agent_path=altered_agent_path,
            resume_run_dir=run_dir,
        )


def test_resume_rejects_corrupt_checkpoint_before_starting_agent(tmp_path: Path) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/pi.yaml")
    runs_root = tmp_path / "runs"
    task, agent, run_dir = _prepare_incomplete_pi_run(runs_root, task_path, agent_path)
    (run_dir / "checkpoint.json").write_text("{corrupt", encoding="utf-8")

    with pytest.raises(ValueError):
        Orchestrator(runs_root).run(
            task=task, task_path=task_path, agent=agent, agent_path=agent_path,
            resume_run_dir=run_dir,
        )


class _LiveWorldSession:
    def __init__(self, run_dir: Path) -> None:
        self.run_dir = run_dir
        self.feedback = []

    async def events(self):
        (self.run_dir / "proof.txt").write_text("red-harness-ok", encoding="utf-8")
        yield AgentEvent(
            type="action.intent",
            data={
                "action_id": "local-proof",
                "description": "write proof",
                "expected_observations": ["proof written"],
            },
        )
        yield AgentEvent(
            type="world.observe",
            data={
                "id": "obs-live-local",
                "type": "task.progress",
                "summary": "proof written",
                "content": {"proof": "written"},
                "confidence": 1.0,
            },
        )
        yield AgentEvent(type="progress.updated", data={"made_progress": True})

    async def observe(self, observation):
        self.feedback.append(observation)

    async def result(self):
        return AgentResult(
            returncode=0,
            timed_out=False,
            budget_exceeded=None,
            stdout="",
            stderr="",
            metrics=UsageMetrics(),
        )


class _LiveWorldAdapter:
    def __init__(self) -> None:
        self.session: _LiveWorldSession | None = None

    async def start_session(self, *, run_dir: Path, **_kwargs):
        self.session = _LiveWorldSession(run_dir)
        return self.session


def test_orchestrator_ingests_world_state_during_session(
    monkeypatch,
    tmp_path: Path,
) -> None:
    import harness.orchestrator as orchestrator_module

    fake = _LiveWorldAdapter()
    monkeypatch.setattr(
        orchestrator_module,
        "build_agent_adapter",
        lambda *_args, **_kwargs: fake,
    )

    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    result = Orchestrator(tmp_path / "runs").run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        seed=9,
    )

    assert result["success"] is True
    assert result["world"]["revision"] >= 3
    assert result["world"]["aci"]["world_mutations"] >= 1
    assert result["world"]["legacy_inbox"]["accepted"] == 0
    assert result["world"]["agent_authored_records"] >= 1
    assert result["progress"]["verified_actions"] == 0
    run_dir = tmp_path / "runs" / result["run_id"]
    assert fake.session is not None
    assert fake.session.run_dir == run_dir / "agent-workspace"
    assert (fake.session.run_dir / "proof.txt").is_file()
    assert not (run_dir / "proof.txt").exists()
    assert (run_dir / "result.json").is_file()
    assert result["progress"]["pending_actions"] == 0
    assert result["progress"]["skipped_verifications"] >= 1
    assert fake.session is not None
    assert any(item.type == "world.state.updated" for item in fake.session.feedback)

    run_dir = tmp_path / "runs" / result["run_id"]
    context = (run_dir / "world.context.txt").read_text(encoding="utf-8")
    assert "obs-live-local" in context


class _NoProgressSession:
    def __init__(self, run_dir: Path) -> None:
        self.run_dir = run_dir
        self.close_reason = None

    async def events(self):
        (self.run_dir / "proof.txt").write_text("red-harness-ok", encoding="utf-8")
        for index in range(6):
            yield AgentEvent(
                type="tool.result",
                event_id=f"no-progress-{index}",
                data={"tool": "probe", "tool_call_id": f"call-{index}"},
            )

    async def observe(self, _observation):
        return None

    async def close(self, reason: str):
        self.close_reason = reason

    async def checkpoint(self):
        return AgentCheckpoint("no-progress-checkpoint", 6, 0)

    async def result(self):
        return AgentResult(0, False, None, "", "", UsageMetrics())


class _NoProgressAdapter:
    def __init__(self) -> None:
        self.session = None

    async def start_session(self, *, run_dir: Path, **_kwargs):
        self.session = _NoProgressSession(run_dir)
        return self.session


def test_orchestrator_closes_agent_at_no_progress_bound(monkeypatch, tmp_path: Path) -> None:
    import harness.orchestrator as orchestrator_module

    adapter = _NoProgressAdapter()
    monkeypatch.setattr(orchestrator_module, "build_agent_adapter", lambda *_a, **_k: adapter)
    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    result = Orchestrator(tmp_path / "runs").run(
        task=load_task(task_path),
        task_path=task_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
    )

    assert adapter.session.close_reason == "solver-stop:no_progress_limit"
    assert result["success"] is True
    assert result["progress"]["no_progress_count"] == 0
    run_dir = tmp_path / "runs" / result["run_id"]
    trace = [json.loads(line) for line in (run_dir / "trace.jsonl").read_text().splitlines()]
    assert any(
        event["type"] == "solver.decision"
        and event["data"]["action"] == "stop"
        and event["data"]["reason"] == "no_progress_limit"
        and event["data"]["no_progress_count"] == 6
        for event in trace
    )
