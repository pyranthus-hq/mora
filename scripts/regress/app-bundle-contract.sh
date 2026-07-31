#!/usr/bin/env bash
# Secret-free contract gate for the branded Mora.app release lane (issue #257,
# Lane A). Apple tooling is replaced with deterministic PATH fixtures, exactly
# like release-signing-contract.sh: this proves the app pipeline fails closed
# on identity, seal, staple, Gatekeeper, layout, version, architecture, and
# zip-path drift — without a certificate, a notary submission, or any secret.
# The live macOS release job remains the real Developer ID + notary gate.
set -euo pipefail

ROOT="$(unset CDPATH; cd -- "$(dirname -- "$0")/../.." && pwd)"
ASSEMBLE="$ROOT/scripts/assemble-darwin-app.sh"
APP_RELEASE="$ROOT/scripts/appbundle-darwin-release.sh"
VERIFY_ZIP="$ROOT/scripts/verify-app-zip.sh"
WORKFLOW="$ROOT/.github/workflows/release.yml"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/mora-app-bundle-contract.XXXXXX")"
MOCK_BIN="$WORK/mock-bin"
MOCK_LOG="$WORK/mock-calls.log"
VERSION="0.11.9"

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

pass() { printf 'ok   %s\n' "$*"; }
die() { printf 'FAIL %s\n' "$*" >&2; exit 1; }

expect_pass() {
	local label="$1"
	shift
	if "$@" >"$WORK/stdout" 2>"$WORK/stderr"; then
		pass "$label"
	else
		printf '%s\n' "--- stdout ($label) ---" >&2
		sed -n '1,120p' "$WORK/stdout" >&2
		printf '%s\n' "--- stderr ($label) ---" >&2
		sed -n '1,120p' "$WORK/stderr" >&2
		die "$label: expected success"
	fi
}

expect_fail() {
	local label="$1"
	shift
	if "$@" >"$WORK/stdout" 2>"$WORK/stderr"; then
		die "$label: expected a fail-closed non-zero exit"
	fi
	pass "$label (refused)"
}

assert_log_contains() {
	local needle="$1" label="$2"
	grep -F -- "$needle" "$MOCK_LOG" >/dev/null 2>&1 || {
		sed -n '1,200p' "$MOCK_LOG" >&2
		die "$label: mock call log lacks: $needle"
	}
}

require_text() {
	local file="$1" pattern="$2" label="$3"
	grep -E -- "$pattern" "$file" >/dev/null 2>&1 || die "$label ($file)"
}

line_of() {
	local file="$1" pattern="$2"
	awk -v p="$pattern" '$0 ~ p { print NR; exit }' "$file"
}

assert_before() {
	local file="$1" first="$2" second="$3" label="$4" a b
	a="$(line_of "$file" "$first")"
	b="$(line_of "$file" "$second")"
	[ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ] || \
		die "$label (first=${a:-missing}, second=${b:-missing})"
}

[ -f "$ASSEMBLE" ] || die "missing assembly helper: $ASSEMBLE"
[ -f "$APP_RELEASE" ] || die "missing app release helper: $APP_RELEASE"
[ -f "$VERIFY_ZIP" ] || die "missing zip safety helper: $VERIFY_ZIP"
bash -n "$ASSEMBLE" "$APP_RELEASE" "$VERIFY_ZIP" "$0" || die "app bundle shell scripts must parse under bash"
command -v python3 >/dev/null 2>&1 || die "python3 is required to craft hostile zip fixtures"
command -v plutil >/dev/null 2>&1 || die "plutil is required (macOS-only contract, like sandbox-exec in release-signing-contract.sh)"

mkdir -p "$MOCK_BIN"

# --- PATH fixtures -----------------------------------------------------------

cat >"$MOCK_BIN/codesign" <<'MOCK_CODESIGN'
#!/usr/bin/env bash
set -euo pipefail
printf 'codesign' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"

display=0
requirements=0
sign=0
for arg in "$@"; do
	case "$arg" in -d*|--display) display=1 ;; esac
	case "$arg" in --requirements|-r-) requirements=1 ;; esac
	case "$arg" in --sign) sign=1 ;; esac
done

if [ "$sign" -eq 1 ]; then
	target="${@: -1}"
	mkdir -p "$target/Contents/_CodeSignature"
	: >"$target/Contents/_CodeSignature/CodeResources"
fi

