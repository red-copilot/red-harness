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
  --max-cost-usd 20
```

## Security boundary

The Pi container never receives `BENCHMARK_TOKEN` or `BENCHMARK_BASE_URL`. Red Harness calls the SDK to start, submit and close challenges. Pi emits candidate answers as:

```text
REDHARNESS_FLAG=flag{...}
```

Harness extracts them from Pi JSONL and submits them through the SDK. Trace records a SHA256 hash of candidate flags, not plaintext flag values.

Challenge close is executed in `finally`, so target resources are released even when Pi or submission fails.
