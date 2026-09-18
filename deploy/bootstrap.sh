#!/usr/bin/env bash
# Prepare a fresh Debian box for the bridge. Idempotent: safe to re-run.
#
#   ssh root@<ip>
#   curl -fsSLO https://raw.githubusercontent.com/arielpts/instinct-whatsapp-bridge/main/deploy/bootstrap.sh
#   less bootstrap.sh     # it runs as root; read it first
#   bash bootstrap.sh
#
# It installs no bridge binary and starts no service. It only makes the box
# ready for one, and prints what to do next.

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
	echo "run as root" >&2
	exit 1
fi

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

say "Updating and installing"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get full-upgrade -y -qq
apt-get install -y -qq unattended-upgrades ufw sqlite3 ca-certificates

say "Enabling unattended security upgrades"
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'CONF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
CONF

say "Firewall: deny inbound except SSH"
# The bridge only dials out -- WhatsApp over WSS, IMAP, SMTP submission -- so
# nothing needs to reach it.
ufw --force default deny incoming
ufw --force default allow outgoing
ufw allow OpenSSH
ufw --force enable

say "SSH: keys only, no password logins"
sed -i 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
systemctl restart ssh || systemctl restart sshd

say "Service user and directories"
id -u wa-bridge >/dev/null 2>&1 || adduser --system --group --home /var/lib/wa-bridge wa-bridge
install -d -m 0700 -o wa-bridge -g wa-bridge /var/lib/wa-bridge
install -d -m 0750 -o root -g wa-bridge /etc/wa-bridge

say "Secrets file"
if [ ! -f /etc/wa-bridge/env ]; then
	# The signing key is born here and never travels: generating it on the box
	# means it has never existed anywhere else (SEC-8).
	KEY=$(openssl rand -base64 48)
	cat > /etc/wa-bridge/env <<CONF
WA_BRIDGE_MAIL_DOMAIN=
WA_BRIDGE_MAILBOX=
WA_BRIDGE_ASSISTANT_ADDRESS=

WA_BRIDGE_IMAP_HOST=
WA_BRIDGE_IMAP_USER=
WA_BRIDGE_IMAP_PASSWORD=

WA_BRIDGE_SMTP_HOST=
WA_BRIDGE_SMTP_USER=
WA_BRIDGE_SMTP_PASSWORD=

WA_BRIDGE_HMAC_KEY=${KEY}

WA_BRIDGE_STATE_DIR=/var/lib/wa-bridge
WA_BRIDGE_CONFIG=/etc/wa-bridge/config.toml
WA_BRIDGE_MODE=draft-only
CONF
	chown root:wa-bridge /etc/wa-bridge/env
	chmod 0640 /etc/wa-bridge/env
	echo "created /etc/wa-bridge/env with a fresh signing key"
else
	echo "/etc/wa-bridge/env exists; left alone"
fi

say "Done"
cat <<SUMMARY

  architecture : $(uname -m)     <- tells you which binary to build
  debian       : $(. /etc/os-release && echo "$PRETTY_NAME")
  memory       : $(free -h | awk '/^Mem:/ {print $2}')
  disk free    : $(df -h / | awk 'NR==2 {print $4}')

Next:
  1. Report the architecture above. aarch64 -> make arm64, x86_64 -> make amd64.
  2. Fill in the mail settings in /etc/wa-bridge/env once the catch-all exists.
  3. Write /etc/wa-bridge/config.toml -- the policy half. Start with an empty
     conversation list; the bridge forwards nothing until one is added.
  4. Install the binary, then pair.

The mode is pinned to draft-only. Nothing can be sent until that changes, and
the environment can only ever tighten it further.
SUMMARY
