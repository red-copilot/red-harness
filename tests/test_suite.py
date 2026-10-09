import hashlib
from pathlib import Path

import pytest

from harness.gateway_runtime import GatewayConfig
from harness.models import load_agent, load_suite
from harness.suite import SuiteRunner, _artifact_hashes, _bounded_file_sha256


def test_manifest_hashing_bounds_and_rejects_linked_inputs(tmp_path: Path) -> None:
    source = tmp_path / "source.yaml"
    source.write_bytes(b"apiVersion: harness/v1\n")

    assert _bounded_file_sha256(source) == hashlib.sha256(source.read_bytes()).hexdigest()
    with pytest.raises(ValueError, match="bounded regular file"):
        _bounded_file_sha256(source, max_bytes=4)

    linked = tmp_path / "linked.yaml"
    linked.symlink_to(source)
    with pytest.raises((OSError, ValueError)):
        _bounded_file_sha256(linked)


def test_manifest_hashing_rejects_file_changed_during_read(
    tmp_path: Path, monkeypatch
) -> None:
    import harness.suite as suite_module

    source = tmp_path / "source.yaml"
    source.write_bytes(b"before")
    open_file = suite_module.open_regular_file

    class MutatingReader:
        def __init__(self, inner) -> None:
            self.inner = inner
            self.mutated = False

        def __enter__(self):
            self.inner.__enter__()
            return self

        def __exit__(self, *args):
            return self.inner.__exit__(*args)

        def fileno(self):
            return self.inner.fileno()

        def read(self, size: int = -1):
            content = self.inner.read(size)
            if content and not self.mutated:
                self.mutated = True
                with source.open("ab") as writer:
                    writer.write(b" changed")
            return content

    monkeypatch.setattr(
        suite_module,
        "open_regular_file",
        lambda path, mode: MutatingReader(open_file(path, mode)),
    )

    with pytest.raises(ValueError, match="changed while hashing"):
        suite_module._bounded_file_sha256(source)


@pytest.mark.parametrize("workers", [0, 65, True, 1.5])
def test_suite_runner_rejects_invalid_worker_override_before_allocating(
    tmp_path: Path, workers
) -> None:
    suite_path = Path("benchmarks/examples/smoke-suite.yaml")
    suite = load_suite(suite_path)
    agent_path = Path("agents/examples/demo.yaml")
    suites_root = tmp_path / "suites"
    runner = SuiteRunner(runs_root=tmp_path / "runs", suites_root=suites_root)

    with pytest.raises(ValueError, match="workers must be between 1 and 64"):
        runner.run(
            suite=suite,
            suite_path=suite_path,
            agent=load_agent(agent_path),
            agent_path=agent_path,
            allow_host_agent=True,
            workers=workers,
        )

    assert not suites_root.exists()


