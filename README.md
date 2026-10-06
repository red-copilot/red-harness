# Red Harness

Red Harness is a reproducible evaluation runtime for security agents. It separates benchmark tasks, agent implementations, target environments, model/tool access, telemetry, verification, and scoring so results can be independently checked and compared.

## Current capabilities

Version 0.4 provides:

- declarative task, agent, suite, and gateway-policy contracts
- Docker Compose or no-op benchmark environments
- trusted local CLI agent adapter
- restricted Docker agent adapter
- independent Python verifier
- append-only JSONL trace and result bundles
- real-time wall-time, token, model-call, tool-call, and cost budgets
- seed/repeat execution and weighted suites
- policy-gated Tool Gateway
- OpenAI-compatible streaming and non-streaming model proxy
- provider credential isolation
- per-run Gateway lifecycle for CLI and Docker agents
- automatic `OPENAI_BASE_URL` / one-time token injection
- Docker-sandboxed verifier option\n- GitHub Actions CI covering direct, Gateway-backed, Docker Agent, and Docker Verifier runs

> [!WARNING]
> The local CLI adapter and Python verifier execute host processes and are intended only for trusted development inputs. Use the Docker adapter for agent isolation. Per-run Gateway auto-injection currently supports CLI agents; Docker agents can use the standalone Gateway service until a network-sidecar mode is added.

## Install

```bash
python -m pip install -e ".[dev]"
```

Python 3.11+ is required. Docker is required for Docker agents and Docker Compose benchmark environments.

## Quick start

Validate the example contracts:

```bash
redharness validate benchmarks/examples/hello/task.yaml
redharness validate-suite benchmarks/examples/smoke-suite.yaml
```

Run directly:

```bash
redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent \
  --seed 42
```

Run through the per-run Tool Gateway:

```bash
redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/gateway-demo.yaml \
  --allow-host-agent \
  --gateway \
  --gateway-policy examples/gateway-policy.yaml \
  --seed 42
```

When `--gateway` is enabled for a CLI agent, Harness starts an ephemeral localhost service, generates a one-time bearer token, and injects:

```text
REDHARNESS_GATEWAY_URL
REDHARNESS_GATEWAY_TOKEN
OPENAI_BASE_URL
OPENAI_API_KEY
```

`OPENAI_API_KEY` contains only the one-time Gateway token. The real provider credential stays in the Harness process.

## Model proxy

Set the actual model API key in the Harness environment:

```bash
export REDHARNESS_MODEL_API_KEY="..."
```

Then run with an OpenAI-compatible upstream:

```bash
redharness run benchmark/task.yaml \
  --agent agents/my-agent.yaml \
  --allow-host-agent \
  --gateway \
  --model-upstream https://provider.example/v1 \
  --input-price-per-million 2.50 \
  --output-price-per-million 10.00
```

The Agent talks to the ephemeral Red Harness URL. The Gateway replaces the Agent bearer token with the real provider credential before forwarding the request.

For successful non-streaming `/v1/chat/completions` requests, the Gateway emits:

```text
model.request
model.response
model.usage
```

Usage is extracted from the upstream response and converted into Harness token/cost metrics. Failed model requests still count toward `max_model_calls`.

Streaming is supported in v0.4. The proxy forces `stream_options.include_usage=true`, relays SSE chunks, and records final usage/cost when the upstream provides it. If an upstream omits usage, Harness records `model.usage_missing` rather than inventing token counts.

## Tool Gateway

The v0.3 built-ins are deliberately narrow:

```text
file.read
file.write
```

Unknown or denied tools are rejected. There is no host-side arbitrary shell execution.

Example policy:

```yaml
allowed_tools:
  - file.read
  - file.write

denied_tools: []

max_tool_output_bytes: 65536
max_file_write_bytes: 1048576
```

Rules:

- `file.read` is restricted to the benchmark task directory and run workspace.
- `file.write` is restricted to the run workspace.
- path traversal outside those roots is rejected
- denied tool attempts are still counted against `max_tool_calls`
- outputs and writes have policy-controlled size limits

