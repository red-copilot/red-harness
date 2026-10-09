"""Frozen-suite A/B comparison and safety regression gates."""
from __future__ import annotations

import math
from typing import Any

_MANIFEST_FIELDS = (
    "schema_version",
    "suite_sha256",
    "seed_base",
    "repeat",
    "scoring",
    "tasks",
    "agent_runtime",
    "gateway",
)
_METRICS = (
    "attempts",
    "total_duration_ms",
    "total_input_tokens",
    "total_output_tokens",
    "total_tokens",
    "total_tool_calls",
    "total_model_calls",
    "total_cost_usd",
    "total_replans",
    "failed_tool_calls",
    "objective_verdicts",
    "false_positive_verifications",
    "unverified_completion_claims",
)
_COUNT_METRICS = {
    "attempts",
    "total_input_tokens",
    "total_output_tokens",
    "total_tokens",
    "total_tool_calls",
    "total_model_calls",
    "total_replans",
    "failed_tool_calls",
    "objective_verdicts",
    "false_positive_verifications",
    "unverified_completion_claims",
}
_HOST_TOOLCHAIN_FIELDS = (
    "cli_executable_sha256",
    "platform",
    "python_implementation",
    "python_version",
)
_SOLVER_PROFILES = {"pi-only", "pi-world", "pi-world-heuristic"}


def _number(value: Any, *, field: str, minimum: float | None = None) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise TypeError(f"invalid numeric field: {field}")
    result = float(value)
    if not math.isfinite(result) or (minimum is not None and result < minimum):
        raise ValueError(f"invalid numeric field: {field}")
    return result


def _bounded_rate(value: Any, *, field: str) -> float:
    result = _number(value, field=field, minimum=0)
    if result > 1:
        raise ValueError(f"invalid evaluation domain value: {field}")
    return result


def _validate_metric_values(metrics: dict[str, Any], *, domain: str) -> dict[str, float]:
    values: dict[str, float] = {}
    for key in _METRICS:
        field = f"{domain}.metrics.{key}"
        value = metrics.get(key)
        if key in _COUNT_METRICS and (isinstance(value, bool) or not isinstance(value, int)):
            raise ValueError(f"invalid evaluation domain value: {field}")
        values[key] = _number(value, field=field, minimum=0)
    attempts = values["attempts"]
    for key in ("objective_verdicts", "false_positive_verifications", "unverified_completion_claims"):
        if values[key] > attempts:
            raise ValueError(f"invalid evaluation domain value: {domain}.metrics.{key}")
    return values


def _runtime_contract_and_toolchain(runtime: Any) -> tuple[dict[str, Any], dict[str, Any]]:
    if not isinstance(runtime, dict):
        raise TypeError("evaluation agent_runtime must be an object")
    contract = dict(runtime)
    raw_toolchain = runtime.get("toolchain")
    if raw_toolchain is None:
        contract.pop("toolchain", None)
        return contract, {}
    if not isinstance(raw_toolchain, dict):
        raise TypeError("evaluation agent_runtime.toolchain must be an object")
    toolchain = dict(raw_toolchain)
    drift_fields = {key: toolchain.pop(key) for key in _HOST_TOOLCHAIN_FIELDS if key in toolchain}
    if toolchain:
        contract["toolchain"] = toolchain
    else:
        contract.pop("toolchain", None)
    return contract, drift_fields


def _attempt_artifacts(report: dict[str, Any]) -> dict[tuple[str, str, int], dict[str, tuple[str, int]]]:
    runs = report.get("runs")
    if runs is None:
        return {}
    if not isinstance(runs, list):
        raise TypeError("evaluation runs must be a list")
    indexed: dict[tuple[str, str, int], dict[str, tuple[str, int]]] = {}
    for attempt in runs:
        if not isinstance(attempt, dict):
            raise TypeError("evaluation attempt must be an object")
        task_id = attempt.get("task_id")
        domain = attempt.get("category")
        seed = attempt.get("seed")
        if (
            not isinstance(task_id, str)
            or not task_id
            or not isinstance(domain, str)
            or not domain
            or isinstance(seed, bool)
            or not isinstance(seed, int)
            or seed < 0
        ):
            raise ValueError("invalid evaluation attempt identity")
        identity = (domain, task_id, seed)
        if identity in indexed:
            raise ValueError("duplicate attempt identity in evaluation report")
        raw_artifacts = attempt.get("output_artifacts")
        if not isinstance(raw_artifacts, list):
            raise TypeError("evaluation attempt output_artifacts must be a list")
        artifacts: dict[str, tuple[str, int]] = {}
        for artifact in raw_artifacts:
            if not isinstance(artifact, dict):
                raise TypeError("output artifact must be an object")
            path_hash = artifact.get("path_sha256")
            content_hash = artifact.get("sha256")
            size = artifact.get("size_bytes")
            if (
                not isinstance(path_hash, str)
                or len(path_hash) != 64
                or any(char not in "0123456789abcdef" for char in path_hash)
                or not isinstance(content_hash, str)
                or len(content_hash) != 64
                or any(char not in "0123456789abcdef" for char in content_hash)
                or isinstance(size, bool)
                or not isinstance(size, int)
                or size < 0
            ):
                raise ValueError("invalid output artifact fingerprint")
            if path_hash in artifacts:
                raise ValueError("duplicate output artifact path fingerprint")
            artifacts[path_hash] = (content_hash, size)
        indexed[identity] = artifacts
    return indexed


