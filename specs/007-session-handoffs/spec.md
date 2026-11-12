# Feature Specification: Cross-machine session handoffs

**Feature**: `007-session-handoffs`

**Status**: D1 is a pilot. D is conditional on a wider sync decision.

**Acceptance**: D1 now, D only if wider sync enters scope.

## User Scenarios and Testing

### User Story 1 — Resume work from a handoff on another machine (Priority: P1)

As the same owner working from Mac and Windows, I can start a fresh
session on either machine and recover a short, eligible project handoff
written on the other.

## Functional Requirements

- **FR-001**. Use separate writable origin vaults and an authorized
  scoped share. Transfer only eligible structured handoffs. Preserve
  identity, project scope, and agent provenance. Held and rejected
  fixtures stay outside the share.
- **FR-002**. Offline retry and receiver refresh leave one destination
  copy and preserve the origin. Keep raw traces, index databases,
  credentials, and local approval state on the machine. A one-way mirror
  that can delete destination-only records does not satisfy D1.
- **FR-003**. Wider 15-minute, single-owner sync requires owner opt-in,
  an agreed private transport and scope, observable errors and last
  success, preserved conflict versions, and exact per-machine activation
  of synced rules and skills. The current private Git proposal stores
  unencrypted files. Connector data is excluded by default.

## Success Criteria

- **AC-HANDOFF (D1)**. Save one eligible handoff on Windows and receive it
  on Mac. Repeat in reverse. Include offline retry, receiver index
  refresh, repeated delivery, and held/rejected controls. Verify the
  persisted record, explicit search, and startup recall independently in
  the actual fresh agent sessions. Verify an unrelated scope excludes it.
- **AC-SYNC (D, conditional)**. With both hosts online, the wider proposal
  passes when an eligible Mac rule and skill appear as waiting on Windows
  within 15 minutes of saving, retain exact revision, require approval
  there, and offer the kit for a matching project. Test conflicts, failure
  reporting, default exclusions, and no force push.
