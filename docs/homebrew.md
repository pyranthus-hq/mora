# Homebrew Cask release handoff

Mora's Homebrew Cask installs the signed, notarized, stapled `Mora.app` release
assets. `cmd/gencask` produces the Ruby from `checksums-app.txt`; the Homebrew
workflow checks the public release, verifies the manifest's tag-bound cosign
signature, downloads both app ZIPs, checks their bytes, and opens a PR in
`pyranthus-hq/homebrew-tap`. It never merges or publishes that PR.

## One-time setup

1. Keep `pyranthus-hq/homebrew-tap` private until its replacement Cask has been
   reviewed. Its current v0.4.0 Cask installs a raw archive and strips
   quarantine; do not use it as an installation path.
2. Add `HOMEBREW_TAP_TOKEN` to the Mora repository's Actions secrets. Use a
   dedicated fine-grained token limited to `pyranthus-hq/homebrew-tap`, with
   Contents read/write and Pull requests read/write. It needs access to that
   private repository. The source repository's `GITHUB_TOKEN` only reads its
   published release.
3. Set the Mora repository Actions variable `MORA_HOMEBREW_ENABLED` to `true`
   when the tap PR lane is ready. While unset, the workflow is inert.

## Release and review

After a successful tag-triggered `Release` workflow completes, `homebrew.yml`
uses its tag only when the run's commit matches that tag. A maintainer can use
`workflow_dispatch` with a canonical tag to retry a missed handoff. Both paths
require a published, non-draft, non-prerelease GitHub release and the exact two
post-staple app ZIPs, checksum manifest, signature, and certificate. The
preparation script binds the certificate identity to the release workflow at
that tag and checks the SHA-256 of each downloaded ZIP against the manifest.

The tap job rejects any version older than the existing Cask, then opens or
updates `codex/mora-vVERSION` as a review PR. Review the app ZIP URLs, hashes,
and installation behavior before manually merging. The generator intentionally
omits `auto_updates` until Mora's updater schedule and notification audit is
complete. It does not strip quarantine or mutate an existing source install.

For local preparation, with `gh`, `jq`, `cosign`, Go, and `shasum` installed:

```sh
bash scripts/prepare-homebrew-release.sh --tag v0.15.0 --out /tmp/mora.rb
```

This command only writes the requested output file. It does not change the tap.
Run the secret-free regression with `bash scripts/regress/homebrew-release.sh`.

## Installation and updates

After the replacement Cask is merged, users with access to the private tap can run:

```sh
brew tap pyranthus-hq/tap
brew install --cask pyranthus-hq/tap/mora
mora version
```

This installs the signed memory CLI app and its command, not the desktop companion.
For Brew-managed updates, run `brew update` and
`brew upgrade --cask pyranthus-hq/tap/mora`. For Mora's own scheduled updater,
run `mora upgrade --policy auto`, followed by
`mora schedule install update-daily`. Inspect `mora schedule list` and
`mora upgrade --status` to confirm the local setup. Installation alone does not
create that schedule. Both update paths only deliver published releases.

If a signed app or old Brew package already exists, remove that installation
using its documented uninstaller first, preserving the vault and configuration.
Do not use `--adopt`, `--force`, or clear quarantine. A conflicting app in
`~/Applications` is refused. Avoid reinstalling an older Cask over a newer
self-updated app; wait for the tap version to catch up.

The third-party tap retains a narrowly scoped Ruby preflight because Homebrew's
literal `preflight_steps` cannot express the conflicting-user-app check. Its
style check requires an explicit `Cask/InstallSteps` exception for this guard. `brew uninstall --cask
pyranthus-hq/tap/mora` removes the installation without a data-deleting `zap`.
Manage any schedules separately before uninstalling.
