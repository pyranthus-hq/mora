#!/usr/bin/env bash
# Build, sign, notarize, staple, and package the Mora Status menu bar app
# (apps/mora-menubar) for Apple silicon. Same trust chain as
# appbundle-darwin-release.sh, which this mirrors:
#
#   build collector (pure Go, arm64) → build app → arch + layout checks →
#   sign the nested collector, then the whole bundle (Developer ID, hardened
#   runtime, secure timestamp) → notarize → staple → validate (stapler +
#   codesign + Gatekeeper) → zip AFTER stapling → asset-name guard → extract
#   and revalidate the FINAL zip → checksums-status.txt.
#
# Usage: mora-status-darwin-release.sh <out-dir> <X.Y.Z>
#        mora-status-darwin-release.sh --check-asset-name <name>
set -euo pipefail

ROOT="$(unset CDPATH; cd -- "$(dirname -- "$0")/.." && pwd)"
BUNDLE_ID="com.pyranthus.mora-status"
COLLECTOR_ID="com.pyranthus.mora-status.tentacles"

# The legacy standalone updater (go-selfupdate) lowercases asset names and
# picks one that ENDS WITH `<os><sep><arch><ext>`; the Mora.app updater wants
# the exact mora_<v>_darwin_<arch>_app.zip name. A Status asset must match
# neither, or an old `mora upgrade` could install the menu bar app as the CLI.
check_asset_name() {
	local name lower sep ext arch
	name="$(basename "$1")"
	lower="$(tr '[:upper:]' '[:lower:]' <<<"$name")"
	for sep in _ -; do
		for ext in .zip .tar.gz .tgz .gzip .gz .tar.xz .xz .bz2 ""; do
			for arch in amd64 arm64; do
				if [[ "$lower" == *"darwin${sep}${arch}${ext}" ]]; then
					echo "error: asset name '$name' ends with legacy updater suffix 'darwin${sep}${arch}${ext}'" >&2
					return 1
				fi
			done
		done
	done
	[[ "$name" =~ ^mora-status_[0-9]+\.[0-9]+\.[0-9]+_arm64\.zip$ ]] || {
		echo "error: asset name '$name' does not match mora-status_<version>_arm64.zip" >&2
		return 1
	}
}

if [[ "${1:-}" == "--check-asset-name" ]]; then
	[[ -n "${2:-}" ]] || { echo "usage: $0 --check-asset-name <name>" >&2; exit 2; }
	check_asset_name "$2"
	echo "asset name ok: $2"
	exit 0
fi

out_dir="${1:-}"
version="${2:-}"
[[ -n "$out_dir" ]] || { echo "usage: $0 <out-dir> <X.Y.Z>" >&2; exit 2; }
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
	echo "error: expected a numeric release version (X.Y.Z), got: ${version:-<empty>}" >&2
	exit 1
}

: "${APPLE_SIGNING_IDENTITY:?APPLE_SIGNING_IDENTITY is required}"
: "${APPLE_TEAM_ID:?APPLE_TEAM_ID is required}"
[[ "$APPLE_TEAM_ID" == "VS8M5VJBZ5" ]] || { echo "error: refusing unexpected Apple team: $APPLE_TEAM_ID" >&2; exit 1; }
[[ "$APPLE_SIGNING_IDENTITY" == "Developer ID Application: ADIT ABHIJIT KARODE (${APPLE_TEAM_ID})" ]] || {
	echo "error: refusing unexpected signing identity: $APPLE_SIGNING_IDENTITY" >&2
	exit 1
}
if [[ -n "${APPLE_NOTARY_KEYCHAIN_PROFILE:-}" ]]; then
	notary_auth=(--keychain-profile "$APPLE_NOTARY_KEYCHAIN_PROFILE")
else
	: "${APPLE_NOTARY_KEY_PATH:?APPLE_NOTARY_KEY_PATH or APPLE_NOTARY_KEYCHAIN_PROFILE is required}"
	: "${APPLE_NOTARY_KEY_ID:?APPLE_NOTARY_KEY_ID is required}"
	: "${APPLE_NOTARY_ISSUER_ID:?APPLE_NOTARY_ISSUER_ID is required}"
	[[ -f "$APPLE_NOTARY_KEY_PATH" ]] || { echo "error: notary key does not exist: $APPLE_NOTARY_KEY_PATH" >&2; exit 1; }
	notary_auth=(--key "$APPLE_NOTARY_KEY_PATH" --key-id "$APPLE_NOTARY_KEY_ID" --issuer "$APPLE_NOTARY_ISSUER_ID")
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
mkdir -p "$out_dir"

verify_signature() {
	local path="$1" id="$2" metadata requirement
	codesign --verify --strict --verbose=2 "$path"
	metadata="$(codesign --display --verbose=4 "$path" 2>&1)"
	grep -Fqx "Identifier=$id" <<<"$metadata" || { echo "error: $path has the wrong signing identifier" >&2; return 1; }
	grep -Fqx "TeamIdentifier=$APPLE_TEAM_ID" <<<"$metadata" || { echo "error: $path has the wrong signing team" >&2; return 1; }
	grep -Eq '^Authority=Developer ID Application:' <<<"$metadata" || { echo "error: $path is not Developer ID signed" >&2; return 1; }
	grep -Eq '^CodeDirectory .*flags=.*\(runtime\)' <<<"$metadata" || { echo "error: $path does not enable the hardened runtime" >&2; return 1; }
	grep -Eq '^Timestamp=.+' <<<"$metadata" || { echo "error: $path has no secure timestamp" >&2; return 1; }
	requirement="$(codesign --display --requirements - "$path" 2>&1)"
	if ! grep -Fq "identifier \"$id\"" <<<"$requirement" || \
		! grep -Eq "subject\\.OU[^=]*= (\"${APPLE_TEAM_ID}\"|${APPLE_TEAM_ID})( |$)" <<<"$requirement"; then
		echo "error: $path has an incompatible designated requirement" >&2; return 1
	fi
}

