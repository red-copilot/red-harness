from __future__ import annotations

import secrets
import shutil
import socket
import subprocess
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

import uvicorn

from .gateway import ModelPricing, create_gateway_app
from .policy import load_policy
from .trace import TraceRecorder


class GatewayRuntimeError(RuntimeError):
    pass


@dataclass(frozen=True)
class GatewayConfig:
    policy_path: Path | None = None
    model_upstream: str | None = None
    model_api_key: str | None = None
    input_price_per_million_usd: float = 0.0
    output_price_per_million_usd: float = 0.0
    mode: Literal["host", "sidecar"] = "host"
    sidecar_image: str | None = None
    sidecar_runtime: str | None = None


class HostGatewayRuntime:
    mode = "host"
    network_name: str | None = None

    def __init__(
        self,
        *,
        config: GatewayConfig,
        run_dir: Path,
        task_dir: Path,
        trace: TraceRecorder,
        expose_to_docker: bool = False,
    ) -> None:
        self.config = config
        self.run_dir = run_dir
        self.task_dir = task_dir
        self.trace = trace
        self.expose_to_docker = expose_to_docker
        self.token = secrets.token_urlsafe(32)
        self.listen_host = "0.0.0.0" if expose_to_docker else "127.0.0.1"
        self.port = self._pick_port(self.listen_host)
        client_host = "host.docker.internal" if expose_to_docker else "127.0.0.1"
        self.url = f"http://{client_host}:{self.port}"

        app = create_gateway_app(
            event_file=run_dir / "events.jsonl",
            workspace=run_dir,
            task_dir=task_dir,
            policy=load_policy(config.policy_path),
            gateway_token=self.token,
            model_upstream=config.model_upstream,
            model_api_key=config.model_api_key,
            model_pricing=ModelPricing(
                input_per_million_usd=config.input_price_per_million_usd,
                output_per_million_usd=config.output_price_per_million_usd,
            ),
        )
        uvicorn_config = uvicorn.Config(
            app,
            host=self.listen_host,
            port=self.port,
            log_level="warning",
            access_log=False,
        )
        self.server = uvicorn.Server(uvicorn_config)
        self.thread = threading.Thread(
            target=self.server.run,
            name=f"redharness-gateway-{self.port}",
            daemon=True,
        )

    @staticmethod
    def _pick_port(host: str) -> int:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.bind((host, 0))
            return int(sock.getsockname()[1])

    def start(self) -> None:
        self.thread.start()
        deadline = time.monotonic() + 5
        while not self.server.started:
            if not self.thread.is_alive():
                raise GatewayRuntimeError("gateway server exited during startup")
            if time.monotonic() >= deadline:
                raise GatewayRuntimeError("gateway server startup timed out")
            time.sleep(0.02)
        self.trace.emit(
            "gateway.started",
            data={
                "mode": self.mode,
                "url": self.url,
                "listen_host": self.listen_host,
                "docker_access": self.expose_to_docker,
                "model_proxy": self.config.model_upstream is not None,
                "policy": str(self.config.policy_path) if self.config.policy_path else "default",
            },
        )

    def stop(self) -> None:
        if not self.thread.is_alive():
            return
        self.server.should_exit = True
        self.thread.join(timeout=5)
        if self.thread.is_alive():
            raise GatewayRuntimeError("gateway server did not stop cleanly")
        self.trace.emit("gateway.finished", data={"mode": self.mode, "url": self.url})


