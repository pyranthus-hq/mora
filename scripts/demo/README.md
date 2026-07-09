# Demo recording

Assets for recording a short, clean demo of Mora — on a **synthetic** vault, so no
real account data is ever on screen.

## What it shows

1. `mora brief --clean` — one digest across Calendar, Texts, Emails, and Files.
2. `mora graph "Priya Nair"` — one person resolved across email, texts, and calendar.
3. `ls .../vault/memories/...` — it's just plain Markdown on your machine.

All three run against the vault seeded by **`mora demo`** (protagonist *Sam Rivera*,
project *Halcyon*, client *Northwind*; contacts *Priya Nair* / *Marcus Vance*). The
data is fictional and the install is fully isolated under a temp dir.

## Try it without recording

```sh
export MORA_CONFIG_DIR=$(mktemp -d)/mora-demo
mora demo --dir "$MORA_CONFIG_DIR"
mora brief --clean
mora graph "Priya Nair"
```

Delete `$MORA_CONFIG_DIR` when you're done — it never touches your real vault.

## Record the terminal clip (VHS)

Requires [`vhs`](https://github.com/charmbracelet/vhs) plus `ttyd` and `ffmpeg`,
and `mora` on your `PATH`:

```sh
brew install charmbracelet/tap/vhs ttyd ffmpeg
./record.sh        # → mora-demo.mp4 + mora-demo.gif
```

`launch.tape` is the script; edit timings/theme there. The seed step runs
off-camera (`Hide`/`Show`), so the clip opens straight on the brief.

## Optional animated intro (concept card)

`concept-card.html` is a self-contained, dependency-free title card (four sources
converging into one local memory). Open it in a browser and screen-record the
1280×720 stage, or render it headless and stitch with `ffmpeg`. Use it as a
5–8s open before the terminal clip. For a fancier intro, the same beats animate
well in [Manim](https://www.manim.community/) or
[Motion Canvas](https://motioncanvas.io/) (both MIT) — keep the terminal clip as
the payoff either way.

## Before publishing

Scrub the rendered file frame-by-frame and confirm only the synthetic names
appear. Never record the live vault, even redacted.
