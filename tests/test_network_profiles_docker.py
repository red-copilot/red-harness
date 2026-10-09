from __future__ import annotations

import shutil
import subprocess
import uuid

import pytest


@pytest.mark.skipif(shutil.which("docker") is None, reason="Docker is unavailable")
def test_internal_target_network_blocks_public_egress() -> None:
    image = "harness-isolation-probe:ci"
    if subprocess.run(
        ["docker", "image", "inspect", image], capture_output=True, check=False
    ).returncode:
        pytest.skip(f"required test image is not built: {image}")

    network = f"harness_internal_probe_{uuid.uuid4().hex[:12]}"
    target = f"harness_authorized_target_{uuid.uuid4().hex[:12]}"
    try:
        subprocess.run(
            ["docker", "network", "create", "--internal", network],
            capture_output=True,
            text=True,
            timeout=30,
            check=True,
        )
        subprocess.run(
            [
                "docker",
                "run",
                "-d",
                "--rm",
                "--name",
                target,
                "--network",
                network,
                "--network-alias",
                "authorized-target",
                image,
                "python",
                "-c",
                (
                    "import socket;"
                    "s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);"
                    "s.bind(('0.0.0.0',8765));s.listen();"
                    "conn,_=s.accept();conn.close()"
                ),
            ],
            capture_output=True,
            text=True,
            timeout=30,
            check=True,
        )
        probe = subprocess.run(
            [
                "docker",
                "run",
                "--rm",
                "--network",
                network,
                image,
                "python",
                "-c",
                (
                    "import socket\n"
                    "target=socket.create_connection(('authorized-target',8765),timeout=3)\n"
                    "target.close()\n"
                    "try:\n"
                    " socket.create_connection(('1.1.1.1',443),timeout=2)\n"
                    " raise SystemExit(3)\n"
                    "except OSError:\n"
                    " pass\n"
                ),
            ],
            capture_output=True,
            text=True,
            timeout=15,
            check=False,
        )
        assert probe.returncode == 0, (
            "internal target network must reach the authorized target and block public egress; "
            f"probe exited {probe.returncode}: {probe.stderr}"
        )
    finally:
        subprocess.run(
            ["docker", "rm", "-f", target],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        subprocess.run(
            ["docker", "network", "rm", network],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
