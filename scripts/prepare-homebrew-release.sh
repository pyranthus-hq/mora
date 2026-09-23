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
for command in gh jq cosign shasum go; do
  command -v "$command" >/dev/null || die "missing required command: $command"
done

repo='pyranthus-hq/mora'
version=${tag#v}
amd64="mora_${version}_darwin_amd64_app.zip"
arm64="mora_${version}_darwin_arm64_app.zip"
assets=("$amd64" "$arm64" checksums-app.txt checksums-app.txt.cosign.sig checksums-app.txt.cosign.pem)
metadata=$(gh api "repos/$repo/releases/tags/$tag") || die "release $tag could not be read"
jq -e --arg tag "$tag" '.tag_name == $tag and .draft == false and .prerelease == false and (.published_at | type == "string")' <<< "$metadata" >/dev/null \
  || die "release $tag is not a published, stable release with the requested tag"
for asset in "${assets[@]}"; do
  jq -e --arg name "$asset" '[.assets[] | select(.name == $name and .state == "uploaded" and .size > 0)] | length == 1' <<< "$metadata" >/dev/null \
    || die "release $tag lacks exactly one uploaded $asset"
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gh release download "$tag" --repo "$repo" --dir "$work" \
  --pattern "$amd64" --pattern "$arm64" --pattern checksums-app.txt \
  --pattern checksums-app.txt.cosign.sig --pattern checksums-app.txt.cosign.pem \
  || die "could not download required app assets for $tag"
for asset in "${assets[@]}"; do
  [[ -s "$work/$asset" ]] || die "downloaded $asset is missing or empty"
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
