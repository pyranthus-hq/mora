#!/usr/bin/env bash
# Sign one GoReleaser Darwin binary with Mora's stable Developer ID identity.
# Non-Darwin build targets are intentionally ignored because GoReleaser runs
# post-build hooks once for every target in the build matrix.
set -euo pipefail

artifact="${1:-}"
target="${2:-}"
target_os="${target%%_*}"

if [[ "$target_os" != "darwin" ]]; then
	exit 0
fi

[[ -n "$artifact" && -f "$artifact" ]] || {
	echo "error: Darwin artifact does not exist: ${artifact:-<empty>}" >&2
	exit 1
}

: "${APPLE_SIGNING_IDENTITY:?APPLE_SIGNING_IDENTITY is required for Darwin release signing}"
: "${APPLE_BUNDLE_ID:?APPLE_BUNDLE_ID is required for Darwin release signing}"
: "${APPLE_TEAM_ID:?APPLE_TEAM_ID is required for Darwin release signing}"

expected_identity="Developer ID Application: ADIT ABHIJIT KARODE (${APPLE_TEAM_ID})"
if [[ "$APPLE_SIGNING_IDENTITY" != "$expected_identity" ]]; then
	echo "error: refusing unexpected signing identity: $APPLE_SIGNING_IDENTITY" >&2
	exit 1
fi
if [[ "$APPLE_BUNDLE_ID" != "com.pyranthus.mora" ]]; then
	echo "error: refusing unexpected bundle identifier: $APPLE_BUNDLE_ID" >&2
	exit 1
fi
if [[ "$APPLE_TEAM_ID" != "VS8M5VJBZ5" ]]; then
	echo "error: refusing unexpected Apple team: $APPLE_TEAM_ID" >&2
	exit 1
fi

codesign \
	--force \
	--sign "$APPLE_SIGNING_IDENTITY" \
	--identifier "$APPLE_BUNDLE_ID" \
	--options runtime \
	--timestamp \
	"$artifact"

codesign --verify --strict --verbose=2 "$artifact"
metadata="$(codesign --display --verbose=4 "$artifact" 2>&1)"
require_metadata() {
	local expected="$1"
	if ! grep -Fqx "$expected" <<<"$metadata"; then
		echo "error: signed artifact is missing metadata: $expected" >&2
		exit 1
	fi
}

require_metadata "Identifier=$APPLE_BUNDLE_ID"
require_metadata "TeamIdentifier=$APPLE_TEAM_ID"
if ! grep -Eq '^Authority=Developer ID Application:' <<<"$metadata"; then
	echo "error: signed artifact is not a Developer ID Application artifact" >&2
	exit 1
fi
if ! grep -Eq '^CodeDirectory .*flags=.*\(runtime\)' <<<"$metadata"; then
	echo "error: signed artifact does not enable the hardened runtime" >&2
	exit 1
fi
if ! grep -Eq '^Timestamp=.+' <<<"$metadata"; then
	echo "error: signed artifact has no secure timestamp" >&2
	exit 1
fi

requirement="$(codesign --display --requirements - "$artifact" 2>&1)"
if ! grep -Fq "identifier \"$APPLE_BUNDLE_ID\"" <<<"$requirement" || \
	! grep -Eq "subject\\.OU[^=]*= (\"${APPLE_TEAM_ID}\"|${APPLE_TEAM_ID})( |$)" <<<"$requirement"; then
	echo "error: signed artifact designated requirement does not pin Mora's identifier and team" >&2
	exit 1
fi

echo "signed: $artifact ($APPLE_BUNDLE_ID, team $APPLE_TEAM_ID)"
