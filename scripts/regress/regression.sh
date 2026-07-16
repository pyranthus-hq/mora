#!/usr/bin/env bash
# Mora pre-release regression harness — Tier 1 (cross-platform).
#
# Proves, against a REAL installed binary, that: install.sh works on a clean
# box, the binary is version-stamped (not "dev"), every smoke-testable command
# exits cleanly on a seeded vault, the MCP wire protocol round-trips (incl. the
# notification-no-reply regression guard), write→read→delete and tasks actually
# persist, the filesystem/PDF/DOCX connectors extract+index, git-sync pushes
# without leaking a PAT, `mora doctor --json --strict` reports healthy, and an
# UPGRADE from the previous release preserves the vault/index.
#
# Runs identically on a Linux container (the release shape) and natively on
# macOS (for fast dev validation). The macOS-only data paths — iMessage decode,
# Apple Calendar, launchd/osascript, codesign/Gatekeeper + the cp-over-running
# SIGKILL-137 hazard — are NOT covered here; see regression-macos.sh (Tier 2).
#
# Inputs (env):
#   MORA_REPO      path to the mora repo (for install.sh, build_vault.py, world.json)   [required]
#   MORA_BIN       path to the freshly-built, version-stamped HEAD `mora` binary         [required]
#   EXPECTED_VER   version string the HEAD binary must report (must not be "dev")         [required]
#   PREV_VER       previous published release for the upgrade test (default 0.7.0)
#   WORK           sandbox root (default: a fresh mktemp dir, removed on exit)
#   SKIP_UPGRADE   set to 1 to skip the network upgrade test (no GitHub access)
#   SKIP_GIT       set to 1 to skip the git-sync test
# Exit code is the verdict: 0 = all green, non-zero = first failure.
set -euo pipefail

# ---- inputs -------------------------------------------------------------------
: "${MORA_REPO:?set MORA_REPO=/path/to/mora}"
: "${MORA_BIN:?set MORA_BIN=/path/to/built/mora}"
: "${EXPECTED_VER:?set EXPECTED_VER=x.y.z (the stamped version)}"
PREV_VER="${PREV_VER:-0.7.0}"
OWN_WORK=0
if [ -z "${WORK:-}" ]; then WORK="$(mktemp -d)"; OWN_WORK=1; fi

export MORA_CONFIG_DIR="$WORK/sandbox"      # re-roots vault+index+data+state+config
export MORA_VAULT="$WORK/sandbox/vault"
export MORA_NO_NOTIFY=1
export PATH="$WORK/bin:$PATH"               # the installed binary wins

cleanup() { [ "$OWN_WORK" = 1 ] && rm -rf "$WORK" || true; }
trap cleanup EXIT

PY="$(command -v python3 || command -v python)"
[ -n "$PY" ] || { echo "FATAL: python3 required (for JSON asserts + seeding)"; exit 2; }

