# Tasks: Cross-machine session handoffs

## Phase 1 — Establish prerequisites

- [ ] T001 With baseline T003 recorded, verify the scope and transport
  already authorized for each direction and select distinct disposable
  writable vaults. Stop on any missing authorization. Record normal handoff and
  share commands for each host and the eligible/held/rejected fixtures.
  Covers AC-HANDOFF.

## Phase 2 — Run the two-way pilot

- [ ] T002 [US1] Run D1 both directions with actual Mac and Windows clients.
  Record file, explicit search, startup, scope, and retry evidence. Where
  an authorized path fails, add a focused fixture and fix the scoped path
  in `internal/mora/` before retest. Preserve the share permissions. Depends on T001. Covers AC-HANDOFF.

## Phase 3 — Record verification

- [ ] T003 Cross-review any change and obtain an independent D1 run.
  Capture any unsupported surface and stop at the pilot decision.
  Depends on T002. Covers AC-HANDOFF.

## Conditional wider sync

Reopen this package after D1 and an owner decision to expand. Plan the
private transport, exclusions, conflict copies, cadence, and per-machine
approvals before adding executable AC-SYNC implementation tasks.
