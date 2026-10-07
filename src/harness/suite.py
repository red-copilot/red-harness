from __future__ import annotations

import json
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from .gateway_runtime import GatewayConfig
from .models import AgentSpec, SuiteSpec, TaskSpec, load_task
from .orchestrator import Orchestrator
from .scoring import pass_at_k, weighted_mean


def _suite_run_id() -> str:
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    return f"suite_{stamp}_{uuid.uuid4().hex[:8]}"


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
    ) -> dict[str, Any]:
        result = self.orchestrator.run(
            task=task,
            task_path=task_path,
            agent=agent,
            agent_path=agent_path,
            allow_host_agent=allow_host_agent,
            seed=seed,
            gateway_config=gateway_config,
        )
        return {
            **result,
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
                    "task_id": task_id,
                    "category": runs[0]["category"],
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
    def _domain_summaries(task_summaries: list[dict], pass_k_values: list[int]) -> dict:
        domains: dict[str, list[dict]] = {}
        for item in task_summaries:
            domains.setdefault(str(item["category"]), []).append(item)

        result: dict[str, dict] = {}
        for category, tasks in sorted(domains.items()):
            result[category] = {
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
    ) -> dict:
        suite_id = _suite_run_id()
        suite_dir = (self.suites_root / suite_id).resolve()
        suite_dir.mkdir(parents=True, exist_ok=False)
        base_dir = suite_path.resolve().parent
        worker_count = workers if workers is not None else suite.workers
        worker_count = max(1, worker_count)

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
                        "weight": task_entry.weight,
                        "repeat_index": repeat_index,
                        "seed": suite.base_seed + repeat_index,
                    }
                )
                index += 1

        indexed_results: dict[int, dict] = {}
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
                ): int(job["index"])
                for job in jobs
            }
            for future in as_completed(futures):
                indexed_results[futures[future]] = future.result()

        attempts = [indexed_results[i] for i in sorted(indexed_results)]
        successes = sum(1 for item in attempts if item["success"])
        task_summaries = self._task_summaries(attempts, suite.pass_k)
        domains = self._domain_summaries(task_summaries, suite.pass_k)

        summary = {
            "suite_run_id": suite_id,
            "suite_id": suite.id,
            "agent_id": agent.id,
            "workers": worker_count,
            "attempts": len(attempts),
            "successes": successes,
            "success_rate": successes / len(attempts) if attempts else 0.0,
            "weighted_score": weighted_mean(
                (float(item["score"]), float(item["weight"])) for item in attempts
            ),
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
            "runs": attempts,
        }
        (suite_dir / "result.json").write_text(
            json.dumps(summary, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        return summary
