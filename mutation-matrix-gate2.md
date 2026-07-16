# Gate 2 mutation matrix — PR 6 ownership (`gate2-concurrency-upgrade`)

Packets F + G. Every owned row reproduced: production mutation → named test RED → restore.
Rows owned by PRs 1–5 are out of scope here; this file records PR 6's certificates only
until the full 93-row rollup lands at gate close.

| # | Mutation (production call site) | Test | Status |
|---|---|---|---|
| 28 | Remove `busy_timeout` from `rwIndexDSN` | `TestNoUserVisibleSQLITEBUSY` | CLOSED |
| 29 | Swallow the FDA open error in `ingestIMessage` | `TestFDALossNeverStampsSuccess` | CLOSED |
| 30 | Serve a schema-stale index instead of refusing | `TestUpgradePreservesState` | CLOSED |

## Incident replays (Finding 9c, real chokepoints)

| Incident | Test | Status |
|---|---|---|
| #108 WAL lock storm (multi-PROCESS) | `TestNoUserVisibleSQLITEBUSY` | CLOSED |
| Accidental unattended vault flip | `TestAccidentalVaultFlipIsBlockedAndVisible` | CLOSED |
| `sources.json` lost update across PROCESSES | `TestSourcesRMWNoLostUpdateAcrossProcesses` | CLOSED |
| FDA revocation | `TestFDALossNeverStampsSuccess` | CLOSED |

## HEALTH-08 signing clause — dated HOLE

Mora ships unsigned/unnotarized (`install.sh` ad-hoc-signs at install time). The
"preserve macOS permission identity across a signed binary swap" sub-clause of
HEALTH-08 is unmeetable today. Quarantined until a dedicated signing/notarization
tracking issue lands; expiry **2026-10-15**. Not a false certificate — the FDA
loud-failure arm and the upgrade-preserves-state arm still close.
