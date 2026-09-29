# Memory replay pilot — comparison report plumbing

**Issue:** [#545](https://github.com/pyranthus-hq/mora/issues/545) (parent [#540](https://github.com/pyranthus-hq/mora/issues/540))  
**Depends on:** frozen contract [#541](https://github.com/pyranthus-hq/mora/issues/541) / [#549](https://github.com/pyranthus-hq/mora/pull/549); cases [#542](https://github.com/pyranthus-hq/mora/issues/542) / [#550](https://github.com/pyranthus-hq/mora/pull/550); runner [#543](https://github.com/pyranthus-hq/mora/issues/543) / [#551](https://github.com/pyranthus-hq/mora/pull/551); oracle [#544](https://github.com/pyranthus-hq/mora/issues/544) / [#552](https://github.com/pyranthus-hq/mora/pull/552).  
**Go package:** `github.com/pyranthus-hq/mora/internal/mora/pilotreplay/report`

Public **report assembly**, **comparison-plan validation** and **synthetic
fixtures** only. Real comparison of original / no-memory / edited memory is
**Adit-gated** and waits for private eligibility plus explicit spend
authorization.

## Owned paths

| Path | Role |
| --- | --- |
| `internal/mora/pilotreplay/report/` | Plan validation, assembly, damage gate, synthetic fixtures, public method summary |
| `docs/experiments/memory-replay/report/` | Human docs (this tree) |

Do **not** edit `contract/`, `cases/`, `runner/`, or `oracle/` from this track.
Do **not** place private repairs, run artifacts, outcomes or spend receipts in
the public repository.

## What this package proves (offline)

1. Comparison plans reject missing spend / run / time ceilings
2. Matrix resolves against an **explicit** spend ceiling (planning default ≤45 is **not** approved spend)
3. Assembled reports keep failed/missing trials visible
4. Case count is distinguished from repetition count
5. Mismatched checker hashes / scorer versions are rejected
6. Negative results (including damage-gate fails) are preserved
7. Both controls have synthetic fixtures (valid-memory preserve + no-relevant-memory)
8. Public methodological summary has `claims_efficacy=false`

It does **not** execute models, authorize spend, or claim that a memory edit
helped.

## Documents

| File | Contents |
| --- | --- |
| [comparison_plan.md](./comparison_plan.md) | Ceilings, matrix resolution, AuthorizeRealComparison fail-closed |
| [assembly.md](./assembly.md) | Visibility, counts, scorer binding, damage gate |
| [method_summary_template.md](./method_summary_template.md) | Public-safe methodological summary (`claims_efficacy=false`) |

## Private comparison handoff

Real runs wait for:

- [#542](https://github.com/pyranthus-hq/mora/issues/542) private eligibility
- Frozen actual matrix (not only the ≤45 planning default)
- Explicit monetary / run / time ceilings and stopping rule
- Explicit **Adit** spend authorization record
- Isolation readiness for the private package

Until then, `AuthorizeRealComparison` fails closed. Public half =
plumbing + synthetic fixtures + methodological summary only.

## Validation

```text
go test ./internal/mora/pilotreplay/report/ -count=1
```
