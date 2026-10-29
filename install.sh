#!/bin/sh
# curl -fsSL https://raw.githubusercontent.com/9elt/bumboat/main/install.sh | sudo sh
set -eu

fail() { echo "bumboat: $*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || fail "runs on Linux only"
[ "$(id -u)" = 0 ] || fail "run as root: curl … | sudo sh"
command -v docker >/dev/null || fail "install Docker first: https://docs.docker.com/engine/install/"
docker buildx version >/dev/null 2>&1 || fail "the docker buildx plugin is missing (apt install docker-buildx-plugin)"
command -v git >/dev/null || fail "install git first"
if command -v ss >/dev/null && ss -ltnH '( sport = :80 or sport = :443 )' | grep -q .; then
	fail "ports 80 and 443 must be free: bumboat is the reverse proxy"
fi

case "$(uname -m)" in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "unsupported architecture $(uname -m)" ;;
esac

tag=$(curl -fsSL https://api.github.com/repos/9elt/bumboat/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
[ -n "$tag" ] || fail "no release found"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/bumboat-linux-$arch" "https://github.com/9elt/bumboat/releases/download/$tag/bumboat-linux-$arch"
curl -fsSL -o "$tmp/checksums.txt" "https://github.com/9elt/bumboat/releases/download/$tag/checksums.txt"
(cd "$tmp" && grep " bumboat-linux-$arch\$" checksums.txt | sha256sum -c - >/dev/null) || fail "checksum mismatch"
install -m 755 "$tmp/bumboat-linux-$arch" /usr/local/bin/bumboat

getent group bumboat >/dev/null || groupadd --system bumboat
[ -n "${SUDO_USER:-}" ] && usermod -aG bumboat "$SUDO_USER"

cat >/etc/systemd/system/bumboat.service <<'EOF'
[Unit]
Description=Bumboat
After=docker.service network-online.target
Requires=docker.service

[Service]
ExecStart=/usr/local/bin/bumboat daemon
Environment=GOMEMLIMIT=64MiB
RuntimeDirectory=bumboat
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now bumboat

echo "bumboat $tag is running."
echo "Log out and back in (to join the bumboat group), then run: bumboat"
