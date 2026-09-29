# gravel — context for Claude Code

## What this is

An open-source, Kubernetes-native framework for hosting many game servers across many games with one control interface and cross-game stats. Written in Go. Design is **complete** as of 2026-09-25; the codebase is at the first-commit stage.

**Read `README.md` first** — it is the distilled design and the source of truth for every architectural decision. The full block-level design doc with the reasoning behind each decision is at https://claude.ai/code/artifact/e559420c-d087-4f74-8829-853cafbb5eb8 (private).

## Owner

John (independent developer, Fedora Linux, Go expert). Proving ground is his own community, Hidden Token Gaming (hiddentoken.com). Launch games: War Dogs (external, RCON-only, Windows) and Counter-Strike (fully managed in-cluster via Agones).

## Non-negotiable principles (honor these in every change)

1. **Don't reinvent the wheel.** Build on Goth, Agones, SPIFFE/SPIRE, CloudNativePG, TimescaleDB, Connect/buf, OCM, LinuxGSM, goreleaser/cosign. If you're about to hand-roll something one of these does, stop.
2. **Every seam is a provider behind a Go interface**, written in domain terms, with a boring default implementation. Never leak a specific backend's features into an interface.
3. **API-only clients.** CLI and web UI talk to the core API, never to Kubernetes. Business rules live in the API exactly once.
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

Go · controller-runtime/kubebuilder · Agones (in-cluster only) · Connect (buf) · SPIFFE/SPIRE + mTLS · Goth · Postgres via CloudNativePG (identity) · TimescaleDB (stats; ClickHouse later behind the stats-store interface) · templ + htmx (web UI) · Patreon (pluggable payments) · Discord bot · Kind default for local dev, OpenShift supported (Helm + OLM) · Apache 2.0.

## Repo layout

This repo is the **monorepo for the core**: operator, core API, CLI, web UI, agent. Stats adapters and the game catalog live in **separate repos** — the repo boundary is the plugin boundary. Do not put a game-specific adapter in here.

## Build order

1. Core API (Connect) + `ManagedServer`/`Game` CRDs + operator
2. `gravel` CLI as a thin HTTP-JSON client, including the hub bootstrap command (one-time owner-claim token, valid only while the hub is unowned)
3. Web UI over the same API
4. First adapters (separate repos): Counter-Strike, War Dogs
5. Standalone agent + DigitalOcean droplet test harness (cast as a fake untrusted community member)

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
- GitHub org + domain for `gravel` not yet staked.
