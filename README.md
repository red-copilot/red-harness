# Red Harness

Red Harness is a reproducible evaluation runtime for security agents. It separates benchmark tasks, agents, target environments, model/tool access, budgets, verification, and scoring so results can be independently checked and compared.

## Current capabilities

Version 0.5 provides:

- declarative task, agent, suite, and gateway-policy contracts
- Docker Compose or no-op benchmark environments
- trusted local CLI agents and restricted Docker agents
- Python or Docker-sandboxed verifiers
- append-only JSONL traces and result bundles
- real-time wall-time, token, model-call, tool-call, and cost budgets
- policy-gated Tool Gateway
- OpenAI-compatible streaming and non-streaming model proxy
- provider credential isolation
- host Gateway mode for development
- isolated Docker Gateway sidecar mode
- Docker runtime profiles, including gVisor via `runtime: runsc`
- parallel suite workers
- standard pass@k estimation
- weighted score and per-domain aggregation
- GitHub Actions coverage for real Docker Agent, Gateway, sidecar, and Docker Verifier flows

> [!WARNING]
> CLI agents and `verification.type: python` execute trusted host processes. For untrusted evaluation inputs, use Docker agents and Docker verifiers. The `runtime: runsc` profile requires gVisor to be installed and registered with the local Docker daemon. Firecracker is not implemented in v0.5.

## Install

```bash
python -m pip install -e ".[dev]"
```

Python 3.11+ is required. Docker is required for Docker agents, sidecar Gateway mode, Docker verifiers, and Docker Compose benchmark environments.

## Quick start

Validate the example contracts:

```bash
redharness validate benchmarks/examples/hello/task.yaml
redharness validate-suite benchmarks/examples/smoke-suite.yaml
```

Run the trusted local smoke agent:

```bash
redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent \
  --seed 42
```

Run the smoke suite in parallel:

```bash
redharness suite benchmarks/examples/smoke-suite.yaml \
  --agent agents/examples/demo.yaml \
  --allow-host-agent \
  --workers 2
```

## Suite contract and scoring

A suite can declare parallelism and pass@k targets:

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

Each repeat remains a fully independent run with its own trace, budget, verifier, and seed.

Suite summaries contain:

```json
{
  "workers": 4,
  "success_rate": 0.6,
  "weighted_score": 72.5,
  "pass_at_k": {
    "1": 0.6,
    "3": 0.94,
    "5": 1.0
  },
  "domains": {
    "web": {
      "weighted_score": 72.5,
      "weighted_success_rate": 0.6,
      "pass_at_k": {
        "1": 0.6,
        "3": 0.94,
        "5": 1.0
      }
    }
  }
}
```

pass@k uses the standard unbiased estimator:

```text
pass@k = 1 - C(n-c, k) / C(n, k)
```

where `n` is the number of attempts and `c` is the number of successful attempts. Domain grouping comes from each task's `category`.

## Gateway modes

### Host mode

Host mode is convenient for trusted local development:

```bash
redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/gateway-demo.yaml \
  --allow-host-agent \
  --gateway \
  --gateway-policy examples/gateway-policy.yaml
```

Harness injects:

```text
REDHARNESS_GATEWAY_URL
REDHARNESS_GATEWAY_TOKEN
OPENAI_BASE_URL
OPENAI_API_KEY
```

The injected API key is a one-time Gateway token. The actual model-provider credential remains on the Harness/Gateway side.

### Docker sidecar mode

Build the supplied Gateway image:

```bash
docker build -f docker/gateway/Dockerfile \
  -t redharness-gateway:local .
```

Then run a Docker Agent through an isolated sidecar:

```bash
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

The Gateway publishes no host port. The Agent can join both the private Gateway network and the benchmark environment network. Target containers do not need to share the Gateway network.

If a model upstream is configured, only the Gateway sidecar is additionally attached to an egress-capable Docker bridge. The private Agent-to-Gateway network remains internal.

`network: none` is intentionally incompatible with any Gateway mode because it means no Agent networking.

## Model proxy

Configure the real provider credential only in the Harness environment:

```bash
export REDHARNESS_MODEL_API_KEY="..."
```

Then:

```bash
redharness run benchmark/task.yaml \
  --agent agents/my-agent.yaml \
  --gateway \
  --gateway-mode sidecar \
  --gateway-image redharness-gateway:local \
  --model-upstream https://provider.example/v1 \
  --input-price-per-million 2.50 \
  --output-price-per-million 10.00
```

For non-streaming requests, usage is read from the provider response. For streaming requests, the proxy forces `stream_options.include_usage=true`, relays SSE chunks, and records the final usage/cost event.

If an upstream omits usage, Harness records:

```text
model.usage_missing
```

rather than fabricating token counts.

## Tool Gateway

The built-in v0.5 tools remain intentionally narrow:

```text
file.read
file.write
```

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

- `file.read` is restricted to the task directory and run workspace
- `file.write` is restricted to the run workspace
- path traversal outside those roots is rejected
- denied calls still count against `max_tool_calls`
- read and write sizes are policy-limited
- no host-side arbitrary shell tool is exposed

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

When `runtime` is set, Harness passes it directly through Docker's `--runtime` flag. A standard Docker installation can omit the field. A gVisor installation typically uses `runsc`.

The Docker Agent sandbox also uses:

- read-only root filesystem
- all Linux capabilities dropped
- `no-new-privileges`
- PID, CPU, and memory limits
- isolated tmpfs
- task mounted read-only
- run workspace mounted read/write
- no Docker socket mount

Docker Verifier uses a read-only run bundle as well.

## Budgets and accounting

Task budgets:

```yaml
budgets:
  wall_time: 3600
  max_tokens: 200000
  max_model_calls: 300
  max_tool_calls: 1000
  max_cost_usd: 20
```

Exceeding an enforced budget terminates the Agent and returns:

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

## Architecture

```text
Task / Suite / Seed
        │
        v
   Orchestrator
        │
        ├── Environment Provider
        │
        ├── Gateway Runtime
        │      ├── host
        │      └── isolated Docker sidecar
        │              ├── Tool Policy
        │              └── Model Proxy
        │
        ├── Agent Adapter
        │      ├── trusted CLI
        │      └── Docker / custom runtime
        │
        ├── Budget Monitor
        ├── Trace Recorder
        └── Verifier
               ├── trusted Python
               └── Docker / custom runtime

SuiteRunner
   ├── parallel workers
   ├── weighted score
   ├── pass@k
   └── domain aggregation
```

Core separation rule:

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

Suite summaries are written beneath `.redharness/suites/<suite_run_id>/result.json`.

## Roadmap

1. **Done:** task/environment/verifier/result execution contract.
2. **Done:** Docker Agent, budgets, repeat/seed, suites.
3. **Done:** Tool Gateway, model proxy, credential isolation, streaming accounting.
4. **Done:** Docker Verifier sandbox and Docker Agent Gateway support.
5. **Done:** isolated Gateway sidecar, Docker runtime profiles, parallel workers, pass@k, domain scoring.
6. Firecracker/microVM backend, distributed worker queue, OpenTelemetry export, trace UI, benchmark registry, and leaderboard.
