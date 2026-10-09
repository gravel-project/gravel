# Deploying the hub with podman

Two ways to run the hub, both rootless podman on Fedora (the quadlets run anywhere with systemd and
podman 4.5 or later):

| | Development | Production |
|---|---|---|
| Files | `compose.yaml`, `hub.yaml` | `quadlet/*` |
| Start | `make up` | `systemctl --user start gravel-hub` |
| Secrets | `dev-secrets.sh` generates them into podman's secret store | `podman secret create` from your own files |
| Image | `localhost/gravel-hub:dev` and `localhost/gravel-postgres:dev`, built by `make image` and `make postgres-image` | `ghcr.io/gravel-project/gravel-hub` and, with backups, `ghcr.io/gravel-project/gravel-postgres`, pinned by digest, signatures verified |
| Backups | always on, into a local S3 store (`store`); `make backup` takes a base backup; `make backup-drill` restores one | `deploy/quadlet/backup/`: drop-ins, a nightly timer, a real bucket (ADR-0006) |

Secrets are never bind-mounted: podman copies each secret into the container's tmpfs, so SELinux
labels and file ownership on the host don't matter and nothing readable is left in a working tree.

## Development: `make up`

```sh
make up                                  # image, secrets, compose up, waits for /readyz
podman compose -f deploy/compose.yaml logs -f hub
make down                                # keeps the gravel-postgres volume
GRAVEL_PORT=18080 GRAVEL_INTERNAL_PORT=19090 make up   # when 8080/9090 are taken
```

`deploy/secrets/` (gitignored) keeps the generated Postgres password and database URL, the
local store's key and the backup job's credentials file and pgpass line; `dev-secrets.sh` loads
them as the podman secrets `gravel-db-password`, `gravel-db-url`, `gravel-backup-credentials`
and `gravel-backup-pgpass`, the same names the quadlet units use. `deploy/store/` (gitignored)
is the local S3 store's data, one directory per bucket. `hub.yaml` is the hub's configuration
for the stack, mounted read-only.

Login is off in the stack's `hub.yaml`, so it runs with no Discord application. To try it:
create a Discord application with `http://127.0.0.1:8080/auth/discord/callback` as an OAuth2
redirect, then

```sh
printf '%s' '<client secret>' | podman secret create gravel-discord-client-secret -
printf '%s' '<steam web api key>' | podman secret create gravel-steam-api-key -   # optional
```

set `auth.discord.enabled: true` with your `client_id` (and `auth.steam.enabled: true`) in
`deploy/hub.yaml`, uncomment the two secrets in `compose.yaml`, and `make up`. The owner-claim
token the hub logs is pasted on the account page after the first login (`docs/hub.md`).

## Production: quadlet units

1. The unit pins a release of `ghcr.io/gravel-project/gravel-hub` by digest. Verify it before
   trusting it (`docs/releasing.md`): `cosign verify ghcr.io/gravel-project/gravel-hub@<digest>
   --certificate-oidc-issuer https://token.actions.githubusercontent.com
   --certificate-identity-regexp '^https://github.com/gravel-project/gravel/\.github/workflows/release\.yml@refs/tags/v'`.
   For a local build: `make image` and `Image=localhost/gravel-hub:dev`.
2. Create the secrets from files you keep outside any repository:

   ```sh
   podman secret create gravel-db-password /path/to/db-password      # Postgres superuser password
   podman secret create gravel-db-url      /path/to/db-url           # postgres://gravel:<password>@gravel-postgres:5432/gravel?sslmode=disable
   podman secret create gravel-tunnel-token /path/to/tunnel-token    # only with gravel-cloudflared
   podman secret create gravel-discord-client-secret /path/to/secret # with login: the Discord application's OAuth2 client secret
   podman secret create gravel-steam-api-key /path/to/key            # optional: persona names and avatars for linked Steam accounts
   ```

   With login enabled, uncomment the matching `Secret=` lines in `quadlet/gravel-hub.container`.

3. Write `~/.config/gravel/hub.yaml` (reference: `docs/hub.md`; start from `deploy/hub.yaml` with
   `url_file: /run/secrets/gravel-db-url`). For login, set `auth.base_url` to the origin members
   use (`https://…` makes the cookies `Secure`), enable the providers, register
   `<base_url>/auth/discord/callback` on the Discord application, and behind cloudflared set
   `server.client_ip_header: CF-Connecting-IP` so rate limits see members, not the tunnel. Make
   the file world-readable (`chmod 644`): the hub runs as uid 65532 inside a rootless container,
   which maps to one of your subordinate uids, so a 0600 file owned by you is "permission denied"
   from inside. The file holds no secrets by design.
