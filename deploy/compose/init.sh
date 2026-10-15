#!/bin/sh
# Prepare a Compose box for the hub (gravel#210, deploy/README.md): ./init.sh [--tunnel] <domain>
#
# Writes local/hub.yaml from hub.yaml (this domain, the ingress's client address header), the
# secrets under local/secrets/ (a database password and URL, the stats pseudonym key, the backup
# job's pgpass, Grafana's admin password, and empty files for the ones you fill in: backup
# credentials, the tunnel token, the bot's two) and .env (the domain and the profiles). It never
# overwrites a secret that exists: run it again to change the domain or the ingress.
set -eu
cd "$(dirname "$0")"
tunnel=false
if [ "${1:-}" = "--tunnel" ]; then tunnel=true; shift; fi
domain=${1:-}
case "$domain" in
"" | *[!A-Za-z0-9.-]* | .* | *.)
	echo "usage: $0 [--tunnel] <domain>   (the name members open, like hub.example.com)" >&2
	exit 2
	;;
esac
# The secrets' directory is this user's alone; the files in it are readable, because Compose
# outside Swarm mounts each with its own mode and the containers run as their own users.
umask 022
mkdir -p local/secrets
chmod 700 local local/secrets

# secret NAME VALUE: write it once.
secret() {
	[ -s "local/secrets/$1" ] || printf '%s' "$2" >"local/secrets/$1"
}
random() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
secret db-password "$(random)"
pw=$(cat local/secrets/db-password)
secret db-url "postgres://gravel:$pw@postgres:5432/gravel?sslmode=disable"
secret backup-pgpass "postgres:5432:*:gravel:$pw"
secret stats-pseudonym-key "$(random)"
secret grafana-admin-password "$(random)"
for f in backup-credentials tunnel-token bot-discord-token bot-hub-client-secret; do
	[ -e "local/secrets/$f" ] || : >"local/secrets/$f"
done

header=X-Real-IP
profiles=https
if $tunnel; then
	header=CF-Connecting-IP
	profiles=tunnel
fi
sed -e "s|^  base_url: https://hub.example.com|  base_url: https://$domain|" \
	-e "s|^  client_ip_header: X-Real-IP|  client_ip_header: $header|" hub.yaml >local/hub.yaml.new
mv local/hub.yaml.new local/hub.yaml
[ -e local/bot.yaml ] || : >local/bot.yaml
# With SELinux on (Fedora, RHEL), a container may read only files labelled for containers, and
# Compose has no relabel option for a secret: label local/ here.
if command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled; then
	chcon -R -t container_file_t local
fi

# .env: the domain and the profiles; keep any other line already there (the backups' settings).
touch .env
grep -v -e '^GRAVEL_DOMAIN=' -e '^COMPOSE_PROFILES=' .env >.env.new || true
{
	echo "GRAVEL_DOMAIN=$domain"
	echo "COMPOSE_PROFILES=$profiles"
	cat .env.new
} >.env
rm -f .env.new

echo "Ready for https://$domain ($profiles). Next: docker compose up -d, then make the owner (deploy/README.md)."
