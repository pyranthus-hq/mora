#!/usr/bin/env sh
# Mora installer — works two ways:
#
#   1) LOCAL (recommended for the demo): extract a release tarball, then run the
#      bundled install.sh. Installs the `mora` binary, clears the macOS Gatekeeper
#      quarantine + ad-hoc-signs it (the binary is unsigned), sets up your vault,
#      and prints next steps.
#
#        tar -xzf mora_0.6.0_darwin_arm64.tar.gz && ./install.sh
#
#   2) REMOTE: if no binary sits next to this script, it downloads the matching
#      asset for this machine from the public GitHub release — plain curl, no
#      auth needed (gh is used when present, but optional).
#
# Env knobs:
#   PREFIX=/usr/local/bin    install dir (default: first writable of
#                            /usr/local/bin, /opt/homebrew/bin, ~/.local/bin)
#   MORA_VAULT=~/vault/mora  vault location passed to `mora init`
#   VERSION=0.6.0            release tag for remote mode
#   REPO=pyranthus-hq/mora   source repo for remote mode
set -eu

VERSION="${VERSION:-0.6.0}"
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
	BIN="$TMP/mora"
	[ -x "$BIN" ] || die "extracted archive has no mora binary"
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
	xattr -dr com.apple.quarantine "$DEST/mora" 2>/dev/null || true
	codesign --force --sign - "$DEST/mora" 2>/dev/null || true
fi

# --- initialize vault (idempotent) and report ----------------------------------
"$DEST/mora" init --vault "$VAULT" >/dev/null 2>&1 || true
VER="$("$DEST/mora" version 2>/dev/null | head -1 || echo mora)"

cat <<EOF

✓ Installed $VER
  binary:  $DEST/mora
  vault:   $VAULT
EOF

case ":$PATH:" in
	*":$DEST:"*) : ;;
	*) printf '\nAdd to your PATH (then restart your shell):\n  echo '\''export PATH="%s:$PATH"'\'' >> ~/.zshrc\n' "$DEST" ;;
esac

cat <<'EOF'

Wire Mora into your agents ONCE (then every session has it):
  claude mcp add mora -s user -- mora mcp serve
  codex  mcp add mora -- mora mcp serve

Next steps:
  mora doctor                     # check readiness (incl. iMessage Full Disk Access)
  mora connectors enable imessage # macOS: read your local Messages (read-only)
  mora sync imessage              # ingest conversations into the vault
  mora search "your text here"    # full-text search the vault

Then ask your agent, cold: "Search my memory — what did I last discuss with Neil?"
EOF