if [ "$display" -eq 1 ]; then
	identifier="com.pyranthus.mora"
	team="VS8M5VJBZ5"
	flags="flags=0x10000(runtime)"
	runtime="Runtime Version=26.0.0"
	timestamp="Timestamp=Jul 31, 2026 at 12:00:00"
	case "${MOCK_CODESIGN_MODE:-valid}" in
		wrong_identifier) identifier="com.example.impostor" ;;
		wrong_team) team="AAAAAAAAAA" ;;
		missing_team) team="" ;;
		missing_runtime) flags="flags=0x0(none)"; runtime="" ;;
		missing_timestamp) timestamp="" ;;
		valid) : ;;
	esac
	if [ "$requirements" -eq 1 ]; then
		printf 'Executable=/fixture/Mora.app/Contents/MacOS/mora\n' >&2
		printf 'designated => identifier "%s" and anchor apple generic and certificate leaf[subject.OU] = %s\n' \
			"$identifier" "$team" >&2
	else
		{
			printf 'Executable=/fixture/Mora.app/Contents/MacOS/mora\n'
			printf 'Identifier=%s\n' "$identifier"
			printf 'Format=app bundle with Mach-O thin (arm64)\n'
			printf 'CodeDirectory v=20500 size=123 %s\n' "$flags"
			printf 'CDHash=0123456789abcdef0123456789abcdef01234567\n'
			printf 'Authority=Developer ID Application: ADIT ABHIJIT KARODE (VS8M5VJBZ5)\n'
			[ -n "$team" ] && printf 'TeamIdentifier=%s\n' "$team"
			[ -n "$runtime" ] && printf '%s\n' "$runtime"
			[ -n "$timestamp" ] && printf '%s\n' "$timestamp"
		} >&2
	fi
fi

[ "${MOCK_CODESIGN_MODE:-valid}" != "verify_failure" ]
MOCK_CODESIGN

# xcrun carries two personalities here: notarytool (submission verdict) and
# stapler (ticket attach/validate). A "noop" staple returns success without
# writing the ticket — production must detect that, not trust the exit code.
cat >"$MOCK_BIN/xcrun" <<'MOCK_XCRUN'
#!/usr/bin/env bash
set -euo pipefail
printf 'xcrun' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"

tool="${1:-}"
case "$tool" in
	notarytool)
		case "${MOCK_NOTARY_MODE:-accepted}" in
			accepted) printf '{"id":"00000000-0000-0000-0000-000000000001","status":"Accepted"}\n' ;;
			invalid) printf '{"id":"00000000-0000-0000-0000-000000000002","status":"Invalid"}\n' ;;
			malformed) printf '{"id":"00000000-0000-0000-0000-000000000003"}\n' ;;
			submit_failure) printf 'network failure\n' >&2; exit 1 ;;
			*) printf 'unknown fixture mode\n' >&2; exit 2 ;;
		esac
		;;
	stapler)
		action="${2:-}"
		target="${3:-}"
		case "$action" in
			staple)
				case "${MOCK_STAPLE_MODE:-accepted}" in
					accepted) mkdir -p "$target/Contents"; : >"$target/Contents/CodeResources" ;;
					noop) : ;;
					fail) printf 'CloudKit query failed\n' >&2; exit 65 ;;
					*) exit 2 ;;
				esac
				;;
			validate)
				[ "${MOCK_STAPLE_VALIDATE_MODE:-accepted}" = "accepted" ] || exit 65
				[ -f "$target/Contents/CodeResources" ] || exit 65
				;;
			*) exit 2 ;;
		esac
		;;
	*) exit 2 ;;
esac
MOCK_XCRUN

# For an app bundle (unlike a raw CLI), Gatekeeper's execute assessment is the
# real verdict, so the mock's rejection must fail the pipeline.
cat >"$MOCK_BIN/spctl" <<'MOCK_SPCTL'
#!/usr/bin/env bash
set -euo pipefail
printf 'spctl' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"

case "${MOCK_SPCTL_MODE:-accepted}" in
	accepted)
		printf '%s: accepted\nsource=Notarized Developer ID\n' "${*: -1}" >&2
		exit 0
		;;
	rejected)
		printf '%s: rejected\n' "${*: -1}" >&2
		exit 3
		;;
	*) exit 2 ;;
esac
MOCK_SPCTL

cat >"$MOCK_BIN/xattr" <<'MOCK_XATTR'
#!/usr/bin/env bash
set -euo pipefail
printf 'xattr' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"

[ "$#" -eq 4 ] || exit 2
[ "$1" = "-w" ] || exit 2
[ "$2" = "com.apple.quarantine" ] || exit 2
[ "${MOCK_QUARANTINE_MODE:-accepted}" = "accepted" ] || exit 1
target="$4"
[ -e "$target" ] || exit 2
: >"${target}.mora-quarantined"
MOCK_XATTR

