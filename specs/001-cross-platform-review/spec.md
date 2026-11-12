# Feature Specification: Cross-platform Mora review app

**Feature**: `001-cross-platform-review`
**Created**: 2026-11-12
**Status**: Draft for implementation review
**Input**: [Mora 0.18 scope and acceptance map](../mora-0.18.md), build item 3 and case G1

## User Scenarios and Testing

### User Story 1 — Review an agent note without a terminal (Priority: P1)

As a Mora owner, I can open one local review window, see what an agent saved
or proposed, and Keep or Discard it. On Windows I launch it from Start. On
Mac I can launch it directly or open it from Mora Status when installed.

**Independent test**: Start with one saved tentative note and one held
proposal in a disposable vault. Review them through the app and observe
their different effects on the queue and vault.

1. **Given** a note waiting for review, **when** I open it, **then** I see the
   full text, title, project or other scope, source, and review state.
2. **Given** I approve an eligible saved note or held proposal, **when** the
   app reports success, **then** the correct note leaves the queue and a
   fresh agent can find it in the intended scope with its original
   agent-authored provenance.
3. **Given** a saved note or held proposal, **when** I Discard and confirm,
   **then** it leaves the queue. A saved note is no longer retrievable,
   and a held proposal does not create a memory.

### User Story 2 — Decide on exactly what I saw (Priority: P1)

As a Mora owner, I can tell which version and scope I am approving. A
concurrent edit or an incomplete preview requires another review.

**Independent test**: Open a note, change its text or scope through a
separate supported fixture process, and attempt Keep on the original view.

1. **Given** the displayed revision and scope, **when** I Keep or Discard,
   **then** Mora verifies a human approved that exact action, revision, and
   scope before applying it.
2. **Given** a changed or already decided item, **when** I act on a stale
   view, **then** the app shows what needs refreshing and makes no new
   decision. Cancel also makes no decision.
3. **Given** unreadable, truncated, or redacted content, **when** it is
   shown, **then** Keep is unavailable. An incomplete preview cannot become
   an approved note.

## Requirements

- **FR-001**: Provide a local review window on Mac and Windows with direct
  launch on both, a Windows Start entry, and an entry from the existing Mac
  menu bar when that companion is installed.
- **FR-002**: Read the configured local Mora installation's saved and held
  queues. Show full eligible text, title, scope, source, and review state.
  Show empty, loading, unavailable, and failed states distinctly.
- **FR-003**: Bind each owner decision to its action, item, scope, and exact
  displayed revision. Cancel, stale, expired, repeated, or unverifiable
  decisions must not produce a new mutation. An agent or background process
  must not be able to approve through the app by asserting it is the owner.
- **FR-004**: Apply existing Keep and Discard governance. Keeping an
  agent-authored note preserves agent provenance and does not mark it as
  the owner's own words.
- **FR-005**: Refresh the queue and counts after a completed decision. Show
  uncertainty, errors, and index lag without claiming the note is already
  retrievable. Omit Keep for unreadable, truncated, or redacted content.
- **FR-006**: Keep review local. Use the existing vault and write-policy
  boundaries. Offer the existing owner-terminal fallback where app approval
  is unavailable and keep the app read-only on that host. Leave project management, sync management, a Windows tray,
  and release signing outside this feature.

## Success Criteria

| ID | Acceptance |
| --- | --- |
| AC-MAC | On a Mac test account, open the installed app directly and via the menu bar when Mora Status is installed. Complete the shared review and recall scenarios without a terminal. |
| AC-WIN | Launch from Start on the supported Windows backend and actual pilot client. Complete the shared review and recall scenarios without a terminal. |
| AC-STALE | On both hosts exercise changed and incomplete previews, expired and repeated approvals, and a caller without owner proof. No new decision applies. A fresh preview with new proof succeeds. An unavailable verifier leaves review read-only and explains the fallback. |

The detailed fixture steps and evidence requirements are in
[quickstart.md](quickstart.md).
