# Red Harness

Red Harness is a reproducible evaluation runtime for security agents. It keeps the benchmark, agent, environment, telemetry, verifier, and scoring path separate so results can be independently checked and compared.

## Current capabilities

Version 0.2 provides:

- declarative `task.yaml`, `agent.yaml`, and suite contracts
- Docker Compose or no-op benchmark environments
- trusted local CLI agent adapter (explicit opt-in)
- restricted Docker agent adapter
- independent Python verifier
- append-only `trace.jsonl`
- agent telemetry protocol through `events.jsonl`
- model token/cost accounting and tool-call accounting
- wall-time, token, model-call, tool-call, and cost budgets
- seed/repeat execution
- weighted benchmark suites
- per-run and per-suite result bundles
- GitHub Actions CI

> [!WARNING]
> The local CLI adapter and Python verifier execute host processes and are intended only for trusted development inputs. Use the Docker adapter for agent isolation. Verifier sandboxing is still roadmap work.

## Install

```bash
python -m pip install -e ".[dev]"
```

Python 3.11+ is required. Docker is required for Docker agents and Docker Compose benchmark environments.

## Quick start

Validate a task and suite:

```bash
redharness validate benchmarks/examples/hello/task.yaml
redharness validate-suite benchmarks/examples/smoke-suite.yaml
```

Run the smoke task with the trusted local demo agent:

```bash
redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent \
  --seed 42
```

Repeat a task:

```bash
redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent \
  --repeat 5 \
  --seed 1000
```

Run a suite:

```bash
redharness suite benchmarks/examples/smoke-suite.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent
```

## Run bundle

Each attempt is written beneath `.redharness/runs/<run_id>/`:

```text
result.json
trace.jsonl
events.jsonl
agent.stdout.log
agent.stderr.log
verifier.stdout.log
verifier.stderr.log
```

A suite summary is written beneath `.redharness/suites/<suite_run_id>/result.json`.

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
  max_cost_usd: 20

verification:
  type: python
  entrypoint: verifier.py
```

The verifier, never the agent's final answer, determines success.

## Agent adapters

Trusted local CLI agent:

```yaml
apiVersion: redharness/v1
id: local-agent
type: cli
command: [python, agent.py]
```

Docker agent:

```yaml
apiVersion: redharness/v1
id: isolated-agent
type: docker
image: ghcr.io/example/security-agent:latest
network: environment
command: []
```

The Docker adapter currently uses these restrictive defaults:

- read-only root filesystem
- all Linux capabilities dropped
- `no-new-privileges`
- PID, memory, and CPU limits
- isolated `/tmp`
- task mounted read-only
- run directory mounted read/write
- no Docker socket mount
- `--pull=never`
- `network: none` unless the agent is intentionally attached to the benchmark environment

When `network: environment` is used, the agent joins the Docker Compose default network. Benchmark authors are responsible for declaring an internal Docker network when internet egress must be prohibited.

## Agent telemetry protocol

Harness creates `REDHARNESS_EVENT_FILE`. Agents or a future model/tool proxy append JSONL events:

```json
{"type":"model.usage","data":{"input_tokens":1200,"output_tokens":300,"cost_usd":0.04,"model":"model-x"}}
{"type":"tool.call","data":{"tool":"shell.exec"}}
```

Python agents can use the included helper:

```python
from redharness.telemetry import model_usage, tool_call

tool_call(tool="shell.exec")
model_usage(
    input_tokens=1200,
    output_tokens=300,
    cost_usd=0.04,
    model="model-x",
)
```

Harness tails this event stream while the Agent is running. If a configured budget is exceeded, the Agent process is terminated and the run ends with `status: budget_exceeded`.

The same event contract is intended to be produced by the future Model Proxy and Tool Gateway, so Agent implementations do not become coupled to one model vendor.

## Suite contract

```yaml
apiVersion: redharness/v1
id: web-suite
repeat: 3
base_seed: 1000

tasks:
  - path: web/task-001/task.yaml
    weight: 1.0
  - path: web/task-002/task.yaml
    weight: 2.0
```

Each repeat receives a stable seed. Results include success rate and weighted score, while individual attempts retain their own full trace and metrics.

## Architecture

```text
Benchmark Task ───────┐
Agent Spec ───────────┼──> Orchestrator
Suite / Seed ─────────┘        │
                               ├──> Environment Provider
                               ├──> Agent Adapter
                               │      └──> CLI / Docker
                               ├──> Budget Monitor
                               │      └──> Agent Telemetry
                               ├──> Trace Recorder
                               └──> Independent Verifier
                                          │
                                      Result Bundle
```

Core rule:

```text
Task != Environment != Agent != Model != Tool != Verifier
```

## Result metrics

A result contains capability outcome plus efficiency metrics:

```json
{
  "success": true,
  "score": 100,
  "seed": 42,
  "metrics": {
    "duration_ms": 731,
    "input_tokens": 1200,
    "output_tokens": 300,
    "total_tokens": 1500,
    "model_calls": 4,
    "tool_calls": 12,
    "cost_usd": 0.04
  }
}
```

## Roadmap

1. **Done:** reproducible task contract, environment lifecycle, verifier and result bundles.
2. **Done:** Docker Agent, telemetry accounting, enforceable budgets, repeat/seed and suites.
3. Tool Gateway with policy engine and model-provider proxy adapters.
4. Sandboxed verifiers plus gVisor/Firecracker execution profiles.
5. Parallel/distributed workers, pass@k and domain scoring profiles.
6. OpenTelemetry export, trace viewer, benchmark registry and leaderboard.
