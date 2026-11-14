# Feature Specification: 0.18 release validation

**Feature**: `009-release-validation`

**Status**: Release gate. Gather evidence after features reach acceptance.

**Acceptance**: G and H, plus H1 pilot comparison. References the approved A–F, E1, E2, and G1 checks, and the S001 skill-library release case.

## User Scenarios and Testing

### User Story 1 — Decide what the release can ship (Priority: P1)

As the release owner, I can see what actually worked on Mac and Windows
and decide what 0.18 can ship and claim.

## Functional Requirements

- **FR-001**. Record the candidate SHA, OS, actual client and backend,
  owner workflow, and pass, fail, or untested status for each approved
  scenario. Keep conditional scope visibly open unless it was approved
  and independently verified.
- **FR-002**. In a disposable setup, time from installation completion to
  the first observed rule follow on both hosts. After installation the
  operator needs no terminal. Report first use and second-day use separately.
- **FR-003**. Require the candidate on main with all required jobs green,
  including native Windows. Rerun the shared-repo recall comparison with
  the same synthetic decision prompts and score against `DECISIONS.md`.
  Tagging remains the release owner's decision.

## Success Criteria

- **AC-FIRST-USE (G)**. Record both elapsed times and the concrete first
  rule-follow action. A terminal in the operator flow fails the
  no-terminal check, including on a host where owner confirmation still
  requires terminal fallback. Untested second-day use is recorded
  separately.
- **AC-RELEASE (H)**. Required jobs are green for the exact candidate SHA.
  Release notes match the recorded outcomes.
- **AC-COMPARE (H1)**. Freeze one synthetic decision set and its
  answer key, including a superseded decision and an out-of-scope
  control. Run the same prompts and client conditions against Mora and
  the plain-file baseline. Score one point per correct scoped decision
  or correct abstention. The pilot passes when Mora ties or beats the
  baseline. Report the comparison separately from CI.
