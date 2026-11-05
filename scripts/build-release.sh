#!/usr/bin/env bash
# build-release.sh VERSION OS ARCH OUT: builds one platform's binaries and packs each with its licences into OUT.
set -euo pipefail

version="${1:?version}"
os="${2:?os}"
arch="${3:?arch}"
out="${4:?output directory}"
cd "$(dirname "$0")/.."
mkdir -p "$out"
out="$(cd "$out" && pwd)"
commit="$(git rev-parse --short=12 HEAD)"
pkg=github.com/ashhart/tf-starlight/internal/buildinfo
ldflags="-s -w -X $pkg.Version=$version -X $pkg.Commit=$commit"
ext=""
if [ "$os" = windows ]; then ext=.exe; fi
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

for cmd in starlight starlight-node tensorfold-connect; do
  name="$cmd-$version-$os-$arch"
  mkdir -p "$work/$name"
  # On a Mac only a cgo build of Connect has the menu-bar app and Keychain storage.
  cgo=0
  if [ "$os-$cmd" = darwin-tensorfold-connect ]; then
    if [ "$(go env GOHOSTOS)" != darwin ]; then
      echo "Connect for macOS needs cgo, so build this platform on a Mac" >&2
      exit 1
    fi
    cgo=1
  fi
  CGO_ENABLED="$cgo" GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$work/$name/$cmd$ext" "./cmd/$cmd"
  cp LICENSE NOTICE THIRD_PARTY_NOTICES.md "$work/$name/"
  if [ "$os-$cmd" = windows-starlight-node ]; then cp deploy/node/install-windows.ps1 "$work/$name/"; fi
  if [ "$os" = windows ]; then
    (cd "$work" && zip -qr "$out/$name.zip" "$name")
  else
    tar -C "$work" -czf "$out/$name.tar.gz" "$name"
  fi
done
