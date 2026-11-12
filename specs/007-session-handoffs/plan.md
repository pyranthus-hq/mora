# Implementation Plan: Cross-machine session handoffs

**Prerequisite**: [Baseline](../002-governance-baseline/tasks.md) T003.
Use existing scope bindings and an explicitly authorized share for each
direction. Record the configured transport class, allowed scope and
direction. Stop if the existing authorization is missing in either direction.
Creating a share or widening its eligibility requires separate authorization.

## Delivery order

1. Trace handoff persistence, export eligibility, and receiving index
   refresh using the current CLI and `internal/mora/share*.go`. Exercise
   retry and deduplication first with a disposable local share fixture.
   Use the recorded transport for the actual host run. A local fixture
   does not establish cross-machine recall.
2. Exercise both directions, an offline retry, and duplicate publication.
   Compare the persisted record, scoped search, and fresh startup context
   separately. Keep local SessionEnd fallback and source traces local.
3. If wider sync is approved after D1, use the existing Git backup
   boundary in `internal/mora/gitsync.go` as a reference. Confirm
   conflict and scheduler behavior, per-machine approval, and byte-stable
   file IDs on Windows before planning the broader implementation.
