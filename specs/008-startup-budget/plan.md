# Implementation Plan: Bounded startup and diagnostics

**Prerequisite**: [Baseline](../002-governance-baseline/tasks.md) T003.
Budget fixtures can start on that baseline. Final release acceptance
requires the [scoped setup](../006-project-setup/tasks.md) T003 path and
the actual in-scope tools. This package owns the catalog record and
budget regression. The release owner resolves proposed catalog changes.
Rerun budgets and dispatch checks after a review or skill catalog change.

## Delivery order

1. Use the existing contract and budget test seams in `internal/mora/`,
   including `mcp_catalog_test.go` and `mora_mcp_budget_test.go`. Capture
   real outputs at their entry points. Count characters for estimated
   tokens and count UTF-8 bytes for the recall cap.
2. Apply budgets at the profile and startup boundaries in `mcp.go` and
   `hook.go`. Preserve authority, evidence scope, and ordinary explicit
   retrieval. Keep any truncation observable.
3. Reuse status and doctor output with the same measurement definitions.
   Record ceilings and observed values without deriving a savings claim.
