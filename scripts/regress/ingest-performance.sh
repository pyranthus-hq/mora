#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel)
# Fixed known-good main, not a moving branch: cumulative regressions must not
# become the next run's baseline. See docs/benchmarks/ingest-2026-08-24.md.
reference=941ae612df6466f0cd9c3673d36da5c0604106c2
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
mkdir "$scratch/reference"
git -C "$root" archive "$reference" | tar -x -C "$scratch/reference"
# Use identical corpus/measurement code for both production implementations.
cp "$root/internal/memory/ingest_benchmark_test.go" "$scratch/reference/internal/memory/ingest_benchmark_test.go"
(
  cd "$scratch/reference"
  go test -c -o "$scratch/reference.test" ./internal/memory
)
printf 'Ingest reference: %s; toolchain: %s\n' "$reference" "$(go version)"
cd "$root"
MORA_ENFORCE_INGEST_BENCH=1 MORA_INGEST_REFERENCE_BINARY="$scratch/reference.test" \
  go test ./internal/memory -run 'TestIngestReferenceBenchmarkRegression|TestIncremental1000RecordsUnder500MB|TestIngestNoOpUnderTwoSeconds' -count=1 -v
