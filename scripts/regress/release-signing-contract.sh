#!/usr/bin/env bash
# Secret-free contract gate for the Developer-ID-signed standalone bridge.
#
# This harness replaces Apple tooling with deterministic PATH fixtures. It
# proves that the release scripts fail closed on signature/notarization drift
# without importing a certificate or sending a submission to Apple. The real
# macOS release job remains responsible for the live Developer ID + notary gate.
set -euo pipefail

ROOT="$(unset CDPATH; cd -- "$(dirname -- "$0")/../.." && pwd)"
SIGN="$ROOT/scripts/codesign-darwin.sh"
NOTARIZE="$ROOT/scripts/notarize-darwin-release.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/mora-release-signing-contract.XXXXXX")"
MOCK_BIN="$WORK/mock-bin"
MOCK_LOG="$WORK/mock-calls.log"

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
		sed -n '1,160p' "$MOCK_LOG" >&2
		die "$label: mock call log lacks: $needle"
	}
}

assert_log_count() {
	local needle="$1" want="$2" label="$3" got
	got="$(grep -F -c -- "$needle" "$MOCK_LOG" || true)"
	[ "$got" -eq "$want" ] || {
		sed -n '1,160p' "$MOCK_LOG" >&2
		die "$label: expected $want call(s) containing '$needle', got $got"
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

[ -f "$SIGN" ] || die "missing signing helper: $SIGN"
[ -f "$NOTARIZE" ] || die "missing notarization helper: $NOTARIZE"
bash -n "$SIGN" "$NOTARIZE" "$0" || die "release signing shell scripts must parse under bash"

mkdir -p "$MOCK_BIN"

# Mock codesign has two jobs: record the exact signing invocation, and emulate
# the metadata emitted by `codesign -d`. Negative modes deliberately return
# exit zero: production must inspect the identity/runtime/timestamp, not merely
# trust that the codesign process ran. The separate notarized-requirement mode
# models Apple's ticket lookup after notarytool reports Accepted.
cat >"$MOCK_BIN/codesign" <<'MOCK_CODESIGN'
#!/usr/bin/env bash
set -euo pipefail
printf 'codesign' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"

display=0
requirements=0
notarized_requirement=0
for arg in "$@"; do
	case "$arg" in -d*|--display) display=1 ;; esac
	case "$arg" in -R=notarized|--test-requirement=notarized) notarized_requirement=1 ;; esac
	case "$arg" in --requirements|-r-) requirements=1 ;; esac
done

if [ "$notarized_requirement" -eq 1 ]; then
	[ "${MOCK_TICKET_MODE:-accepted}" = "accepted" ]
	exit
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
		printf 'Executable=/fixture/mora\n' >&2
		printf 'designated => identifier "%s" and anchor apple generic and certificate leaf[subject.OU] = %s\n' \
			"$identifier" "$team" >&2
	else
		{
			printf 'Executable=/fixture/mora\n'
			printf 'Identifier=%s\n' "$identifier"
			printf 'Format=Mach-O thin (arm64)\n'
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

cat >"$MOCK_BIN/xcrun" <<'MOCK_XCRUN'
#!/usr/bin/env bash
set -euo pipefail
printf 'xcrun' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"

case "${MOCK_NOTARY_MODE:-accepted}" in
	accepted) printf '{"id":"00000000-0000-0000-0000-000000000001","status":"Accepted"}\n' ;;
	invalid) printf '{"id":"00000000-0000-0000-0000-000000000002","status":"Invalid"}\n' ;;
	malformed) printf '{"id":"00000000-0000-0000-0000-000000000003"}\n' ;;
	submit_failure) printf 'network failure\n' >&2; exit 1 ;;
	*) printf 'unknown fixture mode\n' >&2; exit 2 ;;
esac
MOCK_XCRUN

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
[ -f "$target" ] || exit 2
: >"${target}.mora-quarantined"
MOCK_XATTR

cat >"$MOCK_BIN/sleep" <<'MOCK_SLEEP'
#!/usr/bin/env bash
set -euo pipefail
printf 'sleep' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"
MOCK_SLEEP