# The fixture "binaries" are shell scripts, so lipo answers from the path the
# pipeline conventionally gives it (…-amd64…/…-arm64…). wrong_arch inverts the
# answer; universal returns a fat listing — both must be refused.
cat >"$MOCK_BIN/lipo" <<'MOCK_LIPO'
#!/usr/bin/env bash
set -euo pipefail
printf 'lipo' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"

[ "${1:-}" = "-archs" ] || exit 2
target="${2:-}"
arch=""
case "$target" in
	*amd64*) arch="x86_64" ;;
	*arm64*) arch="arm64" ;;
esac
case "${MOCK_LIPO_MODE:-match}" in
	match) printf '%s\n' "$arch" ;;
	wrong_arch)
		if [ "$arch" = "x86_64" ]; then printf 'arm64\n'; else printf 'x86_64\n'; fi
		;;
	universal) printf 'x86_64 arm64\n' ;;
	fail) printf 'lipo: unreadable\n' >&2; exit 1 ;;
	*) exit 2 ;;
esac
MOCK_LIPO

cat >"$MOCK_BIN/sleep" <<'MOCK_SLEEP'
#!/usr/bin/env bash
set -euo pipefail
printf 'sleep' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"
MOCK_SLEEP

# Pin the fixture host to arm64 so the contract deterministically launches
# arm64 and explicitly skips amd64.
cat >"$MOCK_BIN/uname" <<'MOCK_UNAME'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "-m" ]; then
	printf 'arm64\n'
	exit 0
fi
/usr/bin/uname "$@"
MOCK_UNAME

# ditto stays REAL (the zips must be genuine for zipinfo and extraction); the
# wrapper only records the invocation order for the staple-before-zip check.
cat >"$MOCK_BIN/ditto" <<'MOCK_DITTO'
#!/usr/bin/env bash
set -euo pipefail
printf 'ditto' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"
/usr/bin/ditto "$@"
output="${@: -1}"
if [[ "$output" == *_app.zip ]]; then
	case "${MOCK_DITTO_MODE:-valid}" in
		valid) : ;;
		drop_ticket) /usr/bin/zip -dq "$output" 'Mora.app/Contents/CodeResources' ;;
		drop_signature) /usr/bin/zip -dq "$output" 'Mora.app/Contents/_CodeSignature/CodeResources' ;;
		*) exit 2 ;;
	esac
fi
MOCK_DITTO

chmod +x "$MOCK_BIN"/codesign "$MOCK_BIN"/xcrun "$MOCK_BIN"/spctl "$MOCK_BIN"/xattr \
	"$MOCK_BIN"/lipo "$MOCK_BIN"/sleep "$MOCK_BIN"/uname "$MOCK_BIN"/ditto

# --- fixtures ---------------------------------------------------------------

# One real deterministic icon for the assembly tests, built by the actual
# generator from the committed pixel art.
ICNS="$WORK/Mora.icns"
(cd "$ROOT" && go run ./cmd/genicns docs/assets/mora-eye.svg "$ICNS") >/dev/null
[ -s "$ICNS" ] || die "genicns produced no icon"
(cd "$ROOT" && go run ./cmd/genicns docs/assets/mora-eye.svg "$ICNS.second") >/dev/null
cmp -s "$ICNS" "$ICNS.second" || die "genicns output is not byte-deterministic"
pass "genicns regenerates byte-identical Mora.icns from the committed pixel art"

# A release-shaped fixture CLI. It must survive the quarantined app launch:
# require the quarantine marker on the bundle root and report the release
# version on line one, exactly like the real binary.
make_binary() {
	local path="$1" version="$2"
	cat >"$path" <<FIXTURE
#!/bin/sh
set -eu
app="\${0%/Contents/MacOS/mora}"
case "\$app" in
	*.app)
		[ -f "\$app.mora-quarantined" ] || exit 92
		;;
esac
[ "\${MOCK_LAUNCH_MODE:-accepted}" = accepted ] || exit 93
printf 'mora %s\n' "$version"
FIXTURE
	chmod +x "$path"
}

run_assemble() {
	local binary="$1" icns="$2" version="$3" arch="$4" out="$5"
	PATH="$MOCK_BIN:$PATH" MOCK_LOG="$MOCK_LOG" \
	MOCK_LIPO_MODE="${MOCK_LIPO_OVERRIDE:-match}" \
	bash "$ASSEMBLE" "$binary" "$icns" "$version" "$arch" "$out"
}

