# Implementation Plan: Cross-platform Mora review app

**Feature**: `001-cross-platform-review`
**Spec**: [spec.md](spec.md)
**Release dependency**: record the reviewed 0.17.1 governance revision from
[PR #666](https://github.com/pyranthus-hq/mora/pull/666) before implementing
approval behavior. See the [0.18 scope and acceptance map](../mora-0.18.md).

## Summary

Build one review experience and use Mora's existing local governance as the
authority. Use the [Mac review draft](https://github.com/pyranthus-hq/mora/pull/656)
as a behavior reference for full-text preview, Keep, Discard, and count refresh. Add the decision check
missing from that draft before making the window an approval surface.

## Technical Context

| Area | Implementation direction |
| --- | --- |
| Queue | Extend or reuse the typed queue in `internal/mora/review.go`, proposal inspection in `mcp_proposal_show.go`, and saved-note reads. Use disposable vault fixtures. |
| Mutation | Reuse confirmed-note, undo, and proposal-decision operations in `internal/mora`. Keep existing agent/MCP and terminal rules. A desktop adapter invokes the same authority. |
| Shell | Plan one desktop UI in `apps/mora-review/`. Evaluate the existing Tauri shell pattern first in a small Mac and Windows spike. Keep desktop dependencies outside the Go CLI module. The spike records the selected shell and the supported Windows backend, including the actual WSL-backed pilot route if used. |
| Transport | Prefer app-owned local IPC or a typed sidecar channel. Keep note bodies and approval payloads local. Use the existing configured vault. No generic new approval MCP tool. |
| Launch | Install a Windows Start entry and support ordinary Mac app launch. Connect the Mac Status menu item when available. An unsigned prototype must use the host's normal permitted test path. |

## Decision Contract

1. Read the canonical item and issue a preview receipt. The revision covers
   the full title, body, scope, type, source, provenance, item kind (saved
   or held), and review state. The authority rejects Keep for incomplete
   previews even if a caller ignores the disabled control.
2. When the owner clicks an action, issue a short-lived, one-use challenge
   bound to the local installation, action, item, scope, and revision. Use
   an OS-mediated owner-verification flow tied to the trusted app. Enroll
   any verification key through an owner-controlled flow. A generic UI
   event, shared process environment, IPC identity, or MCP result alone
   cannot establish human approval. Require fresh proof per decision.
3. Verify the proof and re-read the item under the existing mutation lock.
   Consume the challenge once and record the decision with the existing
   governance machinery so a retry or restart cannot apply it again. Keep
   retains agent provenance. Use existing approval or rejection for held
   proposals and existing undo semantics for discarded saved notes.
4. Return an explicit applied, cancelled, stale, unavailable, or failed
   result. Distinguish vault success from index visibility. Refresh the
   queue and display lag or any required action.

The spike must prove that ordinary same-account agent callers cannot use
the supported app or local decision endpoint to approve without a person.
It does not claim protection from OS compromise or arbitrary direct edits
to files the account already owns. Record each OS's UI and core proof,
backend configuration, expiry and replay checks, and limitations in
`research.md`. Platform-backed credentials are candidates. Pick a
mechanism only after showing that a fresh owner action binds to the exact
preview and the core can validate it. If proof is unsupported or
unavailable, show a read-only queue and direct the owner to `mora review`.
Leave that host's desktop acceptance open.

## Constitution Check

The desktop layer has no cloud dependency. The core remains pure Go and
the MCP catalog and connector boundaries stay intact. Keep review
authorization, scope, and provenance in core. Use existing test conventions
and verify both host flows separately. See
[the repository rules](../../AGENTS.md).

## Delivery Order

1. Inspect the reviewed baseline and prove a minimal local shell and
   owner-confirmation path on Mac and Windows.
2. Implement the shared typed queue and revision-bound decision checks with
   regressions for stale, cancelled, incomplete, unauthorized, and replayed
   decisions.
3. Wire the single review window and launch entries. Run isolated vault tests
   with saved and held records and verify actual fresh-session retrieval.
4. Have one reviewer inspect the changes and a separate tester run the
   [acceptance script](quickstart.md) on both targets. Record build revision,
   client and backend versions, result, and any gaps in the PR.