# Pin the fixture host so the contract deterministically launches arm64 and
# explicitly skips amd64. The live macos-15 release runner supplies the real
# native architecture.
cat >"$MOCK_BIN/uname" <<'MOCK_UNAME'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "-m" ]; then
	printf 'arm64\n'
	exit 0
fi
/usr/bin/uname "$@"
MOCK_UNAME

# notarytool accepts a zip container, while the release asset remains a tar.gz.
# The fixture need only materialize the requested output; no bytes leave WORK.
cat >"$MOCK_BIN/ditto" <<'MOCK_DITTO'
#!/usr/bin/env bash
set -euo pipefail
printf 'ditto' >>"$MOCK_LOG"
for arg in "$@"; do printf '\t%s' "$arg" >>"$MOCK_LOG"; done
printf '\n' >>"$MOCK_LOG"
src="${@: -2:1}"
dst="${@: -1}"
cp "$src" "$dst"
MOCK_DITTO

chmod +x "$MOCK_BIN/codesign" "$MOCK_BIN/xcrun" "$MOCK_BIN/xattr" "$MOCK_BIN/sleep" "$MOCK_BIN/uname" "$MOCK_BIN/ditto"

run_sign() {
	local artifact="$1" goos="$2" mode="${3:-valid}"
	PATH="$MOCK_BIN:$PATH" \
	MOCK_LOG="$MOCK_LOG" \
	MOCK_CODESIGN_MODE="$mode" \
	APPLE_SIGNING_IDENTITY="Developer ID Application: ADIT ABHIJIT KARODE (VS8M5VJBZ5)" \
	APPLE_BUNDLE_ID="com.pyranthus.mora" \
	APPLE_TEAM_ID="VS8M5VJBZ5" \
	bash "$SIGN" "$artifact" "$goos"
}

printf '#!/bin/sh\nexit 0\n' >"$WORK/mora-amd64"
printf '#!/bin/sh\nexit 0\n' >"$WORK/mora-arm64"
chmod +x "$WORK/mora-amd64" "$WORK/mora-arm64"

: >"$MOCK_LOG"
expect_pass "darwin/amd64 signs with the pinned Developer ID contract" \
	run_sign "$WORK/mora-amd64" darwin_amd64
expect_pass "darwin/arm64 signs with the pinned Developer ID contract" \
	run_sign "$WORK/mora-arm64" darwin_arm64
assert_log_contains $'--sign\tDeveloper ID Application: ADIT ABHIJIT KARODE (VS8M5VJBZ5)' \
	"signer must use the supplied Developer ID identity"
if ! grep -E -- '--identifier(=|[[:space:]])com\.pyranthus\.mora' "$MOCK_LOG" >/dev/null 2>&1; then
	die "signer must pass the explicit identifier com.pyranthus.mora"
fi
if ! grep -E -- '--options(=|[[:space:]])runtime' "$MOCK_LOG" >/dev/null 2>&1; then
	die "signer must enable hardened runtime"
fi
require_text "$MOCK_LOG" '--timestamp([[:space:]]|$|=)' "signer must request a trusted timestamp"
assert_log_count $'codesign\t' 8 "both Darwin architectures must be signed and fully inspected"
pass "signing invocation pins identity, identifier, hardened runtime, timestamp, and both arches"

: >"$MOCK_LOG"
expect_pass "non-Darwin artifacts are not passed to Apple codesign" \
	run_sign "$WORK/mora-amd64" linux
[ ! -s "$MOCK_LOG" ] || die "signer invoked codesign for a non-Darwin artifact"

expect_fail "missing signing identity" env \
	PATH="$MOCK_BIN:$PATH" MOCK_LOG="$MOCK_LOG" \
	APPLE_BUNDLE_ID="com.pyranthus.mora" APPLE_TEAM_ID="VS8M5VJBZ5" \
	bash "$SIGN" "$WORK/mora-amd64" darwin

for mode in wrong_identifier wrong_team missing_team missing_runtime missing_timestamp verify_failure; do
	expect_fail "codesign metadata mode=$mode" \
		run_sign "$WORK/mora-amd64" darwin "$mode"