def _compare_attempt_artifacts(
    baseline: dict[str, Any], candidate: dict[str, Any]
) -> dict[str, Any]:
    before = _attempt_artifacts(baseline)
    after = _attempt_artifacts(candidate)
    before_keys = set(before)
    after_keys = set(after)
    shared = before_keys & after_keys
    differences: list[dict[str, Any]] = []
    unchanged = 0
    changed = 0
    for domain, _task_id, seed in sorted(shared):
        old = before[(domain, _task_id, seed)]
        new = after[(domain, _task_id, seed)]
        old_paths = set(old)
        new_paths = set(new)
        added = sorted(new_paths - old_paths)
        removed = sorted(old_paths - new_paths)
        changed_paths = sorted(
            path for path in old_paths & new_paths if old[path] != new[path]
        )
        if added or removed or changed_paths:
            changed += 1
            if len(differences) < 1000:
                differences.append(
                    {
                        "domain": domain,
                        "seed": seed,
                        "added_path_hashes": added,
                        "removed_path_hashes": removed,
                        "changed_path_hashes": changed_paths,
                    }
                )
        else:
            unchanged += 1
    return {
        "comparable_attempts": len(shared),
        "unchanged_attempts": unchanged,
        "changed_attempts": changed,
        "unmatched_attempts": len(before_keys ^ after_keys),
        "differences": differences,
    }


def _validate_run_seed_schedule(report: dict[str, Any], manifest: dict[str, Any]) -> None:
    """Require persisted attempt records for every frozen task/seed pair."""
    if not report.get("suite_run_id"):
        return  # Lightweight comparison fixtures may omit run-level records.
    expected: set[tuple[str, str, int]] = set()
    expected_domains: set[str] = set()
    tasks = manifest.get("tasks")
    if not isinstance(tasks, list):
        raise TypeError("evaluation manifest tasks must be a list")
    for task in tasks:
        if not isinstance(task, dict):
            raise TypeError("evaluation manifest task must be an object")
        task_id = task.get("task_id")
        category = task.get("category")
        seeds = task.get("seeds")
        if not isinstance(task_id, str) or not isinstance(category, str) or not isinstance(seeds, list):
            raise TypeError("invalid frozen task seed schedule")
        if not task_id or not category:
            raise ValueError("invalid frozen task seed schedule")
        expected_domains.add(category)
        for seed in seeds:
            if isinstance(seed, bool) or not isinstance(seed, int) or seed < 0:
                raise ValueError("invalid frozen task seed schedule")
            identity = (category, task_id, seed)
            if identity in expected:
                raise ValueError("duplicate attempt identity in frozen task seed schedule")
            expected.add(identity)
    actual = set(_attempt_artifacts(report))
    attempt_count = report.get("attempts")
    if (
        isinstance(attempt_count, bool)
        or not isinstance(attempt_count, int)
        or attempt_count != len(expected)
        or actual != expected
    ):
        raise ValueError("evaluation runs do not match frozen task seed schedule")
    domains = report.get("domains")
    if not isinstance(domains, dict) or set(domains) != expected_domains:
        raise ValueError("evaluation domains do not match frozen task schedule")


