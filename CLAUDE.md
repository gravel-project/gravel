# gravel — context for Claude Code

## What this is

An open-source framework in Go for hosting many game servers across many games with one control interface and cross-game stats. The hub is one Go service beside Postgres; Kubernetes (the operator plus Agones) is the fully managed driver, added with the first managed game. Design is **complete** as of 2026-09-25 and the build order was revised by `docs/adr/0001-hub-first-kubernetes-later.md` on 2026-10-08 (hub first, Kubernetes later). The hub skeleton (#12) is in: `cmd/gravel-hub`, `internal/{config,store,org,api,httpx,hub}`, `proto/` with generated `gen/`, `deploy/` (compose + quadlets). `docs/hub.md` says how it runs; `make check` is the pre-PR gate. `docs/adr/0003-cloud-vm-driver-behind-a-cloud-provider-interface.md` (2026-10-08) adds the cloud-VM driver: a VM gravel owns through `cloud.Provider`, DigitalOcean first, with vertical scaling at a map change.

**Read `README.md` first** — it is the distilled design and the source of truth for every architectural decision. The full block-level design doc with the reasoning behind each decision is at https://claude.ai/code/artifact/e559420c-d087-4f74-8829-853cafbb5eb8 (private).

## Owner

John (independent developer, Fedora Linux, Go expert). Proving ground is his own community, Hidden Token Gaming (hiddentoken.com). Launch games, five in three shapes: War Dogs (external, plain-HTTP RCON plus a push kill feed; **first**), Counter-Strike 2 (fully managed in-cluster via Agones; second), and Sea of Thieves, Star Citizen and PUBG (server-less: identity, participation and official-API stats only).

## Non-negotiable principles (honor these in every change)

1. **Don't reinvent the wheel.** Build on Goth, Agones, SPIFFE/SPIRE, CloudNativePG, TimescaleDB, Connect/buf, OCM, LinuxGSM, goreleaser/cosign. If you're about to hand-roll something one of these does, stop.
2. **Every seam is a provider behind a Go interface**, written in domain terms, with a boring default implementation. Never leak a specific backend's features into an interface.
3. **API-only clients.** CLI and web UI talk to the core API, never to Kubernetes or Postgres. Business rules live in the API exactly once. Resources live in the hub's database; Kubernetes is a driver target (ADR-0001).
4. **The CLI consumes the HTTP-JSON side of Connect, never gRPC.** No gRPC stack in the CLI binary.
5. **Stats events are keyed by provider identity (Steam ID), never internal user ID; identity → user is resolved at read time.**
6. **Game-server logs are authoritative.** The ingestion pipeline is reliable delivery on top.
7. **Spokes must keep working when the hub is unreachable.**
8. **The agent never auto-updates and only does what the member-approved Game spec allows.** The hub cannot widen its powers.
9. **The web UI is the authoritative admin surface.** Discord is an optional convenience layer.
10. **Anonymize, don't delete.** No hard deletes of identity-linked data; tombstone + irreversible re-key.
11. **Trust is not retroactive.** Promotion to Official counts from the promotion timestamp forward.
12. **Per-game operational concerns live in the `Game` spec, never in the core.**

## Vocabulary (use these names exactly)

- `ManagedServer` — our CRD for one server (NOT `GameServer`, which is Agones').
- `Game` — per-game spec CRD; also a declarative YAML manifest.
- `location` — where a server runs (a cluster, a cloud VM gravel made, a member's PC). Every server has one.
- `organization` — on every top-level object; single-tenant today, defaults to the built-in org.
- Trust levels: **Official** / **Community**. Curation states: submitted → approved → featured → revoked.
- Drivers: fully managed (Agones) / cloud-VM (a VM gravel creates, sizes, parks and destroys through `cloud.Provider`; ADR-0003) / external-reachable (RCON, SSH) / externally referenced. Local-execution driver has container (Linux) and native-process (Windows/bare) flavors. Every driver declares a capabilities flag.
- Hub / spoke / agent. Agents dial **outbound**.

## Stack

Go · Connect (buf) · Postgres (identity and stats; TimescaleDB as an extension on a measured signal, ClickHouse later, behind the stats-store interface) · Goth · templ + htmx (web UI) · Discord bot modules (disgo) · podman quadlets as the first deployment target · controller-runtime/kubebuilder + Agones for the fully managed driver (in-cluster only, with Counter-Strike 2; Kind for its e2e, OpenShift supported via Helm + OLM) · SPIFFE/SPIRE + mTLS from the first spoke · the clouds' own SDKs (godo first) behind `cloud.Provider` for the cloud-VM driver, a minimal DOKS cluster as the operator path's environment with Kind in CI (ADR-0003) · Patreon (pluggable payments) · Apache 2.0.

## Repo layout

This repo is the **monorepo for the core**: hub (API, web UI), CLI, operator, agent. Stats adapters and the game catalog become **separate repos at the open-source cut**; until then adapters and drivers are packages here (`adapters/<game>`, `drivers/<game>`) behind the designed interfaces and the conformance suite (ADR-0001). Keep the seam clean: nothing outside an adapter package may import a game's wire format. HTG-specific behaviour never lives here; it belongs in `hidden-token-gaming/htg`.

- `cmd/gravel-hub` — the binary (serve, migrate, config check, healthcheck, version).
- `internal/config` (strict versioned YAML, secrets from podman secrets or env) · `internal/store` (pgx, embedded goose migrations; the only package that speaks SQL) · `internal/org` (built-in organization, owner claim) · `internal/api` (Connect handlers; maps domain errors to codes, never leaks internals) · `internal/httpx` (request ids, access logs, recovery, metrics) · `internal/hub` (wiring, listeners, graceful drain).
- `proto/gravel/hub/v1/` is the API source; `gen/` is generated and committed (`make generate`; CI fails on drift).
- `games/<game>/` — a game's client library (RCON, feed, log parsing). **No gravel imports**, enforced by depguard: these become standalone Go modules at the open-source cut. `drivers/<game>/` and `adapters/<game>/` adapt them to gravel's interfaces. No nested go.mod until the cut.
- `cloud/` — the cloud provider interface, its fake and conformance suite, and `cloud/digitalocean/` (godo); `drivers/cloudvm/` is the driver that uses it (ADR-0003). Nothing outside `cloud/` imports a vendor SDK.
- `deploy/` — `compose.yaml` for development, `quadlet/` for production. `docs/` — ADRs and `hub.md`.

## Build order

Per ADR-0001 (hub first, Kubernetes later):

1. Hub: Connect API, Postgres with migrations, config, health, metrics, UI skeleton, podman quadlets
2. Identity: Discord and Steam login, `(provider, subject)` identities, sessions
3. External-reachable driver + War Dogs adapter, stats store, boards, moderation with an audit log
4. Composable Discord modules, OIDC for first-party apps, the participation API
5. `gravel` CLI as a thin HTTP-JSON client, including `gravel claim`, which consumes the one-time owner-claim token the hub printed at start (ADR-0002)
6. Operator + Agones as the fully managed driver, proven on a minimal DOKS cluster; the cloud provider interface and the DigitalOcean implementation; the cloud-VM driver with Counter-Strike 2 and vertical scaling at a map change; the Counter-Strike 2 adapter; the transient-server API (ADR-0003)
7. Standalone agent for machines gravel does not own, and the untrusted-member test harness (cast as a fake untrusted community member)

## Explicit non-goals — do not build these

Matchmaking · player-facing allocation · billing / payment processing · anti-cheat · mod hosting · mobile app (deferred) · hub HA in v1.

## Conventions

- Version the core stats schema from day one (version field in the protobuf).
- Postgres schema migrations from day one; CRD versioning early.
- Structured logs + Prometheus metrics in every component.
- "Clone, one command, it's running": `make up` (ko image + podman compose) for the hub; Kind for the operator path later. Never assume a cloud cluster.
- `make check` before every PR. Tools are pinned at the top of the Makefile and installed to `.bin/` by `make tools` (not `go tool` directives); bump a version there and, for golangci-lint, in `.github/workflows/ci.yml`.
- Secrets reach containers as podman secrets (`Secret=` in quadlets, `external: true` in compose) and the config reads them through `*_file` keys. Never bind-mount a secret file (SELinux blocks it, and it leaves plaintext in a working tree).
- The owner claim: the hub mints and logs the one-time token at start while unowned; `ClaimOwnership` consumes it (ADR-0002).
- Secrets never in plaintext config or DB.

## Open threads (don't guess at these — ask John)

- Exact War Dogs stats format (stats confirmed to exist).
- Legal: raw-event retention (13-month placeholder), retention for ban enforcement, ToS/privacy policy.
- GitHub org staked: `gravel-project` (2026-10-02); this repo moved there from `jomkz/gravel`. New gravel repos (`gravel-catalog`, `gravel-adapter-*`) are created in it. The domain is not yet staked.