4. Install and start:

   ```sh
   make quadlet-install                      # copies deploy/quadlet/* to ~/.config/containers/systemd/ and reloads
   loginctl enable-linger "$USER"            # so the units run without a login session
   systemctl --user start gravel-hub         # pulls in gravel-postgres and gravel.network
   systemctl --user enable gravel-hub gravel-postgres   # start at boot
   journalctl --user -u gravel-hub -f
   ```

`gravel-hub` publishes 8080 (API) and 9090 (metrics, pprof) on 127.0.0.1 only. For the public
side, `gravel-cloudflared.container` runs a Cloudflare Tunnel whose origin is
`http://gravel-hub:8080` on the private `gravel` network, so no inbound port is opened; it is
optional and wants the `gravel-tunnel-token` secret. Hidden Token Gaming's `deploy` repository
parameterises these units for earth and adds the Tunnel hostnames.

`make quadlet-check` dry-runs the units with the quadlet generator, with and without the backup
set (CI does too).

## Backups

Backups are opt-in per deployment (ADR-0006; `docs/hub.md` "Backups"). To turn them on:

1. A bucket on any S3-compatible store (Cloudflare R2, Backblaze B2, MinIO, …) and a key pair
   that may read, write and delete in it.
2. Two secrets: the credentials as an AWS shared credentials file, and a pgpass line for the
   backup job's connection:

   ```sh
   printf '[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n' "$KEY_ID" "$SECRET" | podman secret create gravel-backup-credentials -
   printf 'gravel-postgres:5432:gravel:gravel:%s\n' "$(cat /path/to/db-password)" | podman secret create gravel-backup-pgpass -
   ```

3. Edit the `WALG_S3_PREFIX` and `AWS_ENDPOINT` lines (the `Image=` lines pin the release's
   `gravel-postgres` digest, `docs/releasing.md`) in `deploy/quadlet/backup/gravel-postgres.container.d/10-backup.conf`
   and `deploy/quadlet/backup/gravel-backup.container` (`AWS_REGION=auto` is R2's; others name a
   region), and set `backup.status_file: /var/lib/gravel/backup/status.json` in `hub.yaml`.
4. `make quadlet-install-backup` (the drop-ins and the unit beside the quadlet files, the timer into
   `~/.config/systemd/user/`, where systemd reads plain units), then `systemctl --user restart gravel-postgres gravel-hub`
   (Postgres restarts once onto the new image with archiving on) and
   `systemctl --user enable --now gravel-backup.timer`. `systemctl --user start gravel-backup`
   takes the first base backup now; `journalctl --user -u gravel-backup` shows WAL-G's log and
   `/metrics` the `gravel_backup_*` and `gravel_wal_*` gauges.

Restore: `docs/hub.md` "Restore". Rehearse it with `make backup-drill` before you need it.

Health checks on the hub's image must be the exec form (`HealthCmd=["CMD", …]`): the string form
runs through `/bin/sh -c`, and distroless has no shell, so it reports unhealthy while the hub is fine.

## Dashboards

gravel ships Prometheus and Grafana with a dashboard each for the hub and the bot (ADR-0009). All
of it lives in `observability/`: the scrape configs, Grafana's provisioning, and the dashboards,
which are generated from `internal/tools/dashboards` by `make generate` and read-only in Grafana.

- **Development:** `make up-observability` is `make up` plus the `observability` profile:
  Grafana on <http://127.0.0.1:3000> (admin; the password is in `secrets/grafana-admin-password`)
  and Prometheus on 127.0.0.1:9092 (`GRAVEL_GRAFANA_PORT` and `GRAVEL_PROMETHEUS_PORT` move them).
  Add `--profile bot` for the bot's targets.
- **Production:** `quadlet/observability/` (its README): create `gravel-grafana-admin-password`,
  then `make quadlet-install-observability`, which also installs the configuration and the
  dashboards under `~/.config/gravel/`.
- **Check them:** `go run ./internal/tools/dashboards check --grafana <url> --password-file <file>`
  asks Grafana for every panel's data and fails on an empty panel that isn't marked optional
  (each optional panel says why it may be empty). `make observability-check` does it against a
  throwaway hub in a pod; CI runs it on every PR.

Neither Prometheus's data nor Grafana's state is backed up: the first is 30 days of history, the
second only the admin user, since everything shown is provisioned.

## Upgrading

Change `Image=` to the new digest, `systemctl --user daemon-reload`, then
`systemctl --user restart gravel-hub`. Migrations run at start (`database.migrate: auto`); with
`manual`, run `podman exec gravel-hub /ko-app/gravel-hub migrate --config /etc/gravel/hub.yaml`
first. Back up Postgres before a release that adds migrations. With the observability set,
reinstall it from the new release (`make quadlet-install-observability`) so the dashboards match
the metrics.