def test_artifact_hashes_skip_symlinks_and_oversized_outputs(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    (workspace / "stable.txt").write_bytes(b"stable")
    outside = tmp_path / "outside.txt"
    outside.write_bytes(b"private")
    (workspace / "linked.txt").symlink_to(outside)
    (workspace / "oversized.txt").write_bytes(b"x" * (16 * 1024 * 1024 + 1))

    artifacts = _artifact_hashes(workspace)

    assert len(artifacts) == 1
    assert artifacts[0]["sha256"] == hashlib.sha256(b"stable").hexdigest()


def test_artifact_hashes_skip_file_changed_during_read(tmp_path: Path, monkeypatch) -> None:
    import harness.suite as suite_module

    workspace = tmp_path / "workspace"
    workspace.mkdir()
    output = workspace / "result.txt"
    output.write_bytes(b"before")
    open_file = suite_module.open_beneath

    class MutatingReader:
        def __init__(self, inner) -> None:
            self.inner = inner
            self.mutated = False

        def __enter__(self):
            self.inner.__enter__()
            return self

        def __exit__(self, *args):
            return self.inner.__exit__(*args)

        def fileno(self):
            return self.inner.fileno()

        def read(self, size: int = -1):
            content = self.inner.read(size)
            if content and not self.mutated:
                self.mutated = True
                with output.open("ab") as writer:
                    writer.write(b" changed")
            return content

    monkeypatch.setattr(
        suite_module,
        "open_beneath",
        lambda root, relative, mode: MutatingReader(open_file(root, relative, mode)),
    )

    assert _artifact_hashes(workspace) == []


def test_suite_parallel_pass_k_domain_and_seed(tmp_path: Path) -> None:
    suite_path = Path("benchmarks/examples/smoke-suite.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    summary = SuiteRunner(
        runs_root=tmp_path / "runs",
        suites_root=tmp_path / "suites",
    ).run(
        suite=load_suite(suite_path),
        suite_path=suite_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
    )

    assert summary["workers"] == 2
    assert summary["attempts"] == 2
    assert summary["successes"] == 2
    assert summary["runs"][0]["failure_class"] == "success"
    assert summary["runs"][0]["output_artifacts"][0]["sha256"]
    assert summary["weighted_score"] == 100
    assert summary["metrics"]["attempts"] == 2
    assert summary["metrics"]["total_tokens"] == 24
    assert summary["metrics"]["total_tool_calls"] == 2
    assert summary["metrics"]["false_positive_verifications"] == 0
    assert summary["pass_at_k"] == {"1": 1.0, "2": 1.0}
    assert summary["domains"]["general"]["weighted_score"] == 100
    assert summary["domains"]["general"]["failure_classes"] == {"success": 2}
    assert summary["domains"]["general"]["metrics"]["total_cost_usd"] == 0.02
    manifest = summary["evaluation_manifest"]
    assert manifest["schema_version"] == "harness/evaluation-manifest/v1"
    assert summary["schema_version"] == "harness/evaluation-report/v1"
    assert manifest["harness_version"]
    assert manifest["agent_sha256"]
    assert manifest["agent_runtime"]["type"] == "cli"
    assert manifest["agent_runtime"]["toolchain"]["python_version"]
    assert manifest["agent_runtime"]["toolchain"]["cli_executable_sha256"]
    assert manifest["seed_base"] == 1000
    assert manifest["repeat"] == 2
    assert manifest["tasks"][0]["task_path_sha256"] == hashlib.sha256(
        b"hello/task.yaml"
    ).hexdigest()
    assert manifest["tasks"][0]["seeds"] == [1000, 1001]
    assert manifest["tasks"][0]["budgets"]["wall_time"] == 30
    assert "verification_image_id" in manifest["tasks"][0]
    assert manifest["tasks"][0]["task_sha256"] == hashlib.sha256(
        Path("benchmarks/examples/hello/task.yaml").read_bytes()
    ).hexdigest()
    assert manifest["tasks"][0]["verifier_sha256"] == hashlib.sha256(
        Path("benchmarks/examples/hello/verifier.py").read_bytes()
    ).hexdigest()
    assert summary["tasks"][0]["pass_at_k"] == {"1": 1.0, "2": 1.0}
    assert [run["seed"] for run in summary["runs"]] == [1000, 1001]


def test_manifest_freezes_gateway_settings_without_exposing_credentials(
    tmp_path: Path,
) -> None:
    suite_path = Path("benchmarks/examples/smoke-suite.yaml")
    agent_path = Path("agents/examples/demo.yaml")
    policy_path = tmp_path / "policy.yaml"
    policy_bytes = b"allowed_tools: [file.read, file.write]\n"
    policy_path.write_bytes(policy_bytes)
    upstream = "https://model.example/v1?api_key=private-upstream"
    summary = SuiteRunner(
        runs_root=tmp_path / "runs",
        suites_root=tmp_path / "suites",
    ).run(
        suite=load_suite(suite_path),
        suite_path=suite_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        gateway_config=GatewayConfig(
            policy_path=policy_path,
            model_upstream=upstream,
            model_api_key="private-api-key",
            input_price_per_million_usd=1.25,
            output_price_per_million_usd=2.5,
        ),
    )

    gateway = summary["evaluation_manifest"]["gateway"]
    assert gateway["mode"] == "host"
    assert gateway["model_upstream_sha256"] == hashlib.sha256(upstream.encode()).hexdigest()
    assert gateway["model_api_key_configured"] is True
    assert gateway["input_price_per_million_usd"] == 1.25
    assert gateway["output_price_per_million_usd"] == 2.5
    assert gateway["policy_sha256"] == hashlib.sha256(policy_bytes).hexdigest()
    assert "private-api-key" not in str(summary)
    assert "private-upstream" not in str(summary)


def test_generalization_suite_reports_all_domain_metrics(
    tmp_path: Path,
) -> None:
    suite_path = Path("benchmarks/examples/generalization-smoke-suite.yaml")
    agent_path = Path("agents/examples/multidomain-demo.yaml")
    summary = SuiteRunner(
        runs_root=tmp_path / "runs",
        suites_root=tmp_path / "suites",
    ).run(
        suite=load_suite(suite_path),
        suite_path=suite_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
    )

    expected_domains = {
        "coding",
        "crypto",
        "file",
        "forensics",
        "general",
        "misc",
        "pwn",
        "reverse",
        "web",
    }
    assert summary["attempts"] == 18
    assert summary["successes"] == 18
    assert set(summary["domains"]) == expected_domains
    for domain in summary["domains"].values():
        assert domain["metrics"]["attempts"] == 2
        assert domain["metrics"]["total_replans"] == 2
        assert domain["metrics"]["failed_tool_calls"] == 0
        assert domain["metrics"]["objective_verdicts"] == 2
        assert domain["metrics"]["false_positive_verifications"] == 0
        assert domain["pass_at_k"] == {"1": 1.0, "2": 1.0}
    manifest = summary["evaluation_manifest"]
    assert manifest["seed_base"] == 2000
    assert manifest["repeat"] == 2
    assert {item["category"] for item in manifest["tasks"]} == expected_domains


def test_suite_metrics_include_cost_recovery_time_and_verification_mismatches() -> None:
    attempts = [
        {
            "success": True,
            "metrics": {
                "duration_ms": 100,
                "total_tokens": 40,
                "tool_calls": 2,
                "model_calls": 1,
                "cost_usd": 0.25,
            },
            "progress": {
                "objective_verdict": {"status": "verified"},
                "completion_claims": [],
                "replan_count": 1,
                "failure_count": 0,
            },
        },
        {
            "success": True,
            "metrics": {"duration_ms": 300, "total_tokens": 60, "tool_calls": 4},
            "progress": {
                "objective_verdict": {"status": "contradicted"},
                "completion_claims": ["goal:challenge"],
                "replan_count": 2,
                "failure_count": 3,
            },
        },
    ]

    summary = SuiteRunner._metrics_summary(attempts)

    assert summary["attempts"] == 2
    assert summary["total_duration_ms"] == 400
    assert summary["mean_duration_ms"] == 200
    assert summary["total_tokens"] == 100
    assert summary["total_tool_calls"] == 6
    assert summary["total_model_calls"] == 1
    assert summary["total_cost_usd"] == 0.25
    assert summary["objective_verdicts"] == 2
    assert summary["false_positive_verifications"] == 1
    assert summary["unverified_completion_claims"] == 1
    assert summary["total_replans"] == 3
    assert summary["failed_tool_calls"] == 3


def test_suite_attempt_report_drops_untrusted_error_text(tmp_path: Path, monkeypatch) -> None:
    task_dir = tmp_path / "task"
    task_dir.mkdir()
    (task_dir / "task.yaml").write_text(
        "apiVersion: harness/v1\n"
        "id: hello-001\n"
        "name: secret test\n"
        "objective:\n  description: test private verifier configuration\n"
        "verification:\n  type: python\n  entrypoint: verifier.py\n"
        "  command:\n    - python\n    - -c\n"
        "    - \"print('api_key=secret-value')\"\n",
        encoding="utf-8",
    )
    (task_dir / "verifier.py").write_text("def verify(): return True\n", encoding="utf-8")
    suite_path = tmp_path / "suite.yaml"
    suite_path.write_text(
        "apiVersion: harness/v1\n"
        'id: "suite token=secret-value"\n'
        "repeat: 1\nbase_seed: 9\nworkers: 1\n"
        "tasks:\n  - path: task/task.yaml\n",
        encoding="utf-8",
    )
    agent_path = Path("agents/examples/demo.yaml")
    runner = SuiteRunner(runs_root=tmp_path / "runs", suites_root=tmp_path / "suites")
    monkeypatch.setattr(
        runner.orchestrator,
        "run",
        lambda **_kwargs: {
            "run_id": "run_redacted",
            "task_id": "hello-001",
            "status": "error",
            "success": False,
            "score": 0.0,
            "message": "provider failed with api_key=secret-value",
            "metrics": {},
            "progress": {"objective_verdict": None},
            "world": {"private": "secret-value"},
            "failure_class": "environment_failure",
        },
    )

    summary = runner.run(
        suite=load_suite(suite_path),
        suite_path=suite_path,
        agent=load_agent(agent_path),
        agent_path=agent_path,
        allow_host_agent=True,
        workers=1,
    )

    report_attempt = summary["runs"][0]
    assert report_attempt["status"] == "error"
    assert report_attempt["failure_class"] == "environment_failure"
    assert "message" not in report_attempt
    assert "progress" not in report_attempt
    assert "world" not in report_attempt
    assert "secret-value" not in str(summary)
    assert summary["suite_id"] == "suite [REDACTED]"
    verification = summary["evaluation_manifest"]["tasks"][0]["verification"]
    assert "command" not in verification
    assert "entrypoint" not in verification
