# Red Harness

Red Harness is a reproducible evaluation runtime and control plane for security agents. It keeps benchmark tasks, agents, target environments, model/tool access, budgets, verification, scoring, telemetry, and distributed execution separate so results can be independently reproduced and compared.

## Current capabilities

Version 0.6 provides:

- declarative task, agent, suite, policy, and distributed-job contracts
- Docker Compose or no-op benchmark environments
- trusted CLI agents and restricted Docker agents
- Python or Docker-sandboxed verifiers
- host or isolated Docker-sidecar Tool/Model Gateway
- OpenAI-compatible streaming and non-streaming model proxy
- token, model-call, tool-call, cost, and wall-time budgets
- provider credential isolation
- Docker runtime profiles, including gVisor via `runtime: runsc`
- parallel suite execution, pass@k, weighted score, and domain aggregation
- authenticated control-plane API
- leased distributed worker queue with heartbeat/recovery semantics
- worker-local provider secrets and workspace path resolution
- benchmark registry scanning
- persisted result/trace viewer API
- leaderboard aggregation
- OTLP/HTTP JSON-compatible trace export
- Firecracker capability detection and machine-profile contract
- GitHub Actions coverage for Docker Agent, sidecar Gateway, Docker Verifier, and a real HTTP control-plane/worker flow

> [!WARNING]
> CLI agents and `verification.type: python` execute trusted host processes. For untrusted evaluation inputs, use Docker agents and Docker verifiers. `runtime: runsc` requires gVisor to be installed and registered with Docker. v0.6 defines and validates the Firecracker host/profile contract, but it does not yet launch microVMs; CI does not provide `/dev/kvm`.

## Install

```bash
python -m pip install -e ".[dev]"
```

Python 3.11+ is required. Docker is required for Docker agents, sidecar Gateway mode, Docker verifiers, and Docker Compose benchmark environments.

## Local run

```bash
redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent \
  --seed 42
```

Parallel suite:

```bash
redharness suite benchmarks/examples/smoke-suite.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent \
  --workers 2
```

A suite may declare:

```yaml
apiVersion: redharness/v1
id: web-suite
repeat: 5
base_seed: 1000
workers: 4
pass_k: [1, 3, 5]

tasks:
  - path: web/task-001/task.yaml
    weight: 1.0
  - path: web/task-002/task.yaml
    weight: 2.0
```

pass@k uses the standard unbiased estimator:

```text
pass@k = 1 - C(n-c, k) / C(n, k)
```

Domain aggregation is based on each task's `category`.

## Distributed control plane

The reference control plane uses SQLite for durable job state and leases. Workers communicate only through HTTP, so they can run on different machines as long as each worker has the same benchmark/agent checkout or compatible workspace layout.

Set an authentication token and start the control plane:

```bash
export REDHARNESS_CONTROL_TOKEN="replace-with-a-random-secret"

redharness serve \
  --queue-db .redharness/control.db \
  --runs-root .redharness/runs \
  --benchmarks-root benchmarks \
  --host 0.0.0.0 \
  --port 8780
```

Submit a job using worker-visible repository-relative paths:

```bash
redharness submit benchmarks/examples/hello/task.yaml \
  --agent agents/examples/demo.yaml \
  --control-url http://control-plane:8780 \
  --runs-root .redharness/runs \
  --seed 100
```

Start a worker:

```bash
export REDHARNESS_CONTROL_TOKEN="replace-with-a-random-secret"

redharness worker \
  --control-url http://control-plane:8780 \
  --workspace-root /srv/red-harness
```

Host-process jobs are rejected by workers unless the worker is explicitly started with:

```bash
--allow-host-jobs
```

### Lease model

A job transitions through:

```text
queued -> running -> completed
                  -> failed
```

When a Worker claims a job it receives a time-limited lease. It heartbeats while Orchestrator is running. If the Worker disappears and the lease expires, another Worker can reclaim the job. Each reclaim increments the job `attempts` counter.

The SQLite backend is the single-control-plane reference implementation. The queue API is intentionally separated from Orchestrator so a PostgreSQL/Redis backend can replace it without changing Worker execution semantics.

Provider API keys are never placed in queue payloads. Jobs store only the environment-variable name, such as `REDHARNESS_MODEL_API_KEY`; the Worker resolves the actual secret locally.

## Control-plane API

The authenticated API includes:

```text
POST /v1/jobs
GET  /v1/jobs
GET  /v1/jobs/{job_id}
POST /v1/jobs/claim
POST /v1/jobs/{job_id}/heartbeat
POST /v1/jobs/{job_id}/complete
POST /v1/jobs/{job_id}/fail

GET  /v1/benchmarks
GET  /v1/leaderboard
GET  /v1/capabilities

GET  /v1/runs/{run_id}
GET  /v1/runs/{run_id}/trace
GET  /v1/runs/{run_id}/otel
```

`/health` remains unauthenticated for service health checks. Other control-plane routes require the configured bearer token.

## Leaderboard

Completed distributed jobs are aggregated by `agent_id`. The current reference leaderboard reports:

- run count
- success rate
- mean score
- median duration
- total estimated model cost

