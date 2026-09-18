# Prebuilt binaries

A delivery channel, not a release process. The repository has no CI yet and
the box has no Go toolchain new enough to build whatsmeow, so the binary is
committed here to be fetched with `curl`.

Replace this with GitHub Releases once there is CI; binaries do not belong in
git history, and this one will be pruned when something better exists.

## Install

```sh
cd /tmp
curl -fsSLO https://raw.githubusercontent.com/arielpts/instinct-whatsapp-bridge/main/dist/wa-bridge.amd64.gz
curl -fsSLO https://raw.githubusercontent.com/arielpts/instinct-whatsapp-bridge/main/dist/SHA256SUMS

gunzip -f wa-bridge.amd64.gz
sha256sum -c SHA256SUMS          # verify before running it as root

install -m 0755 wa-bridge.amd64 /usr/local/bin/wa-bridge
wa-bridge
```