# ---- output helpers -----------------------------------------------------------
section() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
pass()    { printf '  \033[32mok\033[0m   %s\n' "$*"; }
die()     { printf '  \033[31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }
run()     { "$@" >/dev/null 2>&1 || die "command failed: $*"; }

# json_true "<json>" "<python-expr over variable d>" — assert a JSON predicate.
json_true() {
  local json="$1" expr="$2"
  printf '%s' "$json" | "$PY" -c "
import sys, json
d = json.load(sys.stdin)
assert ($expr), 'predicate false: $expr'
" 2>/dev/null || die "json predicate failed: $expr"
}

# json_get "<json>" "<python-expr>" — print a derived value.
json_get() {
  printf '%s' "$1" | "$PY" -c "import sys,json; d=json.load(sys.stdin); print($2)"
}

# hash_file — portable file hash (shasum ships with Perl; minimal Linux containers
# only have coreutils' sha256sum). Used by the config tripwire.
hash_file() { if command -v shasum >/dev/null 2>&1; then shasum "$1"; else sha256sum "$1"; fi; }

# cfg_fingerprint — a deterministic fingerprint of the developer's REAL
# ~/.config/mora dir (every file hashed, path-sorted), or the literal ABSENT when
# the dir does not exist. Snapshotting the whole tree (not just config.toml) and
# distinguishing ABSENT vs present lets the tripwire catch BOTH mutation of an
# existing config AND creation of a new one where none existed (the CI/fresh-box
# case the release gate actually runs in). Always exits 0 (never aborts under set -e).
cfg_fingerprint() {
  local d="$HOME/.config/mora"
  [ -e "$d" ] || { printf 'ABSENT\n'; return 0; }
  find "$d" -type f 2>/dev/null | LC_ALL=C sort | while IFS= read -r f; do
    hash_file "$f" 2>/dev/null || true
  done
  return 0
}

# =============================================================================
section "Tier 1a — install.sh (LOCAL mode) on a clean box"
# Stage a dir that mimics a release tarball so install.sh hits its local path.
STAGE="$WORK/staging"
mkdir -p "$STAGE"
cp "$MORA_BIN" "$STAGE/mora"; chmod +x "$STAGE/mora"
cp "$MORA_REPO/install.sh" "$STAGE/install.sh"
PREFIX="$WORK/bin" MORA_VAULT="$MORA_VAULT" sh "$STAGE/install.sh" >/dev/null 2>&1 \
  || die "install.sh failed"
[ -x "$WORK/bin/mora" ] || die "install.sh did not place an executable mora in PREFIX"
pass "install.sh placed binary at \$PREFIX/mora"

VER_LINE="$(mora version | head -1)"
echo "$VER_LINE" | grep -q "$EXPECTED_VER" || die "version mismatch: '$VER_LINE' lacks '$EXPECTED_VER'"
echo "$VER_LINE" | grep -qiw dev && die "binary reports 'dev' — release ldflags not stamped"
pass "mora version stamped: $VER_LINE"

# `mora config` annotates the vault line for humans ("vault_dir = <path>   ← your
# memories (back this up)"); strip the arrow + everything after it so the comparison
# sees only the path. Anchoring on the arrow (not a bare run of spaces) avoids
# truncating a vault path that itself contains consecutive spaces; if the annotation
# is dropped entirely this still yields the bare path.
ACTIVE_VAULT="$(mora config 2>/dev/null | sed -n 's/^vault_dir = //p' | head -1 | sed -E 's/[[:space:]]+←.*$//')"
[ "$ACTIVE_VAULT" = "$MORA_VAULT" ] || die "active vault '$ACTIVE_VAULT' != expected '$MORA_VAULT'"
pass "init repointed vault to the sandbox"

# Version-drift guard: install.sh's hardcoded VERSION drives remote-mode downloads.
INSTALL_VER="$(sed -n 's/^VERSION="\${VERSION:-\([0-9.]*\)}"/\1/p' "$MORA_REPO/install.sh" | head -1)"
if [ -n "$INSTALL_VER" ] && [ "$INSTALL_VER" != "$EXPECTED_VER" ]; then
  if [ "${RELEASE:-0}" = "1" ]; then
    die "install.sh VERSION=$INSTALL_VER != release EXPECTED_VER=$EXPECTED_VER (bump install.sh before tagging)"
  fi
  printf '  \033[33mnote\033[0m install.sh VERSION=%s != EXPECTED_VER=%s (expected in dev; MUST match at release — run RELEASE=1 to gate)\n' \
    "$INSTALL_VER" "$EXPECTED_VER"
fi

# Supply-chain guard: the remote installer MUST verify the download against the
# release checksums and abort on mismatch — don't let that silently regress.
grep -q 'checksums.txt' "$MORA_REPO/install.sh" \
  && grep -q 'CHECKSUM MISMATCH' "$MORA_REPO/install.sh" \
  || die "install.sh no longer verifies the download against checksums.txt (supply-chain regression)"
grep -q 'refusing to install an unverifiable download' "$MORA_REPO/install.sh" \
  && grep -q 'no SHA-256 tool found' "$MORA_REPO/install.sh" \
  || die "install.sh no longer fails closed when checksum verification is unavailable"
pass "install.sh verifies remote downloads and fails closed when verification is unavailable"

# =============================================================================
section "Tier 1b — seed a synthetic vault + smoke every command"
run "$PY" "$MORA_REPO/scripts/bench/agent-ab/build_vault.py" \
  "$MORA_REPO/scripts/bench/agent-ab/world.json" "$MORA_CONFIG_DIR"
LIST_JSON="$(mora list --json)"; COUNT="$(json_get "$LIST_JSON" 'len(d)')"
[ "$COUNT" -gt 0 ] || die "seeded vault has 0 memories"
pass "seeded vault: $COUNT memories"

# Commands that must exit 0 (vault-only, model-free, cross-platform).
for c in \
  "help" "version" "list --json" "search Northwind --json" \
  "entities --json" "graph --top 5 --json" "context --query Northwind --json" \
  "brief --json" "pulse --digest" \
  "lint" "backup" "doctor" "config" \
  "schedule list" "sources list" "connectors list --json" "sync status" \
  "usage report" "hook status" ; do
  # shellcheck disable=SC2086
  mora $c >/dev/null 2>&1 || die "verb failed: mora $c"
done
# think requires a question positional (mora think "<q>" [--json]).
mora think "Northwind pilot" --json >/dev/null 2>&1 || die "verb failed: mora think \"Northwind pilot\" --json"
pass "all smoke verbs exited 0"

# schedule install on Linux must print-and-noop (write nothing); on macOS it
# writes a launchd plist — only assert the no-write invariant off-darwin.
if [ "$(uname -s)" != "Darwin" ]; then
  mora schedule install index-hourly >/dev/null 2>&1 || die "schedule install errored on Linux"
  [ -e "$HOME/Library/LaunchAgents/com.mora.index-hourly.plist" ] && die "schedule install wrote a plist on Linux"
  pass "schedule install: print-and-noop on Linux"
fi

# =============================================================================
section "Tier 1c — MCP wire protocol (newline-delimited JSON-RPC over stdio)"
MCP_OUT="$WORK/mcp.out"
{
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"regress","version":"0"}}}'
  printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
  printf '%s\n' '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"Northwind"}}}'
} | mora mcp serve > "$MCP_OUT" 2>/dev/null || die "mora mcp serve crashed"