done

make_dist() {
	local dir="$1" arches="$2" arch stage
	mkdir -p "$dir"
	for arch in $arches; do
		stage="$WORK/stage-$arch"
		rm -rf "$stage"
		mkdir -p "$stage"
		# shellcheck disable=SC2016 # fixture expands these variables only when launched
		printf '%s\n' \
			'#!/bin/sh' \
			'case "$0" in */amd64/mora|*/arm64/mora) exit 91 ;; esac' \
			'[ -f "$0.mora-quarantined" ] || exit 92' \
			"printf 'launch\\t${arch}\\t%s\\n' \"\$0\" >>\"\$MOCK_LOG\"" \
			'[ "${MOCK_LAUNCH_MODE:-accepted}" = accepted ]' >"$stage/mora"
		chmod +x "$stage/mora"
		tar -czf "$dir/mora_0.11.2_darwin_${arch}.tar.gz" -C "$stage" mora
	done
}

AUTH_KEY="$WORK/AuthKey_TEST123456.p8"
printf '%s\n' 'fixture-not-a-secret' >"$AUTH_KEY"

run_notarize() {
	local dist="$1" notary_mode="${2:-accepted}" codesign_mode="${3:-valid}"
	local ticket_mode="${4:-accepted}" quarantine_mode="${5:-accepted}" launch_mode="${6:-accepted}"
	PATH="$MOCK_BIN:$PATH" \
	MOCK_LOG="$MOCK_LOG" \
	MOCK_NOTARY_MODE="$notary_mode" \
	MOCK_CODESIGN_MODE="$codesign_mode" \
	MOCK_TICKET_MODE="$ticket_mode" \
	MOCK_QUARANTINE_MODE="$quarantine_mode" \
	MOCK_LAUNCH_MODE="$launch_mode" \
	APPLE_NOTARY_KEY_PATH="$AUTH_KEY" \
	APPLE_NOTARY_KEY_ID="TEST123456" \
	APPLE_NOTARY_ISSUER_ID="00000000-0000-0000-0000-000000000000" \
	APPLE_BUNDLE_ID="com.pyranthus.mora" \
	APPLE_TEAM_ID="VS8M5VJBZ5" \
	bash "$NOTARIZE" "$dist"
}

DIST="$WORK/dist-complete"
make_dist "$DIST" "amd64 arm64"

expect_fail "missing Apple notary credentials" env \
	PATH="$MOCK_BIN:$PATH" MOCK_LOG="$MOCK_LOG" \
	APPLE_BUNDLE_ID="com.pyranthus.mora" APPLE_TEAM_ID="VS8M5VJBZ5" \
	bash "$NOTARIZE" "$DIST"

: >"$MOCK_LOG"
expect_pass "both Darwin release archives require an Accepted notarization" \
	run_notarize "$DIST"
assert_log_count $'xcrun\tnotarytool\tsubmit\t' 2 \
	"notarytool must receive both Darwin architecture submissions"
assert_log_count $'\t-R=notarized\t' 2 \
	"Apple's notarized code requirement must pass for both Darwin binaries"
assert_log_count $'xattr\t-w\tcom.apple.quarantine\t' 1 \
	"the native disposable launch copy must carry a quarantine attribute"
assert_log_count $'launch\t' 1 \
	"the native quarantined disposable copy must launch successfully"
require_text "$WORK/stdout" 'launch: skipped non-native Darwin amd64 binary on arm64 runner' \
	"the non-native architecture must be skipped explicitly"
require_text "$WORK/stdout" 'launch: quarantined Darwin arm64 binary executed successfully' \
	"the native architecture must complete the quarantined launch"
pass "both arches satisfy Apple's ticket requirement; native quarantine launch passes and non-native skips"

DIST_ONE="$WORK/dist-one-arm64"
make_dist "$DIST_ONE" "arm64"
: >"$MOCK_LOG"
expect_fail "missing darwin/amd64 release archive" run_notarize "$DIST_ONE"
[ ! -s "$MOCK_LOG" ] || die "notarization started before the two-architecture inventory was complete"

