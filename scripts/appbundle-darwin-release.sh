#!/usr/bin/env bash
# Build, sign, notarize, staple, and package the branded Mora.app release
# bundles — one per Darwin architecture — from the already-signed raw CLI
# archives that GoReleaser produced. The raw archives are never modified;
# the app bundles are additional assets with names the legacy standalone
# go-selfupdate matcher can never select (they end in `_app.zip`, which is
# not one of its `<os><sep><arch><ext>` candidate suffixes).
#
# Pipeline per architecture, strictly ordered and fail-closed:
#   raw archive checksum → extract signed CLI → arch + signature checks →
#   deterministic icon + bundle assembly → whole-bundle Developer ID sign
#   (hardened runtime, secure timestamp) → notarize → staple → validate
#   (stapler + codesign + Gatekeeper) → final zip AFTER stapling →
#   path-safety + legacy-name guards → extract and revalidate FINAL asset →
#   quarantined native launch → checksums-app.txt.
set -euo pipefail

ROOT="$(unset CDPATH; cd -- "$(dirname -- "$0")/.." && pwd)"

# The legacy standalone updater (go-selfupdate) lowercases asset names and
# selects one whose name ENDS WITH `<os><sep><arch><ext>`. Refuse any app
# asset name a legacy client could select, and pin the frozen
# mora_<version>_darwin_<arch>_app.zip contract — this is the bridge-safety
# gate. Exposed as `--check-asset-name <name>` so the secret-free contract
# harness can sabotage it directly.
refuse_legacy_selectable_name() {
	local name lower sep ext arch suffix
	name="$(basename "$1")"
	lower="$(tr '[:upper:]' '[:lower:]' <<<"$name")"
	for sep in _ -; do
		for ext in .zip .tar.gz .tgz .gzip .gz .tar.xz .xz .bz2 ""; do
			for arch in amd64 arm64; do
				suffix="darwin${sep}${arch}${ext}"
				if [[ "$lower" == *"$suffix" ]]; then
					echo "error: app asset name '$name' ends with legacy updater suffix '$suffix' — an old mora upgrade would download the app bundle as a raw binary" >&2
					return 1
				fi
			done
		done
	done
	[[ "$name" =~ ^mora_[0-9]+\.[0-9]+\.[0-9]+_darwin_(amd64|arm64)_app\.zip$ ]] || {
		echo "error: app asset name '$name' does not match the frozen mora_<version>_darwin_<arch>_app.zip contract" >&2
		return 1
	}
}

if [[ "${1:-}" == "--check-asset-name" ]]; then
	[[ -n "${2:-}" ]] || {
		echo "usage: appbundle-darwin-release.sh --check-asset-name <asset-name>" >&2
		exit 2
	}
	refuse_legacy_selectable_name "$2"
	echo "asset name ok: $2"
	exit 0
fi

dist_dir="${1:-}"
version="${2:-}"
[[ -d "$dist_dir" ]] || {
	echo "error: release dist directory does not exist: ${dist_dir:-<empty>}" >&2
	exit 1
}
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "error: expected a stable numeric release version (X.Y.Z) as the second argument, got: ${version:-<empty>}" >&2
	exit 1
fi

: "${APPLE_SIGNING_IDENTITY:?APPLE_SIGNING_IDENTITY is required for app bundle signing}"
: "${APPLE_BUNDLE_ID:?APPLE_BUNDLE_ID is required for app bundle signing}"
: "${APPLE_TEAM_ID:?APPLE_TEAM_ID is required for app bundle signing}"
[[ "$APPLE_BUNDLE_ID" == "com.pyranthus.mora" ]] || {
	echo "error: refusing unexpected bundle identifier: $APPLE_BUNDLE_ID" >&2
	exit 1
}
[[ "$APPLE_TEAM_ID" == "VS8M5VJBZ5" ]] || {
	echo "error: refusing unexpected Apple team: $APPLE_TEAM_ID" >&2
	exit 1
}
expected_identity="Developer ID Application: ADIT ABHIJIT KARODE (${APPLE_TEAM_ID})"
[[ "$APPLE_SIGNING_IDENTITY" == "$expected_identity" ]] || {
	echo "error: refusing unexpected signing identity: $APPLE_SIGNING_IDENTITY" >&2
	exit 1
}

