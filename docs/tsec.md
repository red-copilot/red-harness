# TSec Benchmark SDK integration

Red Harness owns the TSec control-plane credential and challenge lifecycle. The Pi Agent runs inside the Kali container and receives only the challenge description and target addresses.

## Install

```bash
python -m pip install -e ".[tsec]"
docker build -t redharness/pi-kali:local docker/pi-kali
```

Configure the platform SDK:

```bash
export BENCHMARK_BASE_URL="https://benchmark.example.com"
export BENCHMARK_TOKEN="..."
export OPENAI_API_KEY="..."
```

The worker must already have the Benchmark VPN connected.

## Agent

```yaml
apiVersion: redharness/v1
id: pi-kali-tsec
type: pi
image: redharness/pi-kali:local
network: host
pi:
  provider: openai
  model: gpt-5.6-sol
  tools: [read, bash, edit, write]
  env_passthrough: [OPENAI_API_KEY]
  cap_add: [NET_RAW]
```

On Linux, `network: host` is the recommended TSec mode because the Kali container directly shares the Worker network namespace and VPN routes. If your VPN permits Docker bridge forwarding, `network: environment` can be used instead.

## Run

One unfinished challenge:

```bash
redharness tsec --agent agents/examples/pi-tsec.yaml
```

Specific challenge:

```bash
redharness tsec \
  --agent agents/examples/pi-tsec.yaml \
  --challenge WEB-001
```

All unfinished challenges:

```bash
redharness tsec \
  --agent agents/examples/pi-tsec.yaml \
  --all
```

Optional limits:

```bash
redharness tsec \
  --agent agents/examples/pi-tsec.yaml \
  --all \
  --wall-time 3600 \
  --max-tokens 200000 \
  --max-model-calls 300 \
  --max-tool-calls 1000 \
  --max-cost-usd 20 \
  --start-retries 2 \
  --retry-delay 2
```

If the platform returns `ResourceUnavailable` while starting a challenge, Harness retries according to `--start-retries` and `--retry-delay`. Other SDK state errors are recorded in the trace and surfaced instead of being guessed around.

Hints are opt-in because the TSec SDK documents that viewing a hint reduces flag score:

```bash
redharness tsec \
  --agent agents/examples/pi-tsec.yaml \
  --challenge WEB-001 \
  --hint
```

Harness calls `get_hint()` only when `--hint` is present and records that a score penalty is expected.

## Security boundary

The Pi container never receives `BENCHMARK_TOKEN` or `BENCHMARK_BASE_URL`. Red Harness calls the SDK to start, submit and close challenges. Pi emits candidate answers as:

```text
REDHARNESS_FLAG=flag{...}
```

Harness extracts them from Pi JSONL and submits them through the SDK. Trace records a SHA256 hash of candidate flags, not plaintext flag values.

Challenge close is executed in `finally`, so target resources are released even when Pi or submission fails.


## Progress and score semantics

Harness preserves the SDK's distinction between challenge score and platform cumulative score.

A TSec result records:

```json
{
  "score": 35,
  "platform_cumulative_score": 935,
  "benchmark": {
    "challenge_total_score": 120,
    "hint_used": true
  },
  "flags": {
    "expected": 2,
    "initial_correct": 1,
    "remaining_at_start": 1,
    "correct": 2
  }
}
```

`score` is the sum of `SubmitResult.awarded` values produced by this Harness run. `platform_cumulative_score` is the SDK's `cumulative_score` and is kept separate because it is cumulative platform progress, not necessarily the score earned by the current challenge attempt.

If a challenge was partially solved before the run, Pi is told only how many flags remain. `DuplicateSubmit` is treated as an idempotent submission and does not expose the plaintext flag in trace output.

The SDK context manager performs the VPN preflight before challenge API calls. Harness converts a `VpnCheckError` into an actionable TSec adapter error while preserving the SDK-provided detail reason.
