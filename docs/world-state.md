# Universal World State Runtime

Red Harness keeps offensive-agent execution separate from agent reasoning. The world-state
runtime adds durable, replayable state without requiring the Harness to own the Agent's planner.

## Design principles

- **Events are authoritative.** The SQLite `world_events` stream in `world.db` is canonical.\n- **Projections are derived.** `world_objects`, `world.snapshot.json`, and JSONL exports can be rebuilt from events.
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
  "schema_version": "harness/world/v1",
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
world.db                 # authoritative transactional state
world.events.jsonl       # compatibility/debug export
world.snapshot.json      # compatibility/debug export
```

The task objective is seeded as the root Goal when the run starts and updated with the final
success, score, and run status when execution finishes.

Agents receive a `HARNESS_WORLD_INBOX` path. They may append untrusted JSONL submissions
using the `harness/world-submission/v1` envelope. The Harness validates each submission and copies
accepted mutations into the authoritative world event log with the actor forced to `agent`.

Example:

```json
{"schema_version":"harness/world-submission/v1","kind":"capability","op":"upsert","object":{"id":"cap-shell","type":"host.shell","subject":"agent","scope":"host-1","attributes":{}}}
```

Agents never write `world.db`, `world.events.jsonl`, or `world.snapshot.json` directly. Invalid inbox lines are
rejected and summarized in the Harness trace. This keeps the world log replayable even when an
Agent emits malformed or adversarial state.

Pi receives a concise protocol instruction in its task prompt. Generic CLI/Docker agents can opt
in simply by detecting `HARNESS_WORLD_INBOX`.

## Planned evolution

1. **Done:** validated Agent-facing state-event ingestion protocol.\n2. **Done:** compact context builder over persisted world state.\n3. **Done:** resume from a prior world event log using a fresh Agent session.\n4. Domain extension packages and skill metadata.\n5. Optional rolling-horizon planner.\n6. Shared blackboard for multi-agent execution.\n7. Derived graph/timeline views and transition-model research.\n

## Context and resume

Each run materializes `world.context.txt` from the current snapshot and exposes its path as
`HARNESS_WORLD_CONTEXT`. The projection prioritizes goals, capabilities, open hypotheses,
artifacts, bounded observations, recent failures, and relations.

A new run may continue directly from a previous SQLite state database:

```bash
harness run benchmarks/examples/hello/task.yaml \
  --agent agents/examples/pi.yaml \
  --resume-world .harness/runs/<run-id>/world.db
```

The new run copies the prior SQLite state database, re-activates the root task goal, builds a fresh
context projection, and starts a new Agent session. Legacy `world.events.jsonl` files are still
accepted and imported into SQLite on first open. Provider transcripts are never required.


## Repository boundary

World-state consumers depend on the `WorldRepository` protocol rather than on JSONL paths. The
default implementation is `SQLiteWorldRepository`, backed by `world.db` with WAL, persistent
event sequence numbers, event-id deduplication, optimistic `expected_revision` checks, and a
materialized `world_objects` projection. `FileWorldRepository` and `WorldStore` remain available
for compatibility and migration.

Both the Orchestrator and State Plane API continue to accept a repository factory, so a future
PostgreSQL implementation can replace SQLite without changing Agent, planner, or run semantics.

The authoritative source remains the event stream. Snapshots are derived and must never contain
state that cannot be reconstructed from repository events.

## Provenance and temporal semantics

Every world record carries common metadata:

```text
provenance.actor
provenance.source
provenance.event_id
provenance.source_event_id

observed_at
valid_from
expires_at
supersedes[]
```

The reducer derives authoritative event provenance when a mutation is accepted. Agent-provided
domain metadata may add a human/tool source, but cannot replace the event actor or event identity
written by the Harness.

`valid_from` and `expires_at` describe a validity window; invalid reversed windows are rejected.
`supersedes` records explicit replacement relationships without requiring the storage graph to be
a DAG.

These fields are intentionally generic: credentials, sessions, cloud permissions, service
observations, exploit capabilities, and other domain extensions may all become stale over time.


## Retrieval and context compaction

`WorldContextBuilder` no longer treats the full snapshot as the prompt contract. It uses a
deterministic `WorldRetriever` to rank currently valid records against the active objective.

The first implementation intentionally avoids embeddings. Ranking combines:

- record-kind priority;
- lexical overlap with the current objective;
- hypothesis/observation confidence when available;
- current status;
- recency among timestamped records.

Each record kind has a bounded Top-K limit, and the final context has a character budget. Truncation
happens on complete record boundaries rather than in the middle of serialized state. This keeps the
retrieval policy reproducible while leaving the retrieval interface replaceable by semantic or
learned retrieval later.

World State remains durable memory. `progress.json` is separate solver working state and should not
be treated as ground truth about the target.


## Live ingestion and single-writer semantics

Agent processes never write the canonical `world.db` directly. They submit untrusted
JSONL mutations to `world.inbox.jsonl`.

`WorldInboxCursor` incrementally consumes only newly appended lines, validates each submission, and
applies accepted mutations through the Harness-owned `WorldRepository`. This makes the Harness
process the single authoritative writer for a run while preserving the append-only WorldEvent log.

During session-based runs, accepted live mutations cause:

```text
world.inbox.jsonl
  -> WorldInboxCursor
  -> WorldRepository append
  -> world revision N+1
  -> world.context.txt refresh
  -> world.state.updated trace
  -> trusted AgentSession observation
```

Both external benchmark runs and ordinary Orchestrator runs use this session path when the Agent
adapter exposes `start_session()`. Legacy one-shot adapters retain end-of-run ingestion as a
compatibility fallback.

SQLite is the default transactional backend. Multiple readers and multiple repository instances may
share a run database; writes are serialized by SQLite and can use `expected_revision` for optimistic
conflict detection. Agent processes must still submit through the Harness boundary rather than
opening the canonical database directly.