notary_auth=()
if [[ -n "${APPLE_NOTARY_KEYCHAIN_PROFILE:-}" ]]; then
	notary_auth=(--keychain-profile "$APPLE_NOTARY_KEYCHAIN_PROFILE")
else
	: "${APPLE_NOTARY_KEY_PATH:?APPLE_NOTARY_KEY_PATH or APPLE_NOTARY_KEYCHAIN_PROFILE is required}"
	: "${APPLE_NOTARY_KEY_ID:?APPLE_NOTARY_KEY_ID is required when using an API key}"
	: "${APPLE_NOTARY_ISSUER_ID:?APPLE_NOTARY_ISSUER_ID is required when using an API key}"
	[[ -f "$APPLE_NOTARY_KEY_PATH" ]] || {
		echo "error: Apple notary API key does not exist: $APPLE_NOTARY_KEY_PATH" >&2
		exit 1
	}
	notary_auth=(--key "$APPLE_NOTARY_KEY_PATH" --key-id "$APPLE_NOTARY_KEY_ID" --issuer "$APPLE_NOTARY_ISSUER_ID")
fi

work_dir="$(mktemp -d)"
cleanup() { rm -rf "$work_dir"; }
trap cleanup EXIT

# Reproducible bundle mtimes: pin to the release commit unless the caller
# already pinned an epoch.
if [[ -z "${SOURCE_DATE_EPOCH:-}" ]]; then
	commit_epoch="$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || true)"
	[[ -n "$commit_epoch" ]] && export SOURCE_DATE_EPOCH="$commit_epoch"
fi

verify_bundle_signature() {
	local app="$1" metadata requirement
	codesign --verify --deep --strict --verbose=2 "$app"
	metadata="$(codesign --display --verbose=4 "$app" 2>&1)"
	grep -Fqx "Identifier=$APPLE_BUNDLE_ID" <<<"$metadata" || {
		echo "error: $app has the wrong signing identifier" >&2; return 1;
	}
	grep -Fqx "TeamIdentifier=$APPLE_TEAM_ID" <<<"$metadata" || {
		echo "error: $app has the wrong signing team" >&2; return 1;
	}
	grep -Eq '^Authority=Developer ID Application:' <<<"$metadata" || {
		echo "error: $app is not Developer ID signed" >&2; return 1;
	}
	grep -Eq '^CodeDirectory .*flags=.*\(runtime\)' <<<"$metadata" || {
		echo "error: $app does not enable the hardened runtime" >&2; return 1;
	}
	grep -Eq '^Timestamp=.+' <<<"$metadata" || {
		echo "error: $app has no secure timestamp" >&2; return 1;
	}
	requirement="$(codesign --display --requirements - "$app" 2>&1)"
	if ! grep -Fq "identifier \"$APPLE_BUNDLE_ID\"" <<<"$requirement" || \
		! grep -Eq "subject\\.OU[^=]*= (\"${APPLE_TEAM_ID}\"|${APPLE_TEAM_ID})( |$)" <<<"$requirement"; then
		echo "error: $app has an incompatible designated requirement" >&2; return 1;
	fi
}

