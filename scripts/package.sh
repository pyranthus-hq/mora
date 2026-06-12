#!/usr/bin/env sh
set -eu

VERSION="${VERSION:-0.5.0}"
GOOS="${GOOS:-darwin}"
GOARCH="${GOARCH:-arm64}"
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
DIST="$ROOT/dist"
WORK="$DIST/mora_${VERSION}_${GOOS}_${GOARCH}"

rm -rf "$WORK"
mkdir -p "$WORK/examples"

cd "$ROOT"

# Embed the real Google OAuth client at build time so shipped binaries authorize
# without the user creating their own Console credentials. internal/google/client.json
# is a committed NON-SECRET placeholder (DEV_PLACEHOLDER); when MORA_GOOGLE_CREDENTIALS
# points at a real installed-app client JSON we swap it in for the //go:embed, then
# ALWAYS restore the placeholder (trap) so the real creds are never left in the tree
# or committed. Without it, the build ships the placeholder and `connect google` fails.
# ABSOLUTE path — the trap fires at EXIT, by which point the script has cd'd into
# $DIST; a relative path would restore from the wrong directory and silently fail.
EMBED="$ROOT/internal/google/client.json"
if [ -n "${MORA_GOOGLE_CREDENTIALS:-}" ] && [ -f "$MORA_GOOGLE_CREDENTIALS" ]; then
	cp "$EMBED" "$EMBED.placeholder.bak"
	trap 'mv -f "$EMBED.placeholder.bak" "$EMBED" 2>/dev/null || true' EXIT INT TERM
	cp "$MORA_GOOGLE_CREDENTIALS" "$EMBED"
	echo "package.sh: embedded real OAuth client from MORA_GOOGLE_CREDENTIALS" >&2
else
	echo "package.sh: WARNING — MORA_GOOGLE_CREDENTIALS unset; shipping placeholder client (connect google will need BYO creds)" >&2
fi

# Stamp the version (mirrors goreleaser's -X targets: main.version/commit/date →
# mora.BuildVersion). REQUIRED: `mora upgrade` REFUSES an unstamped "dev" build
# (upgrade.go), so a hand-built tarball that ships as "dev" can never self-update —
# this is exactly why the earlier Neil zip was stuck. -trimpath + CGO_ENABLED=0 match
# the release builds (pure Go, reproducible, clean cross-compile).
COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}"
CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags "$LDFLAGS" -o "$WORK/mora" ./cmd/mora

# Guard: if we were given real creds, assert the built binary actually EMBEDS the
# real client id. (We can't just grep for absence of DEV_PLACEHOLDER — that string
# is a detection constant in oauth.go, compiled into every binary, so it always
# matches.) The trap still restores the tree on a failed exit.
if [ -n "${MORA_GOOGLE_CREDENTIALS:-}" ] && [ -f "$MORA_GOOGLE_CREDENTIALS" ]; then
	REAL_ID=$(grep -oE '[0-9A-Za-z_-]+\.apps\.googleusercontent\.com' "$MORA_GOOGLE_CREDENTIALS" | head -1)
	if [ -z "$REAL_ID" ] || ! strings "$WORK/mora" | grep -qF "$REAL_ID"; then
		echo "package.sh: FATAL — built binary does not embed the real OAuth client id from MORA_GOOGLE_CREDENTIALS" >&2
		exit 1
	fi
fi

cp README.md install.sh "$WORK/"
cp examples/claude-code-mcp.json examples/codex-mcp.json "$WORK/examples/"

cd "$DIST"
tar -czf "mora_${VERSION}_${GOOS}_${GOARCH}.tar.gz" "mora_${VERSION}_${GOOS}_${GOARCH}"
# checksums.txt is the UPGRADE CONTRACT: `mora upgrade` (upgrade.go's
# ChecksumValidator) refuses any release whose checksum asset is not named
# exactly "checksums.txt" — v0.6.0 shipped only SHA256SUMS and broke every
# install's self-update. Emit both: checksums.txt for the validator (matches
# goreleaser's checksum.name_template), SHA256SUMS for humans/scripts already
# pointing at it.
shasum -a 256 *.tar.gz > SHA256SUMS
cp SHA256SUMS checksums.txt

echo "$DIST/mora_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
