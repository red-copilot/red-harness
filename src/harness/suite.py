from __future__ import annotations

import hashlib
import json
import math
import os
import platform
import shutil
import stat
import subprocess
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from . import __version__
from .gateway_runtime import GatewayConfig
from .models import AgentSpec, SuiteSpec, TaskSpec, load_task
from .orchestrator import Orchestrator
from .runtime.solver_profile import DEFAULT_SOLVER_PROFILE, SolverProfile
from .scoring import pass_at_k, weighted_mean
from .secureio import open_beneath, open_regular_file
from .trace import sanitize_observability_data


def _suite_run_id() -> str:
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    return f"suite_{stamp}_{uuid.uuid4().hex[:8]}"


def _safe_value(value: Any) -> Any:
    return sanitize_observability_data({"value": value}).get("value")


def _container_image_id(image: str | None) -> str | None:
    if not image or not shutil.which("docker"):
        return None
    try:
        inspected = subprocess.run(
            ["docker", "image", "inspect", "--format", "{{.Id}}", image],
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    image_id = inspected.stdout.strip()
    return image_id if inspected.returncode == 0 and image_id else None


def _agent_runtime_manifest(agent: AgentSpec) -> dict[str, Any]:
    pi = agent.pi
    executable = None
    command = agent.command if agent.type == "cli" else ([pi.binary] if pi else [])
    if command:
        executable_path = shutil.which(command[0])
        if executable_path:
            try:
                executable_file = Path(executable_path).resolve()
                executable = _bounded_file_sha256(
                    executable_file,
                    max_bytes=256 * 1024 * 1024,
                )
            except OSError:
                pass
    return {
        "type": agent.type,
        "image_config_sha256": (
            hashlib.sha256(agent.image.encode()).hexdigest() if agent.image else None
        ),
        "image_id": (
            _container_image_id(agent.image) if agent.type in {"docker", "pi"} else None
        ),
        "runtime": _safe_value(agent.runtime),
        "pi_binary": _safe_value(pi.binary) if pi else None,
        "pi_mode": pi.mode if pi else None,
        "model_provider": _safe_value(pi.provider) if pi else None,
        "model": _safe_value(pi.model) if pi else None,
        "toolchain": {
            "python_version": platform.python_version(),
            "python_implementation": platform.python_implementation(),
            "platform": platform.platform(),
            "cli_executable_sha256": executable,
        },
    }


def _string_sha256(value: str | None) -> str | None:
    return hashlib.sha256(value.encode("utf-8")).hexdigest() if value is not None else None


def _bounded_file_sha256(path: Path, *, max_bytes: int = 4 * 1024 * 1024) -> str:
    digest = hashlib.sha256()
    total = 0
    before_path = path.stat(follow_symlinks=False)
    if not stat.S_ISREG(before_path.st_mode) or before_path.st_size > max_bytes:
        raise ValueError("manifest input is not a bounded regular file")
    with open_regular_file(path, "rb") as stream:
        before = os.fstat(stream.fileno())
        before_identity = (
            before.st_dev,
            before.st_ino,
            before.st_size,
            before.st_mtime_ns,
        )
        if (
            not stat.S_ISREG(before.st_mode)
            or before_identity
            != (
                before_path.st_dev,
                before_path.st_ino,
                before_path.st_size,
                before_path.st_mtime_ns,
            )
        ):
            raise ValueError("manifest input changed before hashing")
        while True:
            chunk = stream.read(min(1024 * 1024, max_bytes + 1 - total))
            if not chunk:
                break
            total += len(chunk)
            if total > max_bytes:
                raise ValueError("manifest input exceeds the hashing limit")
            digest.update(chunk)
        after = os.fstat(stream.fileno())
    after_path = path.stat(follow_symlinks=False)
    after_identity = (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns)
    if before_identity != after_identity or before_identity != (
        after_path.st_dev,
        after_path.st_ino,
        after_path.st_size,
        after_path.st_mtime_ns,
    ) or total != after.st_size:
        raise ValueError("manifest input changed while hashing")
    return digest.hexdigest()


def _gateway_manifest(config: GatewayConfig | None) -> dict[str, Any] | None:
    if config is None:
        return None
    prices = (
        config.input_price_per_million_usd,
        config.output_price_per_million_usd,
    )
    if any(not math.isfinite(value) or value < 0 for value in prices):
        raise ValueError("gateway model prices must be finite and non-negative")
    return {
        "mode": config.mode,
        "model_upstream_sha256": _string_sha256(config.model_upstream),
        "model_api_key_configured": bool(config.model_api_key),
        "input_price_per_million_usd": config.input_price_per_million_usd,
        "output_price_per_million_usd": config.output_price_per_million_usd,
        "policy_sha256": (
            _bounded_file_sha256(config.policy_path) if config.policy_path is not None else None
        ),
        "sidecar_image_config_sha256": _string_sha256(config.sidecar_image),
        "sidecar_image_id": _container_image_id(config.sidecar_image),
        "sidecar_runtime_sha256": _string_sha256(config.sidecar_runtime),
    }


def _artifact_hashes(workspace: Path) -> list[dict[str, Any]]:
    """Hash bounded regular output files without following Agent-created links."""
    excluded = {
        "world.context.txt", "progress.json", "events.jsonl", "world.inbox.jsonl",
        "submission.inbox.jsonl", "agent.feedback.jsonl",
    }
    artifacts: list[dict[str, Any]] = []
    total = 0
    if not workspace.is_dir() or workspace.is_symlink():
        return artifacts
    for directory, dirs, files in os.walk(workspace, followlinks=False):
        dirs[:] = sorted(name for name in dirs if not (Path(directory) / name).is_symlink())
        for name in sorted(files):
            path = Path(directory) / name
            relative = path.relative_to(workspace).as_posix()
            if name in excluded or len(artifacts) >= 512:
                continue
            try:
                relative_path = path.relative_to(workspace)
                before_path = path.stat(follow_symlinks=False)
                if (
                    not stat.S_ISREG(before_path.st_mode)
                    or before_path.st_size > 16 * 1024 * 1024
                ):
                    continue
                if total + before_path.st_size > 64 * 1024 * 1024:
                    continue
                with open_beneath(workspace, relative_path, "rb") as stream:
                    before = os.fstat(stream.fileno())
                    if (
                        not stat.S_ISREG(before.st_mode)
                        or (before.st_ino, before.st_size, before.st_mtime_ns)
                        != (before_path.st_ino, before_path.st_size, before_path.st_mtime_ns)
                    ):
                        continue
                    digest = hashlib.sha256()
                    size = 0
                    overflow = False
                    while True:
                        remaining = min(16 * 1024 * 1024, 64 * 1024 * 1024 - total) - size
                        chunk = stream.read(min(1024 * 1024, remaining + 1))
                        if not chunk:
                            break
                        size += len(chunk)
                        if size > 16 * 1024 * 1024 or total + size > 64 * 1024 * 1024:
                            overflow = True
                            break
                        digest.update(chunk)
                    after = os.fstat(stream.fileno())
                after_path = path.stat(follow_symlinks=False)
                identity = (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns)
                if (
                    overflow
                    or size != before.st_size
                    or identity
                    != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns)
                    or identity
                    != (
                        after_path.st_dev,
                        after_path.st_ino,
                        after_path.st_size,
                        after_path.st_mtime_ns,
                    )
                ):
                    continue
            except OSError:
                continue
            total += size
            artifacts.append({
                "path_sha256": hashlib.sha256(relative.encode()).hexdigest(),
                "sha256": digest.hexdigest(),
                "size_bytes": before.st_size,
            })
    return artifacts


