# Red Harness

Red Harness is a reproducible evaluation runtime for security agents. It keeps the benchmark, agent, environment, trace, and verifier separate so results can be independently checked and compared.

## MVP

The first implementation provides:

- declarative `task.yaml` and `agent.yaml`
- Docker Compose or no-op environments
- trusted local CLI agent adapter (explicit opt-in)
- independent Python verifier
- append-only JSONL trace
- per-run result bundle
- wall-clock budget enforcement
- validation and report CLI commands

> [!WARNING]
> The CLI adapter and Python verifier execute local processes. They are intended for trusted development inputs only. Do not run untrusted agents or benchmark packages with the MVP adapter. Hardened agent/verifier sandboxes and a policy-gated tool gateway are follow-up work.

## Install

```bash
python -m pip install -e ".[dev]"
```

Python 3.11+ is required. Docker Compose is only required for tasks whose environment provider is `docker-compose`.

## Quick start

Validate the example:

```bash
redharness validate benchmarks/examples/hello/task.yaml
```

Run it with the trusted demo agent:

```bash
redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent
```

A run is written beneath `.redharness/runs/<run_id>/`:

```text
result.json
trace.jsonl
agent.stdout.log
agent.stderr.log
verifier.stdout.log
verifier.stderr.log
```

Inspect a result:

```bash
redharness report .redharness/runs/<run_id>
```

## Task contract

```yaml
apiVersion: redharness/v1
id: example
name: Example task
category: web
objective:
  description: Complete the objective in the isolated benchmark environment.
environment:
  provider: docker-compose
  manifest: env/docker-compose.yml
budgets:
  wall_time: 3600
  max_tokens: 200000
  max_model_calls: 300
  max_tool_calls: 1000
verification:
  type: python
  entrypoint: verifier.py
```

The verifier, not the agent's final answer, determines success.

## Architecture

```text
Task Spec ──┐
Agent Spec ─┼──> Orchestrator ──> Environment
            │          │
            │          ├──> Agent Adapter
            │          ├──> Trace Recorder
            │          └──> Independent Verifier
            │
            └──────────────> Result Bundle
```

Core rule: Task != Environment != Agent != Verifier.

## Roadmap

1. MVP execution contract and reproducible result bundles.
2. Tool Gateway + policy engine + model proxy for tool/token/cost accounting.
3. Docker/gVisor/Firecracker agent isolation and sandboxed verifiers.
4. Suites, repeats/seeds, pass@k, domain scoring, parallel workers.
5. OpenTelemetry export, web trace viewer, registry and leaderboard.
