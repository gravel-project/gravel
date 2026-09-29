# gravel

> Open-source, Kubernetes-native framework for hosting many game servers across many games, with one control interface and cross-game statistics. Each server is a grain; the fleet is the gravel.

**Status:** design complete (2026-09-25), pre-code. The next step is the first commit.
**Design doc (full, block-level detail):** https://claude.ai/code/artifact/e559420c-d087-4f74-8829-853cafbb5eb8 (private Claude Docs; this README is the distilled version)
**License:** Apache 2.0

---

## Why this exists

Hidden Token Gaming (hiddentoken.com) is the proving ground — a real community with a real Discord, real players, real donations. Launch games exercise opposite ends of the design:

- **War Dogs** — externally hosted, RCON-only, assume Windows-only. We don't run it; we link it, read stats and push moderation over RCON. Stats are confirmed to exist; exact format still to be pinned down before its adapter is built.
- **Counter-Strike** — fully managed, containerized in-cluster via Agones. Where all the operational concerns (SteamCMD, persistent volumes, updates, backups) actually apply.

**Build closed first, open-source v1 later.** Frameworks built in the abstract get the abstractions wrong; this one gets battle-tested on a live community before extraction. Pragmatic shortcuts are fine as long as the provider-behind-an-interface seams stay clean.

## Guiding principle: don't reinvent the wheel

Wherever a solid open-source project already solves a layer, build on it. Our value is the layers above — identity, stats, community, external-server drivers — not the plumbing.

Applied: Goth (auth) · TimescaleDB (stats) · Agones (in-cluster servers) · SPIFFE/SPIRE (machine identity) · Open Cluster Management (first spoke) · CloudNativePG (Postgres ops + backup/PITR) · Connect/buf (API) · LinuxGSM (game knowledge) · goreleaser + sigstore/cosign (releases)

The principle applies to itself: nothing above is hard-wired. Each sits behind a Go interface as the *first* implementation.

## Product goals

- User accounts linked to in-game identity on the server
- Cross-game stat tracking with per-game customization
- Aggregate stats across games under one identity
- Donations / membership (Patreon first)
- Discord-based community with role sync
- Default layout per game, customizable by the host
- Members can contribute servers they run into the community listing

## Load-bearing principles

These recur across the whole design. When in doubt, these decide.

1. **Every seam is a provider behind a Go interface.** Stats stores, payments, identity providers, lifecycle drivers, spoke implementations, game-knowledge providers, pseudonym generators — all pluggable, all with a boring-but-solid default.
2. **The declarative resource is the source of truth; every client is API-only.** CLI and web UI never touch Kubernetes directly. Validation and business rules live in exactly one place.
3. **Stats are keyed by provider identity (Steam ID), never internal user ID, and resolved to a user at read time.** Unlinked players accumulate history; late linking inherits it; merges never move events; erasure re-keys instead of deleting.
4. **Game-server logs are authoritative.** Our pipeline is reliable delivery on top, never the sole record.
5. **Spokes keep working when the hub is unreachable.** Servers keep running; stats buffer locally and sync on reconnect.
6. **Agent updates are always manual, and the agent only ever does what the member-approved spec allows.** Never silently change software on someone's personal rig; never let the hub widen the agent's powers.
7. **The web UI is the authoritative admin surface. Discord is a convenience layer.** Never weld the control plane to a third party we don't own.
8. **Anonymize, don't delete.** GDPR erasure scrubs the person and keeps the record.
9. **Trust is not retroactive.** Promotion to Official counts from the promotion timestamp forward.

## Architecture

### Hub and spoke

- **Hub** runs the core: API, identity, stats databases, web UI. It is the single point of no return — backed up with CloudNativePG WAL archiving + PITR from day one.
- **Spoke** = any location that runs servers: a Kubernetes cluster (our operator + Agones + a small agent), a Podman box, a DigitalOcean droplet, or a member's personal Windows gaming rig running the standalone agent as a native `.exe`.
- **Agents dial outbound** to the hub over a persistent gRPC stream. A Kind cluster behind a home router joins with no inbound firewall holes.
- **`location`** is the unifying field on every server resource. Multi-cluster, member-contributed servers, and the transient "spin one up for my buddies" case are all the same design.
- Enrollment: SPIRE join-token attestation. One-time token, 15-minute default lifetime, burned on use.
- Open Cluster Management is the first spoke implementation, behind our own interface. A thin standalone Go agent covers non-Kubernetes locations.

