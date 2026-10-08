#!/usr/bin/env bash
# Create the development secrets for the compose stack: a random Postgres password and the
# matching database URL, kept in a gitignored directory and loaded into podman's secret store as
# gravel-db-password and gravel-db-url (the names the quadlet units use too). Idempotent.
set -euo pipefail
dir="${1:?usage: dev-secrets.sh <dir>}"
umask 077
mkdir -p "$dir"
if [[ ! -s "$dir/db-password" ]]; then
  head -c 24 /dev/urandom | base64 | tr -d '/+=\n' > "$dir/db-password"
  echo "wrote $dir/db-password"
fi
pw="$(cat "$dir/db-password")"
url="postgres://gravel:${pw}@postgres:5432/gravel?sslmode=disable"
if [[ ! -s "$dir/db-url" || "$(cat "$dir/db-url")" != "$url" ]]; then
  printf '%s\n' "$url" > "$dir/db-url"
  echo "wrote $dir/db-url"
fi
podman secret create --replace gravel-db-password "$dir/db-password" >/dev/null
podman secret create --replace gravel-db-url "$dir/db-url" >/dev/null
echo "podman secrets gravel-db-password and gravel-db-url are current"
