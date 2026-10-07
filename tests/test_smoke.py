from pathlib import Path

from harness.agent import AgentResult
from harness.budget import UsageMetrics
from harness.models import load_agent, load_task
from harness.orchestrator import Orchestrator
from harness.session import AgentEvent


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
    )
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


class _LiveWorldSession:
    def __init__(self, run_dir: Path) -> None:
        self.run_dir = run_dir
        self.feedback = []

    async def events(self):
        (self.run_dir / "proof.txt").write_text("red-harness-ok", encoding="utf-8")
        yield AgentEvent(
            type="action.intent",
            source="agent",
            trusted=False,
            data={
                "action_id": "local-proof",
                "description": "write proof",
                "expected_observations": ["proof written"],
            },
        )
        yield AgentEvent(
            type="world.observe",
            source="agent",
            trusted=False,
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
    assert result["progress"]["verified_actions"] >= 1
    assert fake.session is not None
    assert any(item.type == "world.state.updated" for item in fake.session.feedback)

    run_dir = tmp_path / "runs" / result["run_id"]
    context = (run_dir / "world.context.txt").read_text(encoding="utf-8")
    assert "obs-live-local" in context
