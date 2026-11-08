#!/usr/bin/env bash
# #167 — Protected-source FDA continuity canary (static pin + host handoff).
#
# Static/synthetic preparation only by default. This script NEVER claims a green
# live FDA canary. Live N→N+1 signed-app acceptance stays an Adit/host handoff
# and shares one consented Darwin session with #294 (Homebrew ownership).
#
# Modes:
#   PIN (default)  Pin N and N+1 public release app ZIP hashes + commits.
#                  Works on Linux/CI with network. Exit 0 = pin printed;
#                  live_canary remains HAND_OFF.
#   HOST           Darwin-only identity snapshot of an installed Mora.app plus
#                  the exact interactive/unattended evidence commands. Still
#                  HAND_OFF unless a human records real protected-source
#                  successes (this script will not invent them).
#   LIVE=1         With HOST: also attempt interactive syncs and tee evidence.
#                  Exit code reflects command failures only; SUCCESS is never
#                  inferred from process exit alone — check LastSuccessAt.
#
# Env:
#   N_TAG / N1_TAG   release tags (default v0.15.0 / v0.15.1 — #477 in N+1)
#   ARCH             amd64|arm64 (default: host uname -m mapped, else arm64)
#   APP              path to Mora.app (default /Applications/Mora.app then
#                    ~/Applications/Mora.app)
#   EVIDENCE_DIR     where to write RAW, PRIVATE receipts (manual redaction required) (default /tmp/mora-167)
#   REPO             GitHub owner/repo (default pyranthus-hq/mora)
set -euo pipefail
umask 077

REPO="${REPO:-pyranthus-hq/mora}"
N_TAG="${N_TAG:-v0.15.0}"
N1_TAG="${N1_TAG:-v0.15.1}"
MODE="${1:-PIN}"
LIVE="${LIVE:-0}"
EVIDENCE_DIR="${EVIDENCE_DIR:-/tmp/mora-167}"

die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
note() { printf 'note: %s\n' "$*"; }
hand_off() {
  printf '\n=== live_canary: HAND_OFF (shared_host_session: #294) ===\n'
  printf 'Do not close #167 on this script alone. Record real signed-host\n'
  printf 'iMessage + Apple Calendar interactive and unattended successes,\n'
  printf 'unchanged designated requirement, and fail-closed bad-signer evidence.\n'
}

map_arch() {
  if [ -n "${ARCH:-}" ]; then
    printf '%s\n' "$ARCH"
    return
  fi
  case "$(uname -m 2>/dev/null || true)" in
    arm64|aarch64) printf 'arm64\n' ;;
    x86_64|amd64) printf 'amd64\n' ;;
    *) printf 'arm64\n' ;;
  esac
}

resolve_commit() {
  local tag="$1" typ sha
  if ! command -v gh >/dev/null 2>&1; then
    printf 'UNKNOWN\n'
    return
  fi
  typ="$(gh api "repos/${REPO}/git/ref/tags/${tag}" --jq '.object.type' 2>/dev/null || true)"
  sha="$(gh api "repos/${REPO}/git/ref/tags/${tag}" --jq '.object.sha' 2>/dev/null || true)"
  if [ "$typ" = "tag" ] && [ -n "$sha" ]; then
    gh api "repos/${REPO}/git/tags/${sha}" --jq '.object.sha' 2>/dev/null || printf 'UNKNOWN\n'
    return
  fi
  if [ -n "$sha" ]; then
    printf '%s\n' "$sha"
    return
  fi
  printf 'UNKNOWN\n'
}

fetch_checksums_app() {
  local tag="$1" out="$2"
  local url="https://github.com/${REPO}/releases/download/${tag}/checksums-app.txt"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$out" || die "could not download checksums-app.txt for ${tag}"
  else
    die "curl required to pin public release hashes"
  fi
}

