#!/usr/bin/env bash
# The hub's base backup, run by gravel-backup.timer in a container of this image that shares the
# data volume and the WAL-G settings with Postgres: a full base backup, then retention, then a
# status file the hub reads for its metrics (docs/hub.md "Backups"). Exit status is the backup's.
set -euo pipefail
data="${PGDATA:-/var/lib/postgresql/data}"
status="${GRAVEL_BACKUP_STATUS:-/var/lib/gravel/backup/status.json}"
retain="${GRAVEL_BACKUP_RETAIN_FULL:-30}"
started="$(date -u +%FT%TZ)"
mkdir -p "$(dirname "$status")"

write_status() { # ok|error, message, backup name
  python3 - "$1" "$2" "$3" "$started" "$status" <<'PY' 2>/dev/null || printf '{"ok":%s,"started_at":"%s","finished_at":"%s","error":"%s","backup":"%s"}\n' "$([ "$1" = ok ] && echo true || echo false)" "$started" "$(date -u +%FT%TZ)" "$2" "$3" > "$status.tmp"
import json, sys, datetime
ok, msg, name, started, path = sys.argv[1] == "ok", sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
json.dump({"ok": ok, "started_at": started, "finished_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "error": "" if ok else msg, "backup": name}, open(path + ".tmp", "w"))
PY
  mv -f "$status.tmp" "$status"
}

if ! out="$(wal-g backup-push "$data" 2>&1)"; then
  echo "$out" >&2
  write_status error "backup-push failed" ""
  exit 1
fi
echo "$out"
name="$(wal-g backup-list 2>/dev/null | awk 'NR>1 {n=$1} END {print n}')"
if ! wal-g delete retain FULL "$retain" --confirm 2>&1; then
  echo "retention failed; the backup itself succeeded" >&2
fi
write_status ok "" "${name:-unknown}"
echo "backup ${name:-unknown} done; status at $status"
