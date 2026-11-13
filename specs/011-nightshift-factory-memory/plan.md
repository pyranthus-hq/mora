# Implementation Plan: NightShift factory memory

**Input**: [Specification](spec.md), [release map](../mora-0.18.md), and the existing NightShift MemoryProvider contract. This is a two-repository integration; a Mora-only PR does not deliver it.

## Reuse and ownership

| Existing package | Reused contract | Factory extension |
| --- | --- | --- |
| [002 governance](../002-governance-baseline/spec.md), [001 review](../001-cross-platform-review/spec.md) | F/G1 write policy, exact revision and human proof | FM02 mapping and FM-B worker self-approval controls |
| [003 rules](../003-owner-rules/spec.md), [005 skill](../005-project-skill/spec.md), [006 setup](../006-project-setup/spec.md) | Opt-in project files, retirement and E1 isolation | FM01 factory identity; exclude unrelated/global files even for the same repo |
| [007 handoffs](../007-session-handoffs/spec.md) | D1 persisted/read/search/startup checks | FM04 run/session checkpoints; replication remains separate |
| [008 budgets](../008-startup-budget/spec.md) | E2 bounded startup/recall | Separate factory prompt/deadline measurements; no authority widening |
| [009 validation](../009-release-validation/spec.md) | Pinned revision and actual-client evidence | FM-A through FM-E integration receipt |
| [010 friction](../010-friction-feedback/spec.md) | DR1 report/recovery/source distinction | Factory event metadata and replay-safe capture |

NightShift owns the provider adapter, immutable factory/repo binding, outbox and bundle evidence, checkpoint selection, cancellation and required/advisory dispatch policy. Mora owns server-side access enforcement, policy-preserving status/receipt APIs and canonical idempotency-key publication. Factory deployment owns worker filesystem isolation, credentials, backup and retention. Neither repository may silently redefine the other's trust states or approval semantics.

## Delivery order

1. **Pin the integration baseline.** Re-audit the selected deployed revisions and obtain the reviewed 002 baseline receipt. Inventory the actual provider interface and scope-safe Mora operations. Define additive compatibility or an explicit version bump before changing a schema. Select the factory host OS and record its own acceptance; Mac/Windows client evidence does not cover Linux. Confirm that factory workers cannot access owner proof, service configuration or another vault. Use synthetic fixtures; no customer migration.
2. **Read-only provider slice.** Implement FM01 and FM02 read mapping, FM04 evidence/context snapshots and FM05 bounded outage behavior. A dedicated per-factory service is the initial topology. Mora enforces the profile binding and policy-preserving status/receipt reads server-side. Seed and review the corpus through a proven owner path before granting the worker read-only profile; imported/copy approval is not inherited. Do not add a second retrieval engine or broaden a profile to get brief. Run the read-only gate in spec.md and compare the curated-file baseline before adding writes.
3. **Capture and handoff slice.** Agree the stable event key and receipt schema across both repositories. Freeze stable producer-origin identity, payload canonicalization and current-authorization checks on replay. Implement Mora canonical deduplication with record/proposal publication and recovery; a separate adapter cache is insufficient. Then connect NightShift's durable outbox and checkpoint handoffs. Reuse existing governance and D1 fixtures, including held/refused outcomes. Run FM-C and all FM-D recovery cases.
4. **Default-on gate.** An independent tester runs FM-A through FM-E on exact integrated builds, including policy bypasses, restore, malformed/unknown states, source validation and timeout cancellation. Record cold/warm costs and the pre-registered benefit target. All outcomes remain open until measured; one passing deployment does not authorize every factory.
5. **Later deployment shapes.** Multi-origin/offline replication or cross-factory sharing needs its own authorized scopes, conflict/revocation semantics, transport and restore acceptance. Keep this outside the first pilot and the general 0.18 release gate.

## Implementation locations to verify at baseline

- Mora: `internal/mora/agents.go`, `mcp_project.go`, governed MCP write/proposal dispatch, canonical memory publication, handoff and index receipt paths. Folder binding alone is not authorization.
- NightShift: `src/memory/interface.ts`, provider registry/factory, `src/pipeline/memory.ts`, run ledger and retry/checkpoint paths. Preserve the existing trust filter and approved spec/base/config checks.
- Add provider conformance and fault-injection fixtures in the owning repositories. Document runnable integration commands with test names when implemented; this planning PR does not invent a passing harness.

## Evidence receipt

Record both SHAs, schema versions, OS/client, factory/profile identity, corpus and fixture hashes, enabled tool set, write policy, index state, results per FM case, context bytes, latency and unresolved blockers. Redact secrets and keep raw customer transcripts out. Separate document validation, baseline unit checks and actual factory product acceptance.
