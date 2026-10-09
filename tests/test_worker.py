import json
import os
import shutil
import signal
import sqlite3
import subprocess
import sys
import time
from pathlib import Path

import httpx
import pytest
import yaml

from harness.queue import JobPayload, SQLiteQueue
from harness.worker import Worker, WorkerError, _Heartbeat


def test_worker_requires_control_plane_authentication(tmp_path) -> None:
    with pytest.raises(WorkerError, match="requires a configured control-plane token"):
        Worker(control_url="https://control.invalid", token=None, workspace_root=tmp_path)


def test_worker_requires_tls_for_non_loopback_control_plane(tmp_path: Path) -> None:
    with pytest.raises(WorkerError, match="HTTPS unless the control plane is on loopback"):
        Worker(
            control_url="http://control.invalid",
            token="worker-token",
            workspace_root=tmp_path,
        )

    worker = Worker(
        control_url="https://control.invalid",
        token="worker-token",
        workspace_root=tmp_path,
        transport=httpx.MockTransport(lambda request: httpx.Response(204)),
    )
    assert worker.client._trust_env is False
    worker.close()


def test_heartbeat_shutdown_waits_for_inflight_request_without_late_cancel() -> None:
    import threading

    request_started = threading.Event()
    release_request = threading.Event()
    late_cancellations: list[bool] = []

    def handle(request: httpx.Request) -> httpx.Response:
        request_started.set()
        assert release_request.wait(timeout=2)
        return httpx.Response(409, request=request)

    client = httpx.Client(
        base_url="https://control.invalid",
        transport=httpx.MockTransport(handle),
    )
    heartbeat = _Heartbeat(
        client=client,
        job_id="job-shutdown",
        worker_id="worker-shutdown",
        lease_seconds=10,
        on_error=lambda: late_cancellations.append(True),
    )
    heartbeat.interval = 0.01
    heartbeat.__enter__()
    assert request_started.wait(timeout=1)
    release_timer = threading.Timer(0.05, release_request.set)
    release_timer.start()
    heartbeat.__exit__(None, None, None)
    release_timer.join(timeout=1)
    client.close()

    assert not heartbeat.thread.is_alive()
    assert heartbeat.error == "HTTPStatusError"
    assert late_cancellations == []


@pytest.mark.parametrize("url", ["http://127.0.0.1:8780", "http://[::1]:8780", "http://localhost:8780"])
def test_worker_allows_loopback_control_plane_over_http(tmp_path: Path, url: str) -> None:
    worker = Worker(
        control_url=url,
        token="worker-token",
        workspace_root=tmp_path,
        transport=httpx.MockTransport(lambda request: httpx.Response(204)),
    )
    worker.close()


