from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

from .agent import AgentError, AgentResult, _terminate_process
from .budget import BudgetMonitor
from .models import AgentSpec, PiSpec, TaskSpec
from .trace import TraceRecorder


class PiAdapter:
    """First-class adapter for the Pi coding agent JSON event protocol."""

    def __init__(self, spec: AgentSpec, *, allow_host: bool, trace: TraceRecorder) -> None:
        if not allow_host:
            raise AgentError(
                "Pi executes as a host process; pass --allow-host-agent only for trusted runs"
            )
        if spec.pi is None:
            raise AgentError("Pi agent requires a pi configuration block")
        self.spec = spec
        self.pi = spec.pi
        self.trace = trace
        executable = self.pi.binary
        if Path(executable).is_absolute():
            if not Path(executable).is_file():
                raise AgentError(f"Pi executable was not found: {executable}")
        elif shutil.which(executable) is None:
            raise AgentError(f"Pi executable was not found: {executable}")

    @staticmethod
    def _append_event(path: Path, event_type: str, data: dict[str, Any]) -> None:
        payload = {"type": event_type, "data": data}
        with path.open("a", encoding="utf-8") as handle:
            handle.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")) + "\n")
            handle.flush()

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

    def _command(
        self,
        task: TaskSpec,
        *,
        gateway_enabled: bool,
    ) -> list[str]:
        command = [self.pi.binary, *self.pi.launcher_args, "--mode", "json", "--no-session"]
        command.append("--approve" if self.pi.approve_project else "--no-approve")
        if not self.pi.context_files:
            command.append("--no-context-files")
        if not self.pi.extensions:
            command.append("--no-extensions")
        if not self.pi.skills:
            command.append("--no-skills")
        if not self.pi.prompt_templates:
            command.append("--no-prompt-templates")
        if not self.pi.themes:
            command.append("--no-themes")
        if not self.pi.mcp:
            command.append("--no-mcp")
        if self.pi.offline:
            command.append("--offline")
        if self.pi.tools:
            command.extend(["--tools", ",".join(self.pi.tools)])
        else:
            command.append("--no-tools")

        if gateway_enabled:
            command.extend(["--provider", "redharness", "--model", self.pi.model])
        else:
            if self.pi.provider:
                command.extend(["--provider", self.pi.provider])
            command.extend(["--model", self.pi.model])
        if self.pi.thinking:
            command.extend(["--thinking", self.pi.thinking])
        command.extend(self.pi.extra_args)
        command.append(
            "You are running inside Red Harness. Complete the benchmark objective, "
            "and make the required changes in REDHARNESS_RUN_DIR. Do not merely report "
            f"success. Objective: {task.objective.description}"
        )
        return command

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

    def _poll_output(
        self,
        raw_path: Path,
        *,
        offset: int,
        remainder: str,
        event_file: Path,
        account_model: bool,
    ) -> tuple[int, str]:
        if not raw_path.exists():
            return offset, remainder
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
        del environment_network
        if gateway_network is not None:
            raise AgentError("host Pi adapter cannot access Docker sidecar Gateway mode")
        gateway_enabled = bool(gateway_url and gateway_token)
        event_file = run_dir / "events.jsonl"
        event_file.write_text("", encoding="utf-8")
        raw_path = run_dir / "agent.stdout.log"
        stderr_path = run_dir / "agent.stderr.log"
        raw_path.write_text("", encoding="utf-8")
        agent_dir = self._prepare_agent_dir(
            run_dir=run_dir,
            gateway_url=gateway_url,
            gateway_token=gateway_token,
        )
        session_dir = run_dir / "pi-sessions"
        session_dir.mkdir(parents=True, exist_ok=True)

        env = os.environ.copy()
        env.update(self.spec.env)
        env.update(
            {
                "REDHARNESS_TASK_ID": task.id,
                "REDHARNESS_TASK_DIR": str(task_dir),
                "REDHARNESS_RUN_DIR": str(run_dir),
                "REDHARNESS_OBJECTIVE": task.objective.description,
                "REDHARNESS_ENV_PROJECT": environment_project or "",
                "REDHARNESS_EVENT_FILE": str(event_file),
                "REDHARNESS_SEED": str(seed),
                "PI_CODING_AGENT_DIR": str(agent_dir),
                "PI_CODING_AGENT_SESSION_DIR": str(session_dir),
                "PI_TELEMETRY": "0",
                "PI_SKIP_VERSION_CHECK": "1",
            }
        )
        if self.pi.offline:
            env["PI_OFFLINE"] = "1"
        if gateway_enabled:
            env.update(
                {
                    "REDHARNESS_GATEWAY_URL": str(gateway_url),
                    "REDHARNESS_GATEWAY_TOKEN": str(gateway_token),
                    "OPENAI_BASE_URL": f"{gateway_url}/v1",
                    "OPENAI_API_KEY": str(gateway_token),
                }
            )

        command = self._command(task, gateway_enabled=gateway_enabled)
        self.trace.emit(
            "agent.started",
            actor="agent",
            data={
                "agent_id": self.spec.id,
                "type": "pi",
                "model": self.pi.model,
                "provider": "redharness" if gateway_enabled else self.pi.provider,
                "thinking": self.pi.thinking,
                "tools": self.pi.tools,
                "gateway_injected": gateway_enabled,
            },
        )

        monitor = BudgetMonitor(event_file, task.budgets, self.trace)
        timed_out = False
        started = time.monotonic()
        offset = 0
        remainder = ""
        with raw_path.open("ab") as stdout_handle, stderr_path.open("wb") as stderr_handle:
            proc = subprocess.Popen(
                command,
                cwd=run_dir,
                env=env,
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
                exceeded = monitor.poll()
                if exceeded:
                    _terminate_process(proc)
                    break
                if time.monotonic() - started > task.budgets.wall_time:
                    timed_out = True
                    self.trace.emit(
                        "budget.exceeded",
                        data={
                            "budget": "wall_time",
                            "limit": task.budgets.wall_time,
                            "value": time.monotonic() - started,
                        },
                    )
                    _terminate_process(proc)
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
        self.trace.emit(
            "agent.finished",
            actor="agent",
            data={
                "returncode": result.returncode,
                "timed_out": result.timed_out,
                "budget_exceeded": result.budget_exceeded,
                "metrics": result.metrics.as_dict(),
                "pi_protocol": "json",
            },
        )
        return result
