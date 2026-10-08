# ADR-0001: Hub first, Kubernetes later

**Status:** accepted (John, 2026-10-08) · **Changes:** the build order, the storage model and the first deployment target of the 2026-09-25 design · **Decided in:** Hidden Token Gaming's strategic review (handbook `docs/plan.md`, D19) · **Tracked by:** #11

## Context

The design as written builds the Kubernetes layer first: `ManagedServer` and `Game` as CRDs, a controller-runtime operator, k3s, Flux, CloudNativePG, TimescaleDB, Agones and SPIFFE/SPIRE, before any member-visible feature. Hidden Token Gaming, the proving ground, needs none of that for its first game. War Dogs is hosted under a closed licence at QONZER and is reachable only over a plain-HTTP bearer RCON API (29 routes) plus a push kill feed. What HTG needs from gravel for it is an HTTP ingest endpoint, an RCON poller, Postgres, a status card, seeding pings, boards and moderation with an audit log.

Two of the README's own rules already point this way: Agones is a hard dependency of the in-cluster path *only*, and the framework must run without it for external-only hosts; and abstractions are to be battle-tested on a live community before extraction, with pragmatic shortcuts allowed as long as the provider-behind-an-interface seams stay clean. Treating the full Kubernetes design as a prerequisite contradicted both, and on 2026-10-08 gravel had no code while every HTG platform phase waited on it.

## Decision

1. **The hub is one Go service with Postgres as the system of record** for `Game`, `ManagedServer`, Organization settings, identities and stats. The Connect API is the only write path. Principle 2 keeps its meaning: the declarative resource is the source of truth, and it lives in the hub's database, not in etcd.
2. **Kubernetes is a driver target, not the datastore.** The controller-runtime operator becomes the implementation of the *fully managed* driver. It watches hub resources over the hub's API and reconciles Agones Fleets, reporting status back. It is optional and arrives with the first managed game (Counter-Strike 2, HTG's P4; #21). Any CRDs it needs are its projection of hub resources, never the source of truth. There is no CRD versioning before then.
3. **Deployment target one is podman quadlets**: hub, Postgres and cloudflared as rootless user units, so "clone, one command, it's running" is `podman compose up` (or `make up`) on Fedora. Kind stays the e2e target for the operator path. Helm and OLM packaging arrive with the operator; OpenShift stays supported, later.
4. **Drivers and adapters run in the hub process during primary development**, as Go packages (`drivers/wardogs`, `adapters/wardogs`) behind the designed interfaces and the `StatsAdapter` protobuf. The conformance suite is the contract. An adapter becomes its own gRPC service in its own `gravel-adapter-*` repo at the open-source cut, by `git subtree split`, when there is a third party to serve. SPIFFE/SPIRE waits for the first second process (the first spoke); in-process code needs no machine identity.
5. **Stats on plain Postgres first.** TimescaleDB is a Postgres extension adopted on a measured signal and ClickHouse later, both behind the stats-store interface as designed. The schema is versioned from day one as before.
6. **Backups belong to the hub, not an operator:** base backups plus continuous WAL archiving to object storage, and a scripted, timed restore to a wall-clock point. The restore drill is HTG's P1 gate. CloudNativePG's declarative recovery returns as an option when the operator exists.

## Build order (replaces the README's)

1. Hub: Connect API, Postgres with migrations, config, health, metrics, UI skeleton, quadlets (#12, #14)
2. Identity: Discord and Steam login, `(provider, subject)` identities, sessions (#13, #5)
3. External-reachable driver and the War Dogs adapter, the stats store, boards and moderation (#16, #4, #3, #17, #18)
4. Composable Discord modules and OIDC for first-party apps (#15, #6), the participation API (#19)
5. `gravel` CLI as a thin HTTP-JSON client, including the hub bootstrap
6. Operator + Agones as the fully managed driver, the Counter-Strike 2 adapter, the transient-server API (#21, #9, #20)
7. Standalone agent and the droplet test harness

## Shortcuts taken, and where each seam is kept

| Shortcut | Seam kept | Undone when |
|---|---|---|
| Resources in Postgres, no CRDs | The API is the only client surface; resource types and fields are the designed ones | Never: the operator projects, it doesn't own |
| Adapters and drivers in-process | `StatsAdapter` protobuf, driver interface with a capabilities flag, conformance suite | OSS cut: `git subtree split` into `gravel-adapter-*` |
| No SPIRE | Identity-provider interface for machine identity stays in the design | First spoke |
| Plain Postgres for stats | Stats-store interface in domain terms | Measured latency or volume signal |
| Quadlets, no Kind for the hub | Nothing in the hub assumes a cloud cluster, a load balancer or external DNS | Never: quadlets stay a supported target |

## Consequences

- HTG's first member-visible feature (a live War Dogs card) depends on a Go service and Postgres, not on a cluster. The hub can run on an existing workstation behind a Cloudflare Tunnel until the community is big enough to pay for a VM (HTG's D20).
- The operator, Agones, Flux, k3s, kubeconform and the Kind e2e job move to the Counter-Strike 2 phase. Until then CI is gofmt, vet, race tests, golangci-lint, govulncheck, `buf lint` and `buf breaking`, and an integration job against a Postgres service container.
- `Game` specs stay declarative YAML manifests and the catalog repo stays planned; they are applied through the API instead of `kubectl`.
- The README's principles 1–12 hold. Principle 2's wording changes to say where the resource lives; principle 4's restatement is #3.