"$PY" - "$MCP_OUT" <<'PYEOF' || die "MCP wire assertions failed"
import sys, json
lines = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
by_id = {o.get("id"): o for o in lines}
assert 1 in by_id and "result" in by_id[1], "initialize had no result"
assert 2 in by_id, "tools/list had no reply"
tools = json.dumps(by_id[2].get("result", {}))
assert "search_memory" in tools, "tools/list did not advertise search_memory"
assert 3 in by_id and "result" in by_id[3], "tools/call search_memory had no result"
# REGRESSION GUARD (PR #20): a notification (id==null) must get NO reply, esp. no -32601.
for o in lines:
    if o.get("id") is None:
        raise AssertionError("server replied to a notification (regressed #20): %r" % o)
print("  mcp: initialize + tools/list + search_memory OK; no stray notification reply")
PYEOF
pass "MCP round-trip + notification-no-reply guard"

# =============================================================================
section "Tier 1d — write → read → delete + tasks persistence"
WRITE_JSON="$(mora write --scope personal --type fact --title 'delete me' --text 'ephemeral tombstone target' --json)"
ID="$(json_get "$WRITE_JSON" 'd.get("id","")')"
[ -n "$ID" ] || die "mora write --json did not return an id"
READ_JSON="$(mora read "$ID" --json)"; json_true "$READ_JSON" 'd.get("title","").startswith("delete me")'
mora index rebuild >/dev/null 2>&1 || die "index rebuild failed"
mora delete "$ID" --yes >/dev/null 2>&1 || die "delete failed"
mora read "$ID" --json >/dev/null 2>&1 && die "deleted memory still readable (not tombstoned)" || true
pass "write/read/delete round-trip"

# tasks add must PERSIST (memory: 'task added' != persisted — silent-drop bug).
mora tasks add 'regress sentinel task' --pri P1 >/dev/null 2>&1 || die "tasks add errored"
TASKS_JSON="$(mora tasks list --json)"
json_true "$TASKS_JSON" 'any("regress sentinel task" in json.dumps(t) for t in (d if isinstance(d,list) else d.get("tasks",[])))' \
  || die "tasks add did not PERSIST (re-list could not find the task)"
mora tasks done 'regress sentinel task' >/dev/null 2>&1 || die "tasks done errored"
pass "tasks add persisted + done"

# =============================================================================
section "Tier 1e — filesystem + PDF + DOCX extraction"
FIX="$WORK/fixtures"; mkdir -p "$FIX"
printf '# Note\nThe phrase quokka-md-marker lives in this markdown.\n' > "$FIX/note.md"
printf 'plain text with quokka-txt-marker inside\n' > "$FIX/note.txt"
# minimal valid PDF carrying a unique phrase (mirrors pdf_test.go writeMinimalPDF)
"$PY" - "$FIX/doc.pdf" <<'PYEOF'
import sys
body = b"BT /F1 12 Tf 72 720 Td (quokka-pdf-marker) Tj ET"
objs = [
 b"<< /Type /Catalog /Pages 2 0 R >>",
 b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
 b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
 b"<< /Length %d >>\nstream\n%s\nendstream" % (len(body), body),
 b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
]
out = b"%PDF-1.4\n"; offs=[]
for i,o in enumerate(objs,1):
    offs.append(len(out)); out += b"%d 0 obj\n%s\nendobj\n" % (i,o)
