#!/usr/bin/env bash
# Mora pre-release regression harness — Tier 2 (macOS-native).
#
# Covers the surfaces Tier 1 (regression.sh) explicitly defers because they are
# real-Mac-only and can't run in a Linux container:
#   2a Developer ID / Gatekeeper / installer identity preservation
#   2b launchd plist generation            — `mora schedule install` (index/pulse)
#   2c osascript toast gate                — `--notify` + MORA_NO_NOTIFY suppression
#   2d iMessage real decode                — chat.db typedstream decode (livedb test)
#   2e Apple Calendar                      — connector enable + (opt) live decode
#   2f cp-over-running SIGKILL-137 hazard  — and the atomic-rename mitigation
#
# SAFE BY DEFAULT. This script never touches the live `mora` binary, the live
# vault, or the real ~/Library/LaunchAgents:
#   - everything installs into a sandbox PREFIX under $WORK; MORA_CONFIG_DIR
#     re-roots vault+index+data+state+config into $WORK/sandbox.
#   - the launchd test redirects HOME to a sandbox dir, so the plist lands in the
#     sandbox and is NEVER launchctl-loaded (installSchedule only writes the file).
#   - the SIGKILL hazard runs on throwaway sandbox binary copies.
#   - the only live-system access is READ-ONLY chat.db / Calendar.sqlitedb reads
#     via the livedb go-test and the optional MORA_REGRESS_LIVE_BIN path.
#
# Inputs (env):
#   MORA_REPO              path to the mora repo (install.sh, build_vault.py, go test)  [required]
#   MORA_BIN              a version-stamped HEAD `mora`; required with RELEASE=1,
#                         otherwise optional (the harness builds a local binary)
#   EXPECTED_VER          version the HEAD binary must report (default: git describe)  [optional]
#   RELEASE=1             require + install an externally supplied Developer-ID-signed,
#                         Apple-notarized MORA_BIN; any identity failure is fatal
#   WORK                  sandbox root (default: a fresh mktemp dir, removed on exit)
#   MORA_REGRESS_LIVE_BIN an FDA-granted installed mora, to exercise the real
#                         `sync imessage` / calendar success path against the sandbox
#   MORA_REGRESS_REAL_NOTIFY  set to fire ONE real osascript toast (human-visible)
#   SKIP_LIVEDB=1         skip the FDA-gated chat.db decode test
#   SKIP_HAZARD=1         skip the SIGKILL-137 / atomic-rename section
# Exit code is the verdict: 0 = all green, non-zero = first hard failure.
# (Environmental gaps — no chat.db, FDA not granted — are loud SKIPs, not failures.
# Release identity is skipped only in local-dev mode; RELEASE=1 is fail closed.)
set -euo pipefail

# ---- preflight ----------------------------------------------------------------
if [ "$(uname -s)" != "Darwin" ]; then
  echo "Tier 2 is macOS-only. On Linux/CI run Tier 1: scripts/regress/regression.sh" >&2
  exit 2
fi
: "${MORA_REPO:?set MORA_REPO=/path/to/mora}"
[ -f "$MORA_REPO/go.mod" ] || { echo "FATAL: MORA_REPO=$MORA_REPO has no go.mod" >&2; exit 2; }
command -v go      >/dev/null 2>&1 || { echo "FATAL: go toolchain required" >&2; exit 2; }
command -v codesign>/dev/null 2>&1 || { echo "FATAL: codesign required (Xcode CLT)" >&2; exit 2; }
PY="$(command -v python3 || command -v python || true)"
[ -n "$PY" ] || { echo "FATAL: python3 required (for seeding + JSON asserts)" >&2; exit 2; }

RELEASE="${RELEASE:-0}"
case "$RELEASE" in
  0|1) : ;;
  *) echo "FATAL: RELEASE must be 0 or 1 (got $RELEASE)" >&2; exit 2 ;;
esac
if [ "$RELEASE" = 1 ]; then
  [ -n "${MORA_BIN:-}" ] || {
    echo "FATAL: RELEASE=1 requires an externally supplied notarized MORA_BIN" >&2
    exit 2
  }
  [ -x "$MORA_BIN" ] || {
    echo "FATAL: RELEASE=1 MORA_BIN is not executable: $MORA_BIN" >&2
    exit 2
  }
  command -v xattr >/dev/null 2>&1 || {
    echo "FATAL: xattr required for RELEASE=1 quarantined launch assessment" >&2
    exit 2
  }
fi