verify_layout() {
	local app="$1" key got exe
	for exe in "Mora Status" mora-tentacles; do
		[[ -x "$app/Contents/MacOS/$exe" ]] || { echo "error: $app has no Contents/MacOS/$exe" >&2; return 1; }
		[[ "$(lipo -archs "$app/Contents/MacOS/$exe" 2>/dev/null || true)" == "arm64" ]] || {
			echo "error: Contents/MacOS/$exe is not arm64-only" >&2; return 1
		}
	done
	for key in "CFBundleIdentifier=$BUNDLE_ID" "CFBundleExecutable=Mora Status" "CFBundleName=Mora Status" \
		"CFBundlePackageType=APPL" "CFBundleShortVersionString=$version" "CFBundleVersion=$version" "LSUIElement=true"; do
		got="$(plutil -extract "${key%%=*}" raw -o - "$app/Contents/Info.plist" 2>/dev/null || true)"
		[[ "$got" == "${key#*=}" ]] || { echo "error: Info.plist ${key%%=*} is '${got:-missing}', expected '${key#*=}'" >&2; return 1; }
	done
}

verify_full() {
	local app="$1"
	verify_layout "$app"
	verify_signature "$app/Contents/MacOS/mora-tentacles" "$COLLECTOR_ID"
	verify_signature "$app" "$BUNDLE_ID"
	codesign --verify --deep --strict --verbose=2 "$app"
}

gatekeeper() {
	local app="$1" attempt assessment=""
	for attempt in 1 2 3 4 5 6; do
		if assessment="$(spctl --assess --type execute --verbose=4 "$app" 2>&1)"; then
			echo "gatekeeper: accepted $(basename "$app")"; return 0
		fi
		if [[ "$attempt" -lt 6 ]]; then sleep 10; fi
	done
	echo "error: Gatekeeper did not accept $app" >&2
	printf '%s\n' "$assessment" >&2
	return 1
}

# Build: the collector is pure Go (CGO_ENABLED=0), like the CLI.
collector="$work_dir/mora-tentacles"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o "$collector" ./cmd/mora-tentacles)
app="$work_dir/Mora Status.app"
APP_DIR="$app" MORA_STATUS_VERSION="$version" sh "$ROOT/apps/mora-menubar/build.sh" "$collector"
verify_layout "$app"

# Sign inside-out: the nested collector first, then the whole bundle.
sign() { codesign --force --sign "$APPLE_SIGNING_IDENTITY" --identifier "$1" --options runtime --timestamp "$2"; }
sign "$COLLECTOR_ID" "$app/Contents/MacOS/mora-tentacles"
sign "$BUNDLE_ID" "$app"
verify_full "$app"

notary_zip="$work_dir/notary.zip"
ditto -c -k --norsrc --keepParent "$app" "$notary_zip"
result="$work_dir/notary.json"
submitted=0
for attempt in 1 2 3; do
	if xcrun notarytool submit "$notary_zip" "${notary_auth[@]}" --wait --timeout 30m --output-format json >"$result"; then
		submitted=1; break
	fi
	if [[ "$attempt" -lt 3 ]]; then echo "notarytool attempt $attempt failed; retrying" >&2; sleep 5; fi
done
[[ "$submitted" -eq 1 ]] || { echo "error: notarization submission failed after 3 attempts" >&2; exit 1; }
status="$(plutil -extract status raw -o - "$result" 2>/dev/null || true)"
[[ "$status" == "Accepted" ]] || { echo "error: notarization status is ${status:-unparseable}, expected Accepted" >&2; cat "$result" >&2; exit 1; }

# stapler fetches the ticket from Apple's CDN, which can lag or drop; retry.
stapled=0
for attempt in 1 2 3 4 5 6; do
	if xcrun stapler staple "$app"; then stapled=1; break; fi
	if [[ "$attempt" -lt 6 ]]; then echo "stapler attempt $attempt failed; retrying in $((attempt * 20))s" >&2; sleep $((attempt * 20)); fi
done
[[ "$stapled" -eq 1 ]] || { echo "error: stapling failed after 6 attempts" >&2; exit 1; }
xcrun stapler validate "$app"
verify_full "$app"
gatekeeper "$app"

asset="mora-status_${version}_arm64.zip"
check_asset_name "$asset"
rm -f "$out_dir/$asset"
ditto -c -k --norsrc --keepParent "$app" "$out_dir/$asset"

# Revalidate the exact bytes that will be uploaded.
final="$work_dir/final"
mkdir -p "$final"
ditto -x -k "$out_dir/$asset" "$final"
[[ "$(cd "$final" && ls)" == "Mora Status.app" ]] || { echo "error: final zip must hold only Mora Status.app" >&2; exit 1; }
[[ -f "$final/Mora Status.app/Contents/CodeResources" ]] || { echo "error: final zip has no stapled ticket" >&2; exit 1; }
xcrun stapler validate "$final/Mora Status.app"
verify_full "$final/Mora Status.app"
codesign --verify --deep --strict -R=notarized "$final/Mora Status.app"
gatekeeper "$final/Mora Status.app"

(cd "$out_dir" && shasum -a 256 "$asset") >"$out_dir/checksums-status.txt"
echo "packaged: $out_dir/$asset"
