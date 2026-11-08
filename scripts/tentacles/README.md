# Local tentacle operations (pilot)

This companion observes existing Mora jobs. It is not a new multiwriter sync protocol.
The Go helper builds with CGO disabled; the macOS UI is a separate Swift app. Neither
changes the signed Mora.app or the core CLI. No AI calls, analytics or new credentials.

## Build and check

```sh
go test ./cmd/mora-tentacles
CGO_ENABLED=0 go build -o /tmp/mora-tentacles ./cmd/mora-tentacles
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/mora-tentacles-linux ./cmd/mora-tentacles
./apps/mora-menubar/test.sh
./apps/mora-menubar/build.sh /tmp/mora-tentacles
```

Install the Linux helper at `~/.local/bin/mora-tentacles` on an explicitly selected
peer. Put an owner-edited copy of `config.example.json` at
`~/.config/mora-tentacles/config.json`. SSH uses the owner's existing config/keys and
requires existing trusted host keys. Optional `host_name` and `host_key_alias` fields
select an existing route and pin. No discovered host is automatically enrolled.

`mora-tentacles --config <file>` checks local receipts and probes peers concurrently
with bounded SSH calls. `probe --profile devbox|windows` runs only read checks on a
peer. It does not sync, search, rebuild, fetch or change connector cursors. The Windows
profile deliberately distinguishes the managed archive from the remote-agent gateway.
A missing native client executable and the ordinary Windows checkout are outside that
profile's health claim. This pilot targets the inspected standard host paths only.

## Fix failure-masking wrappers

Back up an existing wrapper before replacing only its body with:

```sh
#!/bin/sh
exec "$HOME/.local/bin/mora-tentacles" sync --job mac-devbox
```

Jobs: `mac-backup` invokes the existing `mora-sync-git-locked.sh` unchanged; point only its LaunchAgent at the helper (never replace that script with a recursive wrapper). `mac-devbox` pulls `from-devbox-*` and pushes `devbox-*`; `mac-gateway` pushes
`gw-*`; `devbox` pulls `devbox-*` and pushes `from-devbox-*`. `windows-gateway` pulls
`gw-*` using the existing isolated gateway config and binary. The last job is available
but must not be installed over a Windows service another operator owns. Keep schedules
unchanged. Publication stays limited to previously configured scopes/recipients.

The wrapper validates `share list --json`, fails on missing expected directions, runs
each share operation, preserves failures, and writes an atomic durable JSON receipt in
`~/.local/state/mora/tentacles/`. A failed push never advances last_success. A running
receipt without completion becomes failed after 30 minutes. Job exclusion uses an existing kernel-held lock primitive; process exit releases it, including after SIGKILL or reboot. Do not delete lock files while jobs are running. Set MORA_BIN and MORA_TENTACLES_STATE_DIR only for isolated fixture tests.

Receipt success proves those CLI operations completed, not connector completeness,
MCP retrieval, or a new memory's end-to-end round trip. The UI shows this distinction.
Windows→Mac authored-memory sync remains a separate planned change; a one-way archive
must never be relabelled as bidirectional sync.

## Rollback

Quit Mora Status to stop polling; existing sync continues. Restore backed-up wrapper
files to revert scheduling behavior. Preserve receipts for diagnosis. Remove only this
pilot app/helper/config after stopping its readers. Do not delete vaults, proposals,
share indexes, keys, or the other operator's Windows repairs. The app is a locally built
pilot, not a notarized release; launch-at-login is intentionally not configured yet.

The new wrappers preserve the old wrappers' prefix selection policy (including future
matching shares explicitly registered by the owner). They do not create subscriptions,
recipients or shares. `--yes` retains existing unattended publication behavior.
The Mac vault backup cadence is 15 minutes despite its historical `git-daily` name;
a dedicated mac-backup receipt now replaces inference from that job's old log tail.
