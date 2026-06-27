#!/usr/bin/env sh
# Mora installer — works two ways:
#
#   1) LOCAL (recommended for the demo): extract a release tarball, then run the
#      bundled install.sh. Installs the `mora` binary, clears the macOS Gatekeeper
#      quarantine + ad-hoc-signs it (the binary is unsigned), sets up your vault,
#      and prints next steps.
#
#        tar -xzf mora_0.9.0_darwin_arm64.tar.gz && ./install.sh
#
#   2) REMOTE: if no binary sits next to this script, it downloads the matching
#      asset for this machine from the public GitHub release — plain curl, no
#      auth needed (gh is used when present, but optional).
#
# Env knobs:
#   PREFIX=/usr/local/bin    install dir (default: first writable of
#                            /usr/local/bin, /opt/homebrew/bin, ~/.local/bin)
#   MORA_VAULT=~/vault/mora  vault location passed to `mora init`
#   VERSION=0.9.0            release tag for remote mode
#   REPO=pyranthus-hq/mora   source repo for remote mode
set -eu

VERSION="${VERSION:-0.9.0}"
REPO="${REPO:-pyranthus-hq/mora}"
VAULT="${MORA_VAULT:-$HOME/vault/mora}"
HERE="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# --- locate or fetch the binary -------------------------------------------------
BIN=""
if [ -x "$HERE/mora" ]; then
	BIN="$HERE/mora"
else
	OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
	case "$OS" in darwin|linux) : ;; *) die "unsupported OS: $OS" ;; esac
	ARCH="$(uname -m)"
	case "$ARCH" in arm64|aarch64) ARCH=arm64 ;; x86_64|amd64) ARCH=amd64 ;; *) die "unsupported arch: $ARCH" ;; esac
	ASSET="mora_${VERSION}_${OS}_${ARCH}.tar.gz"
	TMP="$(mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT
	say "Fetching $ASSET from $REPO@v$VERSION ..."
	if command -v gh >/dev/null 2>&1; then
		# gh is optional for a public repo; used when present for nicer errors.
		gh release download "v$VERSION" --repo "$REPO" --pattern "$ASSET" --dir "$TMP" \
			|| die "download failed — check your network, or download the release tarball from https://github.com/$REPO/releases"
	else
		curl -fsSL -o "$TMP/$ASSET" \
			"https://github.com/$REPO/releases/download/v$VERSION/$ASSET" \
			|| die "download failed — check your network, or grab the tarball from https://github.com/$REPO/releases"
	fi
	tar -xzf "$TMP/$ASSET" -C "$TMP"
	# Locate the binary regardless of tarball layout (top-level OR nested in a versioned dir).
	BIN="$(find "$TMP" -type f -name mora 2>/dev/null | head -n 1)"
	[ -n "$BIN" ] && [ -x "$BIN" ] || die "extracted archive has no mora binary (looked under $TMP)"
fi

# --- pick an install dir on PATH ------------------------------------------------
if [ -n "${PREFIX:-}" ]; then
	DEST="$PREFIX"
else
	DEST=""
	for d in /usr/local/bin /opt/homebrew/bin "$HOME/.local/bin"; do
		if [ -d "$d" ] && [ -w "$d" ]; then DEST="$d"; break; fi
	done
	[ -n "$DEST" ] || DEST="$HOME/.local/bin"
fi
mkdir -p "$DEST"

# --- install + clear Gatekeeper quarantine + ad-hoc sign ------------------------
# Clear quarantine and ad-hoc re-sign BEFORE running the binary, so a Mora that
# arrived via download/AirDrop/zip never trips the "cannot be opened because Apple
# cannot check it for malicious software" wall. Safe no-op on Linux.
cp "$BIN" "$DEST/mora"
chmod +x "$DEST/mora"
if [ "$(uname -s)" = "Darwin" ]; then
	xattr -d com.apple.quarantine "$DEST/mora" 2>/dev/null || true
	codesign --force --sign - "$DEST/mora" 2>/dev/null || true
fi

# --- initialize vault (idempotent) and report ----------------------------------
# stdin from /dev/null on purpose: an existing install whose config points at a
# DIFFERENT vault makes init refuse the repoint (data safety) — forcing non-TTY
# guarantees the deterministic refusal instead of an invisible confirm prompt
# behind the output capture. The refusal is surfaced (it names the real
# configured vault) and the banner then reports the vault actually in use.
if INIT_OUT="$("$DEST/mora" init --vault "$VAULT" </dev/null 2>&1)"; then
	:
else
	printf 'note: mora init did not run: %s\n' "$INIT_OUT" >&2
fi
ACTIVE_VAULT="$("$DEST/mora" config 2>/dev/null | sed -n 's/^vault_dir = //p' | head -1)"
[ -n "$ACTIVE_VAULT" ] || ACTIVE_VAULT="$VAULT"
VER="$("$DEST/mora" version 2>/dev/null | head -1 || echo mora)"

cat <<EOF

✓ Installed $VER
  binary:  $DEST/mora
  vault:   $ACTIVE_VAULT
EOF

case ":$PATH:" in
	*":$DEST:"*) : ;;
	*) printf '\nAdd to your PATH (then restart your shell):\n  echo '\''export PATH="%s:$PATH"'\'' >> ~/.zshrc\n' "$DEST" ;;
esac

cat <<'EOF'

Wire Mora into your agents ONCE (then every session has it):
  claude mcp add mora -s user -- mora mcp serve
  codex  mcp add mora -- mora mcp serve

Next steps (macOS + Linux unless noted):
  mora doctor                     # check readiness
  mora connect google             # Gmail + Calendar, read-only (OAuth)
  mora connect filesystem ~/notes # index a folder of docs/PDFs
  mora connect imessage           # macOS only: your local Messages (read-only)
  mora search "your text here"    # full-text search the vault

Then ask your agent, cold: "Search my memory — what did I last discuss with Neil?"
EOF
