#!/bin/sh
# Written by the gravel hub (cloud/digitalocean). A DigitalOcean reserved IP reaches the instance on
# its anchor address, and traffic leaves from the reserved IP only when it goes out through the
# anchor gateway. Once the reserved IP is assigned, this makes the anchor gateway the default route
# with the anchor address as its source, so a game's replies, its registration with its publisher
# and its log push come from the address players connect to. Replies from the instance's own address
# (the hub's SSH and RCON) keep the public gateway through a rule of their own. It waits for the
# assignment, which the hub makes after the instance exists, and changes nothing until then.
set -u
md=http://169.254.169.254/metadata/v1
get() { curl -fsS --max-time 5 "$md/$1" 2>/dev/null; }
tries=0
until [ "$(get reserved_ip/ipv4/active)" = "true" ]; do
	tries=$((tries + 1))
	if [ "$tries" -ge "${GRAVEL_EGRESS_TRIES:-120}" ]; then
		echo "gravel-reserved-egress: no reserved IP is assigned; the routes are unchanged"
		exit 0
	fi
	sleep 5
done
anchor=$(get interfaces/public/0/anchor_ipv4/address) &&
	anchor_gw=$(get interfaces/public/0/anchor_ipv4/gateway) &&
	public=$(get interfaces/public/0/ipv4/address) &&
	public_gw=$(get interfaces/public/0/ipv4/gateway) || {
	echo "gravel-reserved-egress: the metadata service did not answer" >&2
	exit 1
}
dev=$(ip -o -4 addr show | awk -v a="$public/" 'index($4, a) == 1 { print $2; exit }')
if [ -z "$dev" ]; then
	echo "gravel-reserved-egress: no interface holds $public" >&2
	exit 1
fi
ip rule del priority 999 2>/dev/null || true
ip rule add from "$public" lookup 201 priority 999
ip route replace default via "$public_gw" dev "$dev" table 201
ip route replace default via "$anchor_gw" dev "$dev" src "$anchor" metric 100
echo "gravel-reserved-egress: outbound traffic leaves from the reserved IP (anchor $anchor via $anchor_gw); replies from $public stay on $public_gw"
