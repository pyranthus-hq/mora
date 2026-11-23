# Tasks: NightShift consumes Mora

The [qualification receipt](qualification.md) records the macOS fixture
results. Live factory acceptance remains open.

## Qualify the released pair

- [x] T001 [NightShift] Run the released pair in disposable macOS vaults.
  Check scoped recall, same-name collisions, mismatched briefs, unconfirmed
  terminal writes, missing/wrong checksum degradation, and independent
  vault write protection. See [results](qualification.md).
- [x] T002 [Joint] Qualify the pinned adapter pair on devbox with synthetic
  project and unreviewed records. Exercise connector startup refusal with a
  fake connector response. See the [devbox receipt](qualification.md#adapter-fix-on-devbox).

## Pull integration after qualification

- [ ] T003 [NightShift/deployment] Select the bounded live pilot, supported OS,
  factory-to-scope mapping, approved corpus and read-only worker access. Verify
  provider/deployment isolation including shell access and verify the real
  connector configuration on that host.
  The devbox fixture proved direct CLI write refusal under a read-only mount.
  Depends on T002.
- [ ] T004 [NightShift] Review and land the
  [adapter candidate](https://github.com/abhikarode/nightshift/pull/73).
  Implemented and fixture-tested on devbox: stable repository/factory binding,
  collision refusal, brief scope/root validation, and read-only mode that
  admits only owner-confirmed search rows and skips terminal writes.
  The simulated run reached the human spec gate before rejection. Landing the
  adapter can proceed alongside T003.
- [ ] T005 [NightShift] Confirm actual context limits/deadlines; record supplied context and visible advisory/required-context behavior through supported interfaces. Keep retries, snapshots and run recovery consumer-owned. Depends on T004.
- [ ] T006 [Independent tester] Run relevant FM-A/FM-B read cases, FM-D1 and FM-E on pinned builds. Publish commands and receipts; stop on isolation/authority failures. No write-back gate is required for pull-only acceptance. Depends on T005.

## Deferred options — separate scope decision required

- [ ] T007 [Deferred] Consider write-back only after a concrete need and separate scope decision. If chosen, design generic receipt/idempotency guarantees; do not add factory semantics to Mora or claim adapter-only deduplication is crash-safe.
- [ ] T008 [Deferred] Consider NightShift outbox and factory checkpoint capture only if write-back is chosen. Retain authoritative run state in NightShift and preserve governed writes. Depends on separately activated T007.
- [ ] T009 [Deferred] If capture is chosen, specify and run FM-C/FM-D2 concurrency/crash/restore cases against the selected generic interfaces. Depends on separately activated T008. Not a read-only gate.

## Decide the tested read-only deployment

- [ ] T010 [Independent tester] Summarize the read-only receipts and measured usefulness/cost from T006. Report limitations; document checks are not integration acceptance. No dependency on deferred T007–T009.
- [ ] T011 [Deployment owner] Decide default read-only use for the tested deployment from T010 evidence. Broader rollout, replication or write-back remain separate decisions; none is a Mora 0.18 release prerequisite.
