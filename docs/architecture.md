# Modular monolith boundaries

Harness is one deployable Python application, not a collection of microservices.
Modules are separated by responsibilities and explicit composition points.

- `runtime/`: application-level composition, solver initialization, run state.
- `world/`: domain state, event storage, snapshots, retrieval. No benchmark or HTTP dependencies.
- `benchmark/`: externally provisioned challenge lifecycle, submissions and adapters.
- `api/`: transport-facing HTTP handlers, not a place for solver policies.
- `agent.py`, `session.py`, `pi_*.py`: agent/session integration and infrastructure.
- `solver_loop.py`, `planner.py`, `action_verifier.py`: solver policies.
- `orchestrator.py`: local-run lifecycle and environment integration.

The first extraction is `runtime.bootstrap_solver`. Both local runs and
externally provisioned benchmarks create the same solver state through this
composition point. Runtime does not provision environments, contact the
benchmark SDK, or run verifiers.

Dependency direction (initial migration):

```text
CLI / HTTP / worker
       | 
local orchestrator       benchmark runner
          \                /
           runtime bootstrap
              |   |   |
         solver world progress
```

New modules must not import the CLI, HTTP API or benchmark-specific adapters
from the domain model. Avoid exposing database connection objects across
module boundaries. Preserve public import paths until migration is complete.

This is an incremental refactor: the older top-level modules remain valid,
and this document does not claim the full package has been relocated.
