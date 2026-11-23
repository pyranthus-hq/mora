# Feature Specification: Bounded startup and diagnostics

**Feature**: `008-startup-budget`

**Status**: Proposed release feature.

**Acceptance**: E, E2. E1 is exercised in scoped setup.

## User Scenarios and Testing

### User Story 1 — Keep startup context within its budget (Priority: P1)

As a Mora user, I know what Mora adds to a fresh session. An unbound
session stays quiet by default, and I can explicitly retrieve relevant
notes omitted from bounded recall.

## Functional Requirements

- **FR-001**. The proposed seven-tool project profile caps MCP initialize
  at 500 estimated tokens and tools/list at 1,500, with at most 2,000
  combined before rules. Resolve catalog changes explicitly without
  loosening these ceilings or expanding mutation authority.
- **FR-002**. Bound SessionStart includes a project marker in at most
  2,000 characters. Unbound SessionStart emits zero Mora text by default.
  Prompt recall carries at most three notes and 800 UTF-8 bytes. Omitted
  in-scope notes remain explicitly findable.
- **FR-003**. Status breaks down measured context cost. Doctor identifies
  missing setup, registration, stale managed files, or applicable sync
  health only where those capabilities are in scope.

## Success Criteria

- **AC-MCP (E2)**. Capture the actual serialized initialize instructions
  and tools/list payload for a project profile. Measure each and their
  sum at four characters per estimated token. Verify the profile's
  tools/list, dispatch, and write policy agree.
- **AC-HOOK (E, E2)**. Check the actual hook output on Mac and Windows with
  A bound, an unbound folder, and an over-cap recall fixture. Measure the
  note count, UTF-8 bytes, and character cap separately. An omitted in-scope
  note is still findable by explicit search.
- **AC-STATUS (E2)**. On the same fixture, status reports the measured
  components consistently, and doctor distinguishes missing or stale
  setup from a healthy installation.
