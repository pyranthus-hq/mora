# Memory replay pilot — case qualification (public half)

**Issue:** [#542](https://github.com/pyranthus-hq/mora/issues/542) (parent [#540](https://github.com/pyranthus-hq/mora/issues/540); contract [#541](https://github.com/pyranthus-hq/mora/issues/541) / PR [#549](https://github.com/pyranthus-hq/mora/pull/549))  
**Go package:** `github.com/pyranthus-hq/mora/internal/mora/pilotreplay/cases`

This directory is the **public** qualification protocol for selecting one failed
coding-agent task plus two controls. It does **not** publish a real case.

## Owned paths

| Path | Role |
| --- | --- |
| `internal/mora/pilotreplay/cases/` | Synthetic eligibility manifests, rejection fixtures, offline validation, tests |
| `docs/experiments/memory-replay/cases/` | Qualification protocol + redacted summary template (this tree) |

**Do not edit** `internal/mora/pilotreplay/contract/`, or any runner / oracle /
report directories, from this track.

## What closing the public PR means

- Protocol + synthetic fixtures + validation tests are in place.
- **No** real-incident qualification.
- **Go/no-go** for the private packet is left to Adit / an authorized owner.
- Private inventory (≈5–10 candidates) + permission review is a **handoff**,
  not invented here to meet a quota.

## Documents

| File | Contents |
| --- | --- |
| [protocol.md](./protocol.md) | How private inventory works; go/no-go outcomes; what never goes public |
| [eligibility_summary_template.md](./eligibility_summary_template.md) | Redacted public eligibility summary template |

## Offline validation

```text
go test ./internal/mora/pilotreplay/cases/ -count=1
```

`ValidateEligibilityManifest` checks required bytes/hashes, temporal cutoff,
repository reconstruction, and **memory snapshot ≠ delivered context**, and
rejects missing delivered context, missing dirty files (under
`relevant_dirty_only`), unknown permissions, and post-cutoff repair evidence.

## Non-claims

- Does **not** assert that memory caused a failure.
- Does **not** authorize model execution or spend.
- Does **not** place private traces, repository identities, or source paths in
  the public repository. The private package stays outside the public repo.
