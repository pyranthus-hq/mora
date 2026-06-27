#!/usr/bin/env bash
# Driver for the Mora regression harness.
#
#   ./build.sh native   # build a stamped binary on this host, run regression.sh natively
#   ./build.sh docker   # build a clean Linux image and run regression.sh inside it
#   ./build.sh macos    # run the macOS-native tier (regression-macos.sh) on this Mac
#
# `native` is for fast dev validation of the cross-platform Tier 1 (works on macOS,
# but the macOS-only data paths are covered by `macos`). `docker` is the real
# release shape: a from-scratch Debian box that has never seen Go or a prior mora
# install. `macos` exercises the surfaces a container can't: codesign/Gatekeeper,
# launchd, iMessage/Calendar decode, and the cp-over-running SIGKILL-137 hazard.
set -euo pipefail

HERE="$(cd -- "$(dirname -- "$0")" && pwd)"
MODE="${1:-native}"
# build.sh lives at scripts/regress/, so the repo root is two levels up.
MORA_REPO="${MORA_REPO:-$(cd "$HERE/../.." 2>/dev/null && pwd || true)}"
[ -n "$MORA_REPO" ] && [ -f "$MORA_REPO/go.mod" ] || {
  echo "FATAL: set MORA_REPO=/path/to/mora (could not auto-locate the clone)"; exit 2; }

VER="$(git -C "$MORA_REPO" describe --tags --always --dirty 2>/dev/null | sed 's/^v//')"
SHA="$(git -C "$MORA_REPO" rev-parse --short HEAD 2>/dev/null || echo local)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
[ -n "$VER" ] || VER="0.0.0-regress"
LDFLAGS="-s -w -X main.version=$VER -X main.commit=$SHA -X main.date=$DATE"

case "$MODE" in
  native)
    echo ">> native build @ $VER ($SHA)"
    WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
    ( cd "$MORA_REPO" && CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$WORK/mora" ./cmd/mora )
    echo ">> running regression.sh natively"
    MORA_REPO="$MORA_REPO" MORA_BIN="$WORK/mora" EXPECTED_VER="$VER" \
      bash "$HERE/regression.sh"
    ;;
  docker)
    command -v docker >/dev/null 2>&1 || { echo "FATAL: docker not installed"; exit 2; }
    docker info >/dev/null 2>&1 || { echo "FATAL: docker daemon not running (start Docker Desktop)"; exit 2; }
    echo ">> assembling build context"
    STAGE="$(mktemp -d)"; trap 'rm -rf "$STAGE"' EXIT
    if command -v rsync >/dev/null 2>&1; then
      rsync -a --exclude '.git' --exclude 'dist' --exclude 'node_modules' --exclude '/mora' "$MORA_REPO/" "$STAGE/repo/"
    else
      mkdir -p "$STAGE/repo"; (cd "$MORA_REPO" && git archive HEAD | tar -x -C "$STAGE/repo")
    fi
    cp "$HERE/regression.sh" "$STAGE/regression.sh"
    cp "$HERE/Dockerfile" "$STAGE/Dockerfile"
    echo ">> docker build @ $VER"
    docker build --build-arg VER="$VER" --build-arg SHA="$SHA" -t mora-regress "$STAGE"
    echo ">> docker run (clean Linux box, tmpfs sandbox)"
    # /work must be exec — the harness installs and runs the mora binary there,
    # and Docker's default --tmpfs is noexec (would fail with Permission denied
    # and silently flip install.sh into remote mode).
    docker run --rm --tmpfs /work:exec mora-regress
    ;;
  macos)
    [ "$(uname -s)" = "Darwin" ] || { echo "FATAL: macos mode requires macOS"; exit 2; }
    echo ">> Tier 2 (macOS-native) @ $VER ($SHA)"
    MORA_REPO="$MORA_REPO" EXPECTED_VER="$VER" bash "$HERE/regression-macos.sh"
    ;;
  *)
    echo "usage: $0 [native|docker|macos]"; exit 2;;
esac