### Custom resources (operator: Go, controller-runtime / kubebuilder)

- **`ManagedServer`** — the source of truth for one server. Named to avoid colliding with Agones' `GameServer` and because the framework *manages* servers it doesn't necessarily *run*. Carries `location`, `organization`, driver, adapter, trust level, curation state, ownership.
- **`Game`** — per-game spec: distribution (Steam app ID), persistence, updates, backups, hard/soft stat sanity bounds, default UI layout, and the operational concerns that must never leak into the core.
- Every top-level object carries an **`organization`** field defaulting to the built-in org. Single-tenant by design; the field is the seam for later. Do NOT build quotas or isolation until a real second tenant exists.

### Representation vs execution — lifecycle drivers

A `ManagedServer` records a server that exists somewhere; a **driver** controls it. Each driver declares a **capabilities flag** and the UI only shows what the driver supports.

- **Fully managed** — Agones underneath (port assignment, health, safe node drain). Agones is a hard dependency of the in-cluster path *only*; the framework must run without it for external-only hosts.
- **External but reachable** — RCON / SSH for stats and commands, no lifecycle control.
- **Externally referenced** — registered so it shows in the UI; we can barely touch it.
- The local-execution driver has **two flavors**: container-based (Linux) and native-process-based (Windows / bare). War Dogs, if ever brought in-cluster, rides the native flavor.
- Transient servers: on-demand start, **auto-tear-down after N minutes empty**, join info in the web UI, Community trust by default.

### API — Connect (buf)

One protobuf definition serves gRPC, gRPC-Web, and plain HTTP-JSON. No separate gateway.

- gRPC/protobuf earns its place on three paths only: heavy service-to-service traffic, a schema-first contract for **third-party-written stats adapters**, and native **streaming** (stats events, hub↔spoke link).
- **The CLI consumes the HTTP-JSON API, not gRPC.** Smaller binary, and it proves the public surface is complete. Live log tailing is one-way server-streaming, which Connect covers over HTTP — no gRPC needed.
- Versioning rides the protobuf package; OpenAPI is emitted for the JSON side.
- Rate limits: per-identity on authenticated calls, per-IP on the public surface; public leaderboard pages are cached and scraping is bounded, not blocked.

### Identity & accounts

- Internal user ID is the primary key. External providers (Steam, Xbox, Apple, Google, Discord, Patreon) are rows in a **linked-identities** table.
- **Goth** (markbates/goth) for provider login. Ory Kratos/Hydra rejected as too heavy.
- Two verification layers: provider login (Steam OpenID) and the **RCON code handshake** (player types a one-time code in-game; the adapter watching over RCON confirms the link).
- RCON code hardening: CSPRNG token, minutes-long expiry, single-use, two-layer rate limits (requests + attempts), bound to the exact identity + server it was issued for.
- Account linking/merging: link while logged in (primary path); shared verified email; else RCON proof-of-control; **absorb, don't delete**; one transaction; immutable log.
- Human roles: player / moderator / admin / owner, scoped to `organization`.
- Machine identity: **SPIFFE/SPIRE all the way** — mTLS on internal gRPC, short-lived CLI tokens, behind a pluggable identity-provider interface.
- Account recovery: provider-only identity means losing your only provider loses the account — an accepted, documented v1 tradeoff. Mitigation is multi-provider linking, not a recovery flow.
- Day-zero bootstrap: `gravel` CLI bootstrap command initializes the hub and prints a one-time owner-claim token, single-use and only valid while the hub is unowned.

### Data layer

- **Identity → Postgres** (CloudNativePG). Relational, uniqueness enforced at the DB level.
- **Stats → TimescaleDB** first, **ClickHouse** later, behind a Go stats-store interface written in domain terms (write event, query aggregates over a time range). Never leak DB-specific features into the interface.
- Stats schema: fixed common fields (kills, deaths, playtime, score, match count) + an extensions map for game-specific fields. **Version the core schema from day one.**
- Retention: TimescaleDB continuous aggregates + compression + retention policies. **Raw identity-linked events roll off at 13 months** (placeholder pending legal review); aggregates persist indefinitely.
- Hub backup/restore: the operator owns it as a declarative workflow ("desired recovery point = T"), PITRs both DBs independently to the same wall-clock moment, and delegates all data movement to CloudNativePG. Restore is a documented, rehearsed ops task.
- Configuration: one **Organization settings** resource, managed through the API, holds every host-configurable knob (Discord role mapping, token lifetimes, layout overrides, pseudonym provider, trust floors). No scattered ConfigMaps.

