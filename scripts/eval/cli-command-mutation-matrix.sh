#!/usr/bin/env bash
set -euo pipefail

# CLI-wide dispatch mutation audit for issue #205.
#
# Each exact production dispatch token is renamed in an isolated source
# snapshot. The mutated package is recompiled and that row's real-Run regression
# test MUST turn red because the registered token has become behaviorally
# indistinguishable from its unknown-token control. The AST drift test remains
# the fast CI guard; this manual replay proves the runtime witness is load-bearing.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO="${GO:-go}"
STATUS="$(git -C "$ROOT" status --porcelain)"
if [[ -n "$STATUS" && "${ALLOW_DIRTY:-0}" != "1" ]]; then
  echo "refusing CLI mutation closeout from a dirty checkout; commit/stash first (or use ALLOW_DIRTY=1 for non-closeout development evidence)" >&2
  exit 1
fi

TMP="$(mktemp -d "${TMPDIR:-/tmp}/mora-cli-mutation.XXXXXX")"
BASE="$TMP/base"
WORK="$TMP/work"
ROWS="$TMP/rows.tsv"
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
mkdir -p "$BASE" "$WORK"

if [[ -z "$STATUS" ]]; then
  git -C "$ROOT" archive HEAD | tar -x -C "$BASE"
else
  (
    cd "$ROOT"
    git ls-files --cached --others --exclude-standard -z |
      tar --null --files-from=- -cf -
  ) | tar -x -C "$BASE"
fi
cp -R "$BASE/." "$WORK/"

(
  cd "$WORK"
  "$GO" test ./internal/mora -run '^TestCLIRegistry' -count=1
)

python3 - "$BASE/internal/mora/eval/cli-command-registry.json" >"$ROWS" <<'PY'
import json
import re
import sys

registry = json.load(open(sys.argv[1], encoding="utf-8"))
families = {
    "brief": ("internal/mora/mora.go", "cmdBrief"),
    "config": ("internal/mora/config.go", "cmdConfig"),
    "index": ("internal/mora/index.go", "cmdIndex"),
    "forget": ("internal/mora/governance_cmd.go", "cmdForget"),
    "merge": ("internal/mora/merge_cmd.go", "cmdMerge"),
    "teach": ("internal/mora/teach_cmd.go", "cmdTeach"),
    "tasks": ("internal/mora/tasks.go", "cmdTasks"),
    "schedule": ("internal/mora/schedule.go", "cmdSchedule"),
    "sources": ("internal/mora/sources.go", "cmdSources"),
    "connectors": ("internal/mora/setup.go", "cmdConnectors"),
    "ingest": ("internal/mora/ingest.go", "cmdIngest"),
    "connect": ("internal/mora/ingest.go", "cmdConnect"),
    "sync": ("internal/mora/ingest.go", "cmdSync"),
    "share": ("internal/mora/share.go", "cmdShare"),
    "usage": ("internal/mora/usage.go", "cmdUsage"),
    "disconnect": ("internal/mora/setup.go", "cmdDisconnect"),
    "mcp": ("internal/mora/mcp.go", "cmdMCP"),
    "serve": ("internal/mora/serve_http.go", "cmdServe"),
    "hook": ("internal/mora/hook.go", "cmdHook"),
    "loop": ("internal/mora/loop.go", "cmdLoop"),
}
deep_families = {
    ("teach", "identity"): ("internal/mora/merge_cmd.go", "cmdMerge"),
    ("teach", "consent"): ("internal/mora/teach_cmd.go", "teachConsent"),
    ("usage", "queries"): ("internal/mora/usage.go", "cmdUsage"),
    ("serve", "http"): ("internal/mora/serve_http.go", "cmdServe"),
}
for row in registry["commands"]:
    fields = row["path"].split()
    if len(fields) == 1:
        source, function, token = "internal/mora/mora.go", "Run", fields[0]
    elif len(fields) == 2:
        source, function = families[fields[0]]
        token = fields[1]
    elif len(fields) == 3:
        source, function = deep_families[tuple(fields[:2])]
        token = fields[2]
    else:
        raise SystemExit(f"unsupported registry depth: {row['path']}")
    run_re = r"^TestCLIRegistryRealRunDispatch$" + "".join(
        r"/^" + re.escape(field) + r"$" for field in fields
    )
    anchor = "args[1]" if fields[:2] == ["usage", "queries"] else "-"
    print(row["path"], source, function, token, run_re, anchor, sep="\t")
