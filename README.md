# Red Harness

Red Harness is a reproducible evaluation runtime and control plane for security agents. It keeps benchmark tasks, agents, target environments, model/tool access, budgets, verification, scoring, telemetry, and distributed execution separate so results can be independently reproduced and compared.

## Current capabilities

Version 0.8 provides:

- declarative task, agent, suite, policy, and distributed-job contracts
- first-class [Pi](https://pi.dev/docs/latest) Agent Adapter running inside Kali Rolling Docker
- Pi tool/model event normalization into Harness traces and budgets
- Kali Rolling + `kali-linux-core` Pi image with curated security tools\n- isolated per-run Pi configuration and deterministic non-interactive defaults\n- generic Benchmark Adapter contracts (`discover/provision/submit/evaluate/teardown`)
- TSec Benchmark adapter over the official SDK (`list/start/submit/close`) with Harness-owned benchmark credentials
- optional Pi model routing through the Red Harness credential-isolating Gateway
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
- event-sourced universal world-state runtime with materialized snapshots
- domain-neutral skill metadata registry for planner and domain extensions
- rolling-horizon planner integrated with the interactive SolverLoop
- leased coordination blackboard for explicit multi-agent work distribution
- leaderboard aggregation
- OTLP/HTTP JSON-compatible trace export
- Firecracker capability detection and machine-profile contract
- GitHub Actions coverage for Pi 1.0.4, Docker Agent, sidecar Gateway, Docker Verifier, and a real HTTP control-plane/worker flow

> [!WARNING]
> CLI agents and `verification.type: python` execute trusted host processes. Pi runs inside a restricted Docker container by default. `runtime: runsc` requires gVisor to be installed and registered with Docker. v0.8 defines and validates the Firecracker host/profile contract, but it does not yet launch microVMs; CI does not provide `/dev/kvm`.

## Install

```bash
python -m pip install -e ".[dev]"
```

Python 3.11+ is required. Docker is required for Docker agents, sidecar Gateway mode, Docker verifiers, and Docker Compose benchmark environments.

For Pi support, install Pi separately. Red Harness CI pins Pi 1.0.4:

```bash
npm install -g --ignore-scripts @earendil-works/pi-coding-agent@1.0.4
pi --version
```

Pi requires Node.js 22.19 or newer. See [`docs/pi.md`](docs/pi.md) for the full integration contract.

## Local run

```bash
harness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent \
  --seed 42
```

Parallel suite:

```bash
harness suite benchmarks/examples/smoke-suite.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent \
  --workers 2
```

A suite may declare:

```yaml
apiVersion: harness/v1
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

## Pi Agent Adapter

Example `agents/examples/pi.yaml`:

```yaml
apiVersion: harness/v1
id: pi-openai-sol
type: pi

pi:
  provider: openai
  model: gpt-5.6-sol
  thinking: medium
  tools: [read, bash, edit, write]
  approve_project: false
  context_files: false
  extensions: false
  skills: false
  prompt_templates: false
  themes: false
  mcp: false
  offline: true
```

Direct-provider mode uses the provider's normal environment credential:

```bash
export OPENAI_API_KEY="..."

harness run benchmark/task.yaml \
  --agent agents/examples/pi.yaml \
  --allow-host-agent
```

The adapter runs Pi in one-shot JSON mode and normalizes its structured events:

```text
Pi session                    -> pi.session
assistant message_start       -> model.request
assistant message_end         -> model.response + model.usage
tool_execution_start          -> tool.call
tool_execution_end            -> tool.result
agent_settled                 -> pi.agent_settled
```

Pi model usage contributes input/output/total tokens, cache usage, reasoning usage, and model cost to the existing Harness budget and result metrics. The raw Pi JSONL stream is retained in `agent.stdout.log`.

By default, Pi runs in Docker with a read-only root filesystem, dropped Linux capabilities, bounded CPU/RAM/PIDs, read-only task mount, writable run workspace, and a fresh per-run `PI_CODING_AGENT_DIR`. Pi sessions are ephemeral, telemetry/update checks are disabled, project trust is denied, and project context/extensions/skills/MCP/templates/themes are not loaded unless explicitly enabled.

### Pi through the Harness model Gateway

Pi can be forced through the Harness OpenAI-compatible model proxy:

```bash
export HARNESS_MODEL_API_KEY="real-provider-key"

harness run benchmark/task.yaml \
  --agent agents/examples/pi.yaml \
  --allow-host-agent \
  --gateway \
  --model-upstream https://provider.example/v1 \
  --input-price-per-million 2.0 \
  --output-price-per-million 10.0
```

For that run Harness creates an isolated Pi `models.json` with a `harness` provider whose API key is the one-time `HARNESS_GATEWAY_TOKEN`. The real upstream credential remains inside the Gateway process. Gateway model events are authoritative in this mode, so Pi's copy of model usage is not counted twice.

The Pi adapter is containerized and supports both host Gateway mode and Docker sidecar Gateway mode. For VPN-backed benchmarks such as TSec, `network: host` is available on Linux workers; sidecar Gateway mode is intentionally incompatible with `network: host` because the Agent must join the private Gateway network.

## TSec Benchmark SDK

Install the optional SDK dependency and build the Pi/Kali image:

```bash
python -m pip install -e ".[tsec]"
docker build -t harness/pi-kali:local docker/pi-kali
```

Set `BENCHMARK_BASE_URL` and `BENCHMARK_TOKEN` only on the Harness worker. Run one challenge with:

```bash
harness tsec --agent agents/examples/pi-tsec.yaml
```

Use `--challenge WEB-001` for a specific challenge or `--all` for every unfinished challenge. Harness performs the SDK lifecycle and never passes the Benchmark token into Pi. Pi receives only the challenge description and target addresses and emits candidates as `HARNESS_FLAG=<flag>`. Candidate plaintext is submitted through the SDK; trace records only the SHA256 of each candidate. Challenge close is always executed in `finally`.

For VPN-backed targets, `agents/examples/pi-tsec.yaml` uses `network: host` so the Kali container shares the Linux worker's VPN routes. See [`docs/tsec.md`](docs/tsec.md).

## Distributed control plane

The reference control plane uses SQLite for durable job state and leases. Workers communicate only through HTTP, so they can run on different machines as long as each worker has the same benchmark/agent checkout or compatible workspace layout.

Set an authentication token and start the control plane:

```bash
export HARNESS_CONTROL_TOKEN="replace-with-a-random-secret"

harness serve \
  --queue-db .harness/control.db \
  --runs-root .harness/runs \
  --benchmarks-root benchmarks \
  --host 0.0.0.0 \
  --port 8780
```

Submit a job using worker-visible repository-relative paths:

```bash
harness submit benchmarks/examples/hello/task.yaml \
  --agent agents/examples/demo.yaml \
  --control-url http://control-plane:8780 \
  --runs-root .harness/runs \
  --seed 100
```

Start a worker:

```bash
export HARNESS_CONTROL_TOKEN="replace-with-a-random-secret"

harness worker \
  --control-url http://control-plane:8780 \
  --workspace-root /srv/red-harness
```

Host-process CLI jobs are rejected by workers unless the worker is explicitly started with:

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

Provider API keys are never placed in queue payloads. Jobs store only the environment-variable name, such as `HARNESS_MODEL_API_KEY`; the Worker resolves the actual secret locally.

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
GET  /v1/skills
GET  /v1/leaderboard
GET  /v1/capabilities

GET  /v1/runs/{run_id}
GET  /v1/runs/{run_id}/trace
GET  /v1/runs/{run_id}/world
GET  /v1/runs/{run_id}/world/events
GET  /v1/runs/{run_id}/plan
POST /v1/runs/{run_id}/plan/publish
GET  /v1/work
POST /v1/work
POST /v1/work/claim
POST /v1/work/{work_id}/heartbeat
POST /v1/work/{work_id}/complete
POST /v1/work/{work_id}/fail
POST /v1/work/{work_id}/release
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
harness otel-export .harness/runs/run_... \
  --output trace.otlp.json
```

Each Red Harness event becomes a span carrying run/task/actor/event attributes plus serialized event data. This export is intentionally file/HTTP payload generation in v0.7; direct collector delivery can be added without modifying the trace recorder.

## Gateway modes

### Host mode

Useful for trusted local development and the first-class Pi adapter:

```bash
harness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/gateway-demo.yaml \
  --allow-host-agent \
  --gateway \
  --gateway-policy examples/gateway-policy.yaml
```

The Agent receives only a one-time Gateway token. Real provider credentials remain on the Harness/Gateway side.

### Isolated Docker sidecar

```bash
docker build -f docker/gateway/Dockerfile \
  -t harness-gateway:local .

harness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/docker-gateway.yaml \
  --gateway \
  --gateway-mode sidecar \
  --gateway-image harness-gateway:local \
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

Pi built-in tools do not pass through this Tool Gateway in v0.8; they are observed through Pi's JSON event stream and execute inside the Kali container. The container receives no Docker socket and no host filesystem mounts beyond the benchmark task/run paths.

## Execution profiles

Docker Agent:

```yaml
apiVersion: harness/v1
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
harness capabilities
```

Output reports availability of:

```text
docker
gvisor_runsc
firecracker
kvm
pi
```

### Firecracker contract

v0.7 contains a `FirecrackerProfile` that validates kernel/rootfs images and generates the boot-source, root drive, and machine configuration expected by a future Firecracker backend. `FirecrackerBackend.validate_host()` requires both the `firecracker` binary and `/dev/kvm`.

This is deliberately not advertised as a runnable backend yet. VM lifecycle, jailer/network setup, snapshotting, and run-bundle mounts remain future work.

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

Each run is stored beneath `.harness/runs/<run_id>/`:

```text
result.json
trace.jsonl
events.jsonl
world.db
world.events.jsonl
world.snapshot.json
world.context.txt
agent.stdout.log
agent.stderr.log
verifier.stdout.log
verifier.stderr.log
```

For Pi runs, `agent.stdout.log` is the raw Pi JSON event stream.

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
               ┌───────┴────────┐
               │                │
          Generic Agent      Pi Adapter
                                │
                          Pi JSON protocol
                       │
                Trace + Result
                       │
              Trace API / OTLP
```

World-state design and extension guidance is documented in [`docs/world-state.md`](docs/world-state.md). Reusable technique metadata uses `harness/skill/v1`; skills describe state preconditions and possible outcomes but are not executable code. The core state schema is domain-neutral and does not require a DAG; graph and timeline representations are derived views over the event-sourced state.

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
7. **Done:** first-class Pi JSON adapter, Pi usage/tool normalization, isolated Pi config, Gateway model routing, Pi capability detection.
8. PostgreSQL/Redis queue backend, containerized Pi adapter, KVM-enabled Firecracker lifecycle, snapshot pooling, OpenTelemetry collector delivery, browser trace UI, signed benchmark registry, and multi-tenant scheduling.


### Benchmark adapters

External benchmark platforms use a common contract:

```text
discover -> BenchmarkCase
provision -> BenchmarkSession
Agent run
submit -> SubmissionResult
evaluate -> EvaluationResult
teardown
```

A `BenchmarkSession` contains a normalized objective, targets, and benchmark metadata. The generic
`BenchmarkRunner` seeds targets into the World Model, builds `world.context.txt`, runs the selected
Agent adapter, ingests world-state submissions, sends benchmark submissions, records evaluation,
and always tears the external session down.

TSecBench is implemented through this contract. Its SDK token stays in the Harness process and is
never passed to the Agent. `HARNESS_FLAG=<flag>` remains a compatibility submission extractor;
submission values are hashed in normalized traces.


### Solver session runtime

The benchmark runtime can now start Agents through a bidirectional `AgentSession` compatibility
boundary while retaining the legacy `run()` API. Sessions expose normalized events, trusted
observations, checkpoints, close requests, and the eventual `AgentResult`.

Externally evaluated benchmarks may accept structured submissions through
`HARNESS_SUBMISSION_INBOX` and write trusted evaluator feedback to
`HARNESS_FEEDBACK_FILE`. TSec uses this path for online flag feedback while retaining
`HARNESS_FLAG=<flag>` as a compatibility fallback.

Each benchmark run also maintains `progress.json` (`harness/progress/v1`) separately from
World State. It tracks active solver progress, recent action status, no-progress/failure counters,
submission outcomes, and objective completion. Containerized Pi sessions can be actively stopped
when the evaluator reports objective completion.


### Rolling-horizon planning

The Harness exposes an optional short-horizon planner at:

```text
GET /v1/runs/{run_id}/plan/rolling?horizon=1..3
```

It combines the current World Snapshot, Skill Registry, and `progress.json` to return at most three
`PlannedAction` records. Planner v2 ranks applicable skills using goal relevance, expected
information gain, novelty, declared and observed success prior, execution friction, repeated-use
penalty, and per-skill failure history. Each action includes the resulting rationale and explicit
replan triggers such as action failure, missing expected observations, changed world revision,
repeated no-progress, or contradicted hypotheses.

The rolling planner is also used by the interactive SolverLoop. A session receives an initial
`solver.plan.updated` observation and receives a new plan when verification, repeated no-progress,
or benchmark feedback requests replanning. Plans are persisted to `plan.json` and guide the Agent;
the Harness still does not execute Skill metadata directly as commands. Coordination-plane work
publication remains explicit.


### SQLite World State

SQLite is the default World State backend. `world.db` is authoritative; `world.events.jsonl` and
`world.snapshot.json` are compatibility/debug exports. The database uses WAL mode, persistent event
sequence numbers, event-id deduplication, and optional optimistic `expected_revision` checks.

Incremental consumers can request:

```text
GET /v1/runs/{run_id}/world/events?after_sequence=<sequence>
```

Each returned event includes its persistent `sequence` cursor. Legacy JSONL state can still be
passed to `--resume-world`; new runs may resume directly from a previous `world.db`.
