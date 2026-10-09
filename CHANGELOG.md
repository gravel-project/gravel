# Changelog

All notable changes to gravel are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions will follow
[Semantic Versioning](https://semver.org/) from the first release.

## [Unreleased]

## [0.4.0] - 2026-10-09

### Added

- The Organization settings resource (#7, ADR-0007): one JSON document on the organization (migration 3) holding the theme (tokens for the light and dark schemes, the font, a logo, a favicon) and navigation links (header or footer, optionally owner-only), with the Discord role mapping, token lifetimes and layout overrides to join it. `GetOrganizationSettings` is public (the login page renders the theme) and `UpdateOrganizationSettings` is the owner's; validation names every invalid field (colours must be plain CSS colours, URLs https or site-relative, at most 12 links). The pages read the theme through the in-process API, so a host's look is data, not a fork. `gravel-hub settings export` prints the document as a YAML manifest and `gravel-hub settings apply <manifest>` stores one (`--dry-run` exits 3 on a change), for deployments that commit their settings until the `gravel` CLI applies them over the API.

### Changed

- The quadlet units pin the 0.3.0 images by index digest, both verified against the release workflow at `v0.3.0`: `ghcr.io/gravel-project/gravel-hub@sha256:95c16cd9a8789019977f4e2057edd1180469c08df2bab7177e5cd5b77e3b2ce0` in `gravel-hub.container`, and `ghcr.io/gravel-project/gravel-postgres@sha256:f9bd60a531e1fc8e1d78935b972ef4b2f3c51543c60b9eb3308282d04c4f75df` in the backup set, which no longer carries a placeholder; `docs/hub.md` names 0.3.0 as the tag example.

### Fixed

- The backup drop-in's archive command wrote `%p`, which systemd expands as a specifier in the generated `ExecStart` (to the unit's prefix name), so a quadlet deployment with the drop-in installed would have archived nothing; it is `%%p` now, and `docs/hub.md` says why. The compose stack and the drill hand the command to podman directly and were unaffected (#49).

## [0.3.0] - 2026-10-09

### Added

- Backups (#28, ADR-0006): `ghcr.io/gravel-project/gravel-postgres`, the official Postgres 17 image plus WAL-G (fetched at build time and checked against its published SHA-256 per architecture; `deploy/postgres/Containerfile`, built by `make postgres-image` and by the release workflow for amd64 and arm64, cosign-signed). Postgres archives every WAL segment to object storage as it is written (`archive_command='wal-g wal-push %p'`, `archive_timeout=60`); `gravel-backup` takes a base backup and keeps 30 (`gravel-backup.timer`, nightly) and writes a status file; `gravel-restore --to <RFC 3339>` prepares a point-in-time recovery into an empty volume. The hub exposes `gravel_backup_last_success_timestamp_seconds`, `gravel_backup_last_run_timestamp_seconds`, `gravel_backup_last_run_ok` and `gravel_backup_status_readable` from the status file (`backup.status_file`) and `gravel_wal_archived_total`, `gravel_wal_last_archived_timestamp_seconds`, `gravel_wal_archive_failed_total`, `gravel_wal_last_failed_timestamp_seconds` and `gravel_wal_archiver_readable` from `pg_stat_archiver`. Backups are opt-in for quadlet deployments (`deploy/quadlet/backup/`: drop-ins, the unit, the timer, the volume; `make quadlet-install-backup`); the development stack archives into a local S3 store (versitygw) and `make backup` takes a base backup. `make backup-drill` (`deploy/backup-drill.sh`) restores to a point in time from a destroyed database and checks the rows and the schema, timed; CI runs it on every pull request as the `backup` job.

- The UI skeleton (#14, ADR-0005): the pages are templ components (`internal/web/templates/`, generated Go committed, `make generate` runs `templ generate`, CI fails on drift) that get their data from the hub's own Connect HTTP-JSON procedures, called in process through a transport that lends the page's cookie and request id and relays the cookies the API sets; a page can do only what the API allows. htmx 2.0.11 is vendored into the binary (`make vendor-htmx`, 0BSD) and swaps the main content on the account forms, with an `HX-Request` answered by the page's content and a plain request by a redirect, so every page works without JavaScript; it runs with `selfRequestsOnly`, no eval, no script tags from responses. The Content-Security-Policy allows nothing inline or third-party (`script-src 'self'`, `style-src 'self'`, `connect-src 'self'`). A theme (tokens for the light and dark schemes, the font, a logo, a favicon, navigation links in the header or footer, optionally owner-only) renders as `/theme.css` with an ETag and as layout attributes, from a `ThemeSource`; gravel#7 stores it on the Organization settings and the defaults, which meet WCAG AA contrast in both schemes, apply until then; a stored token that is not a plain CSS colour is replaced by the default. The layout has a skip link, landmarks and labelled forms; `make a11y` runs axe (`@axe-core/cli` 4.13.0 through npx, driving Chrome) over the login page and fixtures of the account and error pages, and CI runs it as the `a11y` job. `gravel.hub.v1.IdentityService.Logout` ends the calling session and clears its cookie, so the header's logout is an API call too.

### Fixed

- A disabled login provider's `*_file` secret is no longer read at start, so a `hub.yaml` that keeps the file keys with the providers off (the development stack's) loads without the podman secrets; since #40, `make up` failed on the missing Discord secret file (#28).

### Changed

- `deploy/quadlet/gravel-hub.container` pins `ghcr.io/gravel-project/gravel-hub` by the 0.2.0 index digest (`sha256:b286c5d693e36d0be734787c6c8b7c947a43b0ab3946655b769e8f5880eb140b`, verified against the release workflow at `v0.2.0`); `docs/hub.md` names 0.2.0 as the tag example.

## [0.2.0] - 2026-10-09

### Added

- Login (#13): a member signs in with Discord and links Steam, through Goth, on the `(provider, subject)` identity model. New tables for users, identities, an append-only identity log, sessions and attempts in progress (migration 2). Sessions are server-side: an opaque token in an `HttpOnly`, `SameSite=Lax` cookie (`Secure` and `__Host-` behind HTTPS), its hash in Postgres, no signing key; `auth.session_ttl` bounds them. Every login or link is an attempt bound to the browser by a cookie and to the provider's callback by a state value (OAuth2's `state`, or Steam's signed `return_to`), consumed once, so a replayed or forged callback is refused; a link must finish in the session that started it; an identity another member holds cannot be linked; the last identity cannot be unlinked. Pages at `/login` and `/account` (plain `html/template`, a strict CSP, no script; #14 restyles them) with the flows under `/auth/{provider}/start`, `/auth/{provider}/link`, `/auth/{provider}/callback`, `/auth/logout`, `/account/unlink` and `/account/claim`; state-changing forms carry a per-session CSRF token and `net/http`'s cross-origin protection fronts everything. `gravel.hub.v1.IdentityService` (`GetMe`, `UnlinkIdentity`, `RevokeSessions`, `LookupUser` for role sync, owner only until #6). Rate limits per client IP on anonymous requests and per user on authenticated ones (`rate_limit`, 429 with `Retry-After` on pages, `resource_exhausted` on procedures, probes exempt), with `server.client_ip_header` for a trusted proxy. Configuration section `auth` (`base_url`, `session_ttl`, `attempt_ttl`, `discord`, `steam`), the secrets `GRAVEL_DISCORD_CLIENT_SECRET` / `auth.discord.client_secret_file` and `GRAVEL_STEAM_API_KEY` / `auth.steam.api_key_file` with the database URL's precedence, and `gravel-hub config check` naming the providers. Metrics `gravel_auth_completions_total{provider,intent,result}` and `gravel_rate_limited_total{scope}`. Expired sessions and attempts are pruned every ten minutes. ADR-0004 records the decisions.

### Changed

- `ClaimOwnership` needs a session and records the caller as the owner (`Organization.owner_user_id`); the account page carries the claim form (ADR-0002 left the column for this). A claim made by 0.1.0 bound nobody, so migration 2 reopens it and the hub mints a fresh token at its next start (#13).
- Goth's Discord token exchange and Steam profile fetch are done in gravel: the first ignores the provider's HTTP client (no timeout, untestable), the second uses plain `http://` with the API key in the URL and fails without a key. Goth still builds the auth URLs, verifies the OpenID assertion and reads the Discord profile (#13).

### Fixed

- `go.mod`'s `toolchain` moves to Go 1.26.9 (the `go` directive stays 1.26.0): govulncheck flagged GO-2026-6617 in `net/http` at 1.26.8 (reached from the hub's server, the health check and pgx), which turned every PR's `vuln` job red until the toolchain moved. `actions/setup-go` installs exactly the version `go.mod` names, so the bump is the fix.
- `deploy/quadlet/gravel-hub.container`'s health check is the exec form: the string form runs through `/bin/sh -c`, which the distroless image lacks, so podman reported the hub unhealthy while it served fine (found on Hidden Token Gaming's first converge) (#33).

### Added

- `docs/adr/0003-cloud-vm-driver-behind-a-cloud-provider-interface.md`: a fourth lifecycle driver, cloud-VM, for a virtual machine gravel creates, sizes, parks and destroys through a `cloud.Provider` interface that DigitalOcean implements first and AWS, Azure and Google Cloud can implement later; the game under systemd from cloud-init, controlled over RCON and log push; the install on a detachable volume with the root disk minimal and fixed so a resize never touches disk; a vertical scaling policy that walks a tier ladder only at a map change or match end, with hysteresis, a cooldown and a chat warning, in-place first and replace later; parking as a provider-dependent step (a powered-off Droplet bills in full); a minimal DOKS cluster as the operator path's environment with Kind in CI; and the ownership boundary with an operator's infrastructure code (`Adopt`). Decided by John for Hidden Token Gaming on 2026-10-08; the work is #35, #36 and #37 (#34).
- `deploy/README.md` and `docs/hub.md` say the config file must be readable by uid 65532, the container's user: a 0600 file owned by the login user is "permission denied" from inside a rootless container, which cost Hidden Token Gaming's first converge (#32).
- `docs/releasing.md`: how a release is cut, what it contains, how to verify an image's signature and move the digest pin, and the one-time setup done for v0.1.0 (#31).

### Changed

- README and CLAUDE.md follow ADR-0003: the status line, the driver taxonomy and the spoke list gain the cloud-VM driver, the stack names the clouds' SDKs behind `cloud.Provider` and the DOKS environment, the layout gains `cloud/` and `drivers/cloudvm/`, build order step 6 is restated and step 7 is the agent for machines gravel does not own, and the test-harness paragraph says the Counter-Strike 2 test Droplet is the driver's first real server (#34).
- `deploy/quadlet/gravel-hub.container` pins `ghcr.io/gravel-project/gravel-hub` by the 0.1.0 index digest instead of `:latest`; `deploy/README.md` and `docs/hub.md` say the image tag carries no `v` and how to verify it (#31).

## [0.1.0] - 2026-10-08

### Fixed

- The release workflow could not start: `sigstore/cosign-installer@v4` is not a published ref (the repo keeps floating tags only up to `v3`), so the run failed before its first step. Pinned to `v4.1.2` (#30).
### Added

- The hub skeleton (#12): `gravel-hub`, one Go binary beside Postgres. A Connect API (`gravel.hub.v1.OrganizationService`; gRPC, gRPC-Web and HTTP-JSON on the same handlers, with gRPC health and reflection and an OpenAPI document for the JSON side), Postgres through pgx with embedded goose migrations applied at start or by `gravel-hub migrate`, the built-in organization and the one-time owner claim (the hub mints the token at start while unowned and logs it; `ClaimOwnership` consumes it once; ADR-0002), a strict versioned YAML configuration whose only secret comes from a podman secret or the environment, structured logs with request ids, Prometheus metrics and pprof on a separate loopback listener, `/healthz` and `/readyz`, graceful shutdown and a `healthcheck` subcommand for the container. `make up` builds the image with ko (distroless, nonroot) and runs hub + Postgres 17 with podman compose; `deploy/quadlet/` holds the production units. CI runs gofmt, vet, buf lint and breaking, golangci-lint with a depguard rule that keeps `games/` packages free of gravel imports, govulncheck, a generate drift check, a quadlet dry-run, an image build and an integration job against Postgres; a tag releases binaries, SBOMs and a cosign-signed image through goreleaser. `docs/hub.md` and `deploy/README.md` document it.
- Every newly opened or reopened issue and pull request is put on the HTG Platform board (hidden-token-gaming's org project, where gravel's issues are sequenced with the rest of the platform work) by that org's reusable `add-to-project` workflow. Without the `PROJECT_ADMIN_TOKEN` repository secret the job logs a notice and skips (hidden-token-gaming/.github#41).
- `docs/adr/0001-hub-first-kubernetes-later.md`: the hub is one Go service with Postgres as the system of record, Kubernetes (the operator plus Agones) is the fully managed driver added with the first managed game, podman quadlets are the first deployment target, adapters and drivers run in-process until the open-source cut, stats start on plain Postgres, and backups belong to the hub. Decided in Hidden Token Gaming's 2026-10-08 strategic review (#11).

### Changed

- README and CLAUDE.md follow ADR-0001: status line, principle 2 (the resource lives in the hub's database), the hub and backup paragraphs, the adapter-repo rule, the local-dev default, and a seven-step build order with the issues behind each step. The launch games are the five Hidden Token Gaming plays, in three shapes, with War Dogs first; the open threads link the design-delta issues #2–#9 (#1).

[Unreleased]: https://github.com/gravel-project/gravel/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/gravel-project/gravel/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/gravel-project/gravel/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/gravel-project/gravel/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/gravel-project/gravel/releases/tag/v0.1.0
