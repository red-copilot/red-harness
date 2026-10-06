from __future__ import annotations

import json
import uuid
from datetime import UTC, datetime
from pathlib import Path

from .models import AgentSpec, SuiteSpec, load_task
from .orchestrator import Orchestrator


def _suite_run_id() -> str:
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    return f"suite_{stamp}_{uuid.uuid4().hex[:8]}"


class SuiteRunner:
    def __init__(
        self,
        *,
        runs_root: str | Path = ".redharness/runs",
        suites_root: str | Path = ".redharness/suites",
    ) -> None:
        self.orchestrator = Orchestrator(runs_root)
        self.suites_root = Path(suites_root)

    def run(
        self,
        *,
        suite: SuiteSpec,
        suite_path: Path,
        agent: AgentSpec,
        agent_path: Path,
        allow_host_agent: bool = False,
    ) -> dict:
        suite_id = _suite_run_id()
        suite_dir = (self.suites_root / suite_id).resolve()
        suite_dir.mkdir(parents=True, exist_ok=False)
        base_dir = suite_path.resolve().parent
        attempts: list[dict] = []

        for task_entry in suite.tasks:
            task_path = (base_dir / task_entry.path).resolve()
            task = load_task(task_path)
            for repeat_index in range(suite.repeat):
                seed = suite.base_seed + repeat_index
                result = self.orchestrator.run(
                    task=task,
                    task_path=task_path,
                    agent=agent,
                    agent_path=agent_path,
                    allow_host_agent=allow_host_agent,
                    seed=seed,
                )
                attempts.append(
                    {
                        **result,
                        "weight": task_entry.weight,
                        "repeat_index": repeat_index,
                        "seed": seed,
                    }
                )

        total_weight = sum(float(item["weight"]) for item in attempts)
        weighted_score = (
            sum(float(item["score"]) * float(item["weight"]) for item in attempts)
            / total_weight
            if total_weight
            else 0.0
        )
        successes = sum(1 for item in attempts if item["success"])
        summary = {
            "suite_run_id": suite_id,
            "suite_id": suite.id,
            "agent_id": agent.id,
            "attempts": len(attempts),
            "successes": successes,
            "success_rate": successes / len(attempts) if attempts else 0.0,
            "weighted_score": weighted_score,
            "runs": attempts,
        }
        (suite_dir / "result.json").write_text(
            json.dumps(summary, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        return summary
