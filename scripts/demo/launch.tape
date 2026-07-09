# launch.tape — record the Mora demo as a clean terminal video/GIF.
#
# Runs entirely on a throwaway SYNTHETIC vault (`mora demo`), so there is no real
# PII on screen and the recording is reproducible. Render with:
#   vhs scripts/demo/launch.tape      (needs: vhs + ttyd + ffmpeg, and mora on PATH)
# or use scripts/demo/record.sh, which checks deps and seeds for you.

Output mora-demo.mp4
Output mora-demo.gif

Require mora

Set Shell "bash"
Set FontSize 22
Set Width 1280
Set Height 900
Set Padding 28
Set Theme "Catppuccin Mocha"
Set TypingSpeed 55ms
Set PlaybackSpeed 1.0

# --- off-camera setup: an isolated synthetic vault (no real accounts touched) ---
Hide
Type "export MORA_CONFIG_DIR=$(mktemp -d)/mora-demo" Enter
Type "mora demo --dir $MORA_CONFIG_DIR --quiet" Enter
Sleep 3s
Type "clear" Enter
Show

# 1. One command, every source: Calendar + Texts + Emails + Files in one digest.
Type "mora brief --clean" Sleep 800ms Enter
Sleep 6s

# 2. One person, every account: Priya resolved across email, texts, and calendar.
Type "mora graph 'Priya Nair'" Sleep 800ms Enter
Sleep 6s

# 3. It's just plain Markdown on your machine. Nothing left it.
Type "ls $MORA_CONFIG_DIR/vault/memories/personal" Sleep 600ms Enter
Sleep 3s

Hide
Type "clear" Enter
Show
Sleep 500ms
