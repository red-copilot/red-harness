# Pi integration

Red Harness runs Pi inside a dedicated Kali Docker image pinned to the Kali last-snapshot release.
It uses the bidirectional RPC
protocol by default, with one-shot JSON mode available for compatibility.

## Build the Pi/Kali image

```bash
docker build -t harness/pi-kali:local docker/pi-kali
```

The image contains:

- `kalilinux/kali-last-release`, pinned by OCI digest in `docker/pi-kali/Dockerfile`
- `kali-linux-core`
- common network, web, credential, packet, and binary-analysis tools
- Python 3
- Node.js
- Pi 1.0.4

## Agent contract

```yaml
apiVersion: harness/v1
id: pi-openai-sol
type: pi
image: harness/pi-kali:local
network: environment

pi:
  mode: rpc # default; set to json for the one-shot compatibility adapter
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

harness run benchmark/task.yaml \
  --agent agents/examples/pi.yaml
```

Only environment variables explicitly named by `pi.env_passthrough` are copied into the container.
Names containing `TOKEN` and reserved Harness, benchmark, or TSec credential names are rejected
for container Agents. Direct provider API keys such as `OPENAI_API_KEY` remain explicitly
configurable; use the Harness Gateway when the provider credential should stay outside Pi.

## Harness model Gateway

Pi can also use the Red Harness model proxy. In host-Gateway mode the container receives a one-time Gateway token. In sidecar mode the Pi container joins the private Gateway Docker network.

```bash
export HARNESS_MODEL_API_KEY="real-provider-key"

harness run benchmark/task.yaml \
  --agent agents/examples/pi.yaml \
  --gateway \
  --model-upstream https://provider.example/v1
```

For each run Harness writes a private `models.json` under the run directory. The upstream provider credential remains in Harness/Gateway and is not copied into the Pi container.

`network: host` and sidecar Gateway mode are intentionally incompatible because sidecar mode requires the Agent to join the Gateway's private Docker network.

## RPC interaction and event normalization

Harness starts persistent `pi --mode rpc` sessions by default, waits for the startup `session`
event, then writes a baseline checkpoint before sending any prompt. On recovery it passes the saved
session ID through `--session <id>` and refuses startup if Pi reports a different session. It sends
the objective and initial rolling plan as a `prompt` command and waits for `agent_settled` before
completing the session. It continues reading stdout while the model and tools run. Verification,
refreshed World state, benchmark feedback, and replanning instructions are sent as `steer` commands;
steering uses Pi's `all` mode so feedback collected during a tool turn reaches the next model call
together. RPC commands carry request IDs, and JSONL framing splits on LF only.

| Pi JSON event | Red Harness event |
| --- | --- |
| assistant `message_start` | `model.request` |
| assistant `message_end` | `model.response` + `model.usage`, or `model.usage_missing` when provider usage is absent |
| `tool_execution_start` | `tool.call` |
| `tool_execution_end` | `tool.result` |
| startup `session` event (RPC or JSON mode) | `pi.session` |
| `agent_settled` | `pi.agent_settled` |

Pi usage contributes input/output/total tokens and model cost to the normal Harness budgets. When the Harness model Gateway is enabled, Gateway model usage is authoritative so model usage is not double-counted.

Raw Pi JSONL is retained in `agent.stdout.log`. `agent_end` is not treated as session completion
because Pi can continue retries or queued work; `agent_settled` defines the completed turn.

## Container security defaults

Harness creates the Pi container with:

- non-root numeric identity matching the writable workspace owner
- read-only root filesystem
- all Linux capabilities dropped, then only `pi.cap_add` restored
- `no-new-privileges`
- CPU, RAM, PID, and tmpfs limits
- task mounted read-only
- only `agent-workspace/` mounted read/write; authoritative World, result, trace, checkpoint,
  evidence, and verifier files remain outside the Agent mount
- no Docker socket
- explicit network selection
- per-run Pi config/session directories

`pi.cap_add` defaults to an empty list. Add `NET_RAW` only for tasks that need raw sockets.

Pi is also started with deterministic non-interactive defaults:

```text
--mode rpc
--no-approve
--no-context-files
--no-extensions
--no-skills
--no-prompt-templates
--no-themes
--no-mcp
--offline
```

`--no-session` is used for JSON compatibility runs only. RPC sessions persist so an interrupted
run can continue from its checkpointed Pi session ID.

Container Agents also support `network_profile: target-only`, which requires the Compose target
network to be marked `internal: true`; `model-allowed`, which requires a Docker Agent and sidecar
Gateway; and `fully-offline`, which forces Docker `--network none`. A `benchmark-only` profile is
retained for older configurations but is advisory and does not block public egress. Host CLI
Agents cannot enforce the strict profiles.

Set `pi.mode: json` to use the one-shot `--mode json` adapter. RPC mode does not accept positional
prompt files; Harness sends the prompt over stdin.

## TSec Benchmark

For TSec Benchmark integration use `agents/examples/pi-tsec.yaml` and see `docs/tsec.md`. The TSec token stays in Harness; it is never copied into the Pi container.

See [the Agent execution boundary](security/execution-boundary.md) for the full threat model,
workspace contract, secret handling, and artifact trust rules.
