#!/bin/sh
# Builds the release assets that install.sh and `bumboat upgrade` download.
# usage: scripts/release.sh v0.1.0, then upload dist/* to a GitHub release with that tag.
set -eu
cd "$(dirname "$0")/.."
tag=${1:?usage: scripts/release.sh <tag>}

rm -rf dist && mkdir dist
for arch in amd64 arm64; do
	GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$tag" -o "dist/bumboat-linux-$arch" .
done
(cd dist && shasum -a 256 bumboat-linux-* >checksums.txt)
ls -lh dist