### Stats adapters

- Each game's adapter is its own gRPC service (its own repo). protobuf `StatsAdapter`: fetch stats, stream live events, describe schema.
- Adapters **self-register** with the hub on startup over SPIFFE-attested mTLS, advertising game, schema version, and capabilities; health-checked so dead adapters drop out.
- Ingestion reliability: event IDs + dedup (at-least-once), **on-disk durable local queues in v1**, backfill from logs as the backstop.
- **At-source signing in v1**: adapters sign each event batch with their SPIFFE-issued key so the hub verifies provenance.
- An adapter conformance test suite lets third parties prove they honor the contract.

### Trust, leaderboards & moderation

- Two trust tiers: **Official** (org-run, authoritative, feeds global leaderboards, player-visible badge) vs **Community** (local stats/leaderboards only). Admin promotion is the same switch as member-server curation (submitted → approved → featured → revoked).
- Leaderboard scopes are first-class: per-server, per-game, per-organization, global-across-Official — plus **time windows** (weekly, monthly, named seasons). Same engine, different filter. A season reset is a display boundary, never a deletion.
- Sanity bounds from the Game spec: hard bounds reject the impossible at ingestion; soft bounds flag the improbable for review. Applies to Official servers too.
- Fraud → an explicit, logged **purge** (bounded delete by server + time range). Normal demotion leaves history standing.
- Bans are keyed to Steam ID with scope (org-wide vs server-local), reason, expiry, issuer. Proactive push over RCON + startup pull; reactive re-push if a banned identity shows up in the stats stream. Immutable audit log. Alt evasion is an ongoing arms race, not solved in v1.
- **Unified admin review queue**: soft-bound flags, curation submissions, moderation reports, and promotion requests all land in one inbox with approve/reject + audit trail. Discord bot delivers notifications and optional quick actions; the web UI is authoritative.

### Player privacy

- Unlinked players appear on public boards as a **stable, per-identity pseudonym** (not reversible to the Steam ID), never their real Steam name, with a hover icon: "anonymized until claimed." Boards feel alive from day one without naming anyone who doesn't know they're there.
- Linking + opting in swaps the pseudonym for a chosen display name at render time; going private reverts it. Events never move.
- **Pluggable pseudonym provider**: default is a deterministic funny-name generator from a curated word list; an AI-backed provider can sit behind the same interface. Contract: deterministic, stable, unique per identity, safe for public, generated once and persisted.
- GDPR erasure: scrub email / Steam ID / display name, tombstone the user ID, re-key events to an irreversible anonymized token. Aggregates stay intact ("a deleted player"). Retention-for-ban-enforcement is a lawyer question.

### Donations & Discord

- **Patreon first** (OAuth link + webhooks + polling reconciliation). Membership state is tied to the user ID; benefit logic is separated from provider sync; pluggable payments interface for PayPal etc. later. We never process payments ourselves.
- Discord is an OAuth provider (linked identity) plus a bot that syncs roles from internal status (tier, achievements) using host-configurable mapping rules.

### Game specs & catalog

- A Game spec is a declarative **YAML manifest** — the same object the `Game` CRD describes. Adding a game needs no Go.
- **Game-spec-provider interface**: "given a game, produce a Game spec." **LinuxGSM** (Linux Game Server Manager — install/config/update/backup knowledge for ~200 dedicated servers) is the first provider. We reuse its per-game knowledge; we keep our own orchestration.
- Import produces a **draft** the admin edits freely. Edits are stored as an **overlay** on the imported base so upstream refreshes re-apply cleanly; conflicts are surfaced, never silently resolved.
- War Dogs and Counter-Strike specs are hand-authored reference examples. Community catalog lives in its own repo; games are added by pull request.

### Web UI

- **templ + htmx** to start: server-rendered Go, no JS build pipeline, one deploy artifact. Consumes the same Connect HTTP-JSON API, so a later SPA is a front-end-only change.
- Per-game layout is **declarative data in the Game spec** (which stat cards, which leaderboard columns), rendered by the front end, host-overridable. The core stays game-agnostic.
- Grafana is optional and operator-facing only (ops/health dashboards). Player-facing pages are native and branded.

