# Backups as quadlet units

Install this directory beside the base units (`make quadlet-install-backup`) to turn backups on
(ADR-0006, `docs/hub.md` "Backups"): the drop-ins give Postgres and the hub their backup
settings, `gravel-backup.timer` takes a base backup every night (a plain systemd unit, installed into
`~/.config/systemd/user/`, since systemd never reads the quadlet directory itself), and
`gravel-backup.volume` holds the status file the hub reads. Before installing: edit the `WALG_S3_PREFIX` and
`AWS_ENDPOINT` lines in both drop-ins for your bucket (the `Image=` lines pin the release's
`gravel-postgres` digest, moved with each release per `docs/releasing.md`), and create the
credentials secret:

```sh
printf '[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n' "$KEY_ID" "$SECRET" | podman secret create gravel-backup-credentials -
```

The base `gravel-postgres.container` runs the official Postgres image; the drop-in switches it to
`ghcr.io/gravel-project/gravel-postgres` (Postgres 17 plus WAL-G), so the first start after
installing restarts Postgres once.