# --- assemble-darwin-app.sh -------------------------------------------------

mkdir -p "$WORK/bin-amd64" "$WORK/bin-arm64"
make_binary "$WORK/bin-amd64/mora" "$VERSION"
make_binary "$WORK/bin-arm64/mora" "$VERSION"

OUT_A="$WORK/assemble-a"
OUT_B="$WORK/assemble-b"
mkdir -p "$OUT_A" "$OUT_B"
: >"$MOCK_LOG"
# shellcheck disable=SC2016 # the PATH/MOCK segments are intentionally literal
expect_pass "assembly produces the frozen Mora.app layout" \
	env SOURCE_DATE_EPOCH=1753900000 bash -c \
	'PATH="'"$MOCK_BIN"':$PATH" MOCK_LOG="'"$MOCK_LOG"'" bash "'"$ASSEMBLE"'" "'"$WORK"'/bin-arm64/mora" "'"$ICNS"'" "'"$VERSION"'" arm64 "'"$OUT_A"'"'
APP="$OUT_A/Mora.app"
[ -x "$APP/Contents/MacOS/mora" ] || die "assembled bundle lacks executable Contents/MacOS/mora"
[ -f "$APP/Contents/Resources/Mora.icns" ] || die "assembled bundle lacks Contents/Resources/Mora.icns"
cmp -s "$APP/Contents/Resources/Mora.icns" "$ICNS" || die "bundle icon differs from the generated Mora.icns"
plutil -lint "$APP/Contents/Info.plist" >/dev/null || die "assembled Info.plist does not lint"
for kv in \
	"CFBundleIdentifier=com.pyranthus.mora" \
	"CFBundleName=Mora" \
	"CFBundleDisplayName=Mora" \
	"CFBundleExecutable=mora" \
	"CFBundleIconFile=Mora" \
	"CFBundlePackageType=APPL" \
	"CFBundleShortVersionString=$VERSION" \
	"CFBundleVersion=$VERSION"; do
	got="$(plutil -extract "${kv%%=*}" raw -o - "$APP/Contents/Info.plist")"
	[ "$got" = "${kv#*=}" ] || die "Info.plist ${kv%%=*} = '$got', want '${kv#*=}'"
done
pass "Info.plist freezes identifier, names, executable, icon, and release version"

# shellcheck disable=SC2016 # the PATH/MOCK segments are intentionally literal
expect_pass "assembly is deterministic under SOURCE_DATE_EPOCH" \
	env SOURCE_DATE_EPOCH=1753900000 bash -c \
	'PATH="'"$MOCK_BIN"':$PATH" MOCK_LOG="'"$MOCK_LOG"'" bash "'"$ASSEMBLE"'" "'"$WORK"'/bin-arm64/mora" "'"$ICNS"'" "'"$VERSION"'" arm64 "'"$OUT_B"'"'
diff -r "$OUT_A/Mora.app" "$OUT_B/Mora.app" >/dev/null || die "two assemblies of identical inputs differ"
pass "two assemblies of identical inputs are content-identical"

expect_fail "assembly refuses to overwrite an existing bundle" \
	run_assemble "$WORK/bin-arm64/mora" "$ICNS" "$VERSION" arm64 "$OUT_A"
expect_fail "assembly refuses a missing binary" \
	run_assemble "$WORK/missing/mora" "$ICNS" "$VERSION" arm64 "$WORK/assemble-x1"
expect_fail "assembly refuses a bad version string" \
	run_assemble "$WORK/bin-arm64/mora" "$ICNS" "v$VERSION" arm64 "$WORK/assemble-x2"
expect_fail "assembly refuses a non-semver version" \
	run_assemble "$WORK/bin-arm64/mora" "$ICNS" "0.11" arm64 "$WORK/assemble-x3"
expect_fail "assembly refuses an Apple-invalid prerelease bundle version" \
	run_assemble "$WORK/bin-arm64/mora" "$ICNS" "${VERSION}-rc.1" arm64 "$WORK/assemble-x3-prerelease"
expect_fail "assembly refuses an unknown architecture" \
	run_assemble "$WORK/bin-arm64/mora" "$ICNS" "$VERSION" armv7 "$WORK/assemble-x4"
printf 'not an icon\n' >"$WORK/bogus.icns"
expect_fail "assembly refuses a non-ICNS icon" \
	run_assemble "$WORK/bin-arm64/mora" "$WORK/bogus.icns" "$VERSION" arm64 "$WORK/assemble-x5"