DIST_ONE="$WORK/dist-one-amd64"
make_dist "$DIST_ONE" "amd64"
: >"$MOCK_LOG"
expect_fail "missing darwin/arm64 release archive" run_notarize "$DIST_ONE"

for mode in invalid malformed submit_failure; do
	expect_fail "notarytool response mode=$mode" run_notarize "$DIST" "$mode"
done
for mode in wrong_identifier wrong_team missing_team missing_runtime missing_timestamp verify_failure; do
	expect_fail "pre-publish signature metadata mode=$mode" \
		run_notarize "$DIST" accepted "$mode"
done
expect_fail "missing Apple ticket after Accepted notarization" \
	run_notarize "$DIST" accepted valid rejected
expect_fail "quarantine attribute write failure" \
	run_notarize "$DIST" accepted valid accepted rejected
expect_fail "quarantined disposable launch failure" \
	run_notarize "$DIST" accepted valid accepted accepted rejected

# Installer destination selection is security-sensitive because replacing a
# different `mora` than the one the shell currently resolves leaves the old FDA
# identity active and silently installs a second copy. Run the real installer
# under sandbox-exec: only WORK is writable, so even a regressed fallback to
# /usr/local/bin or /opt/homebrew/bin fails without touching the host.
INSTALLER="$ROOT/install.sh"
INSTALL_CASE="$WORK/installer-destination"
INSTALL_STAGE="$INSTALL_CASE/release"
INSTALL_HOME="$INSTALL_CASE/home"
INSTALL_TMPDIR="$INSTALL_CASE/tmp"
ACTIVE_DIR="$INSTALL_CASE/active bin"
SANDBOX_PROFILE="$INSTALL_CASE/installer.sb"
mkdir -p "$INSTALL_STAGE" "$INSTALL_HOME" "$INSTALL_TMPDIR" "$ACTIVE_DIR"
cp "$INSTALLER" "$INSTALL_STAGE/install.sh"

# A release-shaped executable sufficient for install.sh's post-copy init/config
# calls. Apple identity and notarization are exercised through the codesign mock.
cat >"$INSTALL_STAGE/mora" <<'INSTALL_FIXTURE'
#!/bin/sh
set -eu
case "${1:-}" in
	init) exit 0 ;;
	config) printf 'vault_dir = %s\n' "${MORA_VAULT:?}" ;;
	version) printf 'mora fixture-new\n' ;;
	*) exit 0 ;;
esac
INSTALL_FIXTURE
chmod +x "$INSTALL_STAGE/mora"

cat >"$ACTIVE_DIR/mora" <<'ACTIVE_FIXTURE'
#!/bin/sh
printf 'mora fixture-old\n'
ACTIVE_FIXTURE
chmod +x "$ACTIVE_DIR/mora"

WORK_REAL="$(cd "$WORK" && pwd -P)"
cat >"$SANDBOX_PROFILE" <<SANDBOX_PROFILE_EOF
(version 1)
(deny default)
(import "system.sb")
(allow file-read*)
(allow process*)
(allow file-write*
  (subpath "$WORK")
  (subpath "$WORK_REAL")
  (subpath "/tmp")
  (subpath "/private/tmp")
  (literal "/dev/null"))
SANDBOX_PROFILE_EOF

path_fingerprint() {
	local path="$1"
	if [ -L "$path" ]; then
		printf 'symlink:%s\n' "$(readlink "$path")"
	elif [ -f "$path" ]; then
		printf 'file:%s:' "$(stat -f '%i:%z' "$path")"
		shasum -a 256 "$path" | awk '{ print $1 }'
	elif [ -e "$path" ]; then
		printf 'other:%s\n' "$(stat -f '%HT:%i' "$path")"
	else
		printf 'ABSENT\n'
	fi
}