Ordering is capability-first: mean score, success rate, then lower cost and lower duration.

The queue stores full result JSON, so richer benchmark-profile or suite-level leaderboards can be layered on top without changing worker execution.

## Benchmark registry

The control plane recursively scans `task.yaml` files beneath `--benchmarks-root` and exposes valid tasks through `/v1/benchmarks`. Invalid task contracts are returned with validation errors instead of crashing the registry.

## Trace viewer and OpenTelemetry

Every run continues to write append-only `trace.jsonl`.

The control plane exposes the normalized events directly:

```text
GET /v1/runs/{run_id}/trace
```

It also converts them to an OTLP/HTTP JSON-compatible trace document:

```text
GET /v1/runs/{run_id}/otel
```

Local export:

```bash
redharness otel-export .redharness/runs/run_... \
  --output trace.otlp.json
```

Each Red Harness event becomes a span carrying run/task/actor/event attributes plus serialized event data. This export is intentionally file/HTTP payload generation in v0.6; direct collector delivery can be added without modifying the trace recorder.

## Gateway modes

### Host mode

Useful for trusted local development:

```bash
redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/gateway-demo.yaml \
  --allow-host-agent \
  --gateway \
  --gateway-policy examples/gateway-policy.yaml
```

The Agent receives only a one-time Gateway token. Real provider credentials remain on the Harness/Gateway side.

### Isolated Docker sidecar

```bash
docker build -f docker/gateway/Dockerfile \
  -t redharness-gateway:local .

redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/docker-gateway.yaml \
  --gateway \
  --gateway-mode sidecar \
  --gateway-image redharness-gateway:local \
  --gateway-policy examples/gateway-policy.yaml
```

Sidecar mode creates a per-run Docker `--internal` network:

```text
Docker Agent ───── private internal network ───── Gateway sidecar
     │                                               │
     │                                               └─ optional provider egress
     │
     └─ benchmark target network
```

The Gateway publishes no host port. When a provider upstream is configured, only the sidecar is additionally attached to an egress-capable Docker bridge.

## Tool Gateway

The built-in tools remain intentionally narrow:

```text
file.read
file.write
```

The default policy prevents path traversal and constrains reads/writes to task/run roots. No host-side arbitrary shell tool is exposed.

## Execution profiles

Docker Agent:

```yaml
apiVersion: redharness/v1
id: isolated-agent
type: docker
image: my-agent:latest
network: environment
runtime: runsc
```

Docker Verifier:

```yaml
verification:
  type: docker
  image: my-verifier:latest
  command: [python, /task/verifier.py]
  network: none
  runtime: runsc
  timeout: 120
```

Check local support:

```bash
redharness capabilities
```

Output reports availability of:

```text
docker
gvisor_runsc
firecracker
kvm
```

### Firecracker contract

v0.6 contains a `FirecrackerProfile` that validates kernel/rootfs images and generates the boot-source, root drive, and machine configuration expected by a future Firecracker backend. `FirecrackerBackend.validate_host()` requires both the `firecracker` binary and `/dev/kvm`.

This is deliberately not advertised as a runnable backend yet. VM lifecycle, jailer/network setup, snapshotting, and run-bundle mounts remain Phase 7 work.

## Budgets and result bundles

Task budgets can enforce:

```yaml
budgets:
  wall_time: 3600
  max_tokens: 200000
  max_model_calls: 300
  max_tool_calls: 1000
  max_cost_usd: 20
```

Each run is stored beneath `.redharness/runs/<run_id>/`:

```text
result.json
trace.jsonl
events.jsonl
agent.stdout.log
agent.stderr.log
verifier.stdout.log
verifier.stderr.log
```

## Architecture

```text
                         Control Plane
                  ┌────────────┼─────────────┐
                  │            │             │
              Job Queue    Registry     Leaderboard
                  │                          │
          claim / lease / heartbeat          │
                  │                          │
          ┌───────┴────────┐                 │
          │                │                 │
       Worker A         Worker B             │
          │                │                 │
          └────── Orchestrator ──────────────┘
                       │
          ┌────────────┼──────────────┐
          │            │              │
      Environment    Gateway       Verifier
                       │
                   Agent / Model
                       │
                Trace + Result
                       │
              Trace API / OTLP
```

Core separation rule:

```text
Task != Environment != Agent != Model != Tool != Verifier
```

## Roadmap

1. **Done:** task/environment/verifier/result execution contract.
2. **Done:** Docker Agent, budgets, repeat/seed, suites.
3. **Done:** Tool Gateway, model proxy, credential isolation, streaming accounting.
4. **Done:** Docker Verifier sandbox and Docker Agent Gateway support.
5. **Done:** isolated Gateway sidecar, Docker runtime profiles, parallel workers, pass@k, domain scoring.
6. **Done:** authenticated control plane, leased distributed workers, benchmark registry, leaderboard, trace-viewer API, OTLP JSON export, Firecracker host/profile contract.
7. PostgreSQL/Redis queue backend, KVM-enabled Firecracker lifecycle, snapshot pooling, OpenTelemetry collector delivery, browser trace UI, signed benchmark registry, and multi-tenant scheduling.
