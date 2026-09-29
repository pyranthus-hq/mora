# Redacted eligibility summary template

Use this template (or `cases.NewPublicProtocolSummary` /
`RedactedEligibilitySummary.RenderMarkdown`) for **public** artifacts only.
Fill private fields only in the private package.

```markdown
# Redacted eligibility summary (`summary-elig-…`)

- **Issue:** #542
- **Generated at:** <RFC3339 UTC>
- **Decision:** `public_protocol_only` | `eligible` | `no_go` | `prospective_capture`
- **Roles covered:** failure, valid_memory_control, no_relevant_memory_control
- **Controls noted:** true | false
- **Private package:** `outside_public_repo`
- **Permission status:** `not_applicable_public_half` | `recorded` | `unknown`
- **Synthetic only:** true | false
- **Claims memory caused failure:** false   <!-- must remain false -->

## Missing inputs

- private_candidate_inventory
- authorized_permission_record
- private_frozen_case_package
- <!-- add concrete missing bytes/fields without private paths -->

## Exclusions

- No real incident IDs, repo names, or source paths are published.
- Unfavorable or incomplete private candidates are preserved only in the private selection record.

## Limitations

- Public fixtures are synthetic and cannot support historical attribution.
- Closure of the public PR is protocol + fixtures only (when decision is public_protocol_only).
- Go/no-go for the private packet is left to Adit / an authorized owner.
- No model execution, spend, or outreach is authorized by this summary.

## Public artifact note

Synthetic manifests + qualification protocol only. See
`docs/experiments/memory-replay/cases/`.

## Handoff

Private inventory of 5–10 candidates + permission review needs Adit /
authorized owner. Do not invent cases to meet quota.
```

### Field rules

| Field | Rule |
| --- | --- |
| `decision` | Use `public_protocol_only` until a private go/no-go exists |
| `private_package_status` | Always `outside_public_repo` in public docs |
| `claims_memory_caused_failure` | Always `false` |
| Paths / IDs | Synthetic relative paths and synthetic case ids only |
| Permission | Status enum only; no pasted private approval text |

### Example (public half)

The Go helper `NewPublicProtocolSummary` emits a filled instance matching the
interim `public_protocol_only` decision for [#542](https://github.com/pyranthus-hq/mora/issues/542).
