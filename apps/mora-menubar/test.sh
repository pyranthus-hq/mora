#!/bin/sh
set -eu
cd "$(dirname "$0")"
export TMPDIR="${TMPDIR:-/tmp/mora-tentacles-ui}"
mkdir -p "$TMPDIR"
swiftc Sources/StatusData.swift Tests/main.swift -o "$TMPDIR/mora-menubar-status-tests"
"$TMPDIR/mora-menubar-status-tests"