verify_raw_binary_signature() {
	local binary="$1" metadata requirement
	codesign --verify --strict --verbose=2 "$binary"
	metadata="$(codesign --display --verbose=4 "$binary" 2>&1)"
	grep -Fqx "Identifier=$APPLE_BUNDLE_ID" <<<"$metadata" || {
		echo "error: $binary has the wrong signing identifier" >&2; return 1;
	}
	grep -Fqx "TeamIdentifier=$APPLE_TEAM_ID" <<<"$metadata" || {
		echo "error: $binary has the wrong signing team" >&2; return 1;
	}
	grep -Eq '^Authority=Developer ID Application:' <<<"$metadata" || {
		echo "error: $binary is not Developer ID signed" >&2; return 1;
	}
	grep -Eq '^CodeDirectory .*flags=.*\(runtime\)' <<<"$metadata" || {
		echo "error: $binary does not enable the hardened runtime" >&2; return 1;
	}
	grep -Eq '^Timestamp=.+' <<<"$metadata" || {
		echo "error: $binary has no secure timestamp" >&2; return 1;
	}
	requirement="$(codesign --display --requirements - "$binary" 2>&1)"
	if ! grep -Fq "identifier \"$APPLE_BUNDLE_ID\"" <<<"$requirement" || \
		! grep -Eq "subject\\.OU[^=]*= (\"${APPLE_TEAM_ID}\"|${APPLE_TEAM_ID})( |$)" <<<"$requirement"; then
		echo "error: $binary has an incompatible designated requirement" >&2; return 1;
	fi
}

verify_bundle_layout() {
	local app="$1" key got
	[[ -x "$app/Contents/MacOS/mora" ]] || {
		echo "error: $app has no executable Contents/MacOS/mora" >&2; return 1;
	}
	[[ -f "$app/Contents/Resources/Mora.icns" ]] || {
		echo "error: $app has no Contents/Resources/Mora.icns" >&2; return 1;
	}
	for key in \
		"CFBundleIdentifier=$APPLE_BUNDLE_ID" \
		"CFBundleExecutable=mora" \
		"CFBundleIconFile=Mora" \
		"CFBundleName=Mora" \
		"CFBundleDisplayName=Mora" \
		"CFBundlePackageType=APPL" \
		"CFBundleShortVersionString=$version" \
		"CFBundleVersion=$version" \
		"LSUIElement=true"; do
		got="$(plutil -extract "${key%%=*}" raw -o - "$app/Contents/Info.plist" 2>/dev/null || true)"
		[[ "$got" == "${key#*=}" ]] || {
			echo "error: $app Info.plist ${key%%=*} is '${got:-missing}', expected '${key#*=}'" >&2
			return 1
		}
	done
}

verify_final_asset() {
	local asset_zip="$1" arch="$2" verify_dir app binary macho_arch
	verify_dir="$work_dir/final-verify-$arch"
	mkdir -p "$verify_dir"
	ditto -x -k "$asset_zip" "$verify_dir"
	app="$verify_dir/Mora.app"
	binary="$app/Contents/MacOS/mora"

	# These are distinct files: _CodeSignature/CodeResources is the sealed
	# bundle signature, while Contents/CodeResources is the stapled Apple
	# ticket. Validate the exact bytes that will be checksummed and uploaded,
	# for both architectures, rather than trusting the pre-compression bundle.
	[[ -f "$app/Contents/_CodeSignature/CodeResources" ]] || {
		echo "error: final app asset has no sealed bundle signature: $asset_zip" >&2; return 1;
	}
	[[ -f "$app/Contents/CodeResources" ]] || {
		echo "error: final app asset has no stapled notarization ticket: $asset_zip" >&2; return 1;
	}
	verify_bundle_layout "$app"
	verify_bundle_signature "$app"
	verify_raw_binary_signature "$binary"
	xcrun stapler validate "$app"
	codesign --verify --deep --strict --verbose=2 -R=notarized "$app"
	verify_gatekeeper_acceptance "$app"
	case "$arch" in
		amd64) macho_arch=x86_64 ;;
		arm64) macho_arch=arm64 ;;
	esac
	[[ "$(lipo -archs "$binary" 2>/dev/null || true)" == "$macho_arch" ]] || {
		echo "error: final app asset has the wrong executable architecture: $asset_zip" >&2; return 1;
	}
	echo "verified final asset: $(basename "$asset_zip")"
}

