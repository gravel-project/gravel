# Deploying the hub with podman

Two ways to run the hub, both rootless podman on Fedora (the quadlets run anywhere with systemd and
podman 4.5 or later):

| | Development | Production |
|---|---|---|
| Files | `compose.yaml`, `hub.yaml` | `quadlet/*` |
| Start | `make up` | `systemctl --user start gravel-hub` |
| Secrets | `dev-secrets.sh` generates them into podman's secret store | `podman secret create` from your own files |
| Image | `localhost/gravel-hub:dev`, built by `make image` | `ghcr.io/gravel-project/gravel-hub`, pinned by digest |

Secrets are never bind-mounted: podman copies each secret into the container's tmpfs, so SELinux
labels and file ownership on the host don't matter and nothing readable is left in a working tree.

## Development: `make up`

```sh
make up                                  # image, secrets, compose up, waits for /readyz
podman compose -f deploy/compose.yaml logs -f hub
make down                                # keeps the gravel-postgres volume
GRAVEL_PORT=18080 GRAVEL_INTERNAL_PORT=19090 make up   # when 8080/9090 are taken
```

`deploy/secrets/` (gitignored) keeps the generated Postgres password and database URL;
`dev-secrets.sh` loads them as the podman secrets `gravel-db-password` and `gravel-db-url`, the
same names the quadlet units use. `hub.yaml` is the hub's configuration for the stack, mounted
read-only.

## Production: quadlet units

1. Build or pull the image. Until the first release: `make image` and set
   `Image=localhost/gravel-hub:dev` in `gravel-hub.container`; afterwards pin a digest of
   `ghcr.io/gravel-project/gravel-hub`.
2. Create the secrets from files you keep outside any repository:

   ```sh
   podman secret create gravel-db-password /path/to/db-password      # Postgres superuser password
   podman secret create gravel-db-url      /path/to/db-url           # postgres://gravel:<password>@gravel-postgres:5432/gravel?sslmode=disable
   podman secret create gravel-tunnel-token /path/to/tunnel-token    # only with gravel-cloudflared
   ```

3. Write `~/.config/gravel/hub.yaml` (reference: `docs/hub.md`; start from `deploy/hub.yaml` with
   `url_file: /run/secrets/gravel-db-url`).
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

`make quadlet-check` dry-runs the units with the quadlet generator (CI does too).

## Upgrading

Change `Image=` to the new digest, `systemctl --user daemon-reload`, then
`systemctl --user restart gravel-hub`. Migrations run at start (`database.migrate: auto`); with
`manual`, run `podman exec gravel-hub /ko-app/gravel-hub migrate --config /etc/gravel/hub.yaml`
first. Back up Postgres before a release that adds migrations.
