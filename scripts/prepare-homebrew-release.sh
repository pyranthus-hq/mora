#!/usr/bin/env bash
# Verify a published signed-app release and render its Cask. Does not touch a tap.
set -euo pipefail

die() { printf 'prepare-homebrew-release: %s\n' "$*" >&2; exit 1; }
usage() { die 'usage: prepare-homebrew-release.sh --tag vMAJOR.MINOR.PATCH --out Casks/mora.rb'; }

tag=''
out=''
while (($#)); do
  case "$1" in
    --tag) (($# >= 2)) || usage; tag=$2; shift 2 ;;
    --out) (($# >= 2)) || usage; out=$2; shift 2 ;;
    *) usage ;;
  esac
done
[[ $tag =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || usage
[[ -n $out && $out != '-' ]] || usage
for command in gh jq curl cosign shasum go; do
  command -v "$command" >/dev/null || die "missing required command: $command"
done

repo='pyranthus-hq/mora'
version=${tag#v}
amd64="mora_${version}_darwin_amd64_app.zip"
arm64="mora_${version}_darwin_arm64_app.zip"
assets=("$amd64" "$arm64" checksums-app.txt checksums-app.txt.cosign.sig checksums-app.txt.cosign.pem)
metadata=$(gh api "repos/$repo/releases/tags/$tag") || die "release $tag could not be read"
jq -e --arg tag "$tag" '.tag_name == $tag and .draft == false and .prerelease == false and (.published_at | type == "string") and (.id | type == "number" and . > 0)' <<< "$metadata" >/dev/null \
  || die "release $tag is not a published, stable release with the requested tag"
release_id=$(jq -r '.id' <<< "$metadata")
# GitHub can return an empty embedded .assets list for a published release.
# The release assets endpoint is the authority for the complete, paginated list.
asset_pages=$(gh api --paginate --slurp "repos/$repo/releases/$release_id/assets?per_page=100") \
  || die "release $tag assets could not be read"
jq -e 'type == "array" and all(.[]; type == "array")' <<< "$asset_pages" >/dev/null \
  || die "release $tag assets response is invalid"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for asset in "${assets[@]}"; do
  expected_url="https://github.com/$repo/releases/download/$tag/$asset"
  asset_size=$(jq -er --arg name "$asset" --arg url "$expected_url" '
    [.[][] | select(.name == $name)] |
    if length == 1 and .[0].state == "uploaded" and
       (.[0].size | type == "number" and . > 0 and floor == .) and
       .[0].browser_download_url == $url
    then .[0].size else empty end
  ' <<< "$asset_pages") || die "release $tag lacks exactly one valid uploaded $asset"
  [[ -n $asset_size ]] || die "release $tag lacks exactly one valid uploaded $asset"
  curl --fail --location --silent --show-error --retry 3 --retry-all-errors \
    --proto '=https' --proto-redir '=https' --output "$work/$asset" "$expected_url" \
    || die "could not download $asset for $tag"
  downloaded_size=$(wc -c < "$work/$asset" | tr -d '[:space:]')
  [[ $downloaded_size == "$asset_size" ]] \
    || die "downloaded $asset size differs from release metadata"
done

cosign verify-blob \
  --certificate "$work/checksums-app.txt.cosign.pem" \
  --signature "$work/checksums-app.txt.cosign.sig" \
  --certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$tag" \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  "$work/checksums-app.txt" >/dev/null \
  || die "app checksum signature is invalid for $tag"

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
(cd "$root" && go run ./cmd/gencask --tag "$tag" --checksums "$work/checksums-app.txt" --out "$work/mora.rb") >/dev/null \
  || die "app checksum manifest is invalid for $tag"
(cd "$work" && shasum -a 256 -c checksums-app.txt) >/dev/null \
  || die "downloaded app bytes do not match checksums-app.txt"

mkdir -p "$(dirname "$out")"
cp "$work/mora.rb" "$out"
printf 'Verified signed app release %s; wrote %s\n' "$tag" "$out"
