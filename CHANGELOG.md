# Changelog

All notable changes to gravel are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions will follow
[Semantic Versioning](https://semver.org/) from the first release.

## [Unreleased]

## [0.1.0] - 2026-10-08

### Fixed

- The release workflow could not start: `sigstore/cosign-installer@v4` is not a published ref (the repo keeps floating tags only up to `v3`), so the run failed before its first step. Pinned to `v4.1.2` (#30).
### Added

- The hub skeleton (#12): `gravel-hub`, one Go binary beside Postgres. A Connect API (`gravel.hub.v1.OrganizationService`; gRPC, gRPC-Web and HTTP-JSON on the same handlers, with gRPC health and reflection and an OpenAPI document for the JSON side), Postgres through pgx with embedded goose migrations applied at start or by `gravel-hub migrate`, the built-in organization and the one-time owner claim (the hub mints the token at start while unowned and logs it; `ClaimOwnership` consumes it once; ADR-0002), a strict versioned YAML configuration whose only secret comes from a podman secret or the environment, structured logs with request ids, Prometheus metrics and pprof on a separate loopback listener, `/healthz` and `/readyz`, graceful shutdown and a `healthcheck` subcommand for the container. `make up` builds the image with ko (distroless, nonroot) and runs hub + Postgres 17 with podman compose; `deploy/quadlet/` holds the production units. CI runs gofmt, vet, buf lint and breaking, golangci-lint with a depguard rule that keeps `games/` packages free of gravel imports, govulncheck, a generate drift check, a quadlet dry-run, an image build and an integration job against Postgres; a tag releases binaries, SBOMs and a cosign-signed image through goreleaser. `docs/hub.md` and `deploy/README.md` document it.
- Every newly opened or reopened issue and pull request is put on the HTG Platform board (hidden-token-gaming's org project, where gravel's issues are sequenced with the rest of the platform work) by that org's reusable `add-to-project` workflow. Without the `PROJECT_ADMIN_TOKEN` repository secret the job logs a notice and skips (hidden-token-gaming/.github#41).
- `docs/adr/0001-hub-first-kubernetes-later.md`: the hub is one Go service with Postgres as the system of record, Kubernetes (the operator plus Agones) is the fully managed driver added with the first managed game, podman quadlets are the first deployment target, adapters and drivers run in-process until the open-source cut, stats start on plain Postgres, and backups belong to the hub. Decided in Hidden Token Gaming's 2026-10-08 strategic review (#11).

### Changed

- README and CLAUDE.md follow ADR-0001: status line, principle 2 (the resource lives in the hub's database), the hub and backup paragraphs, the adapter-repo rule, the local-dev default, and a seven-step build order with the issues behind each step. The launch games are the five Hidden Token Gaming plays, in three shapes, with War Dogs first; the open threads link the design-delta issues #2–#9 (#1).

[Unreleased]: https://github.com/gravel-project/gravel/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/gravel-project/gravel/releases/tag/v0.1.0
