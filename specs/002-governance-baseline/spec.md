# Feature Specification: 0.17.1 governance baseline

**Feature**: `002-governance-baseline`

**Status**: Release prerequisite. Begin here.

**Acceptance**: F and H (baseline checks)

## User Scenarios and Testing

### User Story 1 — Keep the write policy during an upgrade (Priority: P1)

As a Mora owner, I can upgrade and keep my existing write policy and notes.
Agent-authored content cannot become owner-approved or executable instructions
by entering through a different supported surface.

## Functional Requirements

- **FR-001**. Record a reviewed, landed baseline with required macOS, Linux,
  and Windows checks green on the same revision. Record any subsequent
  release candidate against that baseline.
- **FR-002**. Exercise MCP, CLI write, handoff, corrections, task writes, imported files, and any existing route to rule confirmation
  or activation. For agent-controlled input, verify the expected tentative,
  held, or refused outcome and which index and rule surfaces can see it.
- **FR-003**. Preserve the intended owner path and verify the advertised
  `open`, `propose`, and `readonly` behavior for affected mutation entry
  points. Do not count an agent-session environment marker as proof of
  owner identity.

## Success Criteria

- **AC-BASE (H)**. The evidence identifies a landed SHA and the required
  green jobs for it, including native Windows. Cancelled jobs and a green
  result on a different SHA leave the prerequisite open.
- **AC-POLICY (F)**. With synthetic records, record expected and observed
  results per write path and applicable policy. Any owner-authority escape
  blocks the dependent feature. A known gap needs a blocking regression or
  an explicit disposition before handoff.
