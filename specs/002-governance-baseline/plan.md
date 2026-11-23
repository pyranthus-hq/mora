# Implementation Plan: 0.17.1 governance baseline

**Input**: [Spec](spec.md) · [Release order](../mora-0.18.md)

## Delivery order

1. Start at [PR #666](https://github.com/pyranthus-hq/mora/pull/666).
   Read its latest head, required check results, and existing regression
   evidence. Carry unresolved gaps into the baseline review before opening
   additional work.
2. Trace CLI and MCP authority through `internal/mora/authored_write.go`,
   `internal/mora/mcp_policy.go`, and the review path at the candidate
   revision. Use the existing authority harness and a disposable vault to
   compare authored, held, and refused results. Where a supported path
   misses policy, add a focused failing test and reuse the existing gate.
3. Confirm the Mac and Windows upgrade smoke against the landed SHA. Record
   compatibility, test coverage, and follow-up blockers in the PR evidence.
   The release owner controls merge and tagging.

No new approval surface is part of this baseline. The review app, owner
rules, and typed capture each add their own acceptance coverage.
