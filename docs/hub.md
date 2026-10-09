# The hub

One Go binary, `gravel-hub`, beside Postgres. This page is the operator reference: configuration,
endpoints, login, the owner claim, migrations, running it, observability and tests. ADR-0001 is the
why, ADR-0004 the login design; `deploy/README.md` is the container how-to.

## Commands

| Command | Does |
|---|---|
| `gravel-hub serve` (default) | Serve the API. Stops cleanly on SIGINT/SIGTERM, draining for `server.shutdown_timeout`. |
| `gravel-hub migrate` | Apply pending migrations, then print the schema version. |
| `gravel-hub migrate --status` | Print the schema version and pending count; exit 3 when migrations are pending. |
| `gravel-hub config check` | Load and validate the configuration, print a summary with the login providers (never a secret). |
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
  listen: 127.0.0.1:8080         # API, pages, /healthz, /readyz; put TLS termination (cloudflared) in front
  internal_listen: 127.0.0.1:9090 # /metrics and /debug/pprof/; keep it off the public network
  shutdown_timeout: 15s
  client_ip_header: ""           # CF-Connecting-IP behind cloudflared; see Login below
log:
  level: info                    # debug · info · warn · error
  format: json                   # json · text
claim:
  token_ttl: 15m                 # how long an owner-claim token lives
auth:
  base_url: https://app.example.com   # the origin members use; required once a provider is enabled
  session_ttl: 720h              # a login lasts 30 days; no idle timeout
  attempt_ttl: 10m               # how long a member has to come back from a provider
  discord:
    enabled: true
    client_id: "1234567890"
    client_secret_file: /run/secrets/gravel-discord-client-secret   # or GRAVEL_DISCORD_CLIENT_SECRET
    login: true                  # Discord is the primary login
  steam:
    enabled: true
    api_key_file: /run/secrets/gravel-steam-api-key   # optional: persona name and avatar; or GRAVEL_STEAM_API_KEY
    login: false                 # link-only by default
rate_limit:
  per_ip:   {requests_per_minute: 120, burst: 40}    # anonymous requests, per client address
  per_user: {requests_per_minute: 600, burst: 100}   # authenticated requests, per user
