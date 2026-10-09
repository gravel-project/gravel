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
# The backup store's root key pair (the local S3 store) and the AWS-style credentials file WAL-G
# reads it from, plus a pgpass line for the backup job's connection.
if [[ ! -s "$dir/store-secret" ]]; then
  head -c 24 /dev/urandom | base64 | tr -d '/+=\n' > "$dir/store-secret"
  echo "wrote $dir/store-secret"
fi
printf '[default]\naws_access_key_id = gravel\naws_secret_access_key = %s\n' "$(cat "$dir/store-secret")" > "$dir/backup-credentials"
printf 'postgres:5432:gravel:gravel:%s\n' "$pw" > "$dir/backup-pgpass"
mkdir -p "$(dirname "$dir")/store/gravel"   # the bucket: a directory for the posix store
podman secret create --replace gravel-db-password "$dir/db-password" >/dev/null
podman secret create --replace gravel-db-url "$dir/db-url" >/dev/null
podman secret create --replace gravel-backup-credentials "$dir/backup-credentials" >/dev/null
podman secret create --replace gravel-backup-pgpass "$dir/backup-pgpass" >/dev/null
echo "podman secrets gravel-db-password, gravel-db-url, gravel-backup-credentials and gravel-backup-pgpass are current (the store's key stays in $dir/store-secret)"