PY

mutate() {
  local file="$1" function="$2" token="$3" anchor="$4"
  python3 - "$file" "$function" "$token" "$anchor" <<'PY'
import re
import sys

path, function, token, anchor = sys.argv[1:]
text = open(path, encoding="utf-8").read()
start_match = re.search(r"(?m)^func\s+" + re.escape(function) + r"\s*\(", text)
if not start_match:
    raise SystemExit(f"{path}: function {function} not found")
next_match = re.search(r"(?m)^func\s+", text[start_match.end():])
end = len(text) if not next_match else start_match.end() + next_match.start()
section = text[start_match.start():end]
quoted = re.escape('"' + token + '"')

# A case clause is the normal shape. The comparison forms cover the few
# hand-written dispatchers (brief/index/ingest/connect/sync/mcp/serve).
patterns = []
if anchor != "-":
    patterns.append(
        r"(?m)^([^\n]*" + re.escape(anchor) + r"[^\n]*?(?:==|!=)[ \t]*)" + quoted
    )
patterns += [
    r"(?m)^([ \t]*case[^\n]*?)" + quoted,
    r"(?m)^([^\n]*(?:args\[[01]\]|rest\[0\]|fs\.Arg\(0\))[^\n]*?(?:==|!=)[ \t]*)" + quoted,
]
for pattern in patterns:
    changed, count = re.subn(pattern, r'\1"__issue205_mutant__"', section, count=1)
    if count == 1:
        open(path, "w", encoding="utf-8").write(
            text[:start_match.start()] + changed + text[end:]
        )
        break
else:
    raise SystemExit(f"{path}:{function}: dispatch token {token!r} has no exact production anchor")
PY
}

count=0
selected=0
while IFS=$'\t' read -r path source function token run_re anchor; do
  if [[ -n "${ONLY_PATH:-}" && "$path" != "$ONLY_PATH" ]]; then
    continue
  fi
  selected=$((selected + 1))
  cp "$BASE/$source" "$WORK/$source"
  mutate "$WORK/$source" "$function" "$token" "$anchor"
  log="$TMP/mutant.log"
  if (
    cd "$WORK"
    "$GO" test ./internal/mora -run "$run_re" -count=1 >"$log" 2>&1
  ); then
    echo "SURVIVED  $path ($source:$function token=$token)" >&2
    cat "$log" >&2
    exit 1
  fi
  if ! grep -Eq 'registered token (is behaviorally indistinguishable from an unknown token|did not reach its production behavior)' "$log"; then
    echo "WRONG RED $path: witness failed for an unrelated reason" >&2
    cat "$log" >&2
    exit 1
  fi
  count=$((count + 1))
  printf 'KILLED %3d  %s\n' "$count" "$path"
done <"$ROWS"

if [[ "$selected" -eq 0 ]]; then
  echo "no registry row matched ONLY_PATH=${ONLY_PATH:-}" >&2
  exit 1
fi
if [[ -z "${ONLY_PATH:-}" && "$count" -ne 124 ]]; then
  echo "CLI dispatch mutation matrix processed $count rows; want 124" >&2
  exit 1
fi
echo "CLI dispatch mutation matrix: $count/$selected selected production tokens KILLED; zero selected registry holes"
if [[ -n "$STATUS" ]]; then
  echo "NON-CLOSEOUT: ALLOW_DIRTY=1 snapshot; repeat from a clean immutable revision for final evidence"
fi
