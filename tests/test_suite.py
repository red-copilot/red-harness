from pathlib import Path

from redharness.models import load_agent, load_suite
from redharness.suite import SuiteRunner


def test_suite_repeat_and_seed(tmp_path: Path) -> None:
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

    assert summary["attempts"] == 2
    assert summary["successes"] == 2
    assert summary["weighted_score"] == 100
    assert [run["seed"] for run in summary["runs"]] == [1000, 1001]
