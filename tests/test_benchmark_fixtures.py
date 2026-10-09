from pathlib import Path

import pytest

from harness.models import load_task
from harness.trace import TraceRecorder
from harness.verifier import run_verifier


@pytest.mark.parametrize(
    ("category", "filename", "valid_content", "invalid_content"),
    [
        (
            "coding",
            "solution.py",
            "def solve():\n    return 42\n",
            "def solve():\n    return __import__('pathlib').Path('MARKER_PATH').touch()\n",
        ),
        (
            "file",
            "receipt.txt",
            "harness-fixture-accepted\n",
            "harness-fixture-accepted\nextra",
        ),
        ("web", "answer.txt", "flag{web_fixture_token}\n", "flag{wrong}\n"),
        ("pwn", "answer.txt", "24\n", "25\n"),
        ("reverse", "answer.txt", "flag{reverse_fixture}\n", "not-decoded\n"),
        ("crypto", "answer.txt", "flag{crypto_fixture}\n", "not-decrypted\n"),
        ("forensics", "answer.txt", "flag{forensics_fixture}\n", "not-recovered\n"),
        ("misc", "answer.txt", "42\n", "41\n"),
    ],
)
def test_multidomain_fixtures_use_independent_offline_verifiers(
    tmp_path: Path,
    category: str,
    filename: str,
    valid_content: str,
    invalid_content: str,
) -> None:
    task_dir = Path("benchmarks/examples") / category
    task_path = task_dir / "task.yaml"
    task = load_task(task_path)
    run_dir = tmp_path / "workspace"
    run_dir.mkdir()
    artifact = run_dir / filename
    trace = TraceRecorder(tmp_path / "trace.jsonl", f"run-{category}", task.id)
    marker = tmp_path / "candidate-executed"
    invalid_content = invalid_content.replace("MARKER_PATH", str(marker))

    artifact.write_text(valid_content, encoding="utf-8")
    positive, _, _ = run_verifier(
        task,
        task_dir=task_dir,
        run_dir=run_dir,
        environment_project=None,
        environment_network=None,
        trace=trace,
        seed=2000,
    )
    assert positive.success is True

    artifact.write_text(invalid_content, encoding="utf-8")
    negative, _, _ = run_verifier(
        task,
        task_dir=task_dir,
        run_dir=run_dir,
        environment_project=None,
        environment_network=None,
        trace=trace,
        seed=2000,
    )
    assert negative.success is False
    assert marker.exists() is False