An Agent calls:

```http
POST /v1/tools/call
Authorization: Bearer <REDHARNESS_GATEWAY_TOKEN>
Content-Type: application/json

{
  "name": "file.write",
  "args": {
    "path": "proof.txt",
    "content": "..."
  }
}
```

## Docker Agent Gateway

Docker Agents can use the same per-run Gateway:

```yaml
apiVersion: redharness/v1
id: docker-agent
type: docker
image: my-agent:latest
network: environment
```

When `--gateway` is enabled, Harness binds the ephemeral Gateway on a Docker-reachable host address, injects `host.docker.internal:host-gateway`, and passes the same one-time token variables into the container. If no benchmark network exists, the Agent uses the normal Docker bridge; if a Compose target network exists, it remains attached to that network.

`network: none` is never silently relaxed and causes the run to fail if Gateway access is requested.

## Docker Verifier

A task can move grading out of the host Python process:

```yaml
verification:
  type: docker
  image: my-verifier:latest
  command: [python, /task/verifier.py]
  network: none
  timeout: 120
```

The verifier container receives the task and run bundle as read-only mounts, has a read-only root filesystem, all Linux capabilities dropped, `no-new-privileges`, CPU/RAM/PID limits, and an isolated tmpfs. Set `network: environment` only when the verifier must inspect a live benchmark service.

## Standalone Gateway

Docker agents or external workers can run the Gateway separately.

Set a Gateway token:

```bash
export REDHARNESS_GATEWAY_TOKEN="replace-with-random-token"
```

Then:

```bash
redharness gateway \
  --workspace .redharness/run-workspace \
  --task-dir benchmarks/example \
  --event-file .redharness/events.jsonl \
  --policy examples/gateway-policy.yaml
```

Provider credentials are read from `REDHARNESS_MODEL_API_KEY` by default and are never returned to clients.

## Budgets and accounting

A task can declare:

```yaml
budgets:
  wall_time: 3600
  max_tokens: 200000
  max_model_calls: 300
  max_tool_calls: 1000
  max_cost_usd: 20
```

Harness tails the event stream while the Agent runs. Exceeding a configured budget terminates the Agent and returns:

```text
status: budget_exceeded
```

Result metrics include:

```json
{
  "duration_ms": 731,
  "input_tokens": 1200,
  "output_tokens": 300,
  "total_tokens": 1500,
  "model_calls": 4,
  "tool_calls": 12,
  "cost_usd": 0.04
}
```

## Suites

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

Run:

```bash
redharness suite benchmarks/examples/smoke-suite.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent
```

The same `--gateway` and model proxy options can be applied to suites.

## Architecture

```text
Benchmark Task ─────────────┐
Agent Spec ─────────────────┼──> Orchestrator
Suite / Seed ───────────────┘        │
                                     ├──> Environment Provider
                                     ├──> Gateway Runtime
                                     │      ├── Policy Engine
                                     │      ├── Tool Gateway
                                     │      └── Model Proxy
                                     ├──> Agent Adapter
                                     │      ├── CLI
                                     │      └── Docker
                                     ├──> Budget Monitor
                                     ├──> Trace Recorder
                                     └──> Independent Verifier
                                                │
                                            Result Bundle
```

Core rule:

```text
Task != Environment != Agent != Model != Tool != Verifier
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

## Roadmap

1. **Done:** task/environment/verifier/result execution contract.
2. **Done:** Docker Agent, telemetry budgets, repeat/seed, suites.
3. **Done:** policy-gated Tool Gateway, OpenAI-compatible model proxy, credential isolation, per-run CLI Gateway.
4. Docker network-sidecar Gateway, streaming proxy accounting, provider adapters.
5. Sandboxed verifiers plus gVisor/Firecracker execution profiles.
6. Parallel/distributed workers, pass@k, domain scoring, OpenTelemetry, trace viewer, registry and leaderboard.
