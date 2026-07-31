#!/usr/bin/env bash
# Assemble one architecture-specific Mora.app bundle from an already-signed
# release CLI binary and a generated Mora.icns. Assembly only: no signing, no
# network, no Apple tooling beyond plist/arch validation. The bundle identity
# contract is FROZEN here — CFBundleIdentifier com.pyranthus.mora, bundle name
# Mora, executable mora, icon Mora.icns — because macOS TCC/FDA continuity
# depends on it never drifting.
set -euo pipefail

usage() {
	echo "usage: assemble-darwin-app.sh <signed-mora-binary> <Mora.icns> <version> <amd64|arm64> <output-dir>" >&2
	exit 2
}

binary="${1:-}"
icns="${2:-}"
version="${3:-}"
arch="${4:-}"
out_dir="${5:-}"
[[ -n "$binary" && -n "$icns" && -n "$version" && -n "$arch" && -n "$out_dir" ]] || usage

[[ -f "$binary" && -x "$binary" ]] || {
	echo "error: signed mora binary does not exist or is not executable: $binary" >&2
	exit 1
}
[[ -f "$icns" ]] || {
	echo "error: Mora.icns does not exist: $icns" >&2
	exit 1
}
if [[ "$(head -c 4 "$icns")" != "icns" ]]; then
	echo "error: $icns is not an ICNS file (bad magic)" >&2
	exit 1
fi

# Apple requires CFBundleVersion to be period-separated numeric components.
# This first app lane intentionally ships only stable X.Y.Z tags; fail closed
# instead of stamping a prerelease suffix that Apple will reject.
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "error: version must be a stable numeric release (X.Y.Z), got: $version" >&2
	exit 1
fi
short_version="$version"

case "$arch" in
	amd64) macho_arch="x86_64" ;;
	arm64) macho_arch="arm64" ;;
	*)
		echo "error: arch must be amd64 or arm64, got: $arch" >&2
		exit 1
		;;
esac

# The binary inside the bundle must be a thin Mach-O of exactly the advertised
# architecture — a wrong-arch or universal binary is a packaging error.
archs="$(lipo -archs "$binary" 2>/dev/null || true)"
if [[ "$archs" != "$macho_arch" ]]; then
	echo "error: $binary architecture is '${archs:-unreadable}', expected exactly '$macho_arch'" >&2
	exit 1
fi

app="$out_dir/Mora.app"
if [[ -e "$app" ]]; then
	echo "error: refusing to overwrite existing bundle: $app" >&2
	exit 1
fi

mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$binary" "$app/Contents/MacOS/mora"
chmod 0755 "$app/Contents/MacOS/mora"
cp "$icns" "$app/Contents/Resources/Mora.icns"
chmod 0644 "$app/Contents/Resources/Mora.icns"

# LSUIElement: the executable is a CLI; opening the app from Finder must not
# bounce a Dock icon or leave a phantom foreground app.
cat >"$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key>
	<string>en</string>
	<key>CFBundleDisplayName</key>
	<string>Mora</string>
	<key>CFBundleExecutable</key>
	<string>mora</string>
	<key>CFBundleIconFile</key>
	<string>Mora</string>
	<key>CFBundleIdentifier</key>
	<string>com.pyranthus.mora</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>CFBundleName</key>
	<string>Mora</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>${short_version}</string>
	<key>CFBundleVersion</key>
	<string>${version}</string>
	<key>LSMinimumSystemVersion</key>
	<string>11.0</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
	<key>NSHumanReadableCopyright</key>
	<string>Copyright AdiSam Consulting LLC. Apache-2.0.</string>
</dict>
</plist>
PLIST
chmod 0644 "$app/Contents/Info.plist"

plutil -lint "$app/Contents/Info.plist" >/dev/null || {
	echo "error: generated Info.plist does not lint" >&2
	exit 1
}

# Deterministic assembly: with SOURCE_DATE_EPOCH set (the release pipeline
# passes the commit timestamp), every file and directory carries the same
# mtime, so identical inputs produce an identical unsigned bundle.
if [[ -n "${SOURCE_DATE_EPOCH:-}" ]]; then
	stamp="$(date -u -r "$SOURCE_DATE_EPOCH" +%Y%m%d%H%M.%S)"
	TZ=UTC find "$app" -exec touch -t "$stamp" {} +
fi

echo "assembled: $app (version $version, $arch)"
