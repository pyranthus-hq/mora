# Tasks: Cross-platform Mora review app

**Inputs**: [spec](spec.md), [plan](plan.md), [acceptance](quickstart.md)
**Status**: Ready for review. Implementation tasks are open.

## Phase 1 — Establish the desktop boundary

- [ ] T001 Use the reviewed 0.17.1 baseline from
  [governance T003](../002-governance-baseline/tasks.md), then map queue, preview,
  Keep, Discard, and current authority behavior in `internal/mora/`
  against FR-002 and FR-004. Check the [Mac draft](https://github.com/pyranthus-hq/mora/pull/656)
  for existing review behavior.
- [ ] T002 Prototype a thin shell in `apps/mora-review/` on Mac and Windows.
  Record how it selects the configured backend and launches from Windows
  Start, and demonstrate verifiable human confirmation bound to one action
  and receipt. Record the two host results in `research.md` and prove
  that direct same-account IPC cannot approve without owner verification.
  Resolve the UI host and verification contract before T004.
  Depends on T001. Covers FR-001, FR-003, FR-006.

## Phase 2 — Build the shared review flow

- [ ] T003 [US1] Add typed, bounded saved and held queue previews and fixture
  coverage in `internal/mora/`. Keep incomplete content marked and preserve
  source, scope, and state. Depends on T001. Covers FR-002, FR-005.
- [ ] T004 [US2] Add the revision-bound desktop decision path around existing
  governance in `internal/mora/`. Add `TestDesktopReview` cases for
  missing owner proof, stale body, scope or kind, changed state, incomplete
  preview, cancel, expiry, replay after restart, and competing decisions. Depends on T002, T003.
  Covers FR-003, FR-004, FR-006.
- [ ] T005 [US1] Add the shared queue, full note view, Keep and Discard,
  counts, and actionable states in `apps/mora-review/`. Report index lag,
  disable Keep for incomplete content, and provide read-only review and
  owner-terminal guidance where proof is unavailable. Depends on T003, T004.
  Covers FR-002, FR-004, FR-005.

- [ ] T009 Add backup and sync receipts to the app and Mac Status. Add
  safe retry, divergence guidance, and persistent alert deduplication.
  Reuse one configured backup destination and preserve scoped access during
  migration. Show active consumers before cutover. Depends on T002, T006.
  Covers FR-007–FR-010.

## Phase 3 — Verify desktop acceptance

- [ ] T006 [US1] Add the direct launch on both systems, Windows Start entry,
  and Mac Status entry when available. Prove native Windows UI on the
  supported backend, including the pilot's actual client configuration.
  Depends on T002, T005. Covers FR-001, FR-006 and AC-MAC, AC-WIN.
- [ ] T007 Update review and fallback docs, verify no new agent approval tool,
  and run the core tests and a pure-Go build. Confirm the focused
  `TestDesktopReview` cases actually executed. Run a cross-model review of
  the implementation. Depends on T004–T006 and T009. Covers AC-STALE.
- [ ] T008 Have a blind tester run [quickstart.md](quickstart.md) on Mac and
  Windows. Record which client, backend and build were tested, the correct
  and wrong-scope recall results, and any untested entry points. Depends on
  T007. Covers AC-MAC, AC-WIN, AC-STALE, AC-BACKUP, AC-DESTINATION.
