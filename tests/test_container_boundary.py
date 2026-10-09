from __future__ import annotations

import json
import shutil
import socket
import subprocess
from pathlib import Path

import pytest

from harness.models import AgentSpec, load_task
from harness.orchestrator import Orchestrator


@pytest.mark.skipif(shutil.which("docker") is None, reason="Docker is unavailable")
def test_docker_agent_cannot_reach_harness_state_secrets_or_host_network(
    monkeypatch, tmp_path: Path
) -> None:
    image = "harness-isolation-probe:ci"
    if subprocess.run(
        ["docker", "image", "inspect", image], capture_output=True, check=False
    ).returncode:
        pytest.skip(f"required test image is not built: {image}")

    import harness.orchestrator as orchestrator_module

    run_id = "run_isolation_probe"
    monkeypatch.setattr(orchestrator_module, "_new_run_id", lambda: run_id)
    monkeypatch.setenv("HARNESS_TEST_SECRET", "host-secret-canary")
    monkeypatch.setenv("HARNESS_CONTROL_TOKEN", "control-plane-secret-canary")
    docker_config = tmp_path / "docker-config"
    docker_config.mkdir()
    (docker_config / "config.json").write_text(
        json.dumps(
            {
                "proxies": {
                    "default": {
                        "httpProxy": "http://proxy-user:proxy-secret@127.0.0.1:3128",
                        "httpsProxy": "http://proxy-user:proxy-secret@127.0.0.1:3128",
                        "allProxy": "socks5://proxy-user:proxy-secret@127.0.0.1:1080",
                        "noProxy": "internal-secret.example",
                    }
                }
            }
        ),
        encoding="utf-8",
    )
    monkeypatch.setenv("DOCKER_CONFIG", str(docker_config))
    run_dir = tmp_path / "runs" / run_id
    protected_world_db = run_dir / "world.db"
    protected_result = run_dir / "result.json"

    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    port = listener.getsockname()[1]
    probe = (
        "import json, os, socket\n"
        "from pathlib import Path\n"
        f"world = Path({str(protected_world_db)!r})\n"
        f"result = Path({str(protected_result)!r})\n"
        "try: world.read_bytes(); can_read_world = True\n"
        "except OSError: can_read_world = False\n"
        "try: world.write_bytes(b'corrupted'); can_write_world = True\n"
        "except OSError: can_write_world = False\n"
        "try: result.write_bytes(b'forged'); can_write_result = True\n"
        "except OSError: can_write_result = False\n"
        "try: open('/etc/harness-escape', 'w').write('escaped'); root_writable = True\n"
        "except OSError: root_writable = False\n"
        "try:\n"
        f" s = socket.create_connection(('127.0.0.1', {port}), timeout=0.3); s.close(); network_open = True\n"
        "except OSError: network_open = False\n"
        "checks = [not can_read_world, not can_write_world, not can_write_result, "
        "not root_writable, not network_open, "
        "os.geteuid() != 0, 'HARNESS_TEST_SECRET' not in os.environ, "
        "'HARNESS_CONTROL_TOKEN' not in os.environ, "
        "not any(os.environ.get(name) for name in ('HTTP_PROXY','HTTPS_PROXY','NO_PROXY','FTP_PROXY','ALL_PROXY','http_proxy','https_proxy','no_proxy','ftp_proxy','all_proxy')), "
        "not any('proxy-secret' in value for name,value in os.environ.items() if 'proxy' in name.lower()), "
        "not Path('/task/verifier.py').exists()]\n"
        "Path(os.environ['HARNESS_RUN_DIR'], 'proof.txt').write_text("
        "'red-harness-ok' if all(checks) else json.dumps(checks), encoding='utf-8')\n"
    )
    try:
        task_path = Path("benchmarks/examples/hello/task.yaml")
        result = Orchestrator(tmp_path / "runs").run(
            task=load_task(task_path),
            task_path=task_path,
            agent=AgentSpec.model_validate(
                {
                    "apiVersion": "harness/v1",
                    "id": "isolation-probe",
                    "type": "docker",
                    "image": image,
                    "network": "none",
                    "network_profile": "fully-offline",
                    "command": ["python", "-c", probe],
                }
            ),
            agent_path=Path("agents/examples/isolation-probe/Dockerfile"),
            seed=1,
        )
    finally:
        listener.close()

    assert result["success"] is True
    assert (run_dir / "world.db").is_file()
    assert (run_dir / "result.json").is_file()
    assert (run_dir / "agent-workspace" / "proof.txt").read_text(
        encoding="utf-8"
    ) == "red-harness-ok"
