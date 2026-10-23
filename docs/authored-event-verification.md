# Authored event opt-in verification

Baseline: remote `main` verified by `git ls-remote` as
`46feaf1eacb8e8d26cae294dab83ddbb6b1eba43` on 2026-09-29.
Implementation is on `codex/pyr-69-authored-event-time` in an isolated worktree.
No shared checkout switch, UI changes, or existing test assertion/golden edits.

## Behavior and evidence

| Requirement | Fresh execution evidence |
| --- | --- |
| Reproduce before editing | Built baseline CLI; `write` saved a synthetic note, ordinary list returned one, event list returned `memories: null` (zero records). |
| Explicit opt-in and distinct source | Candidate CLI returned one authored record only with `--include-authored-writes`, `event_source: authored_write`, and event time equal to stored creation instant. MCP parity and alias tests passed. |
| Flag-off compatibility | Baseline and candidate bytes matched for both empty and nonempty connector event receipts, including explicit false. Nonempty comparison SHA-256: `f4c7a2318cebb38870eefce23e482c3975d0397506c65551eb47fbc5daf7559e`. Existing activity goldens and frozen contract corpus checks passed unchanged. |
| Boundaries and exclusions | New tests passed for inclusive bounds, deterministic ties, pre-limit selection, future/malformed/zero timestamps, filesystem document exclusion, unsupported connectors, derived provenance, and required event window/type validation. |
| Resync flood answer | Real CLI filesystem connect/sync on a synthetic `codex-memory` source: unchanged manifest preserved old creation time. Changing only source mtime re-materialized the same note and reset `CreatedAt`, returning it to the opted-in window. Flag-off stayed connector-only. |
| Native note rebuild stability | Real CLI `index rebuild --force` preserved the native note's creation time. |

**Risk:** opt-in does not prevent bulk re-dating of imported authored mirrors.
Default remains false. Consumers requiring the Home anti-resync-flood guarantee
must leave this flag off. This patch does not claim to unblock that guarantee.
Stamps, search, and default derivation remain connector-only. Opted-in event
lists expose the event source for connector rows as well as authored rows.

## Reproduce

Build the baseline in a separate worktree at the SHA above and the candidate:

```sh
CGO_ENABLED=0 go build -buildvcs=false -o "$EVIDENCE_DIR/mora-before" ./cmd/mora
# In the candidate worktree:
CGO_ENABLED=0 go build -buildvcs=false -o "$EVIDENCE_DIR/mora-after" ./cmd/mora
python3 scripts/verify-authored-events.py "$EVIDENCE_DIR/mora-before" "$EVIDENCE_DIR/mora-after" "$EVIDENCE_DIR"
```

`EVIDENCE_DIR` must be an existing private scratch directory. The script creates
only synthetic records in an isolated configuration root and runs real binaries;
it prints assertions and the evidence directory. No live owner vault is used.
The SHA-256 varies with the synthetic timestamps/path; the equality must hold.

Executed checks (all passed):

```sh
CGO_ENABLED=0 go test -buildvcs=false ./internal/activity ./internal/memory ./internal/mcp -count=1
CGO_ENABLED=0 go test -buildvcs=false ./internal/mora -run 'Test(Contract(JSONBaseline|GoldenCorpusIsFrozen|GoldenCorpusIsComplete|LeafJSONReceipts)|Authored|Activity|Recent|ListSourceReceipt|FilesystemIncremental)' -count=1
CGO_ENABLED=0 go vet -buildvcs=false ./internal/activity ./internal/memory ./internal/mcp ./internal/mora
```

The real CLI script passed all four groups: gap/opt-in, nonempty byte equality,
resync re-dating/default guard, and native rebuild stability.

## Failures and remaining review

- Initial sandbox attempts could not resolve dependency hosts or bind loopback
  test listeners; approved escalated execution resolved both.
- Go automatic VCS status failed in this runner. Both binaries used
  `-buildvcs=false`; source SHA was verified separately. No signing or hooks
  were bypassed.
- New-test compile error (wrong parser helper) and a new-test timestamp string
  comparison across timezones were corrected; checks above passed afterward.
- `~/.codex/bin/codex-verify` failed with exit 127: executable absent. It is also
  absent under `/home/akarode/.codex/bin`. This kernel repo has no
  `.codex/verification.json`. No session-gate success is claimed.
- No full repository suite, live connector accounts, or physical macOS testing
  was performed. Gate owns independent review and any required broader checks;
  Adit owns merge approval. Nothing is merged.
