# ruff: noqa: I001
from __future__ import annotations

import asyncio
import json
import subprocess
import time
import uuid
from pathlib import Path
from typing import Any

from .agent import AgentError, AgentResult
from .budget import BudgetMonitor
from .runtime_paths import runtime_event_path
from .session import AgentEvent, AgentObservation, OneShotAgentSession


class ContainerPiRpcSession(OneShotAgentSession):
    """Long-lived Pi RPC session with real bidirectional Harness observations."""

    def __init__(
        self,
        adapter: Any,
        *,
        run_kwargs: dict[str, Any],
        run_dir: Path,
        process: subprocess.Popen[bytes],
        container_name: str,
        stderr_handle: Any,
        gateway_enabled: bool,
    ) -> None:
        super().__init__(adapter, run_kwargs=run_kwargs, run_dir=run_dir)
        self.process = process
        self.container_name = container_name
        self.stderr_handle = stderr_handle
        self.gateway_enabled = gateway_enabled
        self.runtime_event_path = runtime_event_path(run_dir)
        self.raw_path = run_dir / "agent.stdout.log"
        self.stderr_path = run_dir / "agent.stderr.log"
        self.monitor = BudgetMonitor(
            self.runtime_event_path,
            run_kwargs["task"].budgets,
            adapter.trace,
        )
        self.started = time.monotonic()
        self.timed_out = False
        self._rpc_lock = asyncio.Lock()
        self._settled = asyncio.Event()
        self._reader_task: asyncio.Task[None] | None = None
        self._result_cache: AgentResult | None = None
        self._fatal_error: str | None = None
        self._initial_request_id = f"initial_{uuid.uuid4().hex}"

    @classmethod
    async def start(
        cls,
        adapter: Any,
        *,
        run_kwargs: dict[str, Any],
        run_dir: Path,
    ) -> ContainerPiRpcSession:
        task = run_kwargs["task"]
        task_dir = run_kwargs["task_dir"]
        environment_project = run_kwargs.get("environment_project")
        environment_network = run_kwargs.get("environment_network")
        seed = run_kwargs["seed"]
        gateway_url = run_kwargs.get("gateway_url")
        gateway_token = run_kwargs.get("gateway_token")
        gateway_network = run_kwargs.get("gateway_network")
        gateway_enabled = bool(gateway_url and gateway_token)

        if gateway_enabled and (
            adapter.spec.network == "none"
            or adapter.spec.network_profile == "offline"
        ):
            raise AgentError("Pi offline networking is incompatible with per-run Gateway")

        if adapter.spec.network == "host" and gateway_url and gateway_network is None:
            gateway_url = gateway_url.replace(
                "host.docker.internal",
                "127.0.0.1",
            )
            run_kwargs = {**run_kwargs, "gateway_url": gateway_url}

        event_file = runtime_event_path(run_dir)
        event_file.write_text("", encoding="utf-8")
        (run_dir / "agent.events.jsonl").touch(exist_ok=True)
        raw_path = run_dir / "agent.stdout.log"
        stderr_path = run_dir / "agent.stderr.log"
        raw_path.write_text("", encoding="utf-8")
        stderr_path.write_text("", encoding="utf-8")
        adapter._prepare_agent_dir(
            run_dir=run_dir,
            gateway_url=gateway_url,
            gateway_token=gateway_token,
        )
        (run_dir / "pi-sessions").mkdir(parents=True, exist_ok=True)
        (run_dir / "home").mkdir(parents=True, exist_ok=True)

        container_name = (
            "harness_pi_" + run_dir.name.lower()
        ).replace("-", "_")[:63]
        sidecar_gateway = gateway_enabled and gateway_network is not None
        host_gateway = (
            gateway_enabled
            and not sidecar_gateway
            and adapter.spec.network != "host"
        )
        if adapter.spec.network_profile == "offline":
            primary_network = "none"
        elif sidecar_gateway:
            if adapter.spec.network == "host":
                raise AgentError(
                    "Pi network:host is incompatible with sidecar Gateway mode"
                )
            primary_network = str(gateway_network)
        elif adapter.spec.network == "host":
            primary_network = "host"
        elif adapter.spec.network == "environment":
            primary_network = environment_network or "bridge"
        else:
            primary_network = "none"

        create_command = adapter._container_command(
            task,
            task_dir=task_dir,
            run_dir=run_dir,
            container_name=container_name,
            network=primary_network,
            environment_project=environment_project,
            seed=seed,
            gateway_url=gateway_url,
            gateway_token=gateway_token,
            host_gateway=host_gateway,
            rpc=True,
        )

        adapter.trace.emit(
            "agent.started",
            actor="agent",
            data={
                "agent_id": adapter.spec.id,
                "type": "pi",
                "execution": "docker",
                "pi_protocol": "rpc",
                "image": adapter.spec.image,
                "network": primary_network,
                "target_network": environment_network,
                "runtime": adapter.spec.runtime,
                "cap_add": adapter.pi.cap_add,
                "model": adapter.pi.model,
                "provider": "harness" if gateway_enabled else adapter.pi.provider,
                "thinking": adapter.pi.thinking,
                "tools": adapter.pi.tools,
                "gateway_injected": gateway_enabled,
                "gateway_network": gateway_network,
                "network_profile": adapter.spec.network_profile,
            },
        )

        stderr_handle = None
        try:
            adapter._docker(create_command, cwd=task_dir)
            if (
                sidecar_gateway
                and environment_network
                and environment_network != gateway_network
            ):
                adapter._docker(
                    [
                        "docker",
                        "network",
                        "connect",
                        environment_network,
                        container_name,
                    ],
                    cwd=task_dir,
                )

            stderr_handle = stderr_path.open("wb")
            process = subprocess.Popen(
                ["docker", "start", "-a", "-i", container_name],
                cwd=task_dir,
                env=None,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=stderr_handle,
                start_new_session=True,
            )
            session = cls(
                adapter,
                run_kwargs=run_kwargs,
                run_dir=run_dir,
                process=process,
                container_name=container_name,
                stderr_handle=stderr_handle,
                gateway_enabled=gateway_enabled,
            )
            session._reader_task = asyncio.create_task(session._reader_loop())
            await session._send_rpc(
                {
                    "id": session._initial_request_id,
                    "type": "prompt",
                    "message": adapter._solver_prompt(task),
                }
            )
            return session
        except Exception:
            if stderr_handle is not None:
                stderr_handle.close()
            subprocess.run(
                ["docker", "rm", "-f", container_name],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )
            raise

    async def _send_rpc(self, command: dict[str, Any]) -> None:
        if self.process.poll() is not None:
            raise AgentError("Pi RPC process is not running")
        if self.process.stdin is None:
            raise AgentError("Pi RPC stdin is unavailable")
        payload = (
            json.dumps(command, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
            + b"\n"
        )

        def write() -> None:
            assert self.process.stdin is not None
            self.process.stdin.write(payload)
            self.process.stdin.flush()

        async with self._rpc_lock:
            self._settled.clear()
            await asyncio.to_thread(write)

    async def _reader_loop(self) -> None:
        if self.process.stdout is None:
            self._fatal_error = "Pi RPC stdout is unavailable"
            self._settled.set()
            return

        with self.raw_path.open("ab") as raw_handle:
            while True:
                line = await asyncio.to_thread(self.process.stdout.readline)
                if not line:
                    break
                raw_handle.write(line)
                raw_handle.flush()
                text = line.decode("utf-8", errors="replace").rstrip("\r\n")
                if not text:
                    continue

                record: dict[str, Any] | None = None
                try:
                    parsed = json.loads(text)
                    if isinstance(parsed, dict):
                        record = parsed
                except json.JSONDecodeError:
                    pass

                self.adapter._translate_line(
                    text,
                    event_file=self.runtime_event_path,
                    account_model=not self.gateway_enabled,
                )

                if record is None:
                    continue
                if (
                    record.get("type") == "response"
                    and record.get("id") == self._initial_request_id
                    and record.get("success") is False
                ):
                    self._fatal_error = str(
                        record.get("error") or "initial Pi RPC prompt was rejected"
                    )
                    self._settled.set()
                elif record.get("type") == "agent_settled":
                    self._settled.set()

    async def events(self):
        runtime_remainder = ""
        agent_remainder = ""
        while True:
            runtime_events, self._runtime_event_offset, runtime_remainder = self._poll_path(
                self.runtime_event_path,
                offset=self._runtime_event_offset,
                source="runtime",
                remainder=runtime_remainder,
            )
            agent_events, self._agent_event_offset, agent_remainder = self._poll_path(
                self.agent_event_path,
                offset=self._agent_event_offset,
                source="agent",
                remainder=agent_remainder,
            )
            for event in runtime_events:
                yield event
            for event in agent_events:
                yield event

            if self._fatal_error is not None:
                raise AgentError(f"Pi RPC failed: {self._fatal_error}")

            if self.monitor.poll():
                await self.close("budget-exceeded")
                break
            if time.monotonic() - self.started > self.run_kwargs["task"].budgets.wall_time:
                self.timed_out = True
                self.adapter.trace.emit(
                    "budget.exceeded",
                    data={
                        "budget": "wall_time",
                        "limit": self.run_kwargs["task"].budgets.wall_time,
                        "value": time.monotonic() - self.started,
                    },
                )
                await self.close("wall-time-exceeded")
                break

            if self._settled.is_set():
                # One final scheduling turn lets the reader flush all records preceding settled.
                await asyncio.sleep(0)
                final_runtime, self._runtime_event_offset, runtime_remainder = self._poll_path(
                    self.runtime_event_path,
                    offset=self._runtime_event_offset,
                    source="runtime",
                    remainder=runtime_remainder,
                )
                final_agent, self._agent_event_offset, agent_remainder = self._poll_path(
                    self.agent_event_path,
                    offset=self._agent_event_offset,
                    source="agent",
                    remainder=agent_remainder,
                )
                for event in final_runtime:
                    yield event
                for event in final_agent:
                    yield event
                if self._settled.is_set():
                    break

            if self.process.poll() is not None:
                break
            await asyncio.sleep(self.poll_interval)

    async def observe(self, observation: AgentObservation) -> None:
        await super().observe(observation)
        if self._closed or self.process.poll() is not None:
            return
        message = (
            f"[Harness observation: {observation.type}]\n"
            + json.dumps(observation.data, ensure_ascii=False, sort_keys=True)
        )
        await self._send_rpc(
            {
                "id": f"observe_{uuid.uuid4().hex}",
                "type": "prompt",
                "message": message,
                "streamingBehavior": "steer",
            }
        )

    async def close(self, reason: str) -> None:
        if self._closed:
            return
        await super().close(reason)
        if self.process.poll() is None:
            try:
                await self._send_rpc(
                    {
                        "id": f"abort_{uuid.uuid4().hex}",
                        "type": "abort",
                    }
                )
            except (AgentError, BrokenPipeError, OSError):
                pass
            await asyncio.to_thread(
                subprocess.run,
                ["docker", "kill", self.container_name],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )

    async def result(self) -> AgentResult:
        if self._result_cache is not None:
            return self._result_cache

        if self.process.poll() is None and not self._closed:
            if self.process.stdin is not None:
                try:
                    self.process.stdin.close()
                except OSError:
                    pass

        if self.process.poll() is None:
            try:
                await asyncio.to_thread(self.process.wait, 5)
            except subprocess.TimeoutExpired:
                await asyncio.to_thread(
                    subprocess.run,
                    ["docker", "kill", self.container_name],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    check=False,
                )
                await asyncio.to_thread(self.process.wait)

        if self._reader_task is not None:
            await self._reader_task

        self.monitor.finish()
        self.stderr_handle.close()
        subprocess.run(
            ["docker", "rm", "-f", self.container_name],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )

        result = AgentResult(
            returncode=int(self.process.returncode or 0),
            timed_out=self.timed_out,
            budget_exceeded=self.monitor.exceeded,
            stdout=self.raw_path.read_text(encoding="utf-8", errors="replace"),
            stderr=self.stderr_path.read_text(encoding="utf-8", errors="replace"),
            metrics=self.monitor.metrics,
        )
        self.adapter.trace.emit(
            "agent.finished",
            actor="agent",
            data={
                "returncode": result.returncode,
                "timed_out": result.timed_out,
                "budget_exceeded": result.budget_exceeded,
                "metrics": result.metrics.as_dict(),
                "pi_protocol": "rpc",
                "execution": "docker",
                "image": self.adapter.spec.image,
            },
        )
        self._result_cache = result
        return result
