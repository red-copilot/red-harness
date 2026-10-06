from pathlib import Path

from redharness.models import load_agent, load_suite
from redharness.suite import SuiteRunner


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
    assert summary["weighted_score"] == 100
    assert summary["pass_at_k"] == {"1": 1.0, "2": 1.0}
    assert summary["domains"]["general"]["weighted_score"] == 100
    assert summary["tasks"][0]["pass_at_k"] == {"1": 1.0, "2": 1.0}
    assert [run["seed"] for run in summary["runs"]] == [1000, 1001]
