from __future__ import annotations

import json
from pathlib import Path
from typing import Annotated

import typer
from pydantic import ValidationError

from .models import load_agent, load_task
from .orchestrator import Orchestrator

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


@app.command()
def run(
    task: Annotated[Path, typer.Argument(exists=True, dir_okay=False)],
    agent: Annotated[Path, typer.Option("--agent", exists=True, dir_okay=False)],
    runs_root: Annotated[Path, typer.Option("--runs-root")] = Path(".redharness/runs"),
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
    result = Orchestrator(runs_root).run(
        task=task_spec,
        task_path=task,
        agent=agent_spec,
        agent_path=agent,
        allow_host_agent=allow_host_agent,
    )
    typer.echo(json.dumps(result, ensure_ascii=False, indent=2))
    if result["status"] == "error":
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
