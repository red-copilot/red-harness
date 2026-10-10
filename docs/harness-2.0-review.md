# Harness 2.0: contracts and verification review

This is implementation stage 1 of the requested minimal Harness 2.0. The staged
contracts live in `harness.v2`; existing CLI commands, imports, event formats,
dependencies and historical run data keep their current behavior. This is not
the completed 2.0 runner or a release of version 2.0.

Roadmap item: **VER-V2-01**, [draft PR #12](https://github.com/red-copilot/red-harness/pull/12).
Base: remote `main` at `62485bc`. Development branch:
`codex/harness-v2-contracts`, in a separate worktree. Uncommitted changes in the
original worktree are not incorporated into this proposal.

## Implemented contract

- `RunSpec` requires `apiVersion: harness/v2`, a task, Docker Pi configuration,
  a finite positive wall-time budget and a local or TSec evaluation configuration.
  Pi is fixed to 1.0.4. Removed checkpoint/resume/gateway/worker settings are
  rejected. `examples/minimal-run-v2.yaml` is a tested contract example, not an
  executable configuration for the existing CLI.
- Credentials are environment-variable selectors, never literal keys in this
  schema. A Pi credential selector cannot overlap the configured TSec platform
  selectors. Enforcement of process environment isolation is a later stage.
- `PiSession` exposes start, events and close; close returns the observed process
  exit code, or unknown when exit is unconfirmed. `CaseAdapter` exposes prepare,
  evaluate and release. Their protocols contain no SDK, Docker or old Runtime
  types, and no checkpoint or resume interface.
- `AgentEvent` records observations, tool exits, process exits and usage independently of the
  objective verdict. Missing usage stays `null`; it does not become zero.
- `RunResult` separates stop reason, objective verdict and cleanup. Abnormal
  termination has an unknown objective verdict. Cleanup remains unknown until
  confirmed, and cleanup failure does not overwrite an established objective.
- Immutable `EvidenceRef` records run/task scope, a host producer, capture time,
  a relative artifact reference, its SHA256 and the evaluated input-manifest
  digest. Immutable verdicts reject absent, duplicate, conflicting or wrong-scope
  evidence. Native scores are retained; platform cumulative score is separate.

## Verification authority

`VerificationBoundary` receives evaluator objects from trusted host composition.
It invokes those objects itself; it does not accept an Agent event or an
externally supplied verdict as proof. A producer string is only a registry
selection made by host code, not an authentication mechanism.

Evaluator replies contain only status and optional numeric scores. Unknown fields
such as producer/source/evidence, forged verdict objects, non-finite scores and
malformed replies are rejected. Exceptions yield safe error codes without
copying exception text or credentials into evidence. Cancellation propagates.

Before and after evaluation, the boundary checks input-manifest hashes and
rejects missing, changed or symlinked artifacts. The future Runner must create a
host-owned immutable snapshot; these checks do not replace isolation or prevent
concurrent mutation of an Agent-writable directory.

The host evidence-store protocol must commit a canonical response record before
returning a reference. The boundary reads it back and checks its exact bytes,
hash, run, task, producer and input digest before creating a conclusive verdict.
The current tests use a file-store substitute. A production SQLite store,
crash durability and terminal-state atomicity are not implemented in this stage.

## Review decisions

The next stages require maintainer review because the repository contract says:

> Halt and request maintainer review if the task changes public contracts,
> destructive data migrations, credentials, network permissions, verification
> trust boundaries, or evaluation semantics.

See [AGENTS.md](../AGENTS.md). Review the following before proceeding:

1. Accept the proposed v2 schema and the separate stop reason/verdict/cleanup
   meanings, including unknown rather than failed for unavailable evaluation.
2. Accept host-composed evaluator authority and evidence-store ownership.
3. Accept final-only evaluation after normal Pi completion, with no automatic
   resume or replay of uncertain operations. The Runner must enforce this policy;
   the current stage only defines its contracts.
4. Approve adapting the earlier VER-01 World-revision/action-verdict scope to
   input-manifest-bound objective verification for the minimal architecture.
   Existing VER-01/02 completion evidence and active contracts are preserved;
   this proposal does not replace them before reviewed cutover.

After review: implement Runner/SQLite state; Docker Pi and snapshot ownership;
local and optional TSec adapters; then versioned CLI cutover and dependency/module
removal. The future CLI will offer run, report and acknowledge. Acknowledgment
will record manual reconciliation without awarding success or resuming a Run.

## Validation evidence

The initial local `main` baseline (`8c91184`) passed 130 tests. New tests were
written first and failed collection with `ModuleNotFoundError: harness.v2` before
implementation. Initial stage validation passed 191 tests on that baseline. The
branch was then rebased onto remote `main` (`62485bc`), retaining all upstream
verification and architecture checks. Current validation is recorded in the PR.

Run commands from the isolated worktree, with its `src` first on the import path
and the same Python environment used by example subprocesses:

```bash
export PYTHONPATH="$PWD/src"
export PATH="/tmp/red-harness-venv311/bin:$PATH"
python -m pytest -q tests/test_v2_contracts.py tests/test_v2_verification.py tests/test_architecture.py
python -m ruff check .
python -m pytest -q
git diff --check
```

The new contract/boundary tests add 64 cases, plus one architecture gate. Default
CLI import was checked and does not load `harness.v2`.

After rebasing onto `62485bc`, targeted contract/boundary/architecture tests
passed **69 tests**. The full suite passed **570 tests**, with **4 Docker
integration tests skipped** because `harness-isolation-probe:ci` and
`harness-pi-rpc-resume-probe:ci` were not built, and seven existing dependency
deprecation warnings. Ruff and `git diff --check` passed. The existing 1,000-event
SQLite timing test passed in this run; no performance improvement is inferred.

The available environment uses Python
3.11.0rc1, pytest 9.1.1, Pydantic 2.14.0, Ruff 0.16.10, PyYAML 6.0.3 and Typer
0.27.3. Pytest is outside the declared `<9` development constraint and Python is
a prerelease. Stable supported Python and declared development dependencies must
still be exercised in CI before merge.

Docker/Pi process smoke tests, real TSec SDK integration, network-policy
enforcement, crash recovery and performance improvement are not claimed. This
stage adds three source modules, with no default CLI integration or dependency
changes; simplification measurements belong to the later cutover stage.
