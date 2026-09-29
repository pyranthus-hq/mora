# Memory replay pilot — isolated opt-in runner

**Issue:** [#543](https://github.com/pyranthus-hq/mora/issues/543) (parent [#540](https://github.com/pyranthus-hq/mora/issues/540))  
**Depends on:** frozen contract [#541](https://github.com/pyranthus-hq/mora/issues/541) / [#549](https://github.com/pyranthus-hq/mora/pull/549); synthetic cases from [#542](https://github.com/pyranthus-hq/mora/issues/542) / [#550](https://github.com/pyranthus-hq/mora/pull/550)  
**Go package:** `github.com/pyranthus-hq/mora/internal/mora/pilotreplay/runner`

Local, opt-in, resettable runner for original-memory / no-memory / edited-memory
coding trials. It preserves what was delivered to the agent and every attempt's
status without reading or mutating production state.

## Owned paths

| Path | Role |
| --- | --- |
| `internal/mora/pilotreplay/runner/` | Disposable workspace, admission, exposure, gate, synthetic contender, attempt receipts, tests |
| `docs/experiments/memory-replay/runner/` | Human docs (this tree) |

Do **not** edit `contract/`, `cases/`, oracle, or report directories from this
track. Request contract amendments instead.

## What a runner receipt means

A runner receipt establishes **infrastructure readiness** only:

- resets with no bleed between conditions
- attempt / exposure receipts
- failure retention (no silent retries)
- denial without budgets (no provider start)
- containment of observable writes

It is **not** a historical efficacy claim and does **not** authorize paid runs.
Real private-case execution stays blocked until [#545](https://github.com/pyranthus-hq/mora/issues/545).

## Synthetic contender

Ordinary tests use a deterministic local fake contender:

1. known task failure
2. valid-memory constraint control
3. no-relevant-memory task

The runner preserves distinct inputs and outputs; it does **not** score
efficacy. Paid / external model calls are impossible in ordinary product
commands and ordinary test runs.

## Documents

| File | Contents |
| --- | --- |
| [isolation.md](./isolation.md) | Disposable layout, reset, containment, named blockers |
| [receipts.md](./receipts.md) | Attempt / exposure / session receipt fields |

## Validation

```text
go test ./internal/mora/pilotreplay/runner/ -count=1
```
