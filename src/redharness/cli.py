from __future__ import annotations

import json
from pathlib import Path
from typing import Annotated

import typer
from pydantic import ValidationError

from .models import load_agent, load_suite, load_task
from .orchestrator import Orchestrator
from .suite import SuiteRunner

app = typer.Typer(
    name="redharness",
    help="Reproducible evaluation runtime for security agents.",
    no_args_is_help=True,
)


@app.command()
def validate(
    task: Annotated[Path, typer.Argument(exists=True, dir_okay=False)],
) -> None:
    """Validate a task contract."""
    try:
        spec = load_task(task)
    except (ValidationError, ValueError, TypeError) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=2) from exc
    typer.echo(f"valid: {spec.id} ({spec.api_version})")


@app.command("validate-suite")
def validate_suite(
    suite: Annotated[Path, typer.Argument(exists=True, dir_okay=False)],
) -> None:
    """Validate a suite contract."""
    try:
        spec = load_suite(suite)
    except (ValidationError, ValueError, TypeError) as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=2) from exc
    typer.echo(f"valid suite: {spec.id} ({len(spec.tasks)} tasks x {spec.repeat})")


@app.command()
def run(
    task: Annotated[Path, typer.Argument(exists=True, dir_okay=False)],
    agent: Annotated[Path, typer.Option("--agent", exists=True, dir_okay=False)],
    runs_root: Annotated[Path, typer.Option("--runs-root")] = Path(".redharness/runs"),
    repeat: Annotated[int, typer.Option("--repeat", min=1)] = 1,
    seed: Annotated[int, typer.Option("--seed", min=0)] = 0,
    allow_host_agent: Annotated[
        bool,
        typer.Option(
            "--allow-host-agent",
            help="Allow trusted CLI agent command to execute on the host.",
        ),
    ] = False,
) -> None:
    """Run one task with one agent."""
    task_spec = load_task(task)
    agent_spec = load_agent(agent)
    orchestrator = Orchestrator(runs_root)
    results = [
        orchestrator.run(
            task=task_spec,
            task_path=task,
            agent=agent_spec,
            agent_path=agent,
            allow_host_agent=allow_host_agent,
            seed=seed + index,
        )
        for index in range(repeat)
    ]
    if repeat == 1:
        output: dict | list = results[0]
    else:
        output = {
            "attempts": repeat,
            "successes": sum(1 for item in results if item["success"]),
            "success_rate": sum(1 for item in results if item["success"]) / repeat,
            "mean_score": sum(float(item["score"]) for item in results) / repeat,
            "runs": results,
        }
    typer.echo(json.dumps(output, ensure_ascii=False, indent=2))
    if any(item["status"] == "error" for item in results):
        raise typer.Exit(code=1)


@app.command("suite")
def run_suite(
    suite: Annotated[Path, typer.Argument(exists=True, dir_okay=False)],
    agent: Annotated[Path, typer.Option("--agent", exists=True, dir_okay=False)],
    runs_root: Annotated[Path, typer.Option("--runs-root")] = Path(".redharness/runs"),
    suites_root: Annotated[Path, typer.Option("--suites-root")] = Path(".redharness/suites"),
    allow_host_agent: Annotated[
        bool,
        typer.Option(
            "--allow-host-agent",
            help="Allow trusted CLI agent command to execute on the host.",
        ),
    ] = False,
) -> None:
    """Run all tasks and repeats declared by a suite."""
    summary = SuiteRunner(runs_root=runs_root, suites_root=suites_root).run(
        suite=load_suite(suite),
        suite_path=suite,
        agent=load_agent(agent),
        agent_path=agent,
        allow_host_agent=allow_host_agent,
    )
    typer.echo(json.dumps(summary, ensure_ascii=False, indent=2))
    if any(item["status"] == "error" for item in summary["runs"]):
        raise typer.Exit(code=1)


@app.command()
def report(
    run_dir: Annotated[Path, typer.Argument(exists=True, file_okay=False)],
) -> None:
    """Print a saved result bundle."""
    result_path = run_dir / "result.json"
    if not result_path.is_file():
        typer.echo(f"missing {result_path}", err=True)
        raise typer.Exit(code=2)
    typer.echo(result_path.read_text(encoding="utf-8"))


if __name__ == "__main__":
    app()
