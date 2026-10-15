# Tiers and personas

Who gravel is built for, where it runs, and the test that keeps it standing on its own
([ADR-0025](adr/0025-hub-as-a-product-the-unplug-test-tiers-and-personas.md)). The hub is the
product: it owns identity, permissions, events and actions. Discord, Steam, the cloud providers and
Kubernetes are consumers of it behind seams, and the hub stands with none of them configured.

## The three personas

| Persona | Tier | Wants | The test they give us |
|---|---|---|---|
| **Neckbeard** | A single machine or a bare VM | No Discord, local accounts, SSHes in himself, the CLI and the API | If Neckbeard can't do it, there is hidden coupling |
| **VPS Dad** | Compose on a cloud box | A dashboard, no cluster | The everyday path works without Kubernetes |
| **K8s bro** | A full cluster | Every integration, the Discord golden path, GitOps | If K8s bro finds it clunky, the golden path needs work |

A change is done when all three can use it, or when its docs say which one it is not for. Neckbeard
is the floor: a solo operator with no Discord, Steam or Xbox who runs gravel with local accounts to
track a few internet-connected servers for an old game. K8s bro is the ceiling, and the golden path
(Discord, the cloud, GitOps) is measured against him. VPS Dad is most hosts.

## The four tiers

Smallest to largest. The single-machine tier always stays viable.

| Tier | Where | How it runs | Status |
|---|---|---|---|
| **1 · A single machine** | Your own box, or a local VM | The quadlet units in `deploy/quadlet/` as rootless systemd user units; the backup set, the bot and the observability set are opt-ins ([deploy/README.md](../deploy/README.md)). `make up` is the development stack | In |
| **2 · Compose on a cloud box** | A rented VM you install on yourself | A production Compose file with the quadlets' digest pins and secrets from files; `docker compose up` on a fresh box gives a working hub with a local owner | #210 (H1) |
| **3 · A cloud VM the CLI makes** | A Droplet, DigitalOcean first | `gravel hub create --profile stock` makes the host through a cloud provider and installs tier 1's units on it; `destroy` then `create` replaces the host with its data intact ([cli.md](cli.md), [ADR-0020](adr/0020-the-hubs-own-host-made-by-the-cli-through-the-cloud-provider.md)) | In |
| **4 · A Kubernetes cluster** | Kind locally; a managed cluster; OpenShift | The operator projects hub resources into the cluster and Agones runs the managed servers; Helm and OLM packaging ([ADR-0001](adr/0001-hub-first-kubernetes-later.md)) | K1, after P5 (#21, #9) |

What is the same at every tier:

| Surface | What it is | Where it exists |
|---|---|---|
| The API | The front door and the source of every action | Every tier |
| The CLI (`gravel`) | A client on the API; the floor that keeps gravel runnable below the cluster | Every tier, never retired |
| The web UI | A richer client on the same API: the member pages and the admin dashboard | Every tier |
| Kubernetes custom resources | Desired state reconciled by the operator, for GitOps users | The cluster tier only |
| Telemetry | A metrics store and a log store beside the hub, never inside it; with none configured the hub says "no metrics store is configured" and nothing else changes | Optional at every tier (ADR-0032, measured first) |

## The unplug test

If Discord were removed tomorrow, gravel must still stand up completely on its own, with a plainer
face. `make unplug` (#198) proves it on every change, and H1 is open until it passes: the hub runs with
no provider configured and a transport that fails any dial off loopback, and

- a local owner is created from the host and claims the hub through `gravel claim`;
- the fleet summary shows a fake server going up, then unreachable, with an event and an alarm;
- the alarm reaches a notifier with no Discord in the path;
- `/admin` serves the dashboard under the strict CSP;
- `ListDashboards.available` is false, and nothing fails, with no metrics store.

As built (`internal/hub/unplug_e2e_test.go`, `TestUnplug`; CI's `unplug` job): the host's own
binaries do the host's part (`gravel-hub migrate`, `servers apply`, `users create-local`), the member
sets their password from the printed link, `gravel login --local` and `gravel claim` take the hub,
and `gravel settings apply` adds a webhook notifier on loopback. The server is War Dogs' simulator
(`wardogsim`) on loopback; once it stops, the fleet shows it down, the `server.down` alarm follows
after its two minutes, the events log has `server.unreachable`, and the webhook receives it,
signed. The test also calls every API service on the public listener, so one the router doesn't
forward fails it. It takes about two minutes and needs a Postgres and the dashboard's bundle:

```sh
GRAVEL_TEST_DATABASE_URL='postgres://gravel:test@127.0.0.1:55432/gravel?sslmode=disable' make unplug
```

Hidden Token Gaming then runs the main build with all of it for a week.

## What it means for a change

- **Identity:** every way in is an `identity.Provider`, local accounts included (ADR-0026). A hub with
  no OAuth provider offers a login form. A credential is linked only from inside a session; a
  `(provider, subject)` linked elsewhere is refused, never merged.
- **Permissions:** gravel's five roles are bundles of its capabilities, checked in one place
  (ADR-0027). Discord role sync maps onto them and may grant moderator at most, never admin or owner.
- **Notifications:** the hub appends events and a notifier seam delivers them (ADR-0028): a webhook,
  ntfy or email with no Discord configured; the bot is one consumer of the same log.
- **Dashboards:** the hub's own, a static client in the binary (ADR-0029), with operator panels read
  through the hub from whatever store is configured (ADR-0030). Grafana is optional and
  operator-facing.
- **The CLI:** an API client with a credential on this machine (ADR-0031); every admin procedure it
  runs is one the dashboard runs too.
- **A feature that needs Discord** is a bot module (ADR-0008) over a hub capability that works without it.