class DockerGatewaySidecarRuntime:
    mode = "sidecar"

    def __init__(
        self,
        *,
        config: GatewayConfig,
        run_dir: Path,
        task_dir: Path,
        run_id: str,
        trace: TraceRecorder,
    ) -> None:
        if shutil.which("docker") is None:
            raise GatewayRuntimeError("docker executable was not found")
        if not config.sidecar_image:
            raise GatewayRuntimeError("sidecar mode requires gateway sidecar image")

        safe_id = run_id.lower().replace("-", "_")
        self.config = config
        self.run_dir = run_dir
        self.task_dir = task_dir
        self.trace = trace
        self.token = secrets.token_urlsafe(32)
        self.container_name = f"rh_gateway_{safe_id}"[:63]
        self.network_name = f"rh_gateway_net_{safe_id}"[:63]
        self.url = "http://redharness-gateway:8765"

    @staticmethod
    def _run(command: list[str], *, check: bool = True) -> subprocess.CompletedProcess[str]:
        result = subprocess.run(
            command,
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        if check and result.returncode != 0:
            raise GatewayRuntimeError(
                f"{' '.join(command[:3])} failed: {result.stderr.strip()}"
            )
        return result

    def _container_command(self) -> list[str]:
        command = [
            "docker",
            "run",
            "-d",
            "--rm",
            "--pull=never",
            "--name",
            self.container_name,
            "--network",
            self.network_name,
            "--network-alias",
            "redharness-gateway",
            "--read-only",
            "--cap-drop",
            "ALL",
            "--security-opt",
            "no-new-privileges",
            "--pids-limit",
            "128",
            "--memory",
            "512m",
            "--cpus",
            "1",
            "--tmpfs",
            "/tmp:rw,nosuid,nodev,size=128m",
            "-v",
            f"{self.task_dir.resolve()}:/task:ro",
            "-v",
            f"{self.run_dir.resolve()}:/run/redharness:rw",
            "-e",
            f"REDHARNESS_GATEWAY_TOKEN={self.token}",
        ]
        if self.config.sidecar_runtime:
            command.extend(["--runtime", self.config.sidecar_runtime])
        if self.config.model_api_key:
            command.extend(
                ["-e", f"REDHARNESS_MODEL_API_KEY={self.config.model_api_key}"]
            )

        policy_args: list[str] = []
        if self.config.policy_path:
            command.extend(
                [
                    "-v",
                    f"{self.config.policy_path.resolve()}:/config/policy.yaml:ro",
                ]
            )
            policy_args = ["--policy", "/config/policy.yaml"]

        command.extend(
            [
                str(self.config.sidecar_image),
                "--host",
                "0.0.0.0",
                "--port",
                "8765",
                "--event-file",
                "/run/redharness/events.jsonl",
                "--workspace",
                "/run/redharness",
                "--task-dir",
                "/task",
                *policy_args,
            ]
        )
        if self.config.model_upstream:
            command.extend(["--model-upstream", self.config.model_upstream])
        if self.config.input_price_per_million_usd:
            command.extend(
                [
                    "--input-price-per-million",
                    str(self.config.input_price_per_million_usd),
                ]
            )
        if self.config.output_price_per_million_usd:
            command.extend(
                [
                    "--output-price-per-million",
                    str(self.config.output_price_per_million_usd),
                ]
            )
        return command

    def start(self) -> None:
        self._run(["docker", "network", "create", "--internal", self.network_name])
        try:
            self._run(self._container_command())
            if self.config.model_upstream:
                self._run(["docker", "network", "connect", "bridge", self.container_name])
            deadline = time.monotonic() + 10
            health_command = [
                "docker",
                "exec",
                self.container_name,
                "python",
                "-c",
                (
                    "import urllib.request;"
                    "urllib.request.urlopen("
                    "'http://127.0.0.1:8765/health',timeout=1).read()"
                ),
            ]
            while time.monotonic() < deadline:
                health = self._run(health_command, check=False)
                if health.returncode == 0:
                    break
                inspect = self._run(
                    ["docker", "inspect", self.container_name],
                    check=False,
                )
                if inspect.returncode != 0:
                    raise GatewayRuntimeError("gateway sidecar exited during startup")
                time.sleep(0.1)
            else:
                raise GatewayRuntimeError("gateway sidecar health check timed out")
        except Exception:
            self.stop()
            raise

        self.trace.emit(
            "gateway.started",
            data={
                "mode": self.mode,
                "url": self.url,
                "network": self.network_name,
                "container": self.container_name,
                "runtime": self.config.sidecar_runtime,
                "model_proxy": self.config.model_upstream is not None,
                "policy": str(self.config.policy_path) if self.config.policy_path else "default",
            },
        )

    def stop(self) -> None:
        self._run(
            ["docker", "rm", "-f", self.container_name],
            check=False,
        )
        self._run(
            ["docker", "network", "rm", self.network_name],
            check=False,
        )
        self.trace.emit(
            "gateway.finished",
            data={"mode": self.mode, "network": self.network_name},
        )


GatewayRuntime = HostGatewayRuntime | DockerGatewaySidecarRuntime


def build_gateway_runtime(
    *,
    config: GatewayConfig,
    run_dir: Path,
    task_dir: Path,
    run_id: str,
    trace: TraceRecorder,
    agent_type: str,
) -> GatewayRuntime:
    if config.mode == "host":
        return HostGatewayRuntime(
            config=config,
            run_dir=run_dir,
            task_dir=task_dir,
            trace=trace,
            expose_to_docker=agent_type == "docker",
        )
    if config.mode == "sidecar":
        if agent_type != "docker":
            raise GatewayRuntimeError("sidecar Gateway currently requires a Docker Agent")
        return DockerGatewaySidecarRuntime(
            config=config,
            run_dir=run_dir,
            task_dir=task_dir,
            run_id=run_id,
            trace=trace,
        )
    raise GatewayRuntimeError(f"unsupported gateway mode: {config.mode}")
