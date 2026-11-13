# Implementation Plan: NightShift consumes Mora

**Input**: [Specification](spec.md), especially its controlling architecture clarification, and [release map](../mora-0.18.md). Product acceptance remains open.

## Boundary

Mora stays a general-purpose, agent-agnostic memory service. NightShift pulls from its supported scoped interfaces and owns factory semantics. No factory-specific schema, run state machine or execution approval belongs in Mora.

| Responsibility | Owner |
| --- | --- |
| Generic authorization, scope enforcement, memory records, provenance, revision/trust metadata and honest retrieval status | Mora |
| Factory-to-scope mapping, provider adapter, retrieval timing, prompt admission, snapshots, budgets, retries and run recovery | NightShift |
| Worker isolation, credentials and approved access | Deployment |

Reuse [002 governance](../002-governance-baseline/spec.md), [001 review](../001-cross-platform-review/spec.md), [006 scoped setup](../006-project-setup/spec.md), [008 budgets](../008-startup-budget/spec.md) and [009 validation](../009-release-validation/spec.md). [007 handoffs](../007-session-handoffs/spec.md) and [010 friction](../010-friction-feedback/spec.md) remain reusable general capabilities; they do not mandate factory write-back. An adapter filter cannot replace Mora/deployment access enforcement.

## Next checkpoint

Inspect NightShift v0.4.0's actual Mora integration or introduction once available. Pin both revisions and record the interface, supported operations, trust metadata, access boundary and deployment assumptions. Availability and behavior are not yet verified by this plan. Reuse the upstream path; do not prebuild a competing adapter or freeze protocol/schema/topology choices before inspection.

## Pull integration after inspection

1. Compare the actual interface with existing Mora capabilities. Record only demonstrated gaps; any missing Mora feature must be proposed as a generic memory capability with separate scope.
2. Select one bounded factory pilot and its supported host. Use a reviewed corpus and read-only worker access. Keep factory identity interpretation in NightShift and generic scope authorization in Mora/deployment.
3. Verify the relevant FM-A/FM-B read cases, FM-D1 and FM-E from the spec: isolation, provenance and trust, unchanged execution approvals, bounded visible failures and value against curated files. Repeat FM-A after restore if the tested deployment restores from backup; FM-D2 remains deferred. Test installed rules/skills routes only if enabled, reusing [003 rules](../003-owner-rules/spec.md) and [005 skills](../005-project-skill/spec.md) in that case. Confirm numeric budgets against the inspected interface; prior values/topology are candidates. Stop on isolation or authority failures.
4. Decide default read-only use only for the tested deployment based on these read gates. Deferred capture/restore gates (FM-C/FM-D2) do not block this decision; applicable FM-A restore isolation checks still do. Record exact commands, both SHAs, client/OS, corpus, access configuration, results, latency/context cost and limitations.

## Deferred options

FM03 write-back/outbox/idempotency, factory checkpoint persistence, FM-C and FM-D2 capture/restore acceptance, multi-origin sync and wider rollout require a later scope decision. The earlier detailed factory section is a design checklist subject to the controlling architecture clarification, not a mandate to add factory concepts to Mora. If write-back is later selected, preserve provider-side integrity and governed writes; adapter-only deduplication cannot be presented as a crash-safe guarantee.

No production implementation is delivered by this planning update. The first implementation decision follows the v0.4.0 inspection.
