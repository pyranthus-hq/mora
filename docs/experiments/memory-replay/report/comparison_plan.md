# Comparison-plan validation

Authoritative helpers: `ComparisonPlan.Validate`,
`ResolveAgainstCeilings`, `AuthorizeRealComparison` in
`internal/mora/pilotreplay/report`.

## Explicit ceilings (all required)

| Ceiling | Field | Rule |
| --- | --- | --- |
| Spend | `spend_ceiling_usd_micros` | Must be **> 0**. Missing/zero → `missing_ceiling`. Do **not** infer from the planning default. |
| Run | `run_ceiling` | Must be **> 0**. Matrix `max_planned_runs` must not exceed it. |
| Time | `time_ceiling_seconds` | Must be **> 0**. |

A missing spend ceiling **blocks** paid calls. Substituting an inferred budget
is forbidden.

## Planning default ≤45 is not approved spend

The contract planning matrix defaults to:

| Dim | Default |
| --- | --- |
| Reps per cell | 5 |
| Failure cases | 1 |
| Control cases | 2 |
| Conditions | 3 |
| Max planned runs | **45** |

`matrix.is_approved_spend` and `matrix.is_statistical_claim` remain **false**.
`ComparisonPlan.is_approved_spend` also remains false on the public path unless
an external `spend_authorization_ref` is present — and even then,
`AuthorizeRealComparison` still requires private eligibility.

## Resolve against spend ceiling

`ResolveAgainstCeilings`:

1. Validates the plan (ceilings + matrix)
2. Sets `resolved_max_runs = matrix.max_planned_runs` (must be ≤ run ceiling)
3. Sets `resolved_estimated_spend_usd_micros = resolved_max_runs × estimated_cost_per_run_usd_micros`
4. Rejects if estimated spend exceeds the explicit spend ceiling

Fewer runs than 45 are valid when disclosed against the frozen ceilings.

## AuthorizeRealComparison (fail closed)

Requires **all** of:

1. Plan validates and resolves
2. `private_eligibility_ready=true` (#542 private go/no-go)
3. Non-empty `spend_authorization_ref` (Adit)
4. `is_approved_spend=true` only with that ref
5. Non-empty `stopping_rule`

Public synthetic fixtures leave eligibility and spend auth empty, so
authorization **always fails** in the public suite. That is intentional.

## Missing-run policy

`missing_run_policy` ∈ {`record_unavailable`, `exclude_with_note`, `fail_closed`}.
Public fixtures use `record_unavailable` so missing trials stay visible in
assembly.