def _verifier_sha256(task: TaskSpec, task_path: Path) -> str | None:
    if task.verification.type != "python":
        return None
    candidate = (task_path.resolve().parent / task.verification.entrypoint).resolve()
    task_dir = task_path.resolve().parent
    try:
        if not candidate.is_relative_to(task_dir) or not candidate.is_file() or candidate.is_symlink():
            return None
        return _bounded_file_sha256(candidate, max_bytes=64 * 1024 * 1024)
    except OSError:
        return None


class SuiteRunner:
    def __init__(
        self,
        *,
        runs_root: str | Path = ".harness/runs",
        suites_root: str | Path = ".harness/suites",
    ) -> None:
        self.orchestrator = Orchestrator(runs_root)
        self.suites_root = Path(suites_root)

    def _run_attempt(
        self,
        *,
        task: TaskSpec,
        task_path: Path,
        weight: float,
        repeat_index: int,
        seed: int,
        agent: AgentSpec,
        agent_path: Path,
        allow_host_agent: bool,
        gateway_config: GatewayConfig | None,
        solver_profile: SolverProfile,
    ) -> dict[str, Any]:
        result = self.orchestrator.run(
            task=task,
            task_path=task_path,
            agent=agent,
            agent_path=agent_path,
            allow_host_agent=allow_host_agent,
            seed=seed,
            gateway_config=gateway_config,
            solver_profile=solver_profile,
        )
        workspace = self.orchestrator.runs_root / str(result["run_id"]) / "agent-workspace"
        return {
            **result,
            "output_artifacts": _artifact_hashes(workspace),
            "task_path": str(task_path),
            "category": task.category,
            "weight": weight,
            "repeat_index": repeat_index,
            "seed": seed,
        }

    @staticmethod
    def _task_summaries(attempts: list[dict], pass_k_values: list[int]) -> list[dict]:
        groups: dict[str, list[dict]] = {}
        for attempt in attempts:
            groups.setdefault(str(attempt["task_id"]), []).append(attempt)

        summaries: list[dict] = []
        for task_id, runs in groups.items():
            successes = sum(1 for run in runs if run["success"])
            n = len(runs)
            summaries.append(
                {
                    "task_id": _safe_value(task_id),
                    "category": _safe_value(runs[0]["category"]),
                    "weight": float(runs[0]["weight"]),
                    "attempts": n,
                    "successes": successes,
                    "success_rate": successes / n if n else 0.0,
                    "mean_score": (
                        sum(float(run["score"]) for run in runs) / n if n else 0.0
                    ),
                    "pass_at_k": {
                        str(k): pass_at_k(n, successes, k)
                        for k in pass_k_values
                        if k <= n
                    },
                }
            )
        return sorted(summaries, key=lambda item: item["task_id"])

    @staticmethod
    def _metrics_summary(attempts: list[dict]) -> dict[str, int | float]:
        def metric(attempt: dict, name: str) -> float:
            metrics = attempt.get("metrics")
            value = metrics.get(name, 0) if isinstance(metrics, dict) else 0
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                return 0.0
            value = float(value)
            return value if math.isfinite(value) and value >= 0 else 0.0

        durations = [metric(attempt, "duration_ms") for attempt in attempts]
        objective_verdicts = 0
        false_positives = 0
        unverified_claims = 0
        replan_count = 0
        failed_tool_calls = 0
        for attempt in attempts:
            success = attempt.get("success") is True
            progress = attempt.get("progress")
            progress = progress if isinstance(progress, dict) else {}
            for key, accumulator in (
                ("replan_count", "replans"),
                ("failure_count", "failed_tool_calls"),
            ):
                value = progress.get(key, 0)
                if isinstance(value, int) and not isinstance(value, bool) and value >= 0:
                    if accumulator == "replans":
                        replan_count += value
                    else:
                        failed_tool_calls += value
            verdict = progress.get("objective_verdict")
            verdict_status = verdict.get("status") if isinstance(verdict, dict) else None
            has_verdict = verdict_status in {"verified", "contradicted"}
            objective_verdicts += int(has_verdict)
            if success and verdict_status != "verified":
                false_positives += 1
            claims = progress.get("completion_claims")
            if verdict_status == "contradicted" and isinstance(claims, list):
                unverified_claims += sum(1 for claim in claims if isinstance(claim, str))

        count = len(attempts)
        return {
            "attempts": count,
            "total_duration_ms": int(sum(durations)),
            "mean_duration_ms": sum(durations) / count if count else 0.0,
            "total_input_tokens": int(sum(metric(item, "input_tokens") for item in attempts)),
            "total_output_tokens": int(sum(metric(item, "output_tokens") for item in attempts)),
            "total_tokens": int(sum(metric(item, "total_tokens") for item in attempts)),
            "total_tool_calls": int(sum(metric(item, "tool_calls") for item in attempts)),
            "total_model_calls": int(sum(metric(item, "model_calls") for item in attempts)),
            "total_cost_usd": sum(metric(item, "cost_usd") for item in attempts),
            "total_replans": replan_count,
            "failed_tool_calls": failed_tool_calls,
            "objective_verdicts": objective_verdicts,
            "false_positive_verifications": false_positives,
            "unverified_completion_claims": unverified_claims,
        }

    @staticmethod
    def _report_attempt(attempt: dict[str, Any]) -> dict[str, Any]:
        """Project an attempt into a secret-safe, reproducible report record."""
        raw_metrics = attempt.get("metrics")
        metric_names = {
            "duration_ms", "input_tokens", "output_tokens", "total_tokens",
            "model_calls", "tool_calls", "cost_usd",
        }
        metrics: dict[str, int | float] = {}
        if isinstance(raw_metrics, dict):
            for name in metric_names:
                value = raw_metrics.get(name)
                if (
                    isinstance(value, (int, float))
                    and not isinstance(value, bool)
                    and math.isfinite(float(value))
                    and value >= 0
                ):
                    metrics[name] = value
        progress = attempt.get("progress")
        verdict = progress.get("objective_verdict") if isinstance(progress, dict) else None
        verdict_status = verdict.get("status") if isinstance(verdict, dict) else None
        if verdict_status not in {"verified", "contradicted", "inconclusive", "unavailable"}:
            verdict_status = None
        failure_class = attempt.get("failure_class")
        if not isinstance(failure_class, str) or len(failure_class) > 64:
            failure_class = "unknown"
        return {
            "task_id": _safe_value(attempt.get("task_id")),
            "category": _safe_value(attempt.get("category")),
            "weight": attempt.get("weight"),
            "repeat_index": attempt.get("repeat_index"),
            "seed": attempt.get("seed"),
            "status": attempt.get("status"),
            "success": attempt.get("success") is True,
            "score": attempt.get("score", 0.0),
            "failure_class": failure_class,
            "objective_verdict_status": verdict_status,
            "metrics": metrics,
            "output_artifacts": attempt.get("output_artifacts", []),
        }

    @staticmethod
    def _domain_summaries(
        task_summaries: list[dict], pass_k_values: list[int], attempts: list[dict]
    ) -> dict:
        domains: dict[str, list[dict]] = {}
        for item in task_summaries:
            domains.setdefault(str(item["category"]), []).append(item)
        domain_attempts: dict[str, list[dict]] = {}
        for attempt in attempts:
            domain_attempts.setdefault(str(_safe_value(attempt["category"])), []).append(attempt)

        result: dict[str, dict] = {}
        for category, tasks in sorted(domains.items()):
            failure_classes: dict[str, int] = {}
            for attempt in domain_attempts.get(category, []):
                label = attempt.get("failure_class", "unknown")
                if isinstance(label, str) and label:
                    failure_classes[label] = failure_classes.get(label, 0) + 1
            result[_safe_value(category)] = {
                "tasks": len(tasks),
                "weighted_score": weighted_mean(
                    (float(task["mean_score"]), float(task["weight"])) for task in tasks
                ),
                "weighted_success_rate": weighted_mean(
                    (float(task["success_rate"]), float(task["weight"])) for task in tasks
                ),
                "pass_at_k": {
                    str(k): weighted_mean(
                        (
                            float(task["pass_at_k"][str(k)]),
                            float(task["weight"]),
                        )
                        for task in tasks
                        if str(k) in task["pass_at_k"]
                    )
                    for k in pass_k_values
                },
                "metrics": SuiteRunner._metrics_summary(domain_attempts.get(category, [])),
                "failure_classes": dict(sorted(failure_classes.items())),
            }
        return result

    def run(
        self,
        *,
        suite: SuiteSpec,
        suite_path: Path,
        agent: AgentSpec,
        agent_path: Path,
        allow_host_agent: bool = False,
        gateway_config: GatewayConfig | None = None,
        workers: int | None = None,
        solver_profile: SolverProfile = DEFAULT_SOLVER_PROFILE,
    ) -> dict:
        solver_profile = SolverProfile(solver_profile)
        if workers is not None and (
            isinstance(workers, bool)
            or not isinstance(workers, int)
            or not 1 <= workers <= 64
        ):
            raise ValueError("suite workers must be between 1 and 64")
        suite_id = _suite_run_id()
        suite_dir = (self.suites_root / suite_id).resolve()
        suite_dir.mkdir(parents=True, exist_ok=False)
        base_dir = suite_path.resolve().parent
        worker_count = workers if workers is not None else suite.workers
        worker_count = max(1, worker_count)
        gateway_manifest = _gateway_manifest(gateway_config)

        jobs: list[dict[str, Any]] = []
        index = 0
        for task_entry in suite.tasks:
            task_path = (base_dir / task_entry.path).resolve()
            task = load_task(task_path)
            for repeat_index in range(suite.repeat):
                jobs.append(
                    {
                        "index": index,
                        "task": task,
                        "task_path": task_path,
                        "task_entry_path": task_entry.path,
                        "weight": task_entry.weight,
                        "repeat_index": repeat_index,
                        "seed": suite.base_seed + repeat_index,
                    }
                )
                index += 1

        task_manifest: list[dict[str, Any]] = []
        for job in jobs:
            if job["repeat_index"] != 0:
                continue
            task = job["task"]
            task_path = job["task_path"]
            task_manifest.append({
                "task_id": _safe_value(task.id),
                "task_path_sha256": hashlib.sha256(
                    job["task_entry_path"].encode()
                ).hexdigest(),
                "category": _safe_value(task.category),
                "weight": job["weight"],
                "task_sha256": _bounded_file_sha256(task_path.resolve()),
                "budgets": task.budgets.model_dump(exclude_none=True),
                "verification": {
                    "type": task.verification.type,
                    "entrypoint_sha256": hashlib.sha256(
                        task.verification.entrypoint.encode()
                    ).hexdigest(),
                    "command_sha256": hashlib.sha256(
                        json.dumps(task.verification.command, separators=(",", ":")).encode()
                    ).hexdigest(),
                    "command_count": len(task.verification.command),
                    "network": task.verification.network,
                    "timeout": task.verification.timeout,
                    "runtime": _safe_value(task.verification.runtime),
                },
                "verification_image_id": _container_image_id(task.verification.image),
                "verifier_sha256": _verifier_sha256(task, task_path),
                "seeds": [suite.base_seed + index for index in range(suite.repeat)],
            })

        indexed_results: dict[int, dict] = {}
        manifest_suite_sha256 = _bounded_file_sha256(suite_path.resolve())
        manifest_agent_sha256 = _bounded_file_sha256(agent_path.resolve())
        with ThreadPoolExecutor(max_workers=worker_count) as executor:
            futures = {
                executor.submit(
                    self._run_attempt,
                    task=job["task"],
                    task_path=job["task_path"],
                    weight=job["weight"],
                    repeat_index=job["repeat_index"],
                    seed=job["seed"],
                    agent=agent,
                    agent_path=agent_path,
                    allow_host_agent=allow_host_agent,
                    gateway_config=gateway_config,
                    solver_profile=solver_profile,
                ): int(job["index"])
                for job in jobs
            }
            for future in as_completed(futures):
                indexed_results[futures[future]] = future.result()

        attempts = [indexed_results[i] for i in sorted(indexed_results)]
        successes = sum(1 for item in attempts if item["success"])
        task_summaries = self._task_summaries(attempts, suite.pass_k)
        domains = self._domain_summaries(task_summaries, suite.pass_k, attempts)

        summary = {
            "schema_version": "harness/evaluation-report/v1",
            "suite_run_id": suite_id,
            "suite_id": _safe_value(suite.id),
            "agent_id": _safe_value(agent.id),
            "workers": worker_count,
            "attempts": len(attempts),
            "successes": successes,
            "success_rate": successes / len(attempts) if attempts else 0.0,
            "weighted_score": weighted_mean(
                (float(item["score"]), float(item["weight"])) for item in attempts
            ),
            "metrics": self._metrics_summary(attempts),
            "pass_at_k": {
                str(k): weighted_mean(
                    (
                        float(task["pass_at_k"][str(k)]),
                        float(task["weight"]),
                    )
                    for task in task_summaries
                    if str(k) in task["pass_at_k"]
                )
                for k in suite.pass_k
            },
            "domains": domains,
            "tasks": task_summaries,
            "gateway_enabled": gateway_config is not None,
            "evaluation_manifest": {
                "schema_version": "harness/evaluation-manifest/v1",
                "harness_version": __version__,
                "suite_sha256": manifest_suite_sha256,
                "agent_sha256": manifest_agent_sha256,
                "agent_runtime": _agent_runtime_manifest(agent),
                "solver_profile": solver_profile.value,
                "gateway": gateway_manifest,
                "seed_base": suite.base_seed,
                "repeat": suite.repeat,
                "workers": worker_count,
                "scoring": {
                    "score_range": [0, 100],
                    "success_source": "task_verifier_or_benchmark_evaluator",
                    "pass_at_k": suite.pass_k,
                },
                "tasks": task_manifest,
            },
            "runs": [self._report_attempt(attempt) for attempt in attempts],
        }
        (suite_dir / "result.json").write_text(
            json.dumps(summary, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        return summary
