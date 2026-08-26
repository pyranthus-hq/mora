#!/usr/bin/env sh
# Mora installer — works two ways:
#
#   1) LOCAL: extract a release tarball, then run the bundled install.sh. On
#      macOS, the installer verifies the official Developer ID signature and
#      Apple's notarized-code requirement without changing either the signature or
#      the quarantine attribute. It then sets up your vault and prints next steps.
#
#        tar -xzf mora_0.15.0_darwin_arm64.tar.gz && ./install.sh
#
#   2) REMOTE: if no binary sits next to this script, it downloads the matching
#      asset for this machine from the public GitHub release (plain curl, no auth;
#      gh is used when present, but optional) and verifies it against the release
#      checksums.txt before extracting.
#
# Env knobs:
#   PREFIX=/usr/local/bin    install dir (default: first writable of
#                            /usr/local/bin, /opt/homebrew/bin, ~/.local/bin)
#   MORA_VAULT=~/vault/mora  vault location passed to `mora init`
#   VERSION=0.15.0           release tag for remote mode
#   REPO=pyranthus-hq/mora   source repo for remote mode
set -eu

VERSION="${VERSION:-0.15.0}"
REPO="${REPO:-pyranthus-hq/mora}"
VAULT="${MORA_VAULT:-$HOME/vault/mora}"
HERE="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
MACOS_IDENTIFIER="com.pyranthus.mora"
MACOS_TEAM_ID="VS8M5VJBZ5"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# sha256_of <file> — print the file's SHA-256, portable across GNU coreutils
# (sha256sum) and BSD/macOS (shasum). Returns non-zero if neither tool exists.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
	else return 1; fi
}

