#!/bin/sh
set -eu
export TMPDIR="${TMPDIR:-/tmp/mora-source-health-tmp}"
mkdir -p "$TMPDIR"
cd "$(dirname "$0")/../.."
APP_DIR="${APP_DIR:-$PWD/apps/mora-menubar/build/Mora Status.app}"
COLLECTOR="${1:-${MORA_TENTACLES:-}}"
MORA_BINARY="${2:-${MORA_BINARY:-}}"
if [ -z "$COLLECTOR" ] || [ ! -x "$COLLECTOR" ]; then
  echo "Usage: $0 /path/to/mora-tentacles /path/to/mora (or set MORA_TENTACLES and MORA_BINARY)" >&2
  exit 2
fi
if [ -z "$MORA_BINARY" ] || [ ! -x "$MORA_BINARY" ]; then
  echo "The bundled review window requires an executable Mora binary." >&2
  exit 2
fi
mkdir -p "$APP_DIR/Contents/MacOS"
mkdir -p "$APP_DIR/Contents/Resources"
ICON_WORK=$(mktemp -d "$TMPDIR/mora-icon.XXXXXX")
trap 'rm -rf "$ICON_WORK"' EXIT
swiftc apps/mora-menubar/Sources/MenuBarIcon.swift apps/mora-menubar/Tools/BuildIcon.swift -o "$ICON_WORK/build-icon"
"$ICON_WORK/build-icon" "$ICON_WORK/Mora.iconset"
iconutil -c icns "$ICON_WORK/Mora.iconset" -o "$APP_DIR/Contents/Resources/Mora.icns"
swiftc -O -target arm64-apple-macos13.0 -framework SwiftUI -framework AppKit \
  apps/mora-menubar/Sources/StatusData.swift apps/mora-menubar/Sources/ReviewReceipt.swift apps/mora-menubar/Sources/MenuBarIcon.swift apps/mora-menubar/Sources/ReviewWindow.swift apps/mora-menubar/Sources/MoraStatusApp.swift \
  -o "$APP_DIR/Contents/MacOS/Mora Status"
cp "$COLLECTOR" "$APP_DIR/Contents/MacOS/mora-tentacles"
cp "$MORA_BINARY" "$APP_DIR/Contents/MacOS/mora"
cp apps/mora-menubar/Fixtures/ReviewPreviewFixture.json "$APP_DIR/Contents/Resources/ReviewPreviewFixture.json"
cat > "$APP_DIR/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>Mora Status</string>
  <key>CFBundleDisplayName</key><string>Mora Status</string>
  <key>CFBundleIdentifier</key><string>com.pyranthus.mora-status</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSUIElement</key><true/>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>CFBundleExecutable</key><string>Mora Status</string>
  <key>CFBundleIconFile</key><string>Mora</string>
</dict></plist>
PLIST
echo "Built $APP_DIR"
