# DNS for the bridge subdomain

The bridge needs a dedicated subdomain that accepts **any** local part, because
every conversation is its own address. Call it `wa.example.com` here; the real
one lives in `/etc/wa-bridge/env` and nowhere in this repository.

Records go wherever the **apex domain's authoritative nameservers** are --
whoever answers `NS` for it. That is usually the registrar or a DNS host, and it
is often not the same company as the server. Adding the records anywhere else
has no effect.

## What has to exist, and why

| Record | Name | Purpose |
|---|---|---|
| MX | `wa.example.com` | where mail for every conversation address is delivered |
| TXT (SPF) | `wa.example.com` | says which servers may send as this subdomain |
| CNAME ×3 (DKIM) | `key1/2/3._domainkey.wa.example.com` | signs outbound mail, so the assistant can verify a forward is ours |
| TXT (DMARC) | `_dmarc.wa.example.com` | tells receivers to reject anything unsigned |
| TXT (verify) | `wa.example.com` | proves to the provider that the domain is yours |

A subdomain, never the apex: the catch-all must not sit on the domain that
carries real mail, and keeping SPF/DKIM/DMARC separate keeps this project's
sending reputation away from everything else.

## With Migadu

Add the domain at `admin.migadu.com` first. It issues the verification token and
the DKIM targets, which are **per domain** -- take them from the panel rather
than from any example, including this one.

```
wa.example.com.                MX     10  aspmx1.migadu.com.
wa.example.com.                MX     20  aspmx2.migadu.com.

wa.example.com.                TXT    "v=spf1 include:spf.migadu.com -all"
wa.example.com.                TXT    "hosted-email-verify=<token from the panel>"

key1._domainkey.wa.example.com. CNAME  key1.wa.example.com._domainkey.migadu.com.
key2._domainkey.wa.example.com. CNAME  key2.wa.example.com._domainkey.migadu.com.
key3._domainkey.wa.example.com. CNAME  key3.wa.example.com._domainkey.migadu.com.

_dmarc.wa.example.com.         TXT    "v=DMARC1; p=reject; rua=mailto:you@example.com"
```

`-all` and `p=reject`, not `~all` and `p=none`. This subdomain sends to exactly
one recipient and should never be spoofable; there is no legitimate mail it
could break.

Three DKIM selectors exist so the provider can rotate signing keys without an
outage. Publish all three.

Then, in the provider's admin: create one mailbox (`bridge@wa.example.com`) and
set the domain to **catch-all** to it. That single mailbox receives mail for
every conversation address, and the bridge routes by the recipient it was sent
to.

## Doing it with the Cloudflare API

If the zone is on Cloudflare, [`cloudflare-dns.sh`](cloudflare-dns.sh) creates
all eight records in one run. Give it a token scoped to **Zone:DNS:Edit on that
zone only**, and run it somewhere you control -- the bridge's own box is fine.
Re-running updates rather than duplicates.

## Verifying

```sh
dig +short MX    wa.example.com
dig +short TXT   wa.example.com
dig +short TXT   _dmarc.wa.example.com
dig +short CNAME key1._domainkey.wa.example.com
```

Propagation is usually minutes, occasionally an hour. The provider will not
enable the domain until it sees the verification record.

## Then

Fill the mail settings in `/etc/wa-bridge/env` and send one message from the
assistant to the catch-all. Its headers are what pins `SEC-14`: the exact `From`
address, the DKIM `d=` domain, and the envelope domain. Until a real message has
been seen, the sender allowlist stays empty and the bridge accepts nothing --
which is the intended default, not a misconfiguration.
