# Provisioning

## The server

Hetzner **CAX11** — arm64, 2 vCPU (Ampere), 4 GB RAM, 40 GB NVMe. €5.99/month
plus €0.50 for the primary IPv4, as of the June 2026 price adjustment. Several
times what `OPS-1` needs, which is the point: headroom is cheaper than an
outage that unlinks the device.

arm64 matches our `CGO_ENABLED=0` cross-compile target. It is also now the
value tier outright — the June 2026 adjustment raised shared-vCPU plans ~30%
and dedicated-vCPU plans far more.

**CAX is offered only in Falkenstein, Nuremberg and Helsinki.** There is no arm64
in Hetzner's US or Singapore locations, so a box nearer Brazil means x86 and a
higher bill. Take Falkenstein. Stock runs out regularly and usually returns
within hours; if the type is greyed out, wait rather than switching tiers.

In the console: **New project** → `instinct-bridge` → **Add server**.

| Field | Value |
|---|---|
| Location | Falkenstein (fsn1) |
| Image | Debian 13 |
| Type | Arm64 → CAX11 |
| Networking | IPv4 + IPv6 (the bridge only dials out, but IPv6-only needs NAT64) |
| SSH key | your public key — add it here, do not accept a root password by mail |
| Firewall | inbound: SSH only. outbound: all |
| Name | `wa-bridge` |

Backups (+20%) are worth it: the volume holds the linked-device session, and
losing it means re-pairing from the phone.

## Without a terminal

A server can prepare itself. On the creation page, paste
[`cloud-config.yaml`](cloud-config.yaml) into the **Cloud config** field
(under additional features) and the box runs the bootstrap on first boot --
no console, no SSH client.

A terminal is still needed later, for pairing and for reading the journal.

## First login

```sh
ssh root@<ip>

apt update && apt full-upgrade -y
apt install -y unattended-upgrades ufw
dpkg-reconfigure -plow unattended-upgrades

ufw default deny incoming
ufw default allow outgoing
ufw allow OpenSSH
ufw enable

# No password logins, no root shell from outside.
sed -i 's/^#*PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
systemctl restart ssh

adduser --system --group --home /var/lib/wa-bridge wa-bridge
```

Nothing listens. The bridge dials out to WhatsApp over WSS, IMAP to read the
catch-all and SMTP submission to send, so no port is forwarded and no inbound
rule beyond SSH is needed.

## Install

Cross-compile from anywhere, ship one file. Check the box's architecture
first rather than assuming which plan was created:

```sh
ssh root@<ip> uname -m      # aarch64 -> arm64, x86_64 -> amd64

make arm64                  # or: make amd64
scp wa-bridge.arm64 root@<ip>:/usr/local/bin/wa-bridge

install -d -m 0750 -o root -g wa-bridge /etc/wa-bridge
install -m 0640 -o root -g wa-bridge deploy/env.example /etc/wa-bridge/env
install -m 0644 deploy/wa-bridge.service /etc/systemd/system/

openssl rand -base64 48   # -> WA_BRIDGE_HMAC_KEY
editor /etc/wa-bridge/env
editor /etc/wa-bridge/config.toml

systemctl daemon-reload
systemctl enable --now wa-bridge
journalctl -u wa-bridge -f
```

The first journal line names the domain, the mode and whether the sender
allowlist is filled. Read it: a misconfiguration belongs there rather than in a
message that went somewhere unexpected.

## Backups

`/var/lib/wa-bridge/state.db` holds the linked-device session. Lose it and you
re-pair from the phone; leak it and someone else holds a session on the account.
Back it up encrypted, off the box, and restore it once before trusting it.

```sh
sqlite3 /var/lib/wa-bridge/state.db ".backup '/tmp/state.db'"
age -r <recipient> /tmp/state.db > state.db.age   # or gpg -e
```
