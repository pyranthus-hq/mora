#!/usr/bin/env sh
# Mora uninstaller — the mirror of install.sh.
#
# Removes the `mora` binary, its Claude Code hooks, background HTTP service and
# scheduled jobs, your stored credentials, and the MCP registration. With
# --purge it also deletes your vault and Mora's config, data and state. By default your vault is PRESERVED —
# it's your data, and uninstalling the tool should never silently delete it.
#
#   sh uninstall.sh              # remove binary + MCP registration; KEEP the vault
#   MORA_PURGE=1 sh uninstall.sh # also delete the vault (asks to confirm first)
#   sh uninstall.sh --purge      # same as MORA_PURGE=1
#   sh uninstall.sh --yes        # don't prompt (assume yes for the purge confirm)
#
# Env knobs (match install.sh):
#   PREFIX=/usr/local/bin    where the binary was installed (default: auto-detect)
#   MORA_VAULT=~/vault/mora  vault location (default: ~/vault/mora)
set -eu

VAULT="${MORA_VAULT:-${MORA_CONFIG_DIR:+$MORA_CONFIG_DIR/vault}}"
VAULT="${VAULT:-$HOME/vault/mora}"
PURGE="${MORA_PURGE:-0}"
ASSUME_YES=0

for arg in "$@"; do
	case "$arg" in
		--purge) PURGE=1 ;;
		--yes|-y) ASSUME_YES=1 ;;
		*) printf 'unknown option: %s\n' "$arg" >&2; exit 2 ;;
	esac
done

say() { printf '%s\n' "$*"; }

# --- find the mora binary -------------------------------------------------------
# Prefer whatever is on PATH; otherwise search the same dirs install.sh writes to.
if [ -n "${PREFIX:-}" ]; then
	CANDIDATES="$PREFIX/mora"
else
	CANDIDATES=""
	# the one currently resolved on PATH (if any), then install.sh's default dirs
	if command -v mora >/dev/null 2>&1; then
		CANDIDATES="$(command -v mora)"
	fi
	for d in /usr/local/bin /opt/homebrew/bin "$HOME/.local/bin"; do
		CANDIDATES="$CANDIDATES
$d/mora"
	done
fi
CANDIDATES="$(printf '%s\n' "$CANDIDATES" | awk 'NF && !seen[$0]++')"

# --- undo what mora installed outside its own dirs (needs the binary) -----------
# Run BEFORE the binary is deleted. Each step is best-effort: a failure is
# reported and the uninstall keeps going.
FIRST_BIN=""
while IFS= read -r bin; do
	if [ -n "$bin" ] && [ -x "$bin" ]; then FIRST_BIN="$bin"; break; fi
done <<EOF_CANDIDATES
$CANDIDATES
EOF_CANDIDATES

try() { # try <label> <cmd...>: run quietly, report the outcome, never abort
	label="$1"; shift
	if "$@" >/dev/null 2>&1; then say "Removed $label"; else say "Could not remove $label (continuing)"; fi
}

OS="$(uname -s 2>/dev/null || echo unknown)"
SYSTEMD_UNIT="mora-serve-http.service"

