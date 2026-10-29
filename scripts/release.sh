#!/bin/sh
# usage: scripts/release.sh v0.1.0
# Tags HEAD and pushes the tag; .github/workflows/release.yml builds and publishes the release.
set -eu
cd "$(dirname "$0")/.."
tag=${1:?usage: scripts/release.sh v1.2.3}

echo "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || { echo "tag must look like v1.2.3" >&2; exit 1; }
[ -z "$(git status --porcelain)" ] || { echo "commit or stash your changes first" >&2; exit 1; }
git rev-parse -q --verify "refs/tags/$tag" >/dev/null && { echo "$tag already exists" >&2; exit 1; }

go test ./...
git tag -a "$tag" -m "$tag"
git push origin "$tag"
echo "pushed $tag: GitHub Actions is building the release"
