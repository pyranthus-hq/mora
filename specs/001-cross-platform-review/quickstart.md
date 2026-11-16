# Review app acceptance

These scenarios are for the implementation. The planning branch contains
no packaged review app or new review tests.

## Resume the Spec Kit pilot

Spec Kit 1.4.0 is initialized with Codex and Claude Code skills. From the
repository root, select this feature and check its files:

```sh
export SPECIFY_FEATURE_DIRECTORY=specs/001-cross-platform-review
SPECIFY_FEATURE_NO_PERSIST=1 bash .specify/scripts/bash/check-prerequisites.sh --json --require-tasks --include-tasks
```

Run Bash from macOS, WSL, or Git Bash. Codex uses `$speckit-plan`,
`$speckit-tasks`, and `$speckit-analyze`. Claude Code uses
`/speckit-plan`, `/speckit-tasks`, and `/speckit-analyze`. Both read this
feature directory. The project-local feature pointer is generated and
ignored by Git, so set the variable again in a fresh checkout.

## Test preparation

Use a disposable vault and synthetic data. Seed complete saved notes and
held proposals in `project:review-fixture` with unique markers and agent
provenance. Use separate fixtures for Keep, Discard, Cancel, stale preview,
and incomplete content. Bind a second project, `project:other-fixture`.
Record the app revision, Mora backend, OS, and actual agent client. The
tester may use a terminal to prepare fixtures and collect evidence. The
operator reviews entirely through the app.

## Platform launch

- **AC-MAC**. Launch the installed app directly. If Mora Status is installed
  for the test, also open the same queue from its menu item. Record which
  entries were exercised.
- **AC-WIN**. Launch the installed app from Windows Start using the actual
  client configuration for the pilot. Record the native or WSL backend.

## Shared review and recall on both hosts

1. Open one complete saved note and one held proposal. Compare the full
   title, body, scope, source, provenance, and state against the fixtures.
2. Keep a saved note and a held proposal in separate decisions. Confirm
   that the intended entries leave the queue, counts refresh, and resulting
   vault records preserve the exact text, scope, and agent provenance.
3. Discard a different saved note and held proposal. Once the index is
   current, the discarded saved note is absent and the discarded proposal
   has produced no note. Cancel on a separate item. Its state and the queue
   count are unchanged.
4. Start a fresh agent session in the matching project. Ask an ordinary
   question about each kept fixture without supplying an ID. Check that
   both appear with the expected scope and provenance. Repeat in a fresh
   session in the other project and require both markers to be absent.
5. Exercise empty, loading, and backend failure states. Delay indexing in
   a controlled fixture and verify the app distinguishes a successful vault
   write from search visibility.

## Both hosts — AC-STALE

1. Preview a note and change its text or scope with the fixture process.
   Attempt a decision from the old view. No new decision applies. Refresh
   and confirm the new preview. Exactly one decision applies.
2. Submit an expired challenge, a proof for another action or item, and a
   request from a separate same-account client with no owner proof. Replay
   a completed approval before and after backend restart. Confirm that
   none causes a new decision or changes another item.
3. Try to Keep a redacted, truncated, or unreadable note. The UI offers
   no Keep control and the core rejects the same action independently.
4. Make the host verifier unavailable. The queue stays readable, decisions
   are disabled, and the app explains the `mora review` fallback. An
   unavailable verifier leaves that host's no-terminal acceptance open.

Record pass, fail, or untested for each host and scenario. Retain a
synthetic-data screenshot, sanitized queue and decision results, and the
scoped recall evidence IDs. Record an unresolved blocker or missing launch
entry against the relevant case.

## Both hosts — AC-BACKUP and AC-DESTINATION

1. Configure a disposable private backup and an authorized pull. Record
   a successful backup, then inject offline, stale, and diverged states.
   Check the app shows last success, latest attempt, reason, and next
   action. Check the Mac Status summary too. With owner verification
   unavailable, verify status stays visible and retry is disabled.
2. Repeat the same failure over three retries and restart the app. Expect
   one notification and a visible failure. Verify recovery from a success
   receipt, then inject a new failure and expect one new alert.
3. Reconcile diverged history and verify both histories and their unique
   records are recoverable. Repeat setup and retries and verify zero new
   repositories. Verify a scoped reader cannot pull the full backup.
   Verify active consumers are shown before cutover. Retire a fixture share
   and verify its data remains recoverable.

## Core regression commands

Run after implementation. The first line must list the new test group
before the filtered test is accepted as evidence:

```sh
go test ./internal/mora -list '^TestDesktopReview' | grep '^TestDesktopReview'
go test ./internal/mora -count=1 -run '^TestDesktopReview'
CGO_ENABLED=0 go build ./cmd/mora
```