hash_for_arch() {
  local file="$1" version="$2" arch="$3"
  local name="mora_${version}_darwin_${arch}_app.zip"
  local line
  line="$(grep -E "  ${name}\$" "$file" || true)"
  [ -n "$line" ] || die "no ${name} entry in $(basename "$file")"
  local hash="${line%% *}"
  [[ "$hash" =~ ^[0-9a-fA-F]{64}$ ]] || die "invalid SHA-256 for ${name}"
  printf '%s\n' "$hash"
}

pin_pair() {
  local arch work n_ver n1_ver n_commit n1_commit n_amd64 n_arm64 n1_amd64 n1_arm64
  arch="$(map_arch)"
  work="$(mktemp -d)"
  trap 'rm -rf "$work"' RETURN
  n_ver="${N_TAG#v}"
  n1_ver="${N1_TAG#v}"

  note "Pinning public signed app pair for #167 (host_arch=${arch}; both app ZIPs recorded)"
  fetch_checksums_app "$N_TAG" "$work/n.txt"
  fetch_checksums_app "$N1_TAG" "$work/n1.txt"
  n_commit="$(resolve_commit "$N_TAG")"
  n1_commit="$(resolve_commit "$N1_TAG")"
  # Standalone assignments propagate failed substitutions; printf would mask them.
  n_amd64="$(hash_for_arch "$work/n.txt" "$n_ver" amd64)"
  n_arm64="$(hash_for_arch "$work/n.txt" "$n_ver" arm64)"
  n1_amd64="$(hash_for_arch "$work/n1.txt" "$n1_ver" amd64)"
  n1_arm64="$(hash_for_arch "$work/n1.txt" "$n1_ver" arm64)"

  mkdir -p "$EVIDENCE_DIR"
  chmod 700 "$EVIDENCE_DIR"
  {
    printf 'issue: 167
'
    printf 'scope: static_synthetic_only
'
    printf 'live_canary: HAND_OFF
'
    printf 'shared_host_session: 294
'
    printf 'host_arch_default: %s
' "$arch"
    printf 'N_tag: %s
' "$N_TAG"
    printf 'N_commit: %s
' "$n_commit"
    printf 'N_sha256_amd64: %s
' "$n_amd64"
    printf 'N_sha256_arm64: %s
' "$n_arm64"
    printf 'N1_tag: %s
' "$N1_TAG"
    printf 'N1_commit: %s
' "$n1_commit"
    printf 'N1_sha256_amd64: %s
' "$n1_amd64"
    printf 'N1_sha256_arm64: %s
' "$n1_arm64"
    printf 'checksums_url_N: https://github.com/%s/releases/download/%s/checksums-app.txt
' "$REPO" "$N_TAG"
    printf 'checksums_url_N1: https://github.com/%s/releases/download/%s/checksums-app.txt
' "$REPO" "$N1_TAG"
    printf 'note: SQLite open error 14 is not by itself FDA denial (doctor cause_unverified).
'
    printf 'note: The default v0.15.0/v0.15.1 pair is historical, not a current upgrade target.
'
    printf 'note: mora upgrade selects latest, not N1_TAG. For live testing pin N1_TAG to current latest and verify the installed result; otherwise STOP.
'
    printf 'note: Host verifies signature/DR/hardened-runtime/notarization/staple on BOTH arch assets before upgrade.
'
  } | tee "$EVIDENCE_DIR/pin.txt"

  printf '\nHost handoff commands (consented Darwin; share session with #294):\n'
  cat <<HOST
# Run these steps in Bash on a consented host, not automatically.
set -euo pipefail
umask 077
# 0) RAW PRIVATE evidence: review and redact before sharing.
mkdir -p "$EVIDENCE_DIR"
chmod 700 "$EVIDENCE_DIR"
export EVIDENCE_DIR="$EVIDENCE_DIR"

# mora upgrade has no version selector. STOP if the pinned target is not latest.
# Re-run PIN with N1_TAG set to the actual intended latest release first.
[ "\$(gh api repos/${REPO}/releases/latest --jq .tag_name)" = "${N1_TAG}" ] || {
  echo 'STOP: latest release does not match pinned N1_TAG' >&2; exit 1;
}

# 1) Install / confirm N (${N_TAG}) Mora.app — record identity BEFORE upgrade
#    Use a reviewed official installer with VERSION=${n_ver}, or a verified Brew N install.
#    Do not run an unpinned latest installer and label it N.
APP="\${APP:-/Applications/Mora.app}"
[ -d "\$APP" ] || APP="\$HOME/Applications/Mora.app"
codesign -d -r- "\$APP" 2>&1 | tee "\$EVIDENCE_DIR/pre-dr.txt"
codesign -dvvv "\$APP" 2>&1 | tee "\$EVIDENCE_DIR/pre-codesign.txt"
stapler validate "\$APP" 2>&1 | tee "\$EVIDENCE_DIR/pre-staple.txt"
shasum -a 256 "\$APP/Contents/MacOS/mora" | tee "\$EVIDENCE_DIR/pre-inner-sha256.txt"
"\$APP/Contents/MacOS/mora" version | tee "\$EVIDENCE_DIR/pre-version.txt"
[ "\$(cat "\$EVIDENCE_DIR/pre-version.txt")" = "mora ${N_TAG}" ] || [ "\$(cat "\$EVIDENCE_DIR/pre-version.txt")" = "mora ${n_ver}" ] || { echo 'STOP: wrong baseline version' >&2; exit 1; }

# 2) Pre-upgrade protected reads (interactive) — must succeed WITHOUT new FDA grant
"\$APP/Contents/MacOS/mora" doctor --json 2>&1 | tee "\$EVIDENCE_DIR/pre-doctor.json"
"\$APP/Contents/MacOS/mora" sync imessage --json 2>&1 | tee "\$EVIDENCE_DIR/pre-imessage.json"
"\$APP/Contents/MacOS/mora" sync applecalendar --json 2>&1 | tee "\$EVIDENCE_DIR/pre-applecalendar.json"
# Capture SyncStatus LastSuccessAt (paths under state dir; do not paste source content)
# Expect genuine new successes — process exit alone is insufficient.

# 3) Atomic whole-app N→N+1 (supported route). Prefer:
#    - from signed app install: mora upgrade   (whole-bundle swap)
#    - from Brew session (#294): brew upgrade --cask pyranthus-hq/tap/mora
#    Do NOT re-sign, strip quarantine, or swap only Contents/MacOS/mora.
"\$APP/Contents/MacOS/mora" upgrade 2>&1 | tee "\$EVIDENCE_DIR/upgrade.txt"
# or: brew upgrade --cask pyranthus-hq/tap/mora 2>&1 | tee "\$EVIDENCE_DIR/brew-upgrade.txt"

# 4) Post-upgrade: same DR/team/id; CLI resolves into Mora.app; no FDA re-grant
codesign -d -r- "\$APP" 2>&1 | tee "\$EVIDENCE_DIR/post-dr.txt"
diff -u "\$EVIDENCE_DIR/pre-dr.txt" "\$EVIDENCE_DIR/post-dr.txt" | tee "\$EVIDENCE_DIR/dr-diff.txt"
"\$APP/Contents/MacOS/mora" version | tee "\$EVIDENCE_DIR/post-version.txt"
[ "\$(cat "\$EVIDENCE_DIR/post-version.txt")" = "mora ${N1_TAG}" ] || [ "\$(cat "\$EVIDENCE_DIR/post-version.txt")" = "mora ${n1_ver}" ] || { echo 'STOP: upgrade did not install pinned N1_TAG' >&2; exit 1; }
command -v mora; readlink "\$(command -v mora)" 2>/dev/null | tee "\$EVIDENCE_DIR/cli-route.txt" || true

# 5) Post-upgrade interactive + unattended protected reads
"\$APP/Contents/MacOS/mora" sync imessage --json 2>&1 | tee "\$EVIDENCE_DIR/post-imessage.json"
"\$APP/Contents/MacOS/mora" sync applecalendar --json 2>&1 | tee "\$EVIDENCE_DIR/post-applecalendar.json"
# Unattended: trigger installed schedule / ingest-hourly via Mora.app identity
# and require producer LastAttemptAt == LastSuccessAt newer than upgrade time.
# "\$APP/Contents/MacOS/mora" ingest run --all   # only if schedule path uses app relay

# 6) Fail-closed: unrelated signer / broken bundle must NOT advance LastSuccessAt
#    (use disposable fixtures only — never re-sign the live FDA-bearing app).
#    Record refusal evidence or leave this sub-gate explicit as unmet.

# No automatic sanitization is performed. Raw output may include private paths,
# source metadata or diagnostics. Manually redact into a separate shareable copy.
# Publish only versions,
# CDHash/DR, team id, timestamps, item counts, error codes — never message bodies.
HOST
  hand_off
}

