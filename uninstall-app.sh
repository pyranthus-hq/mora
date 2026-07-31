#!/usr/bin/env sh
# Remove only Mora's verified macOS app bundle and PATH symlinks that point to
# it. Vault, config, state, and the standalone migration backup are preserved.
set -eu

APP_PARENT="${MORA_APP_DIR:-$HOME/Applications}"
APP_DEST="$APP_PARENT/Mora.app"
APP_EXECUTABLE="$APP_DEST/Contents/MacOS/mora"
MACOS_IDENTIFIER="com.pyranthus.mora"
MACOS_TEAM_ID="VS8M5VJBZ5"
REMOVAL_STAGE=""
DETACHED_APP=""
REMOVED_LINKS_DIR=""
REMOVED_LINK_COUNT=0

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = "Darwin" ] || die "Mora.app can be uninstalled only on macOS"
command -v plutil >/dev/null 2>&1 || die "plutil is required to identify Mora.app"
command -v codesign >/dev/null 2>&1 || die "codesign is required to identify Mora.app"

case "$APP_PARENT" in
	/*) : ;;
	*) die "MORA_APP_DIR must be an absolute path" ;;
esac
if [ -n "${PREFIX:-}" ]; then
	case "$PREFIX" in
		/*) : ;;
		*) die "PREFIX must be an absolute path" ;;
	esac
fi

[ -d "$APP_DEST" ] && [ ! -L "$APP_DEST" ] || die "no real Mora.app directory exists at $APP_DEST"
[ -f "$APP_DEST/Contents/Info.plist" ] && [ ! -L "$APP_DEST/Contents/Info.plist" ] || \
	die "refusing to remove a bundle without a regular Info.plist"
[ -x "$APP_EXECUTABLE" ] && [ ! -L "$APP_EXECUTABLE" ] || \
	die "refusing to remove a bundle without Mora's regular executable"
[ "$(plutil -extract CFBundleIdentifier raw -o - "$APP_DEST/Contents/Info.plist" 2>/dev/null)" = "$MACOS_IDENTIFIER" ] || \
	die "refusing to remove an app with the wrong bundle identifier"
[ "$(plutil -extract CFBundleExecutable raw -o - "$APP_DEST/Contents/Info.plist" 2>/dev/null)" = "mora" ] || \
	die "refusing to remove an app with the wrong bundle executable"
codesign --verify --deep --strict --verbose=2 "$APP_DEST" >/dev/null 2>&1 || \
	die "refusing to remove Mora.app because its whole-bundle signature is invalid"
SIGN_INFO="$(codesign -dvvv "$APP_DEST" 2>&1)" || die "could not inspect Mora.app's signature"
printf '%s\n' "$SIGN_INFO" | grep -Fqx "Identifier=$MACOS_IDENTIFIER" || \
	die "refusing to remove an app with the wrong signing identifier"
printf '%s\n' "$SIGN_INFO" | grep -Fqx "TeamIdentifier=$MACOS_TEAM_ID" || \
	die "refusing to remove an app with the wrong Apple team"
printf '%s\n' "$SIGN_INFO" | grep -Fq 'Authority=Developer ID Application:' || \
	die "refusing to remove an app without a Developer ID Application signature"

ACTIVE_MORA="$(command -v mora 2>/dev/null || true)"

visit_link_candidates() {
	action="$1"
	if [ -n "${PREFIX:-}" ]; then
		"$action" "$PREFIX/mora" || return 1
	else
		case "$ACTIVE_MORA" in
			/*) "$action" "$ACTIVE_MORA" || return 1 ;;
		esac

		# The installer can use any writable absolute directory already on PATH.
		# Inspect all of them so a different mora earlier on PATH cannot hide a
		# managed app link. Ignore relative and empty PATH entries.
		old_ifs="$IFS"
		case "$-" in
			*f*) restore_glob=0 ;;
			*) set -f; restore_glob=1 ;;
		esac
		IFS=:
		for candidate_dir in ${PATH:-}; do
			case "$candidate_dir" in
				/*)
					if ! "$action" "$candidate_dir/mora"; then
						IFS="$old_ifs"
						[ "$restore_glob" -eq 0 ] || set +f
						return 1
					fi
					;;
			esac
		done
		IFS="$old_ifs"
		[ "$restore_glob" -eq 0 ] || set +f

		"$action" "/usr/local/bin/mora" || return 1
		"$action" "/opt/homebrew/bin/mora" || return 1
		"$action" "$HOME/.local/bin/mora" || return 1
	fi
}

preflight_link() {
	link="$1"
	if [ -L "$link" ] && [ "$(readlink "$link")" = "$APP_EXECUTABLE" ]; then
		[ -w "$(dirname "$link")" ] || die "managed PATH directory is not writable: $(dirname "$link")"
	fi
}

remove_managed_link() {
	link="$1"
	if [ -L "$link" ] && [ "$(readlink "$link")" = "$APP_EXECUTABLE" ]; then
		REMOVED_LINK_COUNT=$((REMOVED_LINK_COUNT + 1))
		# Record the original pathname inside the private staging directory
		# before mutation. A symlink target preserves every byte except NUL,
		# including whitespace and newlines, without an unsafe text parser.
		ln -s "$link" "$REMOVED_LINKS_DIR/$REMOVED_LINK_COUNT" || return 1
		unlink "$link" || return 1
		say "✓ removed managed PATH symlink $link"
		if [ -f "$link.standalone-backup" ] && [ ! -L "$link.standalone-backup" ]; then
			say "  preserved standalone rollback binary: $link.standalone-backup"
		fi
	fi
}

cleanup_removal_stage() {
	for record in "$REMOVED_LINKS_DIR"/*; do
		[ -L "$record" ] || continue
		unlink "$record" || return 1
	done
	rmdir "$REMOVED_LINKS_DIR" || return 1
	rmdir "$REMOVAL_STAGE" || return 1
}

rollback_detach() {
	rollback_failed=0
	if [ -e "$APP_DEST" ] || [ -L "$APP_DEST" ]; then
		rollback_failed=1
	elif ! mv "$DETACHED_APP" "$APP_DEST"; then
		rollback_failed=1
	fi

	if [ "$rollback_failed" -eq 0 ]; then
		for record in "$REMOVED_LINKS_DIR"/*; do
			[ -L "$record" ] || continue
			restore_link="$(readlink "$record")" || {
				rollback_failed=1
				continue
			}
			if [ -L "$restore_link" ] && [ "$(readlink "$restore_link")" = "$APP_EXECUTABLE" ]; then
				continue
			fi
			if [ -e "$restore_link" ] || [ -L "$restore_link" ] || \
				! ln -s "$APP_EXECUTABLE" "$restore_link"; then
				rollback_failed=1
			fi
		done
	fi

	if [ "$rollback_failed" -ne 0 ]; then
		die "PATH cleanup failed and rollback was incomplete; inspect $APP_DEST and $REMOVAL_STAGE"
	fi
	if ! cleanup_removal_stage; then
		die "PATH cleanup failed; Mora.app and managed links were restored, but private staging remains at $REMOVAL_STAGE"
	fi
	die "PATH cleanup failed; Mora.app and previously removed managed links were restored"
}

# Prove every managed link is removable before the app disappears. Unrelated
# files and symlinks are never changed.
visit_link_candidates preflight_link

REMOVAL_STAGE="$(mktemp -d "$APP_PARENT/.mora-app-uninstall.XXXXXX")" || \
	die "could not create same-volume removal staging path"
DETACHED_APP="$REMOVAL_STAGE/Mora.app"
REMOVED_LINKS_DIR="$REMOVAL_STAGE/removed-links"
mkdir "$REMOVED_LINKS_DIR" || die "could not prepare private rollback metadata"
mv "$APP_DEST" "$DETACHED_APP" || die "could not atomically detach Mora.app"
if ! visit_link_candidates remove_managed_link; then
	rollback_detach
fi

# find does not follow symlinks by default. The target is the exact path that
# the validated app was atomically moved into, inside the still-private mktemp
# directory. Rollback metadata is removed separately.
find "$DETACHED_APP" -depth -delete || \
	die "Mora.app was detached but could not be deleted; recover it from $DETACHED_APP"
[ ! -e "$DETACHED_APP" ] && [ ! -L "$DETACHED_APP" ] || \
	die "Mora.app removal staging path still exists: $DETACHED_APP"
cleanup_removal_stage || \
	die "Mora.app was removed, but private staging remains at $REMOVAL_STAGE"

cat <<EOF

✓ Uninstalled $APP_DEST
  Mora's vault, config, state, and any .standalone-backup file were preserved.
  Remove the old Mora entry from System Settings > Privacy & Security > Full Disk Access when you no longer need it.
EOF