run_sandboxed_installer() {
	local active_dir="$1"
	/usr/bin/sandbox-exec -f "$SANDBOX_PROFILE" \
		/usr/bin/env -u PREFIX \
		PATH="$active_dir:$MOCK_BIN:/usr/bin:/bin:/usr/sbin:/sbin" \
		HOME="$INSTALL_HOME" \
		TMPDIR="$INSTALL_TMPDIR" \
		MOCK_LOG="$MOCK_LOG" \
		MOCK_CODESIGN_MODE=valid \
		MOCK_TICKET_MODE=accepted \
		MORA_CONFIG_DIR="$INSTALL_CASE/config" \
		MORA_VAULT="$INSTALL_CASE/vault" \
		/bin/sh "$INSTALL_STAGE/install.sh"
}

USR_LOCAL_BEFORE="$(path_fingerprint /usr/local/bin/mora)"
HOMEBREW_BEFORE="$(path_fingerprint /opt/homebrew/bin/mora)"
ACTIVE_INODE_BEFORE="$(stat -f '%i' "$ACTIVE_DIR/mora")"
: >"$MOCK_LOG"
expect_pass "installer replaces the regular Mora that is first on PATH when PREFIX is unset" \
	run_sandboxed_installer "$ACTIVE_DIR"
ACTIVE_INODE_AFTER="$(stat -f '%i' "$ACTIVE_DIR/mora")"
[ "$ACTIVE_INODE_AFTER" != "$ACTIVE_INODE_BEFORE" ] \
	|| die "installer overwrote the active Mora inode instead of replacing it atomically"
[ "$("$ACTIVE_DIR/mora" version)" = "mora fixture-new" ] \
	|| die "installer did not replace the exact active Mora path"
[ ! -e "$INSTALL_HOME/.local/bin/mora" ] \
	|| die "installer created a second Mora under HOME instead of replacing the active path"
[ "$(path_fingerprint /usr/local/bin/mora)" = "$USR_LOCAL_BEFORE" ] \
	|| die "sandboxed installer changed /usr/local/bin/mora"
[ "$(path_fingerprint /opt/homebrew/bin/mora)" = "$HOMEBREW_BEFORE" ] \
	|| die "sandboxed installer changed /opt/homebrew/bin/mora"
pass "active regular path was atomically replaced; system and fallback destinations were untouched"

# Homebrew exposes installed tools through symlinks into its managed prefix.
# Replacing the link itself would create an unmanaged shadow installation, so
# no-PREFIX mode must refuse and leave both the link and target byte-identical.
BREW_BIN="$INSTALL_CASE/homebrew/bin"
BREW_TARGET_DIR="$INSTALL_CASE/homebrew/Cellar/mora/0.11.1/bin"
mkdir -p "$BREW_BIN" "$BREW_TARGET_DIR"
cp "$ACTIVE_DIR/mora" "$BREW_TARGET_DIR/mora"
ln -s "$BREW_TARGET_DIR/mora" "$BREW_BIN/mora"
BREW_LINK_BEFORE="$(readlink "$BREW_BIN/mora")"
BREW_TARGET_BEFORE="$(path_fingerprint "$BREW_TARGET_DIR/mora")"
: >"$MOCK_LOG"
expect_fail "active Homebrew-style Mora symlink" run_sandboxed_installer "$BREW_BIN"
if ! grep -Eiq 'symlink|homebrew|brew|PREFIX' "$WORK/stdout" "$WORK/stderr"; then
	die "symlink refusal must explain Homebrew/symlink ownership or request an explicit PREFIX"
fi
[ -L "$BREW_BIN/mora" ] && [ "$(readlink "$BREW_BIN/mora")" = "$BREW_LINK_BEFORE" ] \
	|| die "installer replaced or retargeted the active Homebrew-style symlink"
[ "$(path_fingerprint "$BREW_TARGET_DIR/mora")" = "$BREW_TARGET_BEFORE" ] \
	|| die "installer modified the Homebrew-managed symlink target"
if find "$BREW_BIN" -maxdepth 1 -name '.mora.install.*' -print -quit | grep -q .; then
	die "symlink refusal left a staged installer artifact in the managed bin directory"
fi
pass "active Homebrew-style symlink is refused without mutation"

