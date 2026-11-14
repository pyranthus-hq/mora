# Mora Status

Standalone native macOS 13+ menu-bar companion. It probes through the bundled
`mora-tentacles` collector and reports connection, source, sync, and index
health without changing any of them.

The menu-bar panel opens with compact device rows and expandable channel and
source receipts. Its overall status and icon reflect source health and the
local search index; a dirty index is labelled stale because its last committed
snapshot may not include current source data. It starts a collector check
immediately, refreshes every 60 seconds, and retains the last good result when
a refresh fails.

When the collector reports pending review counts, its Status entry opens a
separate read-only review window. That window uses the bundled `mora` binary
and only these public commands:

```sh
mora review --list --json
mora review --show saved ID --json
mora review --show held ID --json
```

It renders the bounded canonical preview as plain text, including the canonical
revision and display digest. The window distinguishes an empty queue, loading,
unavailable binary, command failure, and incomplete preview. Index visibility
is not part of this read-only receipt, so it does not claim that a note is
already searchable.

Keep and Discard are disabled. This unsigned app has no released, enrolled
trusted-verifier decision protocol; use `mora review` in the owner’s terminal
to make a decision. A redacted, truncated, escaped, or omitted-argument preview
also omits Keep. No UI or fixture path invokes `review-act` or writes the vault.

## Build

Requires macOS Swift command-line tools, `iconutil`, an executable collector,
and an executable Mora binary. The build is local and unsigned; it does not
install or launch the app.

```sh
./apps/mora-menubar/build.sh ./path/to/mora-tentacles ./path/to/mora
```

Or set `MORA_TENTACLES` and `MORA_BINARY`. The bundle is written to
`apps/mora-menubar/build/Mora Status.app` by default and contains both binaries
plus the clearly labelled synthetic preview fixture.

Open the bundle to run it as a menu-bar utility. The collector is stopped after
30 seconds; the Details view retains its latest raw response or error.

## Preview and checks

`--preview` loads only that bundled fixture; it does not invoke Mora, the
collector, or a vault.

```sh
"apps/mora-menubar/build/Mora Status.app/Contents/MacOS/Mora Status" --preview
./apps/mora-menubar/test.sh
```

The automated checks cover the status model and the versioned queue/preview
receipt decoders. Actual installed-app screenshots and OS-owner gestures remain
separate human acceptance work.
