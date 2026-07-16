#!/bin/sh
set -eu

# Gate 2 r7 witness-integrity driver. It deliberately fails when any of the 93
# authoritative matrix rows loses its exact named test (including decomposed
# subtests), then runs the full internal/mora suite once. Production-site mutant
# replay results are recorded separately in mutation-matrix-gate2.md; a green
# run here is necessary evidence, never sufficient by itself to close the gate.

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
MANIFEST="$ROOT/scripts/eval/gate2-witnesses.tsv"
MATRIX="$ROOT/mutation-matrix-gate2.md"
JSON=$(mktemp "${TMPDIR:-/tmp}/mora-gate2-witnesses.XXXXXX")
trap 'rm -f "$JSON"' EXIT HUP INT TERM

rows=$(awk 'NF && $1 !~ /^#/ { n++ } END { print n+0 }' "$MANIFEST")
[ "$rows" -eq 93 ] || {
  echo "Gate 2 witness manifest has $rows rows; want exactly 93" >&2
  exit 1
}

matrix_rows=$(awk -F '|' '
  function trim(s) { gsub(/^[[:space:]]+|[[:space:]]+$/, "", s); return s }
  {
    row = trim($2)
    status = trim($5)
    if (row ~ /^[0-9]+[a-z]?$/ && status ~ /^CLOSED([[:space:]]|$)/) n++
  }
  END { print n+0 }
' "$MATRIX")
[ "$matrix_rows" -eq 93 ] || {
  echo "Gate 2 mutation matrix has $matrix_rows CLOSED rows; want exactly 93" >&2
  exit 1
}

(cd "$ROOT" && go test -json ./internal/mora -count=1) >"$JSON"

missing=0
while IFS="$(printf '\t')" read -r row test_name; do
  [ -n "$row" ] || continue
  case "$row" in \#*) continue ;; esac
  matrix_matches=$(awk -F '|' -v want="$row" '
    function trim(s) { gsub(/^[[:space:]]+|[[:space:]]+$/, "", s); return s }
    trim($2) == want && trim($5) ~ /^CLOSED([[:space:]]|$)/ { n++ }
    END { print n+0 }
  ' "$MATRIX")
  if [ "$matrix_matches" -ne 1 ]; then
    echo "MATRIX row $row: found $matrix_matches CLOSED entries; want exactly one" >&2
    missing=$((missing + 1))
  elif ! grep -F '"Action":"pass"' "$JSON" | grep -Fq "\"Test\":\"$test_name\""; then
    echo "TEST row $row: $test_name did not pass" >&2
    missing=$((missing + 1))
  fi
done <"$MANIFEST"

[ "$missing" -eq 0 ] || {
  echo "Gate 2 witness integrity failed: $missing required row(s) absent" >&2
  exit 1
}

echo "Gate 2 witness integrity: all 93 authoritative rows present and green"
