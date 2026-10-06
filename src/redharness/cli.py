from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Annotated

import httpx
import typer
import uvicorn
from pydantic import ValidationError

from .control_plane import create_control_plane
from .execution import ExecutionCapabilities
from .gateway import ModelPricing, create_gateway_app
from .gateway_runtime import GatewayConfig
from .models import load_agent, load_suite, load_task
from .orchestrator import Orchestrator
from .otel import export_otlp_json
from .policy import load_policy
from .queue import GatewayJobSpec, JobPayload
from .suite import SuiteRunner
from .worker import Worker

app = typer.Typer(
    name="redharness",
    help="Reproducible evaluation runtime for security agents.",
    no_args_is_help=True,
)


def _gateway_config(
    *,
    enabled: bool,
    policy: Path | None,
    model_upstream: str | None,
    model_api_key_env: str,
    input_price: float,
    output_price: float,
    mode: str,
    sidecar_image: str | None,
    sidecar_runtime: str | None,
) -> GatewayConfig | None:
    if not enabled:
        return None
    if mode not in {"host", "sidecar"}:
        raise typer.BadParameter("--gateway-mode must be host or sidecar")
    if mode == "sidecar" and not sidecar_image:
        raise typer.BadParameter("--gateway-image is required for sidecar mode")
    return GatewayConfig(
        policy_path=policy.resolve() if policy else None,
        model_upstream=model_upstream,
        model_api_key=os.environ.get(model_api_key_env),
        input_price_per_million_usd=input_price,
        output_price_per_million_usd=output_price,
        mode=mode,
        sidecar_image=sidecar_image,
        sidecar_runtime=sidecar_runtime,
    )


def _required_env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        typer.echo(f"missing required environment variable {name}", err=True)
        raise typer.Exit(code=2)
    return value


