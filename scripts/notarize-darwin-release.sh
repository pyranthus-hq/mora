#!/usr/bin/env bash
# Submit the exact signed Darwin binaries contained in GoReleaser archives to
# Apple's notary service. Raw executables cannot be stapled, so the final
# verification relies on Apple's online ticket for the signed cdhash.
set -euo pipefail

dist_dir="${1:-dist}"
[[ -d "$dist_dir" ]] || {
	echo "error: release dist directory does not exist: $dist_dir" >&2
	exit 1
}

: "${APPLE_BUNDLE_ID:?APPLE_BUNDLE_ID is required for notarization verification}"
: "${APPLE_TEAM_ID:?APPLE_TEAM_ID is required for notarization verification}"
[[ "$APPLE_BUNDLE_ID" == "com.pyranthus.mora" ]] || {
	echo "error: refusing unexpected bundle identifier: $APPLE_BUNDLE_ID" >&2
	exit 1
}
[[ "$APPLE_TEAM_ID" == "VS8M5VJBZ5" ]] || {
	echo "error: refusing unexpected Apple team: $APPLE_TEAM_ID" >&2
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

verify_signature() {
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

verify_notary_ticket() {
	local binary="$1" attempt assessment
	for attempt in 1 2 3 4 5 6; do
		# The special `notarized` code requirement verifies Apple's ticket for
		# this exact code directory. spctl's install policy is for installer
		# packages, while its execute policy rejects a valid raw CLI as not app-like.
		if assessment="$(codesign --verify --strict --verbose=4 -R='notarized' "$binary" 2>&1)"; then
			echo "notary ticket: accepted for $(basename "$binary")"
			return 0
		fi
		if [[ "$attempt" -lt 6 ]]; then sleep 10; fi
	done
	echo "error: Apple did not satisfy the notarized code requirement for $binary" >&2
	printf '%s\n' "${assessment:-<no assessment output>}" >&2
	return 1
}

verify_quarantined_launch() {
	local binary="$1" arch="$2" native_arch launch_dir launch_binary output
	native_arch="$(uname -m)"
	case "$native_arch" in
		x86_64) native_arch=amd64 ;;
		arm64) native_arch=arm64 ;;
		*) echo "error: unsupported macOS runner architecture: $native_arch" >&2; return 1 ;;
	esac
	if [[ "$arch" != "$native_arch" ]]; then
		echo "launch: skipped non-native Darwin $arch binary on $native_arch runner"
		return 0
	fi

	# Exercise the operating system's real quarantine launch path on a disposable
	# copy. Never add or remove xattrs on the release archive or extracted binary.
	launch_dir="$work_dir/launch-$arch"
	mkdir -p "$launch_dir"
	launch_binary="$launch_dir/mora"
	cp "$binary" "$launch_binary"
	chmod +x "$launch_binary"
	xattr -w com.apple.quarantine \
		"0081;$(printf '%x' "$(date +%s)");MoraReleaseVerification;" "$launch_binary"
	if ! output="$("$launch_binary" version 2>&1)"; then
		echo "error: quarantined notarized Darwin $arch binary did not launch" >&2
		printf '%s\n' "$output" >&2
		return 1
	fi
	echo "launch: quarantined Darwin $arch binary executed successfully"
}

for arch in amd64 arm64; do
	shopt -s nullglob
	archives=("$dist_dir"/mora_*_darwin_"${arch}".tar.gz)
	shopt -u nullglob
	if [[ "${#archives[@]}" -ne 1 ]]; then
		echo "error: expected exactly one Darwin $arch archive, found ${#archives[@]}" >&2
		exit 1
	fi
	archive="${archives[0]}"
	if [[ -f "$dist_dir/checksums.txt" ]]; then
		expected="$(awk -v name="$(basename "$archive")" '$2 == name { print $1 }' "$dist_dir/checksums.txt")"
		[[ -n "$expected" ]] || {
			echo "error: checksums.txt has no entry for $(basename "$archive")" >&2; exit 1;
		}
		actual="$(shasum -a 256 "$archive" | awk '{ print $1 }')"
		[[ "$actual" == "$expected" ]] || {
			echo "error: checksum mismatch for $(basename "$archive")" >&2; exit 1;
		}
	fi

	extract_dir="$work_dir/$arch"
	mkdir -p "$extract_dir"
	tar -xzf "$archive" -C "$extract_dir"
	binary="$extract_dir/mora"
	[[ -x "$binary" ]] || {
		echo "error: $(basename "$archive") does not contain an executable root-level mora binary" >&2
		exit 1
	}
	verify_signature "$binary"

	zip_path="$work_dir/mora_${arch}_notary.zip"
	ditto -c -k --keepParent "$binary" "$zip_path"
	result_path="$work_dir/mora_${arch}_notary.json"
	submitted=0
	for attempt in 1 2 3; do
		if xcrun notarytool submit "$zip_path" "${notary_auth[@]}" \
			--wait --timeout 30m --output-format json >"$result_path"; then
			submitted=1
			break
		fi
		if [[ "$attempt" -lt 3 ]]; then
			echo "notarytool attempt $attempt failed for Darwin $arch; retrying" >&2
			sleep 5
		fi
	done
	if [[ "$submitted" -ne 1 ]]; then
		echo "error: Apple notarization submission failed for Darwin $arch after 3 attempts" >&2
		exit 1
	fi
	status="$(plutil -extract status raw -o - "$result_path" 2>/dev/null || true)"
	if [[ "$status" != "Accepted" ]]; then
		echo "error: Apple notarization status for Darwin $arch is ${status:-unparseable}, expected Accepted" >&2
		cat "$result_path" >&2
		exit 1
	fi
	verify_signature "$binary"
	verify_notary_ticket "$binary"
	verify_quarantined_launch "$binary" "$arch"
	echo "notarized: $(basename "$archive")"
done

echo "verified: both Darwin release archives are Developer ID signed and notarized"
