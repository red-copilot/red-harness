from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

from harness.cli import app
from harness.evaluation import compare_suite_reports


def _report(*, agent_hash: str, success: float = 0.5, false_positives: int = 0) -> dict:
    return {
        "schema_version": "harness/evaluation-report/v1",
        "evaluation_manifest": {
            "schema_version": "harness/evaluation-manifest/v1",
            "suite_sha256": "suite-hash",
            "agent_sha256": agent_hash,
            "agent_runtime": {
                "type": "cli",
                "image": None,
                "image_id": None,
                "runtime": None,
                "pi_binary": None,
                "pi_mode": None,
                "model_provider": None,
                "model": None,
            },
            "seed_base": 40,
            "repeat": 4,
            "scoring": {"score_range": [0, 100], "pass_at_k": [1, 2]},
            "tasks": [{"task_id": "web-1", "task_sha256": "task-hash", "budgets": {}}],
        },
        "domains": {
            "web": {
                "weighted_success_rate": success,
                "weighted_score": success * 100,
                "pass_at_k": {"1": success, "2": min(1.0, success + 0.25)},
                "metrics": {
                    "attempts": 4,
                    "total_duration_ms": 0,
                    "total_input_tokens": 0,
                    "total_output_tokens": 0,
                    "total_tokens": 0,
                    "total_tool_calls": 0,
                    "total_model_calls": 0,
                    "total_cost_usd": 0,
                    "total_replans": 0,
                    "failed_tool_calls": 0,
                    "objective_verdicts": 4,
                    "false_positive_verifications": false_positives,
                    "unverified_completion_claims": 0,
                },
            }
        },
    }


def test_comparison_accepts_same_fixtures_and_reports_per_domain_deltas() -> None:
    baseline = _report(agent_hash="agent-a", success=0.5)
    candidate = _report(agent_hash="agent-b", success=0.75)

    comparison = compare_suite_reports(baseline, candidate)

    assert comparison["passed"] is True
    assert comparison["domains"]["web"]["success_rate_delta"] == 0.25
    assert comparison["domains"]["web"]["score_delta"] == 25
    assert comparison["regressions"] == []


def test_comparison_records_solver_profile_ablation() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-a")
    baseline["evaluation_manifest"]["solver_profile"] = "pi-world"
    candidate["evaluation_manifest"]["solver_profile"] = "pi-world-heuristic"

    comparison = compare_suite_reports(baseline, candidate)

    assert comparison["passed"] is True
    assert comparison["solver_profile_comparison"] == {
        "baseline": "pi-world",
        "candidate": "pi-world-heuristic",
    }


def test_comparison_reports_host_toolchain_drift_without_rejecting_same_experiment() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-b")
    baseline["evaluation_manifest"]["agent_runtime"]["toolchain"] = {
        "python_version": "3.11.0rc1",
        "platform": "Linux-baseline",
        "cli_executable_sha256": "a" * 64,
    }
    candidate["evaluation_manifest"]["agent_runtime"]["toolchain"] = {
        "python_version": "3.11.9",
        "platform": "Linux-ci",
        "cli_executable_sha256": "b" * 64,
    }

    comparison = compare_suite_reports(baseline, candidate)

    assert comparison["passed"] is True
    assert comparison["toolchain_comparison"] == {
        "same": False,
        "changed_fields": [
            "cli_executable_sha256",
            "platform",
            "python_version",
        ],
    }


def test_comparison_rejects_unknown_solver_profile() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-b")
    candidate["evaluation_manifest"]["solver_profile"] = "pi-world-model"

    with pytest.raises(ValueError, match="invalid solver profile"):
        compare_suite_reports(baseline, candidate)


def test_comparison_reports_output_artifact_variance_without_exposing_paths() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-b")
    baseline["runs"] = [
        {
            "task_id": "web-1", "category": "web", "seed": 40,
            "output_artifacts": [
                {"path_sha256": "a" * 64, "sha256": "b" * 64, "size_bytes": 5}
            ],
        }
    ]
    candidate["runs"] = [
        {
            "task_id": "web-1", "category": "web", "seed": 40,
            "output_artifacts": [
                {"path_sha256": "a" * 64, "sha256": "c" * 64, "size_bytes": 6},
                {"path_sha256": "d" * 64, "sha256": "e" * 64, "size_bytes": 4},
            ],
        }
    ]

    comparison = compare_suite_reports(baseline, candidate)

    assert comparison["artifact_comparison"] == {
        "comparable_attempts": 1,
        "unchanged_attempts": 0,
        "changed_attempts": 1,
        "unmatched_attempts": 0,
        "differences": [
            {
                "domain": "web",
                "seed": 40,
                "added_path_hashes": ["d" * 64],
                "removed_path_hashes": [],
                "changed_path_hashes": ["a" * 64],
            }
        ],
    }
    assert "input.txt" not in str(comparison)


def test_comparison_rejects_duplicate_artifact_attempt_identity() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-b")
    baseline["runs"] = [
        {"task_id": "web-1", "category": "web", "seed": 40, "output_artifacts": []},
        {"task_id": "web-1", "category": "web", "seed": 40, "output_artifacts": []},
    ]
    candidate["runs"] = []

    with pytest.raises(ValueError, match="duplicate attempt identity"):
        compare_suite_reports(baseline, candidate)