if [ -n "$FIRST_BIN" ]; then
	try "Claude Code hooks" "$FIRST_BIN" hook uninstall
	# On Linux `serve http uninstall` only deletes the unit file; it leaves the
	# server running, so stop and disable it first (the unit must still exist).
	if [ "$OS" = "Linux" ] && [ -e "$HOME/.config/systemd/user/$SYSTEMD_UNIT" ]; then
		if command -v systemctl >/dev/null 2>&1; then
			try "systemd unit $SYSTEMD_UNIT (stopped and disabled)" systemctl --user disable --now "$SYSTEMD_UNIT"
		else
			say "Note: systemctl not found; stop the server yourself: systemctl --user disable --now $SYSTEMD_UNIT"
		fi
	fi
	try "background HTTP service" "$FIRST_BIN" serve http uninstall
	if [ "$OS" = "Linux" ] && command -v systemctl >/dev/null 2>&1; then
		systemctl --user daemon-reload >/dev/null 2>&1 || true
	fi
	# Same job names as `mora schedule install`; only touch jobs that exist.
	# macOS only: on Linux mora installs no timer (it prints a cron/timer hint).
	for job in pulse-daily doctor-pulse index-hourly backup-daily lint-weekly ingest-hourly git-daily update-daily; do
		if [ -e "$HOME/Library/LaunchAgents/com.mora.$job.plist" ]; then
			try "scheduled job $job" "$FIRST_BIN" schedule uninstall "$job"
		fi
	done
	if [ "$OS" = "Linux" ]; then
		say "Note: if you added a cron line or systemd timer for a Mora job, remove it by hand."
	fi
else
	say "Note: no mora binary found, so hooks, the HTTP service and scheduled jobs were NOT removed."
	say "      Install mora again (or set PREFIX) and re-run to clean them up."
fi

# --- secrets: always removed, with or without --purge ---------------------------
# The vault is your data; these are credentials, so they never stay behind.
CONFIG_DIR="${MORA_CONFIG_DIR:-$HOME/.config/mora}"
DATA_DIR="${MORA_CONFIG_DIR:+$MORA_CONFIG_DIR/data}"
DATA_DIR="${DATA_DIR:-$HOME/.local/share/mora}"
STATE_DIR="${MORA_CONFIG_DIR:+$MORA_CONFIG_DIR/state}"
STATE_DIR="${STATE_DIR:-$HOME/.local/state/mora}"

