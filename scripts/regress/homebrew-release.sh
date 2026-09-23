#!/usr/bin/env bash
# Secret-free release metadata and asset contract for the Homebrew handoff.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/assets"
tag=v0.15.0
version=${tag#v}
amd64="mora_${version}_darwin_amd64_app.zip"
arm64="mora_${version}_darwin_arm64_app.zip"
printf 'amd64-app-zip\n' > "$work/assets/$amd64"
printf 'arm64-app-zip\n' > "$work/assets/$arm64"
(cd "$work/assets" && shasum -a 256 "$amd64" "$arm64") > "$work/assets/checksums-app.txt"
printf 'fixture signature\n' > "$work/assets/checksums-app.txt.cosign.sig"
printf 'fixture certificate\n' > "$work/assets/checksums-app.txt.cosign.pem"

cat > "$work/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'api repos/pyranthus-hq/mora/releases/tags/v0.15.0') cat "$MOCK_METADATA" ;;
  'api --paginate --slurp repos/pyranthus-hq/mora/releases/394528664/assets?per_page=100') cat "$MOCK_ASSET_METADATA" ;;
  *) printf 'unexpected gh call: %s\n' "$*" >&2; exit 2 ;;
esac
SH
cat > "$work/bin/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
output=''
url=''
while (($#)); do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --proto|--proto-redir|--retry) shift 2 ;;
    --fail|--location|--silent|--show-error|--retry-all-errors) shift ;;
    https://github.com/pyranthus-hq/mora/releases/download/v0.15.0/*) url=$1; shift ;;
    *) printf 'unexpected curl argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
[[ -n $output && -n $url ]] || exit 2
cp "$MOCK_ASSETS/${url##*/}" "$output"
SH
cat > "$work/bin/cosign" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == verify-blob ]] || exit 2
printf '%s\n' "$*" > "$MOCK_COSIGN_ARGS"
[[ ${MOCK_COSIGN_FAIL:-0} != 1 ]]
SH
chmod +x "$work/bin/gh" "$work/bin/curl" "$work/bin/cosign"
export PATH="$work/bin:$PATH" MOCK_ASSETS="$work/assets" MOCK_METADATA="$work/metadata.json" MOCK_ASSET_METADATA="$work/asset-pages.json" MOCK_COSIGN_ARGS="$work/cosign-args"

metadata() {
  jq -n --arg tag "$tag" \
    '{id:394528664,tag_name:$tag,draft:false,prerelease:false,published_at:"2026-09-23T00:00:00Z",assets:[]}' > "$MOCK_METADATA"
  : > "$work/asset-lines.jsonl"
  for asset in "$amd64" "$arm64" checksums-app.txt checksums-app.txt.cosign.sig checksums-app.txt.cosign.pem; do
    size=$(wc -c < "$MOCK_ASSETS/$asset" | tr -d '[:space:]')
    jq -n --arg name "$asset" --arg url "https://github.com/pyranthus-hq/mora/releases/download/$tag/$asset" --argjson size "$size" \
      '{name:$name,state:"uploaded",size:$size,browser_download_url:$url}' >> "$work/asset-lines.jsonl"
  done
  jq -s '[.]' "$work/asset-lines.jsonl" > "$MOCK_ASSET_METADATA"
}
run() { bash "$root/scripts/prepare-homebrew-release.sh" --tag "$tag" --out "$work/out/mora.rb"; }
fail_case() {
  if run > "$work/stdout" 2> "$work/stderr"; then
    printf 'FAIL: accepted %s\n' "$1" >&2; exit 1
  fi
  [[ ! -e "$work/out/mora.rb" ]] || { printf 'FAIL: wrote output for %s\n' "$1" >&2; exit 1; }
  printf 'PASS: rejects %s\n' "$1"
}

metadata
run > "$work/stdout"
grep -Fq 'version "0.15.0"' "$work/out/mora.rb"
grep -Fq 'refs/tags/v0.15.0' "$MOCK_COSIGN_ARGS"
grep -Fq -- '--certificate-oidc-issuer https://token.actions.githubusercontent.com' "$MOCK_COSIGN_ARGS"
if grep -Eq 'auto_updates|quarantine' "$work/out/mora.rb"; then
  echo 'FAIL: Cask advertised updater or stripped quarantine' >&2; exit 1
fi
printf 'PASS: signed release generates app Cask\n'

rm "$work/out/mora.rb"
metadata
jq '.draft=true' "$MOCK_METADATA" > "$work/changed.json"
cp "$work/changed.json" "$MOCK_METADATA"
fail_case draft

metadata
jq '.prerelease=true' "$MOCK_METADATA" > "$work/changed.json"
cp "$work/changed.json" "$MOCK_METADATA"
fail_case prerelease

metadata
jq --arg name "$arm64" '.[0] |= map(select(.name != $name))' "$MOCK_ASSET_METADATA" > "$work/changed.json"
cp "$work/changed.json" "$MOCK_ASSET_METADATA"
fail_case missing-arm64-asset

metadata
printf 'ARM64-app-zip\n' > "$work/assets/$arm64"
fail_case mismatched-app-checksum

metadata
export MOCK_COSIGN_FAIL=1
fail_case invalid-manifest-signature
