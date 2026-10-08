# gravel — context for Claude Code

## What this is

An open-source framework in Go for hosting many game servers across many games with one control interface and cross-game stats. The hub is one Go service beside Postgres; Kubernetes (the operator plus Agones) is the fully managed driver, added with the first managed game. Design is **complete** as of 2026-09-25 and the build order was revised by `docs/adr/0001-hub-first-kubernetes-later.md` on 2026-10-08 (hub first, Kubernetes later); the codebase is at the first-commit stage.

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
- `location` — where a server runs (a cluster, a droplet, a member's PC). Every server has one.
- `organization` — on every top-level object; single-tenant today, defaults to the built-in org.
- Trust levels: **Official** / **Community**. Curation states: submitted → approved → featured → revoked.
- Drivers: fully managed (Agones) / external-reachable (RCON, SSH) / externally referenced. Local-execution driver has container (Linux) and native-process (Windows/bare) flavors. Every driver declares a capabilities flag.
- Hub / spoke / agent. Agents dial **outbound**.

## Stack

Go · Connect (buf) · Postgres (identity and stats; TimescaleDB as an extension on a measured signal, ClickHouse later, behind the stats-store interface) · Goth · templ + htmx (web UI) · Discord bot modules (disgo) · podman quadlets as the first deployment target · controller-runtime/kubebuilder + Agones for the fully managed driver (in-cluster only, with Counter-Strike 2; Kind for its e2e, OpenShift supported via Helm + OLM) · SPIFFE/SPIRE + mTLS from the first spoke · Patreon (pluggable payments) · Apache 2.0.

## Repo layout

This repo is the **monorepo for the core**: hub (API, web UI), CLI, operator, agent. Stats adapters and the game catalog become **separate repos at the open-source cut**; until then adapters and drivers are packages here (`adapters/<game>`, `drivers/<game>`) behind the designed interfaces and the conformance suite (ADR-0001). Keep the seam clean: nothing outside an adapter package may import a game's wire format. HTG-specific behaviour never lives here; it belongs in `hidden-token-gaming/htg`.

## Build order

Per ADR-0001 (hub first, Kubernetes later):

1. Hub: Connect API, Postgres with migrations, config, health, metrics, UI skeleton, podman quadlets
2. Identity: Discord and Steam login, `(provider, subject)` identities, sessions
3. External-reachable driver + War Dogs adapter, stats store, boards, moderation with an audit log
4. Composable Discord modules, OIDC for first-party apps, the participation API
5. `gravel` CLI as a thin HTTP-JSON client, including the hub bootstrap command (one-time owner-claim token, valid only while the hub is unowned)
6. Operator + Agones as the fully managed driver, the Counter-Strike 2 adapter, the transient-server API
7. Standalone agent + DigitalOcean droplet test harness (cast as a fake untrusted community member)

## Explicit non-goals — do not build these

Matchmaking · player-facing allocation · billing / payment processing · anti-cheat · mod hosting · mobile app (deferred) · hub HA in v1.

## Conventions

- Version the core stats schema from day one (version field in the protobuf).
- Postgres schema migrations from day one; CRD versioning early.
- Structured logs + Prometheus metrics in every component.
- "Clone, one command, it's running" on Kind is a first-class requirement — never assume a cloud cluster.
- Secrets never in plaintext config or DB.

## Open threads (don't guess at these — ask John)

- Exact War Dogs stats format (stats confirmed to exist).
- Legal: raw-event retention (13-month placeholder), retention for ban enforcement, ToS/privacy policy.
- GitHub org staked: `gravel-project` (2026-10-02); this repo moved there from `jomkz/gravel`. New gravel repos (`gravel-catalog`, `gravel-adapter-*`) are created in it. The domain is not yet staked.
