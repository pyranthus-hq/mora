# Homebrew Cask release handoff

Mora's Homebrew Cask installs the signed, notarized, stapled `Mora.app` release
assets. `cmd/gencask` produces the Ruby from `checksums-app.txt`; the Homebrew
workflow checks the public release, verifies the manifest's tag-bound cosign
signature, downloads both app ZIPs, checks their bytes, and opens a PR in
`pyranthus-hq/homebrew-tap`. It never merges or publishes that PR.

## One-time setup

1. The public `pyranthus-hq/homebrew-tap` now distributes the signed-app Cask.
   Its legacy v0.4.0 raw archive Cask has been replaced.
2. Add `HOMEBREW_TAP_TOKEN` to the Mora repository's Actions secrets. Use a
   dedicated fine-grained token limited to `pyranthus-hq/homebrew-tap`, with
   Contents read/write and Pull requests read/write. It needs write access to that
   repository. The source repository's `GITHUB_TOKEN` only reads its
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

For local preparation, with `gh`, `jq`, `curl`, `cosign`, Go, and `shasum` installed:

```sh
bash scripts/prepare-homebrew-release.sh --tag v0.15.1 --out /tmp/mora.rb
```

This command reads the release's dedicated assets endpoint because the tag
response can omit its embedded asset list. Asset pages are normalized with
`jq -s` so preparation works on GitHub CLI builds that lack `gh api --slurp`
(added in gh 2.48) as well as newer runners. It only writes the requested
output file and does not change the tap. Run the secret-free regressions with
`bash scripts/regress/homebrew-release.sh` and
`bash scripts/regress/homebrew-release-contract.sh`.

## Installation and updates

Install from the public tap:

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
self-updated app; wait for the tap version to catch up. Prefer
`brew upgrade --cask pyranthus-hq/tap/mora` over `--greedy`: greedy must not
silently downgrade a newer self-updated app that the tap has not caught up to.

The third-party tap retains a narrowly scoped Ruby preflight because Homebrew's
literal `preflight_steps` cannot express the conflicting-user-app check. Its
style check requires an explicit `Cask/InstallSteps` exception for this guard.
`brew uninstall --cask pyranthus-hq/tap/mora` removes the installation without a
data-deleting `zap`. Manage any schedules separately before uninstalling.

## Signed-host canary (acceptance handoff)

Static preparation (generator, checksum/signature gating, regress scripts, and
docs) can run without Darwin. Live Brew audit/style/install/upgrade/uninstall,
codesign/stapler proof, CLI→updater route on a real Caskroom link, N→N+1 /
notification behavior, and FDA continuity with #167 require a consented signed
macOS host. Adit owns that host session; do not claim it from static evidence.

On a consented host with the published tap and matching signed release assets:

```sh
# Style + audit (record the Cask/InstallSteps exception; do not hide it)
brew tap pyranthus-hq/tap
brew style --cask pyranthus-hq/tap/mora
brew audit --cask --online pyranthus-hq/tap/mora

# Clean install, CLI symlink, updater route
brew uninstall --cask pyranthus-hq/tap/mora 2>/dev/null || true
brew install --cask pyranthus-hq/tap/mora
command -v mora
readlink "$(command -v mora)"   # expect .../Mora.app/Contents/MacOS/mora
mora version
mora upgrade --check            # expect whole-app route, not raw brew mutate
codesign -dv --verbose=4 /Applications/Mora.app 2>&1 | tee /tmp/mora-294-codesign.txt
stapler validate /Applications/Mora.app

# Fail-closed migration (do not use --adopt/--force)
# With ~/Applications/Mora.app present, install must odie with migration text.

# Uninstall preserves vault/config/state/tokens/backups (no zap)
printf 'probe\n' > "$HOME/Library/Application Support/mora/.294-canary"
brew uninstall --cask pyranthus-hq/tap/mora
test -f "$HOME/Library/Application Support/mora/.294-canary"

# N→N+1 / greedy downgrade / FDA continuity: share one session with #167.
# Record release tag, tap commit, and sanitized receipts. Do not claim
# automatic updater ownership from installation alone.
```

Keep `auto_updates` absent until this canary plus schedule/notification audit
prove the claim. Coordinate FDA evidence with #167; do not duplicate its
protected-source work here.