verify_gatekeeper_acceptance() {
	local app="$1" attempt assessment
	for attempt in 1 2 3 4 5 6; do
		# Unlike the raw CLI, an app bundle is exactly what the execute policy
		# assesses, so Gatekeeper's verdict is the gate here — no hydration
		# workaround, no ignored result.
		if assessment="$(spctl --assess --type execute --verbose=4 "$app" 2>&1)"; then
			echo "gatekeeper: accepted $(basename "$app")"
			return 0
		fi
		if [[ "$attempt" -lt 6 ]]; then sleep 10; fi
	done
	echo "error: Gatekeeper did not accept the stapled app bundle: $app" >&2
	printf '%s\n' "${assessment:-<no assessment output>}" >&2
	return 1
}

verify_quarantined_launch() {
	local asset_zip="$1" arch="$2" native_arch launch_dir app_binary first_line output
	native_arch="$(uname -m)"
	case "$native_arch" in
		x86_64) native_arch=amd64 ;;
		arm64) native_arch=arm64 ;;
		*) echo "error: unsupported macOS runner architecture: $native_arch" >&2; return 1 ;;
	esac
	if [[ "$arch" != "$native_arch" ]]; then
		echo "launch: skipped non-native Darwin $arch app bundle on $native_arch runner"
		return 0
	fi

	# Extract a disposable copy from the FINAL published asset bytes and
	# exercise the real quarantine launch path. The asset itself is never
	# modified.
	launch_dir="$work_dir/launch-app-$arch"
	mkdir -p "$launch_dir"
	ditto -x -k "$asset_zip" "$launch_dir"
	app_binary="$launch_dir/Mora.app/Contents/MacOS/mora"
	[[ -x "$app_binary" ]] || {
		echo "error: extracted app bundle has no executable CLI: $app_binary" >&2
		return 1
	}
	xattr -w com.apple.quarantine \
		"0081;$(printf '%x' "$(date +%s)");MoraAppReleaseVerification;" "$launch_dir/Mora.app"
	if ! output="$("$app_binary" version 2>&1)"; then
		echo "error: quarantined notarized Darwin $arch app bundle did not launch" >&2
		printf '%s\n' "$output" >&2
		return 1
	fi
	first_line="${output%%$'\n'*}"
	[[ "$first_line" == "mora $version" ]] || {
		echo "error: app bundle reports '$first_line', expected 'mora $version'" >&2
		return 1
	}
	echo "launch: quarantined Darwin $arch app bundle executed successfully"
}

# Deterministic branded icon, generated once from the committed pixel art and
# proven stable by generating twice and comparing bytes.
icns_path="$work_dir/Mora.icns"
(cd "$ROOT" && go run ./cmd/genicns docs/assets/mora-eye.svg "$icns_path")
(cd "$ROOT" && go run ./cmd/genicns docs/assets/mora-eye.svg "$icns_path.second")
cmp -s "$icns_path" "$icns_path.second" || {
	echo "error: Mora.icns generation is not deterministic" >&2
	exit 1
}

app_out_dir="$dist_dir/app"
mkdir -p "$app_out_dir"
checksums_file="$app_out_dir/checksums-app.txt"
: >"$checksums_file"