def test_worker_does_not_persist_exception_details_to_control_plane(tmp_path: Path) -> None:
    payload = JobPayload(task_path="task.yaml", agent_path="agent.yaml")
    reported: dict = {}

    def handle(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/jobs/claim":
            return httpx.Response(
                200,
                json={
                    "id": "job_redaction",
                    "run_id": "run_redaction",
                    "state": "running",
                    "payload": payload.model_dump(),
                },
            )
        if request.url.path == "/v1/jobs/job_redaction/fail":
            reported.update(json.loads(request.content))
            return httpx.Response(200, json={"ok": True})
        if request.url.path.endswith("/heartbeat"):
            return httpx.Response(200, json={"ok": True})
        pytest.fail(f"unexpected control-plane request: {request.url.path}")

    worker = Worker(
        control_url="https://control.invalid",
        token="worker-token",
        workspace_root=tmp_path,
        lease_seconds=10,
        transport=httpx.MockTransport(handle),
    )

    def fail_execution(*_args, **_kwargs):
        raise RuntimeError("api_key=private-worker-canary")

    worker._execute = fail_execution
    assert worker.run_once() is True
    worker.close()

    assert reported["error"] == "RuntimeError (details omitted)"
    assert "private-worker-canary" not in json.dumps(reported)


def test_worker_rejects_job_paths_outside_workspace(tmp_path: Path) -> None:
    outside = tmp_path.parent / f"{tmp_path.name}-outside.yaml"
    outside.write_text("sentinel", encoding="utf-8")
    (tmp_path / "linked.yaml").symlink_to(outside)
    worker = Worker(
        control_url="https://control.invalid",
        token="worker-token",
        workspace_root=tmp_path,
    )

    with pytest.raises(WorkerError, match="within the worker workspace"):
        worker._path("../outside.yaml")
    with pytest.raises(WorkerError, match="within the worker workspace"):
        worker._path(str(outside))
    with pytest.raises(WorkerError, match="within the worker workspace"):
        worker._path("linked.yaml")
    worker.close()


@pytest.mark.skipif(os.name != "posix", reason="POSIX signal delivery is required")
def test_worker_terminates_agent_after_heartbeat_lease_loss(tmp_path: Path) -> None:
    task_path = tmp_path / "task.yaml"
    shutil.copyfile("benchmarks/examples/hello/task.yaml", task_path)
    pid_file = tmp_path / "agent.pid"
    agent_path = tmp_path / "agent.yaml"
    agent_path.write_text(
        yaml.safe_dump(
            {
                "apiVersion": "harness/v1",
                "id": "lease-loss-agent",
                "type": "cli",
                "command": [
                    sys.executable,
                    "-c",
                    (
                        "import os,pathlib,time;"
                        f"pathlib.Path({str(pid_file)!r}).write_text(str(os.getpid()));"
                        "time.sleep(60)"
                    ),
                ],
            }
        ),
        encoding="utf-8",
    )
    payload = JobPayload(
        task_path=str(task_path),
        agent_path=str(agent_path),
        runs_root="runs",
        allow_host_agent=True,
    )
    calls: list[str] = []

    def handle(request: httpx.Request) -> httpx.Response:
        calls.append(request.url.path)
        if request.url.path == "/v1/jobs/claim":
            return httpx.Response(
                200,
                json={
                    "id": "job_lease_lost",
                    "run_id": "run_lease_lost",
                    "state": "running",
                    "payload": payload.model_dump(),
                },
            )
        if request.url.path.endswith("/heartbeat"):
            return httpx.Response(409, json={"detail": "lease expired"})
        if request.url.path.endswith("/fail"):
            return httpx.Response(409, json={"detail": "lease expired"})
        pytest.fail(f"unexpected control-plane request: {request.url.path}")

    worker = Worker(
        control_url="https://control.invalid",
        token="worker-token",
        worker_id="worker-lease-lost",
        lease_seconds=10,
        allow_host_jobs=True,
        workspace_root=tmp_path,
        transport=httpx.MockTransport(handle),
    )

    started = time.monotonic()
    assert worker.run_once() is True
    elapsed = time.monotonic() - started
    worker.close()

    run_dir = tmp_path / "runs" / "run_lease_lost"
    result = json.loads((run_dir / "result.json").read_text(encoding="utf-8"))
    agent_pid = int(pid_file.read_text(encoding="utf-8"))
    assert result["status"] == "cancelled"
    assert elapsed < 10
    assert "/v1/jobs/job_lease_lost/complete" not in calls
    assert "/v1/jobs/job_lease_lost/fail" in calls
    with pytest.raises(ProcessLookupError):
        os.kill(agent_pid, 0)


@pytest.mark.skipif(shutil.which("docker") is None, reason="Docker is unavailable")
def test_worker_terminates_pi_container_after_heartbeat_lease_loss(tmp_path: Path) -> None:
    image = "harness-pi-rpc-resume-probe:ci"
    if subprocess.run(
        ["docker", "image", "inspect", image], capture_output=True, check=False
    ).returncode:
        pytest.skip(f"required test image is not built: {image}")

    workspace = tmp_path / "workspace"
    workspace.mkdir()
    task_dir = workspace / "hello"
    shutil.copytree("benchmarks/examples/hello", task_dir)
    agent_path = workspace / "pi-agent.yaml"
    agent_path.write_text(
        "apiVersion: harness/v1\n"
        "id: lease-loss-pi\n"
        "type: pi\n"
        f"image: {image}\n"
        "network: none\n"
        "env:\n"
        "  PI_PROBE_HANG: '1'\n"
        "pi:\n"
        "  binary: /usr/local/bin/fake-pi\n"
        "  provider: fake\n"
        "  model: probe\n"
        "  offline: true\n",
        encoding="utf-8",
    )
    payload = JobPayload(
        task_path=str(task_dir / "task.yaml"),
        agent_path=str(agent_path),
        runs_root="runs",
    )
    calls: list[str] = []

    def handle(request: httpx.Request) -> httpx.Response:
        calls.append(request.url.path)
        if request.url.path == "/v1/jobs/claim":
            return httpx.Response(
                200,
                json={
                    "id": "job_pi_lease_lost",
                    "run_id": "run_pi_lease_lost",
                    "state": "running",
                    "payload": payload.model_dump(),
                },
            )
        if request.url.path.endswith("/heartbeat"):
            return httpx.Response(409, json={"detail": "lease expired"})
        if request.url.path.endswith("/fail"):
            return httpx.Response(409, json={"detail": "lease expired"})
        pytest.fail(f"unexpected control-plane request: {request.url.path}")

    worker = Worker(
        control_url="https://control.invalid",
        token="worker-token",
        worker_id="worker-pi-lease-lost",
        lease_seconds=10,
        workspace_root=workspace,
        transport=httpx.MockTransport(handle),
    )

    assert worker.run_once() is True
    worker.close()

    run_dir = workspace / "runs" / "run_pi_lease_lost"
    result = json.loads((run_dir / "result.json").read_text(encoding="utf-8"))
    checkpoint = json.loads((run_dir / "checkpoint.json").read_text(encoding="utf-8"))
    container_name = "harness_pi_run_pi_lease_lost"
    container = subprocess.run(
        ["docker", "inspect", container_name], capture_output=True, check=False
    )
    assert result["status"] == "cancelled"
    assert checkpoint["pending_actions"] == [
        {"tool": "write", "tool_call_id": "probe-call", "status": "running", "skill_id": None}
    ]
    assert container.returncode != 0
    assert "/v1/jobs/job_pi_lease_lost/complete" not in calls
    assert "/v1/jobs/job_pi_lease_lost/fail" in calls


def test_worker_reuses_terminal_result_for_stable_job_run_id(tmp_path: Path) -> None:
    task_path = tmp_path / "task.yaml"
    agent_path = tmp_path / "agent.yaml"
    shutil.copyfile("benchmarks/examples/hello/task.yaml", task_path)
    shutil.copyfile("agents/examples/demo.yaml", agent_path)
    run_id = "run_job_stable"
    run_dir = tmp_path / "runs" / run_id
    run_dir.mkdir(parents=True)
    result = {"run_id": run_id, "success": True, "score": 100}
    (run_dir / "result.json").write_text(json.dumps(result), encoding="utf-8")
    worker = Worker(
        control_url="https://control.invalid",
        token="worker-token",
        workspace_root=tmp_path,
    )

    actual = worker._execute(
        JobPayload(
            task_path="task.yaml",
            agent_path="agent.yaml",
            runs_root="runs",
        ),
        run_id=run_id,
    )

    assert actual == result
    worker.close()


def test_worker_refuses_to_repeat_interrupted_non_pi_job(tmp_path: Path) -> None:
    task_path = tmp_path / "task.yaml"
    agent_path = tmp_path / "agent.yaml"
    shutil.copyfile("benchmarks/examples/hello/task.yaml", task_path)
    shutil.copyfile("agents/examples/demo.yaml", agent_path)
    (tmp_path / "runs" / "run_job_interrupted").mkdir(parents=True)
    worker = Worker(
        control_url="https://control.invalid",
        token="worker-token",
        workspace_root=tmp_path,
    )

    with pytest.raises(WorkerError, match="manual reconciliation"):
        worker._execute(
            JobPayload(
                task_path="task.yaml",
                agent_path="agent.yaml",
                runs_root="runs",
            ),
            run_id="run_job_interrupted",
        )
    worker.close()


def test_worker_resumes_interrupted_pi_rpc_run(monkeypatch, tmp_path: Path) -> None:
    import harness.worker as worker_module

    task_path = tmp_path / "task.yaml"
    shutil.copyfile("benchmarks/examples/hello/task.yaml", task_path)
    agent_path = tmp_path / "pi-agent.yaml"
    agent_path.write_text(
        "apiVersion: harness/v1\n"
        "id: interrupted-pi\n"
        "type: pi\n"
        "image: harness-pi-rpc-resume-probe:ci\n"
        "network: none\n"
        "pi:\n"
        "  binary: /usr/local/bin/fake-pi\n"
        "  provider: fake\n"
        "  model: probe\n"
        "  mode: rpc\n",
        encoding="utf-8",
    )
    runs_root = tmp_path / "runs"
    run_id = "run_interrupted_pi"
    run_dir = runs_root / run_id
    run_dir.mkdir(parents=True)
    calls: dict[str, object] = {}

    class ResumeOrchestrator:
        def __init__(self, root: Path) -> None:
            calls["runs_root"] = root

        def run(self, **kwargs: object) -> dict[str, object]:
            calls.update(kwargs)
            return {"run_id": run_id, "success": True, "score": 100}

    monkeypatch.setattr(worker_module, "Orchestrator", ResumeOrchestrator)
    worker = Worker(
        control_url="https://control.invalid",
        token="worker-token",
        workspace_root=tmp_path,
    )

    result = worker._execute(
        JobPayload(
            task_path=str(task_path),
            agent_path=str(agent_path),
            runs_root=str(runs_root),
        ),
        run_id=run_id,
    )

    worker.close()
    assert result["run_id"] == run_id
    assert calls["runs_root"] == runs_root
    assert calls["resume_run_dir"] == run_dir
    assert "run_id" not in calls


@pytest.mark.skipif(os.name != "posix", reason="POSIX process signals are required")
def test_killed_worker_requires_reconciliation_before_job_restart(tmp_path: Path) -> None:
    agent_pid_file = tmp_path / "agent.pid"
    external_effects = tmp_path / "external-effects.jsonl"
    worker_job_file = tmp_path / "claimed-job.json"
    agent_path = tmp_path / "long-agent.yaml"
    task_path = tmp_path / "task.yaml"
    shutil.copyfile("benchmarks/examples/hello/task.yaml", task_path)
    agent_path.write_text(
        yaml.safe_dump(
            {
                "apiVersion": "harness/v1",
                "id": "worker-kill-agent",
                "type": "cli",
                "command": [
                    sys.executable,
                    "-c",
                    (
                        "import os,pathlib,time;"
                        f"pathlib.Path({str(agent_pid_file)!r}).write_text(str(os.getpid()));"
                        f"open({str(external_effects)!r},'a').write('effect\\n');"
                        "time.sleep(60)"
                    ),
                ],
            }
        ),
        encoding="utf-8",
    )
    payload = JobPayload(
        task_path=str(task_path),
        agent_path=str(agent_path),
        runs_root=str(tmp_path / "runs"),
        allow_host_agent=True,
    )
    payload_path = tmp_path / "payload.json"
    payload_path.write_text(payload.model_dump_json(), encoding="utf-8")
    queue_path = tmp_path / "queue.db"
    child_script = tmp_path / "worker_child.py"
    child_script.write_text(
        "import json,sys\n"
        "from pathlib import Path\n"
        "from harness.queue import JobPayload,SQLiteQueue\n"
        "from harness.worker import Worker\n"
        "queue_path,payload_path,root,job_file=sys.argv[1:]\n"
        "queue=SQLiteQueue(queue_path)\n"
        "payload=JobPayload.model_validate_json(Path(payload_path).read_text())\n"
        "job=queue.submit(payload)\n"
        "claimed=queue.claim('worker-before-kill',lease_seconds=10)\n"
        "Path(job_file).write_text(json.dumps({'id':job['id'],'run_id':claimed['run_id']}))\n"
        "worker=Worker(control_url='https://control.invalid',token='test-token',"
        "allow_host_jobs=True,workspace_root=root)\n"
        "worker._execute(payload,run_id=claimed['run_id'])\n",
        encoding="utf-8",
    )
    env = os.environ.copy()
    env["PYTHONPATH"] = str(Path.cwd() / "src")
    process = subprocess.Popen(
        [sys.executable, str(child_script), str(queue_path), str(payload_path),
         str(tmp_path), str(worker_job_file)],
        cwd=Path.cwd(),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    agent_pid: int | None = None
    try:
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if worker_job_file.is_file() and agent_pid_file.is_file():
                agent_pid = int(agent_pid_file.read_text(encoding="utf-8"))
                break
            if process.poll() is not None:
                stdout, stderr = process.communicate(timeout=2)
                pytest.fail(f"worker exited early: stdout={stdout} stderr={stderr}")
            time.sleep(0.05)
        assert agent_pid is not None, "worker did not start the long-running Agent"
        claimed = json.loads(worker_job_file.read_text(encoding="utf-8"))

        os.kill(process.pid, signal.SIGKILL)
        process.communicate(timeout=5)
        assert process.returncode == -signal.SIGKILL

        with sqlite3.connect(queue_path) as database:
            database.execute(
                "UPDATE jobs SET lease_expires_at=? WHERE id=?",
                (time.time() - 1, claimed["id"]),
            )
        queue = SQLiteQueue(queue_path)
        assert queue.claim("worker-after-kill", lease_seconds=10) is None
        assert queue.get(claimed["id"])["state"] == "reconciliation_required"

        worker = Worker(
            control_url="https://control.invalid",
            token="test-token",
            allow_host_jobs=True,
            workspace_root=tmp_path,
        )
        with pytest.raises(WorkerError, match="manual reconciliation"):
            worker._execute(payload, run_id=claimed["run_id"])
        worker.close()
        assert external_effects.read_text(encoding="utf-8").splitlines() == ["effect"]
    finally:
        if process.poll() is None:
            process.kill()
            process.communicate(timeout=5)
        if agent_pid is not None:
            try:
                os.kill(agent_pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