# verify_macos_release <binary> — fail closed unless the executable carries
# Mora's stable Apple Developer ID identity and Apple accepts its notarization
# ticket. A raw executable cannot carry a stapled ticket, so the first assessment
# can need a network connection. Do not remove quarantine or re-sign: either
# action would bypass Gatekeeper or replace the identity that macOS uses for FDA.
verify_macos_release() {
	[ "$(uname -s)" = "Darwin" ] || return 0
	command -v codesign >/dev/null 2>&1 || die "codesign is required to verify the macOS release"
	command -v spctl >/dev/null 2>&1 || die "spctl is required to fetch the Apple notarization ticket"

	codesign --verify --strict --verbose=2 "$1" >/dev/null 2>&1 || \
		die "macOS release has an invalid code signature — refusing to install"
	SIGN_INFO="$(codesign -dvvv "$1" 2>&1)" || \
		die "could not inspect the macOS code signature — refusing to install"
	printf '%s\n' "$SIGN_INFO" | grep -Fqx "Identifier=$MACOS_IDENTIFIER" || \
		die "macOS release has the wrong signing identifier — expected $MACOS_IDENTIFIER"
	printf '%s\n' "$SIGN_INFO" | grep -Fqx "TeamIdentifier=$MACOS_TEAM_ID" || \
		die "macOS release has the wrong Apple team — expected $MACOS_TEAM_ID"
	printf '%s\n' "$SIGN_INFO" | grep -Fq 'Authority=Developer ID Application:' || \
		die "macOS release is not signed with a Developer ID Application certificate"
	printf '%s\n' "$SIGN_INFO" | grep -Eq '^CodeDirectory .*flags=.*\(runtime\)' || \
		die "macOS release does not enable the hardened runtime"
	printf '%s\n' "$SIGN_INFO" | grep -Eq '^Timestamp=.+' || \
		die "macOS release has no secure timestamp"
	REQUIREMENT_INFO="$(codesign -d -r- "$1" 2>&1)" || \
		die "could not inspect the macOS designated requirement"
	printf '%s\n' "$REQUIREMENT_INFO" | grep -Fq "identifier \"$MACOS_IDENTIFIER\"" || \
		die "macOS release designated requirement has the wrong identifier"
	printf '%s\n' "$REQUIREMENT_INFO" | \
		grep -Eq "subject\\.OU[^=]*= (\"${MACOS_TEAM_ID}\"|${MACOS_TEAM_ID})( |$)" || \
		die "macOS release designated requirement has the wrong Apple team"
	# Ask Gatekeeper to fetch the online ticket. Its execute assessment can return
	# "not app-like" for a valid raw CLI, so that result is deliberately not the
	# verdict. The special notarized requirement below is the fail-closed check.
	spctl --assess --type execute --verbose=4 "$1" >/dev/null 2>&1 || true
	codesign --verify --strict --verbose=2 -R='notarized' "$1" >/dev/null 2>&1 || \
		die "macOS release does not satisfy Apple's notarized code requirement — connect to the internet and retry"
}

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
	# Prefer gh when it is present AND authenticated; otherwise fall back to plain
	# curl. The repo is public, so curl needs no auth — a present-but-unauthenticated
	# gh must not block the install. USE_GH remembers the verdict so an unusable gh
	# isn't retried for checksums.txt below.
	USE_GH=0
	if command -v gh >/dev/null 2>&1 && \
		gh release download "v$VERSION" --repo "$REPO" --pattern "$ASSET" --dir "$TMP" >/dev/null 2>&1; then
		USE_GH=1
	else
		curl -fsSL -o "$TMP/$ASSET" \
			"https://github.com/$REPO/releases/download/v$VERSION/$ASSET" \
			|| die "download failed — check your network, or grab the tarball from https://github.com/$REPO/releases"
	fi

	# Verify the download against the release checksums BEFORE extracting or running
	# it. checksums.txt is published with every release (and cosign-signed); this
	# catches a corrupt, truncated, or swapped asset. It is not notarization and not
	# a check of this script itself — for the strongest chain, verify the cosign
	# signature by hand (see the release page) or `go install ...@latest` from source.
	if [ "$USE_GH" = 1 ]; then
		gh release download "v$VERSION" --repo "$REPO" --pattern checksums.txt --dir "$TMP" >/dev/null 2>&1 || true
	fi
	[ -f "$TMP/checksums.txt" ] || curl -fsSL -o "$TMP/checksums.txt" \
		"https://github.com/$REPO/releases/download/v$VERSION/checksums.txt" >/dev/null 2>&1 || \
		die "could not fetch checksums.txt — refusing to install an unverifiable download"
	[ -f "$TMP/checksums.txt" ] || \
		die "could not fetch checksums.txt — refusing to install an unverifiable download"
	# tr -d '\r' guards against a CRLF checksums.txt (awk would otherwise see the
	# filename as "name\r" and never match).
	WANT="$(tr -d '\r' < "$TMP/checksums.txt" | awk -v f="$ASSET" '$2 == f {print $1}')"
	[ -n "$WANT" ] || die "checksums.txt has no entry for $ASSET — refusing to install an unverifiable download"
	GOT="$(sha256_of "$TMP/$ASSET")" || \
		die "no SHA-256 tool found — install sha256sum or shasum and retry"
	[ "$GOT" = "$WANT" ] || die "CHECKSUM MISMATCH for $ASSET (expected $WANT, got $GOT) — refusing to install a tampered or corrupt download"
	say "✓ verified $ASSET against the release checksums"

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
	# An upgrade must replace the Mora this shell actually runs. Choosing the
	# first generally writable directory can install a second copy earlier or
	# later on PATH and leave the old FDA-bound executable active.
	ACTIVE_MORA="$(command -v mora 2>/dev/null || true)"
	ACTIVE_PATH=""
	if [ -n "$ACTIVE_MORA" ]; then
		case "$ACTIVE_MORA" in
			/*) ACTIVE_PATH="$ACTIVE_MORA" ;;
			*/*)
				ACTIVE_DIR="$(CDPATH='' cd -- "$(dirname -- "$ACTIVE_MORA")" 2>/dev/null && pwd -P)" || \
					die "could not resolve the active Mora path: $ACTIVE_MORA"
				ACTIVE_PATH="$ACTIVE_DIR/$(basename -- "$ACTIVE_MORA")"
				;;
		esac
	fi
	if [ -n "$ACTIVE_PATH" ] && [ -e "$ACTIVE_PATH" ]; then
		case "$ACTIVE_PATH" in
			*/Cellar/*|*/Caskroom/*)
				die "active Mora is managed by Homebrew at $ACTIVE_PATH — use brew upgrade pyranthus-hq/tap/mora"
				;;
		esac
		[ ! -L "$ACTIVE_PATH" ] || \
			die "active Mora is a symlink at $ACTIVE_PATH — use its package manager or set PREFIX explicitly"
		ACTIVE_DIR="$(CDPATH='' cd -- "$(dirname -- "$ACTIVE_PATH")" 2>/dev/null && pwd -P)" || \
			die "could not resolve the active Mora directory: $ACTIVE_PATH"
		[ -w "$ACTIVE_DIR" ] || \
			die "active Mora directory is not writable: $ACTIVE_DIR — set PREFIX or fix its permissions"
		DEST="$ACTIVE_DIR"
		say "Updating active Mora at $ACTIVE_PATH ..."
	fi
	if [ -z "$DEST" ]; then
		for d in /usr/local/bin /opt/homebrew/bin "$HOME/.local/bin"; do
			# Never replace a package-manager or app-owned symlink implicitly.
			[ -L "$d/mora" ] && continue
			if [ -d "$d" ] && [ -w "$d" ]; then DEST="$d"; break; fi
		done
	fi
	[ -n "$DEST" ] || DEST="$HOME/.local/bin"
fi
mkdir -p "$DEST"

# --- verify, stage, and atomically install without mutating the signed bytes -----
verify_macos_release "$BIN"
INSTALL_TMP="$(mktemp "$DEST/.mora.install.XXXXXX")" || \
	die "could not create a staged install in $DEST"
if ! cp "$BIN" "$INSTALL_TMP" || ! chmod +x "$INSTALL_TMP"; then
	rm -f "$INSTALL_TMP"
	die "could not stage the Mora binary in $DEST"
fi
if [ "$(uname -s)" = "Darwin" ]; then
	# Copying or chmod must not have damaged the embedded signature. Keep the
	# quarantine attribute in place so Gatekeeper evaluates the notarized binary.
	verify_macos_release "$INSTALL_TMP"
	PREPARED_CDHASH="$(codesign -dvvv "$INSTALL_TMP" 2>&1 | sed -n 's/^CDHash=//p')"
	[ -n "$PREPARED_CDHASH" ] || {
		rm -f "$INSTALL_TMP"
		die "could not read the staged Mora code-directory hash"
	}
	if ! mv -f "$INSTALL_TMP" "$DEST/mora"; then
		rm -f "$INSTALL_TMP"
		die "could not atomically install Mora into $DEST"
	fi
	INSTALLED_CDHASH="$(codesign -dvvv "$DEST/mora" 2>&1 | sed -n 's/^CDHash=//p')"
	[ "$INSTALLED_CDHASH" = "$PREPARED_CDHASH" ] || \
		die "installed Mora code-directory hash changed during installation"
	verify_macos_release "$DEST/mora"
	say "✓ verified notarized Developer ID release ($MACOS_IDENTIFIER, team $MACOS_TEAM_ID)"
else
	if ! mv -f "$INSTALL_TMP" "$DEST/mora"; then
		rm -f "$INSTALL_TMP"
		die "could not atomically install Mora into $DEST"
	fi
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

# $PATH is deliberately emitted literally for the user's shell.
# shellcheck disable=SC2016
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
