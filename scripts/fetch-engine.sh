#!/bin/sh
# fetch-engine.sh PLATFORM DIR [latest]: unpack the TensorFold release engine.json pins, or the newest, into DIR.
set -eu
platform=$1
dir=$2
pin=$(dirname "$0")/../engine.json
repo=$(jq -r .repository "$pin")
if [ "${3:-}" = latest ]; then
  auth=
  [ -z "${GH_TOKEN:-}" ] || auth="Authorization: Bearer $GH_TOKEN"
  release=$(curl -fsSL ${auth:+-H "$auth"} "https://api.github.com/repos/$repo/releases?per_page=30" |
    jq '[.[] | select((.draft or .prerelease | not) and (.tag_name | test("^v[0-9]")))][0]')
  version=$(printf '%s' "$release" | jq -r '.tag_name | ltrimstr("v")')
  name=tensorfold-$version-$platform.tar.gz
  sum=$(printf '%s' "$release" | jq -r --arg n "$name" '.assets[] | select(.name == $n) | .digest // empty | ltrimstr("sha256:")')
else
  version=$(jq -r .version "$pin")
  name=tensorfold-$version-$platform.tar.gz
  sum=$(jq -r --arg p "$platform" '.archives[$p] // empty' "$pin")
fi
[ -n "$sum" ] || { echo "TensorFold $version has no $platform archive with a SHA-256" >&2; exit 1; }
archive=$(mktemp)
trap 'rm -f "$archive"' EXIT
curl -fsSL -o "$archive" "https://github.com/$repo/releases/download/v$version/$name"
if command -v sha256sum > /dev/null; then got=$(sha256sum "$archive"); else got=$(shasum -a 256 "$archive"); fi
[ "${got%% *}" = "$sum" ] || { echo "$name does not match SHA-256 $sum" >&2; exit 1; }
mkdir -p "$dir"
tar -xzf "$archive" -C "$dir" --strip-components=1
"$dir/bin/tensorfold-native" --version | grep -q " $version\$" || { echo "$dir is not TensorFold $version" >&2; exit 1; }
echo "$dir/bin/tensorfold-native"