for arch in amd64 arm64; do
	shopt -s nullglob
	archives=("$dist_dir"/mora_*_darwin_"${arch}".tar.gz)
	shopt -u nullglob
	if [[ "${#archives[@]}" -ne 1 ]]; then
		echo "error: expected exactly one Darwin $arch archive, found ${#archives[@]}" >&2
		exit 1
	fi
	archive="${archives[0]}"
	expected_archive="mora_${version}_darwin_${arch}.tar.gz"
	[[ "$(basename "$archive")" == "$expected_archive" ]] || {
		echo "error: raw archive $(basename "$archive") does not match release version $version" >&2
		exit 1
	}
	[[ -f "$dist_dir/checksums.txt" ]] || {
		echo "error: dist/checksums.txt is missing — refusing to build app bundles from unverifiable archives" >&2
		exit 1
	}
	expected="$(awk -v name="$(basename "$archive")" '$2 == name { print $1 }' "$dist_dir/checksums.txt")"
	[[ -n "$expected" ]] || {
		echo "error: checksums.txt has no entry for $(basename "$archive")" >&2; exit 1;
	}
	actual="$(shasum -a 256 "$archive" | awk '{ print $1 }')"
	[[ "$actual" == "$expected" ]] || {
		echo "error: checksum mismatch for $(basename "$archive")" >&2; exit 1;
	}

	extract_dir="$work_dir/raw-$arch"
	mkdir -p "$extract_dir"
	tar -xzf "$archive" -C "$extract_dir"
	binary="$extract_dir/mora"
	[[ -x "$binary" ]] || {
		echo "error: $(basename "$archive") does not contain an executable root-level mora binary" >&2
		exit 1
	}
	verify_raw_binary_signature "$binary"

	bundle_dir="$work_dir/bundle-$arch"
	mkdir -p "$bundle_dir"
	bash "$ROOT/scripts/assemble-darwin-app.sh" "$binary" "$icns_path" "$version" "$arch" "$bundle_dir"
	app="$bundle_dir/Mora.app"
	verify_bundle_layout "$app"

	# Sign the WHOLE bundle: main executable + sealed resources under one
	# Developer ID signature. Never sign only the inner binary.
	codesign \
		--force \
		--sign "$APPLE_SIGNING_IDENTITY" \
		--identifier "$APPLE_BUNDLE_ID" \
		--options runtime \
		--timestamp \
		"$app"
	verify_bundle_signature "$app"

	# --norsrc: no AppleDouble (._*) metadata entries — the archive content is
	# exactly the bundle files, deterministic across build hosts.
	notary_zip="$work_dir/Mora_${arch}_notary.zip"
	ditto -c -k --norsrc --keepParent "$app" "$notary_zip"
	result_path="$work_dir/Mora_${arch}_notary.json"
	submitted=0
	for attempt in 1 2 3; do
		if xcrun notarytool submit "$notary_zip" "${notary_auth[@]}" \
			--wait --timeout 30m --output-format json >"$result_path"; then
			submitted=1
			break
		fi
		if [[ "$attempt" -lt 3 ]]; then
			echo "notarytool attempt $attempt failed for Darwin $arch app; retrying" >&2
			sleep 5
		fi
	done
	if [[ "$submitted" -ne 1 ]]; then
		echo "error: Apple notarization submission failed for Darwin $arch app after 3 attempts" >&2
		exit 1
	fi
	status="$(plutil -extract status raw -o - "$result_path" 2>/dev/null || true)"
	if [[ "$status" != "Accepted" ]]; then
		echo "error: Apple notarization status for Darwin $arch app is ${status:-unparseable}, expected Accepted" >&2
		cat "$result_path" >&2
		exit 1
	fi

	# Staple the ticket INTO the bundle, then re-validate everything: the
	# staple, the seal it must not have broken, and Gatekeeper's verdict.
	xcrun stapler staple "$app"
	[[ -f "$app/Contents/CodeResources" ]] || {
		echo "error: stapler reported success but $app carries no stapled ticket" >&2
		exit 1
	}
	xcrun stapler validate "$app"
	verify_bundle_signature "$app"
	verify_bundle_layout "$app"
	verify_gatekeeper_acceptance "$app"

	# The published asset is zipped AFTER stapling so installs validate
	# offline. Its name must be un-selectable by the legacy updater.
	asset_name="mora_${version}_darwin_${arch}_app.zip"
	refuse_legacy_selectable_name "$asset_name"
	asset_zip="$app_out_dir/$asset_name"
	rm -f "$asset_zip"
	ditto -c -k --norsrc --keepParent "$app" "$asset_zip"
	bash "$ROOT/scripts/verify-app-zip.sh" "$asset_zip"
	verify_final_asset "$asset_zip" "$arch"
	verify_quarantined_launch "$asset_zip" "$arch"

	(cd "$app_out_dir" && shasum -a 256 "$asset_name") >>"$checksums_file"
	echo "packaged: $asset_name"
done

echo "verified: both Mora.app bundles are Developer ID signed, notarized, stapled, and packaged"
