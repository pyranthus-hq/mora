#!/usr/bin/env bash
# Rewrite the committed shard weights from the per-test times a full CI run on
# main recorded (artifacts test-timings-<os>-<shard>). The weights only balance
# shards; coverage never depends on them. Usage:
#   scripts/ci/refresh-test-timings.sh <ci-run-id>
set -euo pipefail
run_id=${1:?usage: scripts/ci/refresh-test-timings.sh <ci-run-id>}
cd "$(git rev-parse --show-toplevel)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
gh run download "$run_id" --pattern 'test-timings-*' --dir "$tmp"
for set in linux-race windows; do
  mapfile -t files < <(find "$tmp" -name "$set-*.tsv" | sort)
  [ "${#files[@]}" -gt 0 ] || continue
  {
    echo "# Per-test seconds for scripts/ci/gotest_shard.py, recorded by CI run $run_id."
    sort "${files[@]}"
  } > "scripts/ci/test-timings/$set.tsv"
  echo "wrote scripts/ci/test-timings/$set.tsv from ${#files[@]} shard(s)"
done