def compare_suite_reports(
    baseline: dict[str, Any],
    candidate: dict[str, Any],
    *,
    max_success_rate_drop: float = 0.0,
    max_score_drop: float = 0.0,
) -> dict[str, Any]:
    """Compare two reports from the same frozen suite and flag regressions.

    Agent hashes may differ by design. Suite/task hashes, task budgets,
    evaluator settings, scoring contract, repetition count and seeds must match.
    Safety metrics are never covered by the quality tolerances.
    """
    max_success_rate_drop = _number(
        max_success_rate_drop, field="max_success_rate_drop", minimum=0
    )
    max_score_drop = _number(max_score_drop, field="max_score_drop", minimum=0)
    if (
        baseline.get("schema_version") != "harness/evaluation-report/v1"
        or candidate.get("schema_version") != "harness/evaluation-report/v1"
    ):
        raise ValueError("unsupported evaluation report schema")
    baseline_manifest = baseline.get("evaluation_manifest")
    candidate_manifest = candidate.get("evaluation_manifest")
    if not isinstance(baseline_manifest, dict) or not isinstance(candidate_manifest, dict):
        raise TypeError("both reports require an evaluation manifest")
    baseline_contract = {key: baseline_manifest.get(key) for key in _MANIFEST_FIELDS}
    candidate_contract = {key: candidate_manifest.get(key) for key in _MANIFEST_FIELDS}
    baseline_runtime, baseline_toolchain = _runtime_contract_and_toolchain(
        baseline_contract.get("agent_runtime")
    )
    candidate_runtime, candidate_toolchain = _runtime_contract_and_toolchain(
        candidate_contract.get("agent_runtime")
    )
    baseline_contract["agent_runtime"] = baseline_runtime
    candidate_contract["agent_runtime"] = candidate_runtime
    changed_toolchain_fields = sorted(
        key
        for key in _HOST_TOOLCHAIN_FIELDS
        if baseline_toolchain.get(key) != candidate_toolchain.get(key)
    )
    baseline_profile = baseline_manifest.get("solver_profile", "pi-world-heuristic")
    candidate_profile = candidate_manifest.get("solver_profile", "pi-world-heuristic")
    if (
        not isinstance(baseline_profile, str)
        or baseline_profile not in _SOLVER_PROFILES
        or not isinstance(candidate_profile, str)
        or candidate_profile not in _SOLVER_PROFILES
    ):
        raise ValueError("evaluation report has an invalid solver profile")
    if (
        baseline_contract["schema_version"] != "harness/evaluation-manifest/v1"
        or candidate_contract["schema_version"] != "harness/evaluation-manifest/v1"
        or baseline_contract != candidate_contract
    ):
        raise ValueError("baseline and candidate must use the same frozen evaluation manifest")

    _validate_run_seed_schedule(baseline, baseline_manifest)
    _validate_run_seed_schedule(candidate, candidate_manifest)

    baseline_domains = baseline.get("domains")
    candidate_domains = candidate.get("domains")
    if not isinstance(baseline_domains, dict) or not isinstance(candidate_domains, dict):
        raise TypeError("both reports require domain summaries")
    if set(baseline_domains) != set(candidate_domains):
        raise ValueError("baseline and candidate domain summaries do not match")

    artifact_comparison = _compare_attempt_artifacts(baseline, candidate)

    regressions: list[dict[str, Any]] = []
    domains: dict[str, dict[str, Any]] = {}
    expected_attempts: dict[str, int] = {}
    manifest_tasks = baseline_manifest.get("tasks")
    repeat_count = baseline_manifest.get("repeat")
    scoring = baseline_manifest.get("scoring")
    score_range = scoring.get("score_range") if isinstance(scoring, dict) else None
    if not isinstance(score_range, list) or len(score_range) != 2:
        raise ValueError("invalid evaluation domain value: scoring.score_range")
    score_min = _number(score_range[0], field="scoring.score_range.minimum")
    score_max = _number(score_range[1], field="scoring.score_range.maximum")
    if score_max < score_min:
        raise ValueError("invalid evaluation domain value: scoring.score_range")
    if isinstance(manifest_tasks, list) and isinstance(repeat_count, int) and not isinstance(repeat_count, bool):
        for task in manifest_tasks:
            if isinstance(task, dict) and isinstance(task.get("category"), str):
                category = task["category"]
                expected_attempts[category] = expected_attempts.get(category, 0) + repeat_count
    for domain in sorted(baseline_domains):
        before = baseline_domains[domain]
        after = candidate_domains[domain]
        if not isinstance(before, dict) or not isinstance(after, dict):
            raise TypeError(f"invalid domain summary: {domain}")

        before_success = _bounded_rate(
            before.get("weighted_success_rate"), field=f"{domain}.weighted_success_rate"
        )
        after_success = _bounded_rate(
            after.get("weighted_success_rate"), field=f"{domain}.weighted_success_rate"
        )
        before_score = _number(
            before.get("weighted_score"), field=f"{domain}.weighted_score", minimum=0
        )
        after_score = _number(
            after.get("weighted_score"), field=f"{domain}.weighted_score", minimum=0
        )
        if not score_min <= before_score <= score_max or not score_min <= after_score <= score_max:
            raise ValueError(f"invalid evaluation domain value: {domain}.weighted_score")
        success_delta = after_success - before_success
        score_delta = after_score - before_score
        if success_delta < -max_success_rate_drop:
            regressions.append(
                {"code": "success_rate_regression", "domain": domain, "delta": success_delta}
            )
        if score_delta < -max_score_drop:
            regressions.append(
                {"code": "score_regression", "domain": domain, "delta": score_delta}
            )

        before_pass = before.get("pass_at_k")
        after_pass = after.get("pass_at_k")
        if not isinstance(before_pass, dict) or not isinstance(after_pass, dict):
            raise TypeError(f"invalid pass_at_k summary for domain {domain}")
        if set(before_pass) != set(after_pass):
            raise ValueError(f"pass_at_k values do not match for domain {domain}")
        pass_deltas: dict[str, float] = {}
        for k in sorted(before_pass, key=lambda item: int(item)):
            after_rate = _bounded_rate(after_pass[k], field=f"{domain}.pass_at_k.{k}")
            before_rate = _bounded_rate(before_pass[k], field=f"{domain}.pass_at_k.{k}")
            delta = after_rate - before_rate
            pass_deltas[k] = delta
            if delta < -max_success_rate_drop:
                regressions.append(
                    {"code": "pass_at_k_regression", "domain": domain, "k": int(k), "delta": delta}
                )

        before_metrics = before.get("metrics")
        after_metrics = after.get("metrics")
        if not isinstance(before_metrics, dict) or not isinstance(after_metrics, dict):
            raise TypeError(f"missing metrics for domain {domain}")
        before_values = _validate_metric_values(before_metrics, domain=domain)
        after_values = _validate_metric_values(after_metrics, domain=domain)
        metric_deltas = {
            key: after_values[key] - before_values[key] for key in _METRICS
        }
        if metric_deltas["attempts"] != 0 or (
            domain in expected_attempts
            and (
                before_metrics["attempts"] != expected_attempts[domain]
                or after_metrics["attempts"] != expected_attempts[domain]
            )
        ):
            regressions.append(
                {
                    "code": "attempt_count_mismatch",
                    "domain": domain,
                    "baseline": before_metrics["attempts"],
                    "candidate": after_metrics["attempts"],
                    "expected": expected_attempts.get(domain),
                }
            )
        if metric_deltas["objective_verdicts"] < 0:
            regressions.append(
                {
                    "code": "objective_verdict_coverage_regression",
                    "domain": domain,
                    "delta": metric_deltas["objective_verdicts"],
                }
            )
        if metric_deltas["false_positive_verifications"] > 0:
            regressions.append(
                {
                    "code": "false_positive_verification_increase",
                    "domain": domain,
                    "delta": metric_deltas["false_positive_verifications"],
                }
            )

        domains[domain] = {
            "baseline": {
                "success_rate": before_success,
                "score": before_score,
                "pass_at_k": before_pass,
                "metrics": before_metrics,
            },
            "candidate": {
                "success_rate": after_success,
                "score": after_score,
                "pass_at_k": after_pass,
                "metrics": after_metrics,
            },
            "success_rate_delta": success_delta,
            "score_delta": score_delta,
            "pass_at_k_delta": pass_deltas,
            "metric_delta": metric_deltas,
        }

    return {
        "schema_version": "harness/evaluation-comparison/v1",
        "passed": not regressions,
        "baseline_agent_sha256": baseline_manifest.get("agent_sha256"),
        "candidate_agent_sha256": candidate_manifest.get("agent_sha256"),
        "solver_profile_comparison": {
            "baseline": baseline_profile,
            "candidate": candidate_profile,
        },
        "toolchain_comparison": {
            "same": not changed_toolchain_fields,
            "changed_fields": changed_toolchain_fields,
        },
        "domains": domains,
        "artifact_comparison": artifact_comparison,
        "regressions": regressions,
    }
