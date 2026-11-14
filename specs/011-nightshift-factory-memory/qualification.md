# NightShift adapter qualification

## Released v0.4.0 baseline

November 13, 2026. macOS arm64. NightShift v0.4.0
(`7f7c4eb1134edd9c448c82be7cfd1ab651c78dbe`) with published Mora v0.17.0
(`c811b3906cea08270d7f641925c4edf8e4a63af5`). The archive passed the
Mora release checksum. The executable SHA-256
`eb8c1cd4911e37ea3df5cf713f26682f40ed3c2adcd169edf8a3692c8123f93e`
matched NightShift's built-in pin without a test pin override.

The fixture used a separate home and connector-free Mora configuration,
synthetic notes and checkout directories, and the unmodified released adapter.
Two end-to-end cases passed through the built NightShift daemon with simulated
worker stages and the real Mora CLI. Direct adapter checks covered both
project bindings, a mismatched checkout, same-name repositories, and missing
or altered binaries.

| Check | Observed result |
| --- | --- |
| Distinct bound projects | Each direct recall returned one in-scope note and excluded the other project's and personal markers. The daemon spec prompt likewise excluded the other scope and personal note. |
| Same short name | `team-one/shared` and `team-two/shared` both bound to `project:shared`. The second recalled the first's synthetic note. |
| Mismatched checkout | An alpha provider called with a beta checkout emitted the beta brief alongside alpha search results. |
| MCP policy set to read-only | The daemon recorded two unconfirmed writes. The reviewer note and run handoff both persisted. |
| Filesystem write barrier | With the vault protected and index/state writable, the daemon still recalled scoped notes. Both terminal writes failed with `MORA_FAILED` and zero write receipts. The direct adapter fixture also had identical before/after vault file hashes. |
| Missing or altered executable | Recall degraded with `MORA_NOT_INSTALLED` or `MORA_CHECKSUM_MISMATCH`. |

The macOS fixture demonstrates filesystem enforcement. The intended factory
host, connector-refusal test, owner-confirmed positive fixture, live worker
run, and full factory acceptance still need qualification. The test supports
the adapter changes in the [specification](spec.md#released-adapter-changes).

## Adapter fix on devbox

November 13, 2026. Devbox (Linux x86-64, Node 22.22.1) ran the
[NightShift adapter candidate](https://github.com/abhikarode/nightshift/pull/73)
at commit `7ed867029c97317e9212025faed103b021153ba5` against the published Mora
0.17.0 binary, source commit `c811b3906cea08270d7f641925c4edf8e4a63af5`. The
archive SHA-256 was `26907123a292a719370abf9790b9fd854d2aad51f38db989f63b3d5a35c20aec`;
the binary SHA-256 was `b1e5ba02392a6c050259c4716946d10c421353d53e761b0b173f005d5a160473`.
Both matched the NightShift pin. The test used the built daemon with simulated
workers, a disposable checkout, isolated home and connector-free Mora config,
and a synthetic vault with distinct project, unreviewed and personal markers.
The regression seeded owner-confirmed synthetic records through Mora's
terminal confirmation flow.

| Check | Devbox result |
| --- | --- |
| Same-name repositories and factory identity | `acme/app` and `second/app` derived distinct scopes and recalled their own owner-confirmed marker only. The same slug under a second factory ID also derived a different scope. |
| Invalid bindings | An unbound checkout, another repo's checkout and a duplicate pre-existing scope failed closed with `MORA_BINDING_MISMATCH` and no prompt section. Read-only startup did not alter bindings. |
| Read-only daemon | A spec reached NightShift's human gate. Its prompt included the approved project marker and excluded the other project's, personal and unreviewed markers. Rejecting the spec recorded `write-back-disabled` with zero write receipts or memory degradation. The adapter CLI runner refused direct note and handoff requests. |
| Independent filesystem protection | With Bubblewrap mounting the vault read-only, direct Mora CLI search still recalled the approved project record. Direct CLI note and handoff writes failed with read-only filesystem errors, and vault file hashes matched before and after. |
| Related regression coverage | All 22 focused tests passed across six suites. The connector startup refusal used the built daemon and a fake CLI returning an enabled connector; no personal account was connected. |

The two direct pulls took 636 ms combined in this run, and their complete prompt
sections were 354 and 353 UTF-8 bytes. These are single-run observations. The
regression is reproducible with a verified Linux amd64 Mora 0.17.0 executable
and Bubblewrap on devbox:

```sh
npm ci --ignore-scripts
node node_modules/typescript/bin/tsc -p tsconfig.build.json
NIGHTSHIFT_TEST_MORA_BIN=/absolute/path/to/mora npx vitest run --project local --reporter=verbose tests/e2e/mora-real-pin.live.ts
```

This qualifies the adapter fixture on devbox. A live pilot still requires a
deployment-specific worker access boundary, source and applicability checks,
outage and context budgets, and the remaining read acceptance cases. No live
factory or vault was used.
