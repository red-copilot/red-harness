"""Ablation statistics must be grounded in persisted benchmark artifacts."""

import json

from harness.benchmark.ablation import compare_solver_profiles


def write_run(root, run_id, profile, case, seed, success, cost):
    folder = root / run_id
    folder.mkdir()
    (folder / "result.json").write_text(json.dumps({
        "run_id": run_id,
        "benchmark": {"type": "tsec", "case_id": case},
        "success": success,
        "metrics": {"duration_ms": 1000, "cost_usd": cost},
    }), encoding="utf-8")
    (folder / "trace.jsonl").write_text(json.dumps({
        "type": "run.started",
        "data": {"solver_profile": profile, "seed": seed},
    }) + "\n", encoding="utf-8")


def test_profile_comparison_uses_observed_runs_and_case_seed_pairs(tmp_path):
    write_run(tmp_path, "a", "pi-only", "case-1", 1, False, 0.1)
    write_run(tmp_path, "b", "pi-world", "case-1", 1, True, 0.2)
    write_run(tmp_path, "c", "pi-only", "case-2", 2, True, 0.3)
    report = compare_solver_profiles(tmp_path)
    assert report["included_runs"] == 3
    assert report["profiles"]["pi-only"]["runs"] == 2
    assert report["profiles"]["pi-only"]["success_rate"] == 0.5
    assert report["profiles"]["pi-world"]["success_rate"] == 1.0
    assert len(report["paired_cases"]) == 1
    assert report["paired_cases"][0]["case_id"] == "case-1"


def test_missing_profile_data_is_not_guessed(tmp_path):
    run = tmp_path / "legacy"
    run.mkdir()
    (run / "result.json").write_text('{"success":true}', encoding="utf-8")
    assert compare_solver_profiles(tmp_path) == {
        "profiles": {}, "paired_cases": [], "included_runs": 0,
    }
