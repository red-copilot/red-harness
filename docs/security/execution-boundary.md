# Agent execution boundary

## Assets and trust levels

The run root is Harness-owned state. It contains the authoritative World store and event
history, progress ledger, trace, result, checkpoint, objective evidence, and verifier logs.
Agent output, tool output, submissions, and files in `agent-workspace/` are untrusted.
An Agent claim or successful process exit is never objective evidence by itself.

## Filesystem boundary

Container Agents receive a read-only task input snapshot at `/task` and only the run's
`agent-workspace/` at `/run/harness` read-write. The snapshot omits the configured task verifier,
skips symbolic links, rejects a configured verifier path that contains a symbolic link (including
parent components), is bounded to 4,096 files and 2 GiB, and is checked against its source on
resume. Preflight and snapshot creation both enforce the verifier path check before allocating the
Agent task view. They do not receive a mount of the run root,
World database, result, checkpoint, trace, evidence store, or verifier logs. The workspace
contains writable artifacts and untrusted event/submission input, plus refreshed copies of
`world.context.txt` and `progress.json`; those copies are for Agent guidance and are not read
back as authoritative state. The verifier runs from the original Harness-side task directory;
its source is excluded from the Agent's task input snapshot, and it sees the workspace read-only.

Harness reads Agent-produced files as hostile input: JSONL inboxes and telemetry require
regular files opened without following a final symlink. Gateway writes use the same rule.
The task mount and container root filesystem are read-only; only `/tmp` and the Agent
workspace are writable. No Docker socket is mounted.

## Process and network boundary

Docker Agents run as the workspace's mapped numeric identity (UID/GID 65532 when Harness
runs as root), use a read-only root filesystem, drop all capabilities by default, set
`no-new-privileges`, and have explicit PID, memory, CPU, and temporary-filesystem limits.
Pi capability additions are explicit configuration. Container networking is selected from
the Agent's declared network profile and network mode. `fully-offline` (and legacy `offline`)
uses Docker `none`. `target-only` requires the target Compose network to report Docker
`Internal=true` and attaches the Agent only to that network. `model-allowed` requires the
sidecar Gateway network and any configured target network to report `Internal=true`; the Agent
uses the Docker network IDs returned by those checks while the Gateway proxies the configured
model endpoint. Using the checked IDs for container creation and target-network attachment
prevents a network name from being rebound to a different network between validation and use.
These strict profiles are checked before Agent startup. Strict-profile Agent containers explicitly
clear proxy variables, including values Docker CLI can inject from its client configuration, and reject
Agent-configured proxy passthrough. This prevents proxy credentials from entering the Agent
environment and prevents direct proxy egress from bypassing the Gateway. Legacy
`benchmark-only` remains advisory and does not provide egress isolation. Host CLI Agents cannot
use strict profiles.
Host networking is a deliberate configuration with broader reach and is incompatible with
sidecar Gateway mode.

Python task verifiers run under bubblewrap with a private network namespace, no Linux
capabilities, read-only task/workspace views, a minimal environment, and resource limits.
Unavailable isolation fails closed. Host CLI Agents require explicit opt-in and are not a
container security boundary.

## Secrets and artifacts

Benchmark credentials remain in Harness adapters and are not copied into Agent containers.
Provider credentials are passed only when the Agent configuration explicitly requests
environment passthrough; container Agent configs cannot passthrough token-named variables or
reserved Harness, benchmark, and TSec credentials. Gateway mode injects a per-run Gateway token
instead of the direct provider key. Agent output remains untrusted even when stored as a run
artifact. Objective verdicts are produced by registered verifiers and reference Harness-persisted
evidence.
Host CLI Agents are an explicit development escape hatch: they are not isolated and inherit
the host process environment. Run only trusted commands in that mode and keep sensitive
benchmark credentials out of their environment.

This boundary limits filesystem access; it does not claim exactly-once side effects, safe
host networking, or trust in Agent-generated files. Interrupted external actions require
reconciliation before recovery.
