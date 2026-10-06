# Universal World State Runtime

Red Harness keeps offensive-agent execution separate from agent reasoning. The world-state
runtime adds durable, replayable state without requiring the Harness to own the Agent's planner.

## Design principles

- **Events are authoritative.** `world.events.jsonl` is append-only.
- **Snapshots are derived.** `world.snapshot.json` is rebuilt from events and is safe to discard.
- **Core defines mechanics, domains define semantics.** Core type strings are intentionally open.
- **The world is not assumed to be a DAG.** Relations may form arbitrary directed graphs.
- **Beliefs are not facts.** Observations and hypotheses may carry confidence and provenance.
- **Capabilities are first-class.** Offensive progress is often best represented by what the Agent
  can now do rather than by a fixed task graph.
- **Reasoning remains optional.** Agents may plan internally; future Harness planners can consume
  the same state contract.

## Core objects

The v1 state schema contains:

- `Entity`: any world object such as a host, endpoint, process, binary, principal, role, or resource.
- `Relation`: a typed directed relation between state objects.
- `Observation`: an observed result with optional confidence and source.
- `Artifact`: a durable or addressable object such as a file, dump, token bundle, or report.
- `Capability`: an ability acquired by an Agent within a scope.
- `Hypothesis`: a testable belief with confidence and lifecycle status.
- `Goal`: a persistent objective or sub-objective.
- `ActionRecord`: a planned/running/completed offensive action with optional target.
- `Constraint`: an active/satisfied/violated/expired execution or policy boundary.
- `Failure`: a structured failed attempt or execution condition.

The core does not define security-domain taxonomies. Extensions can use namespaced type strings,
for example `web.endpoint`, `binary.elf`, `cloud.principal`, or `ad.dcsync`.

## Persistence model

Each mutation is represented as a `WorldEvent`:

```json
{
  "schema_version": "redharness.world/v1",
  "kind": "capability",
  "op": "upsert",
  "object": {
    "id": "cap-1",
    "type": "network.tcp_connect",
    "subject": "agent",
    "scope": "target:443",
    "attributes": {}
  },
  "actor": "agent"
}
```

`WorldReducer` folds events into `WorldSnapshot`. `WorldStore` persists an event and atomically
materializes the current snapshot after each mutation.

## Run integration

Every normal Harness run creates:

```text
world.events.jsonl
world.snapshot.json
```

The task objective is seeded as the root Goal when the run starts and updated with the final
success, score, and run status when execution finishes.

Agents receive a `REDHARNESS_WORLD_INBOX` path. They may append untrusted JSONL submissions
using the `redharness.world.submit/v1` envelope. The Harness validates each submission and copies
accepted mutations into the authoritative world event log with the actor forced to `agent`.

Example:

```json
{"schema_version":"redharness.world.submit/v1","kind":"capability","op":"upsert","object":{"id":"cap-shell","type":"host.shell","subject":"agent","scope":"host-1","attributes":{}}}
```

Agents never write `world.events.jsonl` or `world.snapshot.json` directly. Invalid inbox lines are
rejected and summarized in the Harness trace. This keeps the world log replayable even when an
Agent emits malformed or adversarial state.

Pi receives a concise protocol instruction in its task prompt. Generic CLI/Docker agents can opt
in simply by detecting `REDHARNESS_WORLD_INBOX`.

## Planned evolution

1. **Done:** validated Agent-facing state-event ingestion protocol.\n2. **Done:** compact context builder over persisted world state.\n3. **Done:** resume from a prior world event log using a fresh Agent session.\n4. Domain extension packages and skill metadata.\n5. Optional rolling-horizon planner.\n6. Shared blackboard for multi-agent execution.\n7. Derived graph/timeline views and transition-model research.\n

## Context and resume

Each run materializes `world.context.txt` from the current snapshot and exposes its path as
`REDHARNESS_WORLD_CONTEXT`. The projection prioritizes goals, capabilities, open hypotheses,
artifacts, bounded observations, recent failures, and relations.

A new run may continue from a previous authoritative event log:

```bash
redharness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/pi.yaml \
  --resume-world .redharness/runs/<run-id>/world.events.jsonl
```

The new run copies and replays the prior world event log, re-activates the root task goal, builds a
fresh context projection, and starts a new Agent session. It does not replay or depend on the old
provider transcript.