find_app() {
  if [ -n "${APP:-}" ]; then
    printf '%s\n' "$APP"
    return
  fi
  if [ -d /Applications/Mora.app ]; then
    printf '/Applications/Mora.app\n'
  elif [ -d "${HOME}/Applications/Mora.app" ]; then
    printf '%s\n' "${HOME}/Applications/Mora.app"
  else
    printf '\n'
  fi
}

host_snapshot() {
  [ "$(uname -s)" = "Darwin" ] || die "HOST mode requires Darwin"
  local app failed=0
  app="$(find_app)"
  [ -n "$app" ] || die "Mora.app not found; set APP=/path/to/Mora.app"
  mkdir -p "$EVIDENCE_DIR"
  chmod 700 "$EVIDENCE_DIR"
  note "Evidence is RAW and PRIVATE; manually redact before sharing"
  note "HOST identity snapshot for $app (not a live FDA verdict)"
  {
    printf 'app: %s\n' "$app"
    printf 'live: %s\n' "$LIVE"
    printf 'live_canary: HAND_OFF\n'
    printf 'shared_host_session: 294\n'
  } | tee "$EVIDENCE_DIR/host-meta.txt"
  codesign -d -r- "$app" 2>&1 | tee "$EVIDENCE_DIR/host-dr.txt" || die "codesign DR failed"
  codesign -dvvv "$app" 2>&1 | tee "$EVIDENCE_DIR/host-codesign.txt" || die "codesign -dvvv failed"
  if command -v stapler >/dev/null 2>&1; then
    stapler validate "$app" 2>&1 | tee "$EVIDENCE_DIR/host-staple.txt" || { note "stapler validate failed (record as UNKNOWN)"; failed=1; }
  else
    note "stapler not available (record as UNKNOWN)"
    failed=1
  fi
  "$app/Contents/MacOS/mora" version 2>&1 | tee "$EVIDENCE_DIR/host-version.txt" || failed=1

  if [ "$LIVE" = 1 ]; then
    note "LIVE=1: running interactive syncs; SUCCESS requires LastSuccessAt evidence review"
    "$app/Contents/MacOS/mora" doctor --json 2>&1 | tee "$EVIDENCE_DIR/live-doctor.json" || failed=1
    "$app/Contents/MacOS/mora" sync imessage --json 2>&1 | tee "$EVIDENCE_DIR/live-imessage.json" || failed=1
    "$app/Contents/MacOS/mora" sync applecalendar --json 2>&1 | tee "$EVIDENCE_DIR/live-applecalendar.json" || failed=1
    note "Review JSON for genuine new successes; SQLite 14 ≠ FDA denial by itself"
  else
    note "Skipping LIVE syncs (set LIVE=1 on a consented host after PIN + N install)"
  fi
  hand_off
  return "$failed"
}

case "$MODE" in
  PIN|pin) pin_pair ;;
  HOST|host) host_snapshot ;;
  *)
    printf 'usage: %s [PIN|HOST]\n' "$(basename "$0")" >&2
    exit 2
    ;;
esac
