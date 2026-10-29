#!/bin/sh
# Runs the end-to-end test in a Linux container against the host's Docker.
set -eu
cd "$(dirname "$0")/.."

docker build -q -t bumboat-e2e - <<'EOF'
FROM golang:1.27
COPY --from=docker:28-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker/buildx-bin /buildx /usr/libexec/docker/cli-plugins/docker-buildx
EOF

docker network inspect bumboat >/dev/null 2>&1 || docker network create bumboat >/dev/null

docker run --rm --network bumboat \
	-v /var/run/docker.sock:/var/run/docker.sock \
	-v "$PWD":/src -w /src \
	-v bumboat-e2e-gomod:/go/pkg/mod -v bumboat-e2e-gocache:/root/.cache \
	bumboat-e2e go test -tags e2e -run E2E -count 1 -v -timeout 20m "$@" .
