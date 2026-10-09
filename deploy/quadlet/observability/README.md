# Dashboards as quadlet units

Install this directory beside the base units (`make quadlet-install-observability`) for gravel's
Prometheus and Grafana (ADR-0009, `deploy/README.md` "Dashboards"). Prometheus scrapes the hub
and the bot on `gravel.network`; Grafana shows the `gravel hub` and `gravel bot` dashboards on
<http://127.0.0.1:3000>. Both listen on loopback only.

Before installing, create the admin password secret (the target installs the configuration):

```sh
head -c 24 /dev/urandom | base64 | tr -d '/+=\n' | podman secret create gravel-grafana-admin-password -
```

Then `systemctl --user start gravel-prometheus gravel-grafana` and log in as `admin`. Check every
panel from a clone at the same release:

```sh
go run ./internal/tools/dashboards check --grafana http://127.0.0.1:3000 --password-file <file>
```

- **Upgrading gravel** moves the dashboards with it: reinstall this directory from the new
  release. The dashboards are read-only in the UI; a file removed here leaves Grafana within 30 s.
- **Changing the admin password:** Grafana reads `GF_SECURITY_ADMIN_PASSWORD__FILE` only when it
  creates the admin user, so replace the secret and reset the user from it:

  ```sh
  podman exec gravel-grafana sh -c 'grafana cli admin reset-admin-password --password-from-stdin < /run/secrets/gravel-grafana-admin-password'
  ```

- **Data:** neither volume is backed up. Prometheus keeps 30 days or 2 GB; Grafana's state is
  only its admin user and sessions.
