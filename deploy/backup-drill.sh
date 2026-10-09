#!/usr/bin/env bash
# The restore drill (ADR-0006; `make backup-drill`, CI's backup job): a fresh Postgres from the
# hub's image archives WAL to a local S3 store; rows go in, a base backup is taken, more rows go
# in, a point in time is noted, more rows go in; then the database is stopped and restored to
# that point into a fresh volume, and the restore is checked (exactly the rows before the point,
# the hub's schema current) and timed. Needs podman, the image (IMAGE) and the hub binary (HUB).
set -euo pipefail
IMAGE="${IMAGE:-localhost/gravel-postgres:dev}"
STORE_IMAGE="${STORE_IMAGE:-ghcr.io/versity/versitygw:v1.8.0}"
HUB="${HUB:-.bin/gravel-hub}"
name="drill-$$"
net="$name-net"
work="$(mktemp -d)"
cleanup() {
  podman rm -f "$name-pg" "$name-pg2" "$name-store" >/dev/null 2>&1 || true
  podman volume rm -f "$name-data" "$name-restore" >/dev/null 2>&1 || true
  podman secret rm "$name-credentials" "$name-pgpass" >/dev/null 2>&1 || true
  podman network rm -f "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
say() { printf '\n== %s\n' "$*"; }

say "store"
mkdir -p "$work/store/gravel" && chmod -R 0777 "$work/store"
podman network create "$net" >/dev/null
podman run -d --name "$name-store" --network "$net" -v "$work/store:/data:Z" \
  -e ROOT_ACCESS_KEY=drill -e ROOT_SECRET_KEY=drillsecret "$STORE_IMAGE" posix /data >/dev/null
printf '[default]\naws_access_key_id = drill\naws_secret_access_key = drillsecret\n' | podman secret create "$name-credentials" - >/dev/null
printf '%s:5432:gravel:gravel:drill\n' "$name-pg" | podman secret create "$name-pgpass" - >/dev/null

pg_env=(-e POSTGRES_USER=gravel -e POSTGRES_DB=gravel -e POSTGRES_PASSWORD=drill
  -e WALG_S3_PREFIX="s3://gravel/drill" -e AWS_ENDPOINT="http://$name-store:7070" -e AWS_REGION=us-east-1
  -e AWS_S3_FORCE_PATH_STYLE=true -e AWS_SHARED_CREDENTIALS_FILE="/run/secrets/$name-credentials"
  -e PGHOST="$name-pg" -e PGUSER=gravel -e PGDATABASE=gravel -e PGPASSFILE="/run/secrets/$name-pgpass")
archive=(postgres -c archive_mode=on -c "archive_command=wal-g wal-push %p" -c archive_timeout=5)

start_pg() { # name volume
  podman run -d --name "$1" --network "$net" -p 127.0.0.1::5432 "${pg_env[@]}" --secret "$name-credentials" --secret "$name-pgpass,mode=0400" \
    -v "$2:/var/lib/postgresql/data" "$IMAGE" "${archive[@]}" >/dev/null
  # Readiness over the socket: the container's PGHOST names the first instance, which the
  # restored one must not depend on.
  for _ in $(seq 1 60); do podman exec -e PGHOST=/var/run/postgresql "$1" pg_isready -U gravel -d gravel >/dev/null 2>&1 && return 0; sleep 1; done
  echo "$1 did not become ready" >&2; podman logs "$1" | tail -20 >&2; return 1
}
# The drill's own queries go over the container's socket (trust auth); the backup job connects
# over TCP with the pgpass secret, as it does in production.
psql() { podman exec -e PGHOST=/var/run/postgresql "$1" psql -U gravel -d gravel -tAq -c "$2"; }
hub_url() { echo "postgres://gravel:drill@127.0.0.1:$(podman port "$1" 5432 | head -1 | sed 's/.*://')/gravel?sslmode=disable"; }

say "postgres, archiving"
start_pg "$name-pg" "$name-data"
cat > "$work/hub.yaml" <<YAML
version: 1
organization:
  name: Drill
YAML
GRAVEL_DATABASE_URL="$(hub_url "$name-pg")" "$HUB" migrate --config "$work/hub.yaml" >/dev/null
psql "$name-pg" "CREATE TABLE drill (n int PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())"
psql "$name-pg" "INSERT INTO drill (n) SELECT generate_series(1, 100)"

say "base backup"
podman exec "$name-pg" gravel-backup
psql "$name-pg" "INSERT INTO drill (n) SELECT generate_series(101, 200)"
sleep 2
target="$(psql "$name-pg" "SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')")"
echo "point in time: $target (200 rows)"
sleep 2
psql "$name-pg" "INSERT INTO drill (n) SELECT generate_series(201, 300)"
psql "$name-pg" "SELECT pg_switch_wal()" >/dev/null
for _ in $(seq 1 30); do
  [ "$(psql "$name-pg" "SELECT archived_count FROM pg_stat_archiver")" -ge 2 ] && break; sleep 1
done
archived="$(psql "$name-pg" "SELECT archived_count FROM pg_stat_archiver")"
failed="$(psql "$name-pg" "SELECT failed_count FROM pg_stat_archiver")"
echo "archived $archived segments, $failed failures"
[ "$failed" = 0 ] || { echo "WAL archiving failed" >&2; exit 1; }
[ "$archived" -ge 2 ] || { echo "too few segments archived" >&2; exit 1; }

say "the disk dies"
podman rm -f "$name-pg" >/dev/null

say "restore to $target"
t0="$(date +%s)"
podman run --rm --network "$net" "${pg_env[@]}" --secret "$name-credentials" --secret "$name-pgpass,mode=0400" \
  -v "$name-restore:/var/lib/postgresql/data" "$IMAGE" gravel-restore --to "$target"
start_pg "$name-pg2" "$name-restore"
for _ in $(seq 1 60); do
  [ "$(psql "$name-pg2" "SELECT pg_is_in_recovery()")" = f ] && break; sleep 1
done
t1="$(date +%s)"
rows="$(psql "$name-pg2" "SELECT count(*) FROM drill")"
last="$(psql "$name-pg2" "SELECT max(n) FROM drill")"
echo "restored in $((t1 - t0)) s: $rows rows, last n = $last"
[ "$rows" = 200 ] && [ "$last" = 200 ] || { echo "expected exactly the 200 rows before the point" >&2; exit 1; }
GRAVEL_DATABASE_URL="$(hub_url "$name-pg2")" "$HUB" migrate --status --config "$work/hub.yaml"
say "drill passed: restore to a point in time in $((t1 - t0)) s"
