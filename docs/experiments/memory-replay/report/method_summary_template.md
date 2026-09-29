# Public methodological summary template

Use this template (or `report.NewPublicMethodSummary` /
`PublicMethodSummary.RenderMarkdown`) for **public** artifacts only.
Private findings, repairs, artifacts and spend receipts stay outside the
public repository.

```markdown
# Public methodological summary (`summary-method-…`)

- **Issue:** #545 (parent #540)
- **Depends on:** #541, #542, #543, #544
- **Generated at:** <RFC3339 UTC>
- **Conditions compared:** original_memory, no_memory, reviewed_memory_edit
- **Controls required:** valid_memory_control, no_relevant_memory_control
- **Planning default max runs:** 45 (approved spend: false)
- **Private package:** `outside_public_repo`
- **Real runs executed:** false
- **Spend authorized:** false
- **Private eligibility:** `pending` | `ready` | `no_go`
- **Synthetic only:** true
- **Claims efficacy:** false   <!-- must remain false -->

## Limitations

- Public PR = report plumbing + synthetic fixtures + methodological summary only.
- Planning default ≤45 runs is a schedule upper bound, not approved spend.
- Real comparison waits for #542 private eligibility and explicit Adit spend authorization.
- Failed/missing trials remain visible; case count ≠ repetition count.
- A repair that drops valid constraints fails the damage gate.
- No private repairs, artifacts or spend receipts appear in the public repo.

## Public artifact note

Synthetic report fixtures + comparison-plan validation only. See
`docs/experiments/memory-replay/report/`.

## Handoff (Adit)

Private comparison (original / no-memory / edited) needs Adit: private package
eligibility (#542), frozen matrix, explicit monetary/run/time ceilings, spend
authorization, and isolation readiness. Do not substitute the planning default
for approved spend.
```

### Field rules

| Field | Rule |
| --- | --- |
| `claims_efficacy` | Always `false` |
| `planning_is_approved_spend` | Always `false` on the public half |
| `real_runs_executed` | Always `false` on the public half |
| `spend_authorized` | Always `false` on the public half |
| `private_package_status` | Always `outside_public_repo` |
| Paths / IDs | Synthetic relative ids only |

### Example (public half)

`NewPublicMethodSummary` emits a filled instance for
[#545](https://github.com/pyranthus-hq/mora/issues/545) with
`private_eligibility=pending` until Adit completes the private packet.
