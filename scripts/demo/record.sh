#!/usr/bin/env bash
# record.sh — render the Mora demo to mora-demo.mp4 + mora-demo.gif.
#
# The recording runs on a throwaway SYNTHETIC vault seeded by `mora demo`, so no
# real account data ever appears on screen. Safe to run anywhere.
set -euo pipefail

cd "$(dirname "$0")"

missing=()
for tool in mora vhs ttyd ffmpeg; do
  command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
done
if [ "${#missing[@]}" -ne 0 ]; then
  echo "missing required tools: ${missing[*]}" >&2
  echo "install:  brew install charmbracelet/tap/vhs ttyd ffmpeg   (and build/install mora)" >&2
  exit 1
fi

echo "rendering scripts/demo/launch.tape (synthetic data only) ..."
vhs launch.tape
echo "done → $(pwd)/mora-demo.mp4 and mora-demo.gif"
echo
echo "Before posting: scrub the output frame-by-frame to confirm only synthetic"
echo "names (Sam Rivera / Priya Nair / Northwind / Project Halcyon) appear."
