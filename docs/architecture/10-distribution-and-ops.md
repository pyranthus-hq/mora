# Distribution, Build & Ops

This document explains how Mora is built, signed, released, updated, and
installed. The full pipeline keeps the pure-Go build and explicit network
boundary described in the [README](../../README.md#privacy-boundary).

## Files

| File | Responsibility |
|---|---|
| `.goreleaser.yaml` | Release pipeline config (v2 schema): CGO-free cross-compile matrix (darwin/linux/windows), macOS signing hook, archives, checksums, cosign keyless signing, SBOM, disabled-by-default Homebrew cask upload, and a draft GitHub Release. |
| `.github/workflows/ci.yml` | PR/push gate: macOS secret-free Mora.app release-contract sabotage, gofmt, `go vet`, `go test -race`, a Windows portability job, golangci-lint, five-target build matrix, advisory size diff, and gitleaks. |
| `.github/workflows/release.yml` | Tag-triggered (`v*`), macOS-hosted signed/notarized release workflow; it publishes only after the artifact and Tier-1 gates pass. |
| `cmd/genicns` + `internal/appbundle` | Deterministic stdlib-only `Mora.icns` generator from the committed pixel-art SVG. |
| `scripts/assemble-darwin-app.sh` | Deterministic `Mora.app` assembly from an already-signed CLI and generated icon; assembly only, no signing. |
| `scripts/appbundle-darwin-release.sh` | Checksum-verified assembly, whole-bundle Developer ID signing, notarization, stapling, Gatekeeper validation, packaging, and final ZIP re-verification. |
| `scripts/verify-app-zip.sh` | Fail-closed path and layout validation for app ZIP assets. |
| `scripts/regress/app-bundle-contract.sh` | Secret-free adversarial contract for app layout, identity, seal, staple, Gatekeeper, hostile ZIPs, and release ordering. |
| `install-app.sh` / `uninstall-app.sh` | Fail-closed macOS app install/migration and uninstall paths that preserve user data. |
| `internal/mora/upgrade.go` | Checksum-validated, Homebrew-aware self-update, including whole-app replacement on macOS. |
| `cmd/mora/main.go` | Thin entrypoint that forwards linker-supplied version metadata into `internal/mora`. |
| `install.sh` | POSIX installer; on macOS it verifies the fixed Developer ID identity and notarized-code requirement without changing quarantine or the signature. |
| `install.ps1` | Windows installer with checksum verification and User PATH setup. |
| `uninstall.ps1` | Windows uninstaller that preserves vault/config unless explicitly purged. |
| `scripts/build-release.sh` | Local GoReleaser-mirror cross-build of all five targets plus `checksums.txt`. |
| `scripts/package.sh` | Single-target packaging with guarded build-time OAuth-client embedding. |
| `scripts/codesign-darwin.sh` | Darwin signing hook with pinned identity, hardened runtime, timestamp, and post-sign verification. |
| `scripts/notarize-darwin-release.sh` | Pre-publish Apple notarization and quarantined-launch gate for raw Darwin binaries. |
| `scripts/regress/release-signing-contract.sh` | Secret-free adversarial signing/notarization contract test. |
| `scripts/bootstrap-release-project.sh` | One-shot GitHub Projects board scaffold; not part of the build. |
| `.golangci.yml` | golangci-lint v2 tuning for the blocking lint gate. |
| `go.mod` | Module graph; pins pure-Go SQLite and `go-selfupdate`, with `go 1.25.8`. |
| `LICENSE` | Apache-2.0. |

> **Scope note.** This document describes the main build under `./internal/`,
> `./cmd/`, and the repo-root config. The `dist/` directory is build output.
> Generated `dist/` output is outside the source-of-truth surface.

---

## The single thesis: pure-Go, CGO-off, one static binary

The build pipeline enforces one rule: **Mora is one static binary with no CGO
or Python. `modernc.org/sqlite` is its only SQL engine.** This rule makes
one-file installs possible. They need no toolchain or dynamic libraries. The
pipeline tests the rule on each build.

- `go.mod:12` requires `modernc.org/sqlite v1.29.0` (the pure-Go SQLite). There is no `mattn/go-sqlite3` or any cgo driver in the graph — `go.mod:78-83` shows the supporting `modernc.org/{libc,memory,mathutil,...}` tree, all pure Go.
- Every **build/release** step sets `CGO_ENABLED=0`: GoReleaser (`.goreleaser.yaml:19-20`), the CI build matrix (`ci.yml:79`), the size job (`ci.yml:99`), and `build-release.sh:31`.
- **The one exception is the test job**, and it is deliberate and narrow: `ci.yml:36-38` sets `CGO_ENABLED=1` *only* for `go test -race` (the race detector needs cgo). The comment at `ci.yml:34-35` is explicit that this is the lone place cgo is on, "every build/release step stays `CGO_ENABLED=0` (the product's thesis)."

### Cross-compile targets

The baseline release ships **darwin + linux × amd64 + arm64** (four binaries). Windows support adds a **windows/amd64** zip archive with `mora.exe`. Windows/arm64 remains deferred. Go compilation is still a pure cross-build, but the **release** uses a macOS runner for native `codesign`, `notarytool`, and quarantined-launch verification. This does not change the product: every target builds with `CGO_ENABLED=0`; the Apple tools operate on the completed Darwin executables.

The CI **build matrix** (`ci.yml:85-108`) compiles all five targets (darwin/linux × amd64/arm64 plus windows/amd64) with `CGO_ENABLED=0` on every PR. The comment (`ci.yml:58-59`) frames this precisely: it "proves CGO_ENABLED=0 builds on every target before a tag is ever cut. This is the single-binary guarantee, gated."

### Windows support seam

Windows support keeps the same pure-Go single-binary model. Platform behavior is selected at runtime with `runtime.GOOS`. The codebase does not split behavior into OS-specific build-tag files. The Windows release archive contract is `mora_<version>_windows_amd64.zip` containing `mora.exe` at archive root, plus the same release `checksums.txt` sha256 manifest consumed by self-update and installers.

The Windows hand-install path is `install.ps1`: it installs to `%LOCALAPPDATA%\Mora\bin\mora.exe` and writes only the User PATH. The v1 binary is unsigned, so the installer and docs must tell users about the SmartScreen **Windows protected your PC** prompt and `Unblock-File`. MSI/MSIX, winget/Scoop, Authenticode signing, Windows toast notifications, and windows/arm64 archives are deferred.

Windows scheduling is a CLI/runtime seam, not a daemon. `mora schedule install <job>` uses `schtasks` and names tasks `Mora\<job>`; `uninstall.ps1` deletes all tasks under `\Mora\`.

### Reproducibility & version stamping

Builds are reproducible: `-trimpath` strips local paths (`.goreleaser.yaml:22`, `ci.yml:82`) and `mod_timestamp: "{{ .CommitTimestamp }}"` (`.goreleaser.yaml:30`) pins file mtimes to the commit. Version metadata is injected via ldflags `-s -w -X main.version=... -X main.commit=... -X main.date=...` (`.goreleaser.yaml:23-27`, `build-release.sh:17`). `cmd/mora/main.go:12-21` declares the `version/commit/date` vars (defaulting to `dev`/`none`/`unknown`) and copies them into `mora.BuildVersion/BuildCommit/BuildDate`, which `cmdVersion` prints (`mora.go:247-253`) and the MCP `serverInfo.version` reports (`mora.go:2871`). **A source build therefore self-identifies as version `dev`** — and that fact gates self-update (below).

---

## Release pipeline (tag → signed artifacts → Apple notary → publish)

A maintainer cuts a release by pushing a `v*` tag. `release.yml:3-5` triggers on `tags: ["v*"]` and runs one `goreleaser` job.

```mermaid
flowchart TD
    tag["git tag vX.Y.Z<br/>git push --tags"] --> wf["release.yml<br/>(macOS runner)<br/>contents:write + id-token:write"]
    wf --> keychain["validate secrets<br/>create ephemeral keychain<br/>import Developer ID certificate"]
    keychain --> gr["goreleaser release --clean<br/>draft GitHub release"]
    gr --> build["CGO_ENABLED=0 cross-build<br/>darwin/linux/windows"]
    build --> sign["codesign Darwin binaries<br/>identifier com.pyranthus.mora<br/>hardened runtime + timestamp"]
    sign --> arch["frozen archive names<br/>binary at archive root"]
    arch --> sum["checksums.txt (sha256)<br/>+ keyless cosign signature"]
    arch --> notary["submit each signed Darwin binary<br/>notarytool --wait"]
    notary --> assess["status == Accepted<br/>codesign --strict + notarized requirement<br/>quarantined native launch"]
    sum --> assess
    assess --> regress["artifact verification<br/>Tier-1 regression"]
    regress --> publish["publish draft release"]
    publish --> consume["install.sh + mora upgrade"]
```

### Stage details

1. **Credentials and keychain.** The workflow fails before building if an Apple or OAuth secret is absent. It creates a throwaway build keychain, imports the Developer ID `.p12`, allows non-interactive `codesign`, and removes the keychain on the always-run cleanup path. The App Store Connect `.p8` exists only in the ephemeral runner workspace. No credential enters an artifact or the repository.
2. **Build and sign.** One build id `mora` compiles the five-target matrix from `./cmd/mora` with `CGO_ENABLED=0`. Only the two Darwin outputs then receive a hardened-runtime, secure-timestamp Developer ID signature. Their fixed signing contract is identifier `com.pyranthus.mora`, Team Identifier `VS8M5VJBZ5`, and an Apple-trusted Developer ID Application chain.
3. **Archives.** Darwin/Linux use `tar.gz`; Windows/amd64 uses `zip`. The executable stays at archive root so `go-selfupdate` can find it. The frozen name is `mora_{{.Version}}_{{.Os}}_{{.Arch}}`. Archives also carry `LICENSE`, `README.md`, `docs/guide.md`, `install.sh`, and `examples/*`.
4. **Checksums, cosign, and SBOM.** One SHA-256 `checksums.txt` remains the self-update trust anchor. Keyless cosign signs that manifest and Syft emits the archive SBOMs. Apple code signing is an additional platform trust layer, not a replacement for the cross-platform checksum contract.
5. **Notarize and verify before publish.** Each archive's already-signed Darwin executable is submitted to Apple with `notarytool --wait`. The gate requires `Accepted`, reruns `codesign --verify --strict`, confirms the identifier/team/designated requirement, and requires Apple's special `notarized` code requirement for both architectures. Before that requirement, a best-effort `spctl --type execute` probe makes Gatekeeper fetch the online ticket. Its expected raw-CLI "not app-like" result is ignored; it is ticket hydration, not the verdict. The gate also adds quarantine to a disposable copy of the runner-native binary and launches `mora version`. `spctl --type install` is never used because that policy is for installer packages. The release archive is never modified after checksums are generated.
6. **Mora.app lane.** After the raw artifacts pass their notary gate, `appbundle-darwin-release.sh` builds both branded app bundles from the same checksum-verified signed CLIs, signs each **whole bundle**, notarizes, staples, validates (stapler + codesign + Gatekeeper), and packages post-staple `_app.zip` assets with `checksums-app.txt`. The workflow then cosign-signs that manifest (keyless, like `checksums.txt`) and uploads everything to the still-draft release. The lane runs before credential cleanup (it needs the ephemeral keychain) and before publish. See [Standalone bridge, then `Mora.app`](#standalone-bridge-then-moraapp).
7. **Draft, then publish.** GoReleaser creates a draft release. OAuth embedding, Windows archive, checksums, macOS signing/notarization, the Mora.app lane, and Tier-1 regression gates all run while it is invisible to `mora upgrade`. Only their success publishes the draft. A signing or notary failure therefore cannot become the latest installable release.

## `mora upgrade` — self-update flow

`mora upgrade` is the in-place self-update path from the latest GitHub release.

```mermaid
flowchart TD
    start["mora upgrade [--check]"] --> dev{"BuildVersion == 'dev' or ''?"}
    dev -- yes --> refuse["refuse:<br/>'this is a source build…<br/>use git pull && go build'"]
    dev -- no --> exe["os.Executable()<br/>+ EvalSymlinks → real path"]
    exe --> brew{"path contains<br/>/Cellar/ or /Caskroom/ ?"}
    brew -- yes --> brewmsg["print 'brew upgrade<br/>pyranthus-hq/tap/mora'<br/>and exit (no self-update)"]
    brew -- no --> token["token = first non-empty of<br/>MORA_GITHUB_TOKEN / GITHUB_TOKEN / GH_TOKEN"]
    token --> detect["DetectLatest(pyranthus-hq/mora)"]
    detect -- error --> hint["error + hint:<br/>'repo is private — set GITHUB_TOKEN'"]
    detect -- not found --> none["'no published releases found'"]
    detect -- found --> cmp{"latest <= current?"}
    cmp -- yes --> uptodate["'mora is up to date'"]
    cmp -- no --> avail["print 'update available: X → Y'"]
    avail --> checkonly{"--check?"}
    checkonly -- yes --> stop["'run mora upgrade to install it'"]
    checkonly -- no --> dl["download asset<br/>(name matched by go-selfupdate)"]
    dl --> validate["ChecksumValidator<br/>verify vs checksums.txt"]
    validate --> swap["UpdateTo: atomic same-path swap"]
    swap --> done["'✓ updated mora to Y'"]
    done --> reindex["postUpgradeRebuild:<br/>exec NEW binary `index rebuild`<br/>(warn-don't-fail)"]
```

Key behaviors, each grounded:

- **Source builds are refused** (`upgrade.go:32-35`). If `BuildVersion` is `"dev"` or empty, upgrade errors out and tells the user to `git pull && go build`. This is why the ldflags version-stamp (above) is a hard dependency of self-update.
- **Homebrew installs defer to brew** (`upgrade.go:45-49`). After resolving symlinks (`upgrade.go:41-43`), `isHomebrewManaged` (`upgrade.go:107-110`) checks whether the *resolved* path contains `/Cellar/` or `/Caskroom/`. The symlink-resolve-first step matters: a binary that merely *sits in* `/opt/homebrew/bin` via `install.sh` is a real file there, not a Homebrew symlink, so it is **not** flagged (`upgrade.go:102-106`) and self-update proceeds normally.
- **Token discovery** (`upgrade.go:52`). The repo is public, so no token is required. When one of `MORA_GITHUB_TOKEN`, `GITHUB_TOKEN`, `GH_TOKEN` is set it is used (rate limits, forks of the private lineage). On detection failure with no token the error still hints `export GITHUB_TOKEN=$(gh auth token)`.
- **Post-upgrade reindex** (`postUpgradeRebuild`). After `UpdateTo` succeeds, upgrade execs the swapped-in binary as `<exe> index rebuild` — the running process is still the old code; `indexSchemaVersion` knowledge lives in the new executable. Failure warns and prints the manual command. It never fails the upgrade (the swap already happened). Belt-and-braces with `indexAutoHeal`: static-floor vaults would self-heal at first read anyway. This hook is what spares semantic-embedder vaults the actionable error.
- **Checksum validation before swap** (`upgrade.go:60-63`). The updater is built with `&selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"}` — the downloaded archive is verified against the release's published `checksums.txt` (the same file GoReleaser/`build-release.sh` emit) before the binary is swapped. The comment is explicit: "don't trust TLS + the GitHub API alone."
- **Atomic, failure-safe swap** (`upgrade.go:94-96`). `UpdateTo` replaces the running binary in place. On error the message guarantees "binary left unchanged."
- **Stable macOS signing identity.** Every official Darwin replacement keeps identifier `com.pyranthus.mora`, Team Identifier `VS8M5VJBZ5`, and the Developer ID designated requirement. `mora upgrade` must copy the release bytes unchanged: any local `codesign --sign -` would replace that identity and can invalidate Full Disk Access. The release gate, not the updater, performs Apple trust assessment before publication.
- **`--check` is read-only** (`upgrade.go:88-91`): reports availability and stops.

The asset go-selfupdate fetches must match the `name_template` GoReleaser produces — that coupling is the single most fragile seam in this subsystem (see Invariants).

---

## `install.sh` — the hand-install path

`install.sh` is the non-Homebrew installer. It runs two ways:

1. **Local**: if an executable `mora` sits next to the script, use it — the bundled-tarball case (`tar -xzf … && ./install.sh`).
2. **Remote**: otherwise detect OS/arch (normalizing `aarch64→arm64`, `x86_64→amd64`), construct `mora_${VERSION}_${OS}_${ARCH}.tar.gz` — the same frozen name the release produces — and download it. It prefers `gh` when authenticated and falls back to public `curl`.

Then it:
- **Verifies the downloaded archive before extraction.** The archive's SHA-256 must match `checksums.txt`.
- **Fails closed on macOS identity.** Before copying, `codesign --verify --strict` must pass; the signature metadata must be exactly `Identifier=com.pyranthus.mora` and `TeamIdentifier=VS8M5VJBZ5` with a Developer ID Application authority; and `codesign -R='notarized'` must accept Apple's ticket for that exact code directory. The same checks run after the copy. A best-effort `spctl --type execute` probe first fetches that ticket; its raw-CLI "not app-like" result is not treated as success or failure. `spctl --type install` is for installer packages and is not used. A raw executable cannot carry a stapled ticket, so the first ticket check can require network access.
- **Never mutates Gatekeeper state or the signature.** There is no `xattr -d` and no ad-hoc re-sign. Quarantine remains for Gatekeeper to evaluate, and the Developer ID identity remains the one TCC sees. This macOS verification is a no-op on Linux; Linux install behavior is otherwise unchanged.
- **Replaces the active install.** It honors an explicit `PREFIX`. Otherwise, if `command -v mora` resolves to a regular non-Homebrew file in a writable directory, it replaces that exact pathname. It refuses an active symlink or a path under `Cellar`/`Caskroom` so it cannot silently write through a package-manager or future app-bundle boundary. Only a first install uses the first writable of `/usr/local/bin`, `/opt/homebrew/bin`, and `~/.local/bin`, falling back to `~/.local/bin`.
- **Runs `mora init` idempotently** against `MORA_VAULT` (default `~/vault/mora`), reports the active vault, then prints the agent-wiring commands and next steps.

### Standalone bridge, then `Mora.app`

The signed raw executable is deliberately a **compatibility bridge**. Its archive
name and root-level `mora` entry stay unchanged, so every existing installer and
`go-selfupdate` client can consume the first signed release. Apple stores the
notarization ticket server-side for a raw executable; unlike an app, it has no
bundle onto which `stapler` can attach that ticket.

The branded `Mora.app` is a distinct distribution target with bundle ID
`com.pyranthus.mora`. **As built (issue #257 Lane A), every tag release also
produces two app assets** with this frozen contract:

- **Asset names:** `mora_<version>_darwin_<arch>_app.zip` for stable numeric
  `X.Y.Z` releases on `amd64` and
  `arm64`, plus `checksums-app.txt` (sha256, same format as `checksums.txt`)
  with a keyless cosign signature/certificate
  (`checksums-app.txt.cosign.sig`/`.cosign.pem`, mirroring `checksums.txt`).
  The legacy standalone updater (go-selfupdate, pinned v1.5.2) lowercases
  asset names and selects one that **ends with** `<os><sep><arch><ext>`;
  `_app.zip` is not such a suffix, so an old `mora upgrade` can never download
  an app bundle as a raw binary. `appbundle-darwin-release.sh` enforces this
  at packaging time (`--check-asset-name`), and the contract harness
  sabotages the guard with every legacy suffix combination.
- **Bundle layout:** `Contents/MacOS/mora` (the same signed pure-Go CLI the
  raw archive ships, byte-verified against `checksums.txt` before assembly),
  `Contents/Resources/Mora.icns` (generated deterministically from
  `docs/assets/mora-eye.svg` by `cmd/genicns` — same pixel art, rounded-square
  tile, crisp nearest-neighbor edges), and a frozen `Info.plist`:
  `CFBundleIdentifier` `com.pyranthus.mora`, `CFBundleName`/`DisplayName`
  `Mora`, `CFBundleExecutable` `mora`, `CFBundleIconFile` `Mora`,
  `CFBundleShortVersionString`/`CFBundleVersion` from the release tag.
- **Whole-bundle trust:** the bundle is signed as a unit with the pinned
  Developer ID identity (identifier `com.pyranthus.mora`, team `VS8M5VJBZ5`,
  hardened runtime, secure timestamp), notarized, **stapled**
  (`Contents/CodeResources` must exist — a silent no-op staple fails the
  release), `stapler validate`d, and Gatekeeper-assessed. Unlike the raw CLI,
  `spctl --assess --type execute` on an app is the real verdict and gates the
  release. The published zip is produced **after** stapling so installs
  validate offline. Both final ZIPs are then re-extracted and must retain the
  sealed signature, stapled ticket, exact identity, layout, architecture,
  notarized-code requirement, and Gatekeeper acceptance before checksumming;
  `scripts/verify-app-zip.sh` also refuses unsafe archive paths before upload.
- **The raw update contract is preserved:** the archive names, root-level
  binary, and `checksums.txt` trust anchor are unchanged. Adding the app
  migration scripts intentionally changes each archive's bytes and therefore
  its checksum, but not legacy selection, extraction, or verification.

`install-app.sh` puts the whole app at `~/Applications/Mora.app` by default and
exposes its CLI through a symlink. It preserves a prior standalone executable
as `.standalone-backup`, refuses unrelated/Homebrew symlinks, and prints the
planned FDA migration with the unproven-continuity warning. It verifies the checksum, canonical ZIP inventory,
exact plist and Apple identity, both signatures, staple, notarized-code
requirement, Gatekeeper verdict, architecture, and executable version before
the atomic first-install rename. Post-rename verification runs in a rollback-
capable subshell, so its fail-closed diagnostics cannot bypass removal of an
incomplete first install. It never re-signs or removes quarantine.

When the resolved running executable is
`Mora.app/Contents/MacOS/mora`, `mora upgrade` selects the exact stable release
asset `mora_<version>_darwin_<arch>_app.zip` and its unique
`checksums-app.txt`. It bounds and checksum-verifies both downloads, rejects
hostile ZIP paths/types/duplicates before `ditto` extraction, and applies the
same pinned bundle verification. The staged bundle is below the installed
app's parent on the same volume. Darwin `renameatx_np(RENAME_SWAP)` exchanges
the two directories atomically; a failed post-swap verification performs the
same swap to restore the old bundle. If that rollback fails, cleanup is disabled
and the error reports the exact previous-app path inside the preserved private
staging directory. Only after success does the new binary rebuild the index and
the old bundle leave the private staging directory.
Replacing only `Contents/MacOS/mora` is forbidden because it invalidates the
bundle seal. A standalone install still follows the original raw updater.

`uninstall-app.sh` is similarly fail-closed before destructive work. The target
must be a real `Mora.app` at the configured app parent with bundle/signing
identifier `com.pyranthus.mora`, Team ID `VS8M5VJBZ5`, executable `mora`, and a
valid whole-bundle signature. It first proves every matching PATH directory is
writable, atomically moves the app inside a same-parent private directory, then
removes only symlinks whose exact target is the app executable. A PATH cleanup
failure restores both the app and every link already removed. Vault, config,
state, unrelated PATH entries, and `.standalone-backup` files remain untouched.

The permission transition is explicit:

- An FDA grant made to an old ad-hoc executable cannot be assumed to transfer to
  the first Developer ID-signed bridge. The user can need to grant the signed
  executable once.
- Moving from the raw executable to `Mora.app` changes the TCC target. Plan for
  an app grant and retain the old entry until app-launched `mora doctor` plus an
  iMessage sync succeed. It is not yet proven to be the last grant.
- Only a real version N to N+1 whole-app replacement that reads iMessage without
  a re-grant can close the continuity claim. The intended steady state is that
  routine app upgrades preserve the grant; the release pipeline alone is not
  evidence that macOS did so.

### `scripts/package.sh` — the OAuth-embed footgun

`package.sh` packages a single target and, critically, **embeds the real Google OAuth client at build time** when `MORA_GOOGLE_CREDENTIALS` is set. `internal/google/client.json` is a committed **non-secret** `DEV_PLACEHOLDER` (the embed target at `oauth.go:28`, detected at `oauth.go:77`). The script copies the real client over the placeholder, builds, then **always restores the placeholder via a `trap … EXIT INT TERM`** (`package.sh:25-32`) so real creds are never left in the tree or committed. The trap uses an **absolute** `$EMBED` path on purpose — the script `cd`s into `$DIST` before the trap fires, and a relative path would restore from the wrong directory (`package.sh:21-24`). When given real creds it then **asserts the built binary actually embeds the real client id** (`package.sh:40-46`), because the `DEV_PLACEHOLDER` string is itself a detection constant compiled into every binary and so can't be used as a negative test. `build-release.sh` does **not** do this embed step — it always ships the placeholder.

---

## CI gates (`.github/workflows/ci.yml`)

CI runs on every PR and every push to `main`, with `contents: read` only and
per-ref concurrency cancellation. Seven job definitions expand to eleven job
instances because the cross-build matrix covers five targets.

```mermaid
flowchart LR
    pr["PR / push to main"] --> test["test<br/>gofmt -l, go vet,<br/>go test -race (CGO=1)"]
    pr --> app["app-bundle-contract<br/>secret-free macOS release checks"]
    pr --> windows["build-windows<br/>full Windows test suite"]
    pr --> lint["lint<br/>golangci-lint v2.12.2"]
    pr --> build["build matrix<br/>5× CGO=0 cross-build"]
    pr --> size["size (PR only)<br/>size-diff vs main<br/>(advisory)"]
    pr --> secrets["secrets<br/>gitleaks detect<br/>(blocking)"]
```

| Job | Blocking? | What it enforces |
|---|---|---|
| `app-bundle-contract` | **yes** | Runs the secret-free adversarial Mora.app packaging and workflow-order contract on macOS. |
| `test` | **yes** | Requires clean gofmt, `go vet ./...`, and the full race-enabled test suite. |
| `build-windows` | **yes** | Runs the full Go test suite natively on `windows-latest`. |
| `lint` | **yes** | Runs golangci-lint v2.12.2 through `golangci-lint-action@v8`. |
| `build` | **yes** | Cross-builds all five release targets with `CGO_ENABLED=0`; `fail-fast: false` lets every target report. |
| `size` | **no (advisory)** | Reports the PR binary-size delta from `main` with `continue-on-error: true`. |
| `secrets` | **yes** | Runs the OSS gitleaks CLI over full history with a blocking exit code. |

`.golangci.yml` is v2 format (`.golangci.yml:3`), uses the `standard` linter set plus the `std-error-handling` exclusion preset, and adds narrow rule exclusions (capitalized error strings for proper nouns. The fire-and-forget OAuth loopback server's `Serve/Shutdown`. Deprecated `ParseDir` in tests) — `.golangci.yml:13-32`.

---

## License — Apache-2.0

The repository ships under **Apache-2.0**, and the GoReleaser cask metadata
declares the same SPDX identifier. Cask upload is currently disabled with
`skip_upload: true`; the release workflow does not publish a Homebrew cask.

---

## Security posture as a product invariant

Mora keeps the corpus local by default and makes its bounded network surfaces
explicit. This matches the canonical [README privacy boundary](../../README.md#privacy-boundary):

- **Read-only Google scopes, least-privilege.** `internal/google/oauth.go:31-32` hardcodes `Scopes = {gmail.GmailReadonlyScope, calendar.CalendarReadonlyScope}` — there is no write scope anywhere, and Drive is deferred. (`oauth_test.go` exercises `ResolveOAuthConfig`'s scope round-trip — `oauth_test.go:29-30` checks the *passed* scopes survive resolution — but does not itself assert the read-only `Scopes` global. The global at `oauth.go:32` is the ground truth.)
- **No telemetry / opt-out usage logging.** Usage logging is local-only JSONL in the state dir (never the vault). `usageEnabled` (`usage.go`) honors `DO_NOT_TRACK=1` **and** a `<StateDir>/usage/OFF` sentinel (written by `mora usage off` — `cmdUsage`, `usage.go`). MCP events measure the serialized final `CallToolResult`; `read_memory` adds only allowlisted structural fields and never ids, bodies, matches, evidence, metadata, or paths. The query field remains an explicit raw-search-query tier only and is never sent (`usageEvent`, `mora.go`; exact schema in [MCP server](./06-mcp-server.md#usage-logging)).
- **Loopback-only embeddings guard.** The explicit Ollama mode refuses a non-loopback or unavailable endpoint rather than sending memory off-machine. Read paths surface the degraded state and can use FTS; rebuild/write paths fail closed. The default static embedder performs no network call.
- **Explicit connector and operator egress.** Google and GitHub Issues connectors make read-only API requests. `mora upgrade` reads GitHub releases. User-invoked `mora sync git` can push the plaintext vault to a remote the user controls. `mora share` sends age-encrypted authored memories over an access-controlled git remote or S3/R2; confidentiality comes from age encryption, not an assumed bucket setting.
- **Downstream-agent boundary.** Mora itself makes no model call. An MCP client can send retrieved text to its own model provider; the agent and its group policy control that action.
- **Supply-chain integrity at rest and on update.** Releases are sha256-checksummed and the checksum manifest is cosign-signed (Sigstore keyless); `mora upgrade` re-verifies the downloaded archive against `checksums.txt` before swapping (`upgrade.go:60-63`). Darwin binaries additionally carry the fixed Developer ID identity and an Apple notary acceptance ticket, both gated before publication. gitleaks blocks any committed secret (`ci.yml:107-122`).

The corpus therefore remains local by default, while backup, sharing, connector,
update, and downstream-agent transfers stay explicit rather than being hidden
behind a blanket "zero egress" claim.

---

## Invariants & gotchas

1. **`CGO_ENABLED=0` on every build/release path. Cgo is allowed ONLY in `go test -race`.** WHY: the single static binary is the product. If any build step turns cgo on, the artifact gains dynamic-lib dependencies and the "drop one file" story breaks. The race-test exception (`ci.yml:34-38`) is the lone, documented carve-out.

2. **`modernc.org/sqlite` is the only SQL engine — never add a cgo SQLite driver.** WHY: a cgo driver (e.g. `mattn/go-sqlite3`) would silently re-introduce cgo and defeat invariant #1. Verify after any dep change that `go.mod` still has no cgo SQL driver.

3. **The archive `name_template` is a contract shared by installers and self-update.** POSIX assets use `mora_{Version}_{Os}_{Arch}.tar.gz`; Windows uses `mora_{Version}_windows_amd64.zip`. GoReleaser produces the archives, `install.sh` / `install.ps1` reconstruct the names, and `go-selfupdate` matches them. WHY: change the template in one place and self-update or install scripts silently stop finding assets. Treat the templates as frozen.

4. **The binary must live at the archive root (no nested directory).** Both `build-release.sh:38` and GoReleaser's default do this. WHY: go-selfupdate and the Homebrew cask resolve `mora` at the archive root. A nested path breaks both.

5. **The ldflags version stamp gates self-update.** A build without `-X main.version` reports `dev`, and `mora upgrade` refuses dev builds (`upgrade.go:32-35`). WHY: self-update on an unversioned binary has no baseline to compare against and would loop/misfire. Releases MUST carry the stamp.

6. **Resolve symlinks before the Homebrew check.** `isHomebrewManaged` matches `/Cellar/` or `/Caskroom/` on the **resolved** path (`upgrade.go:41-43,107-110`). WHY: skipping the symlink-resolve would misclassify a real file in `/opt/homebrew/bin` (installed via `install.sh`) as Homebrew-managed and wrongly refuse self-update — or vice versa.

7. **Checksum-validate every self-update before swapping.** `ChecksumValidator` against `checksums.txt` (`upgrade.go:60-63`). WHY: TLS + the GitHub API alone is not a sufficient integrity guarantee. The swap replaces a binary the user will execute. Never drop the validator.

8. **`package.sh` must always restore the placeholder `client.json`.** The `trap … EXIT INT TERM` with an **absolute** path (`package.sh:25-32`) is mandatory. WHY: a real OAuth client left in the working tree is a secret leak and would be committed. The absolute path is required because the script `cd`s into `$DIST` before the trap runs.

9. **`//go:embed client.json` requires the file to exist.** `internal/google/client.json` is a committed non-secret `DEV_PLACEHOLDER` (`oauth.go:28,77`). WHY: deleting it breaks `go build` entirely (embed fails at compile time), and committing a real client there leaks credentials. Swap at release time only (via `package.sh` / `MORA_GOOGLE_CREDENTIALS`), never in git.

10. **Read-only Google scopes are a hard product invariant.** `Scopes` is `{gmail.readonly, calendar.readonly}` (`oauth.go:32`), pinned by test. WHY: Mora's trust model is "it can read, it can never write or delete your data." Adding a write scope is a posture change, not a feature.

11. **Local by default, with explicit egress boundaries.** Read-only connectors, releases, loopback Ollama, user-invoked backup/share transports, and a downstream agent are the declared boundaries. WHY: users must be able to distinguish local corpus storage from actions that intentionally cross the machine boundary.

12. **The size job is advisory. The secrets and lint jobs are blocking.** `size` is `continue-on-error: true` (`ci.yml:103`). Gitleaks is `--exit-code 1` (`ci.yml:121`). WHY: a size regression should inform, not block. A leaked secret or lint regression must block.

13. **Every Darwin release keeps one designated requirement.** The identifier is `com.pyranthus.mora`, the Team Identifier is `VS8M5VJBZ5`, and the authority is an Apple-trusted Developer ID Application certificate. Hardened runtime and a secure timestamp are required. WHY: macOS TCC continuity depends on stable signed identity; a valid signature from the wrong team is still the wrong product identity.

14. **Never clear quarantine or re-sign an official macOS release during install or upgrade.** `install.sh` verifies before and after copying. Homebrew and future installers must preserve the same behavior. WHY: quarantine is the Gatekeeper trigger, and ad-hoc re-signing replaces the designated requirement that Full Disk Access was granted to.

15. **The raw bridge and app bundle are different update contracts.** The existing archive keeps `mora` at its root. The app asset name `mora_<version>_darwin_<arch>_app.zip` does not end in a `<os><sep><arch><ext>` suffix, so the legacy `go-selfupdate` matcher can never select it (locked by the packaging-time name guard and the contract harness's suffix sabotage), and app updates replace the whole signed bundle. WHY: an old `go-selfupdate` client otherwise selects the app archive and extracts the wrong executable, while an inner-binary-only swap invalidates the app seal.

16. **The app bundle is signed, notarized, stapled, and zipped — in that order.** `appbundle-darwin-release.sh` signs the whole bundle (never only `Contents/MacOS/mora`), staples after Apple accepts, verifies the ticket file and Gatekeeper's verdict, and only then produces the published `_app.zip`. WHY: a zip cut before stapling ships an app that cannot validate offline, and an inner-binary-only signature leaves the resource seal unverified. The contract test asserts the ordering from the mock call log.

---

## Related

- [CLI & UX](./08-cli-and-ux.md) — `mora version`, `mora doctor`, `mora init`, command dispatch (`Run`/`cmdVersion`).
- [Connectors — Google](./04-connectors-google.md) — OAuth, embedded `client.json`, the read-only scopes referenced here.
- [Eval & testing](./09-eval-and-testing.md) — the `go test -race` gate and the size/budget regression tests.
- [Sync & freshness](./11-sync-and-freshness.md) — honest-snapshot sync, the other place the binary touches the network.
- [Overview](./00-overview.md) — how the binary fits the whole system.
