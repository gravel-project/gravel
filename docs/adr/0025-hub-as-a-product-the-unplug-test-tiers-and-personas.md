# ADR-0025: Hub as a product — the unplug test, deployment tiers and personas

**Status:** proposed (2026-10-13; accepted when John merges it) · **Changes:** what gravel promises to do with nothing plugged in, who it is built for and where it runs; amends README "Hub and spoke" and its load-bearing principles, and ADR-0001 (a Compose tier, and H1 in the build order) · **Decided in:** John's design session *gravel — Design Session: Hub, Spines & Dashboard* (2026-10-10 to 12) and Hidden Token Gaming's roadmap re-cut (handbook `docs/plan.md`, D35 and D36) · **Tracked by:** #183; the H1 milestone is #182 (hidden-token-gaming/.github#59)

## Context

gravel was designed as the hub of one community, and that community lives on Discord. Discord became its login, its notice board and, through the bot, the way staff act. The truth pass of 2026-10-12 found the seams mostly where they should be: the hub starts with no login provider (login is off in the stock `hub.yaml`); the bot is a separate process that reaches the hub as an app (ADR-0008); the cloud sits behind `cloud.Provider` (ADR-0003); quadlets run the hub on one machine and `gravel hub create` makes it a cloud VM (ADR-0020). It also found what only shows when something is unplugged:

- With no OAuth provider configured nobody can log in, so the owner claim never happens: a hub with no Discord is a hub with no owner.
- Permissions are the owner plus one granted role, `moderator`, checked by hand-written helpers (ADR-0013). There is no way to let someone restart a server without letting them moderate people, and no way to hand the hub to someone else.
- What the hub knows (the audit log, identity events, host events, role changes) reaches people only through the bot, which polls those tables. The web pages poll nothing. Take the bot away and the hub is silent.
- The operator's dashboards are Grafana's, a second web application beside the hub (ADR-0009), and the only place a fleet is seen as a whole.
- The `gravel` CLI makes the hub's host and talks to no hub.

John's design session answered with a philosophy rather than a fix: gravel is the hub for a gaming community and owns the truth; every integration is a consumer of gravel, never a dependency of it. Discord is the golden path, because most hosts will use it, and it sits behind the same seam as everything else. The session also named who gravel is for, as three people and the test each gives us, and where it runs, as four tiers with the smallest always viable. This ADR records that frame. The spines it asks for each get their own ADR as they land in H1: local accounts (ADR-0026), capabilities and five roles (ADR-0027), events and notifiers (ADR-0028), the admin dashboard (ADR-0029), operator dashboards through the hub (ADR-0030), the CLI as an API client (ADR-0031) and the telemetry store (ADR-0032, proposed by a measurement first).

## Decision

1. **gravel stands with no integration configured.** A hub with no Discord application, no Steam key and no cloud token starts, gets an owner, shows its fleet, says what is wrong and lets its owner act, with a plainer face. Discord, Steam, the cloud providers and Kubernetes are consumers of the hub behind their seams; none is a dependency. The hub owns the truth: identity, permissions, events and actions are gravel's own, and an integration maps onto them. A feature that needs Discord is a bot module over a hub capability that works without it.

2. **The unplug test is H1's gate.** One end-to-end test (`make unplug`, #198) runs the hub with no provider configured and a transport that fails any dial off loopback, and checks that:
   - a local owner is created from the host and claims the hub through `gravel claim`;
   - the fleet summary shows a fake server going up and then unreachable, with an event and an alarm;
   - the alarm reaches a notifier with no Discord in the path;
   - `/admin` serves the dashboard under the strict CSP;
   - `ListDashboards.available` is false, and nothing fails, with no metrics store.

   CI runs it on every change and the `main` ruleset requires it. Hidden Token Gaming then runs the main build with all of it for a week (hidden-token-gaming/deploy#202). H1 is open until both hold.

3. **Three personas are the acceptance criteria.** A change is done when all three can use it, or when its docs say which one it is not for.

   | Persona | Tier | Wants | The test they give us |
   |---|---|---|---|
   | **Neckbeard** | A single machine or a bare VM | No Discord, local accounts, SSHes in himself, the CLI and the API | If Neckbeard can't do it, there is hidden coupling |
   | **VPS Dad** | Compose on a cloud box | A dashboard, no cluster | The everyday path works without Kubernetes |
   | **K8s bro** | A full cluster | Every integration, the Discord golden path, GitOps | If K8s bro finds it clunky, the golden path needs work |

   Neckbeard is the floor, the old-school operator with no Discord, Steam or Xbox who tracks a few internet-connected servers for an old game with local accounts. K8s bro is the ceiling, and the golden path is measured against him. VPS Dad is most hosts.

4. **Four deployment tiers, smallest to largest, and the smallest always stays viable:** a single machine (or a local VM) on quadlets; Compose on a cloud box you install on yourself (#210); a cloud VM the CLI makes (ADR-0020); a Kubernetes cluster with the operator (K1). At every tier the API is the front door and the source of every action; the CLI is a client on the API and is never retired, because it is the floor that keeps gravel runnable below the cluster; the web UI is a richer client on the same API. Kubernetes custom resources are an extra desired-state path in the cluster tier only, for GitOps users. Telemetry is optional at every tier: a solo operator who doesn't care about it runs the hub with the whole pipeline absent, and the hub says so instead of failing. `docs/tiers.md` is the reference.

5. **The design session's principles join the README's load-bearing ones:** the glue, not an add-on; Discord is the golden path, not a requirement; native spine first, plugins second; an old-school floor; lightweight open source everywhere. Principle 7 is reworded: the hub owns the truth and its own web UI is the authoritative admin surface; Discord mirrors the hub's events and actions and never holds the control plane.

6. **The build order gains H1** between the cloud-VM driver and the operator (ADR-0001 amended): local accounts, capabilities with five roles and ownership transfer, an events log with notifiers, the CLI as an API client, the admin dashboard, the Compose tier, and the unplug test last. Kubernetes (the operator and Agones) follows P5, as K1: one hub node is the cheaper home for a small community (John, 2026-10-12).

## Consequences

- New: this ADR and `docs/tiers.md`. Amended: README (status, principles 7 and 10 to 13, "Hub and spoke", identity, roles, build order), `CLAUDE.md` (principles, vocabulary, build order), `docs/hub.md` "Running it" (a tier table) and ADR-0001 (the Compose tier, H1). Nothing a host runs changes; each later ADR brings its own `**Breaking:**` line.
- Every H1 pull request is checked against the three personas, and the unplug test is the last H1 PR (#198) because every spine before it is a prerequisite: local accounts (#186, #187), capabilities (#184, #185), events and notifiers (#188 to #190), the CLI (#194, #195) and the dashboard (#191 to #193).
- The Compose tier is new work (#210, after this ADR): `docker compose up` on a fresh box gives a working hub with a local owner. GRVL hands a customer this tier.
- A feature that only works with Discord is now a bug, not a shortcut. The plainer face is a real face to maintain: the templ pages and the admin dashboard must carry every action the bot does.
- Local accounts mean gravel owns password hashing, lockout and resets; ADR-0026 says how, and the README's "provider-only identity" tradeoff and its email-matching merge path are withdrawn.
- Not decided here: how each spine works (ADR-0026 to 0031), what the telemetry store is and whether Grafana stays (measured first, hidden-token-gaming/deploy#201, then ADR-0032), and the Kubernetes tier's packaging (Helm, OLM; K1).
