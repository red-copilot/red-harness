# Pi integration

Red Harness v0.7 includes a first-class adapter for [Pi](https://pi.dev/docs/latest).

Pi is executed in JSON mode so Harness can consume its structured session, model, and tool events. The adapter currently uses the Pi CLI process boundary rather than the TypeScript SDK or long-lived RPC mode.

## Install Pi

Pi 1.0.4 is the version pinned by the Red Harness CI workflow.

```bash
npm install -g --ignore-scripts @earendil-works/pi-coding-agent@1.0.4
pi --version
```

Pi requires Node.js 22.19 or newer.

## Direct provider mode

Example `agents/pi.yaml`:

```yaml
apiVersion: redharness/v1
id: pi-openai-sol
type: pi

env: {}

pi:
  provider: openai
  model: gpt-5.6-sol
  thinking: medium
  tools: [read, bash, edit, write]

  # Deterministic evaluation defaults.
  approve_project: false
  context_files: false
  extensions: false
  skills: false
  prompt_templates: false
  themes: false
  mcp: false
  offline: true
```

Provide the provider credential through the environment, for example:

```bash
export OPENAI_API_KEY="..."

redharness run benchmark/task.yaml \
  --agent agents/pi.yaml \
  --allow-host-agent
```

The Pi adapter intentionally uses a per-run `PI_CODING_AGENT_DIR` and an ephemeral session. Stored credentials from the user's normal Pi directory are not imported into the evaluation by default.

## Harness model Gateway mode

Pi can also be forced through the Red Harness model proxy:

```bash
export REDHARNESS_MODEL_API_KEY="real-provider-key"

redharness run benchmark/task.yaml \
  --agent agents/pi.yaml \
  --allow-host-agent \
  --gateway \
  --model-upstream https://provider.example/v1 \
  --input-price-per-million 2.0 \
  --output-price-per-million 10.0
```

For each run Harness writes an isolated Pi `models.json` containing a `redharness` provider:

```json
{
  "providers": {
    "redharness": {
      "baseUrl": "http://127.0.0.1:<port>/v1",
      "api": "openai-completions",
      "apiKey": "$REDHARNESS_GATEWAY_TOKEN",
      "models": [{"id": "gpt-5.6-sol"}]
    }
  }
}
```

The Pi process sees only the one-time `REDHARNESS_GATEWAY_TOKEN`. The upstream provider credential remains inside the Harness Gateway process.

The host Pi adapter supports the host Gateway mode. Docker sidecar Gateway mode is intentionally rejected for `type: pi` because the host Pi process is not attached to the sidecar's private Docker network.

## Event normalization

Pi JSON events are normalized into the existing Harness trace and budget protocol.

| Pi JSON event | Red Harness event |
| --- | --- |
| assistant `message_start` | `model.request` |
| assistant `message_end` | `model.response` + `model.usage` |
| `tool_execution_start` | `tool.call` |
| `tool_execution_end` | `tool.result` |
| `session` | `pi.session` |
| `agent_settled` | `pi.agent_settled` |

Pi usage fields are preserved where possible, including input/output/total tokens, cache read/write tokens, reasoning tokens, and total model cost.

When the Harness model Gateway is enabled, model usage comes from the Gateway rather than being counted again from Pi's JSON stream. Pi tool events are still normalized because Pi's built-in tools execute inside the Pi process.

The raw Pi JSONL stream remains available as `agent.stdout.log` in the run bundle.

## Evaluation safety defaults

The first-class adapter starts Pi with non-interactive evaluation defaults equivalent to:

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

The selected built-in tools are supplied explicitly with `--tools`.

These defaults prevent benchmark directories from silently loading project Pi extensions, skills, MCP configuration, prompt templates, or context files. Individual fields can be enabled explicitly in the agent contract when a benchmark intentionally depends on them.

## Security boundary

Pi's own project trust is not a sandbox. Its built-in tools run with the permissions of the Pi process. For that reason `type: pi` currently requires `--allow-host-agent`, just like the generic trusted CLI adapter.

For hostile or public benchmark workloads, use a containerized agent boundary. A dedicated containerized Pi adapter is a later hardening step; using the host Pi adapter is intended for trusted development and controlled evaluation workers.

## Why JSON mode instead of RPC

JSON mode is currently the best fit for one Harness attempt:

- one process per attempt
- deterministic completion after `agent_settled`
- strict JSONL framing
- structured model usage and cost
- structured tool lifecycle
- no long-lived session state to leak across benchmark attempts

RPC remains a good future option for warm Pi worker pools where process startup becomes material.
