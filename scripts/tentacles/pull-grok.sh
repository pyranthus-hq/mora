#!/bin/bash
set -euo pipefail
# SSH arguments are owner-supplied; the last argument is the existing pinned host alias.
if [ "$#" -eq 0 ]; then
  echo 'usage: pull-grok.sh [ssh options] host-alias' >&2
  exit 2
fi
"$HOME/.local/bin/mora-bot-return" seen | \
ssh -T -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=10 \
  -o ServerAliveInterval=15 -o ServerAliveCountMax=2 "$@" \
  'MORA_CONFIG_DIR="$HOME/.config/mora-gateway" "$HOME/.local/bin/mora-bot-return" export' \
  | "$HOME/.local/bin/mora-bot-return" import
