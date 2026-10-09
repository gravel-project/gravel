# ADR-0002: The hub mints the owner-claim token

**Status:** accepted (John, 2026-10-08); the claim binds to a logged-in user since ADR-0004 · **Changes:** who prints the one-time owner-claim token in the day-zero bootstrap of the 2026-09-25 design · **Tracked by:** #12

## Context

A fresh hub has no owner and every later action needs one. The design settled on a CLI-driven bootstrap: the `gravel` CLI initialises the hub and prints a one-time owner-claim token, single-use and only valid while the hub is unowned, chosen over first-login-claims because that has a land-grab window. The CLI is build-order step 5 and consumes only the HTTP-JSON API, so "the CLI prints the token" means an API call returns it. Any unauthenticated call that returns the token on a fresh hub reopens the land-grab window the design wanted closed: whoever reaches the new hub first holds the token. Closing it with a bootstrap secret adds a second secret to provision for a one-time step.

## Decision

The hub mints the token itself. On every start while it is unowned, the hub creates the built-in organization row if it is missing, stores a fresh token's SHA-256 and expiry (`claim.token_ttl`, 15 minutes by default) and prints the token once, to its own log. `OrganizationService.ClaimOwnership` consumes it: constant-time compare, atomic single-use update, refused once expired, and refused forever once the hub is owned, because an owned hub never mints a token again. Reading the host's journal is the proof of operator access; no network call hands the token out.

The `gravel` CLI's bootstrap command becomes `gravel claim <token>`: still the first call against the fresh API, still an ordinary client. Binding the claim to a logged-in user is the identity work's job (#13); the skeleton records the claim and leaves the owner column for it.

## Consequences

- One token, one secret-free step, no bootstrap endpoint on the network. A leaked token is worthless after 15 minutes, after one use, or once the hub is owned, and a restart rotates it.
- The token appears in the hub's log at WARN level. Operators who ship logs elsewhere should expect that line; it is the only secret the hub ever logs, and it is dead within minutes.
- The design doc's "CLI bootstrap prints the token" reads as "the hub prints it; the CLI claims it" from here on. README and CLAUDE.md say so.
