#!/bin/sh
# Opens the TUI against a dev daemon running in a Linux container on the host's Docker.
# `scripts/dev.sh` rebuilds and opens the TUI, `scripts/dev.sh restart` also rebuilds the daemon,
# `scripts/dev.sh logs` follows the daemon, `scripts/dev.sh reset` wipes its state.
set -eu
cd "$(dirname "$0")/.."

docker build -q -t bumboat-e2e - >/dev/null <<'EOF'
FROM golang:1.27
COPY --from=docker:28-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker/buildx-bin /buildx /usr/libexec/docker/cli-plugins/docker-buildx
EOF

docker network inspect bumboat >/dev/null 2>&1 || docker network create bumboat >/dev/null

case "${1:-}" in
restart) docker rm -f bumboat-dev >/dev/null 2>&1 || true ;;
logs) exec docker logs -f bumboat-dev ;;
reset)
	docker rm -f bumboat-dev >/dev/null 2>&1 || true
	docker volume rm bumboat-dev bumboat-dev-key >/dev/null 2>&1 || true
	exit
	;;
esac

if [ "$(docker inspect -f '{{.State.Running}}' bumboat-dev 2>/dev/null)" != true ]; then
	docker rm -f bumboat-dev >/dev/null 2>&1 || true
	docker run -d --name bumboat-dev --network bumboat \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-v "$PWD":/src -w /src \
		-v bumboat-e2e-gomod:/go/pkg/mod -v bumboat-e2e-gocache:/root/.cache \
		-v bumboat-dev:/var/lib/bumboat -v bumboat-dev-key:/etc/bumboat \
		bumboat-e2e sh -c 'go build -o /bumboatd . && exec /bumboatd daemon' >/dev/null
	until docker exec bumboat-dev test -S /run/bumboat/bumboat.sock 2>/dev/null; do
		[ "$(docker inspect -f '{{.State.Running}}' bumboat-dev)" = true ] || exec docker logs bumboat-dev
		sleep 0.5
	done
fi

exec docker exec -it -e TERM="${TERM:-xterm-256color}" -e COLORTERM="${COLORTERM:-}" bumboat-dev \
	sh -c 'go build -o /usr/local/bin/bumboat . && exec bumboat'