## Local dev & runnability

- "Clone, one command, it's running" is a first-class requirement. Never assume a managed cloud cluster: no hard dependency on cloud load balancers, storage classes, or external DNS.
- **Kind is the documented default** (Podman-backed on Fedora). **OpenShift is also supported**, with OpenShift-specific features where available (CRC for a local OpenShift).
- Packaging: Helm chart + OLM bundle so it's a first-class OpenShift operator.

## Ops & release engineering

- Secrets: Kubernetes Secrets at minimum; External Secrets Operator / Vault later.
- Observability: structured logs + Prometheus metrics across operator, adapters, agents.
- Migrations: Postgres schema migration tool from day one; CRD versioning early.
- Releases: semver + cadence; signed images + SBOM via goreleaser and sigstore/cosign; CI stands the operator up against Kind on every change; CONTRIBUTING, code of conduct, DCO.
- Hub availability: **v1 is single hub + restore-from-backup.** HA (multi-replica API, CNPG replicas) is a supported future config, not a v1 requirement — the spokes-survive rule keeps the blast radius small.
- Version skew: core/operator/adapters upgrade as a coordinated rollout. The agent↔hub protocol is versioned and negotiated on connect; the hub supports agents a couple of minor versions back, and too-old agents are refused politely with a pointer to the new version.

## Scale target

- v1 reality: tens of servers, hundreds of concurrent players, single-to-low-tens of events/sec. **v1 is a correctness problem, not a scaling one.**
- Comfortable: ≤ ~100 servers, a few thousand concurrent, tens of events/sec → TimescaleDB, don't think about ClickHouse.
- Watch-it: several hundred servers, tens of thousands concurrent, hundreds of events/sec → instrument and pay attention.
- Trigger: sustained ~1,000+ events/sec **or** analytical queries missing their latency budget → ClickHouse. Migrate on a measured signal, never on a feeling.

## Explicit non-goals

- **Matchmaking** — belongs to the game or a dedicated service.
- **Player-facing allocation** — no "give me a match" queue in v1; the on-demand transient lifecycle stays.
- **Billing** — we integrate with donation platforms; we never process payments or hold payment state.
- **Anti-cheat** — lean on the game's own (VAC etc.); our sanity bounds protect leaderboard integrity, not the game.
- **Mod hosting** — a Game spec may say "run these mods" as config; we never store or serve mod files.
- **Mobile app** — deferred, not foreclosed; the HTTP-JSON API + OpenAPI make it a front-end project later.
- **Hub HA in v1** — see above.

## Test harness

A DigitalOcean droplet running Counter-Strike, deliberately cast as a **fake untrusted community member**. Double duty: first remote-spoke validation (outbound agent, stats over gRPC, `location` outside the hub cluster) and the untrusted-source path end to end (Community trust level, at-source signing verification, curation/moderation reach).

## Repo layout (planned)

- **This repo (`gravel`) is the monorepo for the core:** operator, core API, CLI, web UI, agent — everything that versions and releases together.
- **Separate repos across the plugin seams:** stats adapters (one per game) and the community game catalog. The repo boundary is the plugin boundary.
- Name rationale: `gsf` rejected (one letter off LinuxGSM's `GSM`), `grain` taken (grain-lang ships a `grain` binary), `grit` crowded, `gman` is Half-Life's. `gravel` is meaningful and effectively free; the only namesake is a small, low-activity Go build tool. "Gravel Server Manager = GSM" is a docs wink only, never the official expansion.

## Build order

1. Core API (Connect) + `ManagedServer` / `Game` CRDs + operator
2. `gravel` CLI as a thin HTTP-JSON client (including hub bootstrap)
3. Web UI (templ + htmx) over the same API
4. First adapters: Counter-Strike (in-cluster, Agones) and War Dogs (external, RCON)
5. Standalone agent + droplet test harness

## Open threads that need the outside world

- **Lawyer:** raw-event retention (13-month placeholder), retention-for-ban-enforcement under GDPR, ToS + privacy policy for the public site.
- **War Dogs:** stats confirmed to exist — pin down the exact format before building its adapter.
- **Stake the name:** GitHub org + domain for gravel.
