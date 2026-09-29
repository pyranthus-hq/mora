# Report assembly

Authoritative helpers: `Assemble`, `EvaluateDamageGate`,
`SyntheticControlBundle` in `internal/mora/pilotreplay/report`.

## Visibility

Failed, timed-out, skipped, unavailable and inconclusive trials **remain
visible**. Assembly rejects any such row with `visible=false`
(`missing_trial_visibility`).

Negative results (task fail, constraint drop, damage-gate fail) are likewise
preserved and counted in `CountSummary.negative_trials`.

## Case count vs repetition count

`CountSummary` reports:

| Field | Meaning |
| --- | --- |
| `case_count` / `distinct_cases` | Unique `case_id` values |
| `repetition_count` | Number of trial rows (rep slots) |
| `trial_count` | Same as rows assembled |

Repeated runs of **one** incident are **not** independent incidents. Report
construction must not collapse reps into inflated case counts.

## Scorer / hash binding

Every trial must match the report `ScorerBinding`:

- `checker_id`, `checker_version`
- `checker_digest`, `policy_digest`

Mismatch → `scorer_mismatch` or `hash_mismatch`. Changing the checker after
seeing results invalidates that labeled comparison (oracle freeze rule).

## Damage gate

A repair (or valid-memory control) that **drops** a required valid-memory
constraint fails the damage gate — even if the task markers appear to pass.

| Situation | Result |
| --- | --- |
| `reviewed_memory_edit` + constraint not preserved | fail |
| `valid_memory_control` + constraint not preserved | fail |
| Constraint unobserved when required | fail closed |
| Constraint preserved | pass |

`ApplyDamageGate` stamps `damage_gate_failed=true`, forces `visible=true`, and
downgrades a bare `pass` outcome to `fail`.

## Synthetic control fixtures

| Fixture | Role |
| --- | --- |
| `FixtureValidMemoryPreserved` | Valid-memory control, constraint intact |
| `FixtureValidMemoryDropped` | Valid-memory control NEGATIVE → damage gate |
| `FixtureNoRelevantMemoryOK` | No-relevant-memory control succeeds without edit |
| `FixtureNoRelevantMemoryNegative` | NRM NEGATIVE |
| `FixtureFailureMissing` | Unavailable trial (must stay visible) |
| `FixtureEditDamageDrop` | Edited failure case wins by dropping constraints |

No private gold, repairs or spend receipts appear in these fixtures.

## Contract projection

`ToContractReport` emits a `contract.ReportDocument` id envelope with
`claims_efficacy=false`, the plan matrix, and the spend ceiling mirrored into
`cost.ceiling_usd_micros`. Trial bodies stay on `AssembledReport`.
