#!/usr/bin/env bash
# Create the bridge's DNS records in Cloudflare.
#
# Run it on a machine you control -- the VPS is fine. The token stays there.
#
#   export CF_API_TOKEN=...        # Zone:DNS:Edit on this zone only
#   export ZONE=example.com        # the apex, as Cloudflare names the zone
#   export SUB=wa                  # the bridge subdomain label
#   export MIGADU_VERIFY=...       # hosted-email-verify value from admin.migadu.com
#   export DMARC_RUA=you@example.com
#   bash cloudflare-dns.sh
#
# Re-running is safe: an existing record with the same name and type is
# updated rather than duplicated.

set -euo pipefail

: "${CF_API_TOKEN:?set CF_API_TOKEN}"
: "${ZONE:?set ZONE, e.g. example.com}"
: "${SUB:?set SUB, e.g. wa}"
: "${MIGADU_VERIFY:?set MIGADU_VERIFY from admin.migadu.com}"
: "${DMARC_RUA:?set DMARC_RUA, an address for reports}"

FQDN="${SUB}.${ZONE}"
API="https://api.cloudflare.com/client/v4"

command -v jq >/dev/null || { apt-get update -qq && apt-get install -y -qq jq; }

cf() { curl -sS -H "Authorization: Bearer ${CF_API_TOKEN}" -H "Content-Type: application/json" "$@"; }

echo "==> Verifying the token"
cf "${API}/user/tokens/verify" | jq -e '.success' >/dev/null || {
	echo "token rejected by Cloudflare" >&2
	exit 1
}

echo "==> Finding zone ${ZONE}"
ZONE_ID=$(cf "${API}/zones?name=${ZONE}" | jq -r '.result[0].id // empty')
[ -n "$ZONE_ID" ] || { echo "no zone named ${ZONE} on this token" >&2; exit 1; }

# upsert <type> <name> <content> [priority]
upsert() {
	local type="$1" name="$2" content="$3" priority="${4:-}"
	local body existing id

	body=$(jq -nc --arg t "$type" --arg n "$name" --arg c "$content" \
		'{type:$t, name:$n, content:$c, ttl:300, proxied:false}')
	if [ -n "$priority" ]; then
		body=$(echo "$body" | jq -c --argjson p "$priority" '. + {priority:$p}')
	fi

	existing=$(cf "${API}/zones/${ZONE_ID}/dns_records?type=${type}&name=${name}")
	if [ -n "$priority" ]; then
		id=$(echo "$existing" | jq -r --arg c "$content" '.result[] | select(.content==$c) | .id' | head -1)
	else
		id=$(echo "$existing" | jq -r '.result[0].id // empty')
	fi

	if [ -n "$id" ]; then
		cf -X PUT "${API}/zones/${ZONE_ID}/dns_records/${id}" --data "$body" \
			| jq -e '.success' >/dev/null && echo "    updated ${type} ${name}"
	else
		cf -X POST "${API}/zones/${ZONE_ID}/dns_records" --data "$body" \
			| jq -e '.success' >/dev/null && echo "    created ${type} ${name}"
	fi
}

echo "==> Mail delivery"
upsert MX "${FQDN}" "aspmx1.migadu.com" 10
upsert MX "${FQDN}" "aspmx2.migadu.com" 20

echo "==> Ownership and sending policy"
upsert TXT "${FQDN}" "hosted-email-verify=${MIGADU_VERIFY}"
# -all, not ~all: this subdomain sends to exactly one recipient, so there is no
# legitimate mail a hard fail could break, and it should never be spoofable.
upsert TXT "${FQDN}" "v=spf1 include:spf.migadu.com -all"
upsert TXT "_dmarc.${FQDN}" "v=DMARC1; p=reject; rua=mailto:${DMARC_RUA}"

echo "==> DKIM (three selectors so the provider can rotate keys)"
for k in key1 key2 key3; do
	upsert CNAME "${k}._domainkey.${FQDN}" "${k}.${FQDN}._domainkey.migadu.com"
done

echo
echo "Done. Check propagation:"
echo "  dig +short MX ${FQDN}"
echo "  dig +short TXT ${FQDN}"
echo "  dig +short CNAME key1._domainkey.${FQDN}"
echo
echo "Then in admin.migadu.com: create bridge@${FQDN} and set the domain to"
echo "catch-all into it. That one mailbox receives every conversation address."
