#!/usr/bin/env python3
"""Validate experiment result records and summarize observed outcomes.

Input: JSON array of per-attempt objects. Refuses silent missing/duplicate trials.
No success-rate output from unimplemented experiment arms.
"""
from __future__ import annotations

import argparse
import json
import statistics
from pathlib import Path

ARMS = {"01-native-pi", "02-minimal", "03-world", "04-collaboration"}
REQUIRED = {"arm", "challenge_id", "seed", "status", "verified_success",
            "verified_flags", "awarded_score", "elapsed_seconds",
            "input_tokens", "output_tokens", "model_calls", "tool_calls",
            "cost_usd", "first_flag_seconds", "hint_used", "initial_correct"}


def summarize(rows: list[dict]) -> dict:
    if not rows:
        raise ValueError("empty results: do not report a success rate")
    seen = set()
    grouped = {arm: [] for arm in sorted(ARMS)}
    for i, row in enumerate(rows):
        if not isinstance(row, dict) or missing := (REQUIRED - row.keys()):
            raise ValueError(f"row {i}: missing fields or invalid record: {sorted(missing) if isinstance(row, dict) else 'not an object'}")
        if row["arm"] not in ARMS:
            raise ValueError(f"row {i}: unknown experiment arm")
        if not isinstance(row["challenge_id"], str) or not row["challenge_id"]:
            raise ValueError(f"row {i}: invalid challenge_id")
        if type(row["seed"]) is not int:
            raise ValueError(f"row {i}: seed must be integer")
        key = (row["arm"], row["challenge_id"], row["seed"])
        if key in seen:
            raise ValueError(f"duplicate trial: {key}")
        seen.add(key)
        if row["status"] not in {"completed", "unsolved", "timeout", "budget",
                                 "model_error", "infra_error", "verifier_unavailable",
                                 "target_unavailable"}:
            raise ValueError(f"row {i}: invalid status")
        if row["verified_success"] is not None and type(row["verified_success"]) is not bool:
            raise ValueError(f"row {i}: verified_success must be bool or null")
        if row["status"] == "completed" and row["verified_success"] is not True:
            raise ValueError(f"row {i}: completed requires independently verified success")
        if row["status"] in {"verifier_unavailable", "infra_error", "target_unavailable"} and row["verified_success"] is not None:
            raise ValueError(f"row {i}: non-evaluable run must use null success")
        if type(row["initial_correct"]) is not int or row["initial_correct"] < 0:
            raise ValueError(f"row {i}: invalid initial_correct")
        if type(row["hint_used"]) is not bool:
            raise ValueError(f"row {i}: hint_used must be bool")
        for field in ("verified_flags", "awarded_score", "elapsed_seconds",
                      "input_tokens", "output_tokens", "model_calls", "tool_calls",
                      "cost_usd", "first_flag_seconds"):
            value = row[field]
            if value is not None and (type(value) not in (int, float) or value < 0):
                raise ValueError(f"row {i}: invalid {field}")
        grouped[row["arm"]].append(row)
    output = {}
    for arm, group in grouped.items():
        if not group:
            continue
        eligible = [r for r in group if r["initial_correct"] == 0 and not r["hint_used"]
                    and r["verified_success"] is not None]
        output[arm] = {
            "attempts": len(group), "eligible": len(eligible),
            "excluded_or_censored": len(group) - len(eligible),
            "verified_success_rate": (sum(r["verified_success"] for r in eligible) / len(eligible)
                                      if eligible else None),
            "mean_awarded_score": (statistics.mean(r["awarded_score"] for r in eligible
                                                  if r["awarded_score"] is not None)
                                   if any(r["awarded_score"] is not None for r in eligible)
                                   else None)
        }
    baseline = { (r["challenge_id"],r["seed"]): r for r in grouped["01-native-pi"]
                if r["initial_correct"] == 0 and not r["hint_used"]
                and r["verified_success"] is not None }
    for arm in ("02-minimal", "03-world", "04-collaboration"):
        if arm not in output:
            continue
        paired = [(r,baseline[(r["challenge_id"],r["seed"])])
                  for r in grouped[arm] if (r["challenge_id"],r["seed"]) in baseline
                  and r["initial_correct"] == 0 and not r["hint_used"]
                  and r["verified_success"] is not None]
        output[arm]["paired_trials"] = len(paired)
        output[arm]["paired_success_delta"] = (
            sum(int(r["verified_success"]) - int(b["verified_success"])
                for r,b in paired) / len(paired) if paired else None)
    return {"schema_version":"pi-ablation-summary/v1", "arms":output,
            "note":"Descriptive only: no confidence interval; challenge-cluster inference required."}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("results", type=Path)
    args = parser.parse_args()
    rows = json.loads(args.results.read_text(encoding="utf-8"))
    if not isinstance(rows, list):
        parser.error("results must be a JSON array")
    print(json.dumps(summarize(rows), indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
