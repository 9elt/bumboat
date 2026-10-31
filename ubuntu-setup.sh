#!/bin/sh
# Sets up an empty Ubuntu 24.04 server for bumboat, from your machine:
# curl -fsSL https://raw.githubusercontent.com/9elt/bumboat/main/ubuntu-setup.sh | sh
set -eu

fail() { echo "bumboat: $*" >&2; exit 1; }

command -v ssh >/dev/null && command -v ssh-keygen >/dev/null || fail "install OpenSSH first"

# read from the terminal: stdin may be this script piped from curl
printf 'server ip: ' >/dev/tty
read -r ip </dev/tty
ip=$(echo "$ip" | tr -d '[:space:]')
[ -n "$ip" ] || fail "no ip given"

# bumboat's proxy answers unknown hosts with "no service for <host>"
url="http://$ip/"
case "$ip" in *:*) url="http://[$ip]/" ;; esac
if command -v curl >/dev/null && curl -s -m 5 -H 'Host: localhost' "$url" 2>/dev/null | grep -q '^no service for localhost'; then
	echo "bumboat is already running on $ip"
	exit 0
fi

name="bumboat-$(echo "$ip" | tr '.:' '--')"
key="$HOME/.ssh/$name"
mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh"
if [ ! -f "$key" ]; then
	echo "creating the ssh key $key"
	ssh-keygen -q -t ed25519 -N "" -C "bumboat@$ip" -f "$key"
fi

config="$HOME/.ssh/config"
if ! grep -qx "Host $ip" "$config" 2>/dev/null; then
	echo "adding $ip to $config"
	printf '\nHost %s\n    Hostname %s\n    AddKeysToAgent yes\n    IdentityFile ~/.ssh/%s\n' "$ip" "$ip" "$name" >>"$config"
	chmod 600 "$config"
fi

remote() { ssh -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes -i "$key" "$@"; }

# ssh asks for the root password once, to install the key
if ! remote -o BatchMode=yes "root@$ip" true </dev/null 2>/dev/null; then
	echo "copying the key to root@$ip"
	ssh -o StrictHostKeyChecking=accept-new -o PubkeyAuthentication=no "root@$ip" \
		'mkdir -p ~/.ssh && chmod 700 ~/.ssh && cat >>~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys' <"$key.pub"
	remote -o BatchMode=yes "root@$ip" true </dev/null || fail "the key was copied but logging in with it failed"
fi

echo "setting up $ip"
remote "root@$ip" sh -s <<'EOF'
set -eu
export DEBIAN_FRONTEND=noninteractive
. /etc/os-release
[ "$ID" = ubuntu ] || { echo "bumboat: this script is for Ubuntu, found $PRETTY_NAME" >&2; exit 1; }

apt-get -qq update
apt-get -qq -y upgrade
apt-get -qq -y install ufw git curl ca-certificates unattended-upgrades

ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
ufw allow OpenSSH >/dev/null
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw --force enable >/dev/null

# keys only from now on: the root password stops working over ssh
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin prohibit-password\n' >/etc/ssh/sshd_config.d/10-bumboat.conf
systemctl reload ssh

command -v docker >/dev/null || curl -fsSL https://get.docker.com | sh >/dev/null

id bumboat >/dev/null 2>&1 || adduser --disabled-password --gecos "" bumboat >/dev/null
install -d -m 700 -o bumboat -g bumboat /home/bumboat/.ssh
install -m 600 -o bumboat -g bumboat /root/.ssh/authorized_keys /home/bumboat/.ssh/authorized_keys

command -v bumboat >/dev/null || curl -fsSL https://raw.githubusercontent.com/9elt/bumboat/main/install.sh | sh
usermod -aG bumboat bumboat
EOF

echo "done: log in with ssh bumboat@$ip"
# stdin may be this script piped from curl: the TUI needs the terminal
remote -t "bumboat@$ip" bumboat </dev/tty