# Also refuse a regular executable resolved directly inside a Homebrew-managed
# Cellar path. This covers shells whose PATH includes a keg's bin directory
# rather than Homebrew's usual top-level symlink.
BREW_TARGET_BEFORE="$(path_fingerprint "$BREW_TARGET_DIR/mora")"
expect_fail "active regular Mora inside a Homebrew Cellar" \
	run_sandboxed_installer "$BREW_TARGET_DIR"
if ! grep -Eiq 'homebrew|brew upgrade|package manager' "$WORK/stdout" "$WORK/stderr"; then
	die "Cellar refusal must direct the user back to Homebrew"
fi
[ "$(path_fingerprint "$BREW_TARGET_DIR/mora")" = "$BREW_TARGET_BEFORE" ] \
	|| die "installer modified an active Homebrew Cellar executable"
if find "$BREW_TARGET_DIR" -maxdepth 1 -name '.mora.install.*' -print -quit | grep -q .; then
	die "Cellar refusal left a staged installer artifact in the managed directory"
fi
pass "active Homebrew Cellar executable is refused without mutation"

# Static release wiring contracts. These catch bypasses that a mocked happy path
# cannot: wrong runner, absent secrets, publishing before notarization, ad-hoc
# re-signing in the installer, or quarantine stripping in any install surface.
WORKFLOW="$ROOT/.github/workflows/release.yml"
GORELEASER="$ROOT/.goreleaser.yaml"
MACOS_REGRESSION="$ROOT/scripts/regress/regression-macos.sh"

require_text "$WORKFLOW" 'runs-on:[[:space:]]+macos-(latest|[0-9]+)' \
	"release signing/notarization must run on a macOS runner"
ACTION_COUNT="$(grep -Ec '^[[:space:]]*-[[:space:]]+uses:' "$WORKFLOW")"
PINNED_ACTION_COUNT="$(grep -Ec '^[[:space:]]*-[[:space:]]+uses:[^@]+@[0-9a-f]{40}([[:space:]]|$)' "$WORKFLOW")"
[ "$ACTION_COUNT" -gt 0 ] && [ "$PINNED_ACTION_COUNT" -eq "$ACTION_COUNT" ] || \
	die "every action in the Apple-credential release job must use a full commit SHA"
VERSIONED_ACTION_COUNT="$(grep -Ec 'uses:.*@[0-9a-f]{40}[[:space:]]+#[[:space:]]+v[0-9]' "$WORKFLOW")"
[ "$VERSIONED_ACTION_COUNT" -eq "$ACTION_COUNT" ] || \
	die "every immutable action pin must retain a human-readable version comment"
for secret in APPLE_CERT_P12_BASE64 APPLE_CERT_PASSWORD APPLE_API_KEY_P8 APPLE_API_KEY_ID APPLE_API_ISSUER_ID APPLE_TEAM_ID; do
	require_text "$WORKFLOW" "secrets\\.${secret}" "release workflow must consume $secret"
done
require_text "$WORKFLOW" 'APPLE_BUNDLE_ID:.*com\.pyranthus\.mora' \
	"release workflow must pin com.pyranthus.mora"
require_text "$WORKFLOW" 'APPLE_TEAM_ID:.*VS8M5VJBZ5' \
	"release workflow must pin Team ID VS8M5VJBZ5"
for keychain_step in create-keychain import set-key-partition-list delete-keychain; do
	require_text "$WORKFLOW" "security[[:space:]]+${keychain_step}" \
		"release workflow must use an ephemeral signing keychain ($keychain_step)"
done
require_text "$WORKFLOW" 'scripts/notarize-darwin-release\.sh' \
	"release workflow must run the fail-closed notarization helper"
assert_before "$WORKFLOW" 'goreleaser/goreleaser-action' 'scripts/notarize-darwin-release\.sh' \
	"notarization must inspect the built release artifacts"
assert_before "$WORKFLOW" 'scripts/notarize-darwin-release\.sh' 'name:[[:space:]]+Publish release' \
	"an unnotarized draft must never be published"
pass "workflow imports credentials ephemerally and notarizes before publish"

