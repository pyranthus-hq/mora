# Mora 0.19 NightShift integration

The release scope is NightShift integration and the Spearhead workflow that
exercises it. [Mora 0.18](mora-0.18.md) owns the Saahith onboarding and desktop
workflows, skill library, and one-report friction pilot.

## Released baseline

[NightShift v0.4.0](https://github.com/abhikarode/nightshift/releases/tag/v0.4.0) ships an existing
[Mora CLI adapter](https://github.com/abhikarode/nightshift/blob/7f7c4eb1134edd9c448c82be7cfd1ab651c78dbe/docs/memory.md). Real daemons default to Mora when
memory is unconfigured. Configured Eywa takes precedence and `none` opts out.
The adapter pins Mora 0.17.0 by binary checksum on Linux and macOS. The
[pin table](https://github.com/abhikarode/nightshift/blob/7f7c4eb1134edd9c448c82be7cfd1ab651c78dbe/src/memory/mora/pin.ts) has no native Windows entry.
It reads a project brief and scoped search at station start. The
[spec and code prompt path](https://github.com/abhikarode/nightshift/blob/7f7c4eb1134edd9c448c82be7cfd1ab651c78dbe/src/pipeline/mora.ts) labels owner-confirmed
results as rules and other results as advisory notes. At run end it writes
unconfirmed outcome notes and a handoff. Its ledger records identifiers and
hashes. Owner review remains in Mora, and NightShift owns run approval.

A [real-binary fixture run](011-nightshift-factory-memory/qualification.md)
on macOS arm64 confirmed scoped recall for distinct, correctly bound
projects. Two scope checks failed. Repositories with the same short name
shared an effective scope, and a mismatched checkout mixed the returned
brief with the expected project's search results. MCP read-only allowed
both CLI writes. A filesystem write barrier preserved recall and blocked
both writes.

## Build order

1. Fix the demonstrated [adapter gaps](011-nightshift-factory-memory/spec.md#released-adapter-changes).
   Use a stable factory/repository binding, reject collisions and invalid
   bindings, and validate the returned brief before it can reach a prompt.
2. Add an explicit read-only mode that skips terminal notes and handoffs.
   Keep the independent vault write barrier and qualify it on the intended
   factory host. Run the remaining [acceptance tasks](011-nightshift-factory-memory/tasks.md)
   with the real binary and approved corpus before a live pilot.
3. Test the Spearhead logging, recovery and recall workflow. Keep NightShift
   in charge of editing the spec, reapproval and run state. Capture, replay,
   restore and wider deployment are separate acceptance work.

## Spearhead integration acceptance

Replay the redacted T001 case in a disposable project. An inverted criterion
and a missing edit action made the pending spec unusable. Record the scoped
friction report, attach the verified relaunch and card comparison, and recall
the report in a fresh session by symptom. Require zero matching results in an
unrelated project, including during an index refresh. Re-import the reviewed
Drona fixture twice and verify one report and one entry per recovery ID. Keep
the workaround and the open NightShift defect separately visible.

Where the integration supports editing a pending spec, show the changed
criterion and require human approval of the new revision. NightShift owns
the spec edit and run state. Mora owns the scoped record and its evidence.
Default native friction capture depends on the replay passing. Keep raw
traces and private client material on the source host.

## Workstream

[Spec](011-nightshift-factory-memory/spec.md) ·
[Plan](011-nightshift-factory-memory/plan.md) ·
[Tasks](011-nightshift-factory-memory/tasks.md)

## Factory acceptance

The factory cases reuse the applicable 0.18 acceptance fixtures.
See [011 specification](011-nightshift-factory-memory/spec.md) for complete
pass conditions and the controlling architecture clarification. Relevant FM-A/FM-B
read cases, FM-D1 and FM-E govern pull-only use; capture/write-back is deferred.
The pilot measures the actual limits and deployment boundary.

| Case | Extends | Factory pass condition |
| --- | --- | --- |
| FM-A | E1 | Same-repo factories remain isolated across tools, evidence, generated files, filesystem access, refresh and restore. Unbound/revoked identities refuse. |
| FM-B | F/G1 | Exact trust mapping and human proof; no worker self-approval or injected authority; reproducible bundles and revocation-aware replay. |
| FM-C (deferred) | D1/DR1 write paths | Four workers, 100 unique events, duplicate delivery and crash injection preserve captures/proposals exactly once logically, including restore and changed-payload conflicts. |
| FM-D1/FM-D2 | D1 | FM-D1: visible bounded advisory failure and required-context blocking. FM-D2 (deferred capture): ledger-backed checkpoint recovery and isolated restore. |
| FM-E | H1 | Frozen 20-task comparison with curated files and memory off; no harmful carry-over, baseline recall parity and a pre-registered material benefit, with cost measurements. |
