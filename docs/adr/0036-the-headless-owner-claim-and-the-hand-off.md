# ADR-0036: The headless owner claim and the hand-off

**Status:** accepted (2026-10-15, John: the host mints on demand, the provisioner claims first and hands over, its account stays an admin the customer may revoke; built in #215) · **Changes:** how a token is minted besides at start; amends ADR-0002 (a second way to the token, by the same proof) and reads with ADR-0027 §6 (the hand-off is its transfer) · **Decided in:** GRVL's upstream needs (grvl-handbook §3.2 and §6), filed from Hidden Token Gaming's roadmap re-cut (handbook D35) · **Part of:** H1 (#182), framed by ADR-0025

## Context

A hosted hub is made by a program, not a person: GRVL's control plane creates the host through `hubvm` (ADR-0020, #211), and the hub must end up owned by the customer who pays for it, not by whoever provisioned it. ADR-0002 gives a fresh hub one way to an owner: the hub logs a one-time token at start, for 15 minutes, and a restart replaces it. A program can scrape that log line over SSH (`gravel hub status` does), but the line is a log format, it may be gone by the time the program looks, and a restart in between silently rotates the token. A customer told to claim within 15 minutes of provisioning would often miss it.

Two constraints from earlier decisions stand. No network call ever hands out a claim token (ADR-0002: on a fresh hub, that reopens the land-grab window). And a hub has exactly one owner at every moment, with ownership moving only by nomination and acceptance in person (ADR-0027 §6).

## Decision

1. **The host mints a token on demand.** `gravel-hub claim-token [--ttl DURATION]`, run on the hub's host against its database, mints a fresh token while the hub is unowned and prints it to stdout, once; it is never logged. Shell access to the host is the proof, the same proof reading the start-up log is, so ADR-0002's reasoning holds and nothing new is on the network. The token replaces any earlier one, the logged one included. It works for `claim.token_ttl` (15 minutes) unless `--ttl` says otherwise, from a minute to 7 days (a nomination's life, ADR-0027 §6). An owned hub refuses: it never mints again. Each mint is an event, `ownership.claim_token_minted` (notice, by the host's command line), carrying the expiry and nothing of the token. On a stock host the command runs in the hub's container: `podman exec gravel-hub /ko-app/gravel-hub claim-token --config /etc/gravel/hub.yaml`, over SSH as the runtime user.
2. **The provisioner claims first, as a member.** A claim binds to a logged-in member (ADR-0004), so the provisioner makes its own local account on the host (`gravel-hub users create-local`, ADR-0026), sets the password from the link, logs in (`gravel login --local`), mints a token and claims (`gravel claim`). It is then the owner and sets the hub up over the API like any owner: the settings, the servers, the apps, the customer's Discord application.
3. **The hand-off is the ownership transfer.** The customer becomes a member of the hub (a local account the provisioner makes for them, whose password link it sends, or their own login through a provider the hub has), the provisioner nominates them (`gravel owner nominate`), and they accept within 7 days (#197). The hub has one owner throughout, and nothing about the hand-off is GRVL's own code in gravel.
4. **Afterwards the provisioner's account is an admin, like any old owner.** ADR-0027 §6 holds unchanged: accepting gives the old owner an admin grant. The customer sees it on Members and may revoke it at any time. A provisioner's routine work (upgrades, backups, a replaced host) goes through its access to the host, not through that account; one that wants no standing access to the hub revokes the grant itself once it sees `ownership.transferred`.

## Not chosen

- **An API that mints or returns a token.** On a fresh hub, whoever reaches it first would hold it (ADR-0002); a bootstrap secret to guard it is one more secret to provision for a one-time step.
- **The customer claims directly, with a long-lived link.** No provisioner account would remain, but whoever holds the link owns the hub, and the provisioner could not set the hub up before the customer arrives.
- **Removing the old owner's admin grant on acceptance.** It would need a flag on the nomination for one caller's case; revoking a grant is already the owner's, and the provisioner's to do for itself.

## Consequences

- New: `gravel-hub claim-token`, `org.Service.MintClaimToken` (bounds `org.MinClaimTokenTTL` to `org.MaxClaimTokenTTL`), `store.MintClaimToken` with the event in the same transaction, and the event kind.
- GRVL's provisioning steps 3 and 4 (grvl-handbook §3.2) become: wait for cloud-init, push the secrets, make the provisioner's local account, mint, claim, configure, make the customer's account, nominate. Every step is a command or an API call gravel documents; none parses a log.
- `gravel hub status` still reads the token from the start-up log: status is read-only, and minting would replace a token the operator may already have copied. An operator who missed the line mints one with `gravel hub ssh -- podman exec …`.
- A longer `--ttl` widens the window a leaked token is good for; the default stays 15 minutes, and a provisioner that claims at once never needs more. The token reaches only the stdout of a command run on the host.
