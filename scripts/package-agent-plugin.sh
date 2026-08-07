#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-dev}
OUT=${2:-"$ROOT/dist/mora-agent-plugin-${VERSION#v}.tar.gz"}

case "$VERSION" in
  *[!0-9A-Za-z.+-]*|'')
    echo "invalid plugin version: $VERSION" >&2
    exit 2
    ;;
esac

STAGE=$(mktemp -d "${TMPDIR:-/tmp}/mora-agent-plugin.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/plugin" "$(dirname -- "$OUT")"
cp -R "$ROOT/plugins/mora/." "$STAGE/plugin/"

if find "$STAGE/plugin" -type l -print -quit | grep -q .; then
  echo "agent plugin package contains a symlink; refusing an escaping archive" >&2
  exit 1
fi

python3 - "$STAGE/plugin/plugin.json" "${VERSION#v}" <<'PY'
import json
import pathlib
import sys
path = pathlib.Path(sys.argv[1])
manifest = json.loads(path.read_text())
if sys.argv[2] != "dev":
    manifest["version"] = sys.argv[2]
path.write_text(json.dumps(manifest, indent=2) + "\n")
PY

for required in plugin.json mcp.json LICENSE README.md; do
  test -f "$STAGE/plugin/$required" || {
    echo "agent plugin package is missing $required" >&2
    exit 1
  }
done

tmp_archive="$OUT.tmp.$$"
trap 'rm -rf "$STAGE"; rm -f "$tmp_archive"' EXIT
COPYFILE_DISABLE=1 tar -czf "$tmp_archive" -C "$STAGE/plugin" .
mv "$tmp_archive" "$OUT"
printf '%s\n' "$OUT"