MOCK_LIPO_OVERRIDE=wrong_arch expect_fail "assembly refuses a wrong-architecture binary" \
	run_assemble "$WORK/bin-arm64/mora" "$ICNS" "$VERSION" arm64 "$WORK/assemble-x6"
MOCK_LIPO_OVERRIDE=universal expect_fail "assembly refuses a universal (fat) binary" \
	run_assemble "$WORK/bin-arm64/mora" "$ICNS" "$VERSION" arm64 "$WORK/assemble-x7"
MOCK_LIPO_OVERRIDE=fail expect_fail "assembly refuses an unreadable Mach-O" \
	run_assemble "$WORK/bin-arm64/mora" "$ICNS" "$VERSION" arm64 "$WORK/assemble-x8"

# --- verify-app-zip.sh ------------------------------------------------------

GOOD_ZIP="$WORK/good_app.zip"
(cd "$OUT_A" && /usr/bin/ditto -c -k --norsrc --keepParent Mora.app "$GOOD_ZIP")
expect_pass "zip safety gate accepts the genuine bundle zip" bash "$VERIFY_ZIP" "$GOOD_ZIP"

hostile_zip() {
	local path="$1" mode="$2"
	python3 - "$path" "$mode" <<'PYEOF'
import sys, zipfile
path, mode = sys.argv[1], sys.argv[2]
z = zipfile.ZipFile(path, "w")
for name in ("Mora.app/Contents/Info.plist",
             "Mora.app/Contents/MacOS/mora",
             "Mora.app/Contents/Resources/Mora.icns"):
    if mode == "missing_member" and name.endswith(".icns"):
        continue
    z.writestr(name, "fixture")
if mode == "traversal":
    z.writestr("Mora.app/../evil", "fixture")
elif mode == "absolute":
    z.writestr("/tmp/evil", "fixture")
elif mode == "outside":
    z.writestr("Evil.app/payload", "fixture")
elif mode == "symlink":
    info = zipfile.ZipInfo("Mora.app/Contents/link")
    info.external_attr = (0o120777 << 16)
    z.writestr(info, "/etc/passwd")
z.close()
PYEOF
}

for mode in traversal absolute outside symlink missing_member; do
	hostile_zip "$WORK/hostile_$mode.zip" "$mode"
	expect_fail "zip safety gate refuses $mode zip" bash "$VERIFY_ZIP" "$WORK/hostile_$mode.zip"
done
expect_fail "zip safety gate refuses a missing zip" bash "$VERIFY_ZIP" "$WORK/nope.zip"

# --- appbundle-darwin-release.sh --------------------------------------------

AUTH_KEY="$WORK/AuthKey_TEST123456.p8"
printf '%s\n' 'fixture-not-a-secret' >"$AUTH_KEY"

make_dist() {
	local dir="$1" version="$2" binary_version="${3:-$2}" arches="${4:-amd64 arm64}" arch stage
	rm -rf "$dir"
	mkdir -p "$dir"
	for arch in $arches; do
		stage="$WORK/dist-stage-$arch"
		rm -rf "$stage"
		mkdir -p "$stage"
		make_binary "$stage/mora" "$binary_version"
		tar -czf "$dir/mora_${version}_darwin_${arch}.tar.gz" -C "$stage" mora
	done
	(cd "$dir" && shasum -a 256 mora_*.tar.gz >checksums.txt)
}

run_app_release() {
	local dist="$1"
	shift
	PATH="$MOCK_BIN:$PATH" \
	MOCK_LOG="$MOCK_LOG" \
	MOCK_NOTARY_MODE="${MOCK_NOTARY_MODE:-accepted}" \
	MOCK_CODESIGN_MODE="${MOCK_CODESIGN_MODE:-valid}" \
	MOCK_STAPLE_MODE="${MOCK_STAPLE_MODE:-accepted}" \
	MOCK_STAPLE_VALIDATE_MODE="${MOCK_STAPLE_VALIDATE_MODE:-accepted}" \
	MOCK_SPCTL_MODE="${MOCK_SPCTL_MODE:-accepted}" \
	MOCK_QUARANTINE_MODE="${MOCK_QUARANTINE_MODE:-accepted}" \
	MOCK_LAUNCH_MODE="${MOCK_LAUNCH_MODE:-accepted}" \
	MOCK_LIPO_MODE="${MOCK_LIPO_MODE:-match}" \
	APPLE_NOTARY_KEY_PATH="$AUTH_KEY" \
	APPLE_NOTARY_KEY_ID="TEST123456" \
	APPLE_NOTARY_ISSUER_ID="00000000-0000-0000-0000-000000000000" \
	APPLE_SIGNING_IDENTITY="Developer ID Application: ADIT ABHIJIT KARODE (VS8M5VJBZ5)" \
	APPLE_BUNDLE_ID="com.pyranthus.mora" \
	APPLE_TEAM_ID="VS8M5VJBZ5" \
	bash "$APP_RELEASE" "$dist" "${1:-$VERSION}"
}

