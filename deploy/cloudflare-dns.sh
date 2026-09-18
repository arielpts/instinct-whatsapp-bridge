#!/usr/bin/env bash
# Create the bridge's DNS records in Cloudflare.
#
# Run it on a machine you control -- the VPS is fine. The token stays there.
#
#   bash cloudflare-dns.sh [env-file]
#
# Settings come from the environment, or from a KEY=value file given as the
# first argument:
#
#   CF_API_TOKEN    Zone:DNS:Edit on this zone only            (required)
#   CF_ACCOUNT_ID   required for account-owned tokens (cfat_)  (see below)
#   ZONE            the apex, as Cloudflare names the zone     (required)
#   SUB             the bridge subdomain label, e.g. wa        (required)
#   DMARC_RUA       an address for DMARC reports               (default: postmaster@ZONE)
#   MIGADU_VERIFY   hosted-email-verify from admin.migadu.com  (optional)
#
# Without MIGADU_VERIFY everything except the ownership record is still
# created, so DNS can propagate while the mailbox is being set up. Re-running
# is safe: a record with the same name and type is updated, not duplicated.

set -euo pipefail

# An env file may hold the token, so do not let it be world-readable.
if [ $# -ge 1 ]; then
	[ -r "$1" ] || { echo "cannot read $1" >&2; exit 1; }
	perms=$(stat -c %a "$1")
	case "$perms" in
	*[04]) ;;
	*) echo "warning: $1 is mode $perms; chmod 600 it -- it holds a token" >&2 ;;
	esac
	set -a
	# shellcheck disable=SC1090
	. "$1"
	set +a
fi

: "${CF_API_TOKEN:?set CF_API_TOKEN (or pass an env file holding it)}"
: "${ZONE:?set ZONE, e.g. example.com}"
: "${SUB:?set SUB, e.g. wa}"
DMARC_RUA="${DMARC_RUA:-postmaster@${ZONE}}"

FQDN="${SUB}.${ZONE}"
API="https://api.cloudflare.com/client/v4"

command -v jq >/dev/null || { apt-get update -qq && apt-get install -y -qq jq; }

cf() { curl -sS -H "Authorization: Bearer ${CF_API_TOKEN}" -H "Content-Type: application/json" "$@"; }

# Cloudflare has two kinds of token and they verify at different endpoints.
# An account-owned token (cfat_ prefix) is rejected by /user/tokens/verify --
# not because it is invalid, but because that endpoint is for user tokens.
echo "==> Verifying the token"
case "${CF_API_TOKEN}" in
cfat_*)
	if [ -z "${CF_ACCOUNT_ID:-}" ]; then
		echo "this is an account-owned token (cfat_); set CF_ACCOUNT_ID to the" >&2
		echo "account it belongs to -- it cannot be verified without one" >&2
		exit 1
	fi
	VERIFY_URL="${API}/accounts/${CF_ACCOUNT_ID}/tokens/verify"
	;;
*)
	VERIFY_URL="${API}/user/tokens/verify"
	;;
esac

VERIFY=$(cf "${VERIFY_URL}")
if ! echo "$VERIFY" | jq -e '.success' >/dev/null; then
	echo "token rejected by Cloudflare:" >&2
	echo "$VERIFY" | jq -r '.errors[]? | "  \(.code): \(.message)"' >&2
	exit 1
fi
echo "    ok"

echo "==> Finding zone ${ZONE}"
ZONES_URL="${API}/zones?name=${ZONE}"
[ -n "${CF_ACCOUNT_ID:-}" ] && ZONES_URL="${ZONES_URL}&account.id=${CF_ACCOUNT_ID}"
ZONES=$(cf "$ZONES_URL")
ZONE_ID=$(echo "$ZONES" | jq -r '.result[0].id // empty')
if [ -z "$ZONE_ID" ]; then
	echo "no zone named ${ZONE} visible to this token" >&2
	echo "$ZONES" | jq -r '.errors[]? | "  \(.code): \(.message)"' >&2
	echo "  (the token needs Zone:Read as well as Zone:DNS:Edit)" >&2
	exit 1
fi
echo "    ${ZONE_ID}"

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
if [ -n "${MIGADU_VERIFY:-}" ]; then
	upsert TXT "${FQDN}" "hosted-email-verify=${MIGADU_VERIFY}"
else
	echo "    skipping ownership record: MIGADU_VERIFY is not set."
	echo "    Add the domain at admin.migadu.com, then re-run with it."
fi
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
