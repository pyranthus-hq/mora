# Pre-release regression harness

Proves, against a **real installed binary**, that a Mora release still works
before the tag goes out — install, every command, the MCP wire protocol, the
connectors, and an **upgrade from the previous release**. Tier 1 is pure
cross-platform (runs on the CI Linux runner and locally); Tier 2 covers the
macOS-only surfaces a container can't reach.

```sh
./build.sh native     # build a stamped binary on this host, run Tier 1 natively
./build.sh docker     # run Tier 1 in a clean Debian container (the release shape)
./build.sh macos      # run Tier 2 (macOS-native) on this Mac
```

Exit code is the verdict. CI runs `regression.sh` directly against the goreleaser
artifact in the release workflow, before the publish step — so a build that fails
regression never becomes a published release.

## What Tier 1 checks

- **install.sh** places the binary, `mora version` is stamped (not `dev`) and
  matches, init repoints the vault. With `RELEASE=1`, also hard-fails if
  `install.sh`'s hardcoded `VERSION` ≠ the release version.
- **every smoke-testable command** exits cleanly on a synthetically seeded vault
  (`scripts/bench/agent-ab/build_vault.py` + `world.json`).
- **MCP wire**: `initialize` + `tools/list` + `search_memory` round-trip over
  newline-delimited JSON-RPC, plus the notification-no-reply guard.
- **data**: write→read→delete tombstone round-trip; `tasks add` actually persists.
- **connectors**: filesystem + PDF + DOCX extraction; `git sync` to a local bare
  remote with a no-PAT-leak assert.
- **health gate**: `mora doctor --json --strict` reports healthy.
- **upgrade**: installs the previous release, populates it, swaps to the new
  binary in place, and asserts the vault/index survive (count preserved, search
  auto-heals, brief + doctor OK). Needs network; `SKIP_UPGRADE=1` to skip.

## What Tier 2 (macOS-native) checks

`regression-macos.sh` exercises the surfaces a Linux container can't — all
sandboxed so they never touch the live binary, vault, or `~/Library/LaunchAgents`:

- **codesign / Gatekeeper**: install.sh ad-hoc-signs the binary and clears the
  quarantine xattr so a downloaded Mora runs.
- **launchd**: `mora schedule install` writes a correct plist (verified under a
  redirected `HOME`, so it stays inert — never loaded into the live session).
- **osascript**: `--notify` honours `MORA_NO_NOTIFY`.
- **iMessage / Apple Calendar**: real read-only `chat.db` decode (the `livedb`
  go test, FDA-gated) and the Calendar connector.
- **upgrade hazard**: the cp-over-running-binary SIGKILL-137 case and the
  atomic-rename mitigation `mora upgrade` relies on.

FDA-gated and live-data steps degrade to a loud SKIP when chat.db / Calendar /
Full Disk Access aren't available, so the run still passes on a bare Mac.

## Out of scope (needs credentials)

The Gmail/Calendar OAuth ingest path needs a dedicated test Google account — run
it manually (point `MORA_REGRESS_LIVE_BIN` at an authenticated binary, or mount a
token) rather than in CI.

## Inputs (env)

`MORA_REPO`, `MORA_BIN` (a stamped binary), `EXPECTED_VER` (required);
`PREV_VER` (default `0.7.0`), `SKIP_UPGRADE`, `SKIP_GIT`, `RELEASE`, `WORK`.

> Note: `--tmpfs /work` must be mounted `exec` (build.sh does this) — Docker's
> default tmpfs is `noexec`, which would block running the installed binary.
> JSON assertions use `python3` (already required by `build_vault.py`); no `jq`.