DIST="$WORK/dist-complete"
make_dist "$DIST" "$VERSION"

expect_fail "app release requires Apple signing env" env \
	PATH="$MOCK_BIN:$PATH" MOCK_LOG="$MOCK_LOG" \
	APPLE_BUNDLE_ID="com.pyranthus.mora" APPLE_TEAM_ID="VS8M5VJBZ5" \
	bash "$APP_RELEASE" "$DIST" "$VERSION"

: >"$MOCK_LOG"
expect_pass "both Mora.app bundles sign, notarize, staple, and package" \
	run_app_release "$DIST"
for arch in amd64 arm64; do
	ASSET="$DIST/app/mora_${VERSION}_darwin_${arch}_app.zip"
	[ -f "$ASSET" ] || die "missing packaged asset: $ASSET"
	bash "$VERIFY_ZIP" "$ASSET" >/dev/null || die "packaged $arch asset fails the zip safety gate"
	# Consume zipinfo fully before grepping: grep -q would SIGPIPE the
	# producer under pipefail (the Tier 1 `head` hazard, same shape).
	asset_entries="$(zipinfo -1 "$ASSET")"
	grep -Fxq "Mora.app/Contents/CodeResources" <<<"$asset_entries" || \
		die "packaged $arch asset lacks the stapled ticket — the asset was zipped before stapling"
	grep -Fxq "Mora.app/Contents/_CodeSignature/CodeResources" <<<"$asset_entries" || \
		die "packaged $arch asset lacks the sealed bundle signature"
	grep -E "  mora_${VERSION}_darwin_${arch}_app\.zip\$" "$DIST/app/checksums-app.txt" >/dev/null || \
		die "checksums-app.txt lacks the $arch asset entry"
	expected="$(awk -v n="mora_${VERSION}_darwin_${arch}_app.zip" '$2 == n { print $1 }' "$DIST/app/checksums-app.txt")"
	actual="$(shasum -a 256 "$ASSET" | awk '{ print $1 }')"
	[ "$expected" = "$actual" ] || die "checksums-app.txt hash for $arch does not match the asset"
done
assert_log_contains $'--sign\tDeveloper ID Application: ADIT ABHIJIT KARODE (VS8M5VJBZ5)' \
	"whole-bundle signing must use the pinned Developer ID identity"
grep -E $'codesign\t--force\t--sign\t[^\t]+\t--identifier\tcom\\.pyranthus\\.mora\t--options\truntime\t--timestamp\t.*/Mora\\.app$' \
	"$MOCK_LOG" >/dev/null || die "codesign must target the WHOLE Mora.app bundle with identifier, runtime, and timestamp"
assert_log_contains $'xcrun\tnotarytool\tsubmit\t' "app bundles must be submitted to Apple notary"
assert_log_contains $'xcrun\tstapler\tstaple\t' "notarized app bundles must be stapled"
assert_log_contains $'xcrun\tstapler\tvalidate\t' "stapled tickets must be validated"
assert_log_contains $'spctl\t--assess\t--type\texecute\t' "Gatekeeper must assess the stapled app bundle"
if ! awk -F '\t' '
	$1 == "xcrun" && $2 == "notarytool" && $3 == "submit" { submits++ }
	$1 == "xcrun" && $2 == "stapler" && $3 == "staple" { if (submits < staples + 1) exit 1; staples++ }
	$1 == "ditto" && $NF ~ /_app\.zip$/ { if (staples < assets + 1) exit 1; assets++ }
	END { if (submits != 2 || staples != 2 || assets != 2) exit 1 }
' "$MOCK_LOG"; then
	sed -n '1,240p' "$MOCK_LOG" >&2
	die "per-arch order must be notarize -> staple -> final asset zip, for both architectures"
fi
require_text "$WORK/stdout" 'launch: skipped non-native Darwin amd64 app bundle on arm64 runner' \
	"the non-native architecture must be skipped explicitly"
require_text "$WORK/stdout" 'launch: quarantined Darwin arm64 app bundle executed successfully' \
	"the native architecture must complete the quarantined app launch"