def _control_headers(token_env: str) -> dict[str, str]:
    return {"authorization": f"Bearer {_required_env(token_env)}"}


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
    gateway_enabled: Annotated[bool, typer.Option("--gateway")] = False,
    gateway_policy: Annotated[
        Path | None,
        typer.Option("--gateway-policy", exists=True, dir_okay=False),
    ] = None,
    gateway_mode: Annotated[str, typer.Option("--gateway-mode")] = "host",
    gateway_image: Annotated[str | None, typer.Option("--gateway-image")] = None,
    gateway_runtime: Annotated[str | None, typer.Option("--gateway-runtime")] = None,
    model_upstream: Annotated[str | None, typer.Option("--model-upstream")] = None,
    model_api_key_env: Annotated[str, typer.Option("--model-api-key-env")] = (
        "REDHARNESS_MODEL_API_KEY"
    ),
    input_price: Annotated[float, typer.Option("--input-price-per-million", min=0)] = 0.0,
    output_price: Annotated[float, typer.Option("--output-price-per-million", min=0)] = 0.0,
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
    gateway_config = _gateway_config(
        enabled=gateway_enabled,
        policy=gateway_policy,
        model_upstream=model_upstream,
        model_api_key_env=model_api_key_env,
        input_price=input_price,
        output_price=output_price,
        mode=gateway_mode,
        sidecar_image=gateway_image,
        sidecar_runtime=gateway_runtime,
    )
    orchestrator = Orchestrator(runs_root)
    results = [
        orchestrator.run(
            task=task_spec,
            task_path=task,
            agent=agent_spec,
            agent_path=agent,
            allow_host_agent=allow_host_agent,
            seed=seed + index,
            gateway_config=gateway_config,
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
    workers: Annotated[int | None, typer.Option("--workers", min=1, max=64)] = None,
    gateway_enabled: Annotated[bool, typer.Option("--gateway")] = False,
    gateway_policy: Annotated[
        Path | None,
        typer.Option("--gateway-policy", exists=True, dir_okay=False),
    ] = None,
    gateway_mode: Annotated[str, typer.Option("--gateway-mode")] = "host",
    gateway_image: Annotated[str | None, typer.Option("--gateway-image")] = None,
    gateway_runtime: Annotated[str | None, typer.Option("--gateway-runtime")] = None,
    model_upstream: Annotated[str | None, typer.Option("--model-upstream")] = None,
    model_api_key_env: Annotated[str, typer.Option("--model-api-key-env")] = (
        "REDHARNESS_MODEL_API_KEY"
    ),
    input_price: Annotated[float, typer.Option("--input-price-per-million", min=0)] = 0.0,
    output_price: Annotated[float, typer.Option("--output-price-per-million", min=0)] = 0.0,
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
        workers=workers,
        gateway_config=_gateway_config(
            enabled=gateway_enabled,
            policy=gateway_policy,
            model_upstream=model_upstream,
            model_api_key_env=model_api_key_env,
            input_price=input_price,
            output_price=output_price,
            mode=gateway_mode,
            sidecar_image=gateway_image,
            sidecar_runtime=gateway_runtime,
        ),
    )
    typer.echo(json.dumps(summary, ensure_ascii=False, indent=2))
    if any(item["status"] == "error" for item in summary["runs"]):
        raise typer.Exit(code=1)


@app.command("serve")
def serve(
    queue_db: Annotated[Path, typer.Option("--queue-db")] = Path(".redharness/control.db"),
    runs_root: Annotated[Path, typer.Option("--runs-root")] = Path(".redharness/runs"),
    benchmarks_root: Annotated[Path, typer.Option("--benchmarks-root")] = Path("benchmarks"),
    host: Annotated[str, typer.Option("--host")] = "127.0.0.1",
    port: Annotated[int, typer.Option("--port", min=1, max=65535)] = 8780,
    token_env: Annotated[str, typer.Option("--token-env")] = "REDHARNESS_CONTROL_TOKEN",
) -> None:
    """Serve the authenticated queue, leaderboard, registry, and trace-viewer API."""
    token = _required_env(token_env)
    api = create_control_plane(
        queue_db=queue_db.resolve(),
        runs_root=runs_root.resolve(),
        benchmarks_root=benchmarks_root.resolve(),
        token=token,
    )
    uvicorn.run(api, host=host, port=port, access_log=False)


@app.command("submit")
def submit(
    task: Annotated[str, typer.Argument(help="Worker-visible task path")],
    agent: Annotated[str, typer.Option("--agent", help="Worker-visible agent path")],
    control_url: Annotated[str, typer.Option("--control-url")],
    runs_root: Annotated[str, typer.Option("--runs-root")] = ".redharness/runs",
    seed: Annotated[int, typer.Option("--seed", min=0)] = 0,
    allow_host_agent: Annotated[bool, typer.Option("--allow-host-agent")] = False,
    gateway_enabled: Annotated[bool, typer.Option("--gateway")] = False,
    gateway_policy: Annotated[str | None, typer.Option("--gateway-policy")] = None,
    gateway_mode: Annotated[str, typer.Option("--gateway-mode")] = "host",
    gateway_image: Annotated[str | None, typer.Option("--gateway-image")] = None,
    gateway_runtime: Annotated[str | None, typer.Option("--gateway-runtime")] = None,
    model_upstream: Annotated[str | None, typer.Option("--model-upstream")] = None,
    model_api_key_env: Annotated[str, typer.Option("--model-api-key-env")] = (
        "REDHARNESS_MODEL_API_KEY"
    ),
    input_price: Annotated[float, typer.Option("--input-price-per-million", min=0)] = 0.0,
    output_price: Annotated[float, typer.Option("--output-price-per-million", min=0)] = 0.0,
    token_env: Annotated[str, typer.Option("--token-env")] = "REDHARNESS_CONTROL_TOKEN",
) -> None:
    """Submit one run to a Red Harness control plane."""
    if gateway_mode not in {"host", "sidecar"}:
        raise typer.BadParameter("--gateway-mode must be host or sidecar")
    if gateway_enabled and gateway_mode == "sidecar" and not gateway_image:
        raise typer.BadParameter("--gateway-image is required for sidecar mode")
    payload = JobPayload(
        task_path=task,
        agent_path=agent,
        runs_root=runs_root,
        allow_host_agent=allow_host_agent,
        seed=seed,
        gateway=GatewayJobSpec(
            enabled=gateway_enabled,
            policy_path=gateway_policy,
            mode=gateway_mode,
            sidecar_image=gateway_image,
            sidecar_runtime=gateway_runtime,
            model_upstream=model_upstream,
            model_api_key_env=model_api_key_env,
            input_price_per_million_usd=input_price,
            output_price_per_million_usd=output_price,
        ),
    )
    response = httpx.post(
        control_url.rstrip("/") + "/v1/jobs",
        headers=_control_headers(token_env),
        json=payload.model_dump(),
        timeout=30.0,
    )
    response.raise_for_status()
    typer.echo(json.dumps(response.json(), ensure_ascii=False, indent=2))


@app.command("worker")
def worker(
    control_url: Annotated[str, typer.Option("--control-url")],
    workspace_root: Annotated[Path, typer.Option("--workspace-root", file_okay=False)] = Path(
        "."
    ),
    worker_id: Annotated[str | None, typer.Option("--worker-id")] = None,
    lease_seconds: Annotated[int, typer.Option("--lease-seconds", min=10, max=3600)] = 60,
    poll_interval: Annotated[float, typer.Option("--poll-interval", min=0.1)] = 2.0,
    once: Annotated[bool, typer.Option("--once")] = False,
    allow_host_jobs: Annotated[bool, typer.Option("--allow-host-jobs")] = False,
    token_env: Annotated[str, typer.Option("--token-env")] = "REDHARNESS_CONTROL_TOKEN",
) -> None:
    """Claim and execute leased jobs from a control plane."""
    instance = Worker(
        control_url=control_url,
        token=_required_env(token_env),
        worker_id=worker_id,
        lease_seconds=lease_seconds,
        allow_host_jobs=allow_host_jobs,
        workspace_root=workspace_root,
    )
    try:
        if once:
            worked = instance.run_once()
            typer.echo("executed" if worked else "no-job")
        else:
            instance.run_forever(poll_interval=poll_interval)
    finally:
        instance.close()


@app.command("otel-export")
def otel_export(
    run_dir: Annotated[Path, typer.Argument(exists=True, file_okay=False)],
    output: Annotated[Path | None, typer.Option("--output")] = None,
) -> None:
    """Export trace.jsonl as OTLP/HTTP JSON-compatible trace data."""
    trace_path = run_dir / "trace.jsonl"
    if not trace_path.is_file():
        typer.echo(f"missing {trace_path}", err=True)
        raise typer.Exit(code=2)
    target = output or (run_dir / "otel-traces.json")
    export_otlp_json(trace_path, target)
    typer.echo(str(target))


@app.command("capabilities")
def capabilities() -> None:
    """Show locally available execution backends."""
    typer.echo(json.dumps(ExecutionCapabilities.detect().as_dict(), indent=2))


@app.command("gateway")
def gateway(
    event_file: Annotated[Path, typer.Option("--event-file")] = Path(
        ".redharness/gateway/events.jsonl"
    ),
    workspace: Annotated[Path, typer.Option("--workspace", file_okay=False)] = Path("."),
    task_dir: Annotated[Path, typer.Option("--task-dir", file_okay=False)] = Path("."),
    policy: Annotated[Path | None, typer.Option("--policy", dir_okay=False)] = None,
    host: Annotated[str, typer.Option("--host")] = "127.0.0.1",
    port: Annotated[int, typer.Option("--port", min=1, max=65535)] = 8765,
    model_upstream: Annotated[str | None, typer.Option("--model-upstream")] = None,
    gateway_token_env: Annotated[str, typer.Option("--gateway-token-env")] = (
        "REDHARNESS_GATEWAY_TOKEN"
    ),
    model_api_key_env: Annotated[str, typer.Option("--model-api-key-env")] = (
        "REDHARNESS_MODEL_API_KEY"
    ),
    input_price: Annotated[float, typer.Option("--input-price-per-million", min=0)] = 0.0,
    output_price: Annotated[float, typer.Option("--output-price-per-million", min=0)] = 0.0,
) -> None:
    """Serve the policy-gated Tool Gateway and OpenAI-compatible model proxy."""
    gateway_token = os.environ.get(gateway_token_env)
    if not gateway_token:
        typer.echo(
            f"missing gateway token in environment variable {gateway_token_env}",
            err=True,
        )
        raise typer.Exit(code=2)

    app_instance = create_gateway_app(
        event_file=event_file.resolve(),
        workspace=workspace.resolve(),
        task_dir=task_dir.resolve(),
        policy=load_policy(policy),
        gateway_token=gateway_token,
        model_upstream=model_upstream,
        model_api_key=os.environ.get(model_api_key_env),
        model_pricing=ModelPricing(
            input_per_million_usd=input_price,
            output_per_million_usd=output_price,
        ),
    )
    uvicorn.run(app_instance, host=host, port=port, access_log=False)


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
