# Implementation Plan: Typed memory capture

**Prerequisite**: [Baseline](../002-governance-baseline/tasks.md) T003.
The rule-draft path reuses [owner rules](../003-owner-rules/tasks.md) T002.
Use [review app](../001-cross-platform-review/tasks.md) T008 evidence
before presenting app review as verified on either host.

## Delivery order

1. Test what each actual client emits for interactive typed input and
   agent-launched or headless input. Record the version and trust boundary.
   Continue only where the authority can independently verify origin.
2. Reuse `internal/mora/hook.go` and the existing authored-write policy
   where the client provides the required proof. Keep exact text, intended
   scope, and provenance through persistence and retrieval. Any in-chat
   capture alias is a convenience that uses the same authority.
3. Keep the supported fallback visible. Prove the positive and negative
   scenarios against disposable vaults before listing a client as capable.