xref=len(out); out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs)+1)
for o in offs: out += b"%010d 00000 n \n" % o
out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF" % (len(objs)+1, xref)
open(sys.argv[1],"wb").write(out)
PYEOF
# minimal valid .docx (zip) carrying a unique phrase
"$PY" - "$FIX/doc.docx" <<'PYEOF'
import sys, zipfile
doc='<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>quokka-docx-marker</w:t></w:r></w:p></w:body></w:document>'
ct='<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>'
rel='<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>'
z=zipfile.ZipFile(sys.argv[1],"w",zipfile.ZIP_DEFLATED)
z.writestr("[Content_Types].xml",ct); z.writestr("_rels/.rels",rel); z.writestr("word/document.xml",doc); z.close()
PYEOF
mora connect filesystem "$FIX" --name regressdocs >/dev/null 2>&1 || die "connect filesystem failed"
mora index rebuild >/dev/null 2>&1 || die "index rebuild after fs connect failed"
for phrase in quokka-md-marker quokka-txt-marker quokka-pdf-marker quokka-docx-marker; do
  R="$(mora search "$phrase" --json)"
  json_true "$R" 'len(d) > 0' || die "filesystem connector did not index '$phrase'"
done
pass "filesystem + md + txt + PDF + DOCX extracted and indexed"

# =============================================================================
if [ "${SKIP_GIT:-0}" != "1" ]; then
section "Tier 1f — git sync to a local bare remote (no network)"
if command -v git >/dev/null 2>&1; then
  git init --bare "$WORK/remote.git" >/dev/null 2>&1
  mora sync git --init --remote "file://$WORK/remote.git" -m "regress backup" >/dev/null 2>&1 \
    || die "mora sync git failed"
  git --git-dir="$WORK/remote.git" log --oneline 2>/dev/null | grep -q "regress backup" \
    || die "git sync pushed nothing to the bare remote"
  git --git-dir="$WORK/remote.git" log -p 2>/dev/null | grep -Eiq 'ghp_[A-Za-z0-9]|github_pat_|x-access-token:' \
    && die "a PAT/token leaked into the synced commit" || true
  pass "git sync pushed to bare remote; no PAT leak"
else
  printf '  \033[33mnote\033[0m git not installed — skipping Tier 1f\n'
fi
fi

# =============================================================================
section "Tier 1 doctor gate — mora doctor --json --strict"
DOC_JSON="$(mora doctor --json)"
json_true "$DOC_JSON" 'd["healthy"] is True' || die "doctor reports unhealthy on a seeded vault: $DOC_JSON"
mora doctor --json --strict >/dev/null 2>&1 || die "doctor --strict exited non-zero on a healthy vault"
pass "doctor --json --strict: healthy"

# =============================================================================
section "Tier 1g — vault-identity guard (refuse empty/foreign rebuild, no network)"
# The vault-flip P0 (#43): rebuilding the index against a vault that suddenly looks
# EMPTY must REFUSE and leave the populated index intact, not silently wipe it. The
# real-upgrade test below exercises the legacy->v2 ADOPT path but is skipped without
# network; this synthetic check proves the empty-vault guard fires on the real HEAD
# binary EVERY run. It uses its own config dir so it can't disturb the main sandbox.
VIDCFG="$WORK/vid"; VIDVAULT="$VIDCFG/vault"
# build_vault.py wipes+inits its own sandbox (creating the v2 identity marker) and
# rebuilds the index, so the vault starts populated with a built, marked index.
run "$PY" "$MORA_REPO/scripts/bench/agent-ab/build_vault.py" \
  "$MORA_REPO/scripts/bench/agent-ab/world.json" "$VIDCFG"
VID_N="$(json_get "$(MORA_CONFIG_DIR="$VIDCFG" mora list --json)" 'len(d)')"
[ "$VID_N" -gt 0 ] || die "vid: seeded vault has 0 memories"
json_true "$(MORA_CONFIG_DIR="$VIDCFG" mora search Northwind --json)" 'len(d) > 0' \
  || die "vid: search found nothing on the seeded index (bad fixture)"
