#!/usr/bin/env bash
# Restore the hub's database from object storage into an EMPTY data directory, to a wall-clock
# point, then exit; start Postgres on that directory afterwards and it replays WAL to the point
# (docs/hub.md "Restore"). Run in a container of this image with the WAL-G settings and the
# empty data volume mounted; never against a directory Postgres is using.
#
#   gravel-restore --to 2026-10-09T02:00:00Z     # a point in time (UTC, RFC 3339)
#   gravel-restore --latest                      # everything the archive has
#   gravel-restore --backup base_000000010000000000000002 --to …   # a named base backup
set -euo pipefail
data="${PGDATA:-/var/lib/postgresql/data}"
backup=LATEST
target=""
while [ $# -gt 0 ]; do
  case "$1" in
    --to) target="$2"; shift 2 ;;
    --latest) target=""; shift ;;
    --backup) backup="$2"; shift 2 ;;
    *) echo "usage: gravel-restore (--to <RFC 3339>|--latest) [--backup <name>]" >&2; exit 2 ;;
  esac
done
# Postgres reads the target as a timestamptz but, at startup, not in the RFC 3339 shape with
# "T" and "Z"; "YYYY-MM-DD HH:MM:SS[.frac]+00" is the same instant and always accepted.
if [ -n "$target" ]; then
  case "$target" in
    *Z) target="${target%Z}+00" ;;
  esac
  target="${target/T/ }"
fi
if [ -n "$(ls -A "$data" 2>/dev/null)" ]; then
  echo "refusing to restore into a non-empty data directory: $data" >&2
  exit 1
fi
echo "fetching base backup $backup into $data"
wal-g backup-fetch "$data" "$backup"
# Recovery: Postgres pulls the WAL it needs from the archive and stops at the target.
{
  echo "restore_command = 'wal-g wal-fetch %f %p'"
  if [ -n "$target" ]; then
    echo "recovery_target_time = '$target'"
    echo "recovery_target_action = 'promote'"
    echo "recovery_target_inclusive = true"
  fi
} > "$data/postgresql.auto.conf.restore"
cat "$data/postgresql.auto.conf.restore" >> "$data/postgresql.auto.conf"
rm -f "$data/postgresql.auto.conf.restore"
touch "$data/recovery.signal"
chmod 0700 "$data"
echo "restored; start Postgres on this directory to replay WAL${target:+ to $target}"
