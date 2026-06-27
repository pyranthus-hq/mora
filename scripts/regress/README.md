# Pre-release regression harness (Tier 1)

Proves, against a **real installed binary**, that a Mora release still works
before the tag goes out — install, every command, the MCP wire protocol, the
connectors, and an **upgrade from the previous release**. Pure cross-platform
checks; runs on the CI Linux runner and locally.

```sh
./build.sh native     # build a stamped binary on this host, run the harness natively
./build.sh docker     # run in a clean Debian container (the release shape)
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

## Out of scope (needs a real Mac / credentials)

iMessage `chat.db` decode, Apple Calendar, launchd/osascript, codesign/Gatekeeper
(and the cdhash→Full-Disk-Access and SIGKILL-137 upgrade hazards); and the
Gmail/Calendar OAuth ingest path (needs a dedicated test Google account).

## Inputs (env)

`MORA_REPO`, `MORA_BIN` (a stamped binary), `EXPECTED_VER` (required);
`PREV_VER` (default `0.7.0`), `SKIP_UPGRADE`, `SKIP_GIT`, `RELEASE`, `WORK`.

> Note: `--tmpfs /work` must be mounted `exec` (build.sh does this) — Docker's
> default tmpfs is `noexec`, which would block running the installed binary.
> JSON assertions use `python3` (already required by `build_vault.py`); no `jq`.
