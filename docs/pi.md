# Pi integration

Red Harness v0.8 runs Pi inside a dedicated Kali Rolling Docker image and consumes Pi's JSON protocol for model/tool telemetry.

## Build the Pi/Kali image

```bash
docker build -t redharness/pi-kali:local docker/pi-kali
```

The image contains:

- `kalilinux/kali-rolling`
- `kali-linux-core`
- common network, web, credential, packet, and binary-analysis tools
- Python 3
- Node.js
- Pi 1.0.4

## Agent contract

```yaml
apiVersion: redharness/v1
id: pi-openai-sol
type: pi
image: redharness/pi-kali:local
network: environment

pi:
  provider: openai
  model: gpt-5.6-sol
  thinking: medium
  tools: [read, bash, edit, write]
  env_passthrough: [OPENAI_API_KEY]
  cap_add: [NET_RAW]

  approve_project: false
  context_files: false
  extensions: false
  skills: false
  prompt_templates: false
  themes: false
  mcp: false
  offline: true
```

For VPN-backed benchmark targets on Linux workers, `network: host` is supported. It lets the Pi/Kali container share the worker's VPN routes. `network: none` disables networking.

## Direct-provider mode

```bash
export OPENAI_API_KEY="..."

redharness run benchmark/task.yaml \
  --agent agents/examples/pi.yaml
```

Only environment variables explicitly named by `pi.env_passthrough` are copied into the container.

## Harness model Gateway

Pi can also use the Red Harness model proxy. In host-Gateway mode the container receives a one-time Gateway token. In sidecar mode the Pi container joins the private Gateway Docker network.

```bash
export REDHARNESS_MODEL_API_KEY="real-provider-key"

redharness run benchmark/task.yaml \
  --agent agents/examples/pi.yaml \
  --gateway \
  --model-upstream https://provider.example/v1
```

For each run Harness writes a private `models.json` under the run directory. The upstream provider credential remains in Harness/Gateway and is not copied into the Pi container.

`network: host` and sidecar Gateway mode are intentionally incompatible because sidecar mode requires the Agent to join the Gateway's private Docker network.

## Event normalization

| Pi JSON event | Red Harness event |
| --- | --- |
| assistant `message_start` | `model.request` |
| assistant `message_end` | `model.response` + `model.usage` |
| `tool_execution_start` | `tool.call` |
| `tool_execution_end` | `tool.result` |
| `session` | `pi.session` |
| `agent_settled` | `pi.agent_settled` |

Pi usage contributes input/output/total tokens and model cost to the normal Harness budgets. When the Harness model Gateway is enabled, Gateway model usage is authoritative so model usage is not double-counted.

Raw Pi JSONL is retained in `agent.stdout.log`.

## Container security defaults

Harness creates the Pi container with:

- read-only root filesystem
- all Linux capabilities dropped, then only `pi.cap_add` restored
- `no-new-privileges`
- CPU, RAM, PID, and tmpfs limits
- task mounted read-only
- run directory mounted read/write
- no Docker socket
- explicit network selection
- per-run Pi config/session directories

Pi is also started with deterministic non-interactive defaults:

```text
--mode json
--no-session
--no-approve
--no-context-files
--no-extensions
--no-skills
--no-prompt-templates
--no-themes
--no-mcp
--offline
```

## TSec Benchmark

For TSec Benchmark integration use `agents/examples/pi-tsec.yaml` and see `docs/tsec.md`. The TSec token stays in Harness; it is never copied into the Pi container.
