#!/usr/bin/env bash
# Offline contract for a release whose tag API omits embedded assets.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/fixtures"
tag=v0.15.1
version=${tag#v}
amd64="mora_${version}_darwin_amd64_app.zip"
arm64="mora_${version}_darwin_arm64_app.zip"
assets=("$amd64" "$arm64" checksums-app.txt checksums-app.txt.cosign.sig checksums-app.txt.cosign.pem)

printf 'amd64 app fixture\n' > "$work/fixtures/$amd64"
printf 'arm64 app fixture\n' > "$work/fixtures/$arm64"
(cd "$work/fixtures" && shasum -a 256 "$amd64" "$arm64") > "$work/fixtures/checksums-app.txt"
printf 'signature fixture\n' > "$work/fixtures/checksums-app.txt.cosign.sig"
printf 'certificate fixture\n' > "$work/fixtures/checksums-app.txt.cosign.pem"

jq -n --arg tag "$tag" '{id:394528665,tag_name:$tag,draft:false,prerelease:false,published_at:"2026-09-23T00:00:00Z",assets:[]}' > "$work/release.json"
: > "$work/asset-lines.jsonl"
for asset in "${assets[@]}"; do
  size=$(wc -c < "$work/fixtures/$asset" | tr -d '[:space:]')
  jq -n --arg name "$asset" --arg url "https://github.com/pyranthus-hq/mora/releases/download/$tag/$asset" --argjson size "$size" \
    '{name:$name,state:"uploaded",size:$size,browser_download_url:$url}' >> "$work/asset-lines.jsonl"
done
jq -s '[.]' "$work/asset-lines.jsonl" > "$work/assets-good.json"
cp "$work/assets-good.json" "$work/assets.json"

cat > "$work/bin/gh" <<'MOCK_GH'
#!/usr/bin/env bash
set -euo pipefail
printf 'gh %s\n' "$*" >> "$MOCK_LOG"
case "$*" in
  'api repos/pyranthus-hq/mora/releases/tags/v0.15.1') cat "$MOCK_RELEASE" ;;
  'api --paginate --slurp repos/pyranthus-hq/mora/releases/394528665/assets?per_page=100') cat "$MOCK_ASSETS" ;;
  *) printf 'unexpected gh call: %s\n' "$*" >&2; exit 2 ;;
esac
MOCK_GH
cat > "$work/bin/curl" <<'MOCK_CURL'
#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\n' "$*" >> "$MOCK_LOG"
output=''
url=''
while (($#)); do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --proto|--proto-redir) shift 2 ;;
    --fail|--location|--silent|--show-error|--retry-all-errors) shift ;;
    --retry) shift 2 ;;
    https://github.com/pyranthus-hq/mora/releases/download/v0.15.1/*) url=$1; shift ;;
    *) printf 'unexpected curl argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
[[ -n $output && -n $url ]] || exit 2
cp "$MOCK_FIXTURES/${url##*/}" "$output"
if [[ ${MOCK_CURL_MODE:-} == oversize && $url == *darwin_arm64_app.zip ]]; then
  printf x >> "$output"
fi
MOCK_CURL
cat > "$work/bin/cosign" <<'MOCK_COSIGN'
#!/usr/bin/env bash
set -euo pipefail
printf 'cosign %s\n' "$*" >> "$MOCK_LOG"
[[ $1 == verify-blob ]]
MOCK_COSIGN
chmod +x "$work/bin/gh" "$work/bin/curl" "$work/bin/cosign"

run_prepare() {
  PATH="$work/bin:$PATH" MOCK_LOG="$work/calls.log" MOCK_RELEASE="$work/release.json" \
    MOCK_ASSETS="$work/assets.json" MOCK_FIXTURES="$work/fixtures" \
    bash "$root/scripts/prepare-homebrew-release.sh" --tag "$tag" --out "$work/mora.rb"
}
expect_failure() {
  local label=$1
  rm -f "$work/mora.rb"
  if run_prepare > "$work/stdout" 2> "$work/stderr"; then
    printf 'FAIL %s: unexpectedly succeeded\n' "$label" >&2
    exit 1
  fi
  [[ ! -e "$work/mora.rb" ]] || { printf 'FAIL %s: published output\n' "$label" >&2; exit 1; }
  printf 'ok   %s (refused)\n' "$label"
}

: > "$work/calls.log"
run_prepare > "$work/stdout" 2> "$work/stderr" || { cat "$work/stderr" >&2; exit 1; }
[[ -s "$work/mora.rb" ]] || { printf 'FAIL missing Cask\n' >&2; exit 1; }
grep -Eq 'version "0\.15\.1"' "$work/mora.rb"
grep -Eq 'releases/394528665/assets\?per_page=100' "$work/calls.log"
if grep -Eq 'gh release download' "$work/calls.log"; then
  printf 'FAIL used release download despite empty embedded assets\n' >&2
  exit 1
fi
[[ $(grep -Ec '^curl ' "$work/calls.log") == 5 ]]
grep -Eq '^cosign verify-blob ' "$work/calls.log"
printf 'ok   empty embedded assets use dedicated API and verified public downloads\n'

jq --arg name "$amd64" '.[0] |= map(select(.name != $name))' "$work/assets-good.json" > "$work/assets.json"
expect_failure 'missing required asset'
jq --arg name "$amd64" '.[0] += [.[0][] | select(.name == $name)]' "$work/assets-good.json" > "$work/assets.json"
expect_failure 'duplicate required asset'
jq --arg name "$amd64" '.[0] |= map(if .name == $name then .state = "new" else . end)' "$work/assets-good.json" > "$work/assets.json"
expect_failure 'asset not uploaded'
jq --arg name "$amd64" '.[0] |= map(if .name == $name then .size = 0 else . end)' "$work/assets-good.json" > "$work/assets.json"
expect_failure 'empty asset metadata'
jq --arg name "$amd64" '.[0] |= map(if .name == $name then .browser_download_url = "https://evil.example/app.zip" else . end)' "$work/assets-good.json" > "$work/assets.json"
expect_failure 'untrusted asset URL'
cp "$work/assets-good.json" "$work/assets.json"
MOCK_CURL_MODE=oversize expect_failure 'download size mismatch'
