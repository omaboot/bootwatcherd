#!/bin/bash
# Configures Cloudflare for status.boot.watch. Safe to re-run.
#
#   tunnel "bootwatcherd" → http://127.0.0.1:8471 on doiomad's server for status.boot.watch
#   (anything else 404s at the tunnel); DNS status.boot.watch → the tunnel, proxied; the tunnel
#   token to that server (/etc/bootwatcherd/tunnel-token, for --token-file), where only
#   cloudflared-bootwatcherd (re)starts. doiomad's tunnel is never touched.
#   boot.watch's redirect rule must leave status.boot.watch alone: bootwatchd's cloudflare.sh
#   owns that rule and exempts it.
#
# Environment:
#   CF_API_TOKEN    the "bootwatch setup" token: Account "Cloudflare Tunnel: Edit"; Zone
#                   (boot.watch): "Zone: Read", "DNS: Edit", "Zone Settings: Edit", "Single Redirect: Edit"
#   DROPLET, DO_CONTEXT, ADMIN_USER, ADMIN_KEY as in provision.sh (or deploy/local.env)
set -euo pipefail
local_env="$(dirname "$0")/local.env"
[ ! -f "$local_env" ] || . "$local_env"

ZONE_NAME=boot.watch
HOST=status.boot.watch
TUNNEL_NAME=bootwatcherd
ORIGIN=http://127.0.0.1:8471
: "${CF_API_TOKEN:?}"
API=https://api.cloudflare.com/client/v4

step() { echo "==> $*"; }
cf() {
	local out
	# the token goes to curl as a header file, never on its command line (ps would show it)
	out=$(curl -sS -X "$1" "$2" -H @<(printf 'Authorization: Bearer %s\n' "$CF_API_TOKEN") -H "Content-Type: application/json" ${3:+--data "$3"})
	if ! jq -e .success >/dev/null <<<"$out"; then
		echo "Cloudflare API: $1 $2 failed:" >&2
		jq -c '.errors' <<<"$out" >&2 || echo "$out" >&2
		return 1
	fi
	jq -c .result <<<"$out"
}

step "zone"
zone_json=$(cf GET "$API/zones?name=$ZONE_NAME" | jq -c '.[0] // empty')
[ -n "$zone_json" ] || { echo "zone $ZONE_NAME not found for this token" >&2; exit 1; }
zone=$(jq -r .id <<<"$zone_json")
ACCT=$API/accounts/$(jq -r .account.id <<<"$zone_json")

step "tunnel $TUNNEL_NAME"
tunnel=$(cf GET "$ACCT/cfd_tunnel?name=$TUNNEL_NAME&is_deleted=false" | jq -r '.[0].id // empty')
if [ -z "$tunnel" ]; then
	tunnel=$(cf POST "$ACCT/cfd_tunnel" "{\"name\":\"$TUNNEL_NAME\",\"config_src\":\"cloudflare\"}" | jq -r .id)
fi
echo "    $tunnel"
cf PUT "$ACCT/cfd_tunnel/$tunnel/configurations" "$(jq -nc --arg o "$ORIGIN" --arg h "$HOST" '{config: {ingress: [
	{hostname: $h, service: $o},
	{service: "http_status:404"}]}}')" >/dev/null

step "DNS"
target="$tunnel.cfargotunnel.com"
have=$(cf GET "$API/zones/$zone/dns_records?name=$HOST")
jq -r --arg t "$target" '.[] | select(.type == "A" or .type == "AAAA" or (.type == "CNAME" and .content != $t)) | .id' <<<"$have" |
	while read -r id; do cf DELETE "$API/zones/$zone/dns_records/$id" >/dev/null; done
if ! jq -e --arg t "$target" 'any(.[]; .type == "CNAME" and .content == $t and .proxied)' <<<"$have" >/dev/null; then
	cf POST "$API/zones/$zone/dns_records" "$(jq -nc --arg n "$HOST" --arg c "$target" \
		'{type: "CNAME", name: $n, content: $c, proxied: true, comment: "Cloudflare Tunnel bootwatcherd"}')" >/dev/null
fi
echo "    $HOST → tunnel"

step "tunnel token → doiomad's server (cloudflared-bootwatcherd only)"
token=$(cf GET "$ACCT/cfd_tunnel/$tunnel/token" | jq -r .)
name=${DROPLET:?set DROPLET in deploy/local.env}
ip=$(doctl --context "${DO_CONTEXT:?set DO_CONTEXT in deploy/local.env}" compute droplet list --format Name,PublicIPv4 --no-header | awk -v n="$name" '$1==n {print $2}')
[ -n "$ip" ] || { echo "droplet $name not found" >&2; exit 1; }
printf '%s' "$token" | ssh -o IdentitiesOnly=yes -i "${ADMIN_KEY:?set ADMIN_KEY in deploy/local.env}" "${ADMIN_USER:-$USER}@$ip" \
	"sudo install -d -o root -g root -m 0750 /etc/bootwatcherd && sudo install -o root -g root -m 0600 /dev/stdin /etc/bootwatcherd/tunnel-token &&
	 sudo systemctl enable -q cloudflared-bootwatcherd && sudo systemctl restart cloudflared-bootwatcherd"
echo "    cloudflared-bootwatcherd running on $name ($ip)"