# Empty the vault (simulate vault_dir moving / being lost); the dotfile marker stays.
rm -rf "${VIDVAULT:?}"/* 2>/dev/null || true
# A plain rebuild must now be REFUSED (non-zero) — the guard's whole job.
if MORA_CONFIG_DIR="$VIDCFG" mora index rebuild >/dev/null 2>&1; then
  die "vid: rebuild from an EMPTIED vault was NOT refused (vault-flip guard regressed)"
fi
# ...and the populated index must survive: search still answers from the old index.
json_true "$(MORA_CONFIG_DIR="$VIDCFG" mora search Northwind --json)" 'len(d) > 0' \
  || die "vid: blocked rebuild wiped the index (expected it preserved untouched)"
pass "empty-vault rebuild refused; populated index preserved ($VID_N memories)"
# --force is the documented override: it must rebuild from the (now empty) vault.
MORA_CONFIG_DIR="$VIDCFG" mora index rebuild --force >/dev/null 2>&1 \
  || die "vid: --force did not override the guard"
pass "--force overrides the guard"

# =============================================================================
if [ "${SKIP_UPGRADE:-0}" != "1" ]; then
section "Tier 1b/upgrade — install $PREV_VER, populate, swap to HEAD in place"
if curl -fsI https://github.com >/dev/null 2>&1; then
  UPWORK="$WORK/upgrade"; mkdir -p "$UPWORK/bin"
  UP_CONFIG="$UPWORK/sandbox"
  # SAFETY GUARD: the PREVIOUS release may predate MORA_CONFIG_DIR and fall back
  # to the developer's REAL ~/.config/mora — silently repointing their live
  # vault_dir at a (doomed) sandbox path. The HOME/XDG redirect below is the fix;
  # this fingerprint of the WHOLE real ~/.config/mora (or ABSENT) is the tripwire,
  # asserted UNCONDITIONALLY after the test (catches both mutation of an existing
  # config AND creation of one where none existed — the CI/fresh-box case).
  CFG_BEFORE="$(cfg_fingerprint)"
  # 1) Fetch the PREVIOUS release tarball using the REAL $HOME (so gh/curl auth
  #    works), THEN local-install it under a redirected HOME below — only the
  #    config-writing `mora init` is sandboxed; the network step keeps real auth.
  OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
  AR="$(uname -m)"; case "$AR" in arm64|aarch64) AR=arm64 ;; x86_64|amd64) AR=amd64 ;; *) die "unsupported arch: $AR" ;; esac
  ASSET="mora_${PREV_VER}_${OS}_${AR}.tar.gz"
  OLDSTAGE="$UPWORK/oldstage"; mkdir -p "$OLDSTAGE"
  if command -v gh >/dev/null 2>&1; then
    gh release download "v$PREV_VER" --repo pyranthus-hq/mora --pattern "$ASSET" --dir "$UPWORK" >/dev/null 2>&1 || true
  fi
  # Validate any gh-downloaded archive: an interrupted gh leaves a truncated file
  # that would satisfy `[ -f ]` and skip the curl fallback, failing later at tar.
  [ -f "$UPWORK/$ASSET" ] && { tar -tzf "$UPWORK/$ASSET" >/dev/null 2>&1 || rm -f "$UPWORK/$ASSET"; }
  [ -f "$UPWORK/$ASSET" ] || curl -fsSL -o "$UPWORK/$ASSET" \
    "https://github.com/pyranthus-hq/mora/releases/download/v$PREV_VER/$ASSET" \
    || die "could not fetch the $PREV_VER tarball ($ASSET)"
  tar -xzf "$UPWORK/$ASSET" -C "$OLDSTAGE" || die "could not extract $ASSET"
  OLDBIN="$(find "$OLDSTAGE" -type f -name mora 2>/dev/null | head -1)"
  [ -x "$OLDBIN" ] || die "no mora binary in the $PREV_VER tarball"
  # the tarball may extract `mora` to the top level (== $OLDSTAGE/mora) or a
  # nested versioned dir; only copy when it isn't already the staging path
  # (cp onto itself errors under set -e).
  [ "$OLDBIN" = "$OLDSTAGE/mora" ] || cp "$OLDBIN" "$OLDSTAGE/mora"
  cp "$MORA_REPO/install.sh" "$OLDSTAGE/install.sh"
  upgrade_rc=0
  ( # Redirect HOME + XDG into the sandbox so the OLD binary's `mora init` — which
    # may predate MORA_CONFIG_DIR — writes to a throwaway HOME, never the real
    # ~/.config/mora (the vault-flip data hazard).
    export HOME="$UPWORK/home" \
           XDG_CONFIG_HOME="$UPWORK/home/.config" \
           XDG_DATA_HOME="$UPWORK/home/.local/share" \
           XDG_STATE_HOME="$UPWORK/home/.local/state" \
           XDG_CACHE_HOME="$UPWORK/home/.cache" \
           MORA_CONFIG_DIR="$UP_CONFIG" MORA_VAULT="$UP_CONFIG/vault"
    mkdir -p "$HOME/.config" "$HOME/.local/share" "$HOME/.local/state" "$HOME/.cache"
    # LOCAL-install the previous release (its mora binary sits next to install.sh,
    # so install.sh takes its local path — no network, no gh auth needed here).
    PREFIX="$UPWORK/bin" MORA_VAULT="$UP_CONFIG/vault" sh "$OLDSTAGE/install.sh" >/dev/null 2>&1 \
      || die "local install of previous version $PREV_VER failed"
    OLDMORA="$UPWORK/bin/mora"
    "$OLDMORA" version | head -1 | grep -q "$PREV_VER" || die "previous install is not $PREV_VER"
    # populate on the OLD binary
    "$PY" "$MORA_REPO/scripts/bench/agent-ab/build_vault.py" \
      "$MORA_REPO/scripts/bench/agent-ab/world.json" "$UP_CONFIG" >/dev/null 2>&1 \
      || die "seed on previous version failed"
    OLD_LIST="$(MORA_CONFIG_DIR="$UP_CONFIG" "$OLDMORA" list --json)"; OLD_COUNT="$(json_get "$OLD_LIST" 'len(d)')"
    # 2) upgrade IN PLACE by swapping the HEAD binary into the staging dir + reinstall
    cp "$MORA_BIN" "$STAGE/mora"
    PREFIX="$UPWORK/bin" MORA_VAULT="$UP_CONFIG/vault" sh "$STAGE/install.sh" >/dev/null 2>&1 \
      || die "in-place upgrade reinstall failed"
    "$OLDMORA" version | head -1 | grep -q "$EXPECTED_VER" || die "post-upgrade version not $EXPECTED_VER"
    # 3) assert old data survived + index auto-heals (static embedder floor)
    NEW_LIST="$(MORA_CONFIG_DIR="$UP_CONFIG" "$OLDMORA" list --json)"
    NEW_COUNT="$(json_get "$NEW_LIST" 'len(d)')"
    [ "$NEW_COUNT" -ge "$OLD_COUNT" ] || die "data loss on upgrade: $OLD_COUNT -> $NEW_COUNT"
    SR="$(MORA_CONFIG_DIR="$UP_CONFIG" "$OLDMORA" search Northwind --json)"
    json_true "$SR" 'len(d) > 0' || die "search broken after upgrade (index did not auto-heal)"
    MORA_CONFIG_DIR="$UP_CONFIG" "$OLDMORA" brief --json >/dev/null 2>&1 || die "brief crashed after upgrade (schema bump not handled)"
    MORA_CONFIG_DIR="$UP_CONFIG" "$OLDMORA" doctor --json --strict >/dev/null 2>&1 || die "doctor --strict failed after upgrade"
    pass "upgrade $PREV_VER -> $EXPECTED_VER: $OLD_COUNT memories survived, index auto-healed, brief+doctor OK"
  ) || upgrade_rc=$?
  # Tripwire — runs UNCONDITIONALLY (capturing the subshell's rc above instead of
  # letting set -e abort here): the upgrade test must NEVER mutate or create the
  # real ~/.config/mora, on success OR failure paths.
  CFG_AFTER="$(cfg_fingerprint)"
  [ "$CFG_BEFORE" = "$CFG_AFTER" ] \
    || die "the upgrade test touched your real ~/.config/mora (vault-flip hazard) — a previous-release binary escaped the sandbox redirect"
  pass "real ~/.config/mora untouched by the upgrade test"
  [ "$upgrade_rc" -eq 0 ] || die "upgrade test failed (rc=$upgrade_rc)"
else
  printf '  \033[33mnote\033[0m no GitHub access — skipping upgrade test (set SKIP_UPGRADE=1 to silence)\n'
fi
fi

section "ALL TIER-1 CHECKS PASSED for $EXPECTED_VER"
