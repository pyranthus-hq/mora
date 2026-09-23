#!/bin/sh
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
binary="$repo/.cache/recovery/mora"
root="$repo/.cache/recovery/smoke"
test -x "$binary"
test ! -e "$root"
mkdir -p "$root/config" "$root/home" "$root/input"
cp "$repo/integration-fixtures/recovery-input.md" "$root/input/note.md"

run_mora() {
  env -i PATH="$PATH" HOME="$root/home" MORA_CONFIG_DIR="$root/config" \
    MORA_VAULT="$root/config/vault" MORA_EMBEDDER=static DO_NOT_TRACK=1 \
    MORA_NO_BANNER=1 "$binary" "$@"
}

run_mora init > "$root/init.txt"
test -d "$root/config/state"
test -d "$root/config/vault"
run_mora sources add filesystem --name recovery-docs --path "$root/input" \
  --scope project:recovery > "$root/source-add.txt"
run_mora ingest run --source recovery-docs --json > "$root/ingest.json"
run_mora search 'Acorn pilot launch' --scope project:recovery --json > "$root/search.json"
source_id=$(jq -er '.memories[] | select(.title == "note.md") | .id' "$root/search.json")
jq -e --arg id "$source_id" '.memories[] | select(.id == $id and .scope == "project:recovery" and .provenance == "document" and (.text | contains("2026-09-24")))' "$root/search.json" >/dev/null
run_mora read "$source_id" --json > "$root/read.json"
jq -e --arg id "$source_id" 'select(.id == $id and .provenance == "document" and (.text | contains("2026-09-24")))' "$root/read.json" >/dev/null

run_mora write --scope project:recovery --target "$source_id" --disposition outdated \
  --title 'Synthetic correction' --text 'The Acorn pilot launch was cancelled.' \
  --json > "$root/correction-write.json"
correction_id=$(jq -er '.id' "$root/correction-write.json")

# Every run_mora call starts a separate process. These final reads verify the
# persisted vault and index after the writing process has exited.
run_mora search 'Acorn pilot launch' --scope project:recovery --json > "$root/restart-search.json"
jq -e --arg id "$source_id" --arg correction "$correction_id" \
  '.memories[] | select(.id == $id and .disposition.value == "outdated" and .disposition.correction_id == $correction and (.explicit_corrections[]?.id == $correction))' \
  "$root/restart-search.json" >/dev/null
run_mora read "$source_id" --json > "$root/restart-read.json"
jq -e --arg id "$source_id" 'select(.id == $id and .provenance == "document")' "$root/restart-read.json" >/dev/null

jq -n --arg source_id "$source_id" --arg correction_id "$correction_id" \
  --arg config_dir "$root/config" \
  '{result:"pass", source_id:$source_id, correction_id:$correction_id, config_dir:$config_dir, state_dir:($config_dir+"/state"), vault_dir:($config_dir+"/vault")}' \
  > "$root/summary.json"
cat "$root/summary.json"
