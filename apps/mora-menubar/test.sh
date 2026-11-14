#!/bin/sh
set -eu
cd "$(dirname "$0")"
export TMPDIR="${TMPDIR:-/tmp/mora-source-health-tmp}"
mkdir -p "$TMPDIR"
swiftc Sources/StatusData.swift Sources/ReviewReceipt.swift Tests/main.swift -o "$TMPDIR/mora-menubar-status-tests"
"$TMPDIR/mora-menubar-status-tests"
swiftc Sources/MenuBarIcon.swift Tests/MenuBarIconTests.swift -o "$TMPDIR/mora-menubar-icon-tests"
"$TMPDIR/mora-menubar-icon-tests"
grep -Fq '@State private var expandedSources = false' Sources/MoraStatusApp.swift
grep -Fq '.frame(width: 320)' Sources/MoraStatusApp.swift
echo "Compact collapsed panel checks passed"