require_text "$GORELEASER" 'CGO_ENABLED=0' "release must remain pure Go"
require_text "$GORELEASER" 'goos:[[:space:]]*\[[^]]*darwin' \
	"GoReleaser must build Darwin"
require_text "$GORELEASER" 'goarch:[[:space:]]*\[[^]]*amd64[^]]*arm64' \
	"GoReleaser must build Darwin amd64 and arm64"
require_text "$GORELEASER" 'scripts/codesign-darwin\.sh' \
	"GoReleaser must sign each Darwin binary before archiving"
require_text "$GORELEASER" '\{\{[[:space:]]*\.Target[[:space:]]*\}\}' \
	"GoReleaser must tell the signing hook which target it built"

require_text "$NOTARIZE" 'codesign[[:space:]].*-R(=|[[:space:]])['"'"']?notarized' \
	"post-notary verification must evaluate Apple's notarized code requirement"
require_text "$NOTARIZE" 'xattr[[:space:]]+-w[[:space:]]+com\.apple\.quarantine' \
	"post-notary verification must quarantine its disposable launch copy"
if grep -En -- 'spctl[[:space:]].*--type[[:space:]]+install' "$NOTARIZE" >"$WORK/forbidden"; then
	cat "$WORK/forbidden" >&2
	die "raw CLI notarization must not rely on spctl --type install"
fi
require_text "$MACOS_REGRESSION" 'codesign[[:space:]].*-R(=|[[:space:]])['"'"']?notarized' \
	"live macOS regression must independently evaluate Apple's notarized requirement"
require_text "$MACOS_REGRESSION" 'xattr[[:space:]]+-w[[:space:]]+com\.apple\.quarantine' \
	"live macOS regression must exercise a quarantined disposable launch"
if grep -En -- 'spctl[[:space:]].*--type[[:space:]]+install' "$MACOS_REGRESSION" >"$WORK/forbidden"; then
	cat "$WORK/forbidden" >&2
	die "live macOS regression must not rely on spctl --type install"
fi

require_text "$INSTALLER" 'com\.pyranthus\.mora' \
	"installer must verify the pinned signing identifier"
require_text "$INSTALLER" 'VS8M5VJBZ5' \
	"installer must verify the pinned Apple Team ID"
require_text "$INSTALLER" 'codesign[[:space:]].*--verify' \
	"installer must verify rather than replace the release signature"
require_text "$INSTALLER" 'command[[:space:]]+-v[[:space:]]+mora' \
	"no-PREFIX installer must resolve the Mora that is active on PATH"
require_text "$INSTALLER" '(\[|test)[^#]*[[:space:]]-(L|h)[[:space:]]' \
	"no-PREFIX installer must detect and refuse an active symlink"
require_text "$INSTALLER" 'Cellar|Caskroom' \
	"no-PREFIX installer must refuse a Homebrew-managed active path"

RELEASE_SURFACES=("$INSTALLER" "$GORELEASER" "$WORKFLOW" "$SIGN" "$NOTARIZE")
if grep -En -- 'codesign[^#]*--sign(=|[[:space:]])-' "${RELEASE_SURFACES[@]}" >"$WORK/forbidden"; then
	cat "$WORK/forbidden" >&2
	die "release surface still contains ad-hoc signing"
fi
if grep -En -- '^[[:space:]]*xattr[[:space:]]+-d(r)?[[:space:]].*com\.apple\.quarantine' \
	"${RELEASE_SURFACES[@]}" >"$WORK/forbidden"; then
	cat "$WORK/forbidden" >&2
	die "release surface still strips the quarantine attribute"
fi
if grep -En -- '(system_command.*xattr|/usr/bin/xattr|args:[[:space:]]*\["-dr".*com\.apple\.quarantine)' \
	"$GORELEASER" >"$WORK/forbidden"; then
	cat "$WORK/forbidden" >&2
	die "Homebrew release surface still bypasses Gatekeeper with xattr"
fi
pass "installer and cask preserve the Apple signature and Gatekeeper path"

printf '\nrelease-signing contract: PASS\n'
