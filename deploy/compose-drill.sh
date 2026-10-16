#!/usr/bin/env bash
# The Compose tier's drill (gravel#210): what "docker compose up on a fresh box gives a working hub
# with a local owner" means, run end to end in a scratch copy of deploy/compose. init.sh for
# localhost (Caddy serves it from its own local CA), up with the https profile, the hub healthy
# behind Caddy, a local account made from the box, its password set through the one-time link,
# then gravel login --local and gravel claim with the token the hub logged: the owner is the
# account. CI's compose job runs it; make compose-drill runs it locally.
#
# Environment: COMPOSE (default "docker compose"), GRAVEL_CLI (the gravel binary; built when
# unset), GRAVEL_DRILL_HTTPS_PORT (default 8443, Caddy's published port here), KEEP=1 to leave the
# stack up.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose=${COMPOSE:-docker compose}
port=${GRAVEL_DRILL_HTTPS_PORT:-8443}
work="$(mktemp -d)"
project="gravel-drill-$$"
cli=${GRAVEL_CLI:-}
if [ -z "$cli" ]; then
	cli="$work/gravel"
	(cd "$root" && go build -o "$cli" ./cmd/gravel)
fi

cp -r "$root/deploy/compose" "$root/deploy/observability" "$work/"
mv "$work/compose" "$work/stack"
cd "$work/stack"
# The scratch copy's ../observability is the copy beside it.

dc() { $compose -p "$project" "$@"; }
cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		dc logs --no-color --tail 80 || true
	fi
	if [ "${KEEP:-}" != 1 ]; then
		dc down -v --remove-orphans >/dev/null 2>&1 || true
		rm -rf "$work"
	fi
	exit "$status"
}
trap cleanup EXIT

./init.sh localhost
# Caddy's host ports, moved off 80/443 for the drill, and no metrics port (the box running the
# drill may have its own on 9090): an override the scratch copy keeps.
cat >compose.override.yaml <<EOF
services:
  hub:
    ports: !reset []
  caddy:
    ports: !override
      - "127.0.0.1:$port:443"
EOF
dc up -d --wait --wait-timeout 300

base="https://localhost:$port"
# Caddy's local CA signed localhost's certificate: trust it for the drill only.
dc cp caddy:/data/caddy/pki/authorities/local/root.crt "$work/ca.crt"
for _ in $(seq 1 30); do
	if curl -fsS --cacert "$work/ca.crt" "$base/readyz" >/dev/null 2>&1; then break; fi
	sleep 2
done
curl -fsS --cacert "$work/ca.crt" "$base/readyz" >/dev/null
echo "the hub answers through Caddy at $base"

token=$(dc logs hub 2>/dev/null | sed -n 's/.*the hub is unowned.*"token":"\([^"]*\)".*/\1/p' | tail -1)
[ -n "$token" ] || { echo "the hub logged no claim token" >&2; exit 1; }

out=$(dc exec -T hub /ko-app/gravel-hub users create-local --username drillhost --config /etc/gravel/hub.yaml)
link=$(printf '%s\n' "$out" | grep -o '/auth/local/reset/[A-Za-z0-9_-]*')
[ -n "$link" ] || { echo "users create-local printed no link: $out" >&2; exit 1; }
password="correct horse battery staple"
landed=$(curl -fsS --cacert "$work/ca.crt" -c "$work/jar" -b "$work/jar" -o /dev/null -w '%{url_effective}' -L \
	--data-urlencode "password=$password" "$base$link")
case "$landed" in */account) ;; *) echo "setting the password landed on $landed" >&2; exit 1 ;; esac

export HOME="$work/home" SSL_CERT_FILE="$work/ca.crt"
mkdir -p "$HOME"
printf '%s\n' "$password" | "$cli" login --local --username drillhost --hub "$base" >/dev/null
"$cli" claim --hub "$base" -- "$token" # -- : a token hubs before 0.10.0 mint may start with '-'
"$cli" whoami --hub "$base" | grep -q owner || { echo "gravel whoami does not say owner" >&2; exit 1; }
echo "compose drill: a fresh stack, a local account, the hub claimed by it"
