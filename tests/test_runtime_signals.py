from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest
import yaml


@pytest.mark.skipif(os.name != "posix", reason="POSIX process and signal semantics required")
def test_sigterm_cancels_agent_and_persists_terminal_run(tmp_path: Path) -> None:
    root = Path.cwd()
    task_path = root / "benchmarks/examples/hello/task.yaml"
    agent_path = tmp_path / "long-agent.yaml"
    agent_path.write_text(
        yaml.safe_dump(
            {
                "apiVersion": "harness/v1",
                "id": "signal-test-agent",
                "type": "cli",
                "command": [
                    sys.executable,
                    "-c",
                    (
                        "import os,pathlib,subprocess,sys,time;"
                        "pathlib.Path(os.environ['HARNESS_RUN_DIR'],'child.pid').write_text(str(os.getpid()));"
                        "p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)']);"
                        "pathlib.Path(os.environ['HARNESS_RUN_DIR'],'grandchild.pid').write_text(str(p.pid));"
                        "time.sleep(60)"
                    ),
                ],
            }
        ),
        encoding="utf-8",
    )
    runs_root = tmp_path / "runs"
    run_id = "run_signal_cleanup"
    child_script = (
        "from pathlib import Path;"
        "from harness.models import load_agent,load_task;"
        "from harness.orchestrator import Orchestrator;"
        "import sys;"
        "Orchestrator(sys.argv[1]).run(task=load_task(sys.argv[2]),task_path=Path(sys.argv[2]),"
        "agent=load_agent(sys.argv[3]),agent_path=Path(sys.argv[3]),allow_host_agent=True,"
        "run_id=sys.argv[4])"
    )
    env = os.environ.copy()
    env["PYTHONPATH"] = str(root / "src")
    process = subprocess.Popen(
        [sys.executable, "-c", child_script, str(runs_root), str(task_path), str(agent_path), run_id],
        cwd=root,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    run_dir = runs_root / run_id
    child_pid: int | None = None
    grandchild_pid: int | None = None
    deadline = time.monotonic() + 12
    try:
        while time.monotonic() < deadline:
            pid_file = run_dir / "agent-workspace" / "child.pid"
            grandchild_file = run_dir / "agent-workspace" / "grandchild.pid"
            if pid_file.is_file() and grandchild_file.is_file():
                raw_pid = pid_file.read_text(encoding="utf-8").strip()
                if raw_pid:
                    child_pid = int(raw_pid)
                    grandchild_pid = int(grandchild_file.read_text(encoding="utf-8").strip())
                    break
            if process.poll() is not None:
                break
            time.sleep(0.05)
        assert child_pid is not None, "Agent process did not start"
        process.send_signal(signal.SIGTERM)
        stdout, stderr = process.communicate(timeout=12)
        assert process.returncode == 0, f"stdout={stdout}\nstderr={stderr}"

        result = json.loads((run_dir / "result.json").read_text(encoding="utf-8"))
        trace = [
            json.loads(line)
            for line in (run_dir / "trace.jsonl").read_text(encoding="utf-8").splitlines()
        ]
        assert result["status"] == "cancelled"
        assert any(event["type"] == "agent.cancelled" for event in trace)
        assert trace[-1]["type"] == "run.finished"
        with pytest.raises(ProcessLookupError):
            os.kill(child_pid, 0)
        with pytest.raises(ProcessLookupError):
            os.kill(grandchild_pid, 0)
    finally:
        if process.poll() is None:
            process.kill()
            process.communicate(timeout=5)
        if child_pid is not None:
            try:
                os.killpg(child_pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        if grandchild_pid is not None:
            try:
                os.kill(grandchild_pid, signal.SIGKILL)
            except ProcessLookupError:
                pass


@pytest.mark.skipif(sys.platform != "linux", reason="Linux parent-death signal required")
def test_sigkill_parent_terminates_agent_and_leaves_run_resumable(tmp_path: Path) -> None:
    root = Path.cwd()
    task_path = root / "benchmarks/examples/hello/task.yaml"
    agent_path = tmp_path / "kill-agent.yaml"
    agent_path.write_text(
        yaml.safe_dump(
            {
                "apiVersion": "harness/v1",
                "id": "sigkill-test-agent",
                "type": "cli",
                "command": [
                    sys.executable,
                    "-c",
                    (
                        "import os,pathlib,subprocess,sys,time;"
                        "pathlib.Path(os.environ['HARNESS_RUN_DIR'],'child.pid').write_text(str(os.getpid()));"
                        "p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)']);"
                        "pathlib.Path(os.environ['HARNESS_RUN_DIR'],'grandchild.pid').write_text(str(p.pid));"
                        "time.sleep(60)"
                    ),
                ],
            }
        ),
        encoding="utf-8",
    )
    runs_root = tmp_path / "runs"
    run_id = "run_sigkill_cleanup"
    child_script = (
        "from pathlib import Path;"
        "from harness.models import load_agent,load_task;"
        "from harness.orchestrator import Orchestrator;"
        "import sys;"
        "Orchestrator(sys.argv[1]).run(task=load_task(sys.argv[2]),task_path=Path(sys.argv[2]),"
        "agent=load_agent(sys.argv[3]),agent_path=Path(sys.argv[3]),allow_host_agent=True,"
        "run_id=sys.argv[4])"
    )
    env = os.environ.copy()
    env["PYTHONPATH"] = str(root / "src")
    process = subprocess.Popen(
        [sys.executable, "-c", child_script, str(runs_root), str(task_path), str(agent_path), run_id],
        cwd=root,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    run_dir = runs_root / run_id
    agent_pid: int | None = None
    grandchild_pid: int | None = None
    deadline = time.monotonic() + 12
    try:
        while time.monotonic() < deadline:
            pid_file = run_dir / "agent-workspace" / "child.pid"
            grandchild_file = run_dir / "agent-workspace" / "grandchild.pid"
            if pid_file.is_file() and grandchild_file.is_file():
                agent_pid = int(pid_file.read_text(encoding="utf-8").strip())
                grandchild_pid = int(grandchild_file.read_text(encoding="utf-8").strip())
                break
            if process.poll() is not None:
                break
            time.sleep(0.05)
        assert agent_pid is not None, "Agent process did not start"
        process.kill()
        process.communicate(timeout=5)
        assert process.returncode == -signal.SIGKILL

        for pid in (agent_pid, grandchild_pid):
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                try:
                    os.kill(pid, 0)
                except ProcessLookupError:
                    break
                time.sleep(0.05)
            else:
                pytest.fail(f"Agent descendant {pid} remained after its Harness parent was killed")

        assert (run_dir / "checkpoint.json").is_file()
        assert not (run_dir / "result.json").exists()
    finally:
        if process.poll() is None:
            process.kill()
            process.communicate(timeout=5)
        if agent_pid is not None:
            try:
                os.kill(agent_pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        if grandchild_pid is not None:
            try:
                os.kill(grandchild_pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
