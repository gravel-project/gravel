# The hub

One Go binary, `gravel-hub`, beside Postgres. This page is the operator reference: configuration,
endpoints, the owner claim, migrations, running it, observability and tests. ADR-0001 is the why;
`deploy/README.md` is the container how-to.

## Commands

| Command | Does |
|---|---|
| `gravel-hub serve` (default) | Serve the API. Stops cleanly on SIGINT/SIGTERM, draining for `server.shutdown_timeout`. |
| `gravel-hub migrate` | Apply pending migrations, then print the schema version. |
| `gravel-hub migrate --status` | Print the schema version and pending count; exit 3 when migrations are pending. |
| `gravel-hub config check` | Load and validate the configuration, print a summary (never a secret). |
| `gravel-hub healthcheck [--url …]` | GET `/healthz` and exit 0 when it answers 200. The container healthcheck. |
| `gravel-hub version` | Print the build version. |

The config path is `--config`, else `$GRAVEL_CONFIG`, else `/etc/gravel/hub.yaml`.

## Configuration

One YAML file, strict (an unknown key is an error) and versioned. Every key except the first three
has a default.

```yaml
version: 1                       # required; this build reads version 1
organization:
  name: Hidden Token Gaming      # required; the built-in organization's display name
database:
  url_file: /run/secrets/gravel-db-url   # a podman secret holding postgres://user:pw@host:5432/db
  # url: postgres://…            # development only; never commit a password
  migrate: auto                  # auto: apply pending migrations at start · manual: refuse to serve until `gravel-hub migrate` ran
  max_conns: 8
  connect_timeout: 60s           # how long to wait for Postgres to accept connections at start
server:
  listen: 127.0.0.1:8080         # API, /healthz, /readyz; put TLS termination (cloudflared) in front
  internal_listen: 127.0.0.1:9090 # /metrics and /debug/pprof/; keep it off the public network
  shutdown_timeout: 15s
log:
  level: info                    # debug · info · warn · error
  format: json                   # json · text
claim:
  token_ttl: 15m                 # how long an owner-claim token lives
```

**Secrets.** The database URL is the only secret. Precedence: the `GRAVEL_DATABASE_URL` environment
variable, then `database.url_file`, then `database.url`. Production uses a podman secret
(`Secret=gravel-db-url` in the quadlet unit, which lands at `/run/secrets/gravel-db-url`). The hub
logs the URL with the password redacted and says which source it came from.

## Endpoints

Public listener (`server.listen`), HTTP/1.1 and unencrypted HTTP/2 so gRPC works behind TLS termination:

| Path | What |
|---|---|
| `/gravel.hub.v1.OrganizationService/GetOrganization` | Connect procedure; POST JSON `{}` or call it over gRPC / gRPC-Web |
| `/gravel.hub.v1.OrganizationService/ClaimOwnership` | Connect procedure; `{"token": "…"}` |
| `/grpc.health.v1.Health/Check` | gRPC health |
| `/grpc.reflection.v1.ServerReflection/…` (and v1alpha) | gRPC reflection, so `grpcurl` and `buf curl` discover the API |
| `GET /healthz` | 200 `{"status":"ok","version":"…"}` while the process runs |
| `GET /readyz` | 200 `{"status":"ready"}` when Postgres answers and no migration is pending, else 503 with a reason |

Internal listener (`server.internal_listen`): `GET /metrics` (Prometheus) and `GET /debug/pprof/`.

The protobuf source is `proto/gravel/hub/v1/`; the generated Go is `gen/`; the OpenAPI document
for the JSON side is `gen/openapi/hub.openapi.yaml`. `make generate` rebuilds them.

```sh
# JSON
curl -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8080/gravel.hub.v1.OrganizationService/GetOrganization
# gRPC, via reflection
grpcurl -plaintext 127.0.0.1:8080 gravel.hub.v1.OrganizationService/GetOrganization
```

## Owner claim (ADR-0002)

A fresh hub is unowned. On each start while unowned it mints a one-time owner-claim token and logs
it once at WARN level:

