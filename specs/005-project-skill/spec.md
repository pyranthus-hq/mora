# Feature Specification: One project skill

**Feature**: `005-project-skill`

**Status**: Pilot. One hand-authored skill and one recurring task.

**Acceptance**: C. Provides the skill fixture for E1.

## User Scenarios and Testing

### User Story 1 — Approve one skill for a recurring task (Priority: P1)

As a project owner, I can approve one skill and have the client's native
loader use it for a recurring task in that project.

## Functional Requirements

- **FR-001**. Approve the exact files of one standard skill folder and
  install it into the project's native loader location. Approval applies
  to the approved revision on that machine.
- **FR-002**. The task can discover and load the skill in a fresh session.
  Other projects and the unrelated task must not load it. Detaching it
  removes only Mora-managed files.
- **FR-003**. Changed content needs a new approval. The [0.18 skill library](../012-skill-library/spec.md) adds chat intake,
  version history, and discovery after the pilot. Skill sync and expanded
  sub-agent support remain follow-ups.

## Success Criteria

- **AC-SKILL (C)**. Use one approved skill with a synthetic output marker
  on the actual Windows client and repeat the workflow on Mac. Record the
  prompts and native loader evidence. The skill loads in at least four of
  five matching fresh sessions per host and never on the unrelated control.
- **AC-SKILL-SCOPE (E1)**. Verify another project cannot load the skill,
  edited content does not receive the old approval, an agent cannot
  approve, and detach preserves unmanaged and edited files. This gives E1 a known skill marker.
