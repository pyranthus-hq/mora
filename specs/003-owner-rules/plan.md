# Implementation Plan: Opt-in project owner rules

**Prerequisite**: [Baseline receipt](../002-governance-baseline/tasks.md)
T003. Evaluate the existing owner-interactive local review first so the
pilot can run before the app is available. Record the signal that proves
human confirmation of the revision and scope, and the direct agent call
that it rejects. If the baseline cannot prove this, leave activation
blocked until a verified path is available.

## Delivery order

1. Inspect project binding in `internal/mora/project_bind.go`, startup in
   `internal/mora/hook.go`, and existing local owner review. Reuse the
   confirmed revision and scope boundary. Keep instruction projection
   distinct from the existing command-warning rules.
2. Project the active set into `.claude/rules/mora.md` only in an opted-in
   project. Use a deterministic, bounded render. Reconcile generated
   content only. If a hand edit overlaps a generated section, preserve it
   and report the conflict. Retirement must remove or disable the stale
   Mora-owned projection. If necessary, preserve the edited file outside
   the loader path and give the owner a recovery path.
3. Verify opt-out, retirement, stale confirmation, and restart. Check the
   native loader and resulting behavior in separate actual sessions.
   Record a minimal pilot receipt before widening rule scope.
