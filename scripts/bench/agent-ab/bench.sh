#!/usr/bin/env bash
# Mora end-to-end agent A/B/C benchmark (codegraph-inspired), on a HERMETIC
# synthetic vault. Holds model + question fixed; varies ONLY the retrieval arm:
#   baseline      : no Mora; READ-ONLY Read/Grep/Glob over the synthetic vault notes.
#   mora-mcp      : Mora via MCP server (no filesystem tools).
#   mora-mcp-mmr  : identical to mora-mcp but with the diversity-aware MMR rerank ON
#                   (`mora config mmr on`) — the controlled with-vs-without-MMR arm.
#   mora-cli      : Mora via the `mora` shell CLI (Bash; no filesystem grep of the vault).
# All arms point at the SAME isolated vault via MORA_CONFIG_DIR ($SYNTH) so the real
# ~/vault/mora and ~/.config/mora are never touched. The MMR arm flips ONLY the rerank
# flag in that one sandbox (same index, same embedder), so any delta is purely MMR.
# For the MMR study run:  ARMS="mora-mcp mora-mcp-mmr"  (the sandbox MUST be on Ollama —
# MMR only reranks under a semantic embedder).
#
# Cost/usage come from Claude Code's own `result` event (authoritative).
# Env: SYNTH (isolated MORA_CONFIG_DIR, default /tmp/bench-mora), MODEL (sonnet),
#      REPS (1), QFILTER (id/substr), JUDGE (1 to run the LLM judge).
# COST: each cell + each judge call is a real billed `claude -p`.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/out}"; [[ "${KEEP_OUT:-}" == "1" ]] || rm -rf "$OUT"; mkdir -p "$OUT"
ARMS="${ARMS:-baseline mora-mcp mora-cli}"   # restrict to re-run one arm (e.g. ARMS="mora-mcp mora-cli")
SYNTH="${SYNTH:-/tmp/bench-mora}"
VAULT="$SYNTH/vault"
MODEL="${MODEL:-sonnet}"
REPS="${REPS:-1}"
QFILTER="${QFILTER:-}"
QFILE="$SYNTH/questions.json"
[[ -f "$QFILE" ]] || { echo "FATAL: no $QFILE — run build_vault.py first" >&2; exit 1; }

VAULT_HINT="The user's personal memory is stored as markdown files under ${VAULT}. Search that directory with Read/Grep/Glob and answer from what you find. Do not write anything."
MORA_HINT="You have a 'mora' MCP server exposing the user's memory across email, calendars, iMessage, and notes. Use its tools (e.g. search_memory, think, graph) to answer. Cite the evidence."
CLI_HINT="You have a 'mora' command-line tool exposing the user's memory across email, calendars, iMessage, and notes. Use it via Bash, e.g.  mora think \"<q>\" --json , mora search \"<q>\" --json , mora graph \"<person>\". Answer from its output and cite the evidence. Do not try to read files directly."

cell () { # file arm question
  local f="$1" arm="$2" q="$3" d; d="$(mktemp -d)"
  # Set the MMR rerank flag EXPLICITLY per cell so each cell is hermetic regardless of
  # arm order (the on-arm and off-arms share one $SYNTH config.toml). Only the Mora
  # arms touch it; baseline never reads Mora config. Needs a `mora` with the W2 setter.
  case "$arm" in
    mora-mcp-mmr) MORA_CONFIG_DIR="$SYNTH" mora config mmr on  >/dev/null 2>&1 ;;
    mora-mcp|mora-cli) MORA_CONFIG_DIR="$SYNTH" mora config mmr off >/dev/null 2>&1 ;;
  esac
  case "$arm" in
    baseline)
      ( cd "$d" && claude -p "$q" --model "$MODEL" --output-format stream-json --verbose \
          --strict-mcp-config --mcp-config "$HERE/baseline-mcp.json" \
          --add-dir "$VAULT" --disallowedTools Bash Edit Write WebFetch WebSearch \
          --append-system-prompt "$VAULT_HINT" --dangerously-skip-permissions
      ) </dev/null >"$f" 2>"${f%.jsonl}.err" ;;
    mora-mcp|mora-mcp-mmr)
      printf '{"mcpServers":{"mora":{"command":"mora","args":["mcp","serve"],"env":{"MORA_CONFIG_DIR":"%s"}}}}' "$SYNTH" >"$d/mcp.json"
      ( cd "$d" && claude -p "$q" --model "$MODEL" --output-format stream-json --verbose \
          --strict-mcp-config --mcp-config "$d/mcp.json" \
          --disallowedTools Read Grep Glob Bash Edit Write WebFetch WebSearch \
          --append-system-prompt "$MORA_HINT" --dangerously-skip-permissions
      ) </dev/null >"$f" 2>"${f%.jsonl}.err" ;;
    mora-cli)
      ( cd "$d" && MORA_CONFIG_DIR="$SYNTH" claude -p "$q" --model "$MODEL" --output-format stream-json --verbose \
          --strict-mcp-config --mcp-config "$HERE/baseline-mcp.json" \
          --disallowedTools Read Grep Glob Edit Write WebFetch WebSearch \
          --append-system-prompt "$CLI_HINT" --dangerously-skip-permissions
      ) </dev/null >"$f" 2>"${f%.jsonl}.err" ;;
  esac
  rm -rf "$d"
}

# questions.json -> "id<TAB>question" lines (bash 3.2-safe: while-read, no mapfile)
n=0
while IFS=$'\t' read -r qid q; do
  [[ -z "${qid:-}" ]] && continue
  [[ -n "$QFILTER" && "$qid" != *"$QFILTER"* && "$q" != *"$QFILTER"* ]] && continue
  for rep in $(seq 1 "$REPS"); do
    for arm in $ARMS; do
      echo ">> $qid rep$rep  $arm" >&2
      cell "$OUT/${qid}.${arm}.${rep}.jsonl" "$arm" "$q"
      n=$((n+1))
    done
  done
done < <(python3 -c "import json
for x in json.load(open('$QFILE')): print(x['id']+chr(9)+x['question'].replace(chr(9),' '))")
echo ">> ran $n cells" >&2

if [[ "${JUDGE:-}" == "1" ]]; then
  echo ">> judging…" >&2
  node "$HERE/judge.mjs" "$OUT" "$QFILE" "$MODEL"
fi
node "$HERE/metrics.mjs" "$OUT" "$QFILE"
