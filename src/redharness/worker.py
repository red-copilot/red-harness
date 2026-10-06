from __future__ import annotations

import os
import socket
import threading
import time
from pathlib import Path
from typing import Any

import httpx

from .gateway_runtime import GatewayConfig
from .models import load_agent, load_task
from .orchestrator import Orchestrator
from .queue import JobPayload


class WorkerError(RuntimeError):
    pass


class _Heartbeat:
    def __init__(
        self,
        *,
        client: httpx.Client,
        job_id: str,
        worker_id: str,
        lease_seconds: int,
    ) -> None:
        self.client = client
        self.job_id = job_id
        self.worker_id = worker_id
        self.lease_seconds = lease_seconds
        self.interval = max(3.0, lease_seconds / 3)
        self.stop_event = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.error: str | None = None

    def _run(self) -> None:
        while not self.stop_event.wait(self.interval):
            try:
                response = self.client.post(
                    f"/v1/jobs/{self.job_id}/heartbeat",
                    json={
                        "worker_id": self.worker_id,
                        "lease_seconds": self.lease_seconds,
                    },
                )
                response.raise_for_status()
            except Exception as exc:  # noqa: BLE001 - heartbeat boundary records failures.
                self.error = str(exc)
                self.stop_event.set()
                return

    def __enter__(self) -> _Heartbeat:
        self.thread.start()
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.stop_event.set()
        self.thread.join(timeout=5)


class Worker:
    def __init__(
        self,
        *,
        control_url: str,
        token: str | None,
        worker_id: str | None = None,
        lease_seconds: int = 60,
        allow_host_jobs: bool = False,
        workspace_root: str | Path = ".",
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        self.worker_id = worker_id or f"{socket.gethostname()}-{os.getpid()}"
        self.lease_seconds = lease_seconds
        self.allow_host_jobs = allow_host_jobs
        self.workspace_root = Path(workspace_root).resolve()
        headers = {"authorization": f"Bearer {token}"} if token else {}
        self.client = httpx.Client(
            base_url=control_url.rstrip("/"),
            headers=headers,
            timeout=30.0,
            transport=transport,
        )

    def close(self) -> None:
        self.client.close()

    def _claim(self) -> dict[str, Any] | None:
        response = self.client.post(
            "/v1/jobs/claim",
            json={"worker_id": self.worker_id, "lease_seconds": self.lease_seconds},
        )
        response.raise_for_status()
        return response.json()

    def _path(self, raw: str) -> Path:
        path = Path(raw)
        return path.resolve() if path.is_absolute() else (self.workspace_root / path).resolve()

    def _gateway_config(self, payload: JobPayload) -> GatewayConfig | None:
        spec = payload.gateway
        if not spec.enabled:
            return None
        model_key = os.environ.get(spec.model_api_key_env)
        return GatewayConfig(
            policy_path=self._path(spec.policy_path) if spec.policy_path else None,
            model_upstream=spec.model_upstream,
            model_api_key=model_key,
            input_price_per_million_usd=spec.input_price_per_million_usd,
            output_price_per_million_usd=spec.output_price_per_million_usd,
            mode=spec.mode,
            sidecar_image=spec.sidecar_image,
            sidecar_runtime=spec.sidecar_runtime,
        )

    def _execute(self, payload: JobPayload) -> dict[str, Any]:
        if payload.allow_host_agent and not self.allow_host_jobs:
            raise WorkerError("job requested host agent execution but worker disallows host jobs")
        task_path = self._path(payload.task_path)
        agent_path = self._path(payload.agent_path)
        runs_root = self._path(payload.runs_root)
        return Orchestrator(runs_root).run(
            task=load_task(task_path),
            task_path=task_path,
            agent=load_agent(agent_path),
            agent_path=agent_path,
            allow_host_agent=payload.allow_host_agent and self.allow_host_jobs,
            seed=payload.seed,
            gateway_config=self._gateway_config(payload),
        )

    def run_once(self) -> bool:
        job = self._claim()
        if job is None:
            return False

        job_id = str(job["id"])
        payload = JobPayload.model_validate(job["payload"])
        try:
            with _Heartbeat(
                client=self.client,
                job_id=job_id,
                worker_id=self.worker_id,
                lease_seconds=self.lease_seconds,
            ) as heartbeat:
                result = self._execute(payload)
                if heartbeat.error:
                    raise WorkerError(f"lease heartbeat failed: {heartbeat.error}")
            response = self.client.post(
                f"/v1/jobs/{job_id}/complete",
                json={"worker_id": self.worker_id, "result": result},
            )
            response.raise_for_status()
            return True
        except Exception as exc:  # noqa: BLE001 - worker must report terminal job failure.
            response = self.client.post(
                f"/v1/jobs/{job_id}/fail",
                json={"worker_id": self.worker_id, "error": f"{type(exc).__name__}: {exc}"},
            )
            response.raise_for_status()
            return True

    def run_forever(self, *, poll_interval: float = 2.0) -> None:
        while True:
            if not self.run_once():
                time.sleep(poll_interval)
