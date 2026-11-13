# Tasks: NightShift factory memory

All boxes remain open. Owner labels identify repository/deployment responsibility; they are not parallel-execution instructions.

## Phase 1 — Pin and contract

- [ ] T001 [Joint] Record the selected Mora/NightShift SHAs, reviewed 002 baseline, provider interface, supported operations and deployment boundary. Inventory all exposed mutation paths and distinguish the tool catalog from access grants. Covers FM01/FM02; prerequisite for all other tasks.
- [ ] T002 [Joint] Freeze identity, trust-state, source, context-bundle, receipt and stable event-key schemas; agree compatibility/versioning, supported factory OS, corpus seeding/destination review, stable producer-origin identity, canonical payload hashing and authorization on replay, required-memory defaults and evidence retention. Pin the numeric context caps and counting rules from FM05. Pre-register the 20-task corpus and material benefit threshold. Depends on T001. Covers FM01–FM05 and FM-E.

## Phase 2 — Read-only pilot

- [ ] T003 [Joint/deployment] Provision immutable factory/repo bindings and isolated service/worker access. Mora must enforce the binding/profile and policy-preserving status/receipt reads server-side; adapter filtering alone cannot satisfy this task. Seed/review the synthetic corpus through a proven owner path, without importing approval implicitly. Remove mutation/proposal tools from the worker read-only profile. Add same-repo A/B/personal fixtures and unbound/revoked controls, including filesystem and generated-file routes. Depends on T002. Covers FM01/FM-A.
- [ ] T004 [Joint] Implement the scoped Mora provider read/admission mapping and immutable source validation. Preserve agent authorship, exact revision approval and unknown-state refusal; no worker self-approval. Depends on T003. Covers FM02/FM04/FM-B.
- [ ] T005 [NightShift] Record frozen bundles and access-controlled prompt evidence; implement authority revalidation, conflict handling, total deadlines/cancellation and advisory/required outcomes. Depends on T004. Covers FM04/FM05/FM-B/FM-D.
- [ ] T006 [Independent tester] Run existing provider conformance and the read-only gate: FM-A/FM-B read cases, FM-D1 and FM-E. Publish exact commands and receipts; do not widen scope or proceed after an isolation/authority failure. Depends on T005. Covers read-only acceptance.

## Phase 3 — Durable capture and handoffs

- [ ] T007 [Mora] Implement canonical event-key/payload-hash deduplication and outcome recovery for direct writes, held proposals and later decisions. Test same-key replay, changed-payload conflict, save-before-response failure and saved-but-unindexed recovery. Depends on T002 and T006. Covers FM03/FM-C.
- [ ] T008 [NightShift] Implement durable outbox/acknowledgements against T007, preserving event IDs across restarts and retries. Implement run/session checkpoint handoffs using existing governed paths and ledger provenance; unreviewed recovery views cannot become approved context. Depends on T007. Covers FM03/FM04/FM-C/FM-D.
- [ ] T009 [Joint/deployment] Verify simultaneous writers and readers during rebuild, crash recovery, pending decisions and backup/restore of records plus control state. Enforce snapshot retention and revocation on reuse. Depends on T008. Covers FM03–FM05/FM-A–FM-D.

## Phase 4 — Decide deployment readiness

- [ ] T010 [Independent tester] Run FM-A through FM-E on pinned integrated builds, compare the pre-registered benefit target and report every failure/limitation. Do not count document or baseline-unit checks as integration acceptance. Depends on T009. Covers all factory acceptance.
- [ ] T011 [Release owner] Review the receipts and decide default use only for the tested deployment shape. Record wider rollout/replication as separate work with explicit scope and new isolation/load evidence. Depends on T010. Covers FM05.
