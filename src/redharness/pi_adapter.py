from __future__ import annotations

import json
import os
import shutil
import subprocess
import time
from pathlib import Path
from typing import Any

from .agent import AgentError, AgentResult, _terminate_process
from .budget import BudgetMonitor
from .models import AgentSpec, TaskSpec
from .trace import TraceRecorder


class PiAdapter:
    """Run Pi inside a constrained Docker/Kali container and consume Pi JSONL."""

    def __init__(self, spec: AgentSpec, *, allow_host: bool, trace: TraceRecorder) -> None:
        del allow_host
        if spec.pi is None:
            raise AgentError("Pi agent requires a pi configuration block")
        if not spec.image:
            raise AgentError("Pi agent requires a container image")
        if shutil.which("docker") is None:
            raise AgentError("docker executable was not found")
        self.spec = spec
        self.pi = spec.pi
        self.trace = trace

    @staticmethod
    def _append_event(path: Path, event_type: str, data: dict[str, Any]) -> None:
        payload = {"type": event_type, "data": data}
        with path.open("a", encoding="utf-8") as handle:
            handle.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")) + "\n")
            handle.flush()

    def _translate_line(
        self,
        line: str,
        *,
        event_file: Path,
        account_model: bool,
    ) -> None:
        try:
            event = json.loads(line)
        except json.JSONDecodeError as exc:
            self._append_event(
                event_file,
                "agent.telemetry_error",
                {"source": "pi", "message": str(exc), "line": line[:500]},
            )
            return
        if not isinstance(event, dict):
            return

        event_type = event.get("type")
        if event_type == "session":
            self._append_event(
                event_file,
                "pi.session",
                {
                    "session_id": event.get("id"),
                    "version": event.get("version"),
                    "cwd": event.get("cwd"),
                },
            )
            return
        if event_type == "agent_settled":
            self._append_event(event_file, "pi.agent_settled", {})
            return
        if event_type == "tool_execution_start":
            self._append_event(
                event_file,
                "tool.call",
                {
                    "tool": event.get("toolName", "unknown"),
                    "tool_call_id": event.get("toolCallId"),
                    "source": "pi",
                },
            )
            return
        if event_type == "tool_execution_end":
            self._append_event(
                event_file,
                "tool.result",
                {
                    "tool": event.get("toolName", "unknown"),
                    "tool_call_id": event.get("toolCallId"),
                    "source": "pi",
                    "is_error": bool(event.get("isError", False)),
                },
            )
            return
        if not account_model:
            return
        if event_type == "message_start":
            message = event.get("message")
            if isinstance(message, dict) and message.get("role") == "assistant":
                self._append_event(
                    event_file,
                    "model.request",
                    {
                        "source": "pi",
                        "provider": message.get("provider"),
                        "model": message.get("model", self.pi.model),
                    },
                )
            return
        if event_type != "message_end":
            return
        message = event.get("message")
        if not isinstance(message, dict) or message.get("role") != "assistant":
            return
        usage = message.get("usage")
        self._append_event(
            event_file,
            "model.response",
            {
                "source": "pi",
                "provider": message.get("provider"),
                "model": message.get("model", self.pi.model),
                "stop_reason": message.get("stopReason"),
            },
        )
        if not isinstance(usage, dict):
            return
        cost = usage.get("cost") if isinstance(usage.get("cost"), dict) else {}
        self._append_event(
            event_file,
            "model.usage",
            {
                "source": "pi",
                "provider": message.get("provider"),
                "model": message.get("model", self.pi.model),
                "input_tokens": int(usage.get("input", 0) or 0),
                "output_tokens": int(usage.get("output", 0) or 0),
                "total_tokens": int(usage.get("totalTokens", 0) or 0),
                "cache_read_tokens": int(usage.get("cacheRead", 0) or 0),
                "cache_write_tokens": int(usage.get("cacheWrite", 0) or 0),
                "reasoning_tokens": int(usage.get("reasoning", 0) or 0),
                "cost_usd": float(cost.get("total", 0.0) or 0.0),
            },
        )


    async def start_session(
        self,
        task: TaskSpec,
        *,
        task_dir: Path,
        run_dir: Path,
        environment_project: str | None,
        environment_network: str | None,
        seed: int,
        gateway_url: str | None = None,
        gateway_token: str | None = None,
        gateway_network: str | None = None,
    ):
        from .session import OneShotAgentSession

        return await OneShotAgentSession.start(
            self,
            run_dir=run_dir,
            run_kwargs={
                "task": task,
                "task_dir": task_dir,
                "run_dir": run_dir,
                "environment_project": environment_project,
                "environment_network": environment_network,
                "seed": seed,
                "gateway_url": gateway_url,
                "gateway_token": gateway_token,
                "gateway_network": gateway_network,
            },
        )

    def _poll_output(
        self,
        raw_path: Path,
        *,
        offset: int,
        remainder: str,
        event_file: Path,
        account_model: bool,
    ) -> tuple[int, str]:
        with raw_path.open("r", encoding="utf-8", errors="replace") as handle:
            handle.seek(offset)
            chunk = handle.read()
            offset = handle.tell()
        if not chunk:
            return offset, remainder
        text = remainder + chunk
        lines = text.splitlines(keepends=True)
        remainder = ""
        for line in lines:
            if not line.endswith(("\n", "\r")):
                remainder = line
                continue
            self._translate_line(
                line.strip(),
                event_file=event_file,
                account_model=account_model,
            )
        return offset, remainder

    def _prepare_agent_dir(
        self,
        *,
        run_dir: Path,
        gateway_url: str | None,
        gateway_token: str | None,
    ) -> Path:
        agent_dir = run_dir / "pi-agent"
        agent_dir.mkdir(parents=True, exist_ok=True)
        if gateway_url and gateway_token:
            models = {
                "providers": {
                    "redharness": {
                        "baseUrl": f"{gateway_url}/v1",
                        "api": "openai-completions",
                        "apiKey": "$REDHARNESS_GATEWAY_TOKEN",
                        "models": [{"id": self.pi.model}],
                    }
                }
            }
            (agent_dir / "models.json").write_text(
                json.dumps(models, ensure_ascii=False, indent=2) + "\n",
                encoding="utf-8",
            )
        return agent_dir

    def _pi_args(self, task: TaskSpec, *, gateway_enabled: bool) -> list[str]:
        args = [*self.pi.launcher_args, "--mode", "json", "--no-session"]
        args.append("--approve" if self.pi.approve_project else "--no-approve")
        for enabled, flag in (
            (self.pi.context_files, "--no-context-files"),
            (self.pi.extensions, "--no-extensions"),
            (self.pi.skills, "--no-skills"),
            (self.pi.prompt_templates, "--no-prompt-templates"),
            (self.pi.themes, "--no-themes"),
            (self.pi.mcp, "--no-mcp"),
        ):
            if not enabled:
                args.append(flag)
        if self.pi.offline:
            args.append("--offline")
        if self.pi.tools:
            args.extend(["--tools", ",".join(self.pi.tools)])
        else:
            args.append("--no-tools")
        if gateway_enabled:
            args.extend(["--provider", "redharness", "--model", self.pi.model])
        else:
            if self.pi.provider:
                args.extend(["--provider", self.pi.provider])
            args.extend(["--model", self.pi.model])
        if self.pi.thinking:
            args.extend(["--thinking", self.pi.thinking])
        args.extend(self.pi.extra_args)
        world_protocol = (
            "\n\nRed Harness world-state protocol:\n"
            "Read $REDHARNESS_WORLD_CONTEXT before acting. When you discover durable, task-relevant "
            "state, append one JSON object per line to "
            "$REDHARNESS_WORLD_INBOX. Use schema_version redharness.world.submit/v1 and one of "
            "entity, relation, observation, artifact, capability, hypothesis, goal, action, constraint, or failure. "
            "Prefer high-value state changes over verbose narration. Do not write to "
            "world.events.jsonl or world.snapshot.json directly."
        )
        args.append(task.objective.description + world_protocol)
        return args

    def _create_command(
        self,
        task: TaskSpec,
        *,
        task_dir: Path,
        run_dir: Path,
        container_name: str,
        network: str,
        seed: int,
        gateway_url: str | None,
        gateway_token: str | None,
        host_gateway: bool,
    ) -> list[str]:
        command = [
            "docker", "create", "--rm", "--pull=never",
            "--name", container_name,
            "--read-only",
            "--cap-drop", "ALL",
            "--security-opt", "no-new-privileges",
            "--pids-limit", "1024",
            "--memory", "8g",
            "--cpus", "4",
            "--tmpfs", "/tmp:rw,nosuid,nodev,size=1g",
            "--tmpfs", "/home/agent:rw,nosuid,nodev,size=512m,uid=1000,gid=1000",
            "--network", network,
        ]
        if self.spec.runtime:
            command.extend(["--runtime", self.spec.runtime])
        for capability in self.pi.cap_add:
            command.extend(["--cap-add", capability])
        if host_gateway:
            command.extend(["--add-host", "host.docker.internal:host-gateway"])
        command.extend(
            [
                "-v", f"{task_dir.resolve()}:/task:ro",
                "-v", f"{run_dir.resolve()}:/run/redharness:rw",
                "-w", "/run/redharness",
                "-e", f"REDHARNESS_TASK_ID={task.id}",
                "-e", "REDHARNESS_TASK_DIR=/task",
                "-e", "REDHARNESS_RUN_DIR=/run/redharness",
                "-e", "REDHARNESS_WORLD_INBOX=/run/redharness/world.inbox.jsonl",
                "-e",
                "REDHARNESS_WORLD_CONTEXT=/run/redharness/world.context.txt",
                "-e",
                "REDHARNESS_SUBMISSION_INBOX=/run/redharness/submission.inbox.jsonl",
                "-e",
                "REDHARNESS_FEEDBACK_FILE=/run/redharness/agent.feedback.jsonl",
                "-e",
                "REDHARNESS_PROGRESS_FILE=/run/redharness/progress.json",
                "-e", f"REDHARNESS_SEED={seed}",
                "-e", "PI_CODING_AGENT_DIR=/run/redharness/pi-agent",
                "-e", "PI_CODING_AGENT_SESSION_DIR=/run/redharness/pi-sessions",
                "-e", "PI_TELEMETRY=0",
                "-e", "PI_SKIP_VERSION_CHECK=1",
            ]
        )
        if self.pi.offline:
            command.extend(["-e", "PI_OFFLINE=1"])
        for name in self.pi.env_passthrough:
            value = os.environ.get(name)
            if value is not None:
                command.extend(["-e", f"{name}={value}"])
        for key, value in sorted(self.spec.env.items()):
            command.extend(["-e", f"{key}={value}"])
        if gateway_url and gateway_token:
            command.extend(
                [
                    "-e", f"REDHARNESS_GATEWAY_URL={gateway_url}",
                    "-e", f"REDHARNESS_GATEWAY_TOKEN={gateway_token}",
                    "-e", f"OPENAI_BASE_URL={gateway_url}/v1",
                    "-e", f"OPENAI_API_KEY={gateway_token}",
                ]
            )
        command.extend([str(self.spec.image), self.pi.binary, *self._pi_args(task, gateway_enabled=bool(gateway_url and gateway_token))])
        return command

    def run(
        self,
        task: TaskSpec,
        *,
        task_dir: Path,
        run_dir: Path,
        environment_project: str | None,
        environment_network: str | None,
        seed: int,
        gateway_url: str | None = None,
        gateway_token: str | None = None,
        gateway_network: str | None = None,
    ) -> AgentResult:
        del environment_project
        gateway_enabled = bool(gateway_url and gateway_token)
        sidecar_gateway = gateway_enabled and gateway_network is not None
        if self.spec.network == "none" and gateway_enabled:
            raise AgentError("Pi network:none is incompatible with Gateway access")
        if sidecar_gateway and self.spec.network == "host":
            raise AgentError("Pi network:host is incompatible with sidecar Gateway mode")

        if sidecar_gateway:
            primary_network = str(gateway_network)
        elif self.spec.network == "host":
            primary_network = "host"
        elif self.spec.network == "environment":
            primary_network = environment_network or "bridge"
        else:
            primary_network = "none"

        event_file = run_dir / "events.jsonl"
        event_file.write_text("", encoding="utf-8")
        raw_path = run_dir / "agent.stdout.log"
        stderr_path = run_dir / "agent.stderr.log"
        raw_path.write_text("", encoding="utf-8")
        (run_dir / "pi-sessions").mkdir(parents=True, exist_ok=True)
        self._prepare_agent_dir(
            run_dir=run_dir,
            gateway_url=gateway_url,
            gateway_token=gateway_token,
        )

        container_name = ("rh_pi_" + run_dir.name.lower()).replace("-", "_")[:63]
        command = self._create_command(
            task,
            task_dir=task_dir,
            run_dir=run_dir,
            container_name=container_name,
            network=primary_network,
            seed=seed,
            gateway_url=gateway_url,
            gateway_token=gateway_token,
            host_gateway=gateway_enabled and not sidecar_gateway and primary_network != "host",
        )
        self.trace.emit(
            "agent.started",
            actor="agent",
            data={
                "agent_id": self.spec.id,
                "type": "pi",
                "image": self.spec.image,
                "network": primary_network,
                "runtime": self.spec.runtime,
                "model": self.pi.model,
                "provider": "redharness" if gateway_enabled else self.pi.provider,
                "tools": self.pi.tools,
            },
        )

        create = subprocess.run(command, capture_output=True, text=True, check=False)
        if create.returncode != 0:
            raise AgentError(f"docker create Pi failed: {create.stderr.strip()}")
        try:
            if sidecar_gateway and environment_network and environment_network != gateway_network:
                connected = subprocess.run(
                    ["docker", "network", "connect", environment_network, container_name],
                    capture_output=True,
                    text=True,
                    check=False,
                )
                if connected.returncode != 0:
                    raise AgentError(f"docker network connect failed: {connected.stderr.strip()}")

            monitor = BudgetMonitor(event_file, task.budgets, self.trace)
            timed_out = False
            started = time.monotonic()
            offset = 0
            remainder = ""
            with raw_path.open("ab") as stdout_handle, stderr_path.open("wb") as stderr_handle:
                proc = subprocess.Popen(
                    ["docker", "start", "-a", container_name],
                    stdout=stdout_handle,
                    stderr=stderr_handle,
                    start_new_session=True,
                )
                while proc.poll() is None:
                    offset, remainder = self._poll_output(
                        raw_path,
                        offset=offset,
                        remainder=remainder,
                        event_file=event_file,
                        account_model=not gateway_enabled,
                    )
                    if monitor.poll():
                        _terminate_process(proc)
                        subprocess.run(["docker", "kill", container_name], capture_output=True, check=False)
                        break
                    if time.monotonic() - started > task.budgets.wall_time:
                        timed_out = True
                        self.trace.emit("budget.exceeded", data={"budget": "wall_time"})
                        _terminate_process(proc)
                        subprocess.run(["docker", "kill", container_name], capture_output=True, check=False)
                        break
                    time.sleep(0.05)
                returncode = proc.wait()

            offset, remainder = self._poll_output(
                raw_path,
                offset=offset,
                remainder=remainder,
                event_file=event_file,
                account_model=not gateway_enabled,
            )
            del offset
            if remainder.strip():
                self._translate_line(
                    remainder.strip(),
                    event_file=event_file,
                    account_model=not gateway_enabled,
                )
            monitor.finish()
            result = AgentResult(
                returncode=returncode,
                timed_out=timed_out,
                budget_exceeded=monitor.exceeded,
                stdout=raw_path.read_text(encoding="utf-8", errors="replace"),
                stderr=stderr_path.read_text(encoding="utf-8", errors="replace"),
                metrics=monitor.metrics,
            )
        finally:
            subprocess.run(
                ["docker", "rm", "-f", container_name],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )

        self.trace.emit(
            "agent.finished",
            actor="agent",
            data={
                "returncode": result.returncode,
                "timed_out": result.timed_out,
                "budget_exceeded": result.budget_exceeded,
                "metrics": result.metrics.as_dict(),
                "pi_protocol": "json",
                "containerized": True,
            },
        )
        return result

    def _command(self, task: TaskSpec, *, gateway_enabled: bool) -> list[str]:
        return [self.pi.binary, *self._pi_args(task, gateway_enabled=gateway_enabled)]


