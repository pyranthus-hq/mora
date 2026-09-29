# Runner isolation and disposable layout

## Layout (per session)

Under an opt-in parent directory (never a production vault/home):

| Dir | Purpose |
| --- | --- |
| `repo/` | Disposable repository copy |
| `config/` | Trial config |
| `vault/` | Disposable memory vault/index inputs |
| `index/` | Disposable index |
| `harness-home/` | Disposable harness home |
| `contender/` | Contender-visible workspace only |
| `oracle-ref/` | Hash-ref staging for oracle package (not mounted into contender) |
| `artifacts/` | Attempt outputs and receipts |
| `private-admit/` | Staged caller-selected private package (outside public fixtures/logs) |

A preserved snapshot of the clean layout is kept beside the live root. Each
trial **resets** from that snapshot and verifies clean initial hashes before
injection.

## Default deny

- production writes
- external actions
- live vault / connector stores / real write credentials
- hidden grading material inside the contender workspace
- gold / expected answers / reference patches / post-cutoff evidence for repair

## Private package admission

Caller-selected private packages are validated before process start and staged
only under `private-admit/`. They are **not** copied into public fixtures or
default logs. Admitting from `docs/experiments/memory-replay/` or
`internal/mora/pilotreplay/cases|contract` trees is rejected.

## Named blockers for real private cases

Recorded on session receipts (non-exhaustive):

1. OS-level network namespace isolation is not verified by this runner.
2. Paid/external model calls remain disabled in this package; #545 required.
3. Real historical attribution requires private eligibility + oracle readiness.
4. Unverified isolation guarantees void historical attribution for that attempt.

## Condition injection

For each condition the runner stages vault bytes and records an
`ExposureRecord` whose delivered-parts digest is **separate** from the memory
snapshot digest. Tool-result reads are marked `captured` or explicitly
`unavailable` (request logs alone are not proof of model exposure).
