# Tasks: NightShift consumes Mora

All boxes remain open. The current next step is inspection of the actual v0.4.0 integration. Subsequent implementation needs that evidence; deferred options are not commitments or release gates.

## Inspect before designing

- [ ] T001 [NightShift] When v0.4.0 is available, pin its actual revision and inspect the Mora integration/introduction, provider contract and existing adapter. Record availability and behavior without assuming either. No competing adapter before this checkpoint. If v0.4.0 is unavailable or provides no usable Mora integration, report that gap to Adit and stop; do not start T002 or design a substitute without a separate scope decision.
- [ ] T002 [Joint] Pin Mora's revision; map supported scoped operations, generic authorization, provenance/trust/status metadata and demonstrated gaps. Keep factory/run semantics in NightShift. Propose any missing generic Mora capability separately. Depends on T001.

## Pull integration, subject to inspection

- [ ] T003 [NightShift/deployment] Select the bounded pilot, supported OS, factory-to-scope mapping, approved corpus and read-only worker access. Verify provider/deployment isolation including shell access; consumer filtering alone is insufficient. Depends on T002.
- [ ] T004 [NightShift] Reuse the upstream integration path and adapt only demonstrated gaps. Preserve source/revision/trust mapping, exclude unapproved context and keep execution approvals authoritative in NightShift. Depends on T003.
- [ ] T005 [NightShift] Confirm actual context limits/deadlines; record supplied context and visible advisory/required-context behavior through supported interfaces. Keep retries, snapshots and run recovery consumer-owned. Depends on T004.
- [ ] T006 [Independent tester] Run relevant FM-A/FM-B read cases, FM-D1 and FM-E on pinned builds. Publish commands and receipts; stop on isolation/authority failures. No write-back gate is required for pull-only acceptance. Depends on T005.

## Deferred options — separate scope decision required

- [ ] T007 [Deferred] Consider write-back only after a concrete need and separate scope decision. If chosen, design generic receipt/idempotency guarantees; do not add factory semantics to Mora or claim adapter-only deduplication is crash-safe.
- [ ] T008 [Deferred] Consider NightShift outbox and factory checkpoint capture only if write-back is chosen. Retain authoritative run state in NightShift and preserve governed writes. Depends on separately activated T007.
- [ ] T009 [Deferred] If capture is chosen, specify and run FM-C/FM-D2 concurrency/crash/restore cases against the selected generic interfaces. Depends on separately activated T008. Not a read-only gate.

## Decide the tested read-only deployment

- [ ] T010 [Independent tester] Summarize the read-only receipts and measured usefulness/cost from T006. Report limitations; document checks are not integration acceptance. No dependency on deferred T007–T009.
- [ ] T011 [Deployment owner] Decide default read-only use for the tested deployment from T010 evidence. Broader rollout, replication or write-back remain separate decisions; none is a Mora 0.18 release prerequisite.
