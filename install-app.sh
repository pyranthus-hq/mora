#!/usr/bin/env sh
# Install Mora's signed, notarized macOS application bundle without changing
# its bytes, quarantine state, signature, or stapled ticket. The stable PATH
# entry is a symlink into the bundle; future `mora upgrade` calls replace the
# whole bundle with Darwin's atomic directory-swap primitive.
set -eu

VERSION="${VERSION:-0.15.0}"
REPO="${REPO:-pyranthus-hq/mora}"
VAULT="${MORA_VAULT:-$HOME/vault/mora}"
APP_PARENT="${MORA_APP_DIR:-$HOME/Applications}"
APP_DEST="$APP_PARENT/Mora.app"
MACOS_IDENTIFIER="com.pyranthus.mora"
MACOS_TEAM_ID="VS8M5VJBZ5"
CHECKSUM_ASSET="checksums-app.txt"
MAX_APP_BYTES=536870912
MAX_CHECKSUM_BYTES=1048576
MAX_APP_ENTRIES=10000

DOWNLOAD_TMP=""
APP_STAGE_DIR=""
LINK_STAGE_DIR=""
APP_REPLACE_DIR=""

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
cleanup() {
	if [ -n "$APP_REPLACE_DIR" ] && [ -d "$APP_REPLACE_DIR/Mora.app" ]; then
		# Until replacement verification commits, the old app remains truth. If
		# an error or signal arrives after the new rename, move that uncommitted
		# bundle back into its disposable staging directory before restoring.
		if [ -e "$APP_DEST" ] || [ -L "$APP_DEST" ]; then
			INTERRUPTED_APP="$APP_STAGE_DIR/Mora.app"
			if [ -n "$APP_STAGE_DIR" ] && [ ! -e "$INTERRUPTED_APP" ] && [ ! -L "$INTERRUPTED_APP" ]; then
				mv "$APP_DEST" "$INTERRUPTED_APP" || true
			fi
		fi
		if [ ! -e "$APP_DEST" ] && [ ! -L "$APP_DEST" ] && mv "$APP_REPLACE_DIR/Mora.app" "$APP_DEST"; then
			rmdir "$APP_REPLACE_DIR" 2>/dev/null || true
			APP_REPLACE_DIR=""
			printf 'warning: interrupted replacement restored the previous Mora.app\n' >&2
		fi
	fi
	[ -z "$DOWNLOAD_TMP" ] || rm -rf "$DOWNLOAD_TMP"
	[ -z "$APP_STAGE_DIR" ] || rm -rf "$APP_STAGE_DIR"
	[ -z "$LINK_STAGE_DIR" ] || rm -rf "$LINK_STAGE_DIR"
	# Never delete APP_REPLACE_DIR from a signal/error trap: while replacement
	# is active it is the only copy of the user's previous app. Success and a
	# completed rollback remove it explicitly below.
	if [ -n "$APP_REPLACE_DIR" ] && [ -d "$APP_REPLACE_DIR/Mora.app" ]; then
		printf 'warning: previous Mora.app preserved for recovery at %s\n' "$APP_REPLACE_DIR/Mora.app" >&2
	fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf '%s\n' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || \
	die "VERSION must be a stable numeric release (X.Y.Z)"
printf '%s\n' "$REPO" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' || \
	die "REPO must be an owner/repository pair"

[ "$(uname -s)" = "Darwin" ] || die "Mora.app is available only on macOS; use install.sh on this platform"
command -v codesign >/dev/null 2>&1 || die "codesign is required to verify Mora.app"
command -v spctl >/dev/null 2>&1 || die "spctl is required to verify Mora.app"
command -v plutil >/dev/null 2>&1 || die "plutil is required to verify Mora.app"
command -v xcrun >/dev/null 2>&1 || die "xcrun is required to validate the stapled ticket"
command -v lipo >/dev/null 2>&1 || die "lipo is required to verify the app architecture"
command -v ditto >/dev/null 2>&1 || die "ditto is required to preserve the stapled ticket"
command -v zipinfo >/dev/null 2>&1 || die "zipinfo is required for safe archive inspection"
command -v unzip >/dev/null 2>&1 || die "unzip is required for safe archive inspection"

case "$APP_PARENT" in
	/*) : ;;
	*) die "MORA_APP_DIR must be an absolute path" ;;
esac
[ ! -L "$APP_PARENT" ] || die "application directory must not be a symlink: $APP_PARENT"
mkdir -p "$APP_PARENT"
[ -d "$APP_PARENT" ] && [ -w "$APP_PARENT" ] || die "application directory is not writable: $APP_PARENT"

case "$(uname -m)" in
	arm64|aarch64) ARCH=arm64; LIPO_ARCH=arm64 ;;
	x86_64|amd64) ARCH=amd64; LIPO_ARCH=x86_64 ;;
	*) die "unsupported macOS architecture: $(uname -m)" ;;
esac
ASSET="mora_${VERSION}_darwin_${ARCH}_app.zip"

sha256_of() {
	if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
	elif command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
	else return 1; fi
}

file_size() {
	if stat -f '%z' "$1" >/dev/null 2>&1; then stat -f '%z' "$1"
	else stat -c '%s' "$1"; fi
}

version_is_newer() {
	awk -v left="$1" -v right="$2" 'BEGIN {
		split(left, a, "."); split(right, b, ".")
		for (i = 1; i <= 3; i++) {
			if ((a[i] + 0) > (b[i] + 0)) exit 0
			if ((a[i] + 0) < (b[i] + 0)) exit 1
		}
		exit 1
	}'
}

plist_value() {
	plutil -extract "$2" raw -o - "$1/Contents/Info.plist" 2>/dev/null || return 1
}

verify_signing_target() {
	TARGET="$1"
	codesign --verify --strict --verbose=2 "$TARGET" >/dev/null 2>&1 || \
		die "invalid code signature on $TARGET"
	SIGN_INFO="$(codesign -dvvv "$TARGET" 2>&1)" || die "could not inspect code signature on $TARGET"
	printf '%s\n' "$SIGN_INFO" | grep -Fqx "Identifier=$MACOS_IDENTIFIER" || \
		die "$TARGET has the wrong signing identifier"
	printf '%s\n' "$SIGN_INFO" | grep -Fqx "TeamIdentifier=$MACOS_TEAM_ID" || \
		die "$TARGET has the wrong Apple team"
	printf '%s\n' "$SIGN_INFO" | grep -Fq 'Authority=Developer ID Application:' || \
		die "$TARGET is not Developer ID Application signed"
	printf '%s\n' "$SIGN_INFO" | grep -Eq '^CodeDirectory .*flags=.*\(runtime\)' || \
		die "$TARGET does not enable the hardened runtime"
	printf '%s\n' "$SIGN_INFO" | grep -Eq '^Timestamp=.+' || \
		die "$TARGET has no secure timestamp"
	REQUIREMENT="$(codesign -d -r- "$TARGET" 2>&1)" || die "could not inspect designated requirement on $TARGET"
	printf '%s\n' "$REQUIREMENT" | grep -Fq "identifier \"$MACOS_IDENTIFIER\"" || \
		die "$TARGET designated requirement has the wrong identifier"
	printf '%s\n' "$REQUIREMENT" | \
		grep -Eq "subject\\.OU[^=]*= (\"${MACOS_TEAM_ID}\"|${MACOS_TEAM_ID})( |$)" || \
		die "$TARGET designated requirement has the wrong Apple team"
}

# A damaged v0.12.0 bundle can fail strict verification after its legacy
# updater replaces only Contents/MacOS/mora. Before repairing that bundle, pin
# the surviving product identity so this installer can never overwrite an
# unrelated app that merely occupies the Mora.app path.
verify_existing_app_identity() {
	APP="$1"
	[ "$(plist_value "$APP" CFBundleIdentifier)" = "$MACOS_IDENTIFIER" ] || \
		die "refusing to replace an existing app with the wrong CFBundleIdentifier: $APP"
	[ "$(plist_value "$APP" CFBundleExecutable)" = "mora" ] || \
		die "refusing to replace an existing app with the wrong CFBundleExecutable: $APP"
	SIGN_INFO="$(codesign -dvvv "$APP" 2>&1)" || die "could not inspect the existing Mora.app identity"
	printf '%s\n' "$SIGN_INFO" | grep -Fqx "Identifier=$MACOS_IDENTIFIER" || \
		die "refusing to replace an existing app with the wrong signing identifier: $APP"
	printf '%s\n' "$SIGN_INFO" | grep -Fqx "TeamIdentifier=$MACOS_TEAM_ID" || \
		die "refusing to replace an existing app from the wrong Apple team: $APP"
	REQUIREMENT="$(codesign -d -r- "$APP" 2>&1)" || die "could not inspect the existing Mora.app designated requirement"
	printf '%s\n' "$REQUIREMENT" | grep -Fq "identifier \"$MACOS_IDENTIFIER\"" || \
		die "refusing to replace an existing app with the wrong designated identifier: $APP"
	printf '%s\n' "$REQUIREMENT" | \
		grep -Eq "subject\\.OU[^=]*= (\"${MACOS_TEAM_ID}\"|${MACOS_TEAM_ID})( |$)" || \
		die "refusing to replace an existing app with the wrong designated Apple team: $APP"
}

verify_mora_app() {
	APP="$1"
	EXPECTED_VERSION="$2"
	[ -d "$APP" ] && [ ! -L "$APP" ] || die "Mora.app must be a real directory: $APP"
	[ -f "$APP/Contents/Info.plist" ] && [ ! -L "$APP/Contents/Info.plist" ] || die "Mora.app has no regular Info.plist"
	[ -x "$APP/Contents/MacOS/mora" ] && [ ! -L "$APP/Contents/MacOS/mora" ] || die "Mora.app has no regular executable"
	[ -f "$APP/Contents/Resources/Mora.icns" ] && [ ! -L "$APP/Contents/Resources/Mora.icns" ] || die "Mora.app has no regular Mora.icns"
	[ "$(plist_value "$APP" CFBundleIdentifier)" = "$MACOS_IDENTIFIER" ] || die "Mora.app has the wrong CFBundleIdentifier"
	[ "$(plist_value "$APP" CFBundleExecutable)" = "mora" ] || die "Mora.app has the wrong CFBundleExecutable"
	[ "$(plist_value "$APP" CFBundleName)" = "Mora" ] || die "Mora.app has the wrong CFBundleName"
	[ "$(plist_value "$APP" CFBundleDisplayName)" = "Mora" ] || die "Mora.app has the wrong CFBundleDisplayName"
	[ "$(plist_value "$APP" CFBundleIconFile)" = "Mora" ] || die "Mora.app has the wrong CFBundleIconFile"
	[ "$(plist_value "$APP" CFBundlePackageType)" = "APPL" ] || die "Mora.app has the wrong CFBundlePackageType"
	[ "$(plist_value "$APP" CFBundleShortVersionString)" = "$EXPECTED_VERSION" ] || die "Mora.app has the wrong short version"
	[ "$(plist_value "$APP" CFBundleVersion)" = "$EXPECTED_VERSION" ] || die "Mora.app has the wrong bundle version"
	[ "$(plist_value "$APP" LSUIElement)" = "true" ] || die "Mora.app must be an agent app"
	codesign --verify --deep --strict --verbose=2 "$APP" >/dev/null 2>&1 || die "Mora.app has an invalid whole-bundle signature"
	verify_signing_target "$APP"
	verify_signing_target "$APP/Contents/MacOS/mora"
	xcrun stapler validate "$APP" >/dev/null 2>&1 || die "Mora.app has no valid stapled notarization ticket"
	codesign --verify --deep --strict --verbose=2 -R='notarized' "$APP" >/dev/null 2>&1 || \
		die "Mora.app does not satisfy Apple's notarized code requirement"
	spctl --assess --type execute --verbose=4 "$APP" >/dev/null 2>&1 || die "Gatekeeper rejected Mora.app"
	[ "$(lipo -archs "$APP/Contents/MacOS/mora" 2>/dev/null)" = "$LIPO_ARCH" ] || die "Mora.app has the wrong executable architecture"
	VERSION_OUTPUT="$("$APP/Contents/MacOS/mora" version 2>&1)" || die "Mora.app executable did not launch"
	VERSION_LINE="$(printf '%s\n' "$VERSION_OUTPUT" | sed -n '1p')"
	[ "$VERSION_LINE" = "mora $EXPECTED_VERSION" ] || die "Mora.app executable reports '$VERSION_LINE', expected 'mora $EXPECTED_VERSION'"
}

preflight_app_zip() {
	ARCHIVE="$1"
	ENTRY_LIST="$DOWNLOAD_TMP/app-entries.txt"
	zipinfo -1 "$ARCHIVE" > "$ENTRY_LIST" || die "could not inventory $ASSET"
	[ -s "$ENTRY_LIST" ] || die "$ASSET is empty"
	ENTRY_COUNT="$(wc -l < "$ENTRY_LIST" | tr -d '[:space:]')"
	case "$ENTRY_COUNT" in ''|*[!0-9]*) die "could not count entries in $ASSET" ;; esac
	[ "$ENTRY_COUNT" -le "$MAX_APP_ENTRIES" ] || die "$ASSET contains too many entries"
	while IFS= read -r ENTRY || [ -n "$ENTRY" ]; do
		[ -n "$ENTRY" ] || die "$ASSET contains an empty path"
		case "$ENTRY" in
			/*|*\\*) die "$ASSET contains an unsafe path: $ENTRY" ;;
		esac
		TRIMMED="${ENTRY%/}"
		case "$ENTRY" in
			*//*|*/../*|*/..|*/./*|*/.|./*) die "$ASSET contains a non-canonical path: $ENTRY" ;;
		esac
		case "$TRIMMED" in
			Mora.app|Mora.app/*|__MACOSX|__MACOSX/*) : ;;
			*) die "$ASSET contains an unexpected root path: $ENTRY" ;;
		esac
	done < "$ENTRY_LIST"
	DUPLICATE="$(sed 's#/$##' "$ENTRY_LIST" | tr '[:upper:]' '[:lower:]' | LC_ALL=C sort | uniq -d | sed -n '1p')"
	[ -z "$DUPLICATE" ] || die "$ASSET contains a duplicate path: $DUPLICATE"
	if zipinfo -l "$ARCHIVE" | awk '$1 ~ /^[lbcps]/ { found=1 } END { exit(found ? 0 : 1) }'; then
		die "$ASSET contains a symbolic link or irregular file"
	fi
	grep -Fqx 'Mora.app/Contents/Info.plist' "$ENTRY_LIST" || die "$ASSET has no Info.plist"
	grep -Fqx 'Mora.app/Contents/MacOS/mora' "$ENTRY_LIST" || die "$ASSET has no Mora executable"
	grep -Fqx 'Mora.app/Contents/Resources/Mora.icns' "$ENTRY_LIST" || die "$ASSET has no Mora.icns"
	EXPANDED_SIZE="$(unzip -l "$ARCHIVE" | awk 'END { print $1 }')"
	case "$EXPANDED_SIZE" in ''|*[!0-9]*) die "could not determine expanded size of $ASSET" ;; esac
	[ "$EXPANDED_SIZE" -le "$MAX_APP_BYTES" ] || die "$ASSET expands beyond $MAX_APP_BYTES bytes"
}

INSTALL_APP=1
REPLACE_APP=0
if [ -e "$APP_DEST" ] || [ -L "$APP_DEST" ]; then
	[ -d "$APP_DEST" ] && [ ! -L "$APP_DEST" ] || die "existing app target is not a real directory: $APP_DEST"
	INSTALLED_VERSION="$(plist_value "$APP_DEST" CFBundleShortVersionString)" || die "could not read the installed Mora.app version"
	printf '%s\n' "$INSTALLED_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || \
		die "existing Mora.app has an invalid release version: $INSTALLED_VERSION"
	verify_existing_app_identity "$APP_DEST"
	if (trap - EXIT INT TERM; verify_mora_app "$APP_DEST" "$INSTALLED_VERSION") >/dev/null 2>&1; then
		if [ "$INSTALLED_VERSION" = "$VERSION" ]; then
			INSTALL_APP=0
			say "✓ existing Mora.app $INSTALLED_VERSION is signed, notarized, and stapled"
		elif version_is_newer "$INSTALLED_VERSION" "$VERSION"; then
			die "refusing to downgrade signed Mora.app from $INSTALLED_VERSION to $VERSION"
		else
			REPLACE_APP=1
			say "Replacing stale Mora.app $INSTALLED_VERSION with signed release $VERSION ..."
		fi
	else
		REPLACE_APP=1
		say "Replacing stale or damaged Mora.app $INSTALLED_VERSION with signed release $VERSION ..."
	fi
fi

if [ "$INSTALL_APP" = 1 ]; then
	DOWNLOAD_TMP="$(mktemp -d)" || die "could not create download directory"
	USE_GH=0
	say "Fetching $ASSET from $REPO@v$VERSION ..."
	if command -v gh >/dev/null 2>&1 && \
		gh release download "v$VERSION" --repo "$REPO" --pattern "$ASSET" --dir "$DOWNLOAD_TMP" >/dev/null 2>&1; then
		USE_GH=1
	else
		command -v curl >/dev/null 2>&1 || die "curl is required when gh is unavailable"
		curl --proto '=https' --proto-redir '=https' -fsSL --max-filesize "$MAX_APP_BYTES" -o "$DOWNLOAD_TMP/$ASSET" \
			"https://github.com/$REPO/releases/download/v$VERSION/$ASSET" || die "app download failed"
	fi
	if [ "$USE_GH" = 1 ]; then
		gh release download "v$VERSION" --repo "$REPO" --pattern "$CHECKSUM_ASSET" --dir "$DOWNLOAD_TMP" >/dev/null 2>&1 || true
	fi
	[ -f "$DOWNLOAD_TMP/$CHECKSUM_ASSET" ] || \
		curl --proto '=https' --proto-redir '=https' -fsSL --max-filesize "$MAX_CHECKSUM_BYTES" -o "$DOWNLOAD_TMP/$CHECKSUM_ASSET" \
			"https://github.com/$REPO/releases/download/v$VERSION/$CHECKSUM_ASSET" || \
		die "could not fetch $CHECKSUM_ASSET — refusing an unverifiable app"
	[ -f "$DOWNLOAD_TMP/$ASSET" ] || die "downloaded app asset is missing"
	[ "$(file_size "$DOWNLOAD_TMP/$ASSET")" -le "$MAX_APP_BYTES" ] || die "$ASSET exceeds the size limit"
	[ "$(file_size "$DOWNLOAD_TMP/$CHECKSUM_ASSET")" -le "$MAX_CHECKSUM_BYTES" ] || die "$CHECKSUM_ASSET exceeds the size limit"
	WANT="$(tr -d '\r' < "$DOWNLOAD_TMP/$CHECKSUM_ASSET" | awk -v f="$ASSET" '$2 == f {print $1}')"
	MATCH_COUNT="$(tr -d '\r' < "$DOWNLOAD_TMP/$CHECKSUM_ASSET" | awk -v f="$ASSET" '$2 == f {n++} END {print n+0}')"
	[ "$MATCH_COUNT" = 1 ] || die "$CHECKSUM_ASSET must contain exactly one entry for $ASSET"
	case "$WANT" in *[!0-9A-Fa-f]*|'') die "$CHECKSUM_ASSET contains an invalid SHA-256 for $ASSET" ;; esac
	[ "${#WANT}" -eq 64 ] || die "$CHECKSUM_ASSET contains an invalid SHA-256 for $ASSET"
	GOT="$(sha256_of "$DOWNLOAD_TMP/$ASSET")" || die "no SHA-256 tool found"
	[ "$(printf '%s' "$GOT" | tr '[:upper:]' '[:lower:]')" = "$(printf '%s' "$WANT" | tr '[:upper:]' '[:lower:]')" ] || \
		die "CHECKSUM MISMATCH for $ASSET"
	say "✓ verified $ASSET against $CHECKSUM_ASSET"
	preflight_app_zip "$DOWNLOAD_TMP/$ASSET"
	APP_STAGE_DIR="$(mktemp -d "$APP_PARENT/.mora-app.install.XXXXXX")" || die "could not create same-volume app staging directory"
	ditto -x -k "$DOWNLOAD_TMP/$ASSET" "$APP_STAGE_DIR" || die "could not extract $ASSET"
	STAGED_APP="$APP_STAGE_DIR/Mora.app"
	verify_mora_app "$STAGED_APP" "$VERSION"
	if [ "$REPLACE_APP" = 1 ]; then
		APP_REPLACE_DIR="$(mktemp -d "$APP_PARENT/.mora-app.replace.XXXXXX")" || die "could not create same-volume app replacement directory"
		PREVIOUS_APP="$APP_REPLACE_DIR/Mora.app"
		mv "$APP_DEST" "$PREVIOUS_APP" || die "could not stage the previous Mora.app for replacement"
		if ! mv "$STAGED_APP" "$APP_DEST"; then
			mv "$PREVIOUS_APP" "$APP_DEST" || die "new app install failed and the previous Mora.app could not be restored; recover it from $PREVIOUS_APP"
			rmdir "$APP_REPLACE_DIR" 2>/dev/null || true
			APP_REPLACE_DIR=""
			die "could not install the replacement Mora.app; the previous app was restored"
		fi
	else
		mv "$STAGED_APP" "$APP_DEST" || die "could not atomically install Mora.app"
	fi
	# verify_mora_app uses die() for precise diagnostics. Run it in a subshell so
	# its exit cannot bypass the parent shell's rollback branch.
	if ! (trap - EXIT INT TERM; verify_mora_app "$APP_DEST" "$VERSION"); then
		if [ "$REPLACE_APP" = 1 ]; then
			if ! mv "$APP_DEST" "$STAGED_APP" || ! mv "$PREVIOUS_APP" "$APP_DEST"; then
				die "replacement verification failed and rollback failed; recover the previous app from $PREVIOUS_APP"
			fi
			rmdir "$APP_REPLACE_DIR" 2>/dev/null || true
			APP_REPLACE_DIR=""
			die "replacement Mora.app failed post-install verification; the previous app was restored"
		elif ! mv "$APP_DEST" "$STAGED_APP"; then
			die "installed Mora.app failed post-install verification and rollback failed; inspect $APP_DEST"
		fi
		die "installed Mora.app failed post-install verification; the incomplete app was removed"
	fi
	if [ "$REPLACE_APP" = 1 ]; then
		rm -rf "$APP_REPLACE_DIR"
		APP_REPLACE_DIR=""
		say "✓ replaced stale or damaged Mora.app with signed, notarized, stapled release $VERSION"
	else
		say "✓ installed signed, notarized, stapled Mora.app $VERSION"
	fi
fi

if [ -n "${PREFIX:-}" ]; then
	LINK_DIR="$PREFIX"
else
	LINK_DIR=""
	ACTIVE_MORA="$(command -v mora 2>/dev/null || true)"
	if [ -n "$ACTIVE_MORA" ]; then
		case "$ACTIVE_MORA" in
			/*) ACTIVE_PATH="$ACTIVE_MORA" ;;
			*/*)
				ACTIVE_DIR="$(CDPATH='' cd -- "$(dirname -- "$ACTIVE_MORA")" 2>/dev/null && pwd -P)" || die "could not resolve active Mora path"
				ACTIVE_PATH="$ACTIVE_DIR/$(basename -- "$ACTIVE_MORA")"
				;;
			*) ACTIVE_PATH="" ;;
		esac
		if [ -n "${ACTIVE_PATH:-}" ]; then
			case "$ACTIVE_PATH" in */Cellar/*|*/Caskroom/*) die "active Mora is managed by Homebrew; migrate it explicitly after uninstalling the cask" ;; esac
			if [ -L "$ACTIVE_PATH" ]; then
				CURRENT_LINK="$(readlink "$ACTIVE_PATH")"
				case "$CURRENT_LINK" in
					"$APP_DEST/Contents/MacOS/mora") LINK_DIR="$(dirname -- "$ACTIVE_PATH")" ;;
					*) die "active Mora is an unrelated symlink at $ACTIVE_PATH" ;;
				esac
			elif [ -f "$ACTIVE_PATH" ]; then
				LINK_DIR="$(dirname -- "$ACTIVE_PATH")"
			fi
		fi
	fi
	if [ -z "$LINK_DIR" ]; then
		for DIR in /usr/local/bin /opt/homebrew/bin "$HOME/.local/bin"; do
			if [ -d "$DIR" ] && [ -w "$DIR" ] && [ ! -L "$DIR/mora" ]; then LINK_DIR="$DIR"; break; fi
		done
	fi
	[ -n "$LINK_DIR" ] || LINK_DIR="$HOME/.local/bin"
fi

case "$LINK_DIR" in
	/*) : ;;
	*) die "PATH directory must be absolute: $LINK_DIR" ;;
esac
mkdir -p "$LINK_DIR"
[ -d "$LINK_DIR" ] && [ ! -L "$LINK_DIR" ] && [ -w "$LINK_DIR" ] || die "PATH directory is not a writable real directory: $LINK_DIR"
LINK_DEST="$LINK_DIR/mora"
APP_EXECUTABLE="$APP_DEST/Contents/MacOS/mora"
if [ -L "$LINK_DEST" ]; then
	[ "$(readlink "$LINK_DEST")" = "$APP_EXECUTABLE" ] || die "refusing to replace unrelated symlink: $LINK_DEST"
elif [ -e "$LINK_DEST" ]; then
	[ -f "$LINK_DEST" ] || die "refusing to replace non-file PATH entry: $LINK_DEST"
	BACKUP="$LINK_DEST.standalone-backup"
	if [ -e "$BACKUP" ] || [ -L "$BACKUP" ]; then
		BACKUP_NUMBER=1
		while [ -e "$BACKUP.$BACKUP_NUMBER" ] || [ -L "$BACKUP.$BACKUP_NUMBER" ]; do
			BACKUP_NUMBER=$((BACKUP_NUMBER + 1))
			[ "$BACKUP_NUMBER" -le 1000 ] || die "could not allocate a standalone backup path beside $LINK_DEST"
		done
		BACKUP="$BACKUP.$BACKUP_NUMBER"
	fi
	cp -p "$LINK_DEST" "$BACKUP" || die "could not preserve standalone Mora at $BACKUP"
	LINK_STAGE_DIR="$(mktemp -d "$LINK_DIR/.mora-link.XXXXXX")" || die "could not stage PATH symlink"
	ln -s "$APP_EXECUTABLE" "$LINK_STAGE_DIR/mora" || die "could not create PATH symlink"
	mv -f "$LINK_STAGE_DIR/mora" "$LINK_DEST" || die "could not atomically install PATH symlink"
	say "✓ preserved the standalone binary at $BACKUP"
else
	LINK_STAGE_DIR="$(mktemp -d "$LINK_DIR/.mora-link.XXXXXX")" || die "could not stage PATH symlink"
	ln -s "$APP_EXECUTABLE" "$LINK_STAGE_DIR/mora" || die "could not create PATH symlink"
	mv "$LINK_STAGE_DIR/mora" "$LINK_DEST" || die "could not atomically install PATH symlink"
fi

INIT_OUT="$("$APP_EXECUTABLE" init --vault "$VAULT" </dev/null 2>&1)" || printf 'note: mora init did not run: %s\n' "$INIT_OUT" >&2
ACTIVE_VAULT="$("$APP_EXECUTABLE" config 2>/dev/null | sed -n 's/^vault_dir = //p' | sed -n '1p')"
[ -n "$ACTIVE_VAULT" ] || ACTIVE_VAULT="$VAULT"
INSTALLED_VERSION="$(plist_value "$APP_DEST" CFBundleShortVersionString)"

cat <<EOF

✓ Installed Mora.app $INSTALLED_VERSION
  app:     $APP_DEST
  command: $LINK_DEST -> $APP_EXECUTABLE
  vault:   $ACTIVE_VAULT

Planned Mora.app Full Disk Access migration:
  1. Open System Settings > Privacy & Security > Full Disk Access.
  2. Add $APP_DEST and enable it.
  3. Keep the old Mora entry until these pass:
       mora doctor
       mora sync imessage

After that, mora upgrade is designed to replace the whole signed bundle at the same path.
FDA continuity is not proven until a real signed N to N+1 upgrade passes without a re-grant.
The old standalone binary remains at ${BACKUP:-<no prior standalone install>} for rollback.
EOF

# Emit the user's future $PATH literally.
# shellcheck disable=SC2016
case ":$PATH:" in
	*":$LINK_DIR:"*) : ;;
	*) printf '\nAdd to PATH, then restart your shell:\n  echo '\''export PATH="%s:$PATH"'\'' >> ~/.zshrc\n' "$LINK_DIR" ;;
esac
