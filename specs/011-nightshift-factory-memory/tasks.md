# Tasks: NightShift consumes Mora

The [qualification receipt](qualification.md) records the macOS fixture
results. Live factory acceptance remains open.

## Qualify the released pair

- [x] T001 [NightShift] Run the released pair in disposable macOS vaults.
  Check scoped recall, same-name collisions, mismatched briefs, unconfirmed
  terminal writes, missing/wrong checksum degradation, and independent
  vault write protection. See [results](qualification.md).
- [ ] T002 [Joint] Validate connector startup refusal, owner-confirmed rule
  and advisory-note labels, and the real pinned pair on the intended
  factory host. Confirm the server-controlled factory/repository mapping
  and approved corpus. Depends on T001.

## Pull integration after qualification

- [ ] T003 [NightShift/deployment] Select the bounded pilot, supported OS, factory-to-scope mapping, approved corpus and read-only worker access. Verify provider/deployment isolation including shell access. Prove that the shipped terminal writes cannot persist in the read-only pilot. Depends on T002.
- [ ] T004 [NightShift] Implement the stable repository/factory binding,
  collision refusal, brief scope/root validation, and read-only mode that
  skips terminal writes. Retest same-name repos and a wrongly bound or
  unbound checkout with the real binary. Verify approved-only admission,
  unchanged execution approval, and zero terminal write calls. Depends on T003.
- [ ] T005 [NightShift] Confirm actual context limits/deadlines; record supplied context and visible advisory/required-context behavior through supported interfaces. Keep retries, snapshots and run recovery consumer-owned. Depends on T004.
- [ ] T006 [Independent tester] Run relevant FM-A/FM-B read cases, FM-D1 and FM-E on pinned builds. Publish commands and receipts; stop on isolation/authority failures. No write-back gate is required for pull-only acceptance. Depends on T005.

## Deferred options — separate scope decision required

- [ ] T007 [Deferred] Consider write-back only after a concrete need and separate scope decision. If chosen, design generic receipt/idempotency guarantees; do not add factory semantics to Mora or claim adapter-only deduplication is crash-safe.
- [ ] T008 [Deferred] Consider NightShift outbox and factory checkpoint capture only if write-back is chosen. Retain authoritative run state in NightShift and preserve governed writes. Depends on separately activated T007.
- [ ] T009 [Deferred] If capture is chosen, specify and run FM-C/FM-D2 concurrency/crash/restore cases against the selected generic interfaces. Depends on separately activated T008. Not a read-only gate.

## Decide the tested read-only deployment

- [ ] T010 [Independent tester] Summarize the read-only receipts and measured usefulness/cost from T006. Report limitations; document checks are not integration acceptance. No dependency on deferred T007–T009.
- [ ] T011 [Deployment owner] Decide default read-only use for the tested deployment from T010 evidence. Broader rollout, replication or write-back remain separate decisions; none is a Mora 0.18 release prerequisite.
