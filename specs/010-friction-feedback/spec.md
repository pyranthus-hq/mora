# Feature Specification: Governed friction feedback pilot

**Feature**: `010-friction-feedback`

**Status**: Companion pilot outside the 0.18 release gates.

**Acceptance**: DR1, with E1 providing the isolation fixture pattern.

## User Scenarios and Testing

### User Story 1 — Recall a reviewed friction report (Priority: P1)

As an agent starting relevant work, I can find one reviewed, scoped
friction report and understand the tested recovery before repeating
the same mistake.

## Functional Requirements

- **FR-001**. Record meaningful rework in the existing tracker with project, symptom,
  expected result, recovery when available, and an evidence reference.
  A reviewed summary can enter Mora once through the governed write path
  as agent evidence. Keep raw traces on their originating hosts.
- **FR-002**. Deduplicate by source feedback ID within its scope.
  Distinguish pending approval, persisted note, and successful scoped
  retrieval. Separate service recovery from defect resolution.
- **FR-003**. Expand import only after authorized devbox recall and
  refreshing-index isolation pass. Keep warning and recurrence
  experiments in the existing experiment harness. Automated write-back and adoption are
  outside the pilot.

## Success Criteria

- **AC-FRICTION (DR1)**. Recheck the existing incident or a sanitized
  equivalent with the same duplicate-process failure pattern. The same
  import key resolves to one record, and a fresh session retrieves it
  without the ID through an eligible share. The other scope returns no
  matching result or evidence entry. Run with a current index and during
  refresh, and verify the account of recovery does not claim a code fix.