# Recursive deletes only ever happen through safe_rmdir. It refuses unless the
# path is absolute, resolves to a real directory strictly inside $HOME (never
# $HOME, "/" or an ancestor of $HOME), and looks like Mora's own:
#   kind "dir"   the last component must be "mora"
#   kind "vault" the directory must hold the .mora-vault.json marker
# A symlink is removed as a link and never followed.
refuse() { say "Refusing to delete $1: $2"; say "  Remove it yourself if it is really Mora's."; }
safe_rmdir() { # safe_rmdir <path> <label> <dir|vault>
	p="$1"; label="$2"; kind="$3"
	[ -e "$p" ] || [ -L "$p" ] || return 0
	case "$p" in /*) : ;; *) refuse "$label ($p)" "path is not absolute"; return 0 ;; esac
	p="${p%/}"
	[ -n "$p" ] || { refuse "$label (/)" "that is the filesystem root"; return 0; }
	if [ "$kind" = "dir" ] && [ "${p##*/}" != "mora" ]; then
		refuse "$label ($p)" "its name is not 'mora'"; return 0
	fi
	if [ "$kind" = "vault" ] && [ ! -f "$p/.mora-vault.json" ]; then
		refuse "$label ($p)" "no .mora-vault.json marker, so it may not be a Mora vault"; return 0
	fi
	if [ -L "$p" ]; then
		# Judge where the link itself lives, not where it points.
		canon="$(cd -P "${p%/*}/" 2>/dev/null && pwd -P)/${p##*/}" || { refuse "$label ($p)" "cannot resolve its parent"; return 0; }
	else
		canon="$(cd -P "$p" 2>/dev/null && pwd -P)" || { refuse "$label ($p)" "cannot resolve it to a directory"; return 0; }
	fi
	home="$(cd -P "$HOME" 2>/dev/null && pwd -P)" || { refuse "$label ($p)" "cannot resolve \$HOME"; return 0; }
	case "$canon" in
		"$home"/?*) : ;;
		*) refuse "$label ($p)" "it is not strictly inside your home directory"; return 0 ;;
	esac
	if [ -L "$p" ]; then
		rm -f "$p" && say "Removed $label (symlink only; its target is untouched): $p"
	else
		rm -rf "$canon" && say "Removed $label: $p"
	fi
}
remove_path() { # remove_path <path> <label>: one regular credential file, never a directory or link
	if [ -f "$1" ] && [ ! -L "$1" ]; then
		rm -f "$1" && say "Removed $2: $1"
	fi
}
# Credentials are deleted file by file, never recursively, and only from a config
# dir that is absolute and holds Mora's config.toml, so an odd MORA_CONFIG_DIR
# such as /tmp cannot point these deletes at someone else's files.
case "$CONFIG_DIR" in
	/*) if [ -f "$CONFIG_DIR/config.toml" ]; then SECRETS_OK=1; else SECRETS_OK=0; say "No config.toml in '$CONFIG_DIR': credentials there were NOT removed."; fi ;;
	*) SECRETS_OK=0; say "Config dir '$CONFIG_DIR' is not an absolute path: credentials there were NOT removed." ;;
esac
if [ "$SECRETS_OK" = "1" ]; then
if [ -d "$CONFIG_DIR/tokens" ] && [ ! -L "$CONFIG_DIR/tokens" ]; then
	for f in "$CONFIG_DIR"/tokens/*; do remove_path "$f" "login token"; done
	rmdir "$CONFIG_DIR/tokens" 2>/dev/null || true
fi
remove_path "$CONFIG_DIR/share/identity.txt" "sharing private key"
remove_path "$CONFIG_DIR/share/signing.key" "sharing signing key"
remove_path "$CONFIG_DIR/http.json" "HTTP bearer token"
fi

# --- remove the binary ----------------------------------------------------------
printf '%s\n' "$CANDIDATES" | while IFS= read -r bin; do
	if [ -e "$bin" ]; then
		rm -f "$bin" && say "Removed binary: $bin"
	fi
done
if command -v mora >/dev/null 2>&1; then
	say "Note: a 'mora' is still on PATH at $(command -v mora) — remove it manually or set PREFIX and re-run."
fi

# --- de-register the MCP server from agents (best-effort) ----------------------
if command -v claude >/dev/null 2>&1; then
	claude mcp remove mora -s user >/dev/null 2>&1 && say "Removed Mora from Claude Code MCP config." || true
fi
if command -v codex >/dev/null 2>&1; then
	codex mcp remove mora >/dev/null 2>&1 && say "Removed Mora from Codex MCP config." || true
fi

# --- vault, config, data, state: preserve by default, delete only on --purge ----
if [ "$PURGE" = "1" ] && [ -d "$VAULT" ] && [ "$ASSUME_YES" != "1" ]; then
	printf 'Delete your vault at %s? This is your data and cannot be undone. [y/N] ' "$VAULT"
	read -r ans || ans=""
	case "$ans" in y|Y|yes|YES) : ;; *) PURGE=0 ;; esac
fi
if [ "$PURGE" = "1" ]; then
	# The vault may sit inside CONFIG_DIR (MORA_CONFIG_DIR), so delete it first.
	safe_rmdir "$VAULT" "vault" vault
	if [ -n "${MORA_CONFIG_DIR:-}" ]; then
		# Data and state live inside the isolated root; one delete covers them.
		safe_rmdir "$CONFIG_DIR" "config, data and state" dir
	else
		safe_rmdir "$CONFIG_DIR" "config" dir
		safe_rmdir "$DATA_DIR" "data (search index)" dir
		safe_rmdir "$STATE_DIR" "state" dir
	fi
else
	[ -d "$VAULT" ] && say "Kept your vault: $VAULT  (re-run with --purge to delete it)"
	say "Kept config, data and state: $CONFIG_DIR, $DATA_DIR, $STATE_DIR  (--purge deletes them)"
fi

# --- final notes ---------------------------------------------------------------
say ""
say "Mora uninstalled."
say "If you added Mora to your PATH, remove the matching line from your shell rc, e.g.:"
say "  ~/.zshrc or ~/.bashrc:  export PATH=\"...mora install dir...:\$PATH\""
