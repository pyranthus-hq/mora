# Memory replay pilot — hidden outcome checker

**Issue:** [#544](https://github.com/pyranthus-hq/mora/issues/544) (parent [#540](https://github.com/pyranthus-hq/mora/issues/540))  
**Depends on:** frozen contract [#541](https://github.com/pyranthus-hq/mora/issues/541) / [#549](https://github.com/pyranthus-hq/mora/pull/549); eligible synthetic shapes from [#542](https://github.com/pyranthus-hq/mora/issues/542) / [#550](https://github.com/pyranthus-hq/mora/pull/550). Runner [#543](https://github.com/pyranthus-hq/mora/issues/543) is not a construction dependency (fixtures exercise the checker offline).  
**Go package:** `github.com/pyranthus-hq/mora/internal/mora/pilotreplay/oracle`

Frozen outcome checker that detects a selected coding failure and collateral
loss of valid constraints **without** exposing expected outcomes to the
contender or repair authoring process.

## Owned paths

| Path | Role |
| --- | --- |
| `internal/mora/pilotreplay/oracle/` | Checker interfaces, frozen scoring policy, synthetic fixtures, access-boundary tests |
| `docs/experiments/memory-replay/oracle/` | Human docs (this tree) |

Do **not** edit `contract/`, `cases/`, `runner/`, or report directories from
this track. Public files are interfaces + synthetic fixtures only.

## Private gold handoff

Case-specific private hidden tests, reference patches and gold labels stay in
an **authorized evaluator-only location outside the public repository**.
Until [#542](https://github.com/pyranthus-hq/mora/issues/542) private
eligibility completes, that inventory remains an **Adit / evaluator handoff**.
This package must not invent private gold in-repo.

## What a grade means

A grade establishes **checker sensitivity** on synthetic fixtures:

1. fails a known fault
2. passes independently justified expected behavior
3. detects valid-constraint violation
4. handles the no-relevant-memory control
5. cannot leak gold through contender / repair / runner-hash-ref interfaces

It is **not** a repair-benefit claim and does **not** authorize paid runs.
[#545](https://github.com/pyranthus-hq/mora/issues/545) is the first real
comparison gate.

## Documents

| File | Contents |
| --- | --- |
| [scoring.md](./scoring.md) | Frozen scoring policy, dimensions, missing-output / grader-error rules, checker freeze |
| [isolation.md](./isolation.md) | Evaluator env, access boundaries, allowed feedback |

## Validation

```text
go test ./internal/mora/pilotreplay/oracle/ -count=1
```
