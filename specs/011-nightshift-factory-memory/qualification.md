# Released adapter qualification

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
