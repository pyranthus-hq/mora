# Feature Specification: Typed memory capture

**Feature**: `004-typed-capture`

**Status**: Proposed release feature, gated by a client provenance test.

**Acceptance**: B and F. Setup timing is measured in G.

## User Scenarios and Testing

### User Story 1 — Save a fact by typing it in the client (Priority: P1)

As a Mora owner, I can type a fact in my usual client and recall it later
without a terminal or a per-note confirmation. Rules I type can begin as
drafts and become active only after explicit confirmation.

## Functional Requirements

- **FR-001**. Use a supported client signal to distinguish owner-typed
  input from agent-submitted or headless input. A string prefix, caller
  label, or generic MCP response alone does not establish owner standing.
- **FR-002**. For verified owner input, retain the exact fact in its intended
  scope. Personal facts can be recalled across projects. Keep a typed rule
  inactive until its exact revision and scope are explicitly confirmed.
- **FR-003**. Keep unverified input on the ordinary governed path. If a
  client cannot prove provenance, use its supported review fallback and
  keep no-confirmation acceptance open for that client.

## Success Criteria

- **AC-TYPED (B)**. In each supported Mac and Windows client, type a
  synthetic personal fact in one project, then retrieve it in a fresh
  session in another project. Verify text, provenance, and scope with no
  confirmation prompt or terminal on the verified save path.
- **AC-PROVENANCE (F)**. Repeat identical text through agent tools,
  headless submission, a non-capture prompt quoting the trigger, and an
  unsupported client. None receives owner
  standing. Verify a typed rule stays inactive until explicit approval.