```json
{"level":"WARN","msg":"the hub is unowned: claim it once with this token (it expires; a restart mints a new one)","token":"…","expires_at":"…"}
```

Claim it within `claim.token_ttl`:

```sh
curl -X POST -H 'Content-Type: application/json' -d '{"token":"<token>"}' http://127.0.0.1:8080/gravel.hub.v1.OrganizationService/ClaimOwnership
```

The claim is single-use and atomic. A second call answers `failed_precondition` (the hub is
owned), a wrong or expired token `permission_denied`, an empty token `invalid_argument`. An owned
hub never mints a token again. Until the identity work (#13) lands, the claim records `claimed_at`
and leaves the owner's user id empty.

## Migrations

SQL migrations live in `internal/store/migrations/` and are embedded in the binary (goose). With
`database.migrate: auto` the hub applies pending ones at start; with `manual` it refuses to start
until `gravel-hub migrate` has run, and `/readyz` reports pending migrations either way. Every
migration has a `-- +goose Down` section so tests can reset a database.

## Running it

- **Development:** `make up` builds the image with ko, writes dev secrets into podman's secret
  store and starts hub + Postgres 17 with `podman compose` (`deploy/compose.yaml`). Host ports
  come from `GRAVEL_PORT` and `GRAVEL_INTERNAL_PORT` (8080 and 9090 by default). `make down` stops
  it and keeps the Postgres volume.
- **Production:** the quadlet units in `deploy/quadlet/` as rootless systemd user units; see
  `deploy/README.md`.

The image is built by ko from `cmd/gravel-hub` on `gcr.io/distroless/static-debian12:nonroot`,
runs as uid 65532, and has the binary at `/ko-app/gravel-hub` (`.ko.yaml`). Releases push it to
`ghcr.io/gravel-project/gravel-hub` as `<version>` (no `v`: `0.1.0`) and `latest`, multi-arch
(amd64, arm64), signed with cosign and with an SBOM attached; `docs/releasing.md` has the verify
command and the digest-pin procedure.

## Observability

- **Logs:** slog, JSON by default, one `request` line per HTTP request with method, path, status,
  bytes, duration, remote and `request_id`. Health probes log at debug unless they fail.
- **Request ids:** `X-Request-ID` is honoured when well-formed (up to 128 of `A-Za-z0-9._:-`),
  otherwise a UUIDv7 is generated; it is echoed on the response and attached to every log line
  and internal-error log.
- **Metrics:** `gravel_http_requests_total{handler,method,code}`,
  `gravel_http_request_duration_seconds{handler}`, `gravel_rpc_requests_total{procedure,code}`
  (`code` is the Connect code, `ok` on success), `gravel_rpc_request_duration_seconds{procedure}`,
  `gravel_build_info{version,go_version}`, plus the Go and process collectors.
- **Panics** become a 500 and an error log line with the stack; the server stays up.

## Tests

`make test` runs every package with the race detector. Packages that need Postgres (`store`,
`hub`) skip unless `GRAVEL_TEST_DATABASE_URL` points at a server the user may create databases on;
each test binary then drops and recreates its own database (`gravel_test_<package>`), so packages
run in parallel. `make test-integration` insists on the variable. Locally:

```sh
podman run -d --name gravel-test-pg -p 127.0.0.1:55432:5432 -e POSTGRES_USER=gravel -e POSTGRES_PASSWORD=test -e POSTGRES_DB=gravel docker.io/library/postgres:17
GRAVEL_TEST_DATABASE_URL='postgres://gravel:test@127.0.0.1:55432/gravel?sslmode=disable' make test-integration
```

## Tooling

`make tools` installs the pinned CLIs (buf, the protoc plugins, golangci-lint, govulncheck, ko)
into `.bin/`; the versions live at the top of the Makefile and the CI workflow pins the same
golangci-lint. `make check` runs what CI runs, except the integration job. `make lint` also enforces
the `games/` boundary: packages under `games/` must not import gravel (depguard), because they
become standalone modules at the open-source cut.
