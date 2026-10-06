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

This initial integration deliberately does not extract cognitive state from model prose or tool
output. A later layer can ingest explicit Agent events or domain adapters without changing the
world-state storage contract.

## Planned evolution

1. Agent-facing state-event ingestion protocol.
2. Context builder over relevant world state.
3. Resume from world state using a fresh Agent session.
4. Domain extension packages and skill metadata.
5. Optional rolling-horizon planner.
6. Shared blackboard for multi-agent execution.
7. Derived graph/timeline views and transition-model research.
