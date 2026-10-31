#!/bin/sh
# Sets up an empty Ubuntu 24.04 server for bumboat, from your machine:
# curl -fsSL https://raw.githubusercontent.com/9elt/bumboat/main/ubuntu-setup.sh | sh
set -eu

fail() { echo "bumboat: $*" >&2; exit 1; }
step() { echo "[$1/12] $2"; }

command -v ssh >/dev/null && command -v ssh-keygen >/dev/null || fail "install OpenSSH first"

# read from the terminal: stdin may be this script piped from curl
printf 'server ip: ' >/dev/tty
read -r ip </dev/tty
ip=$(echo "$ip" | tr -d '[:space:]')
[ -n "$ip" ] || fail "no ip given"

# bumboat's proxy answers unknown hosts with "no service for <host>"
step 1 "checking $ip for a running bumboat"
url="http://$ip/"
case "$ip" in *:*) url="http://[$ip]/" ;; esac
if command -v curl >/dev/null && curl -s -m 5 -H 'Host: localhost' "$url" 2>/dev/null | grep -q '^no service for localhost'; then
	echo "bumboat is already running on $ip"
	exit 0
fi

name="bumboat-$(echo "$ip" | tr '.:' '--')"
key="$HOME/.ssh/$name"
mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh"
if [ -f "$key" ]; then
	step 2 "ssh key $key (already done)"
else
	step 2 "creating the ssh key $key"
	ssh-keygen -q -t ed25519 -N "" -C "bumboat@$ip" -f "$key"
fi

config="$HOME/.ssh/config"
if grep -qx "Host $ip" "$config" 2>/dev/null; then
	step 3 "$ip in $config (already done)"
else
	step 3 "adding $ip to $config"
	printf '\nHost %s\n    Hostname %s\n    AddKeysToAgent yes\n    IdentityFile ~/.ssh/%s\n' "$ip" "$ip" "$name" >>"$config"
	chmod 600 "$config"
fi

remote() { ssh -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes -i "$key" "$@"; }

# ssh asks for the root password once, to install the key
if remote -o BatchMode=yes "root@$ip" true </dev/null 2>/dev/null; then
	step 4 "key on root@$ip (already done)"
else
	step 4 "copying the key to root@$ip"
	ssh -o StrictHostKeyChecking=accept-new -o PubkeyAuthentication=no "root@$ip" \
		'mkdir -p ~/.ssh && chmod 700 ~/.ssh && cat >>~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys' <"$key.pub"
	remote -o BatchMode=yes "root@$ip" true </dev/null || fail "the key was copied but logging in with it failed"
fi

remote "root@$ip" sh -s <<'EOF'
set -eu
step() { echo "[$1/12] $2"; }
export DEBIAN_FRONTEND=noninteractive LC_ALL=C.UTF-8
. /etc/os-release
[ "$ID" = ubuntu ] || { echo "bumboat: this script is for Ubuntu, found $PRETTY_NAME" >&2; exit 1; }

step 5 "upgrading packages, this can take a few minutes"
apt-get -qq update
apt-get -qq -y upgrade >/dev/null
step 6 "installing ufw, git and curl"
apt-get -qq -y install ufw git curl ca-certificates unattended-upgrades >/dev/null

step 7 "allowing only ssh, http and https through the firewall"
ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
ufw allow OpenSSH >/dev/null
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw --force enable >/dev/null

# keys only from now on: the root password stops working over ssh
step 8 "turning off ssh password login"
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin prohibit-password\n' >/etc/ssh/sshd_config.d/10-bumboat.conf
systemctl try-reload-or-restart ssh

if command -v docker >/dev/null; then
	step 9 "docker (already done)"
else
	step 9 "installing docker"
	curl -fsSL https://get.docker.com | sh >/dev/null
fi

step 10 "creating the bumboat user with your key"
id bumboat >/dev/null 2>&1 || adduser --disabled-password --gecos "" bumboat >/dev/null
install -d -m 700 -o bumboat -g bumboat /home/bumboat/.ssh
install -m 600 -o bumboat -g bumboat /root/.ssh/authorized_keys /home/bumboat/.ssh/authorized_keys

if command -v bumboat >/dev/null; then
	step 11 "bumboat (already done)"
else
	step 11 "installing bumboat"
	curl -fsSL https://raw.githubusercontent.com/9elt/bumboat/main/install.sh | sh >/dev/null
fi
usermod -aG bumboat bumboat
EOF

step 12 "done, opening bumboat (next time run ssh bumboat@$ip bumboat)"
# stdin may be this script piped from curl: the TUI needs the terminal
remote -t "bumboat@$ip" bumboat </dev/tty