OWN_WORK=0
if [ -z "${WORK:-}" ]; then WORK="$(mktemp -d)"; OWN_WORK=1; fi
SANDBOX="$WORK/sandbox"
SBHOME="$WORK/home"                 # redirected HOME for the launchd test (inert)
export MORA_CONFIG_DIR="$SANDBOX"   # re-roots EVERYTHING into the sandbox
export MORA_VAULT="$SANDBOX/vault"

# Track background servers so the trap always reaps them.
SRV_PIDS=()
cleanup() {
  if [ "${#SRV_PIDS[@]}" -gt 0 ]; then
    for p in "${SRV_PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  fi
  exec 3>&- 2>/dev/null || true
  [ "$OWN_WORK" = 1 ] && rm -rf "$WORK" || true
}
trap cleanup EXIT

# alive PID — true only if the process exists AND is not a zombie. A SIGKILLed
# child is a zombie until reaped, and `kill -0` reports a zombie as alive; the
# ps STAT check avoids misclassifying a killed server as "survived".
alive() { local s; s="$(ps -o stat= -p "$1" 2>/dev/null | tr -d ' ')"; [ -n "$s" ] && [ "${s#Z}" = "$s" ]; }

# ---- output helpers (match Tier 1) -------------------------------------------
section() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
pass()    { printf '  \033[32mok\033[0m   %s\n' "$*"; }
note()    { printf '  \033[33mnote\033[0m %s\n' "$*"; }
skip()    { printf '  \033[33mSKIP\033[0m %s\n' "$*"; }
die()     { printf '  \033[31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }
json_true() {
  printf '%s' "$1" | "$PY" -c "import sys,json; d=json.load(sys.stdin); assert ($2), 'predicate false: $2'" \
    2>/dev/null || die "json predicate failed: $2"
}
json_get() { printf '%s' "$1" | "$PY" -c "import sys,json; d=json.load(sys.stdin); print($2)"; }

release_cdhash() {
  local hash
  hash="$(codesign -dvvv "$1" 2>&1 | sed -n 's/^CDHash=//p')"
  [ -n "$hash" ] || return 1
  printf '%s\n' "$hash"
}

# Independently enforce the release identity contract instead of trusting the
# installer's own checks. Raw command-line executables cannot carry a stapled
# ticket. Evaluate Apple's `notarized` code requirement for the exact
# code-directory hash, then launch a quarantined disposable copy so the test
# follows the downloaded-command path without changing the release artifact.
verify_release_identity() {
  local binary="$1" label="$2" sign_info requirement_info launch_dir launch_copy quarantine_value

  codesign --verify --strict --verbose=2 "$binary" >/dev/null 2>&1 \
    || die "$label fails strict code-signature verification"
  sign_info="$(codesign -dvvv "$binary" 2>&1)" \
    || die "could not inspect $label's code signature"
  printf '%s\n' "$sign_info" | grep -Fqx 'Identifier=com.pyranthus.mora' \
    || die "$label has the wrong signing identifier (expected com.pyranthus.mora)"
  printf '%s\n' "$sign_info" | grep -Fqx 'TeamIdentifier=VS8M5VJBZ5' \
    || die "$label has the wrong Apple team (expected VS8M5VJBZ5)"
  printf '%s\n' "$sign_info" | grep -Fq 'Authority=Developer ID Application:' \
    || die "$label is not signed with a Developer ID Application certificate"
  printf '%s\n' "$sign_info" | grep -Eq '^CodeDirectory .*flags=.*\(runtime\)' \
    || die "$label does not enable the hardened runtime"
  printf '%s\n' "$sign_info" | grep -Eq '^Timestamp=.+' \
    || die "$label has no secure timestamp"

  requirement_info="$(codesign -d -r- "$binary" 2>&1)" \
    || die "could not inspect $label's designated requirement"
  printf '%s\n' "$requirement_info" | grep -Fq 'designated =>' \
    || die "$label has no designated requirement"
  printf '%s\n' "$requirement_info" | grep -Fq 'identifier "com.pyranthus.mora"' \
    || die "$label designated requirement has the wrong identifier"
  printf '%s\n' "$requirement_info" | grep -Fq 'anchor apple generic' \
    || die "$label designated requirement is not anchored to Apple"
  printf '%s\n' "$requirement_info" | \
    grep -Eq 'subject\.OU[^=]*= ("VS8M5VJBZ5"|VS8M5VJBZ5)( |$)' \
    || die "$label designated requirement has the wrong Apple team"

  codesign --verify --strict --verbose=2 -R='notarized' "$binary" >/dev/null 2>&1 \
    || die "$label does not satisfy Apple's notarized code requirement"

  launch_dir="$(mktemp -d "$WORK/notarized-launch.XXXXXX")" \
    || die "could not create the disposable notarized-launch directory"
  launch_copy="$launch_dir/mora"
  cp "$binary" "$launch_copy" \
    || die "could not stage $label for the quarantined launch"
  chmod +x "$launch_copy"
  quarantine_value='0083;00000000;MoraReleaseRegression;'
  xattr -w com.apple.quarantine "$quarantine_value" "$launch_copy" \
    || die "could not quarantine the disposable $label launch copy"
  [ "$(xattr -p com.apple.quarantine "$launch_copy" 2>/dev/null || true)" = "$quarantine_value" ] \
    || die "disposable $label launch copy did not retain its quarantine attribute"
  "$launch_copy" version >/dev/null 2>&1 \
    || die "quarantined disposable copy of $label did not launch"
  rm -rf "$launch_dir"
}

# ---- build (or locate) a stamped HEAD binary ---------------------------------
section "Tier 2 setup — build a stamped binary + sandbox install"
VER="${EXPECTED_VER:-$(git -C "$MORA_REPO" describe --tags --always --dirty 2>/dev/null | sed 's/^v//')}"
[ -n "$VER" ] || VER="0.0.0-regress"
SHA="$(git -C "$MORA_REPO" rev-parse --short HEAD 2>/dev/null || echo local)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X main.version=$VER -X main.commit=$SHA -X main.date=$DATE"

BUILT="$WORK/build/mora"; mkdir -p "$WORK/build"
if [ -n "${MORA_BIN:-}" ] && [ -x "$MORA_BIN" ]; then
  cp "$MORA_BIN" "$BUILT"
  pass "using provided MORA_BIN ($MORA_BIN)"
else
  ( cd "$MORA_REPO" && CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$BUILT" ./cmd/mora )
  pass "built stamped binary @ $VER ($SHA)"
fi

PREFIX="$WORK/bin"
MORA="$PREFIX/mora"
SOURCE_CDHASH=""
REPLACED_INODE=""
INSTALLED_INODE=""
if [ "$RELEASE" = 1 ]; then
  verify_release_identity "$MORA_BIN" "provided MORA_BIN"
  SOURCE_CDHASH="$(release_cdhash "$MORA_BIN")" \
    || die "could not read provided MORA_BIN's code-directory hash"

  # Shape the source like an extracted release archive so install.sh uses LOCAL
  # mode. Seed the destination with a deliberately ad-hoc-signed TEST FIXTURE:
  # a different inode and cdhash make the installer's atomic pathname replacement
  # observable without touching a real installed Mora.
  STAGE="$WORK/staging"; mkdir -p "$STAGE" "$PREFIX"
  cp "$BUILT" "$STAGE/mora"; chmod +x "$STAGE/mora"
  cp "$MORA_REPO/install.sh" "$STAGE/install.sh"
  [ "$(release_cdhash "$STAGE/mora")" = "$SOURCE_CDHASH" ] \
    || die "release staging changed MORA_BIN's code-directory hash"

  cp "$BUILT" "$MORA"
  codesign --force --sign - --identifier com.pyranthus.mora.regress-old "$MORA" >/dev/null 2>&1 \
    || die "could not create the ad-hoc pre-install test fixture"
  REPLACED_INODE="$(stat -f '%i' "$MORA")"

  if ! INSTALL_OUT="$(PREFIX="$PREFIX" MORA_VAULT="$MORA_VAULT" sh "$STAGE/install.sh" 2>&1)"; then
    printf '%s\n' "$INSTALL_OUT" >&2
    die "install.sh failed for the signed/notarized release binary"
  fi
  [ -x "$MORA" ] || die "install.sh did not place an executable mora in PREFIX"
  INSTALLED_INODE="$(stat -f '%i' "$MORA")"
  [ "$INSTALLED_INODE" != "$REPLACED_INODE" ] \
    || die "install.sh overwrote the existing inode instead of replacing it atomically"
  pass "install.sh atomically installed the signed release into the sandbox PREFIX"
else
  # The production installer intentionally rejects locally built unsigned/ad-hoc
  # binaries. Stage directly so the unrelated macOS Tier 2 checks remain useful
  # during development; this path makes no release-identity claim.
  mkdir -p "$PREFIX"
  cp "$BUILT" "$MORA"; chmod +x "$MORA"
  pass "direct-staged local development binary (release installer not exercised)"
fi

"$MORA" version | grep -q "$VER" || die "sandbox binary not stamped $VER"
export PATH="$PREFIX:$PATH"
pass "sandbox binary is stamped $VER"

# Seed a synthetic vault (same corpus as Tier 1 / the bench).
"$PY" "$MORA_REPO/scripts/bench/agent-ab/build_vault.py" \
  "$MORA_REPO/scripts/bench/agent-ab/world.json" "$SANDBOX" >/dev/null 2>&1 \
  || die "seeding the sandbox vault failed"
pass "seeded sandbox vault"

# =============================================================================
section "Tier 2a — Developer ID / Gatekeeper / atomic installer identity"
if [ "$RELEASE" = 1 ]; then
  verify_release_identity "$MORA" "installed Mora"
  INSTALLED_CDHASH="$(release_cdhash "$MORA")" \
    || die "could not read installed Mora's code-directory hash"
  [ "$INSTALLED_CDHASH" = "$SOURCE_CDHASH" ] \
    || die "installer changed the signed release's code-directory hash"
  [ "$INSTALLED_INODE" != "$REPLACED_INODE" ] \
    || die "installer did not atomically replace the pre-existing binary"
  "$MORA" version >/dev/null 2>&1 || die "installed signed release does not execute"
  pass "identifier com.pyranthus.mora, team VS8M5VJBZ5, Developer ID, runtime + timestamp"
  pass "designated requirement, Apple's notarized requirement, and quarantined launch"
  pass "atomic installer replacement preserved cdhash $INSTALLED_CDHASH"
else
  skip "release identity/notarization + installer preservation (requires RELEASE=1 with signed MORA_BIN)"
fi

# =============================================================================
section "Tier 2b — launchd plist generation (HOME-redirected → inert, no load)"
REAL_LA="$HOME/Library/LaunchAgents"
BEFORE="$(ls "$REAL_LA"/com.mora.*.plist 2>/dev/null | sort || true)"
mkdir -p "$SBHOME/Library/LaunchAgents"
SBLA="$SBHOME/Library/LaunchAgents"

# index-hourly: StartInterval 3600, RunAtLoad present, MORA_CONFIG_DIR snapshotted.
HOME="$SBHOME" "$MORA" schedule install index-hourly >/dev/null 2>&1 \
  || die "schedule install index-hourly errored"
PL="$SBLA/com.mora.index-hourly.plist"
[ -f "$PL" ] || die "schedule install wrote no plist into the (sandbox) LaunchAgents"
grep -q '<string>com.mora.index-hourly</string>' "$PL" || die "index-hourly plist: wrong Label"
grep -q '<string>index</string>' "$PL" && grep -q '<string>rebuild</string>' "$PL" \
  || die "index-hourly plist: ProgramArguments missing 'index rebuild'"
grep -q '<key>StartInterval</key><integer>3600</integer>' "$PL" \
  || die "index-hourly plist: StartInterval 3600 missing"
grep -q '<key>RunAtLoad</key><true/>' "$PL" || die "index-hourly plist: RunAtLoad should be present"
grep -q "<string>$SANDBOX</string>" "$PL" \
  || die "index-hourly plist: MORA_CONFIG_DIR not snapshotted into EnvironmentVariables"
pass "index-hourly plist: label, args, StartInterval, RunAtLoad, MORA_CONFIG_DIR snapshot"

# pulse-daily: StartCalendarInterval Hour 8, and RunAtLoad ABSENT (watermark guard).
HOME="$SBHOME" "$MORA" schedule install pulse-daily >/dev/null 2>&1 \
  || die "schedule install pulse-daily errored"
PP="$SBLA/com.mora.pulse-daily.plist"
[ -f "$PP" ] || die "pulse-daily plist not written"
grep -q '<key>StartCalendarInterval</key>' "$PP" && grep -q '<key>Hour</key><integer>8</integer>' "$PP" \
  || die "pulse-daily plist: StartCalendarInterval Hour 8 missing"
grep -q '<key>RunAtLoad</key>' "$PP" \
  && die "pulse-daily plist must NOT set RunAtLoad (would consume the morning watermark on boot)"
pass "pulse-daily plist: calendar Hour 8, RunAtLoad correctly absent"

# schedule list (HOME-redirected) globs the sandbox LaunchAgents.
LIST="$(HOME="$SBHOME" "$MORA" schedule list 2>/dev/null || true)"
echo "$LIST" | grep -q 'com.mora.index-hourly.plist' || die "schedule list missed index-hourly"
echo "$LIST" | grep -q 'com.mora.pulse-daily.plist'  || die "schedule list missed pulse-daily"
pass "schedule list reflects both installed jobs"

# Guard: the redirect worked — the REAL LaunchAgents was not touched.
AFTER="$(ls "$REAL_LA"/com.mora.*.plist 2>/dev/null | sort || true)"
[ "$BEFORE" = "$AFTER" ] || die "schedule install LEAKED a plist into the real ~/Library/LaunchAgents"
pass "real ~/Library/LaunchAgents untouched (HOME redirect held)"

# =============================================================================
section "Tier 2c — osascript toast gate (--notify honours MORA_NO_NOTIFY)"
MORA_NO_NOTIFY=1 "$MORA" pulse --digest --notify >/dev/null 2>&1 \
  || die "pulse --digest --notify errored under MORA_NO_NOTIFY=1"
pass "pulse --notify is a clean no-op when MORA_NO_NOTIFY is set"
if [ -n "${MORA_REGRESS_REAL_NOTIFY:-}" ]; then
  ( unset MORA_NO_NOTIFY; "$MORA" pulse --digest --notify >/dev/null 2>&1 || true )
  note "fired one real osascript toast (best-effort, fire-and-forget — verify visually)"
fi

# =============================================================================
if [ "${SKIP_LIVEDB:-0}" != "1" ]; then
section "Tier 2d — iMessage real decode (read-only; FDA-gated)"
# Darwin readiness must render (Tier 1 only asserts the Linux no-op).
DOC="$("$MORA" doctor 2>&1 || true)"
echo "$DOC" | grep -q '^ok   imessage_macos' || die "doctor did not render iMessage readiness on darwin"
pass "doctor renders iMessage readiness block on darwin"
FDA_OK=0; echo "$DOC" | grep -q '^ok   imessage_full_disk_access' && FDA_OK=1
DB_OK=0;  echo "$DOC" | grep -q '^ok   imessage_chat_db'           && DB_OK=1

# enable on darwin must succeed WITHOUT the "only runs on macOS" note.
EN="$("$MORA" connectors enable imessage 2>&1 || true)"
echo "$EN" | grep -qi 'only runs on macOS\|only on macOS' \
  && die "connectors enable imessage printed a non-darwin note ON darwin"
pass "connectors enable imessage succeeds on darwin"

# Real chat.db typedstream decode via the livedb-tagged integration test.
set +e
LIVEDB_OUT="$(cd "$MORA_REPO" && go test ./internal/imessage/ -run TestLiveChatDBConversation -tags=livedb -count=1 2>&1)"
LIVEDB_RC=$?
set -e
if [ "$LIVEDB_RC" -eq 0 ]; then
  if echo "$LIVEDB_OUT" | grep -qi 'SKIP'; then
    skip "livedb decode skipped (no chat.db on this Mac)"
  else
    pass "livedb chat.db decode: real conversation decoded with non-empty body + plausible date"
  fi
elif [ "$DB_OK" = 0 ]; then
  skip "livedb decode: no readable chat.db (Messages not set up)"
elif [ "$FDA_OK" = 0 ]; then
  skip "livedb decode: chat.db present but Full Disk Access not granted to this shell — grant FDA and re-run"
else
  printf '%s\n' "$LIVEDB_OUT" | tail -20 >&2
  die "livedb chat.db decode FAILED with FDA granted — real decode regression"
fi

# Optional true end-to-end: an FDA-granted binary reads real chat.db (read-only)
# and writes derived memories into the SANDBOX vault — never the live vault.
if [ -n "${MORA_REGRESS_LIVE_BIN:-}" ] && [ -x "$MORA_REGRESS_LIVE_BIN" ]; then
  if MORA_CONFIG_DIR="$SANDBOX" "$MORA_REGRESS_LIVE_BIN" sync imessage --since-days 7 >/dev/null 2>&1; then
    pass "live-bin sync imessage --since-days 7 succeeded against the sandbox vault"
  else
    note "live-bin sync imessage returned non-zero (likely FDA on that binary, or no recent messages)"
  fi
fi
fi

# =============================================================================
section "Tier 2e — Apple Calendar connector (read-only)"
ENC="$("$MORA" connectors enable applecalendar 2>&1 || true)"
echo "$ENC" | grep -qi 'only runs on macOS\|only on macOS' \
  && die "connectors enable applecalendar printed a non-darwin note ON darwin"
pass "connectors enable applecalendar succeeds on darwin"
CAL_MODERN="$HOME/Library/Group Containers/group.com.apple.calendar/Calendar.sqlitedb"
CAL_LEGACY="$HOME/Library/Calendars/Calendar.sqlitedb"
if [ -r "$CAL_MODERN" ] || [ -r "$CAL_LEGACY" ]; then
  note "Calendar store present + readable (live decode covered by applecal_test.go + optional live-bin)"
  if [ -n "${MORA_REGRESS_LIVE_BIN:-}" ] && [ -x "$MORA_REGRESS_LIVE_BIN" ]; then
    MORA_CONFIG_DIR="$SANDBOX" "$MORA_REGRESS_LIVE_BIN" ingest run --all >/dev/null 2>&1 \
      && pass "live-bin ingest run --all completed against the sandbox vault" \
      || note "live-bin ingest run --all returned non-zero (FDA / no events)"
  fi
else
  skip "no readable Calendar.sqlitedb (Calendar not set up or FDA not granted)"
fi

# =============================================================================
if [ "${SKIP_HAZARD:-0}" != "1" ]; then
section "Tier 2f — cp-over-running SIGKILL-137 hazard + atomic-rename mitigation"
# Fresh ad-hoc-signed TEST-ONLY sandbox copies; we never touch the live binary
# and these fixtures make no release-identity claim.
SRVDIR="$WORK/srv"; mkdir -p "$SRVDIR"
cp "$MORA" "$SRVDIR/mora"; codesign --force --sign - "$SRVDIR/mora" 2>/dev/null || true
# The swap-in must have a DIFFERENT cdhash than the running copy, or overwriting
# wouldn't change the code-signature and the hazard wouldn't even be exercised.
# Same bytes, different --identifier → different CodeDirectory → different cdhash
# (no second `go build` needed); the running copies keep the default identifier.
SWAPIN="$WORK/swapin/mora"; mkdir -p "$WORK/swapin"
cp "$BUILT" "$SWAPIN"; codesign --force --sign - --identifier mora-swapin "$SWAPIN" 2>/dev/null || true

# A fifo keeps the server's stdin open so `mora mcp serve` stays alive (no EOF).
FIFO="$WORK/fifo"; mkfifo "$FIFO"; exec 3<>"$FIFO"

start_server() {  # $1 = binary path → echoes the PID (parent appends to SRV_PIDS)
  "$1" mcp serve <&3 >/dev/null 2>&1 &
  local p=$!; sleep 1
  alive "$p" || die "mcp serve did not start"
  echo "$p"
}

# Mitigation (DETERMINISTIC): atomic rename — what the installer and `mora
# upgrade` do — leaves the running inode intact, so a live server SURVIVES the
# swap.
SRV1="$(start_server "$SRVDIR/mora")"; SRV_PIDS+=("$SRV1")
cp "$SWAPIN" "$WORK/newbin"
mv -f "$WORK/newbin" "$SRVDIR/mora"
sleep 1
if alive "$SRV1"; then
  pass "atomic rename over a running server: server SURVIVED (mora upgrade's swap is safe)"
  kill "$SRV1" 2>/dev/null || true
else
  die "atomic rename killed the running server — unexpected; upgrade swap would be unsafe"
fi

# Hazard (REPRODUCE-AND-REPORT, non-fatal): a plain `cp` over the destination
# truncates the SAME inode; on a signed binary the cdhash check then fails and
# the kernel SIGKILLs the running process. alive() excludes zombies, so a
# killed-but-not-yet-reaped server isn't misread as "survived".
SRV2DIR="$WORK/srv2"; mkdir -p "$SRV2DIR"
cp "$MORA" "$SRV2DIR/mora"; codesign --force --sign - "$SRV2DIR/mora" 2>/dev/null || true
SRV2="$(start_server "$SRV2DIR/mora")"; SRV_PIDS+=("$SRV2")
cp -f "$SWAPIN" "$SRV2DIR/mora" 2>/dev/null || true
sleep 2
if alive "$SRV2"; then
  note "cp-over-running did NOT kill on this macOS build — still hazardous; mora upgrade uses rename to be safe"
  kill "$SRV2" 2>/dev/null || true
else
  pass "REPRODUCED SIGKILL-137: cp-over-running is unsafe (installer + upgrade use atomic swap)"
fi
exec 3>&- 2>/dev/null || true
fi

section "ALL TIER-2 CHECKS PASSED for $VER (macOS-native)"
