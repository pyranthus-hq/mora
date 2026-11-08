# Mora Status

Standalone native macOS 13+ menu bar companion. It probes through the bundled
`mora-tentacles` collector and displays connection reachability separately from
each sync channel's reported state. It never changes sync state.

The menu bar keeps Mora's pixel-eye mascot and name visible in every state.
Failed or overdue checks add a warning badge; full status appears inside the panel. The build reuses the
existing Go icon generator and `docs/assets/mora-eye.svg` for all app branding.

## Build

From the repository root, build after the collector is available:

Requires the Go toolchain and macOS Swift command-line tools.

```sh
./apps/mora-menubar/build.sh ./path/to/mora-tentacles
```

The result is `apps/mora-menubar/build/Mora Status.app`. You can also pass the
collector path through `MORA_TENTACLES=/path/to/mora-tentacles`. The build is
local and unsigned; it does not install or launch the app.

## Run

Open `apps/mora-menubar/build/Mora Status.app`. The app runs as a menu bar
utility, starts a collector check immediately, and refreshes every 60 seconds.
The collector is stopped after 30 seconds. Open details shows the latest raw
collector response or error. A failed refresh keeps the last good result marked
as previous data.

For a deterministic UI preview, run the app executable with `--preview`:

```sh
"apps/mora-menubar/build/Mora Status.app/Contents/MacOS/Mora Status" --preview
```

Preview mode uses clearly labelled fixture data and does not run the collector.

## Checks

```sh
./apps/mora-menubar/test.sh
```

This exercises failed, stale, unknown, malformed, and independent connection
states using the same Foundation parser as the app.
