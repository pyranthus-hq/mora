# Mora Status

Standalone native macOS 13+ menu bar companion. It probes through the bundled
`mora-tentacles` collector and displays connection reachability separately from
each sync channel's reported state. It never changes sync state.

The menu bar, panel, and app icon share a monochrome eye-and-tentacles mark.
Its native 20×18pt menu image keeps a fixed footprint, including its attention dot.
The 320pt panel opens with one compact row per device. Select a row to expand
channel receipts and explanations; only one device expands at a time. A separate
Sources row stays collapsed by default and expands to show enabled connectors,
freshness, last recorded source sync, and health detail. Device rows keep their own
aggregate; the overall status and menu icon also reflect source health and the local
search index. A dirty index is labelled stale because its last committed snapshot may
not include current source data. Details retains the collector status response.

## Build

From the repository root, build after the collector is available:

Requires macOS Swift command-line tools and iconutil. The bundle icon is rendered from the same vector drawing as the menu mark.

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

When Mora 0.16.0 or later reports agent notes waiting for the owner, the panel
shows one row under the headline, such as "3 agent notes to review, 2 held",
with a hint to run `mora review` in a terminal. The count is information only:
it does not change the health state or the icon, and no row appears when the
counts are zero or the doctor report is unavailable.

Preview mode uses clearly labelled fixture data and does not run the collector.

## Checks

```sh
./apps/mora-menubar/test.sh
```

This exercises failed, stale, unknown, malformed, and independent connection
states, source summaries, unknown source states, timestamps, and unavailable probes
using the same Foundation parser as the app.
