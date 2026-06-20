#!/usr/bin/env sh
# Mora uninstaller — the mirror of install.sh.
#
# Removes the `mora` binary, de-registers the MCP server from your agents, and
# (only if you ask) deletes your vault. By default your vault is PRESERVED —
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

# --- find and remove the mora binary -------------------------------------------
# Prefer whatever is on PATH; otherwise search the same dirs install.sh writes to.
REMOVED=""
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

# de-dup and remove each existing binary
printf '%s\n' "$CANDIDATES" | awk 'NF && !seen[$0]++' | while IFS= read -r bin; do
	if [ -e "$bin" ]; then
		rm -f "$bin" && say "Removed binary: $bin"
	fi
done
if command -v mora >/dev/null 2>&1; then
	say "Note: a 'mora' is still on PATH at $(command -v mora) — remove it manually or set PREFIX and re-run."
else
	REMOVED="yes"
fi

# --- de-register the MCP server from agents (best-effort) ----------------------
if command -v claude >/dev/null 2>&1; then
	claude mcp remove mora -s user >/dev/null 2>&1 && say "Removed Mora from Claude Code MCP config." || true
fi
if command -v codex >/dev/null 2>&1; then
	codex mcp remove mora >/dev/null 2>&1 && say "Removed Mora from Codex MCP config." || true
fi

# --- vault: preserve by default, delete only on explicit purge -----------------
if [ "$PURGE" = "1" ]; then
	if [ -d "$VAULT" ]; then
		if [ "$ASSUME_YES" != "1" ]; then
			printf 'Delete your vault at %s? This is your data and cannot be undone. [y/N] ' "$VAULT"
			read -r ans || ans=""
			case "$ans" in y|Y|yes|YES) : ;; *) say "Kept vault: $VAULT"; PURGE=0 ;; esac
		fi
		if [ "$PURGE" = "1" ]; then
			rm -rf "$VAULT" && say "Deleted vault: $VAULT"
		fi
	else
		say "No vault found at $VAULT (nothing to delete)."
	fi
else
	if [ -d "$VAULT" ]; then
		say "Kept your vault: $VAULT  (re-run with --purge to delete it)"
	fi
fi

# --- final notes ---------------------------------------------------------------
say ""
say "Mora uninstalled."
say "If you added Mora to your PATH, remove the matching line from your shell rc, e.g.:"
say "  ~/.zshrc or ~/.bashrc:  export PATH=\"...mora install dir...:\$PATH\""
