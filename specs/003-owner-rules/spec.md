# Feature Specification: Opt-in project owner rules

**Feature**: `003-owner-rules`

**Status**: First 0.18 implementation slice after the baseline. Start with
one rule on the actual Windows client.

**Acceptance**: A and F. Provides the rule fixture for E1.

## User Scenarios and Testing

### User Story 1 — Confirm and retire a project rule (Priority: P1)

As a project owner, I can confirm a rule once, have my opted-in project
load it in new sessions, and retire it when it no longer applies.

## Functional Requirements

- **FR-001**. A draft or agent suggestion stays inactive until the owner
  confirms the exact rule revision and project. Confirming a suggestion
  records the owner's decision and preserves the suggestion's provenance.
- **FR-002**. After opt-in, project only active confirmed rules into Mora's
  own project rules file. Preserve existing hand edits, remove retired
  rules, and avoid duplicating a rule in startup context when the native
  loader receives it.
- **FR-003**. Keep each generated project file within 6,000 UTF-8 bytes.
  Report the omitted rule count. Global rule generation and automatic
  activation on a second machine require separate decisions.

## Success Criteria

- **AC-RULE (A)**. On the actual Windows client, use the same synthetic
  project task for two fresh sessions after confirmation and one after
  retirement. Observe the rule's specific requested action in both active
  runs and confirm the rule is absent from the retired run. Use a marker
  that makes the action attributable to the rule. Repeat the lifecycle on
  Mac. The wider release evaluation retains the five-run target of at
  least four observed follows.
- **AC-BOUNDARY (E1, F)**. An unconfirmed, changed, retired, or wrong-project
  rule cannot reach the loader. A direct agent call cannot confirm or
  activate it. Verify opt-in, the byte cap and omission notice, repeated
  generation, and preservation of hand edits. The full E1 switching and
  refresh test belongs to [scoped setup](../006-project-setup/spec.md).