if ! awk -F '\t' '
	$1 == "ditto" && $2 == "-x" && $3 == "-k" && $4 ~ /_app\.zip$/ && $5 ~ /final-verify-/ { verified++ }
	END { exit(verified == 2 ? 0 : 1) }
' "$MOCK_LOG"; then
	sed -n '1,280p' "$MOCK_LOG" >&2
	die "both final app ZIPs must be extracted and revalidated before checksumming"
fi
pass "both final ZIPs are re-extracted, trust-validated, checksummed, path-safe, and legacy-unselectable"

DIST_ONE="$WORK/dist-one"
make_dist "$DIST_ONE" "$VERSION" "$VERSION" "arm64"
: >"$MOCK_LOG"
expect_fail "missing darwin/amd64 raw archive" run_app_release "$DIST_ONE"
[ ! -s "$MOCK_LOG" ] || die "app pipeline started before the two-architecture inventory was complete"

make_dist "$DIST" "$VERSION"
expect_fail "wrong release version argument" run_app_release "$DIST" "0.12.0"
expect_fail "malformed release version argument" run_app_release "$DIST" "not-a-version"
expect_fail "prerelease tag cannot enter the stable app lane" run_app_release "$DIST" "${VERSION}-rc.1"

rm "$DIST/checksums.txt"
expect_fail "missing checksums.txt" run_app_release "$DIST"
make_dist "$DIST" "$VERSION"
printf '\nchecksum sabotage\n' >>"$DIST/mora_${VERSION}_darwin_amd64.tar.gz"
expect_fail "raw archive checksum mismatch" run_app_release "$DIST"

make_dist "$DIST" "$VERSION" "0.0.1"
expect_fail "app bundle reporting the wrong binary version" run_app_release "$DIST"

make_dist "$DIST" "$VERSION"
for mode in wrong_identifier wrong_team missing_team missing_runtime missing_timestamp verify_failure; do
	MOCK_CODESIGN_MODE="$mode" expect_fail "bundle signature metadata mode=$mode" run_app_release "$DIST"
done
for mode in invalid malformed submit_failure; do
	MOCK_NOTARY_MODE="$mode" expect_fail "app notarytool response mode=$mode" run_app_release "$DIST"
done
MOCK_STAPLE_MODE=noop expect_fail "silent no-op staple (exit 0, no ticket)" run_app_release "$DIST"
MOCK_STAPLE_MODE=fail expect_fail "staple failure" run_app_release "$DIST"
MOCK_STAPLE_VALIDATE_MODE=rejected expect_fail "staple validation failure" run_app_release "$DIST"
MOCK_SPCTL_MODE=rejected expect_fail "Gatekeeper rejection of the stapled app" run_app_release "$DIST"
MOCK_QUARANTINE_MODE=rejected expect_fail "quarantine attribute write failure" run_app_release "$DIST"
MOCK_LAUNCH_MODE=rejected expect_fail "quarantined app launch failure" run_app_release "$DIST"
MOCK_LIPO_MODE=wrong_arch expect_fail "wrong-architecture CLI inside the raw archive" run_app_release "$DIST"
MOCK_DITTO_MODE=drop_ticket expect_fail "final ZIP missing stapled ticket" run_app_release "$DIST"
MOCK_DITTO_MODE=drop_signature expect_fail "final ZIP missing sealed signature" run_app_release "$DIST"

# --- legacy asset-name guard (suffix sabotage) ------------------------------
# go-selfupdate lowercases every asset name and selects one that ENDS WITH
# `<os><sep><arch><ext>` (detect.go getSuffixes/assetMatchSuffixes, pinned at
# v1.5.2). The guard must refuse every name a legacy client could select and
# everything outside the frozen lowercase contract.

check_name() { bash "$APP_RELEASE" --check-asset-name "$1"; }

expect_pass "frozen amd64 app asset name is accepted" \
	check_name "mora_${VERSION}_darwin_amd64_app.zip"
expect_pass "frozen arm64 app asset name is accepted" \
	check_name "mora_${VERSION}_darwin_arm64_app.zip"
for bad in \
	"mora_${VERSION}_darwin_amd64.zip" \
	"mora_${VERSION}_darwin_arm64.zip" \
	"mora_${VERSION}_darwin-arm64.tar.gz" \
	"mora_${VERSION}_darwin_arm64" \
	"mora_app_${VERSION}_darwin_arm64.gz" \
	"MORA_${VERSION}_DARWIN_ARM64.ZIP" \
	"Mora_${VERSION}_darwin_arm64_app.zip" \
	"mora_${VERSION}-rc.1_darwin_arm64_app.zip" \
	"mora_${VERSION}_linux_arm64_app.zip" \
	"mora_darwin_arm64_app.zip"; do
	expect_fail "legacy-selectable or off-contract asset name: $bad" check_name "$bad"
