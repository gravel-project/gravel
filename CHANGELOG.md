# Changelog

All notable changes to gravel are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions will follow
[Semantic Versioning](https://semver.org/) from the first release.

## [Unreleased]

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

[Unreleased]: https://github.com/gravel-project/gravel/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/gravel-project/gravel/releases/tag/v0.1.0