```

**Secrets.** Three, each with the same precedence: the environment variable, then the `*_file`
key (a podman secret mount), then the inline key, which is for development only.

| Secret | Environment | File key | Inline key |
|---|---|---|---|
| Database URL | `GRAVEL_DATABASE_URL` | `database.url_file` | `database.url` |
| Discord OAuth2 client secret | `GRAVEL_DISCORD_CLIENT_SECRET` | `auth.discord.client_secret_file` | `auth.discord.client_secret` |
| Steam Web API key (optional) | `GRAVEL_STEAM_API_KEY` | `auth.steam.api_key_file` | `auth.steam.api_key` |

Production uses podman secrets (`Secret=gravel-db-url` in the quadlet unit lands at
`/run/secrets/gravel-db-url`, and so on). The hub logs the database URL with the password
redacted and, for every secret, which source it came from. There is no session key: sessions are
random tokens whose hashes live in Postgres (ADR-0004).

## Endpoints

Public listener (`server.listen`), HTTP/1.1 and unencrypted HTTP/2 so gRPC works behind TLS termination:

| Path | What |
|---|---|
| `/gravel.hub.v1.OrganizationService/GetOrganization` | Connect procedure; POST JSON `{}` or call it over gRPC / gRPC-Web |
| `/gravel.hub.v1.OrganizationService/ClaimOwnership` | Connect procedure; `{"token": "…"}`; needs a session, binds the caller as owner |
| `/gravel.hub.v1.IdentityService/GetMe` | the caller and their linked identities; needs a session |
| `/gravel.hub.v1.IdentityService/UnlinkIdentity` | `{"provider": "steam", "subject": "…"}`; the last identity is refused (`failed_precondition`) |
| `/gravel.hub.v1.IdentityService/Logout` | end the calling session and clear its cookie |
| `/gravel.hub.v1.IdentityService/RevokeSessions` | log out everywhere; answers how many sessions ended and clears the cookie |
| `/gravel.hub.v1.IdentityService/LookupUser` | `{"provider": "discord", "subject": "…"}` → the user and their identities, for role sync; the owner only until #6 |
| `/grpc.health.v1.Health/Check` | gRPC health |
| `/grpc.reflection.v1.ServerReflection/…` (and v1alpha) | gRPC reflection, so `grpcurl` and `buf curl` discover the API |
| `GET /healthz` | 200 `{"status":"ok","version":"…"}` while the process runs |
| `GET /readyz` | 200 `{"status":"ready"}` when Postgres answers and no migration is pending, else 503 with a reason |

Pages and flows, on the same listener (ADR-0005: the pages are templ components that call the
procedures above in process; htmx, served from the binary, swaps the main content on forms, and
every page works without it):

| Path | What |
|---|---|
| `GET /` | to `/account`, or `/login` when anonymous |
| `GET /login` | the login providers |
| `GET /account` | the member's identities, link buttons, unlink forms, "log out everywhere", and the claim form while the hub is unowned |
| `GET /auth/{provider}/start` | begin a login (providers with `login: true`) |
| `GET /auth/{provider}/link` | begin a link, logged in |
| `GET /auth/{provider}/callback` | the provider's return |
| `POST /auth/logout` | this session (`Logout`), or every session with `everywhere=1` (`RevokeSessions`) |
| `POST /account/unlink`, `POST /account/claim` | forms; every POST carries the session's CSRF token in `_csrf`; with `HX-Request: true` the answer is the page's content, else a redirect |
| `GET /theme.css` | the theme's tokens as custom properties, with an ETag |
| `GET /static/app.css`, `GET /static/vendor/htmx.min.js` | the stylesheet and htmx |

The pages send `Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self';
img-src https: data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors
'none'`: nothing inline, nothing from a third party. A host's look comes from a theme (tokens
for the light and dark schemes, the font, a logo, a favicon, navigation links in the header or
the footer, optionally owner-only); gravel#7 stores it on the Organization settings, and the
defaults apply until then. Editing a page means editing its `.templ` file under
`internal/web/templates/` and running `make generate`; the generated Go is committed.

Internal listener (`server.internal_listen`): `GET /metrics` (Prometheus) and `GET /debug/pprof/`.

The protobuf source is `proto/gravel/hub/v1/`; the generated Go is `gen/`; the OpenAPI document
for the JSON side is `gen/openapi/hub.openapi.yaml`. `make generate` rebuilds them.

```sh
# JSON
curl -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8080/gravel.hub.v1.OrganizationService/GetOrganization
# gRPC, via reflection
grpcurl -plaintext 127.0.0.1:8080 gravel.hub.v1.OrganizationService/GetOrganization
```

## Login (ADR-0004)

A member logs in with a provider whose `login` is true (Discord by default) and links the others
(Steam by default) from the account page. Every attempt is a row bound to the browser by an auth
cookie and to the provider's callback by a state value, consumed once: a replayed, forged or
expired callback is refused, and a link must finish in the session that started it. A
`(provider, subject)` pair belongs to one user; linking one another member holds is refused, and
so is unlinking the last one (a provider-only account would be unreachable).

- **Discord:** register `<auth.base_url>/auth/discord/callback` as a redirect in the Developer
  Portal's OAuth2 settings; the scope is `identify` alone (no email, no connections).
- **Steam:** nothing to register; the OpenID assertion proves the SteamID64. The Web API key only
  fetches the persona name and avatar, and may be left out.
- **Sessions:** server-side, `auth.session_ttl` long, no idle timeout. The cookie is `HttpOnly`,
  `SameSite=Lax`, and `Secure` with the `__Host-` prefix when `auth.base_url` is `https://`. A
  restart keeps everyone logged in; `RevokeSessions` or the page's "log out everywhere" ends every
  session of the member.
- **Cross-site requests:** `net/http`'s cross-origin protection refuses unsafe cross-origin
  browser requests before any handler, the API included; forms also carry the session's CSRF
  token. Clients that are not browsers send neither `Sec-Fetch-Site` nor `Origin` and pass.
- **Rate limits:** anonymous requests are limited per client address, authenticated ones per
  user (`rate_limit`); pages answer 429 with `Retry-After`, procedures `resource_exhausted`;
  `/healthz` and `/readyz` are never limited. Behind a reverse proxy that is the only way in, set
  `server.client_ip_header` to the header carrying the client's address (`CF-Connecting-IP` for
  cloudflared); otherwise everyone shares the proxy's bucket. Set it only when nothing else can
  reach the listener: the header is trusted as given.
- Expired sessions and attempts are pruned every ten minutes.

## Owner claim (ADR-0002, ADR-0004)

A fresh hub is unowned. On each start while unowned it mints a one-time owner-claim token and logs
it once at WARN level:

```json
{"level":"WARN","msg":"the hub is unowned: claim it once with this token (it expires; a restart mints a new one)","token":"…","expires_at":"…"}
```

