# Pi Ablation Experiments (TSec + Generic Benchmarks)

Status: experimental design and measurement scaffold. **No experimental success-rate claims yet.**

## Research question

Under the same challenge, model, tools, network, budget and seed, does the Harness improve verified outcomes over a standalone Pi/Kali solver?

| Arm | Agent | Control | World State | Multi-agent |
|---|---|---|---|---|
| 01 native-pi | Pi + Kali | external watchdog only (time, resources, SDK submit/verify) | forbidden | forbidden |
| 02 minimal | same Pi + Kali | budgets, progress detection, bounded recovery, trusted verification | forbidden | forbidden |
| 03 world | same Pi + Kali | 02 + rolling-horizon planning | retrieval, capabilities, hypotheses and provenance | forbidden |
| 04 collaboration | same Pi + Kali | best of 01-03 with targeted delegation/model routing | opt-in as dictated by winning arm | selective, predeclared |

**Important:** `harness tsec` currently runs `BenchmarkRunner.run_case` which calls `bootstrap_solver` and integrates `solver_loop`, world mutations and feedback. Therefore **it is not a valid Arm 01 runner**. Do not label existing `harness tsec` results as native-Pi baseline or Arm 02. Implement explicit runtime arms first and write tests establishing their boundaries.

## Phase 1: native baseline (first implementation milestone)

1. Implement `native-pi` runner using the existing official SDK/TSec adapter only for challenge discovery/provisioning, independently verified flag submission, teardown and safe reporting.
2. Execute one Pi process in the same Kali image and with the same model/tool configuration as the other arms. Hard timeout and resource accounting are allowed; they must not send planning or feedback prompts.
3. Do not create or expose `world.db`, `world.context.txt`, `world.inbox.jsonl`, `plan.json`, `progress.json`, blackboards, planner events or Harness skills to the baseline Agent. Stop if any appear in the agent-visible workspace.
4. Allow the official SDK to verify candidates, but do not inject benchmark feedback to alter Pi's planning while the attempt is in progress. Record outcome only outside the Agent.
5. Collect token usage, model/tool calls, wall clock, score, verified flags, errors and first flag latency (if timestamp is captured). Missing metrics must be null, never zero.
6. Add smoke tests with a fake adapter and scripted Pi session. Confirm no world/bootstrap/planner calls, no model credentials in SDK exposure, correct teardown on failure and verifier-only success.
7. Do not run on live targets in CI. Require explicit opt-in for authorized competition environments.

## Experimental protocol

- Freeze a benchmark case manifest and dataset snapshot. Stratify by Web, Pwn, Reverse, Crypto, Misc and difficulty where supported, but preserve unsupported/unknown categories.
- Paired design: fixed challenge IDs and repeated seeds for every enabled arm. Randomize arm execution order to mitigate time and infrastructure drift. Use **>=3 repeats per case per arm** where feasible.
- Freeze exact model identifier/version, thinking effort, tool list, container image digest, Pi version, SDK version, networking/VPN mode, hardware and prompt. Model stochasticity remains a limitation even with the same seed.
- Equal per-attempt ceilings: 3600 seconds, 200000 tokens, 300 model calls, 1000 tool calls, USD 20 (adjust only by *versioned* manifest).
- Hints **off** by default; report hint use and penalties separately. Never silently resume partially solved cases in paired comparisons; record initial correct count and remove contaminated trials from primary analysis.
- Distinguish total score from score newly awarded in an attempt. Do not equate verified solved flags to submitted candidate count.
- Capture: success, verified flags, awarded score, first flag latency, elapsed seconds, input/output tokens, cost, model calls, tool calls, unproductive calls, replans, recoveries, infrastructure errors. No invented values.
- Primary outcome: verified completion rate paired by case and seed. Secondary: verified flags, awarded score, time-to-first-flag, wall time, cost and tool efficiency.
- Failure taxonomy: unsolved, time limit, model/tool budget, model error, container/VPN infra error, verifier unavailable, target unavailable. Preserve censored/incomplete runs.
- Report bootstrap confidence intervals across **challenge IDs** (not correlated attempts as independent samples), and paired delta from Arm 01. Explain small sample uncertainty.
- Decision gate: promote a more complex arm only if it shows material verified improvement (proposed relative +15% success) or comparable success with clearly lower resources and no safety/reliability regression. This is a project threshold, not a contest rule.
- Arm 04 may only be scheduled after an analysis of Arm 01-03 failure clusters; pre-register which clusters qualify for delegation. Avoid testing a large hyperparameter grid against the same evaluation cases.

## Build sequence

- [x] Separate experiment branch and versioned protocol.
- [ ] EXP-01: actual native Pi execution path + tests. **Block further live comparisons until this lands.**
- [ ] EXP-02: minimal control profile with assertions world/planner are disabled.
- [ ] EXP-03: state/profile and deterministic retrieval-vs-no-retrieval ablations.
- [ ] EXP-04: selective collaboration/model routing triggered by observed failure classes.
- [ ] Run authorized paired trials; publish reproducible anonymized metrics and raw-run audit trail.

## Commands (existing behavior; not native baseline)

```bash
# Existing full benchmark runtime; use only as an exploratory smoke check
harness tsec --agent agents/examples/pi-tsec.yaml --challenge WEB-001 \
  --wall-time 3600 --max-tokens 200000 --max-model-calls 300 \
  --max-tool-calls 1000 --max-cost-usd 20
```

Once EXP-01 is implemented, document its **real** command here. Do not add an alias that silently invokes the full BenchmarkRunner.

See `experiments/pi-ablation/manifest.json` for experiment freeze parameters and `experiments/pi-ablation/summarize.py` for strict result completeness checks.