def test_comparison_blocks_reports_with_missing_seed_attempts() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-b")
    baseline["domains"]["web"]["metrics"]["attempts"] = 4
    candidate["domains"]["web"]["metrics"]["attempts"] = 3
    candidate["domains"]["web"]["metrics"]["objective_verdicts"] = 3

    comparison = compare_suite_reports(baseline, candidate)

    assert comparison["passed"] is False
    assert {item["code"] for item in comparison["regressions"]} == {
        "attempt_count_mismatch",
        "objective_verdict_coverage_regression",
    }


def test_comparison_rejects_run_report_missing_seed_records() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-b")
    for report in (baseline, candidate):
        report["suite_run_id"] = "suite-run"
        report["attempts"] = 4
        report["evaluation_manifest"]["tasks"] = [
            {"task_id": "web-1", "category": "web", "seeds": [40, 41, 42, 43]}
        ]
    candidate["runs"] = []

    with pytest.raises(ValueError, match="frozen task seed schedule"):
        compare_suite_reports(baseline, candidate)


def test_comparison_rejects_run_report_missing_domain_summary() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-b")
    for report in (baseline, candidate):
        report["suite_run_id"] = "suite-run"
        report["attempts"] = 4
        report["evaluation_manifest"]["tasks"] = [
            {"task_id": "web-1", "category": "web", "seeds": [40, 41, 42, 43]}
        ]
        report["runs"] = [
            {"task_id": "web-1", "category": "web", "seed": seed, "output_artifacts": []}
            for seed in (40, 41, 42, 43)
        ]
    del candidate["domains"]["web"]

    with pytest.raises(ValueError, match="domains do not match"):
        compare_suite_reports(baseline, candidate)


def test_comparison_blocks_quality_and_verification_regressions() -> None:
    baseline = _report(agent_hash="agent-a", success=0.75)
    candidate = _report(agent_hash="agent-b", success=0.25, false_positives=1)

    comparison = compare_suite_reports(
        baseline,
        candidate,
        max_success_rate_drop=0.1,
    )

    assert comparison["passed"] is False
    assert {item["code"] for item in comparison["regressions"]} == {
        "success_rate_regression",
        "score_regression",
        "pass_at_k_regression",
        "false_positive_verification_increase",
    }


def test_comparison_rejects_changed_frozen_manifest() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = copy.deepcopy(baseline)
    candidate["evaluation_manifest"]["seed_base"] = 41

    with pytest.raises(ValueError, match="frozen evaluation manifest"):
        compare_suite_reports(baseline, candidate)


def test_comparison_rejects_changed_gateway_pricing() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-b")
    baseline["evaluation_manifest"]["gateway"] = {
        "input_price_per_million_usd": 1.0,
        "output_price_per_million_usd": 2.0,
    }
    candidate["evaluation_manifest"]["gateway"] = {
        "input_price_per_million_usd": 2.0,
        "output_price_per_million_usd": 2.0,
    }

    with pytest.raises(ValueError, match="frozen evaluation manifest"):
        compare_suite_reports(baseline, candidate)


def test_comparison_rejects_missing_metrics_and_malformed_reports() -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-b")
    del candidate["domains"]["web"]["metrics"]

    with pytest.raises(TypeError, match="metrics"):
        compare_suite_reports(baseline, candidate)

    with pytest.raises(ValueError, match="report schema"):
        compare_suite_reports({}, candidate)


@pytest.mark.parametrize(
    ("path", "value"),
    [
        (("weighted_success_rate",), 1.01),
        (("weighted_success_rate",), -0.01),
        (("weighted_score",), 101),
        (("pass_at_k", "1"), 1.01),
        (("pass_at_k", "2"), -0.01),
        (("metrics", "attempts"), 4.5),
        (("metrics", "attempts"), True),
        (("metrics", "objective_verdicts"), 5),
    ],
)
def test_comparison_rejects_out_of_contract_domain_values(path, value) -> None:
    baseline = _report(agent_hash="agent-a")
    candidate = _report(agent_hash="agent-b")
    target = candidate["domains"]["web"]
    for key in path[:-1]:
        target = target[key]
    target[path[-1]] = value

    with pytest.raises((ValueError, TypeError)):
        compare_suite_reports(baseline, candidate)


def test_compare_evaluations_cli_returns_failure_for_regression(tmp_path: Path) -> None:
    baseline_path = tmp_path / "baseline.json"
    candidate_path = tmp_path / "candidate.json"
    baseline_path.write_text(json.dumps(_report(agent_hash="agent-a")), encoding="utf-8")
    candidate_path.write_text(
        json.dumps(_report(agent_hash="agent-b", success=0.25, false_positives=1)),
        encoding="utf-8",
    )

    result = CliRunner().invoke(app, ["compare-evaluations", str(baseline_path), str(candidate_path)])

    assert result.exit_code == 1
    assert '"passed": false' in result.output


def test_compare_evaluations_cli_rejects_symlink_input(tmp_path: Path) -> None:
    baseline_path = tmp_path / "baseline.json"
    candidate_path = tmp_path / "candidate.json"
    baseline_path.write_text(json.dumps(_report(agent_hash="agent-a")), encoding="utf-8")
    candidate_path.write_text(json.dumps(_report(agent_hash="agent-b")), encoding="utf-8")
    link_path = tmp_path / "linked-candidate.json"
    link_path.symlink_to(candidate_path)

    result = CliRunner().invoke(app, ["compare-evaluations", str(baseline_path), str(link_path)])

    assert result.exit_code != 0
    assert "symbolic link" in result.output or "symlink" in result.output