done
pass "asset-name guard refuses every legacy-matcher suffix and pins the lowercase _app.zip contract"

# --- static wiring contracts ------------------------------------------------

require_text "$APP_RELEASE" '--options[[:space:]]+\\?$|--options' "app signer must enable hardened runtime"
require_text "$APP_RELEASE" '--timestamp' "app signer must request a trusted timestamp"
require_text "$APP_RELEASE" 'stapler[[:space:]]+staple' "app pipeline must staple the notary ticket"
require_text "$APP_RELEASE" 'stapler[[:space:]]+validate' "app pipeline must validate the stapled ticket"
require_text "$APP_RELEASE" 'spctl[[:space:]]+--assess[[:space:]]+--type[[:space:]]+execute' \
	"app pipeline must take Gatekeeper's verdict on the bundle"
require_text "$APP_RELEASE" 'verify-app-zip\.sh' "app pipeline must run the zip path-safety gate"
require_text "$APP_RELEASE" '_app\\?\.zip' "app assets must use the _app.zip name contract"
assert_before "$APP_RELEASE" 'stapler staple' 'verify-app-zip\.sh' \
	"the published asset must be produced from the stapled bundle"

require_text "$WORKFLOW" 'scripts/appbundle-darwin-release\.sh' \
	"release workflow must run the app bundle pipeline"
assert_before "$WORKFLOW" 'goreleaser/goreleaser-action' 'scripts/appbundle-darwin-release\.sh' \
	"the app pipeline must consume GoReleaser's built artifacts"
assert_before "$WORKFLOW" 'scripts/notarize-darwin-release\.sh' 'scripts/appbundle-darwin-release\.sh' \
	"the raw-CLI notarization contract must be proven before the app lane"
assert_before "$WORKFLOW" 'scripts/appbundle-darwin-release\.sh' 'name:[[:space:]]+Sign app checksums manifest' \
	"the app manifest must exist before it is cosign-signed"
assert_before "$WORKFLOW" 'name:[[:space:]]+Sign app checksums manifest' 'name:[[:space:]]+Upload Mora\.app assets' \
	"the cosign signature must exist before the upload"
assert_before "$WORKFLOW" 'name:[[:space:]]+Upload Mora\.app assets' 'name:[[:space:]]+Remove Apple signing credentials' \
	"the app lane must finish before Apple credentials are removed"
assert_before "$WORKFLOW" 'name:[[:space:]]+Upload Mora\.app assets' 'name:[[:space:]]+Publish release' \
	"an unverified app asset must never reach a published release"
require_text "$WORKFLOW" '_app\.zip' "release workflow must upload the _app.zip assets"
require_text "$WORKFLOW" 'cosign[[:space:]]+sign-blob' \
	"release workflow must cosign the app checksums manifest"
require_text "$WORKFLOW" 'checksums-app\.txt\.cosign\.sig' \
	"release workflow must upload the app manifest cosign signature"
require_text "$WORKFLOW" 'checksums-app\.txt\.cosign\.pem' \
	"release workflow must upload the app manifest cosign certificate"
pass "workflow orders the app lane after GoReleaser, before credential cleanup and publish"

# The raw-CLI archives are a frozen contract; the app lane must not have
# touched their GoReleaser name template or added an ad-hoc/quarantine bypass.
GORELEASER="$ROOT/.goreleaser.yaml"
require_text "$GORELEASER" '\{\{ \.ProjectName \}\}_\{\{ \.Version \}\}_\{\{ \.Os \}\}_\{\{ \.Arch \}\}' \
	"raw archive name_template must stay frozen for legacy self-update"
for surface in "$ASSEMBLE" "$APP_RELEASE" "$VERIFY_ZIP"; do
	if grep -En -- 'codesign[^#]*--sign(=|[[:space:]])-' "$surface" >"$WORK/forbidden"; then
		cat "$WORK/forbidden" >&2
		die "app surface contains ad-hoc signing: $surface"
	fi
	if grep -En -- '^[[:space:]]*xattr[[:space:]]+-d(r)?[[:space:]].*com\.apple\.quarantine' "$surface" >"$WORK/forbidden"; then
		cat "$WORK/forbidden" >&2
		die "app surface strips the quarantine attribute: $surface"
	fi
done
pass "raw archive contract untouched; no ad-hoc signing or quarantine stripping in the app lane"

printf '\napp-bundle contract: PASS\n'
