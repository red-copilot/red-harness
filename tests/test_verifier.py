from __future__ import annotations

import json
import socket
import subprocess
from pathlib import Path

import pytest

from harness.models import VerificationSpec, load_task
from harness.trace import TraceRecorder
from harness.verifier import VerifierError, _parse_result, _run_python_verifier, run_verifier


def test_python_verifier_receives_only_explicit_harness_environment(
    monkeypatch, tmp_path: Path
) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    task_dir = task_path.resolve().parent
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    captured: dict = {}

    def fake_run(command, **kwargs):
        captured["command"] = command
        captured.update(kwargs)
        return subprocess.CompletedProcess(command, 0, stdout="{}\n", stderr="")

    monkeypatch.setenv("OPENAI_API_KEY", "must-not-reach-verifier")
    monkeypatch.setenv("HARNESS_CONTROL_TOKEN", "must-not-reach-verifier")
    monkeypatch.setattr("harness.verifier.subprocess.run", fake_run)

    _run_python_verifier(
        task,
        task_dir=task_dir,
        run_dir=run_dir,
        environment_project="local-project",
        seed=7,
    )

    assert "OPENAI_API_KEY" not in captured["env"]
    assert "HARNESS_CONTROL_TOKEN" not in captured["env"]
    assert captured["env"]["HARNESS_TASK_ID"] == task.id
    assert captured["env"]["HARNESS_TASK_DIR"] == "/task"
    assert captured["env"]["HARNESS_RUN_DIR"] == "/run/harness"
    assert captured["stdin"] == subprocess.DEVNULL
    assert captured["close_fds"] is True
    assert callable(captured["preexec_fn"])
    assert "--unshare-net" in captured["command"]
    assert "--ro-bind" in captured["command"]


def test_python_verifier_entrypoint_cannot_escape_task_directory(tmp_path: Path) -> None:
    task_dir = tmp_path / "task"
    task_dir.mkdir()
    outside = tmp_path / "outside.py"
    outside.write_text("print('{}')\n", encoding="utf-8")
    task = load_task(Path("benchmarks/examples/hello/task.yaml")).model_copy(
        update={"verification": VerificationSpec(entrypoint="../outside.py")}
    )

    with pytest.raises(VerifierError, match="inside the task directory"):
        _run_python_verifier(
            task,
            task_dir=task_dir,
            run_dir=tmp_path,
            environment_project=None,
            seed=1,
        )


def test_python_verifier_fails_closed_for_environment_networking(tmp_path: Path) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path).model_copy(
        update={"verification": VerificationSpec(network="environment")}
    )

    with pytest.raises(VerifierError, match="cannot receive environment networking"):
        run_verifier(
            task,
            task_dir=task_path.resolve().parent,
            run_dir=tmp_path,
            environment_project="project",
            environment_network="network",
            trace=TraceRecorder(tmp_path / "trace.jsonl", "run-1", task.id),
            seed=1,
        )


def test_python_verifier_fails_closed_when_bubblewrap_is_unavailable(
    monkeypatch, tmp_path: Path
) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    monkeypatch.setattr("harness.verifier.shutil.which", lambda _name: None)

    with pytest.raises(VerifierError, match="bubblewrap is required"):
        _run_python_verifier(
            task,
            task_dir=task_path.resolve().parent,
            run_dir=tmp_path,
            environment_project=None,
            seed=1,
        )


def test_python_verifier_timeout_is_reported_as_unavailable_verdict(
    monkeypatch, tmp_path: Path
) -> None:
    task_path = Path("benchmarks/examples/hello/task.yaml")
    task = load_task(task_path)
    monkeypatch.setattr("harness.verifier.shutil.which", lambda _name: "/usr/bin/bwrap")
    monkeypatch.setattr(
        "harness.verifier.subprocess.run",
        lambda command, **_kwargs: (_ for _ in ()).throw(
            subprocess.TimeoutExpired(command, timeout=1)
        ),
    )

    with pytest.raises(VerifierError, match="Python verifier timed out"):
        _run_python_verifier(
            task,
            task_dir=task_path.resolve().parent,
            run_dir=tmp_path,
            environment_project=None,
            seed=1,
        )


def test_verifier_rejects_inconsistent_output_contract() -> None:
    for output in ("", "not-json", '{"success": true}', '{"success": true, "score": NaN}'):
        with pytest.raises(VerifierError):
            _parse_result(output)


def test_python_verifier_cannot_reach_host_loopback(tmp_path: Path) -> None:
    task_dir = tmp_path / "task"
    task_dir.mkdir()
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    port = listener.getsockname()[1]
    (task_dir / "verifier.py").write_text(
        "import json, os, socket\n"
        "s = socket.socket()\n"
        "try:\n"
        f"    s.settimeout(1); s.connect(('127.0.0.1', {port})); connected = True\n"
        "except OSError:\n"
        "    connected = False\n"
        "try:\n"
        "    open(os.path.join(os.environ['HARNESS_RUN_DIR'], 'forbidden-write'), 'w').close(); writable = True\n"
        "except OSError:\n"
        "    writable = False\n"
        "success = not connected and not writable\n"
        "print(json.dumps({'success': success, 'score': 1 if success else 0, "
        "'message': 'isolated' if success else 'access escaped sandbox'}))\n",
        encoding="utf-8",
    )
    task = load_task(Path("benchmarks/examples/hello/task.yaml")).model_copy(
        update={"verification": VerificationSpec(entrypoint="verifier.py")}
    )

    try:
        result = _run_python_verifier(
            task,
            task_dir=task_dir,
            run_dir=run_dir,
            environment_project=None,
            seed=1,
        )
    finally:
        listener.close()

    assert result.returncode == 0
    assert json.loads(result.stdout)["success"] is True
