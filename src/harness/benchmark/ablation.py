"""Compare actual solver profiles using saved benchmark results.

No invented success metrics: only existing result.json files are counted.
"""

from __future__ import annotations

import json
from collections import defaultdict
from pathlib import Path


def compare_solver_profiles(runs_root: Path) -> dict:
    """Summarize outcomes per profile; pair only matching benchmark cases/seeds."""
    records = []
    for path in sorted(runs_root.glob("*/result.json")):
        result = json.loads(path.read_text(encoding="utf-8"))
        started_path = path.parent / "trace.jsonl"
        profile = None
        seed = None
        if started_path.exists():
            for line in started_path.read_text(encoding="utf-8").splitlines():
                event = json.loads(line)
                if event.get("type") == "run.started":
                    data = event.get("data") or {}
                    profile = data.get("solver_profile")
                    seed = data.get("seed")
                    break
        case = result.get("benchmark") or {}
        if not profile or not case.get("case_id") or seed is None:
            continue
        metrics = result.get("metrics") or {}
        records.append({
            "profile": profile,
            "case_id": case["case_id"],
            "benchmark": case.get("type"),
            "seed": seed,
            "success": bool(result.get("success")),
            "duration_ms": metrics.get("duration_ms"),
            "cost_usd": metrics.get("cost_usd"),
            "run_id": result.get("run_id"),
        })

    groups = defaultdict(list)
    for record in records:
        groups[record["profile"]].append(record)
    profiles = {}
    for profile, entries in sorted(groups.items()):
        duration = [r["duration_ms"] for r in entries if isinstance(r["duration_ms"], (int, float))]
        cost = [r["cost_usd"] for r in entries if isinstance(r["cost_usd"], (int, float))]
        profiles[profile] = {
            "runs": len(entries),
            "successes": sum(r["success"] for r in entries),
            "success_rate": sum(r["success"] for r in entries) / len(entries),
            "mean_duration_ms": sum(duration) / len(duration) if duration else None,
            "mean_cost_usd": sum(cost) / len(cost) if cost else None,
        }
    cases = defaultdict(dict)
    for record in records:
        key = (record["benchmark"], record["case_id"], record["seed"])
        cases[key][record["profile"]] = record["success"]
    paired = [
        {"benchmark": benchmark, "case_id": case_id, "seed": seed, "outcomes": outcomes}
        for (benchmark, case_id, seed), outcomes in sorted(cases.items(), key=str)
        if len(outcomes) > 1
    ]
    return {"profiles": profiles, "paired_cases": paired, "included_runs": len(records)}
