from __future__ import annotations

import asyncio
import hashlib
import json
import shutil
import subprocess
from pathlib import Path

import pytest

from harness.agent_workspace import create_agent_workspace
from harness.models import load_agent, load_task
from harness.orchestrator import Orchestrator
from harness.runtime.bootstrap import bootstrap_solver
from harness.runtime.checkpoint import save_session_checkpoint
from harness.session import AgentCheckpoint
from harness.trace import TraceRecorder
from harness.world import Goal


@pytest.mark.skipif(shutil.which("docker") is None, reason="Docker is unavailable")
def test_pi_rpc_restores_existing_session_in_docker(tmp_path: Path) -> None:
    image = "harness-pi-rpc-resume-probe:ci"
    if subprocess.run(
        ["docker", "image", "inspect", image], capture_output=True, check=False
    ).returncode:
        pytest.skip(f"required test image is not built: {image}")

    task_path = Path("benchmarks/examples/hello/task.yaml")
    agent_path = tmp_path / "pi-probe.yaml"
    agent_path.write_text(
        "apiVersion: harness/v1\n"
        "id: pi-resume-probe\n"
        "type: pi\n"
        f"image: {image}\n"
        "network: none\n"
        "pi:\n"
        "  binary: /usr/local/bin/fake-pi\n"
        "  provider: fake\n"
        "  model: probe\n"
        "  offline: true\n",
        encoding="utf-8",
    )
    task = load_task(task_path)
    agent = load_agent(agent_path)
    runs_root = tmp_path / "runs"
    run_dir = runs_root / "run_pi_resume"
    run_dir.mkdir(parents=True)
    workspace = create_agent_workspace(run_dir)
    trace = TraceRecorder(run_dir / "trace.jsonl", run_dir.name, task.id)
    trace.emit(
        "run.started",
        data={
            "agent_id": agent.id,
            "task_sha256": hashlib.sha256(task_path.read_bytes()).hexdigest(),
            "agent_sha256": hashlib.sha256(agent_path.read_bytes()).hexdigest(),
        },
    )
    runtime = bootstrap_solver(
        run_dir=run_dir,
        trace=trace,
        goal=Goal(id=f"goal:{task.id}:objective", description=task.objective.description),
        actor=f"agent:{agent.id}",
        context_query=task.objective.description,
        skills_root="skills",
        budget_limits=task.budgets.model_dump(exclude_none=True),
        agent_workspace=workspace,
    )

    class ExistingSession:
        async def checkpoint(self):
            return AgentCheckpoint(
                id="existing-session-checkpoint",
                event_offset=0,
                feedback_offset=0,
                session_id="pi-resume-probe-session",
            )

    asyncio.run(
        save_session_checkpoint(
            run_dir=run_dir,
            session=ExistingSession(),
            world_revision=runtime.world.snapshot.revision,
            progress=runtime.progress,
        )
    )

    result = Orchestrator(runs_root).run(
        task=task,
        task_path=task_path,
        agent=agent,
        agent_path=agent_path,
        resume_run_dir=run_dir,
    )

    assert result["run_id"] == run_dir.name
    assert result["success"] is True
    assert result["metrics"]["total_tokens"] == 2
    assert (workspace / "proof.txt").read_text(encoding="utf-8") == "red-harness-ok"
    events = [
        json.loads(line)
        for line in (run_dir / "trace.jsonl").read_text(encoding="utf-8").splitlines()
    ]
    assert any(event["type"] == "run.resumed" for event in events)
