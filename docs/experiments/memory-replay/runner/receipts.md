# Attempt, exposure and session receipts

## Attempt receipt

Each trial emits an `AttemptReceipt` (also written under `artifacts/receipts/`):

| Field | Meaning |
| --- | --- |
| `attempt` | Contract `AttemptDocument` (id, condition/rep, status, start/end, timeout, cost, provider_invoked, error/skip) |
| `exposure` | Delivered context vs snapshot digests, tool-result status, injected memory digest |
| `digests` | Case/condition/repo/workspace/harness/model input digests |
| `output_refs` | Stdout/stderr/artifact paths |
| `exit_class` | Contender exit class (`ok`, `task_failure`, `malformed`, `timeout`, `unavailable`, …) |
| `usage` | Tokens/cost/unknowns |
| `isolation` | Reset observed, hash match, contender writes, oracle leak flags, named limits |
| `provider_started` | Whether an external provider process began |
| `claims_efficacy` | **Always false** |

Statuses preserved (no silent retries): succeeded, failed, timed_out, skipped,
unavailable.

## Gate denial

Before any external provider start the runner evaluates the contract run gate
plus monetary/run/time ceilings. On denial:

- attempt status is `skipped` (or unavailable when applicable)
- `provider_started=false`
- stub external providers assert `StartCount == 0`

Synthetic local contenders may run plumbing without paid authorization; they
never open a network connection or spend.

## Session receipt

`BuildSessionReceipt` aggregates attempt ids, fixture hashes and unresolved
limits. Notes state explicitly: infrastructure readiness only — **not** a
historical efficacy claim.
