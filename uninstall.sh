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

VAULT="${MORA_VAULT:-$HOME/vault/mora}"
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

if [ -n "$FIRST_BIN" ]; then
	try "Claude Code hooks" "$FIRST_BIN" hook uninstall
	try "background HTTP service" "$FIRST_BIN" serve http uninstall
	# Same job names as `mora schedule install`; only touch jobs that exist.
	for job in pulse-daily doctor-pulse index-hourly backup-daily lint-weekly ingest-hourly git-daily update-daily; do
		if [ -e "$HOME/Library/LaunchAgents/com.mora.$job.plist" ]; then
			try "scheduled job $job" "$FIRST_BIN" schedule uninstall "$job"
		fi
	done
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

remove_path() { # remove_path <path> <label>
	case "$1" in ""|"/"|"$HOME") say "Refusing to remove unsafe path: '$1'"; return 0 ;; esac
	if [ -e "$1" ]; then
		rm -rf "$1" && say "Removed $2: $1"
	fi
}
remove_path "$CONFIG_DIR/tokens" "login tokens"
remove_path "$CONFIG_DIR/share/identity.txt" "sharing private key"
remove_path "$CONFIG_DIR/share/signing.key" "sharing signing key"
remove_path "$CONFIG_DIR/http.json" "HTTP bearer token"

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
	remove_path "$VAULT" "vault"
	remove_path "$CONFIG_DIR" "config"
	remove_path "$DATA_DIR" "data (search index)"
	remove_path "$STATE_DIR" "state"
else
	[ -d "$VAULT" ] && say "Kept your vault: $VAULT  (re-run with --purge to delete it)"
	say "Kept config, data and state: $CONFIG_DIR, $DATA_DIR, $STATE_DIR  (--purge deletes them)"
fi

# --- final notes ---------------------------------------------------------------
say ""
say "Mora uninstalled."
say "If you added Mora to your PATH, remove the matching line from your shell rc, e.g.:"
say "  ~/.zshrc or ~/.bashrc:  export PATH=\"...mora install dir...:\$PATH\""
