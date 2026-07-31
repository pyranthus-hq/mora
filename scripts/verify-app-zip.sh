#!/usr/bin/env bash
# Fail-closed path-safety and layout gate for a Mora.app release zip. An app
# updater extracts this archive, so a hostile or corrupted zip must be refused
# before any byte lands outside the staging directory: no absolute paths, no
# `..` traversal, no symlinks, nothing outside Mora.app/, and the required
# bundle members must be present.
set -euo pipefail

zip_path="${1:-}"
[[ -n "$zip_path" && -f "$zip_path" ]] || {
	echo "error: app zip does not exist: ${zip_path:-<empty>}" >&2
	exit 1
}

command -v zipinfo >/dev/null 2>&1 || {
	echo "error: zipinfo is required to inventory the app zip" >&2
	exit 1
}

entries="$(zipinfo -1 "$zip_path")"
[[ -n "$entries" ]] || {
	echo "error: app zip has no entries: $zip_path" >&2
	exit 1
}

while IFS= read -r entry; do
	case "$entry" in
		/*)
			echo "error: unsafe absolute path in app zip: $entry" >&2
			exit 1
			;;
		../*|*/../*|*/..|..)
			echo "error: unsafe traversal path in app zip: $entry" >&2
			exit 1
			;;
		"Mora.app"|"Mora.app/"*) ;;
		*)
			echo "error: app zip entry escapes Mora.app/: $entry" >&2
			exit 1
			;;
	esac
done <<<"$entries"

# zipinfo's long listing marks symlinks with a leading 'l' mode. The bundle
# contains no symlinks; any symlink entry is an extraction hazard.
if zipinfo "$zip_path" | grep -E '^l' >/dev/null 2>&1; then
	echo "error: app zip contains symlink entries" >&2
	exit 1
fi

for required in \
	"Mora.app/Contents/Info.plist" \
	"Mora.app/Contents/MacOS/mora" \
	"Mora.app/Contents/Resources/Mora.icns"; do
	if ! grep -Fxq "$required" <<<"$entries"; then
		echo "error: app zip is missing required member: $required" >&2
		exit 1
	fi
done

echo "verified: $(basename "$zip_path") contains only safe Mora.app/ paths with the required bundle members"
