#!/usr/bin/env sh
# Cross-build mora release archives locally (mirrors .goreleaser.yaml so a hand-cut
# release matches what CI will later produce). Pure Go, CGO_ENABLED=0, binary at
# archive root so go-selfupdate + the Homebrew cask resolve it cleanly.
#
#   VERSION=0.2.0 sh scripts/build-release.sh
#
# Output: dist/release/mora_<version>_<os>_<arch>.tar.gz (zip on Windows) + checksums.txt
set -eu

VERSION="${VERSION:-0.0.0-dev}"
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
OUT="$ROOT/dist/release"
COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}"

rm -rf "$OUT"
mkdir -p "$OUT"
cd "$ROOT"

# darwin first (Neil's Mac); linux for completeness / single-binary thesis proof;
# windows/amd64 is the v1 Windows target.
for pair in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do
  GOOS="${pair%/*}"
  GOARCH="${pair#*/}"
  name="mora_${VERSION}_${GOOS}_${GOARCH}"
  stage="$OUT/$name"
  mkdir -p "$stage"
  echo "building $name ..."
  bin="mora"
  [ "$GOOS" = "windows" ] && bin="mora.exe"
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$stage/$bin" ./cmd/mora
  cp LICENSE README.md install.sh install-app.sh uninstall-app.sh "$stage/"
  chmod +x "$stage/install.sh" "$stage/install-app.sh" "$stage/uninstall-app.sh"
  mkdir -p "$stage/examples" "$stage/docs"
  cp examples/claude-code-mcp.json examples/codex-mcp.json "$stage/examples/"
  cp docs/guide.md "$stage/docs/"
  # binary at archive root (GoReleaser default; go-selfupdate-friendly)
  if [ "$GOOS" = "windows" ]; then
    (cd "$stage" && zip -qr "$OUT/${name}.zip" "$bin" LICENSE README.md install.sh install-app.sh uninstall-app.sh examples docs)
  else
    tar -C "$stage" -czf "$OUT/${name}.tar.gz" "$bin" LICENSE README.md install.sh install-app.sh uninstall-app.sh examples docs
  fi
  rm -rf "$stage"
done

cd "$OUT"
shasum -a 256 ./*.tar.gz ./*.zip | sed 's#\./##' > checksums.txt
echo
echo "Artifacts in $OUT:"
ls -1 "$OUT"
echo
echo "checksums.txt:"
cat checksums.txt