Log in, then claim it within `claim.token_ttl` on the account page (the form shows while the hub
is unowned) or over the API with the session cookie:

```sh
curl -X POST -H 'Content-Type: application/json' -b '__Host-gravel_session=<cookie>' -d '{"token":"<token>"}' https://app.example.com/gravel.hub.v1.OrganizationService/ClaimOwnership
```

The caller becomes the owner (`owner_user_id`). The claim is single-use and atomic. A call without
a session answers `unauthenticated`, a second call `failed_precondition` (the hub is owned), a
wrong or expired token `permission_denied`, an empty token `invalid_argument`. An owned hub never
mints a token again. A claim made by 0.1.0 bound no user; migration 2 reopens it, so such a hub
logs a fresh token after the upgrade.

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
  `deploy/README.md`. The config file must be readable by uid 65532, the container's user (mode
  0644 is fine: it holds no secrets).

The image is built by ko from `cmd/gravel-hub` on `gcr.io/distroless/static-debian12:nonroot`,
runs as uid 65532, and has the binary at `/ko-app/gravel-hub` (`.ko.yaml`). Releases push it to
`ghcr.io/gravel-project/gravel-hub` as `<version>` (no `v`: `0.2.0`) and `latest`, multi-arch
(amd64, arm64), signed with cosign and with an SBOM attached; `docs/releasing.md` has the verify
command and the digest-pin procedure.

## Observability

- **Logs:** slog, JSON by default, one `request` line per HTTP request with method, path, status,
  bytes, duration, remote and `request_id`. Health probes log at debug unless they fail.
- **Request ids:** `X-Request-ID` is honoured when well-formed (up to 128 of `A-Za-z0-9._:-`),
  otherwise a UUIDv7 is generated; it is echoed on the response and attached to every log line
  and internal-error log.
- **Metrics:** `gravel_http_requests_total{handler,method,code}` (`handler` is `healthz`, `readyz`
  or `pages`), `gravel_http_request_duration_seconds{handler}`,
  `gravel_rpc_requests_total{procedure,code}` (`code` is the Connect code, `ok` on success),
  `gravel_rpc_request_duration_seconds{procedure}`,
  `gravel_auth_completions_total{provider,intent,result}` (`result` is `ok`, `denied`, `failed`,
  `invalid`, `mismatch`, `taken` or `error`), `gravel_rate_limited_total{scope}` (`ip` or `user`),
  `gravel_build_info{version,go_version}`, plus the Go and process collectors.
- **Secrets in logs:** the owner-claim token is the only secret the hub ever logs, once, at WARN.
  Provider secrets are logged by source only.
- **Panics** become a 500 and an error log line with the stack; the server stays up.

## Tests

`make test` runs every package with the race detector. The login flows are tested against an
in-memory store and a fake provider (`internal/identity/identitytest`); the Goth adapters against
fake Discord and Steam endpoints. Packages that need Postgres (`store`, `hub`) skip unless
`GRAVEL_TEST_DATABASE_URL` points at a server the user may create databases on;
each test binary then drops and recreates its own database (`gravel_test_<package>`), so packages
run in parallel. `make test-integration` insists on the variable. Locally:

```sh
podman run -d --name gravel-test-pg -p 127.0.0.1:55432:5432 -e POSTGRES_USER=gravel -e POSTGRES_PASSWORD=test -e POSTGRES_DB=gravel docker.io/library/postgres:17
GRAVEL_TEST_DATABASE_URL='postgres://gravel:test@127.0.0.1:55432/gravel?sslmode=disable' make test-integration
```

`make a11y` runs axe (`@axe-core/cli`, pinned in the Makefile, through npx) over the login page
and fixtures of the account and error pages, driving Chrome; CI runs it too. It needs node and
a browser, so it is not part of `make check`. axe bundles a ChromeDriver that must match the
Chrome it drives: CI installs a synced pair with `browser-driver-manager` and names them in
`AXE_CHROME_PATH` and `AXE_CHROMEDRIVER_PATH`; locally, leave both unset and keep Chrome
current, or set them the same way.

## Tooling

`make tools` installs the pinned CLIs (buf, the protoc plugins, golangci-lint, govulncheck, ko,
templ) into `.bin/`; the versions live at the top of the Makefile and the CI workflow pins the
same golangci-lint. `make check` runs what CI runs, except the integration and accessibility
jobs. `make lint` also enforces the `games/` boundary: packages under `games/` must not import
gravel (depguard), because they become standalone modules at the open-source cut. `make
vendor-htmx` refreshes the vendored htmx to `HTMX_VERSION`.
